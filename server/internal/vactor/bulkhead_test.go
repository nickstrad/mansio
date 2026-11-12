package vactor_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/vactortest"
)

// A store slower than the activation deadline: Get fails with ErrBusy within the deadline (+ε), leaves
// nothing behind, and the next Get starts a fresh activation that succeeds once the store is fast again
// (Release It T1, plan RI-02).
func TestActivationTimesOutAndIsRetryable(t *testing.T) {
	const timeout = 100 * time.Millisecond
	r := newDurableRealTime(t, func(o *vactor.Options) { o.ActivationTimeout = timeout })
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
	if st := r.h.Stats(); st.TombstoneRefusals != 1 || st.FetchShed != 0 {
		t.Fatalf("refusals %+v, want one tombstone refusal", st)
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
	r := newDurableRealTime(t, func(o *vactor.Options) {
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
	vactor.WaitFor(t, func() bool { n, _, q := vactor.FetchGate(h); return n == 1 && q == 1 })

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
	if st, g := h.Stats(), h.Gauges(); st.FetchShed != 1 || g.FetchQueueCap != 1 || len(h.OpenDBs()) != 0 {
		t.Fatalf("shed %d (want 1), queue cap %d (want 1), open %d (want 0: both still activating)", st.FetchShed, g.FetchQueueCap, len(h.OpenDBs()))
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

// A first query that hits its own QueryTimeout is not the activation deadline: not ErrBusy (the
// activation ctx's cause tells the two apart, not the error chain).
func TestQueryTimeoutIsNotActivationTimeout(t *testing.T) {
	f, o := vactortest.NewFetcher(), &vactortest.Opener{}
	f.Put("a", []byte("x"))
	h, err := vactor.NewHost(f, o, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), QueryTimeout: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	_, err = h.Get(ctx, "a")
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, vactor.ErrBusy) {
		t.Fatalf("Get = %v, want the query's DeadlineExceeded and not ErrBusy", err)
	}
}

// A plain Fetcher that is a Sizer is bounded by bytes too, like durable mode: with 100-byte objects and
// MaxFetchBytes 250 (on point: 2 fit, the third would make 300), only 2 of 8 cold keys fetch at once, though
// MaxFetches would allow 16. This is what keeps many large plain fetches inside objstore's per-transfer
// deadline (docs/notes/coldload 2026-11-05 RI-02 note).
func TestFetchBulkheadBoundsPlainFetchesByBytesWithSizer(t *testing.T) {
	const keys = 8
	f, o := vactortest.NewFetcher(), &vactortest.Opener{}
	for i := range keys {
		f.Put(fmt.Sprint(i), make([]byte, 100))
	}
	hold := make(chan struct{})
	f.Hold = hold
	h, err := vactor.NewHost(vactortest.SizedFetcher{Fetcher: f}, o, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"),
		MaxFetches: 16, MaxFetchBytes: 250})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	errc := make(chan error, keys)
	for i := range keys {
		go func() { _, err := h.Get(ctx, fmt.Sprint(i)); errc <- err }()
	}
	vactor.WaitFor(t, func() bool { _, _, q := vactor.FetchGate(h); return q == keys-2 })
	if n, b, _ := vactor.FetchGate(h); n != 2 || b != 200 {
		t.Fatalf("in flight: %d fetches, %d bytes; want 2 and 200", n, b)
	}
	close(hold)
	for range keys {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	if n, b, q := vactor.FetchGate(h); n != 0 || b != 0 || q != 0 {
		t.Fatalf("bulkhead not empty after the fetches: %d %d %d", n, b, q)
	}
}

// A Sizer's answer sets the activation deadline as the manifest's size does in durable mode, and a missing
// object fails at the size lookup with ErrNotFound, before any fetch.
func TestSizerGrowsDeadlineAndReportsMissing(t *testing.T) {
	f, o := vactortest.NewFetcher(), &vactortest.Opener{}
	f.Put("a", make([]byte, 1<<10))
	f.Delay = 80 * time.Millisecond // > ActivationTimeout, < it + 1 KiB at 10 KiB/s
	h, err := vactor.NewHost(vactortest.SizedFetcher{Fetcher: f}, o, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"),
		ActivationTimeout: 50 * time.Millisecond, MinThroughput: 10 << 10, FailBackoff: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	if _, err := h.Get(ctx, "a"); err != nil {
		t.Fatalf("Get within the size-derived deadline: %v", err)
	}
	if _, err := h.Get(ctx, "missing"); !errors.Is(err, vactor.ErrNotFound) || f.Calls("missing") != 0 {
		t.Fatalf("Get(missing) = %v after %d fetches; want ErrNotFound and no fetch", err, f.Calls("missing"))
	}
}

// RI-16: SetFetchLimit at run time. With 4 slots and 2 cold fetches held in flight, lowering the limit to 1
// lets both finish but holds the next 2 cold keys queued until fewer than 1 run; then they fetch one at a
// time. Raising it back lets queued keys start together.
func TestSetFetchLimitAppliesToTheNextFetches(t *testing.T) {
	f, o := vactortest.NewFetcher(), &vactortest.Opener{}
	for _, k := range []string{"a", "b", "c", "d", "e", "g"} {
		f.Put(k, []byte("db:"+k))
	}
	hold := make(chan struct{})
	f.Hold = hold
	h, err := vactor.NewHost(f, o, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), MaxFetches: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	get := func(keys ...string) chan error {
		done := make(chan error, len(keys))
		for _, k := range keys {
			go func() { _, err := h.Get(ctx, k); done <- err }()
		}
		return done
	}
	first := get("a", "b")
	vactor.WaitFor(t, func() bool { n, _, _ := vactor.FetchGate(h); return n == 2 })
	if prev := h.SetFetchLimit(1); prev != 4 || h.FetchLimit() != 1 {
		t.Fatalf("SetFetchLimit(1) = previous %d, FetchLimit %d", prev, h.FetchLimit())
	}
	next := get("c", "d")
	vactor.WaitFor(t, func() bool { _, _, q := vactor.FetchGate(h); return q == 2 })
	if n, _, _ := vactor.FetchGate(h); n != 2 || f.Calls("c")+f.Calls("d") != 0 {
		t.Fatalf("lowering the limit: %d in flight, next fetches started %d; want the 2 running kept, none new", n, f.Calls("c")+f.Calls("d"))
	}
	stop, peak := make(chan struct{}), make(chan int)
	go func() { // the peak once a's and b's fetches are gone
		p := 0
		for {
			select {
			case <-stop:
				peak <- p
				return
			default:
			}
			if f.Calls("a")+f.Calls("b") == 2 && len(first) == 2 {
				n, _, _ := vactor.FetchGate(h)
				p = max(p, n)
			}
			time.Sleep(100 * time.Microsecond) // sample, do not spin against the locks under test
		}
	}()
	close(hold)
	for _, done := range []chan error{first, next} {
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	if p := <-peak; p > 1 {
		t.Fatalf("after lowering the limit to 1, %d fetches ran at once", p)
	}

	hold2 := make(chan struct{})
	f.Hold = hold2
	h.SetFetchLimit(2)
	last := get("e", "g")
	vactor.WaitFor(t, func() bool { n, _, _ := vactor.FetchGate(h); return n == 2 })
	close(hold2)
	for range 2 {
		if err := <-last; err != nil {
			t.Fatal(err)
		}
	}
}
