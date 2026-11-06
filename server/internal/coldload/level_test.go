package coldload_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/coldload"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// getter is a fake host: key "fail*" fails, "panic" panics, "hit*" is a local hit, "wait*" a waiter;
// everything else is an owner activation of 10 bytes.
type getter struct {
	inFlight, peak atomic.Int64
	delay          time.Duration
}

func (g *getter) Get(ctx context.Context, key string) (vactor.Result, error) {
	n := g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	for p := g.peak.Load(); n > p && !g.peak.CompareAndSwap(p, n); p = g.peak.Load() {
	}
	time.Sleep(g.delay)
	switch {
	case strings.HasPrefix(key, "fail"):
		return vactor.Result{}, errors.New("boom")
	case key == "panic":
		panic("getter bug")
	case strings.HasPrefix(key, "hit"):
		return vactor.Result{Act: &vactor.Activation{Bytes: 10, LocalHit: true}}, nil
	case strings.HasPrefix(key, "wait"):
		return vactor.Result{Waited: true}, nil
	}
	return vactor.Result{Act: &vactor.Activation{Bytes: 10}}, ctx.Err()
}

func level(conc int, keys ...string) coldload.Level {
	return coldload.Level{Kind: coldload.KindCold, Conc: conc, Keys: keys}
}

type sink struct {
	mu   sync.Mutex
	recs []coldload.Record
}

func (s *sink) add(r coldload.Record) { s.mu.Lock(); s.recs = append(s.recs, r); s.mu.Unlock() }

func TestRunLevelCounts(t *testing.T) {
	var s sink
	r := coldload.RunLevel(context.Background(), &getter{}, level(2, "a", "b", "fail1", "hit1", "wait1"), s.add)
	// Bytes counts only activations this level fetched: a and b (10 each); the local hit and waiter add none.
	if r.Dispatched != 5 || r.OK != 4 || r.Failed != 1 || r.Bytes != 20 || r.Wall <= 0 {
		t.Fatalf("result = %+v", r)
	}
	seen := map[int]bool{}
	for _, rec := range s.recs {
		seen[rec.Seq] = true
	}
	if len(s.recs) != 5 || len(seen) != 5 {
		t.Fatalf("got %d records over %d seqs, want one per key", len(s.recs), len(seen))
	}
}

func TestRunLevelConcurrencyBound(t *testing.T) {
	tests := []struct {
		name     string
		conc     int
		keys     int
		wantPeak int64
	}{
		{"c=1 is serial", 1, 6, 1},
		{"c=3 of 6", 3, 6, 3},
		{"c above key count is capped", 10, 4, 4},
		{"one key", 4, 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keys := make([]string, tc.keys)
			for i := range keys {
				keys[i] = fmt.Sprint("k", i)
			}
			g := &getter{delay: 20 * time.Millisecond}
			r := coldload.RunLevel(context.Background(), g, level(tc.conc, keys...), func(coldload.Record) {})
			if g.peak.Load() != tc.wantPeak || r.OK != tc.keys {
				t.Fatalf("peak in flight = %d, ok = %d; want %d, %d", g.peak.Load(), r.OK, tc.wantPeak, tc.keys)
			}
		})
	}
}

func TestRunLevelStopsDispatchingWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	keys := make([]string, 100)
	for i := range keys {
		keys[i] = fmt.Sprint("k", i)
	}
	var n atomic.Int64
	r := coldload.RunLevel(ctx, &getter{delay: time.Millisecond}, level(1, keys...), func(coldload.Record) {
		if n.Add(1) == 3 {
			cancel()
		}
	})
	if r.Dispatched >= 100 || r.Dispatched < 3 || r.Dispatched != r.OK+r.Failed {
		t.Fatalf("result after cancel = %+v; want an early stop with consistent counts", r)
	}
}

func TestRunLevelForwardsWorkerPanic(t *testing.T) {
	defer func() {
		if p := recover(); p == nil || !strings.Contains(fmt.Sprint(p), "getter bug") {
			t.Fatalf("recovered %v, want the getter's panic", p)
		}
	}()
	coldload.RunLevel(context.Background(), &getter{}, level(2, "a", "panic", "b"), func(coldload.Record) {})
}
