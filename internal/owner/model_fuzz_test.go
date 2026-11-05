package owner_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/owner/ownertest"
)

var (
	fuzzProcs = []string{"p0", "p1", "p2"}
	fuzzKeys  = []string{"k0", "k1"}
)

type ackedWrite struct {
	id, proc, key string
	epoch         owner.Epoch
}

// model is what the system must preserve, checked after every op.
type model struct {
	w        *world
	acked    []ackedWrite
	lost     map[string]bool // writes lost to an operator-attested VM-loss restore (allowed by design)
	nextID   int
	maxEpoch map[string]owner.Epoch // highest epoch seen in storage per key (must never go down)
	// rogue[k] is the epoch of the last restore done while the old owner was still alive (an operator
	// mistake). Below it, zombies may admit and lose unpublished writes; storage must still stay linear.
	rogue map[string]owner.Epoch
	log   []string
}

const (
	opWrite = iota
	opStaleWrite
	opFlush
	opMove
	opBegin
	opFence
	opCrash
	opRestart
	opPartition
	opReconcile
	opRestore
	opClaim
	opFailWriteBack
	opRogueRestore
	opLostReply
	numOps
)

var opNames = [...]string{"write", "stale-write", "flush", "move", "begin", "fence", "crash", "restart",
	"partition", "reconcile", "restore", "claim", "fail-write-back", "rogue-restore", "lost-reply"}

// FuzzOwnershipModel runs random scripts of activate/write/fence/move/crash/restart/partition/stale-route ops
// over 3 simulated processes sharing one authority and one contract-1 store. After every op: at most one
// process admits each key, and it is the authority's owner at the authority's epoch; the published history
// is linear with no duplicates; and no acknowledged write is lost (unless an operator-attested VM-loss restore
// gave up the dead owner's unpublished writes, which the model records).
func FuzzOwnershipModel(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{opWrite, 0, opMove, 1 + 6, opWrite, 0, opWrite, 3})
	f.Add([]byte{opWrite, 0, opBegin, 0, opWrite, 0, opCrash, 0, opMove, 6, opRestart, 0, opMove, 6, opStaleWrite, 0})
	f.Add([]byte{opWrite, 0, opCrash, 0, opRestore, 6, opClaim, 1, opWrite, 1, opRestart, 0, opWrite, 0, opFlush, 0})
	f.Add([]byte{opPartition, 0, opWrite, 0, opMove, 6, opPartition, 0, opMove, 6, opWrite, 1})
	f.Add([]byte{opFailWriteBack, 0, opWrite, 0, opMove, 12, opFailWriteBack, 0, opMove, 12, opFence, 2})
	f.Add([]byte{opWrite, 0, opFlush, 0, opCrash, 0, opRestore, 12, opRestart, 0, opReconcile, 0, opMove, 6, opWrite, 2})
	f.Add([]byte{opWrite, 0, opRogueRestore, 6, opWrite, 0, opFlush, 0, opClaim, 1, opWrite, 0, opFlush, 0, opWrite, 1, opFlush, 1})
	f.Add([]byte{opWrite, 0, opLostReply, 0, opMove, 6, opWrite, 1, opLostReply, 0, opFlush, 1, opWrite, 1})
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 400 {
			script = script[:400]
		}
		w := newWorld(t, fuzzProcs...)
		m := &model{w: w, lost: map[string]bool{}, maxEpoch: map[string]owner.Epoch{}, rogue: map[string]owner.Epoch{}}
		for i, k := range fuzzKeys {
			w.place(k, fuzzProcs[i])
		}
		for i := 0; i+1 < len(script); i += 2 {
			m.step(t, script[i], script[i+1])
			m.check(t)
		}
	})
}

func (m *model) step(t *testing.T, opb, arg byte) {
	w := m.w
	op := int(opb) % numOps
	p := fuzzProcs[int(arg)%3]
	k := fuzzKeys[int(arg/3)%2]
	q := fuzzProcs[int(arg/6)%3]
	pr := w.procs[p]
	row, _ := w.pl.Row(k)
	m.log = append(m.log, fmt.Sprintf("%s(%s,%s,%s) row=%+v", opNames[op], p, k, q, row))
	switch op {
	case opWrite, opStaleWrite:
		e := row.Epoch // a client with a fresh route
		if op == opStaleWrite {
			e = owner.Epoch(int(arg/18) % int(row.Epoch+2)) // any epoch 0..current+1: stale or too new
		}
		m.nextID++
		id := fmt.Sprintf("w%d", m.nextID)
		if err := w.write(p, k, e, id); err == nil {
			l := pr.tab.State(k)
			if l.State != owner.Active || l.Epoch != e {
				t.Fatalf("%s admitted %s at epoch %d in state %s\n%s", p, k, e, l, m.trace())
			}
			m.acked = append(m.acked, ackedWrite{id, p, k, e})
		}
	case opFlush:
		if pr.tab != nil {
			l := pr.tab.State(k)
			_, _ = pr.tab.Publish(ctx, k, l.Epoch, func(c context.Context) (owner.Generation, error) { return pr.wb.Publish(c, k, l.Epoch) })
		}
	case opMove:
		_, _ = w.mover().Move(ctx, k, q)
	case opBegin:
		if row.State == owner.PlaceActive {
			next := row
			next.State = owner.PlaceMoving
			_ = w.pl.CAS(ctx, k, row, next)
		}
	case opFence:
		_, _ = agentFor{w, pr}.Fence(ctx, k)
	case opCrash:
		pr.tab = nil
	case opRestart:
		if pr.tab == nil {
			_ = w.start(p).ReconcileAll(ctx)
		}
	case opPartition:
		pr.pl.cut = !pr.pl.cut
	case opReconcile:
		if pr.tab != nil {
			_ = pr.tab.ReconcileAll(ctx)
		}
	case opRestore, opRogueRestore:
		// Operator-attested VM loss: allowed only when the owner is really down. The rogue variant breaks
		// that assumption on purpose, to exercise the storage backstop.
		if (op == opRestore && w.procs[row.Owner].tab != nil) || q == row.Owner {
			return
		}
		g, _, _, contents, _ := w.objs.Read(k)
		next := owner.Place{Owner: q, Epoch: row.Epoch + 1, Gen: g, State: owner.PlaceActive}
		if w.pl.CAS(ctx, k, row, next) != nil {
			return
		}
		for _, a := range m.acked {
			if a.key == k && a.proc == row.Owner && !slices.Contains(contents, a.id) {
				m.lost[a.id] = true
			}
		}
		if op == opRogueRestore {
			m.rogue[k] = next.Epoch
		}
		m.log = append(m.log, fmt.Sprintf("  restore gave up %d unpublished writes", len(m.lost)))
	case opClaim:
		_, _ = agentFor{w, pr}.Claim(ctx, k) // a retried or wrong placement: must fail unless p is the owner
	case opFailWriteBack:
		pr.wb.Set(func(wb *ownertest.WriteBack) { wb.FailPublish = !wb.FailPublish })
	case opLostReply:
		w.objs.LoseNextReply() // the next publish or stamp applies, then reports ErrStale
	}
}

func (m *model) check(t *testing.T) {
	w := m.w
	for _, k := range fuzzKeys {
		row, _ := w.pl.Row(k)
		// I1: at most one process admits k, and it is the authority's owner at the authority's epoch.
		var admitters []string
		for _, p := range fuzzProcs {
			tab := w.procs[p].tab
			if tab == nil {
				continue
			}
			if l := tab.State(k); l.State == owner.Active {
				if l.Epoch < m.rogue[k] {
					continue // a zombie left by a rogue restore; checked by I2/I3 below
				}
				admitters = append(admitters, p)
				if row.Owner != p || row.Epoch != l.Epoch {
					t.Fatalf("%s admits %s at %s but the authority says %+v\n%s", p, k, l, row, m.trace())
				}
			}
		}
		if len(admitters) > 1 {
			t.Fatalf("split brain on %s: %v\n%s", k, admitters, m.trace())
		}
		// I2: published history linear, no duplicates, storage epoch never decreases.
		hist := w.objs.History(k)
		for i := 1; i < len(hist); i++ {
			if len(hist[i]) < len(hist[i-1]) || !slices.Equal(hist[i][:len(hist[i-1])], hist[i-1]) {
				t.Fatalf("published history of %s forked: %v → %v\n%s", k, hist[i-1], hist[i], m.trace())
			}
		}
		_, e, _, contents, _ := w.objs.Read(k)
		if e < m.maxEpoch[k] {
			t.Fatalf("storage epoch of %s went back %d → %d\n%s", k, m.maxEpoch[k], e, m.trace())
		}
		m.maxEpoch[k] = e
		seen := map[string]bool{}
		for _, id := range contents {
			if seen[id] {
				t.Fatalf("write %s published twice in %s: %v\n%s", id, k, contents, m.trace())
			}
			seen[id] = true
		}
		// I3: every acknowledged write is published, or still held by its owner at its epoch, or was given
		// up by a VM-loss restore.
		for _, a := range m.acked {
			if a.key != k || seen[a.id] || m.lost[a.id] || a.epoch < m.rogue[k] {
				continue
			}
			pr := w.procs[a.proc]
			r, ok := pr.store.Get(k)
			if !ok || r.State != owner.Active || r.Epoch != a.epoch || !slices.Contains(pr.disk.Contents(k), a.id) {
				t.Fatalf("acknowledged write %s (%s at e%d) lost: not published, and %s holds record %+v ok=%v\n%s",
					a.id, a.proc, a.epoch, a.proc, r, ok, m.trace())
			}
		}
	}
}

func (m *model) trace() string {
	s := "ops:\n"
	for _, l := range m.log {
		s += "  " + l + "\n"
	}
	return s
}
