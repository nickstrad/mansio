package owner_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
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

// orderPlacement records the keys Get was called for, in order.
type orderPlacement struct {
	owner.Placement
	mu   sync.Mutex
	keys []string
}

func (o *orderPlacement) Get(ctx context.Context, key string) (owner.Place, error) {
	o.mu.Lock()
	o.keys = append(o.keys, key)
	o.mu.Unlock()
	return o.Placement.Get(ctx, key)
}

// A round reconciles keys in key order, not map order (DST: a run must replay from its seed; with one worker
// the order decides which keys an outage or a breaker trip hits). 16 keys sorted by chance: ~1 in 2e13.
func TestReconcileAllGoesInKeyOrder(t *testing.T) {
	w, keys := restarted(t, 16)
	op := &orderPlacement{Placement: w.pl}
	w.procs["a"].pl.Placement = op
	_, err := w.tab("a").ReconcileAll(ctx, owner.ReconcileOptions{Workers: 1})
	must(t, err)
	if !slices.Equal(op.keys, keys) {
		t.Fatalf("reconciled %v, want key order %v", op.keys, keys)
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

// A publish the crashed incarnation still had in flight (conditional on the manifest version it read) lands
// after the restart (DST-1 zombie publishes, S3 completing a killed process's PUT). Reconcile's reopen stamps
// the manifest at the same epoch, a new version, so the zombie fails and the restarted owner's next publish
// is not refused as another writer's. The epoch does not move: no placement write, tickets stay valid.
func TestReopenStampFencesTheCrashedIncarnationsInFlightPublish(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	must(t, w.write("a", "k", 1, "w1"))
	_, _, v, _, _ := w.objs.Read("k") // the publish in flight read this version...
	g, err := w.objs.Upload("k", w.procs["a"].disk.Contents("k"))
	must(t, err) // ...and uploaded; then the process died before the publish landed

	w.start("a") // restart: loads ACTIVE(1) as Closed
	must(t, w.tab("a").Reconcile(ctx, "k"))
	if st := w.tab("a").State("k"); st.State != owner.Active || st.Epoch != 1 {
		t.Fatalf("after reconcile: %s, want active at epoch 1", st)
	}
	if err := w.objs.Publish("k", v, g, 1); !errors.Is(err, owner.ErrStale) {
		t.Fatalf("the crashed incarnation's publish landed after the reopen: %v, want ErrStale", err)
	}
	must(t, w.write("a", "k", 1, "w2"))
	if _, err := w.tab("a").Publish(ctx, "k", 1, func(ctx context.Context) (owner.Generation, error) {
		return w.procs["a"].wb.Publish(ctx, "k", 1)
	}); err != nil {
		t.Fatalf("the restarted owner's publish: %v", err)
	}
	if p, _ := w.pl.Row("k"); p.Epoch != 1 {
		t.Fatalf("placement epoch %d: the reopen must not move it", p.Epoch)
	}
}
