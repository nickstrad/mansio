package vactor_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
	"github.com/nickstrad/ts-db-go/internal/vactor"
	"github.com/nickstrad/ts-db-go/internal/vactor/vactortest"
)

// A store slower than the activation deadline: Get fails with ErrBusy within the deadline (+ε), leaves
// nothing behind, and the next Get starts a fresh activation that succeeds once the store is fast again
// (Release It T1, plan RI-02).
func TestActivationTimesOutAndIsRetryable(t *testing.T) {
	const timeout = 100 * time.Millisecond
	r := newDurable(t, func(o *vactor.Options) { o.ActivationTimeout = timeout })
	r.s.Seed("k", 0, []byte("v1"))
	r.s.SetFaults(durabletest.Faults{Delay: 10 * timeout})

	t0 := time.Now()
	_, err := r.h.Get(ctx, "k")
	took := time.Since(t0)
	if !errors.Is(err, vactor.ErrBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get = %v, want ErrBusy wrapping DeadlineExceeded", err)
	}
	mustModule(t, err, "deadline")
	if took > timeout+timeout/2 {
		t.Fatalf("Get took %v, want ≤ %v + ε", took, timeout)
	}
	if exists(r.h.LocalPath("k")) || r.h.Active() != 0 {
		t.Fatal("a timed-out activation left a file or an entry")
	}

	r.s.SetFaults(durabletest.Faults{})
	if _, err := r.h.Get(ctx, "k"); !errors.Is(err, vactor.ErrBusy) {
		t.Fatalf("Get inside the key's backoff = %v, want the same cause, at once", err)
	}
	r.clk.Add(time.Second)
	res, err := r.h.Get(ctx, "k")
	if err != nil || res.Act == nil {
		t.Fatalf("Get after the store recovered = %+v, %v; want a fresh activation", res, err)
	}
	if got := r.local("k"); got != "v1" {
		t.Fatalf("local copy %q, want v1", got)
	}
}

// The deadline grows with the generation's size (size/MinThroughput), so a large database is not cut off
// for being large: here 1 KiB at 10 KiB/s adds 100 ms to a 50 ms ActivationTimeout.
func TestActivationDeadlineAllowsForSize(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) {
		o.ActivationTimeout, o.MinThroughput = 50*time.Millisecond, 10<<10
	})
	r.s.Seed("k", 0, make([]byte, 1<<10))
	// Each store call takes 40 ms: manifest read + fetch = 80 ms > 50 ms but < 150 ms.
	r.s.SetFaults(durabletest.Faults{Delay: 40 * time.Millisecond})
	if _, err := r.h.Get(ctx, "k"); err != nil {
		t.Fatalf("Get of a 1 KiB generation within its size-derived deadline: %v", err)
	}
}

// With one fetch slot and a queue of one, a third cold key is shed at once with ErrBusy while the first is
// fetching and the second waits; nothing is started for it, and both others finish once the fetch is let go
// (Release It B1, B2; plan RI-02).
func TestFetchBulkheadShedsBeyondQueue(t *testing.T) {
	f, o := vactortest.NewFetcher(), &vactortest.Opener{}
	for _, k := range []string{"a", "b", "c"} {
		f.Put(k, []byte("db:"+k))
	}
	hold := make(chan struct{})
	f.Hold = hold
	h, err := vactor.NewHost(f, o, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), MaxFetches: 1, MaxFetchQueue: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })

	done := make(chan error, 2)
	for _, k := range []string{"a", "b"} {
		go func() { _, err := h.Get(ctx, k); done <- err }()
	}
	waitUntil(t, func() bool { n, _, q := vactor.FetchGate(h); return n == 1 && q == 1 })

	t0 := time.Now()
	_, err = h.Get(ctx, "c")
	if !errors.Is(err, vactor.ErrBusy) {
		t.Fatalf("Get beyond the queue = %v, want ErrBusy", err)
	}
	if d := time.Since(t0); d > 50*time.Millisecond {
		t.Fatalf("shedding took %v; it must not wait", d)
	}
	if f.Calls("c") != 0 || h.Active() != 2 {
		t.Fatalf("a shed Get started work: fetches(c)=%d active=%d", f.Calls("c"), h.Active())
	}

	close(hold)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("a queued Get failed: %v", err)
		}
	}
	if n, b, q := vactor.FetchGate(h); n != 0 || b != 0 || q != 0 {
		t.Fatalf("bulkhead not empty after the fetches: %d %d %d", n, b, q)
	}
	if _, err := h.Get(ctx, "c"); err != nil {
		t.Fatalf("Get after the queue drained: %v", err)
	}
}

// Many cold keys at once never have more than MaxFetches fetches in flight.
func TestFetchBulkheadBoundsConcurrency(t *testing.T) {
	const keys, limit = 40, 4
	f, o := vactortest.NewFetcher(), &vactortest.Opener{}
	for i := range keys {
		f.Put(fmt.Sprint(i), []byte("x"))
	}
	f.Delay = 5 * time.Millisecond
	h, err := vactor.NewHost(f, o, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), MaxFetches: limit})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	stop := make(chan struct{})
	peak := make(chan int)
	go func() {
		p := 0
		for {
			select {
			case <-stop:
				peak <- p
				return
			default:
			}
			n, _, _ := vactor.FetchGate(h)
			p = max(p, n)
		}
	}()
	errc := make(chan error, keys)
	for i := range keys {
		go func() { _, err := h.Get(ctx, fmt.Sprint(i)); errc <- err }()
	}
	for range keys {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	if p := <-peak; p > limit || p == 0 {
		t.Fatalf("peak fetches in flight %d, want 1..%d", p, limit)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 5 s")
		}
		time.Sleep(time.Millisecond)
	}
}
