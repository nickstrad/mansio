package sim

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/linearize"
	"github.com/nickstrad/mansio/server/internal/owner"
)

// The cluster checker. Per database it keeps:
//
//   - the marks (one per mutation's request id: pending, none, maybe, acked, lost, as DST-1's model) and checks
//     every published generation (the log table): no mark twice (nothing applied twice), none unknown, none
//     of a write every answer said did nothing, none of a write whose final answer was a definite failure;
//     each generation extends the one before; generations and epochs only rise;
//   - restores: whenever an owner starts serving the database (a claim after a move or restore, a reopen
//     after a restart: the owner table's transition to Active), the generation it starts from must hold
//     every acknowledged write, except, after a kill -9 of the owner, writes inside the RPO (DST-1's rule:
//     a loss is a violation iff a fault-free stretch at least RPO long fits between its acknowledgement and the
//     crash, or it was acknowledged before a write-back or fence that published). A graceful move never loses
//     a write;
//   - commits: every write that commits records its epoch and VM: one VM per epoch, epochs never going down
//     in commit order (at most one owner serves writes per database per epoch); every owner table's epoch for
//     a key only rises;
//   - the client-side history of registers and the bank, checked at the end with internal/linearize, split at
//     the crashes of the database's owner (the IR-5a Jepsen rules: per segment linearizable; a later segment
//     starts from the setup state or a value written before the crash), and bank conservation on every read.

type cmark struct {
	st      status
	ackedAt time.Time // when the agent answered ok: its commit (or the client's receipt, if no commit is known)
	// commitAt: the last time a live agent committed it (zero: never). lostAt: a restore after a crash at
	// lostAt.tc started from a generation without that commit, before the client heard its ok.
	commitAt time.Time
	lostAt   *loss
	final    string // a definite failure the client ended with ("" otherwise)
	vm       string // the VM that committed it (its links and breaker decide the RPO excuse)
	// dup: the network delivered one of its requests twice; everMaybe: some attempt may have applied (no
	// answer, or an ambiguous one), and may still land later. Either way no answer proves it did not run.
	dup, everMaybe bool
}

// unsure: no answer to the mark's request proves it did not run.
func (e *cmark) unsure() bool { return e.dup || e.everMaybe }

// loss is a crash that lost a committed write (committed at commitAt), with what was durable then.
type loss struct{ commitAt, tc, durableBefore time.Time }

type cdbModel struct {
	key           string
	marks         map[string]*cmark
	regOps        []linearize.Op[regIn, regOut]
	bankOps       []linearize.Op[bankIn, bankOut]
	crashes       []int64    // the owner's kills (ns since the start): segment boundaries
	crashStarts   [][]string // per crash: the marks of the generation the next owner started from
	unjudged      time.Time  // the earliest owner kill since the last restore
	hasUnjudged   bool
	durableBefore time.Time
	gen, epoch    uint64
	genMarks      []string
	epochOwner    map[uint64]string
	lastEpoch     uint64
	seenVersion   map[[2]int64]int64 // (account, _version) → the balance a read saw
	seenSeq       map[[2]int64]int64 // (account, commit sequence) → the _version a read saw
}

type cmodel struct {
	w       *cworld
	mu      sync.Mutex
	dbs     map[string]*cdbModel
	found   []Violation
	lostOK  int
	excused int // of lostOK: older than the RPO at the crash, excused by faults on the owner's links or the key
	checked int

	objMu    sync.Mutex          // guards objMarks; not held with mu
	objMarks map[string][]string // object name → its marks (read)
}

func newCModel(w *cworld) *cmodel {
	return &cmodel{w: w, dbs: map[string]*cdbModel{}, objMarks: map[string][]string{}}
}

func (m *cmodel) addDB(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dbs[key] == nil {
		m.dbs[key] = &cdbModel{key: key, marks: map[string]*cmark{}, durableBefore: m.w.t0, epochOwner: map[uint64]string{}}
	}
}

func (m *cmodel) db(key string) *cdbModel { return m.dbs[key] } // pre: m.mu held

// keys lists the databases, sorted.
func (m *cmodel) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.dbs))
}

// violate records a violation. pre: m.mu held
func (m *cmodel) violate(key, format string, args ...any) {
	v := Violation{At: m.w.clk.Now().Sub(m.w.t0), Key: key, What: fmt.Sprintf(format, args...)}
	m.found = append(m.found, v)
	m.w.tr.add("check", "VIOLATION %s: %s", key, v.What)
}

func (m *cmodel) violateNow(key, format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.violate(key, format, args...)
}

func (m *cmodel) failed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.found) > 0
}

func (m *cmodel) violations() []Violation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.found)
}

func (m *cmodel) mark(key, rid string) *cmark { // pre: m.mu held
	d := m.db(key)
	e := d.marks[rid]
	if e == nil {
		e = &cmark{}
		d.marks[rid] = e
	}
	return e
}

// attempt: a client sends an attempt of mutation rid; it may apply until its answer says otherwise.
func (m *cmodel) attempt(key, rid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.mark(key, rid); e.st == none || e.st == lost {
		e.st = pending
	}
}

func (m *cmodel) duplicated(key, rid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dbs[key] != nil {
		m.mark(key, rid).dup = true
	}
}

// uncertain: no answer to rid proves it did not run (the network duplicated one of its requests, or an
// attempt went unanswered and may still land).
func (m *cmodel) uncertain(key, rid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mark(key, rid).unsure()
}

// retryable tags: the attempt did nothing, the client tries again.
var retryable = map[string]bool{"shed": true, "unavailable": true, "aborted": true, "wrong_owner": true, "fenced": true,
	"ticket_invalid": true, "expired": true}

// maybeTags: the attempt may have applied.
var maybeTags = map[string]bool{"no-answer": true, "unreadable": true, "expired-ambiguous": true, "Defect": true, "StorageError": true}

// answer judges one answer to mutation rid as the client saw it (tag: ok, ok-dup, a wire tag, or one of
// maybeTags).
func (m *cmodel) answer(key, rid, tag string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.mark(key, rid)
	now := m.w.clk.Now()
	raise := func(to status) {
		if e.unsure() && to == none {
			to = maybe
		}
		e.everMaybe = e.everMaybe || to == maybe
		if e.st == pending || e.st == none && to == maybe {
			e.st = to
		}
	}
	ack := func() {
		if e.st == acked {
			return
		}
		e.st, e.ackedAt = acked, now
		if !e.commitAt.IsZero() && !e.commitAt.After(now) {
			e.ackedAt = e.commitAt // the agent answered when it committed; the answer may have taken a while
		}
		if l := e.lostAt; l != nil { // its commit was lost at a crash before this ok arrived
			e.lostAt, e.ackedAt = nil, l.commitAt
			m.judgeLoss(m.db(key), rid, e, l.tc, l.durableBefore, "an ok that arrived after its commit was lost")
		}
	}
	switch {
	case tag == "ok":
		ack()
	case tag == "ok-dup":
		if e.st != acked && !e.unsure() {
			m.violate(key, "mutation %s answered duplicate, but no earlier attempt can have applied (status %s)", rid, e.st)
		}
		ack()
	case maybeTags[tag]:
		raise(maybe)
	case retryable[tag]:
		raise(none)
	default: // a definite failure: the request ran its checks and changed nothing
		raise(none)
		if !e.unsure() {
			e.final = tag
		}
	}
}

// committed records write rid committed at epoch on vm.
func (m *cmodel) committed(at time.Time, key, vm string, epoch uint64, rid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dbs[key]
	if d == nil {
		return
	}
	e := m.mark(key, rid)
	e.commitAt, e.lostAt, e.vm = at, nil, vm
	if o, ok := d.epochOwner[epoch]; ok && o != vm {
		m.violate(key, "two owners serve writes at epoch %d: %s and %s", epoch, o, vm)
	}
	d.epochOwner[epoch] = vm
	if epoch < d.lastEpoch {
		m.violate(key, "%s committed a write at epoch %d after epoch %d had committed writes", vm, epoch, d.lastEpoch)
	}
	d.lastEpoch = max(d.lastEpoch, epoch)
}

// ownerChanged checks that a VM's owner-table epoch for key only rises.
func (m *cmodel) ownerChanged(vm, key string, from, to owner.Local) {
	if to.State != owner.Absent && from.State != owner.Absent && to.Epoch < from.Epoch {
		m.violateNow(key, "%s's owner table went from %s down to %s", vm, from, to)
	}
}

// crashed records a kill of vm, the authority's owner of keys.
func (m *cmodel) crashed(vm string, keys []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.w.clk.Now()
	for _, k := range keys {
		d := m.dbs[k]
		if d == nil {
			continue
		}
		d.crashes = append(d.crashes, int64(now.Sub(m.w.t0)))
		if !d.hasUnjudged {
			d.unjudged, d.hasUnjudged = now, true
		}
	}
}

// durable: a write-back, passivation or fence of key that started at from has published.
func (m *cmodel) durable(key string, from time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d := m.dbs[key]; d != nil && !d.hasUnjudged && from.After(d.durableBefore) {
		d.durableBefore = from
	}
}

// published reports whether key's current generation holds rid.
func (m *cmodel) published(key, rid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Contains(m.db(key).genMarks, rid)
}

// read lists the marks in generation man names (none for generation 0). Objects never change, so each is
// read once; callers get their own copy.
func (m *cmodel) read(man durable.Manifest) ([]string, error) {
	m.objMu.Lock()
	defer m.objMu.Unlock()
	if marks, ok := m.objMarks[man.Object]; ok && man.Generation != 0 {
		return slices.Clone(marks), nil
	}
	marks, err := oracleMarks(m.w.store, m.w.s.Dir, man, observeLog)
	if err == nil && man.Generation != 0 {
		m.objMarks[man.Object] = marks
	}
	return slices.Clone(marks), err
}

// checkMarks checks a database file's marks. pre: m.mu held
func (m *cmodel) checkMarks(d *cdbModel, marks []string, what string) map[string]bool {
	seen := make(map[string]bool, len(marks))
	for _, rid := range marks {
		if seen[rid] {
			m.violate(d.key, "%s: mutation %s applied twice", what, rid)
			continue
		}
		seen[rid] = true
		e := d.marks[rid]
		switch {
		case e == nil:
			m.violate(d.key, "%s: unknown mark %q", what, rid)
		case e.st == none:
			m.violate(d.key, "%s: mutation %s is applied though every answer said nothing ran", what, rid)
		case e.final != "":
			m.violate(d.key, "%s: mutation %s is applied though its final answer was %s", what, rid, e.final)
		}
	}
	return seen
}

// checkGenerations looks at every database's manifest (at quiescence): generations and epochs only rise, and
// each new generation extends the one before.
func (m *cmodel) checkGenerations() {
	keys := m.keys()
	for _, k := range keys {
		man, ok := m.w.store.Manifest(k)
		if !ok {
			continue
		}
		m.mu.Lock()
		d := m.dbs[k]
		switch {
		case man.Generation < d.gen || man.Epoch < d.epoch:
			m.violate(k, "manifest went from generation %d epoch %d back to generation %d epoch %d", d.gen, d.epoch, man.Generation, man.Epoch)
		case man.Generation == d.gen:
			if man.Epoch != d.epoch {
				m.w.tr.add("check", "%s stamped epoch %d (gen %d)", k, man.Epoch, man.Generation)
			}
			d.epoch = man.Epoch
		default:
			marks, err := m.read(man)
			if err != nil {
				m.violate(k, "generation %d cannot be read: %v", man.Generation, err)
				break
			}
			what := fmt.Sprintf("generation %d", man.Generation)
			if len(marks) < len(d.genMarks) || !slices.Equal(marks[:len(d.genMarks)], d.genMarks) {
				m.violate(k, "%s does not extend generation %d (%d marks, had %d)", what, d.gen, len(marks), len(d.genMarks))
			}
			m.checkMarks(d, marks, what)
			m.w.tr.add("check", "%s gen %d epoch %d: %d marks", k, man.Generation, man.Epoch, len(marks))
			d.gen, d.epoch, d.genMarks = man.Generation, man.Epoch, marks
		}
		m.mu.Unlock()
	}
}

// restored judges an owner starting to serve key at epoch from manifest man (its claim or reopen stamp).
func (m *cmodel) restored(key, vm string, epoch owner.Epoch, man durable.Manifest) {
	marks, err := m.read(man)
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dbs[key]
	if d == nil {
		return
	}
	if err != nil {
		m.violate(key, "the generation %d %s starts from cannot be read: %v", man.Generation, vm, err)
		return
	}
	for len(d.crashStarts) < len(d.crashes) { // the first start after a crash: the next segment's state
		d.crashStarts = append(d.crashStarts, marks)
	}
	m.judge(d, marks, fmt.Sprintf("%s starting at epoch %d from generation %d", vm, epoch, man.Generation), d.hasUnjudged, d.unjudged)
}

// judge checks that marks (what an owner starts from) hold every acknowledged write, except, when crash is
// set, those the crash at tc may lose inside the RPO. pre: m.mu held
func (m *cmodel) judge(d *cdbModel, marks []string, what string, crash bool, tc time.Time) {
	now := m.w.clk.Now()
	seen := m.checkMarks(d, marks, what)
	rids := make([]string, 0, len(d.marks))
	for rid := range d.marks {
		rids = append(rids, rid)
	}
	slices.Sort(rids)
	for _, rid := range rids {
		e := d.marks[rid]
		if seen[rid] {
			if e.st != acked && e.st != none {
				e.st, e.ackedAt = acked, m.w.t0
			}
			continue
		}
		committed := !e.commitAt.IsZero()
		switch {
		case committed && !crash:
			m.violate(d.key, "%s: mutation %s committed at %v (%s) is lost, and no crash of the owner excuses it (a hand-off loses nothing)",
				what, rid, e.commitAt.Sub(m.w.t0), e.st)
		case committed && !e.commitAt.Before(tc):
			m.violate(d.key, "%s: mutation %s committed at %v, after the crash at %v, is lost", what, rid, e.commitAt.Sub(m.w.t0), tc.Sub(m.w.t0))
		case committed && e.st == acked:
			m.judgeLoss(d, rid, e, tc, d.durableBefore, what)
		case committed: // not answered yet: judged if its ok arrives
			e.lostAt = &loss{e.commitAt, tc, d.durableBefore}
		case e.st == acked:
			m.violate(d.key, "%s: acknowledged mutation %s (at %v), which no live agent committed, is missing", what, rid, e.ackedAt.Sub(m.w.t0))
		}
		e.commitAt = time.Time{}
		if e.st == acked || e.st == maybe {
			e.st = lost
		}
	}
	d.hasUnjudged, d.durableBefore = false, now
}

// judgeLoss judges acknowledged write rid, lost at the crash at tc: allowed only inside the RPO, and never
// for a write committed before a write-back or fence that published. pre: m.mu held
func (m *cmodel) judgeLoss(d *cdbModel, rid string, e *cmark, tc, durableBefore time.Time, what string) {
	e.st = lost
	switch healthy := m.w.health.longestHealthyIn(e.ackedAt, tc, d.key, vmScope(e.vm)); {
	case e.ackedAt.Before(durableBefore):
		m.violate(d.key, "%s: mutation %s acknowledged at %v, before a write-back or fence that published (started %v), is lost at the crash at %v",
			what, rid, e.ackedAt.Sub(m.w.t0), durableBefore.Sub(m.w.t0), tc.Sub(m.w.t0))
	case healthy >= m.w.s.RPO:
		m.violate(d.key, "%s: mutation %s acknowledged at %v is lost at the crash at %v after a fault-free %v (RPO %v)",
			what, rid, e.ackedAt.Sub(m.w.t0), tc.Sub(m.w.t0), healthy, m.w.s.RPO)
	default:
		m.lostOK++
		if tc.Sub(e.ackedAt) >= m.w.s.RPO {
			m.excused++
		}
		m.w.tr.add("check", "%s lost %s inside the RPO (acked %v)", d.key, rid, e.ackedAt.Sub(m.w.t0))
	}
}

// final judges every database at the end as a crash would (its manifest now), then checks the histories.
func (m *cmodel) final() {
	m.checkGenerations()
	keys := m.keys()
	end := m.w.clk.Now()
	for _, k := range keys {
		man, _ := m.w.store.Manifest(k)
		marks, err := m.read(man)
		m.mu.Lock()
		d := m.dbs[k]
		tc := end
		if d.hasUnjudged {
			tc = d.unjudged
		}
		if err != nil {
			m.violate(k, "the final generation cannot be read: %v", err)
		} else {
			m.judge(d, marks, "the end of the run", true, tc)
		}
		m.linearizable(d)
		m.mu.Unlock()
	}
}

// --- Linearizability (the IR-5a Jepsen models) ------------------------------------------------------------------

type regIn struct {
	key   int
	write bool
	value int64
	rid   string // a write's request id (its mark)
}

type regOut struct{ value int64 }

type regState struct {
	known bool
	v     int64
}

// registerModel starts from init or, with init.known false (after a crash), from one of allowed.
func registerModel(init regState, allowed map[int64]bool) linearize.Model[regState, regIn, regOut] {
	return linearize.Model[regState, regIn, regOut]{
		Init: func() regState { return init },
		Step: func(s regState, in regIn, out regOut, _ bool) (bool, regState) {
			if in.write {
				return true, regState{true, in.value}
			}
			if !s.known {
				return allowed[out.value], regState{true, out.value}
			}
			return out.value == s.v, s
		},
		Key: func(s regState) string { return fmt.Sprint(s.known, s.v) },
	}
}

type bankIn struct {
	transfer       bool
	from, to       int
	amount         int64
	vFrom, vTo     int64
	newFrom, newTo int64
	gotFrom, gotTo int64 // the versions the transfer answered; 0: it has no answer
	accounts       []int
	rid            string // a transfer's request id (its mark)
}

type bankOut struct {
	balances, versions []int64
	seq                int64 // the commit sequence the read answered
}

// bankState is what the model knows of each account. A version only rises, by how much is the server's to say
// (spec §6.3: one, or up to the incarnation's floor), so the model learns versions from the answers: exact
// marks the accounts whose version it has seen; of the others it knows only that the version is above
// versions[i] (a new account, or a transfer applied without an answer).
type bankState struct {
	known, exact       [nAccts]bool
	balances, versions [nAccts]int64
}

// account is a balance at a version; version 0 in an allowed set stands for any version.
type account struct{ balance, version int64 }

// holds reports whether account i may be at got: what the model knows of it, or after a crash (unknown) one of
// allowed.
func (s *bankState) holds(i int, got account, allowed map[account]bool) bool {
	switch {
	case !s.known[i]:
		return allowed[got] || allowed[account{got.balance, 0}]
	case s.balances[i] != got.balance:
		return false
	case s.exact[i]:
		return s.versions[i] == got.version
	}
	return got.version > s.versions[i]
}

// bankModel starts from the new bank (known) or, after a crash, from one of allowed per account.
func bankModel(known bool, allowed [nAccts]map[account]bool) linearize.Model[bankState, bankIn, bankOut] {
	return linearize.Model[bankState, bankIn, bankOut]{
		Init: func() bankState {
			var s bankState
			for i := range nAccts {
				s.known[i], s.balances[i] = known, balance0
			}
			return s
		},
		Step: func(s bankState, in bankIn, out bankOut, _ bool) (bool, bankState) {
			if in.transfer {
				for _, a := range [2]struct {
					i                int
					v, old, bal, got int64
				}{{in.from, in.vFrom, in.newFrom + in.amount, in.newFrom, in.gotFrom}, {in.to, in.vTo, in.newTo - in.amount, in.newTo, in.gotTo}} {
					if !s.holds(a.i, account{a.old, a.v}, allowed[a.i]) || a.got != 0 && a.got <= a.v {
						return false, s
					}
					s.known[a.i], s.balances[a.i], s.exact[a.i], s.versions[a.i] = true, a.bal, a.got != 0, max(a.got, a.v)
				}
				return true, s
			}
			for i, a := range in.accounts {
				got := account{out.balances[i], out.versions[i]}
				if !s.holds(a, got, allowed[a]) {
					return false, s
				}
				s.known[a], s.balances[a], s.exact[a], s.versions[a] = true, got.balance, true, got.version
			}
			return true, s
		},
		Key: func(s bankState) string { return fmt.Sprint(s.known, s.exact, s.balances, s.versions) },
	}
}

func (m *cmodel) regOp(key string, op linearize.Op[regIn, regOut]) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dbs[key]
	d.regOps = append(d.regOps, op)
}

func (m *cmodel) bankOp(key string, op linearize.Op[bankIn, bankOut]) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dbs[key]
	d.bankOps = append(d.bankOps, op)
	if op.Input.transfer {
		return
	}
	// Version reuse: a version names one content of a document for good (docexec.VersionFloor). Before the
	// floor, a database rolled back by a kill -9 issued its lost versions again for other contents, and an
	// expect computed before the crash passed against another document (swarm seed 2761).
	if d.seenVersion == nil {
		d.seenVersion = map[[2]int64]int64{}
	}
	for i, a := range op.Input.accounts {
		k := [2]int64{int64(a), op.Output.versions[i]}
		if b, ok := d.seenVersion[k]; ok && b != op.Output.balances[i] {
			m.violate(key, "version reuse: account %d read at version %d with balance %d, earlier with %d", a, k[1], op.Output.balances[i], b)
		}
		d.seenVersion[k] = op.Output.balances[i]
	}
	// Sequence reuse, the same flaw one level up: two answers with one sequence saw the same documents (spec
	// §6.5), or a table assert taken on a lost state holds against the restored one.
	if d.seenSeq == nil {
		d.seenSeq = map[[2]int64]int64{}
	}
	for i, a := range op.Input.accounts {
		k := [2]int64{int64(a), op.Output.seq}
		if v, ok := d.seenSeq[k]; ok && v != op.Output.versions[i] {
			m.violate(key, "sequence reuse: account %d read at sequence %d with version %d, earlier with %d", a, k[1], op.Output.versions[i], v)
		}
		d.seenSeq[k] = op.Output.versions[i]
	}
}

// sortOps orders a history by call, return and input, so its order does not depend on which goroutine
// recorded first within a step.
func sortOps[I, O any](ops []linearize.Op[I, O], show func(linearize.Op[I, O]) string) {
	slices.SortStableFunc(ops, func(a, b linearize.Op[I, O]) int {
		switch {
		case a.Call != b.Call:
			return cmp.Compare(a.Call, b.Call)
		case a.Return != b.Return:
			return cmp.Compare(a.Return, b.Return)
		}
		return strings.Compare(show(a), show(b))
	})
}

func showReg(o linearize.Op[regIn, regOut]) string {
	if o.Input.write {
		return fmt.Sprintf("write r%d=%d", o.Input.key, o.Input.value)
	}
	return fmt.Sprintf("read r%d → %d", o.Input.key, o.Output.value)
}

func showBank(o linearize.Op[bankIn, bankOut]) string {
	if in := o.Input; in.transfer {
		return fmt.Sprintf("transfer %d→%d: a%d=%d@v%d a%d=%d@v%d", in.from, in.to, in.from, in.newFrom, in.vFrom, in.to, in.newTo, in.vTo)
	}
	return fmt.Sprintf("read %v → %v @ %v", o.Input.accounts, o.Output.balances, o.Output.versions)
}

// linearizable checks d's histories, segmented at its owner's crashes. pre: m.mu held
func (m *cmodel) linearizable(d *cdbModel) {
	sortOps(d.regOps, showReg)
	sortOps(d.bankOps, showBank)
	crashes := d.crashes
	for k := range nRegs {
		var all []linearize.Op[regIn, regOut]
		for _, o := range d.regOps {
			if o.Input.key == k {
				all = append(all, o)
			}
		}
		for seg, ops := range segments(all, crashes, func(in regIn) bool { return in.write }) {
			allowed := map[int64]bool{0: true}
			if seg > 0 {
				start, ok := startOf(d, seg)
				if ok { // the register's value in the generation the owner restarted from: its last write there
					allowed, ops = map[int64]bool{lastIn(all, start, func(in regIn) (string, int64) { return in.rid, in.value }, 0): true}, unapplied(ops, start, func(in regIn) string { return in.rid })
				} else {
					for _, o := range all {
						if o.Input.write && o.Call < crashes[seg-1] {
							allowed[o.Input.value] = true
						}
					}
				}
			}
			m.checked += len(ops)
			if res := linearize.Check(registerModel(regState{known: seg == 0}, allowed), ops); !res.OK {
				m.violate(d.key, "register r%d, segment %d: not linearizable (gave up: %v; longest prefix %d of %d ops)\n%s", k, seg, res.GaveUp,
					len(res.Longest), len(ops), around(ops, res.Longest, showReg))
			}
		}
	}
	for seg, ops := range segments(d.bankOps, crashes, func(in bankIn) bool { return in.transfer }) {
		var allowed [nAccts]map[account]bool
		for i := range allowed {
			allowed[i] = map[account]bool{{balance0, 0}: true}
		}
		if start, ok := startOf(d, seg); seg > 0 && ok {
			// Each account's balance and version in the generation the owner restarted from: its last transfer there.
			ops = unapplied(ops, start, func(in bankIn) string { return in.rid })
			for i := range allowed {
				allowed[i] = map[account]bool{lastIn(d.bankOps, start, func(in bankIn) (string, account) {
					switch i {
					case in.from:
						return in.rid, account{in.newFrom, in.gotFrom}
					case in.to:
						return in.rid, account{in.newTo, in.gotTo}
					}
					return "", account{}
				}, account{balance0, 0}): true}
			}
		} else if seg > 0 {
			for _, o := range d.bankOps {
				if in := o.Input; in.transfer && o.Call < crashes[seg-1] {
					allowed[in.from][account{in.newFrom, in.gotFrom}] = true
					allowed[in.to][account{in.newTo, in.gotTo}] = true
				}
			}
		}
		m.checked += len(ops)
		if res := linearize.Check(bankModel(seg == 0, allowed), ops); !res.OK {
			m.violate(d.key, "bank, segment %d: not linearizable (gave up: %v; longest prefix %d of %d ops)\n%s", seg, res.GaveUp,
				len(res.Longest), len(ops), around(ops, res.Longest, showBank))
		}
	}
}

// startOf is the marks of the generation segment seg (> 0) started from: the first owner start after the crash
// that opened it. ok is false when no owner started before the run ended.
func startOf(d *cdbModel, seg int) (map[string]int, bool) {
	if seg == 0 || seg > len(d.crashStarts) {
		return nil, false
	}
	pos := make(map[string]int, len(d.crashStarts[seg-1]))
	for i, rid := range d.crashStarts[seg-1] {
		pos[rid] = i
	}
	return pos, true
}

// lastIn is the value of the write (of ops, in start's commit order) applied last in start, or none if no
// write of ops is there. of names an op's mark ("" for an op that does not write this value) and value.
func lastIn[I, O, V any](ops []linearize.Op[I, O], start map[string]int, of func(I) (string, V), none V) V {
	best, v := -1, none
	for _, o := range ops {
		rid, val := of(o.Input)
		if p, ok := start[rid]; rid != "" && ok && p > best {
			best, v = p, val
		}
	}
	return v
}

// unapplied drops the writes already in start (carried over from before the crash with an unknown outcome):
// they are part of the start state, and the request id makes a retry a duplicate, not a second apply.
func unapplied[I, O any](ops []linearize.Op[I, O], start map[string]int, rid func(I) string) []linearize.Op[I, O] {
	return slices.DeleteFunc(slices.Clone(ops), func(o linearize.Op[I, O]) bool {
		_, in := start[rid(o.Input)]
		return o.Return == linearize.Unknown && rid(o.Input) != "" && in
	})
}

// segments splits a history at the crash times (the IR-5a Jepsen run's rule): an operation called and answered
// between the same two crashes belongs to that segment; one in flight across a crash, or never answered: a read
// is left out, a write goes, with an unknown outcome, to every segment its interval reaches.
func segments[I, O any](ops []linearize.Op[I, O], crashes []int64, write func(I) bool) [][]linearize.Op[I, O] {
	out := make([][]linearize.Op[I, O], len(crashes)+1)
	segOf := func(t int64) int {
		n := 0
		for n < len(crashes) && t > crashes[n] {
			n++
		}
		return n
	}
	for _, o := range ops {
		sc, sr := segOf(o.Call), segOf(o.Return)
		if sc == sr && o.Return != linearize.Unknown {
			out[sc] = append(out[sc], o)
			continue
		}
		if !write(o.Input) {
			continue
		}
		o.Return = linearize.Unknown
		for seg := sc; seg <= sr; seg++ {
			out[seg] = append(out[seg], o)
		}
	}
	return out
}

// around describes a failing history: the first operation after the longest linearizable prefix that returned
// and could not be placed, and the operations around it in time.
func around[I, O any](ops []linearize.Op[I, O], longest []int, show func(linearize.Op[I, O]) string) string {
	in := make([]bool, len(ops))
	for _, i := range longest {
		in[i] = true
	}
	stuck := -1
	for i, o := range ops {
		if !in[i] && o.Return != linearize.Unknown && (stuck < 0 || o.Return < ops[stuck].Return) {
			stuck = i
		}
	}
	if stuck < 0 {
		return "(no stuck operation)"
	}
	lo, hi := ops[stuck].Call-int64(2*time.Second), ops[stuck].Return+int64(500*time.Millisecond)
	var idx []int
	for i, o := range ops {
		if o.Call <= hi && o.Return >= lo {
			idx = append(idx, i)
		}
	}
	if len(idx) > 40 {
		idx = idx[len(idx)-40:]
	}
	var b strings.Builder
	sec := func(t int64) string {
		if t == linearize.Unknown {
			return "?"
		}
		return fmt.Sprintf("%.4f", float64(t)/1e9)
	}
	for _, i := range idx {
		mark := " "
		switch {
		case i == stuck:
			mark = "!"
		case in[i]:
			mark = "+"
		}
		fmt.Fprintf(&b, "  %s [%s, %s] %s\n", mark, sec(ops[i].Call), sec(ops[i].Return), show(ops[i]))
	}
	return b.String()
}
