package metrics_test

import (
	"strings"
	"sync"
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

func TestHighWater(t *testing.T) {
	var h metrics.HighWater
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { h.Add(1); h.Add(-1) })
	}
	wg.Wait()
	h.Add(3)
	h.Add(-2)
	if cur, max := h.Load(); cur != 1 || max < 3 || max > 50 {
		t.Fatalf("cur %d max %d", cur, max)
	}
}
