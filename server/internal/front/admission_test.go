package front

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
)

// Under concurrency the caps hold: never more than DBInFlight running per database, and every caller ends in
// run, shed or expired with nothing leaked.
func TestAdmissionConcurrentCapsHold(t *testing.T) {
	a := NewAdmission(AdmissionLimits{TenantInFlight: 4, TenantQueue: 8, DBInFlight: 2, DBQueue: 4})
	var running [3]atomic.Int32
	var ok, shedN, expired atomic.Int32
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			db := []string{"a", "b", "c"}[i%3]
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(1+i%20)*time.Millisecond)
			defer cancel()
			rel, err := a.Acquire(ctx, "t", db)
			switch {
			case errors.Is(err, ErrShed):
				shedN.Add(1)
				return
			case errors.Is(err, ErrExpired):
				expired.Add(1)
				return
			case err != nil:
				t.Error(err)
				return
			}
			ok.Add(1)
			if n := running[i%3].Add(1); n > 2 {
				t.Errorf("db %s: %d running, cap 2", db, n)
			}
			time.Sleep(time.Millisecond)
			running[i%3].Add(-1)
			rel()
			rel() // idempotent
		})
	}
	wg.Wait()
	if s := a.Stats(); !s.Idle() {
		t.Fatalf("state left: %+v", s)
	}
	if ok.Load()+shedN.Load()+expired.Load() != 200 || ok.Load() == 0 || shedN.Load() == 0 {
		t.Fatalf("ok %d shed %d expired %d", ok.Load(), shedN.Load(), expired.Load())
	}
}

// A waiter whose deadline passes leaves the queue (expired), and a later release does not hand it a slot.
func TestAdmissionWaiterExpires(t *testing.T) {
	a := NewAdmission(AdmissionLimits{TenantInFlight: 1, TenantQueue: 1, DBInFlight: 1, DBQueue: 1})
	rel, err := a.Acquire(context.Background(), "t", "d")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.Acquire(ctx, "t", "d"); !errors.Is(err, ErrExpired) {
		t.Fatalf("err %v", err)
	}
	if s := a.Stats(); s.Queued != 0 || s.InFlight != 1 {
		t.Fatalf("%+v", s)
	}
	// B1: both requests are in the wait histogram, the first at 0 and the second at its ~20 ms in the queue.
	_, counts, sum := a.Wait().Snapshot()
	if counts[0] != 1 || sum < 20*time.Millisecond {
		t.Fatalf("wait histogram %v (sum %v), want one immediate and one of >= 20ms", counts, sum)
	}
	if s, want := a.Stats(), (AdmissionLimits{DBInFlight: 1}).WithDefaults().VMQueue; s.QueueCap != want {
		t.Fatalf("queue cap %d, want the VM queue's default %d", s.QueueCap, want)
	}
	rel()
	if s := a.Stats(); !s.Idle() {
		t.Fatalf("%+v", s)
	}
}

// Granted at the same moment the waiter gives up: the slot goes straight back.
func TestAdmissionGrantRacingCancelReturnsTheSlot(t *testing.T) {
	a := NewAdmission(AdmissionLimits{TenantInFlight: 1, TenantQueue: 1, DBInFlight: 1, DBQueue: 1})
	for range 200 {
		rel, _ := a.Acquire(context.Background(), "t", "d")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error)
		go func() {
			r2, err := a.Acquire(ctx, "t", "d")
			if err == nil {
				r2()
			}
			done <- err
		}()
		for a.Stats().Queued == 0 {
			time.Sleep(10 * time.Microsecond)
		}
		go cancel()
		rel()
		<-done
		if s := a.Stats(); !s.Idle() {
			t.Fatalf("slot leaked: %+v", s)
		}
	}
}

// A request that would finish after its deadline is shed at once with the expected wait as its hint, and
// counted apart from queue-full sheds; one with room in its deadline queues; without a deadline or a measured
// rate nothing changes.
func TestAdmissionShedsWhatWouldExpire(t *testing.T) {
	clock := dettest.NewClock(time.Unix(1_800_000_000, 0))
	a := newAdmission(AdmissionLimits{DBInFlight: 2, DBQueue: 100}, clock)
	bg := context.Background()
	within := func(d time.Duration) context.Context {
		ctx, cancel := det.WithDeadline(bg, clock, clock.Now().Add(d))
		t.Cleanup(cancel)
		return ctx
	}
	// The database stays busy (its rate lives as long as it holds or queues something): one slot is held
	// throughout while the other completes 10 requests in one second; the next second begins with both taken.
	rel1, _ := a.Acquire(bg, "t", "d")
	for range 10 {
		rel, err := a.Acquire(bg, "t", "d")
		if err != nil {
			t.Fatal(err)
		}
		rel()
	}
	clock.Advance(time.Second)
	rel2, _ := a.Acquire(bg, "t", "d")

	// Expected finish with an empty queue: (0 + 1 + 2) / 10 per s = 300 ms. A 300 ms deadline is the on point.
	_, err := a.Acquire(within(299*time.Millisecond), "t", "d")
	var h RetryHint
	if !errors.Is(err, ErrShed) || !errors.As(err, &h) || h.RetryAfter() != 100*time.Millisecond {
		t.Fatalf("a request that would finish 1 ms late: %v, want shed with the 100 ms expected wait", err)
	}
	if s := a.Stats(); s.WouldExpire != 1 || s.Shed != 0 || s.Queued != 0 {
		t.Fatalf("%+v: want one would-expire refusal, no queue-full shed, nothing queued", s)
	}
	queued := make(chan error, 2)
	// Just in time (the on point), then one with no deadline at all behind it: both queue.
	for i, ctx := range []context.Context{within(300 * time.Millisecond), bg} {
		go func() {
			rel, err := a.Acquire(ctx, "t", "d")
			if err == nil {
				rel()
			}
			queued <- err
		}()
		for deadline := time.Now().Add(5 * time.Second); a.Stats().Queued != i+1; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("request %d did not queue: %+v", i, a.Stats())
			}
		}
	}
	rel1()
	rel2()
	for range 2 {
		if err := <-queued; err != nil {
			t.Fatalf("a request with room in its deadline (or none): %v", err)
		}
	}
	if s := a.Stats(); !s.Idle() || s.WouldExpire != 1 {
		t.Fatalf("%+v", s)
	}
}

// B7's overload scenario: one database (2 slots, 50 ms per request: 40/s) offered 200 requests per second
// with 300 ms deadlines for 2.5 s. Before, admission queued whatever fit its fixed queue (256): the queue held
// about 6 s of work for requests with 0.3 s to live, so nearly every queued request answered expired after its
// whole deadline. Now, once a second of completions is measured, a request that cannot finish in time is shed
// at once with a retry hint: the queue stays near deadline × rate, and expiries all but stop.
func TestOverloadShedsInsteadOfExpiring(t *testing.T) {
	if testing.Short() {
		t.Skip("runs 2.5 s of load")
	}
	const (
		service  = 50 * time.Millisecond
		deadline = 300 * time.Millisecond
		offered  = 200 // per second
		warm     = 1200 * time.Millisecond
		total    = 2500 * time.Millisecond
	)
	a := NewAdmission(AdmissionLimits{DBInFlight: 2, DBQueue: 256})
	var ok, expired, wouldExpire, full, maxQueued atomic.Int64
	var shedTook atomic.Int64 // the slowest would-expire refusal, ns
	var wg sync.WaitGroup
	start := time.Now()
	tick := time.NewTicker(time.Second / offered)
	defer tick.Stop()
	for time.Since(start) < total {
		<-tick.C
		late := time.Since(start) >= warm // only the steady state is judged: the first second has no rate yet
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			before := a.Stats().WouldExpire
			t0 := time.Now()
			rel, err := a.Acquire(ctx, "t", "d")
			took := time.Since(t0)
			if q := int64(a.Stats().Queued); late {
				for m := maxQueued.Load(); q > m && !maxQueued.CompareAndSwap(m, q); m = maxQueued.Load() {
				}
			}
			switch {
			case err == nil:
				time.Sleep(service)
				rel()
				if late {
					ok.Add(1)
				}
			case !late:
			case errors.Is(err, ErrExpired):
				expired.Add(1)
			case a.Stats().WouldExpire > before && took < 20*time.Millisecond:
				wouldExpire.Add(1)
				for m := shedTook.Load(); int64(took) > m && !shedTook.CompareAndSwap(m, int64(took)); m = shedTook.Load() {
				}
			default:
				full.Add(1)
			}
		})
	}
	wg.Wait()
	t.Logf("steady state: %d ok, %d shed would_expire (slowest %v), %d expired, %d shed full, deepest queue %d; totals %+v",
		ok.Load(), wouldExpire.Load(), time.Duration(shedTook.Load()), expired.Load(), full.Load(), maxQueued.Load(), a.Stats())
	if wouldExpire.Load() < 100 {
		t.Errorf("%d would-expire refusals in the steady state, want most of the ~200 excess requests", wouldExpire.Load())
	}
	if expired.Load() > 10 {
		t.Errorf("%d requests expired in the queue in the steady state; they should have been refused at admission", expired.Load())
	}
	// deadline × rate = 0.3 s × 40/s = 12 requests can finish in time; the fixed cap is 256.
	if maxQueued.Load() > 24 {
		t.Errorf("deepest queue %d in the steady state, want about deadline × rate (12)", maxQueued.Load())
	}
	if ok.Load() < 30 {
		t.Errorf("only %d requests served in 1.3 s at 40/s capacity", ok.Load())
	}
}
