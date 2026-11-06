package stats

import (
	"math"
	"os"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"syscall"
	"time"
)

// Gauges are read by the sampler each tick. Every func must be cheap and safe to call concurrently.
type Gauges struct {
	Phase     func() int
	DBs       func() int                          // active databases in this phase
	TargetQPS func() float64                      // the schedule's rate right now
	Actors    func() (open, queued, maxQueue int) // open databases, requests queued, the deepest queue (one scan)
	InFlight  func() int64
	Completed func() int64 // attempts answered so far
	Dropped   func() int64 // records dropped by the writer
}

// sampleColumns is the samples.csv header; queries/report.sql depends on these names.
var sampleColumns = []string{
	"t_ms", "phase", "dbs", "target_qps", "completed", "goroutines", "os_threads", "heap_mb", "go_total_mb", "max_rss_mb",
	"fds", "user_cpu_s", "sys_cpu_s", "gc_cycles", "gc_pause_p99_us", "sched_p50_us", "sched_p99_us", "open_dbs",
	"queue_total", "queue_max", "in_flight", "records_dropped",
}

// The runtime metrics a sample reads, by index into runtimeMetrics.
const (
	mGoroutines = iota
	mHeap
	mTotal
	mGCCycles
	mSchedLat
	mGCPauses
	numMetrics
)

var runtimeMetrics = [numMetrics]string{
	mGoroutines: "/sched/goroutines:goroutines",
	mHeap:       "/memory/classes/heap/objects:bytes",
	mTotal:      "/memory/classes/total:bytes",
	mGCCycles:   "/gc/cycles/total:gc-cycles",
	mSchedLat:   "/sched/latencies:seconds",
	mGCPauses:   "/sched/pauses/total/gc:seconds",
}

// probe reads process-wide counters. Histograms are cumulative, so it keeps the previous counts to report
// quantiles over each interval only.
type probe struct {
	samples              []metrics.Sample
	prevSched, prevPause []uint64
}

func newProbe() *probe {
	p := &probe{samples: make([]metrics.Sample, numMetrics)}
	for i, n := range runtimeMetrics {
		p.samples[i].Name = n
	}
	return p
}

// row collects one samples.csv row (t is formatted as milliseconds by Field).
func (p *probe) row(t time.Duration, g Gauges) []any {
	metrics.Read(p.samples)
	sched := p.samples[mSchedLat].Value.Float64Histogram()
	pause := p.samples[mGCPauses].Value.Float64Histogram()
	schedP50, schedP99 := deltaQuantile(p.prevSched, sched, 0.5), deltaQuantile(p.prevSched, sched, 0.99)
	pauseP99 := deltaQuantile(p.prevPause, pause, 0.99)
	p.prevSched = append(p.prevSched[:0], sched.Counts...)
	p.prevPause = append(p.prevPause[:0], pause.Counts...)

	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	maxRSS := float64(ru.Maxrss)
	if runtime.GOOS != "darwin" {
		maxRSS *= 1024 // Linux reports KiB, macOS bytes
	}
	open, qt, qm := g.Actors()
	return []any{
		t, g.Phase(), g.DBs(), round(g.TargetQPS(), 1), g.Completed(),
		p.samples[mGoroutines].Value.Uint64(), pprof.Lookup("threadcreate").Count(),
		mb(p.samples[mHeap].Value.Uint64()), mb(p.samples[mTotal].Value.Uint64()), round(maxRSS/(1<<20), 1),
		countFDs(), round(time.Duration(ru.Utime.Nano()).Seconds(), 3), round(time.Duration(ru.Stime.Nano()).Seconds(), 3), p.samples[mGCCycles].Value.Uint64(),
		round(pauseP99*1e6, 1), round(schedP50*1e6, 1), round(schedP99*1e6, 1),
		open, qt, qm, g.InFlight(), g.Dropped(),
	}
}

// Sampler writes samples.csv every interval from a goroutine of its own.
type Sampler struct {
	csv   *CSV
	every time.Duration
	g     Gauges
	start time.Time
	stop  chan struct{}
	done  chan struct{}
}

func NewSampler(path string, every time.Duration, start time.Time, g Gauges) (*Sampler, error) {
	c, err := NewCSV(path, sampleColumns...)
	if err != nil {
		return nil, err
	}
	return &Sampler{csv: c, every: every, g: g, start: start, stop: make(chan struct{}), done: make(chan struct{})}, nil
}

func (s *Sampler) Start() { go s.loop() }

// Stop takes a final sample and closes the file.
func (s *Sampler) Stop() error {
	close(s.stop)
	<-s.done
	return s.csv.Close()
}

func (s *Sampler) loop() {
	defer close(s.done)
	p := newProbe()
	tick := time.NewTicker(s.every)
	defer tick.Stop()
	for {
		final := false
		select {
		case <-tick.C:
		case <-s.stop:
			final = true
		}
		_ = s.csv.Row(p.row(time.Since(s.start), s.g)...) // a lost sample is not worth stopping the run
		if final {
			return
		}
	}
}

func mb(b uint64) float64 { return round(float64(b)/(1<<20), 1) }

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// deltaQuantile is the q-quantile of the observations added since prev, using bucket upper bounds (the lower
// bound for the open-ended last bucket). It is 0 when nothing was added.
func deltaQuantile(prev []uint64, cur *metrics.Float64Histogram, q float64) float64 {
	var total uint64
	delta := make([]uint64, len(cur.Counts))
	for i, c := range cur.Counts {
		if i < len(prev) {
			c -= prev[i]
		}
		delta[i] = c
		total += c
	}
	if total == 0 {
		return 0
	}
	target := uint64(math.Ceil(q * float64(total)))
	var cum uint64
	for i, c := range delta {
		cum += c
		if cum >= target {
			if hi := cur.Buckets[i+1]; !math.IsInf(hi, 1) {
				return hi
			}
			return cur.Buckets[i]
		}
	}
	return cur.Buckets[len(cur.Buckets)-1]
}

// countFDs counts open file descriptors via /dev/fd (macOS and Linux), or -1.
func countFDs() int {
	d, err := os.Open("/dev/fd")
	if err != nil {
		return -1
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return -1
	}
	return len(names) - 1 // minus the descriptor used to read the directory
}
