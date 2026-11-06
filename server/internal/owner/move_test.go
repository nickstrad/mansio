package owner_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
)

// agent adapts processes for the Mover: a crashed or partitioned process is unreachable, and a successful
// Claim also fetches the database (what the host's activation does).
type agentFor struct {
	w *world
	p *proc
}

func (a agentFor) Fence(ctx context.Context, key string) (owner.Ack, error) {
	if a.p.tab == nil || a.p.pl.cut {
		return owner.Ack{}, ownertest.ErrDown
	}
	return a.p.tab.Fence(ctx, key)
}

func (a agentFor) Claim(ctx context.Context, key string) (owner.Local, error) {
	if a.p.tab == nil || a.p.pl.cut {
		return owner.Local{}, ownertest.ErrDown
	}
	before := a.p.tab.State(key)
	l, err := a.p.tab.Claim(ctx, key)
	// Fetch only when the claim activated the key here. Re-fetching on an idempotent claim would replace the
	// warm local copy, and with it acknowledged writes not yet published (FuzzOwnershipModel found this).
	if err == nil && (before.State != owner.Active || before.Epoch != l.Epoch) {
		m := a.p.wb.Fetch(key)
		if err := a.p.tab.CheckActivation(ctx, key, l.Epoch, m); err != nil {
			return owner.Local{}, err
		}
	}
	return l, err
}

func (w *world) mover() *owner.Mover {
	return &owner.Mover{Placement: w.pl, Agent: func(n string) owner.Agent { return agentFor{w, w.procs[n]} }}
}

// hookAgent lets a test act right after a call returned on the agent (crash the process, lose the reply).
type hookAgent struct {
	agentFor
	after func(op string, err error) error
}

func (h hookAgent) Fence(ctx context.Context, key string) (owner.Ack, error) {
	ack, err := h.agentFor.Fence(ctx, key)
	return ack, h.after("fence", err)
}

func (h hookAgent) Claim(ctx context.Context, key string) (owner.Local, error) {
	l, err := h.agentFor.Claim(ctx, key)
	return l, h.after("claim", err)
}

func (w *world) moverWithHook(after func(proc, op string, err error) error) *owner.Mover {
	return &owner.Mover{Placement: w.pl, Agent: func(n string) owner.Agent {
		return hookAgent{agentFor{w, w.procs[n]}, func(op string, err error) error { return after(n, op, err) }}
	}}
}

func (w *world) crash(name string) { w.procs[name].tab = nil }

func (w *world) restart(name string) {
	w.t.Helper()
	if _, err := w.start(name).ReconcileAll(context.Background(), owner.ReconcileOptions{}); err != nil {
		w.t.Fatalf("reconcile %s: %v", name, err)
	}
}

// checkMoved asserts the end state of a move of k from a to b, given the writes a acknowledged.
func checkMoved(t *testing.T, w *world, acked []string, wantEpoch owner.Epoch) {
	t.Helper()
	row, _ := w.pl.Row("k")
	if row != (owner.Place{Owner: "b", Epoch: wantEpoch, Gen: row.Gen, State: owner.PlaceActive}) {
		t.Fatalf("authority row = %+v", row)
	}
	if got := w.tab("b").State("k"); got.State != owner.Active || got.Epoch != wantEpoch {
		t.Fatalf("target state = %s", got)
	}
	if _, e, _, _, _ := w.objs.Read("k"); e != wantEpoch {
		t.Fatalf("storage epoch = %d, want the target's stamp %d", e, wantEpoch)
	}
	// No acknowledged write lost or duplicated: the target starts from exactly what the source acknowledged.
	if got := w.procs["b"].disk.Contents("k"); !slices.Equal(got, acked) {
		t.Fatalf("target starts from %v, want exactly the acknowledged %v", got, acked)
	}
	if w.tab("a") != nil {
		if _, err := w.tab("a").Admit("k", wantEpoch-1); err != owner.ErrFenced {
			t.Fatalf("source Admit at the old epoch = %v, want ErrFenced", err)
		}
	}
	if _, err := w.tab("b").Admit("k", wantEpoch-1); err != owner.ErrWrongOwner {
		t.Fatalf("target Admit at the old epoch = %v, want ErrWrongOwner", err)
	}
	assertLinear(t, w.objs.History("k"))
}

func setupMove(t *testing.T) (*world, []string) {
	w := newWorld(t, "a", "b")
	w.place("k", "a")
	var acked []string
	for i := range 3 {
		id := fmt.Sprintf("w%d", i)
		must(t, w.write("a", "k", 1, id))
		acked = append(acked, id)
	}
	return w, acked
}

func TestMoveHappyPath(t *testing.T) {
	w, acked := setupMove(t)
	l, err := w.mover().Move(ctx, "k", "b")
	must(t, err)
	if l.Epoch != 2 {
		t.Fatalf("Move = %s", l)
	}
	checkMoved(t, w, acked, 2)
	// The target serves at the new epoch and its writes extend the history.
	must(t, w.write("b", "k", 2, "b1"))
	_, err = w.tab("b").Fence(ctx, "k")
	must(t, err)
	if _, _, _, c, _ := w.objs.Read("k"); !slices.Equal(c, append(acked, "b1")) {
		t.Fatalf("final contents %v", c)
	}
	// Moving to the current owner is a no-op claim.
	w2, _ := setupMove(t)
	l, err = w2.mover().Move(ctx, "k", "a")
	if err != nil || l.Epoch != 1 {
		t.Fatalf("Move to the owner = %s %v", l, err)
	}
}

// Every crash point of option 4 §3.4. After each, the caller calls Move again (resume) and the end state must
// be the same as the happy path: nothing acknowledged lost, nothing duplicated, one admitter.
func TestMoveCrashPoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		// inject runs after an agent call; return a non-nil error to make Move stop there.
		inject func(w *world, proc, op string, err error) error
		// before Move is resumed
		recover func(w *world)
		setup   func(w *world)
	}{
		{
			name: "source dies before the fence ack",
			setup: func(w *world) {
				// Begin went through, then the source crashed before it was asked to fence.
				row, _ := w.pl.Row("k")
				next := row
				next.State = owner.PlaceMoving
				must(w.t.(*testing.T), w.pl.CAS(ctx, "k", row, next))
				w.crash("a")
			},
			recover: func(w *world) { w.restart("a") },
		},
		{
			name: "source dies after the durable fence ack, reply lost",
			inject: func(w *world, proc, op string, err error) error {
				if proc == "a" && op == "fence" && err == nil {
					w.crash("a")
					return ownertest.ErrDown
				}
				return err
			},
			recover: func(w *world) { w.restart("a") },
		},
		{
			name: "source write-back fails during the fence",
			setup: func(w *world) {
				w.procs["a"].wb.Set(func(wb *ownertest.WriteBack) { wb.FailPublish = true })
			},
			recover: func(w *world) {
				w.procs["a"].wb.Set(func(wb *ownertest.WriteBack) { wb.FailPublish = false })
			},
		},
		{
			name:    "flip applied but its reply was lost",
			setup:   func(w *world) { armLoseReplyOnFlip(w) },
			recover: func(w *world) {},
		},
		{
			name: "authority down at the flip",
			inject: func(w *world, proc, op string, err error) error {
				if op == "fence" {
					w.pl.SetDown(true)
				}
				return err
			},
			recover: func(w *world) { w.pl.SetDown(false) },
		},
		{
			name:    "target dies before it claims",
			setup:   func(w *world) { w.crash("b") },
			recover: func(w *world) { w.restart("b") },
		},
		{
			name: "target dies after its durable claim",
			inject: func(w *world, proc, op string, err error) error {
				if proc == "b" && op == "claim" && err == nil {
					w.crash("b")
					return ownertest.ErrDown
				}
				return err
			},
			recover: func(w *world) { w.restart("b") },
		},
		{
			name: "source partitioned from the control plane",
			setup: func(w *world) {
				w.procs["a"].pl.cut = true
			},
			recover: func(w *world) { w.procs["a"].pl.cut = false },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, acked := setupMove(t)
			if tc.setup != nil {
				tc.setup(w)
			}
			inject := tc.inject
			if inject == nil {
				inject = func(_ *world, _, _ string, err error) error { return err }
			}
			m := w.moverWithHook(func(p, op string, err error) error { return inject(w, p, op, err) })
			if _, err := m.Move(ctx, "k", "b"); err == nil {
				t.Fatal("the injected fault did not stop the move")
			}
			// While the move is stuck, at most one process admits, and it is the source at the old epoch.
			if w.tab("b") != nil {
				if _, err := w.tab("b").Admit("k", 2); err == nil {
					row, _ := w.pl.Row("k")
					if row.Owner != "b" {
						t.Fatal("target admits before the flip")
					}
				}
			}
			tc.recover(w)
			_, err := w.moverWithHook(func(_, _ string, err error) error { return err }).Move(ctx, "k", "b")
			must(t, err)
			checkMoved(t, w, acked, 2)
		})
	}
}

// A write the source admits after the authority went MOVING but before the source was fenced is still
// acknowledged, so it must reach the target.
func TestMoveKeepsWritesAdmittedWhileMoving(t *testing.T) {
	w, acked := setupMove(t)
	row, _ := w.pl.Row("k")
	next := row
	next.State = owner.PlaceMoving
	must(t, w.pl.CAS(ctx, "k", row, next))
	must(t, w.write("a", "k", 1, "late"))
	_, err := w.mover().Move(ctx, "k", "b")
	must(t, err)
	checkMoved(t, w, append(acked, "late"), 2)
}

func TestMoveRejectsAMismatchedAck(t *testing.T) {
	w, _ := setupMove(t)
	m := &owner.Mover{Placement: w.pl, Agent: func(n string) owner.Agent { return liar{} }}
	_, err := m.Move(ctx, "k", "b")
	mustIs(t, err, owner.ErrInvalid)
	if row, _ := w.pl.Row("k"); row.Owner != "a" || row.State != owner.PlaceMoving {
		t.Fatalf("flipped on a bad ack: %+v", row)
	}
	w.pl.SetDown(true)
	_, err = w.mover().Move(ctx, "k", "b")
	mustIs(t, err, ownertest.ErrDown)
}

type liar struct{}

func (liar) Fence(context.Context, string) (owner.Ack, error)   { return owner.Ack{Epoch: 7}, nil }
func (liar) Claim(context.Context, string) (owner.Local, error) { return owner.Local{}, nil }

// armLoseReplyOnFlip makes the authority lose the reply of the next CAS after the begin step.
func armLoseReplyOnFlip(w *world) {
	row, _ := w.pl.Row("k")
	next := row
	next.State = owner.PlaceMoving
	must(w.t.(*testing.T), w.pl.CAS(ctx, "k", row, next)) // do the begin step here
	w.pl.SetLoseReply(true)                               // the flip is the next CAS
}

func TestMoveBackToAFencedProcess(t *testing.T) {
	w := newWorld(t, "a", "b")
	w.place("k", "a")
	m := w.mover()
	_, err := m.Move(ctx, "k", "b")
	must(t, err)
	l, err := m.Move(ctx, "k", "a")
	must(t, err)
	if l.Epoch != 3 || w.tab("a").State("k").State != owner.Active {
		t.Fatalf("a = %s", w.tab("a").State("k"))
	}
}
