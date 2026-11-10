package vactor_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// hangingStore hangs every upload of one key until the caller's ctx ends (a PUT that stalls forever).
type hangingStore struct {
	durable.Store
	key string
}

func (s hangingStore) PutGeneration(ctx context.Context, m durable.Manifest, path string) error {
	if m.Key == s.key {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.Store.PutGeneration(ctx, m, path)
}

func newHungRig(t *testing.T) *drig {
	t.Helper()
	r := newDurableRealTime(t, func(o *vactor.Options) {
		o.WriteBackTimeout = 100 * time.Millisecond
		o.Policy = vactor.Policy{WriteBackCommits: 1, IdleAfter: 30 * time.Second}
		o.Store = hangingStore{Store: o.Store, key: "hung"}
	})
	return r
}

// TestTickHungWriteBackDoesNotStopMaintenance (plan RI-05): one key's upload hangs. The tick still writes back
// the other dirty key and passivates the idle one, ends after the job deadline (not never), reports the hung
// key as timed out with the database still warm, and the next tick runs.
func TestTickHungWriteBackDoesNotStopMaintenance(t *testing.T) {
	r := newHungRig(t)
	r.get("idle")
	r.clk.Add(time.Minute)
	r.put("hung", "h1")
	r.put("ok", "o1")

	for tick := range 2 {
		t0 := time.Now()
		rep, err := r.h.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(t0); d > time.Second {
			t.Fatalf("tick %d took %v; a hung job must not hold it past its deadline", tick, d)
		}
		if !slices.Equal(rep.TimedOut, []string{"hung"}) || !errors.Is(rep.Errors["hung"], context.DeadlineExceeded) {
			t.Fatalf("tick %d: TimedOut %v, errors %v", tick, rep.TimedOut, rep.Errors)
		}
		if rep.Took <= 0 {
			t.Fatalf("tick %d: Took %v", tick, rep.Took)
		}
	}
	if got := r.published("ok"); got != "o1" {
		t.Fatalf("the healthy key was not written back: %q", got)
	}
	if r.published("hung") != "" || r.local("hung") != "h1" {
		t.Fatal("the hung key lost its local copy or published something")
	}
	if exists(r.h.LocalPath("idle")) {
		t.Fatal("the idle key was not passivated")
	}
}

// RunMaintenance keeps ticking past a hung job; ticks do not overlap, and a tick longer than the interval
// reports the intervals it missed.
func TestRunMaintenanceSurvivesHungJob(t *testing.T) {
	r := newHungRig(t)
	r.put("hung", "h1")
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	reports := make(chan vactor.TickReport, 16)
	done := make(chan error, 1)
	go func() {
		done <- r.h.RunMaintenance(c, 20*time.Millisecond, func(tr vactor.TickReport) { reports <- tr })
	}()
	for i := range 3 {
		select {
		case tr := <-reports:
			if len(tr.TimedOut) != 1 || tr.Missed < 1 {
				t.Fatalf("report %d: TimedOut %v Missed %d; want the hung key and ≥ 1 missed interval", i, tr.TimedOut, tr.Missed)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("maintenance stalled after %d ticks", i)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunMaintenance = %v", err)
	}
}

// Spec table for oldest_dirty_age and last_writeback_ok: nothing dirty → 0; dirty for 7 s → 7 s, the oldest
// of several wins; a write-back resets it and sets LastWriteBackOK.
func TestStalenessGauges(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) { o.Policy = vactor.Policy{WriteBackCommits: 100} })
	tick := func() vactor.TickReport {
		t.Helper()
		rep, err := r.h.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	r.get("a")
	r.get("b")
	if age := tick().OldestDirtyAge; age != 0 {
		t.Fatalf("clean keys: age %v, want 0", age)
	}
	if !r.h.Stats().LastWriteBackOK.IsZero() {
		t.Fatal("LastWriteBackOK set before any write-back")
	}
	r.put("a", "1")
	r.clk.Add(3 * time.Second)
	r.put("b", "1") // b's last write-back (activation) is as old as a's
	r.clk.Add(4 * time.Second)
	if rep := tick(); rep.OldestDirtyAge != 7*time.Second || rep.Dirty != 2 {
		t.Fatalf("dirty for 7 s: age %v, %d dirty (want 2)", rep.OldestDirtyAge, rep.Dirty)
	}
	if _, err := r.h.WriteBack(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if got := r.h.Stats().LastWriteBackOK; !got.Equal(r.clk.Now()) {
		t.Fatalf("LastWriteBackOK %v, want %v", got, r.clk.Now())
	}
	if _, err := r.h.WriteBack(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	r.put("a", "2")
	r.clk.Add(time.Second)
	if rep := tick(); rep.OldestDirtyAge != time.Second || rep.Dirty != 1 || len(r.h.Open()) != 2 {
		t.Fatalf("dirty again 1 s after a write-back: age %v, %d dirty (want 1), %d open (want 2)", rep.OldestDirtyAge, rep.Dirty, len(r.h.Open()))
	}
}
