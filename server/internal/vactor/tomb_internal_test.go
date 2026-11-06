package vactor

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/det/dettest"
	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
)

// The per-key backoff (internal/retry, equal jitter) at each failure in a row: [d/2, d] with d = 100 ms
// doubling, capped at 5 s (on point n=7, far past it n=1000).
func TestTombstoneBackoff(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clk := dettest.NewClock(now)
	h, err := NewHost(nil, nil, Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: durabletest.NewStore(), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{{1, 100 * time.Millisecond}, {2, 200 * time.Millisecond}, {6, 3200 * time.Millisecond}, {7, 5 * time.Second}, {1000, 5 * time.Second}} {
		for range 50 {
			h.tombs["k"] = tomb{n: tc.n - 1, until: now}
			h.forget("k", &entry{err: errors.New("boom")})
			if d := h.tombs["k"].until.Sub(now); d < tc.want/2 || d > tc.want {
				t.Fatalf("failure %d: backoff %v outside [%v, %v]", tc.n, d, tc.want/2, tc.want)
			}
		}
	}
}

// A tombstone counts failures in a row, starts over once its backoff is long past, and Tick's reaper drops it
// then (Release It S1).
func TestTombstoneCountsResetsAndIsReaped(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clk := dettest.NewClock(now)
	h, err := NewHost(nil, nil, Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: durabletest.NewStore(), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	fail := func() int {
		e := &entry{err: errors.New("boom")}
		h.forget("k", e)
		return h.tombs["k"].n
	}
	if n := fail(); n != 1 {
		t.Fatalf("first failure n=%d", n)
	}
	if n := fail(); n != 2 {
		t.Fatalf("second failure in a row n=%d", n)
	}
	until := h.tombs["k"].until
	clk.Advance(until.Add(h.opts.MaxFailBackoff).Sub(now)) // on point: still "recent"
	h.pruneTombs(clk.Now())
	if _, ok := h.tombs["k"]; !ok {
		t.Fatal("reaped on the boundary")
	}
	clk.Advance(1) // off point: long past
	h.pruneTombs(clk.Now())
	if _, ok := h.tombs["k"]; ok {
		t.Fatal("not reaped once long past")
	}
	if n := fail(); n != 1 {
		t.Fatalf("a failure long after the last one did not start over: n=%d", n)
	}
	if tb, wait, ok := h.tombstoned("k"); !ok || wait <= 0 || tb.err == nil {
		t.Fatalf("the tombstone does not fail fast: %+v %v %v", tb, wait, ok)
	}
}
