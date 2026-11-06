package owner_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
)

var ctx = context.Background()

func TestClaimStampsStorageThenAdmitsOnlyItsEpoch(t *testing.T) {
	w := newWorld(t, "a", "b")
	w.place("k", "a")
	a, b := w.tab("a"), w.tab("b")
	if got := a.State("k"); got != (owner.Local{State: owner.Active, Epoch: 1}) {
		t.Fatalf("State = %s", got)
	}
	if _, e, _, _, _ := w.objs.Read("k"); e != 1 {
		t.Fatalf("manifest epoch = %d, want the claim's stamp 1", e)
	}
	if r, _ := w.procs["a"].store.Get("k"); r.State != owner.Active || r.Epoch != 1 {
		t.Fatalf("durable record = %+v", r)
	}
	p, err := a.Admit("k", 1)
	must(t, err)
	p.Done()
	for _, tc := range []struct {
		tab   *owner.Table
		key   string
		epoch owner.Epoch
		want  error
	}{
		{a, "k", 0, owner.ErrWrongOwner},
		{a, "k", 2, owner.ErrWrongOwner},
		{a, "other", 1, owner.ErrWrongOwner},
		{b, "k", 1, owner.ErrWrongOwner},
	} {
		if _, err := tc.tab.Admit(tc.key, tc.epoch); err != tc.want {
			t.Errorf("Admit(%s, %d) = %v, want %v", tc.key, tc.epoch, err, tc.want)
		}
	}
	// Idempotent.
	l, err := a.Claim(ctx, "k")
	if err != nil || l.Epoch != 1 {
		t.Fatalf("second Claim = %s, %v", l, err)
	}
}

func TestClaimRejects(t *testing.T) {
	t.Run("another owner", func(t *testing.T) {
		w := newWorld(t, "a", "b")
		w.place("k", "a")
		_, err := w.tab("b").Claim(ctx, "k")
		mustIs(t, err, owner.ErrWrongOwner)
		if w.tab("b").State("k").State != owner.Absent {
			t.Fatal("a rejected claim changed state")
		}
	})
	t.Run("move in progress", func(t *testing.T) {
		w := newWorld(t, "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceMoving})
		_, err := w.tab("a").Claim(ctx, "k")
		mustIs(t, err, owner.ErrMoving)
	})
	t.Run("authority down: nothing changed, wrapped", func(t *testing.T) {
		w := newWorld(t, "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
		w.pl.SetDown(true)
		_, err := w.tab("a").Claim(ctx, "k")
		mustIs(t, err, owner.ErrUnavailable)
		if e, ok := errs.Outermost(err); !ok || e.Module != "owner" || !strings.Contains(e.Message, "safe to retry") {
			t.Fatalf("not a well-formed owner error: %v", err)
		}
		if _, _, _, _, ok := w.objs.Read("k"); ok {
			t.Fatal("stamped storage although the authority was not read")
		}
	})
	t.Run("malformed authority row", func(t *testing.T) {
		w := newWorld(t, "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 0, State: owner.PlaceActive})
		_, err := w.tab("a").Claim(ctx, "k")
		mustIs(t, err, owner.ErrInvalid)
	})
	t.Run("storage has a newer epoch than the authority", func(t *testing.T) {
		w := newWorld(t, "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
		must(t, w.objs.Publish("k", 0, 0, 2))
		_, err := w.tab("a").Claim(ctx, "k")
		mustIs(t, err, owner.ErrUnavailable)
	})
	t.Run("storage behind the authority's generation", func(t *testing.T) {
		w := newWorld(t, "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 2, Gen: 5, State: owner.PlaceActive})
		_, err := w.tab("a").Claim(ctx, "k")
		mustIs(t, err, owner.ErrUnavailable)
	})
	t.Run("manifest unreadable", func(t *testing.T) {
		w := newWorld(t, "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
		w.objs.SetDown(true)
		_, err := w.tab("a").Claim(ctx, "k")
		mustIs(t, err, owner.ErrUnavailable)
	})
	t.Run("local record not written: not admitting", func(t *testing.T) {
		w := newWorld(t, "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
		w.procs["a"].store.Set(func(m *ownertest.MemStore) { m.FailPut = true })
		_, err := w.tab("a").Claim(ctx, "k")
		mustIs(t, err, ownertest.ErrDown)
		if _, err := w.tab("a").Admit("k", 1); err == nil {
			t.Fatal("admitted without a durable record")
		}
	})
	t.Run("bad keys", func(t *testing.T) {
		w := newWorld(t, "a")
		for _, k := range []string{"", strings.Repeat("x", owner.MaxKeyLen+1)} {
			_, err := w.tab("a").Claim(ctx, k)
			mustIs(t, err, owner.ErrInvalid)
		}
	})
	t.Run("stamp keeps losing races", func(t *testing.T) {
		w := newWorld(t, "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
		racy := &racyStamp{WriteBack: w.procs["a"].wb, losses: 10}
		w.procs["a"].cfgMod = func(c *owner.Config) { c.WriteBack = racy; c.StampRetries = 2 }
		w.start("a")
		_, err := w.tab("a").Claim(ctx, "k")
		mustIs(t, err, owner.ErrStale)
		if racy.stamps != 2 {
			t.Fatalf("stamps = %d, want 2 (StampRetries)", racy.stamps)
		}
		racy.losses = 1 // on point: one lost race, then success
		w.procs["a"].cfgMod = func(c *owner.Config) { c.WriteBack = racy }
		w.start("a")
		_, err = w.tab("a").Claim(ctx, "k")
		must(t, err)
	})
	t.Run("stamp fails for another reason", func(t *testing.T) {
		w := newWorld(t, "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
		w.procs["a"].cfgMod = func(c *owner.Config) {
			c.WriteBack = &racyStamp{WriteBack: w.procs["a"].wb, losses: 1, err: ownertest.ErrDown}
		}
		w.start("a")
		_, err := w.tab("a").Claim(ctx, "k")
		mustIs(t, err, ownertest.ErrDown)
	})
	t.Run("active at an older epoch must fence first", func(t *testing.T) {
		w := newWorld(t, "a")
		w.place("k", "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 2, State: owner.PlaceActive})
		_, err := w.tab("a").Claim(ctx, "k")
		mustIs(t, err, owner.ErrIllegal)
	})
}

type racyStamp struct {
	owner.WriteBack
	losses int
	err    error
	stamps int
}

func (r *racyStamp) Stamp(ctx context.Context, key string, e owner.Epoch, g owner.Generation) error {
	r.stamps++
	if r.losses > 0 {
		r.losses--
		if r.err != nil {
			return r.err
		}
		return owner.ErrStale
	}
	return r.WriteBack.Stamp(ctx, key, e, g)
}

func TestRestartStaysClosedUntilReconciled(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	must(t, w.write("a", "k", 1, "w1"))
	a := w.start("a") // crash + restart: memory gone, disk kept
	if _, err := a.Admit("k", 1); err != owner.ErrUnavailable {
		t.Fatalf("Admit after restart = %v, want ErrUnavailable", err)
	}
	_, err := a.Claim(ctx, "k")
	mustIs(t, err, owner.ErrUnavailable) // reconcile first
	w.procs["a"].pl.cut = true
	mustIs(t, second(a.ReconcileAll(ctx, owner.ReconcileOptions{})), owner.ErrUnavailable)
	if _, err := a.Admit("k", 1); err != owner.ErrUnavailable {
		t.Fatalf("Admit while partitioned = %v, want ErrUnavailable", err)
	}
	w.procs["a"].pl.cut = false
	must(t, second(a.ReconcileAll(ctx, owner.ReconcileOptions{})))
	must(t, w.write("a", "k", 1, "w2"))
	if got := w.procs["a"].disk.Contents("k"); !slices.Equal(got, []string{"w1", "w2"}) {
		t.Fatalf("local copy = %v", got)
	}
	must(t, a.Reconcile(ctx, "k"))     // already active: no-op
	must(t, a.Reconcile(ctx, "never")) // unknown key: no-op
}

func TestReconcileOutcomes(t *testing.T) {
	t.Run("moved away while down → fenced durably", func(t *testing.T) {
		w := newWorld(t, "a", "b")
		w.place("k", "a")
		w.pl.Create("k", owner.Place{Owner: "b", Epoch: 2, State: owner.PlaceActive}) // operator restore
		a := w.start("a")
		must(t, a.Reconcile(ctx, "k"))
		if got := a.State("k"); got.State != owner.Fenced || !got.Superseded {
			t.Fatalf("State = %s, want fenced superseded", got)
		}
		if r, _ := w.procs["a"].store.Get("k"); r.State != owner.Fenced || !r.Superseded {
			t.Fatalf("record = %+v", r)
		}
		_, err := a.Fence(ctx, "k")
		mustIs(t, err, owner.ErrSuperseded) // no ack for a superseded key
	})
	t.Run("dropped → fenced", func(t *testing.T) {
		w := newWorld(t, "a")
		w.place("k", "a")
		w.pl = ownertest.NewPlacement()
		w.procs["a"].pl.Placement = w.pl
		a := w.start("a")
		must(t, a.Reconcile(ctx, "k"))
		if a.State("k").State != owner.Fenced {
			t.Fatalf("State = %s", a.State("k"))
		}
	})
	t.Run("crashed mid-move → stays stopped; Fence finishes it", func(t *testing.T) {
		w := newWorld(t, "a")
		w.place("k", "a")
		must(t, w.write("a", "k", 1, "w1"))
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceMoving})
		a := w.start("a")
		must(t, a.Reconcile(ctx, "k"))
		if _, err := a.Admit("k", 1); err != owner.ErrMoving {
			t.Fatalf("Admit = %v, want ErrMoving", err)
		}
		ack, err := a.Fence(ctx, "k")
		must(t, err)
		if _, _, _, c, _ := w.objs.Read("k"); ack.Epoch != 1 || !slices.Equal(c, []string{"w1"}) {
			t.Fatalf("ack %+v, published %v", ack, c)
		}
	})
	t.Run("authority behind local → stays closed", func(t *testing.T) {
		w := newWorld(t, "a")
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 3, State: owner.PlaceActive})
		_, err := w.tab("a").Claim(ctx, "k")
		must(t, err)
		w.pl.Create("k", owner.Place{Owner: "a", Epoch: 2, State: owner.PlaceActive})
		a := w.start("a")
		mustIs(t, a.Reconcile(ctx, "k"), owner.ErrUnavailable)
		if a.State("k").State != owner.Closed {
			t.Fatalf("State = %s", a.State("k"))
		}
	})
	t.Run("fence record not written → stays closed", func(t *testing.T) {
		w := newWorld(t, "a")
		w.place("k", "a")
		w.pl.Create("k", owner.Place{Owner: "b", Epoch: 2, State: owner.PlaceActive})
		a := w.start("a")
		w.procs["a"].store.Set(func(m *ownertest.MemStore) { m.FailPut = true })
		mustIs(t, a.Reconcile(ctx, "k"), ownertest.ErrDown)
		if a.State("k").State != owner.Closed {
			t.Fatalf("State = %s", a.State("k"))
		}
	})
	t.Run("manifest unreadable → stays closed", func(t *testing.T) {
		w := newWorld(t, "a")
		w.place("k", "a")
		a := w.start("a")
		w.objs.SetDown(true)
		mustIs(t, a.Reconcile(ctx, "k"), owner.ErrUnavailable)
	})
}

// A stale authority read on restart says "still yours", but the new owner has stamped storage: the storage
// check catches it and the process fences itself instead of reopening.
func TestStaleAuthorityReadOnRestartIsCaughtByStorage(t *testing.T) {
	w := newWorld(t, "a", "b")
	w.place("k", "a")
	w.pl.SetStale(true) // Get keeps answering ACTIVE(a, 1)
	// Operator-attested restore to b while a looks dead (a's process is in fact only partitioned).
	w.pl.Create("k", owner.Place{Owner: "b", Epoch: 2, State: owner.PlaceActive})
	w.pl.SetStale(false)
	_, err := w.tab("b").Claim(ctx, "k")
	must(t, err)
	w.pl.SetStale(true)
	a := w.start("a")
	must(t, a.Reconcile(ctx, "k"))
	if got := a.State("k"); got.State != owner.Fenced || !got.Superseded {
		t.Fatalf("State = %s, want fenced superseded", got)
	}
}

func TestFence(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	must(t, w.write("a", "k", 1, "w1"))
	must(t, w.write("a", "k", 1, "w2"))
	ack, err := a.Fence(ctx, "k")
	must(t, err)
	gen, epoch, _, contents, _ := w.objs.Read("k")
	if ack != (owner.Ack{Key: "k", Epoch: 1, Gen: gen}) || epoch != 1 || !slices.Equal(contents, []string{"w1", "w2"}) {
		t.Fatalf("ack %+v; manifest g%d e%d %v", ack, gen, epoch, contents)
	}
	if _, err := a.Admit("k", 1); err != owner.ErrFenced {
		t.Fatalf("Admit after fence = %v", err)
	}
	if r, _ := w.procs["a"].store.Get("k"); r != (owner.Record{Key: "k", State: owner.Fenced, Epoch: 1, Gen: gen}) {
		t.Fatalf("record = %+v", r)
	}
	calls := w.procs["a"].wb.Calls
	again, err := a.Fence(ctx, "k")
	if err != nil || again != ack || w.procs["a"].wb.Calls != calls {
		t.Fatalf("second Fence = %+v %v, write-backs %d → %d; want the same ack and no new write-back", again, err, calls, w.procs["a"].wb.Calls)
	}
	a = w.start("a") // survives a restart
	if again, err := a.Fence(ctx, "k"); err != nil || again != ack {
		t.Fatalf("Fence after restart = %+v %v", again, err)
	}
	_, err = a.Fence(ctx, "never")
	mustIs(t, err, owner.ErrWrongOwner)
}

func TestFenceOnClosedKeyWritesBack(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	must(t, w.write("a", "k", 1, "w1"))
	a := w.start("a")
	ack, err := a.Fence(ctx, "k")
	must(t, err)
	if _, _, _, c, _ := w.objs.Read("k"); ack.Epoch != 1 || !slices.Equal(c, []string{"w1"}) {
		t.Fatalf("published %v", c)
	}
}

func TestFenceWriteBackFailureStaysStoppedUntilRetried(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	must(t, w.write("a", "k", 1, "w1"))
	w.procs["a"].wb.Set(func(wb *ownertest.WriteBack) { wb.FailPublish = true })
	_, err := a.Fence(ctx, "k")
	mustIs(t, err, ownertest.ErrDown)
	if !strings.Contains(err.Error(), "admission stays stopped") {
		t.Fatalf("message must say what state it left: %v", err)
	}
	if _, err := a.Admit("k", 1); err != owner.ErrMoving {
		t.Fatalf("Admit after failed fence = %v, want ErrMoving", err)
	}
	if r, _ := w.procs["a"].store.Get("k"); r.State != owner.Active {
		t.Fatalf("record = %+v; a failed fence must not record FENCED", r)
	}
	_, err = a.Claim(ctx, "k")
	mustIs(t, err, owner.ErrMoving) // cannot reopen by claiming the same epoch
	w.procs["a"].wb.Set(func(wb *ownertest.WriteBack) { wb.FailPublish = false })
	ack, err := a.Fence(ctx, "k")
	must(t, err)
	if _, _, _, c, _ := w.objs.Read("k"); ack.Epoch != 1 || !slices.Equal(c, []string{"w1"}) {
		t.Fatalf("published %v", c)
	}
}

func TestFenceRecordFailureRetriesWithoutRepublishing(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	w.procs["a"].store.Set(func(m *ownertest.MemStore) { m.FailPut = true })
	_, err := a.Fence(ctx, "k")
	mustIs(t, err, ownertest.ErrDown)
	if w.procs["a"].wb.Calls != 1 {
		t.Fatalf("write-backs = %d", w.procs["a"].wb.Calls)
	}
	w.procs["a"].store.Set(func(m *ownertest.MemStore) { m.FailPut = false })
	_, err = a.Fence(ctx, "k")
	must(t, err)
	if w.procs["a"].wb.Calls != 1 {
		t.Fatalf("retry re-published: write-backs = %d", w.procs["a"].wb.Calls)
	}
}

func TestFenceRejectsAGenerationThatWentBack(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	must(t, w.write("a", "k", 1, "w1"))
	a := w.tab("a")
	_, err := a.Publish(ctx, "k", 1, func(c context.Context) (owner.Generation, error) { return 9, nil })
	must(t, err)
	_, err = a.Fence(ctx, "k")
	mustIs(t, err, owner.ErrInvalid)
}

// The storage backstop (contract 1): two processes both think they own k. The lower epoch's publish fails,
// and the lower-epoch owner stops admitting and reports itself.
func TestStorageBackstopLowerEpochOwnerFencesItself(t *testing.T) {
	for _, order := range []string{"new owner stamps first", "zombie publishes first"} {
		t.Run(order, func(t *testing.T) {
			w := newWorld(t, "a", "b")
			w.place("k", "a")
			must(t, w.write("a", "k", 1, "a1"))
			// A retried placement / operator restore gives k to b at epoch 2 without fencing a.
			w.pl.Create("k", owner.Place{Owner: "b", Epoch: 2, State: owner.PlaceActive})
			a, b := w.tab("a"), w.tab("b")
			publishA := func() error {
				_, err := a.Publish(ctx, "k", 1, func(c context.Context) (owner.Generation, error) {
					return w.procs["a"].wb.Publish(c, "k", 1)
				})
				return err
			}
			if order == "zombie publishes first" {
				must(t, publishA()) // stored epoch is still 1: contract 1 allows it
			}
			_, err := b.Claim(ctx, "k")
			must(t, err)
			w.procs["b"].wb.Fetch("k")
			must(t, w.write("b", "k", 2, "b1"))
			_, err = b.Publish(ctx, "k", 2, func(c context.Context) (owner.Generation, error) {
				return w.procs["b"].wb.Publish(c, "k", 2)
			})
			must(t, err)
			// The zombie, still admitting at epoch 1, tries to publish again.
			must(t, w.write("a", "k", 1, "a2"))
			err = publishA()
			mustIs(t, err, owner.ErrSuperseded)
			mustIs(t, err, owner.ErrStale)
			if _, err := a.Admit("k", 1); err != owner.ErrFenced {
				t.Fatalf("zombie Admit = %v, want ErrFenced", err)
			}
			if r, _ := w.procs["a"].store.Get("k"); r.State != owner.Fenced || !r.Superseded {
				t.Fatalf("zombie record = %+v", r)
			}
			// Published history is linear: each publish extends the previous one.
			assertLinear(t, w.objs.History("k"))
			_, _, _, final, _ := w.objs.Read("k")
			want := []string{"b1"}
			if order == "zombie publishes first" {
				want = []string{"a1", "b1"} // b started from the zombie's generation
			}
			if !slices.Equal(final, want) {
				t.Fatalf("final = %v, want %v", final, want)
			}
		})
	}
}

func assertLinear(t testing.TB, hist [][]string) {
	t.Helper()
	for i := 1; i < len(hist); i++ {
		if len(hist[i]) < len(hist[i-1]) || !slices.Equal(hist[i][:len(hist[i-1])], hist[i-1]) {
			t.Fatalf("published history forked or lost writes at %d: %v → %v", i, hist[i-1], hist[i])
		}
	}
}

func TestFenceFinalWriteBackFindsHigherEpoch(t *testing.T) {
	w := newWorld(t, "a", "b")
	w.place("k", "a")
	w.pl.Create("k", owner.Place{Owner: "b", Epoch: 2, State: owner.PlaceActive})
	_, err := w.tab("b").Claim(ctx, "k")
	must(t, err)
	_, err = w.tab("a").Fence(ctx, "k")
	mustIs(t, err, owner.ErrSuperseded)
	if got := w.tab("a").State("k"); got.State != owner.Fenced || !got.Superseded {
		t.Fatalf("State = %s", got)
	}
	_, err = w.tab("a").Fence(ctx, "k")
	mustIs(t, err, owner.ErrSuperseded) // stays without an ack
}

func TestPublish(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	pub := func(c context.Context) (owner.Generation, error) { return w.procs["a"].wb.Publish(c, "k", 1) }
	g, err := a.Publish(ctx, "k", 1, pub)
	must(t, err)
	if a.State("k").Gen != g {
		t.Fatalf("local gen %d, published %d", a.State("k").Gen, g)
	}
	_, err = a.Publish(ctx, "k", 2, pub)
	mustIs(t, err, owner.ErrWrongOwner)
	_, err = a.Publish(ctx, "nope", 1, pub)
	mustIs(t, err, owner.ErrWrongOwner)
	_, err = a.Publish(ctx, "k", 1, func(context.Context) (owner.Generation, error) { return 0, ownertest.ErrDown })
	mustIs(t, err, ownertest.ErrDown)
	if a.State("k").State != owner.Active {
		t.Fatal("a failed (non-stale) publish must not fence")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = a.Publish(cctx, "k", 1, pub)
	if err != nil {
		mustIs(t, err, context.Canceled) // the lock was free, so it may also succeed; both are fine
	}
}

func TestCheckActivation(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	must(t, a.CheckActivation(ctx, "k", 1, owner.Manifest{Gen: 0, Epoch: 1}))
	mustIs(t, a.CheckActivation(ctx, "k", 2, owner.Manifest{Epoch: 1}), owner.ErrWrongOwner)
	mustIs(t, a.CheckActivation(ctx, "x", 1, owner.Manifest{Epoch: 1}), owner.ErrWrongOwner)
	mustIs(t, a.CheckActivation(ctx, "k", 1, owner.Manifest{Epoch: 0}), owner.ErrUnavailable)
	_, err := a.Publish(ctx, "k", 1, func(c context.Context) (owner.Generation, error) { return w.procs["a"].wb.Publish(c, "k", 1) })
	must(t, err)
	mustIs(t, a.CheckActivation(ctx, "k", 1, owner.Manifest{Gen: 0, Epoch: 1}), owner.ErrUnavailable) // gen behind
	err = a.CheckActivation(ctx, "k", 1, owner.Manifest{Gen: 1, Epoch: 2})
	mustIs(t, err, owner.ErrSuperseded)
	if r, _ := w.procs["a"].store.Get("k"); !r.Superseded {
		t.Fatalf("record = %+v", r)
	}
}

func TestForget(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	mustIs(t, a.Forget(ctx, "k"), owner.ErrIllegal)
	_, err := a.Fence(ctx, "k")
	must(t, err)
	w.procs["a"].store.Set(func(m *ownertest.MemStore) { m.FailDelete = true })
	mustIs(t, a.Forget(ctx, "k"), ownertest.ErrDown)
	w.procs["a"].store.Set(func(m *ownertest.MemStore) { m.FailDelete = false })
	must(t, a.Forget(ctx, "k"))
	must(t, a.Forget(ctx, "never"))
	if _, ok := w.procs["a"].store.Get("k"); ok || a.State("k").State != owner.Absent {
		t.Fatal("forget left state behind")
	}
}

type badStore struct {
	ownertest.MemStore
	recs []owner.Record
	err  error
}

func (b *badStore) Load(context.Context) ([]owner.Record, error) { return b.recs, b.err }

func TestOpenRejects(t *testing.T) {
	pl, wb := ownertest.NewPlacement(), &ownertest.WriteBack{Objects: ownertest.NewObjects(), Disk: ownertest.NewDisk()}
	good := owner.Record{Key: "k", State: owner.Active, Epoch: 1}
	for _, tc := range []struct {
		name string
		cfg  owner.Config
	}{
		{"no self", owner.Config{Store: ownertest.NewMemStore(), Placement: pl, WriteBack: wb}},
		{"no ports", owner.Config{Self: "a"}},
		{"load fails", owner.Config{Self: "a", Store: &badStore{err: ownertest.ErrDown}, Placement: pl, WriteBack: wb}},
		{"bad record", owner.Config{Self: "a", Store: &badStore{recs: []owner.Record{{Key: "k", State: owner.Fencing, Epoch: 1}}}, Placement: pl, WriteBack: wb}},
		{"duplicate", owner.Config{Self: "a", Store: &badStore{recs: []owner.Record{good, good}}, Placement: pl, WriteBack: wb}},
	} {
		if _, err := owner.Open(ctx, tc.cfg); err == nil {
			t.Errorf("%s: Open succeeded", tc.name)
		} else if _, ok := errs.Outermost(err); !ok {
			t.Errorf("%s: not a well-formed error: %v", tc.name, err)
		}
	}
}

func TestDoReturnsFnError(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	boom := errors.New("boom")
	mustIs(t, w.tab("a").Do("k", 1, func() error { return boom }), boom)
	if len(w.tab("a").Keys()) != 1 {
		t.Fatal("Keys")
	}
	owner.Permit{}.Done() // zero permit: no-op
}

// A slow authority: Claim gives up with the caller's deadline, reports "unavailable", and changes nothing.
func TestClaimWithSlowAuthorityHonoursDeadline(t *testing.T) {
	w := newWorld(t, "a")
	w.pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
	w.pl.SetDelay(time.Second)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := w.tab("a").Claim(cctx, "k")
	mustIs(t, err, owner.ErrUnavailable)
	mustIs(t, err, context.DeadlineExceeded)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("Claim waited for the slow authority past its deadline")
	}
	if w.tab("a").State("k").State != owner.Absent {
		t.Fatal("state changed")
	}
}

// An import (dbtool import without -placement-dir, IR-4b) leaves generation 1 at epoch 1 and no row; the
// control plane's create then writes ACTIVE(vm, 1, gen 0). The claim adopts the imported generation: the
// manifest is not behind the row, and the stamp keeps generation 1.
func TestClaimAdoptsAnImportedGeneration(t *testing.T) {
	w := newWorld(t, "a")
	g, err := w.objs.Upload("k", []string{"imported"})
	must(t, err)
	must(t, w.objs.Publish("k", 0, g, 1))
	w.pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
	l, err := w.tab("a").Claim(ctx, "k")
	if err != nil || l != (owner.Local{State: owner.Active, Epoch: 1, Gen: 1}) {
		t.Fatalf("Claim = %s, %v; want active(e1,g1)", l, err)
	}
	if gen, e, _, contents, _ := w.objs.Read("k"); gen != 1 || e != 1 || !slices.Equal(contents, []string{"imported"}) {
		t.Fatalf("manifest after the claim: gen %d epoch %d %v", gen, e, contents)
	}
}

// A fence whose final write-back failed leaves the key Fencing until the control plane retries it. Periodic
// write-backs keep publishing it meanwhile (admission has stopped, so its writes are final): a crash before
// the retry loses nothing beyond the write-back window. Found by the DST-2 cluster sim (seed 1963 in swarm
// mode: a move's fence failed in an S3 storm; ticks refused the Fencing key for the rest of the run).
func TestPublishWhileAFenceIsPending(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	must(t, w.write("a", "k", 1, "w1"))
	w.objs.SetDown(true)
	if _, err := a.Fence(ctx, "k"); err == nil {
		t.Fatal("Fence with storage down succeeded")
	}
	if a.State("k").State != owner.Fencing {
		t.Fatalf("State = %s, want fencing", a.State("k"))
	}
	w.objs.SetDown(false)
	if _, err := a.Admit("k", 1); err != owner.ErrMoving {
		t.Fatalf("Admit while fencing = %v, want ErrMoving", err)
	}
	g, err := a.Publish(ctx, "k", 1, func(c context.Context) (owner.Generation, error) { return w.procs["a"].wb.Publish(c, "k", 1) })
	must(t, err)
	if _, _, _, c, _ := w.objs.Read("k"); !slices.Equal(c, []string{"w1"}) || a.State("k").Gen != g {
		t.Fatalf("published %v (gen %d, local %s), want [w1]", c, g, a.State("k"))
	}
	_, err = a.Publish(ctx, "k", 2, func(context.Context) (owner.Generation, error) { return 0, nil })
	mustIs(t, err, owner.ErrMoving) // only at the fencing epoch
	ack, err := a.Fence(ctx, "k")
	must(t, err)
	if ack.Gen < g || a.State("k").State != owner.Fenced { // the fence's own write-back may publish again
		t.Fatalf("ack %+v after a pending-fence publish of gen %d; state %s", ack, g, a.State("k"))
	}
	_, err = a.Publish(ctx, "k", 1, func(context.Context) (owner.Generation, error) { return 0, nil })
	mustIs(t, err, owner.ErrFenced)
}

// A periodic publish that gets the publish lock between a retried Fence's drain and its write-back raises
// Gen; the fence must judge its write-back against that Gen (re-read under the lock), not the one it read
// before: a lower generation is refused (ErrInvalid, still Fencing), never installed as FENCED.
func TestFenceRereadsGenUnderThePublishLock(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	must(t, w.write("a", "k", 1, "w1"))
	w.objs.SetDown(true)
	if _, err := a.Fence(ctx, "k"); err == nil {
		t.Fatal("Fence with storage down succeeded")
	}
	w.objs.SetDown(false)
	// P1 holds the publish lock and a permit, so the retried Fence drains on it; P2 queues on the lock before
	// the Fence reaches it, and publishes a generation far above anything the fence's write-back returns.
	inP1, releaseP1 := make(chan struct{}), make(chan struct{})
	p1 := make(chan error, 1)
	go func() {
		_, err := a.Publish(ctx, "k", 1, func(context.Context) (owner.Generation, error) {
			close(inP1)
			<-releaseP1
			return a.State("k").Gen, nil
		})
		p1 <- err
	}()
	<-inP1
	p2 := make(chan error, 1)
	go func() {
		_, err := a.Publish(ctx, "k", 1, func(context.Context) (owner.Generation, error) { return 1000, nil })
		p2 <- err
	}()
	time.Sleep(20 * time.Millisecond) // P2 waits on the lock
	fence := make(chan error, 1)
	go func() { _, err := a.Fence(ctx, "k"); fence <- err }()
	time.Sleep(20 * time.Millisecond) // the Fence waits on P1's permit
	close(releaseP1)
	must(t, <-p1)
	must(t, <-p2)
	err := <-fence
	l := a.State("k")
	switch {
	case err == nil && l.State == owner.Fenced && l.Gen >= 1000:
	case errors.Is(err, owner.ErrInvalid) && l.State == owner.Fencing && l.Gen == 1000:
	default:
		t.Fatalf("Fence = %v, state %s; want an ack at gen ≥ 1000 or ErrInvalid while fencing", err, l)
	}
}

// A fence whose write-back published but whose FENCED record failed stays Fencing; from then on no periodic
// publish of that epoch runs (the fence's generation is the last), and the retried Fence acks it.
func TestNoPublishAfterAFenceWriteBack(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	must(t, w.write("a", "k", 1, "w1"))
	w.procs["a"].store.Set(func(m *ownertest.MemStore) { m.FailPut = true })
	if _, err := a.Fence(ctx, "k"); err == nil {
		t.Fatal("Fence with the record store down succeeded")
	}
	if l := a.State("k"); l.State != owner.Fencing {
		t.Fatalf("State = %s, want fencing", l)
	}
	calls := w.procs["a"].wb.Calls
	_, err := a.Publish(ctx, "k", 1, func(c context.Context) (owner.Generation, error) { return w.procs["a"].wb.Publish(c, "k", 1) })
	mustIs(t, err, owner.ErrMoving)
	w.procs["a"].store.Set(func(m *ownertest.MemStore) { m.FailPut = false })
	ack, err := a.Fence(ctx, "k")
	must(t, err)
	if w.procs["a"].wb.Calls != calls || a.State("k").State != owner.Fenced || ack.Gen != a.State("k").Gen {
		t.Fatalf("retried Fence: ack %+v, state %s, write-backs %d → %d (want none)", ack, a.State("k"), calls, w.procs["a"].wb.Calls)
	}
}
