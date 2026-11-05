package vactor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
)

// Spec table for the fetch bulkhead at 0 / 1 / limit / limit+1 of each bound (count, bytes, queue). A
// pre-cancelled ctx makes "would wait" observable without timing: a fetch that fits is granted before the ctx
// is looked at; one that must wait joins the line and leaves it at once with ctx.Err().
func TestFetchGateSpec(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	const maxN, maxBytes = 2, 100
	tests := []struct {
		name     string
		maxQueue int
		held     []int64 // fetches already in flight, by size
		bytes    int64
		want     error // nil: granted at once
	}{
		{"0 in flight", 1, nil, 10, nil},
		{"1 in flight (limit-1)", 1, []int64{10}, 10, nil},
		{"limit in flight: waits", 1, []int64{10, 10}, 10, context.Canceled},
		{"limit in flight, no queue: busy", 0, []int64{10, 10}, 10, ErrBusy},
		{"bytes on point (60+40 = limit)", 1, []int64{60}, 40, nil},
		{"bytes off point (60+41 > limit): waits", 1, []int64{60}, 41, context.Canceled},
		{"bytes over the limit, no queue: busy", 0, []int64{60}, 41, ErrBusy},
		{"one fetch larger than the limit runs alone", 1, nil, 150, nil},
		{"after a too-large fetch, others wait", 1, []int64{150}, 1, context.Canceled},
		{"unknown size (0 bytes) counts only by number", 1, []int64{100}, 0, nil},
		{"zero queue, room: granted", 0, nil, 10, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newFetchGate(maxN, maxBytes, tc.maxQueue)
			for _, b := range tc.held {
				if _, err := g.acquire(context.Background(), b); err != nil {
					t.Fatalf("setup acquire(%d): %v", b, err)
				}
			}
			release, err := g.acquire(cancelled, tc.bytes)
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("acquire(%d) = %v, want %v", tc.bytes, err, tc.want)
			}
			n, b, q := g.inFlight()
			if q != 0 {
				t.Fatalf("a refused or cancelled waiter stayed queued (%d)", q)
			}
			wantN := len(tc.held)
			if err == nil {
				wantN++
				release()
			}
			if err == nil && n != wantN {
				t.Fatalf("in flight %d, want %d", n, wantN)
			}
			if err != nil && n != len(tc.held) {
				t.Fatalf("a refused acquire holds a slot: n=%d bytes=%d", n, b)
			}
		})
	}
}

// The queue bound at limit and limit+1: with one waiter allowed, the first waits and the second is shed at
// once; a release hands the slot to the waiter.
func TestFetchGateQueueBoundAndHandOff(t *testing.T) {
	g := newFetchGate(1, 1<<30, 1)
	release, err := g.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		r, err := g.acquire(context.Background(), 1)
		if err == nil {
			r()
		}
		got <- err
	}()
	waitFor(t, func() bool { _, _, q := g.inFlight(); return q == 1 })
	if !g.shedding() {
		t.Fatal("shedding() false with a full queue")
	}
	if _, err := g.acquire(context.Background(), 1); !errors.Is(err, ErrBusy) {
		t.Fatalf("acquire beyond the queue = %v, want ErrBusy", err)
	}
	release()
	if err := <-got; err != nil {
		t.Fatalf("the waiter got %v after a release", err)
	}
	if n, b, q := g.inFlight(); n != 0 || b != 0 || q != 0 {
		t.Fatalf("gate not empty at the end: %d %d %d", n, b, q)
	}
}

// Waiters are served in order: a large fetch at the head is not overtaken by a smaller one that would fit
// beside the fetch in flight (60 + 30 <= 100, but 80 came first).
func TestFetchGateIsFIFO(t *testing.T) {
	g := newFetchGate(4, 100, 4)
	r1, _ := g.acquire(context.Background(), 60)
	order := make(chan int64, 2)
	for _, b := range []int64{80, 30} {
		go func() {
			r, err := g.acquire(context.Background(), b)
			if err != nil {
				t.Error(err)
				return
			}
			order <- b
			time.Sleep(time.Millisecond)
			r()
		}()
		waitFor(t, func() bool { _, _, q := g.inFlight(); return q >= 1 })
	}
	waitFor(t, func() bool { _, _, q := g.inFlight(); return q == 2 })
	r1()
	if first, second := <-order, <-order; first != 80 || second != 30 {
		t.Fatalf("served %d then %d, want 80 then 30", first, second)
	}
}

// Each invariant assertion can fire.
func TestFetchGateInvariantFires(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out")
	}
	for name, g := range map[string]*fetchGate{
		"over count": {maxN: 1, maxBytes: 10, n: 2},
		"over bytes": {maxN: 4, maxBytes: 10, n: 2, bytes: 11},
		"over queue": {maxN: 1, maxBytes: 10, n: 1, queue: []*waiter{{}}},
		"head fits":  {maxN: 2, maxBytes: 10, maxQueue: 1, n: 1, queue: []*waiter{{bytes: 1}}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invariant did not fire")
				}
			}()
			g.invariant()
		})
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 5 s")
		}
		time.Sleep(time.Millisecond)
	}
}
