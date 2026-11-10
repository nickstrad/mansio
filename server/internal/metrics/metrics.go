// Package metrics is a small, dependency-free metrics exposition (Release It O1, O2): counters, gauges and
// latency histograms written in the Prometheus text format (version 0.0.4) for scraping from the lifecycle
// listener. Values are pulled at scrape time from the components' own Stats (collect funcs), so the hot paths
// keep their atomics and import nothing from here; LatencyHistogram is the one primitive a component may embed. Thresholds and alert rules live in the monitoring config, never here.
//
// Error module: none; misuse (a bad metric name, a duplicate family) is a bug and panics at registration.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
)

// Type is a metric family's type.
type Type string

const (
	Counter   Type = "counter"
	Gauge     Type = "gauge"
	Histogram Type = "histogram"
)

// Labels are name, value pairs: Labels{"op", "fetch", "class", "ok"}.
type Labels []string

// Emit receives one family's samples during a scrape.
type Emit struct {
	w    *bufio.Writer
	name string
	typ  Type
}

// Value writes one counter or gauge sample.
func (e *Emit) Value(l Labels, v float64) {
	assert.That(e.typ != Histogram, "metrics: Value on histogram %s", e.name)
	e.sample(e.name, l, v)
}

// Histogram writes one histogram: counts per upper bound (cumulated here) plus one past the last bound, and the
// sum of observations in seconds.
//
//	pre: len(counts) == len(bounds) + 1
func (e *Emit) Histogram(l Labels, bounds []time.Duration, counts []uint64, sum time.Duration) {
	assert.That(e.typ == Histogram, "metrics: Histogram on %s %s", e.typ, e.name)
	assert.That(len(counts) == len(bounds)+1, "metrics: %s: %d counts for %d bounds", e.name, len(counts), len(bounds))
	var cum uint64
	for i, c := range counts {
		cum += c
		le := "+Inf"
		if i < len(bounds) {
			le = strconv.FormatFloat(bounds[i].Seconds(), 'g', -1, 64)
		}
		e.sample(e.name+"_bucket", append(slices.Clip(l), "le", le), float64(cum))
	}
	e.sample(e.name+"_sum", l, sum.Seconds())
	e.sample(e.name+"_count", l, float64(cum))
}

func (e *Emit) sample(name string, l Labels, v float64) {
	assert.That(len(l)%2 == 0, "metrics: %s: odd label list %v", name, l)
	e.w.WriteString(name)
	if len(l) > 0 {
		e.w.WriteByte('{')
		for i := 0; i < len(l); i += 2 {
			if i > 0 {
				e.w.WriteByte(',')
			}
			if assert.Enabled && !validName.MatchString(l[i]) { // guarded: the check runs per sample
				assert.That(false, "metrics: %s: bad label name %q", name, l[i])
			}
			e.w.WriteString(l[i])
			e.w.WriteString(`="`)
			e.w.WriteString(escape.Replace(l[i+1]))
			e.w.WriteByte('"')
		}
		e.w.WriteByte('}')
	}
	e.w.WriteByte(' ')
	e.w.WriteString(formatValue(v))
	e.w.WriteByte('\n')
}

var (
	validName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	escape    = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
)

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

type family struct {
	name, help string
	typ        Type
	collect    func(*Emit)
}

// Registry holds metric families. Safe for concurrent use; scrapes run one at a time.
type Registry struct {
	mu     sync.Mutex
	fams   map[string]family
	before []func()
	queues []func() []QueueStat // Queues' sources

	scrape sync.Mutex // serializes WriteText, so OnScrape snapshots are shared by one scrape's collectors
}

// OnScrape registers fn to run at the start of every scrape, before any collector: a component takes one
// snapshot there and its families read it, instead of each family locking the component again.
func (r *Registry) OnScrape(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.before = append(r.before, fn)
}

func NewRegistry() *Registry { return &Registry{fams: map[string]family{}} }

// Add registers a family; collect runs at every scrape and must be cheap and safe to call concurrently.
//
//	pre: name is a valid metric name not registered yet (a bug otherwise: panics)
func (r *Registry) Add(name, help string, typ Type, collect func(*Emit)) {
	mustf(validName.MatchString(name), "metrics: bad metric name %q", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	_, dup := r.fams[name]
	mustf(!dup, "metrics: %s registered twice", name)
	r.fams[name] = family{name: name, help: help, typ: typ, collect: collect}
}

// WriteText writes every family, sorted by name, in the Prometheus text format.
func (r *Registry) WriteText(w io.Writer) error {
	r.scrape.Lock()
	defer r.scrape.Unlock()
	r.mu.Lock()
	fams := make([]family, 0, len(r.fams))
	for _, f := range r.fams {
		fams = append(fams, f)
	}
	before := slices.Clone(r.before)
	r.mu.Unlock()
	for _, fn := range before {
		fn()
	}
	slices.SortFunc(fams, func(a, b family) int { return strings.Compare(a.name, b.name) })
	bw := bufio.NewWriter(w)
	for _, f := range fams {
		fmt.Fprintf(bw, "# HELP %s %s\n# TYPE %s %s\n", f.name, strings.ReplaceAll(f.help, "\n", " "), f.name, f.typ)
		f.collect(&Emit{w: bw, name: f.name, typ: f.typ})
	}
	return bw.Flush()
}

// Handler serves GET /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		r.WriteText(w) // a write error means the scraper left
	})
}

// mustf panics in every build (wiring mistakes are bugs found at startup, not on a hot path).
func mustf(cond bool, format string, args ...any) {
	if !cond {
		panic(fmt.Sprintf(format, args...))
	}
}

// LatencyBuckets are the default latency upper bounds (the same as objstore's, so families compare).
var LatencyBuckets = []time.Duration{
	time.Millisecond, 2500 * time.Microsecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond,
	50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second,
	2500 * time.Millisecond, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute,
}

// LatencyHistogram counts durations per bucket with atomics; the zero value is not usable (NewLatency).
type LatencyHistogram struct {
	bounds []time.Duration
	counts []atomic.Uint64 // len(bounds)+1
	sum    atomic.Int64    // nanoseconds
}

// NewLatency returns a histogram over bounds (nil: LatencyBuckets).
//
//	pre: bounds ascending
func NewLatency(bounds []time.Duration) *LatencyHistogram {
	if bounds == nil {
		bounds = LatencyBuckets
	}
	assert.That(slices.IsSorted(bounds), "metrics: histogram bounds not ascending")
	return &LatencyHistogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds)+1)}
}

// Observe counts d in the first bucket whose bound is >= d.
func (h *LatencyHistogram) Observe(d time.Duration) {
	i, _ := slices.BinarySearch(h.bounds, d)
	h.counts[i].Add(1)
	h.sum.Add(int64(d))
}

// Snapshot returns the bounds, the per-bucket counts (not cumulated) and the sum.
func (h *LatencyHistogram) Snapshot() (bounds []time.Duration, counts []uint64, sum time.Duration) {
	counts = make([]uint64, len(h.counts))
	for i := range h.counts {
		counts[i] = h.counts[i].Load()
	}
	return h.bounds, counts, time.Duration(h.sum.Load())
}
