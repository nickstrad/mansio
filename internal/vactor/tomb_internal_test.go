package vactor

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
)

// Spec table for the per-key backoff: first value, doubling, the cap's on and off points, huge n.
func TestBackoff(t *testing.T) {
	const first, most = 100 * time.Millisecond, 5 * time.Second
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{6, 3200 * time.Millisecond}, // last value under the cap
		{7, 5 * time.Second},         // 6400 ms: capped (off point)
		{1000, 5 * time.Second},      // no overflow
	} {
		if got := backoff(first, most, tc.n); got != tc.want {
			t.Errorf("backoff(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
	if got := backoff(5*time.Second, 5*time.Second, 1); got != 5*time.Second {
		t.Errorf("first == cap (on point): %v", got)
	}
	for _, d := range []time.Duration{0, 1, 2, 3, time.Second} {
		for range 100 {
			if j := jitter(d); j < d/2 || j > d {
				t.Fatalf("jitter(%v) = %v outside [d/2, d]", d, j)
			}
		}
	}
}

// A tombstone counts failures in a row, starts over once its backoff is long past, and Tick's reaper drops it
// then (Release It S1).
func TestTombstoneCountsResetsAndIsReaped(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	h, err := NewHost(nil, nil, Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: durabletest.NewStore(), Now: func() time.Time { return now }})
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
	now = until.Add(h.opts.MaxFailBackoff) // on point: still "recent"
	h.pruneTombs(now)
	if _, ok := h.tombs["k"]; !ok {
		t.Fatal("reaped on the boundary")
	}
	now = now.Add(1) // off point: long past
	h.pruneTombs(now)
	if _, ok := h.tombs["k"]; ok {
		t.Fatal("not reaped once long past")
	}
	if n := fail(); n != 1 {
		t.Fatalf("a failure long after the last one did not start over: n=%d", n)
	}
	if err := h.tombstoned("k"); err == nil || !errors.Is(err, h.tombs["k"].err) {
		t.Fatalf("the tombstone does not fail fast with its cause: %v", err)
	}
}
