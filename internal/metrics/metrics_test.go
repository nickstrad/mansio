package metrics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/metrics"
)

func TestTextExposition(t *testing.T) {
	r := metrics.NewRegistry()
	h := metrics.NewLatency([]time.Duration{time.Millisecond, time.Second})
	for _, d := range []time.Duration{0, time.Millisecond, time.Millisecond + 1, time.Hour} { // on and off the bounds
		h.Observe(d)
	}
	r.Add("z_latency_seconds", "latency", metrics.Histogram, func(e *metrics.Emit) {
		b, c, s := h.Snapshot()
		e.Histogram(metrics.Labels{"op", "x"}, b, c, s)
	})
	r.Add("a_requests_total", "requests\nby class", metrics.Counter, func(e *metrics.Emit) {
		e.Value(metrics.Labels{"class", `o"k\`}, 3)
		e.Value(nil, 1.5)
	})
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP a_requests_total requests by class
# TYPE a_requests_total counter
a_requests_total{class="o\"k\\"} 3
a_requests_total 1.5
# HELP z_latency_seconds latency
# TYPE z_latency_seconds histogram
z_latency_seconds_bucket{op="x",le="0.001"} 2
z_latency_seconds_bucket{op="x",le="1"} 3
z_latency_seconds_bucket{op="x",le="+Inf"} 4
z_latency_seconds_sum{op="x"} 3600.002000001
z_latency_seconds_count{op="x"} 4
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestRegistrationMistakesPanic(t *testing.T) {
	for name, fn := range map[string]func(r *metrics.Registry){
		"bad name":  func(r *metrics.Registry) { r.Add("1bad", "", metrics.Gauge, func(*metrics.Emit) {}) },
		"duplicate": func(r *metrics.Registry) { r.Add("x", "", metrics.Gauge, nil); r.Add("x", "", metrics.Gauge, nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			fn(metrics.NewRegistry())
		}()
	}
}

// OnScrape hooks run once per scrape, before the collectors that read their snapshot.
func TestOnScrapeSnapshotsOncePerScrape(t *testing.T) {
	r := metrics.NewRegistry()
	calls, snap := 0, 0
	r.OnScrape(func() { calls++; snap = calls * 10 })
	for _, name := range []string{"a", "b"} {
		r.Add(name, "", metrics.Gauge, func(e *metrics.Emit) { e.Value(nil, float64(snap)) })
	}
	var b strings.Builder
	r.WriteText(&b)
	r.WriteText(&b)
	if calls != 2 || !strings.Contains(b.String(), "a 20\n") || !strings.Contains(b.String(), "b 20\n") {
		t.Fatalf("calls %d:\n%s", calls, b.String())
	}
}
