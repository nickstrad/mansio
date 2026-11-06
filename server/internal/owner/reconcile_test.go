package owner_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/retry"
)

// restarted places n keys on "a", writes once to each, then restarts "a": every key is Closed.
func restarted(t *testing.T, n int) (*world, []string) {
	t.Helper()
	w := newWorld(t, "a")
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%02d", i)
		w.place(keys[i], "a")
		must(t, w.write("a", keys[i], 1, "w"))
	}
	w.start("a")
	return w, keys
}

// RI-07: one slow authority row does not hold the others. Reconciled keys admit while the slow one is still
// pending (ErrUnavailable), and the round ends at the per-key timeout, not when the slow row answers.
func TestServesReconciledKeysWhileOthersPending(t *testing.T) {
	w, keys := restarted(t, 20)
	a := w.tab("a")
	w.pl.SetKeyDelay("k00", time.Hour)
	done := make(chan error, 1)
	var rep owner.ReconcileReport
	t0 := time.Now()
	go func() {
		var err error
		rep, err = a.ReconcileAll(ctx, owner.ReconcileOptions{Workers: 4, PerKey: 300 * time.Millisecond})
		done <- err
	}()
	for _, k := range keys[1:] { // served while k00 is still pending
		deadline := time.Now().Add(250 * time.Millisecond)
		for a.State(k).State != owner.Active && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		must(t, w.write("a", k, 1, "w2"))
	}
	if _, err := a.Admit("k00", 1); err != owner.ErrUnavailable {
		t.Fatalf("pending key: Admit = %v, want ErrUnavailable", err)
	}
	select {
	case <-done:
		t.Fatal("round ended before the slow key's timeout")
	default:
	}
	err := <-done
	if took := time.Since(t0); took > time.Second {
		t.Fatalf("round took %v; the per-key timeout is 300ms", took)
	}
	mustIs(t, err, context.DeadlineExceeded)
	if rep.Tried != 20 || rep.Failed != 1 {
		t.Fatalf("report %+v", rep)
	}
	if _, err := a.Admit("k00", 1); err != owner.ErrUnavailable {
		t.Fatalf("after the round: Admit = %v, want ErrUnavailable", err)
	}
}

// countingPlacement records the most Get calls in flight at once.
type countingPlacement struct {
	owner.Placement
	now, peak atomic.Int64
}

func (c *countingPlacement) Get(ctx context.Context, key string) (owner.Place, error) {
	n := c.now.Add(1)
	defer c.now.Add(-1)
	for p := c.peak.Load(); n > p && !c.peak.CompareAndSwap(p, n); p = c.peak.Load() {
	}
	return c.Placement.Get(ctx, key)
}

func TestReconcileAllBoundsConcurrency(t *testing.T) {
	for _, workers := range []int{1, 4} {
		w, keys := restarted(t, 16)
		cp := &countingPlacement{Placement: w.pl}
		w.procs["a"].pl.Placement = cp
		w.pl.SetDelay(5 * time.Millisecond)
		rep, err := w.tab("a").ReconcileAll(ctx, owner.ReconcileOptions{Workers: workers})
		must(t, err)
		if rep.Tried != len(keys) || rep.Failed != 0 || cp.peak.Load() != int64(workers) {
			t.Fatalf("workers %d: report %+v, peak %d", workers, rep, cp.peak.Load())
		}
	}
}

// The background loop retries with backoff until every key is reconciled; it reports each round.
func TestReconcileUntilDone(t *testing.T) {
	w, _ := restarted(t, 3)
	w.pl.SetDown(true)
	var mu sync.Mutex
	var rounds []owner.ReconcileReport
	o := owner.ReconcileOptions{PerKey: 50 * time.Millisecond}
	o.Backoff.Base, o.Backoff.Max = time.Millisecond, 10*time.Millisecond
	done := make(chan error, 1)
	go func() {
		done <- w.tab("a").ReconcileUntilDone(ctx, o, func(r owner.ReconcileReport, err error) {
			mu.Lock()
			defer mu.Unlock()
			rounds = append(rounds, r)
			if len(rounds) == 3 {
				w.pl.SetDown(false)
			}
			if (err == nil) != (r.Failed == 0) {
				t.Errorf("round %d: failed %d but err %v", r.Round, r.Failed, err)
			}
		})
	}()
	must(t, <-done)
	if len(rounds) < 4 || rounds[0].Failed != 3 || rounds[len(rounds)-1].Failed != 0 || rounds[len(rounds)-1].Round != len(rounds) {
		t.Fatalf("rounds %+v", rounds)
	}
	if _, err := w.tab("a").Admit("k00", 1); err != nil {
		t.Fatal(err)
	}

	w2, _ := restarted(t, 1)
	w2.pl.SetDown(true)
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	err := w2.tab("a").ReconcileUntilDone(cctx, o, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled loop: %v", err)
	}
}

// OnChange sees every state transition (for the agent's OWNER.* logs and counters), but not a generation-only
// update and not the states Open loads.
func TestOnChangeSeesTransitions(t *testing.T) {
	w := newWorld(t, "a", "b")
	var mu sync.Mutex
	var seen []string
	w.procs["a"].cfgMod = func(c *owner.Config) {
		c.OnChange = func(key string, from, to owner.Local) {
			mu.Lock()
			seen = append(seen, fmt.Sprintf("%s:%s>%s", key, from.State, to.State))
			mu.Unlock()
		}
	}
	a := w.start("a")
	w.place("k", "a") // Absent → Active
	_, err := a.Publish(ctx, "k", 1, func(context.Context) (owner.Generation, error) { return 9, nil })
	must(t, err)     // generation only: no callback
	a = w.start("a") // restart: loaded Closed, no callback
	w.pl.Create("k", owner.Place{Owner: "b", Epoch: 2, State: owner.PlaceActive})
	must(t, a.Reconcile(ctx, "k")) // Closed → Fenced (superseded)
	mu.Lock()
	defer mu.Unlock()
	want := []string{"k:absent>active", "k:closed>fenced"}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("seen %v, want %v", seen, want)
	}
	if !a.State("k").Superseded {
		t.Fatal("not superseded")
	}
}

// DST-0: on a simulated clock and a seeded Rand the backoff between rounds is exact and the same every run;
// time moves only when the test advances it.
func TestReconcileUntilDoneOnSimulatedClock(t *testing.T) {
	run := func() []time.Duration {
		w, _ := restarted(t, 2)
		w.pl.SetDown(true)
		clk := dettest.NewClock(time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
		o := owner.ReconcileOptions{Workers: 1, Clock: clk, Rand: det.NewRand(42)}
		reports := make(chan owner.ReconcileReport, 8)
		done := make(chan error, 1)
		go func() {
			done <- w.tab("a").ReconcileUntilDone(ctx, o, func(r owner.ReconcileReport, _ error) { reports <- r })
		}()
		var waits []time.Duration
		for round := 1; ; round++ {
			r := <-reports
			if r.Round != round || r.Took != 0 {
				t.Fatalf("report %+v: want round %d taking no simulated time", r, round)
			}
			if r.Failed == 0 {
				break
			}
			if !clk.AwaitPending(1) {
				t.Fatalf("round %d: no backoff timer", round)
			}
			next, _ := clk.Next()
			wait := next.Sub(clk.Now())
			waits = append(waits, wait)
			clk.Advance(wait - time.Nanosecond)
			select {
			case r := <-reports:
				t.Fatalf("round %d started before its backoff ended", r.Round)
			default:
			}
			if round == 3 {
				w.pl.SetDown(false)
			}
			clk.Advance(time.Nanosecond)
		}
		must(t, <-done)
		return waits
	}
	a, b := run(), run()
	p := retry.Policy{Backoff: retry.Exponential, Jitter: retry.Equal, Base: 100 * time.Millisecond, Max: 5 * time.Second}
	must(t, p.Normalize())
	rng := rand.New(det.NewRand(42))
	var prev time.Duration
	for i := range a {
		prev = p.Delay(rng, i+2, prev)
		if a[i] != b[i] || a[i] != prev {
			t.Fatalf("waits %v and %v; wait %d want %v", a, b, i, prev)
		}
	}
	if len(a) != 3 {
		t.Fatalf("waits %v: want 3 backoffs", a)
	}
}
