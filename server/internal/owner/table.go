package owner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/errs"
)

// Config wires a Table to its ports.
type Config struct {
	Self      string // this process's owner id, as the authority names it (option 4: the VM id)
	Store     LocalStore
	Placement Placement
	WriteBack WriteBack
	// StampRetries bounds how often Claim re-reads the manifest when its Stamp loses a race. Default 3.
	StampRetries int
	// OnChange, if set, is called after a key's state, epoch or superseded flag changes (not on generation-only
	// updates, nor for the states Open loads), on the goroutine that made the change. It must be fast and must
	// not call into the Table. The agent logs and counts transitions with it (OWNER.SUPERSEDED etc.; Release It
	// O3); the table itself does not log.
	OnChange func(key string, from, to Local)
}

// Ack is a fence acknowledgement: the key admits nothing at Epoch any more, and Gen, published at Epoch, holds
// every write it admitted.
type Ack struct {
	Key   string
	Epoch Epoch
	Gen   Generation
}

// Table is one process's ownership state for every key it holds. Safe for concurrent use.
//
// Hot path: Admit reads the key's state with one sync.Map lookup and two atomics; no I/O, no lock shared
// between keys. Lifecycle calls (Claim, Fence, Reconcile, Forget) are serialized per key and may block on I/O.
//
// Invariants (per key): one slot holds one Local, so at most one ACTIVE epoch per key in this table; the local
// epoch never decreases; a permit exists only while the state it was admitted in was Active at its epoch, and
// Fence's write-back starts only after every permit is done.
type Table struct {
	cfg   Config
	slots sync.Map // string → *slot; slots are never removed (Forget leaves an absent slot)
}

// drainSig is closed once, when a fencing slot's in-flight count reaches 0.
type drainSig struct {
	ch   chan struct{}
	done atomic.Bool
}

func (d *drainSig) signal() {
	if d.done.CompareAndSwap(false, true) {
		close(d.ch)
	}
}

// view is an immutable snapshot of a slot's state. drain is non-nil exactly while Fencing.
type view struct {
	l     Local
	drain *drainSig
}

type slot struct {
	key      string
	view     atomic.Pointer[view]
	inflight atomic.Int64
	life     chan struct{} // lifecycle lock (cap 1): Claim, Fence, Reconcile, Forget, durable supersede
	pub      chan struct{} // publish lock (cap 1): at most one publish per key at a time
	onChange func(key string, from, to Local)

	// Guarded by life: the final write-back of a fence succeeded at finalEpoch but FENCED is not yet durable.
	finalOK    bool
	finalEpoch Epoch
	finalGen   Generation
}

func newSlot(key string, l Local, onChange func(string, Local, Local)) *slot {
	s := &slot{key: key, life: make(chan struct{}, 1), pub: make(chan struct{}, 1), onChange: onChange}
	v := &view{l: l}
	if l.State == Fencing {
		v.drain = &drainSig{ch: make(chan struct{})}
	}
	s.view.Store(v)
	return s
}

func lock(ctx context.Context, ch chan struct{}) error {
	select {
	case ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func unlock(ch chan struct{}) { <-ch }

// Open loads the durable state. Every ACTIVE record comes back Closed: nothing is admitted for it until
// Reconcile (or ReconcileAll) has asked the authority. FENCED records come back Fenced.
//
//	post: err == nil ⇒ every loaded key is Closed or Fenced
func Open(ctx context.Context, cfg Config) (*Table, error) {
	switch {
	case cfg.Self == "":
		return nil, errs.Wrap(module, ErrInvalid, "cannot open ownership table: Self is empty")
	case cfg.Store == nil || cfg.Placement == nil || cfg.WriteBack == nil:
		return nil, errs.Wrap(module, ErrInvalid, "cannot open ownership table: Store, Placement and WriteBack are required")
	}
	if cfg.StampRetries <= 0 {
		cfg.StampRetries = 3
	}
	recs, err := cfg.Store.Load(ctx)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot open ownership table: local state unreadable; nothing admitted")
	}
	t := &Table{cfg: cfg}
	for _, r := range recs {
		if err := r.Validate(); err != nil {
			return nil, errs.Wrap(module, err, "cannot open ownership table: bad record for %q; nothing admitted", r.Key)
		}
		l := Local{State: Closed, Epoch: r.Epoch, Gen: r.Gen}
		if r.State == Fenced {
			l = Local{State: Fenced, Epoch: r.Epoch, Gen: r.Gen, Superseded: r.Superseded}
		}
		if _, dup := t.slots.LoadOrStore(r.Key, newSlot(r.Key, l, cfg.OnChange)); dup {
			return nil, errs.Wrap(module, ErrInvalid, "cannot open ownership table: two records for %q; nothing admitted", r.Key)
		}
	}
	return t, nil
}

func (t *Table) lookup(key string) *slot {
	v, ok := t.slots.Load(key)
	if !ok {
		return nil
	}
	return v.(*slot)
}

func (t *Table) slotFor(key string) *slot {
	if s := t.lookup(key); s != nil {
		return s
	}
	v, _ := t.slots.LoadOrStore(key, newSlot(key, Local{}, t.cfg.OnChange))
	return v.(*slot)
}

// State returns key's local state (Absent if unknown). For status pages and tests; never for admission.
func (t *Table) State(key string) Local {
	s := t.lookup(key)
	if s == nil {
		return Local{}
	}
	return s.view.Load().l
}

// Keys lists every key with a slot, in no order.
func (t *Table) Keys() []string {
	var out []string
	t.slots.Range(func(k, _ any) bool { out = append(out, k.(string)); return true })
	return out
}

// Permit is one admitted request. The executor calls Done exactly once when the request has finished
// (committed, rolled back or failed); Fence waits for it. The zero Permit's Done does nothing.
type Permit struct{ s *slot }

// Done releases the permit.
//
//	pre: called once per admitted Permit (a second call is a bug and trips an assertion)
func (p Permit) Done() {
	if p.s != nil {
		p.s.release()
	}
}

func (s *slot) release() {
	n := s.inflight.Add(-1)
	if n < 0 { // guarded: assert.That's arguments would box s.key and allocate on every release
		assert.That(false, "owner: %q in-flight count went negative (Permit.Done called twice?)", s.key)
	}
	if n == 0 {
		if v := s.view.Load(); v.drain != nil {
			v.drain.signal()
		}
	}
}

// Admit decides whether a request for key at epoch may run here, per the admission table (decide). On
// success the caller holds a Permit and must call Done when the request has finished.
//
// Errors are the bare sentinels ErrWrongOwner, ErrMoving, ErrFenced, ErrUnavailable (no allocation).
//
//	post: err == nil ⇒ the key was Active at epoch when admitted, and any Fence waits for this Permit
//	post: err != nil ⇒ no Permit is held
func (t *Table) Admit(key string, epoch Epoch) (Permit, error) {
	s := t.lookup(key)
	if s == nil {
		return Permit{}, ErrWrongOwner
	}
	// Increment before reading the state; Fence stores Fencing before reading the count. Go atomics are
	// sequentially consistent, so either Fence sees this increment and waits, or this read sees Fencing.
	s.inflight.Add(1)
	if err := decide(s.view.Load().l, epoch); err != nil {
		s.release()
		return Permit{}, err
	}
	return Permit{s}, nil
}

// Do runs fn under a permit: Admit, fn, Done. It returns the admission error, or fn's error.
func (t *Table) Do(key string, epoch Epoch, fn func() error) error {
	p, err := t.Admit(key, epoch)
	if err != nil {
		return err
	}
	defer p.Done()
	return fn()
}

// install replaces view old by next. It reports false if the slot changed since old was read.
//
//	pre: next follows from old.l by Step
func (s *slot) install(old *view, next Local) bool {
	if next.Epoch < old.l.Epoch && next.State != Absent { // guarded: the arguments would allocate
		assert.That(false, "owner: %q epoch went backwards: %s → %s", s.key, old.l, next)
	}
	nv := &view{l: next}
	switch {
	case next.State == Fencing && old.drain != nil:
		nv.drain = old.drain
	case next.State == Fencing:
		nv.drain = &drainSig{ch: make(chan struct{})}
	}
	if !s.view.CompareAndSwap(old, nv) {
		return false
	}
	if s.onChange != nil && (old.l.State != next.State || old.l.Epoch != next.Epoch || old.l.Superseded != next.Superseded) {
		s.onChange(s.key, old.l, next)
	}
	return true
}

// apply runs Step on the current state and installs the result, retrying if a concurrent gen update or
// supersede changed the slot in between.
func (s *slot) apply(ev Event) (Local, error) {
	for {
		old := s.view.Load()
		next, err := Step(old.l, ev)
		if err != nil {
			return old.l, err
		}
		if next == old.l { // a Fencing view always carries its drain signal already
			return next, nil
		}
		if s.install(old, next) {
			return next, nil
		}
	}
}

// noteGen records that this process published gen at epoch.
func (s *slot) noteGen(epoch Epoch, gen Generation) {
	for {
		old := s.view.Load()
		l := old.l
		if l.Epoch != epoch || gen <= l.Gen || (l.State != Active && l.State != Fencing) {
			return
		}
		l.Gen = gen
		if s.install(old, l) {
			return
		}
	}
}

// supersedeNow stops admission at epoch at once (in memory), without the lifecycle lock. It reports whether
// the slot is now fenced-superseded at epoch, so the caller should make that durable.
func (s *slot) supersedeNow(epoch Epoch) bool {
	for {
		old := s.view.Load()
		if old.l.Epoch != epoch {
			return false // a newer claim already replaced that epoch
		}
		next, _ := Step(old.l, Event{Kind: EvSupersede})
		if next == old.l {
			return next.State == Fenced && next.Superseded
		}
		if s.install(old, next) {
			return next.Superseded
		}
	}
}

// persistSupersede writes FENCED(superseded) for the slot's current state.
//
//	pre: the caller holds s.life
func (t *Table) persistSupersede(ctx context.Context, s *slot, epoch Epoch) error {
	l := s.view.Load().l
	if l.State != Fenced || !l.Superseded || l.Epoch != epoch {
		return nil
	}
	s.finalOK = false
	return t.cfg.Store.Put(ctx, Record{Key: s.key, State: Fenced, Epoch: l.Epoch, Gen: l.Gen, Superseded: true})
}

// supersede fences the slot at epoch because a higher epoch exists: in memory first (admission stops at
// once), then durably.
func (t *Table) supersede(ctx context.Context, s *slot, epoch Epoch) error {
	if !s.supersedeNow(epoch) {
		return nil
	}
	if err := lock(ctx, s.life); err != nil {
		return err
	}
	defer unlock(s.life)
	return t.persistSupersede(ctx, s, epoch)
}

func superseded(key string, epoch Epoch, cause error, perr error) error {
	e := errs.Wrap(module, fmt.Errorf("%w: %w", ErrSuperseded, cause),
		"%q is owned at a higher epoch than %d; stopped admitting it, do not retry here", key, epoch).With("key", key)
	if perr != nil {
		e = e.With("persist_error", perr.Error())
	}
	return e
}

// Publish runs one write-back (fn, normally WriteBack.Publish) for key at epoch outside a fence: periodic
// uploads and passivation. It holds a permit while fn runs, so Fence waits for it, and it serializes
// publishes per key, so ErrStale from fn always means a higher epoch exists (this process stamped its own
// epoch at Claim). On ErrStale the key is fenced durably and the error matches ErrSuperseded and ErrStale.
//
//	post: err == nil ⇒ gen is recorded as the key's newest generation at epoch
func (t *Table) Publish(ctx context.Context, key string, epoch Epoch, fn func(context.Context) (Generation, error)) (Generation, error) {
	s := t.lookup(key)
	if s == nil {
		return 0, errs.Wrap(module, ErrWrongOwner, "cannot publish %q: not held here; nothing changed", key)
	}
	if err := lock(ctx, s.pub); err != nil {
		return 0, errs.Wrap(module, err, "cannot publish %q: cancelled while waiting; nothing changed, safe to retry", key)
	}
	p, err := t.Admit(key, epoch)
	if err != nil {
		unlock(s.pub)
		return 0, errs.Wrap(module, err, "cannot publish %q at epoch %d: not active here; nothing changed", key, epoch)
	}
	gen, err := fn(ctx)
	stale := errors.Is(err, ErrStale)
	switch {
	case stale:
		s.supersedeNow(epoch) // stop admitting before anyone else can be admitted
	case err == nil:
		s.noteGen(epoch, gen)
	}
	p.Done()
	unlock(s.pub)
	if stale {
		return 0, superseded(key, epoch, err, t.supersede(ctx, s, epoch))
	}
	if err != nil {
		return 0, errs.Wrap(module, err, "cannot publish %q at epoch %d; the previous generation stays current, safe to retry", key, epoch)
	}
	return gen, nil
}

// Claim makes this process the active owner of key, as the authority says. In order: read the authority
// (must be ACTIVE with Self as owner), read the manifest, stamp it with the authority's epoch (so every
// lower-epoch publish fails from now on), record ACTIVE durably, then admit.
//
// The generation it starts from is the manifest's (≥ the authority's): if a zombie published after the flip,
// starting from its generation keeps the published history linear.
//
//	post: err == nil ⇒ State(key) is Active at the authority's epoch, durable, and storage carries that epoch
//	post: err != nil ⇒ nothing is admitted at a new epoch (a stamp may have been written; it is harmless)
//	Idempotent: claiming an epoch already active here only re-reads the authority.
func (t *Table) Claim(ctx context.Context, key string) (Local, error) {
	if !ValidKey(key) {
		return Local{}, errs.Wrap(module, ErrInvalid, "cannot claim %q: key must be 1..%d bytes", key, MaxKeyLen)
	}
	s := t.slotFor(key)
	if err := lock(ctx, s.life); err != nil {
		return Local{}, errs.Wrap(module, err, "cannot claim %q: cancelled; nothing changed, safe to retry", key)
	}
	defer unlock(s.life)
	cur := s.view.Load().l
	if cur.State == Closed {
		return cur, errs.Wrap(module, ErrUnavailable, "cannot claim %q: not reconciled since restart; reconcile first", key)
	}
	p, err := t.cfg.Placement.Get(ctx, key)
	if err != nil {
		return cur, errs.Wrap(module, fmt.Errorf("%w: %w", ErrUnavailable, err), "cannot claim %q: authority unreadable; nothing changed, safe to retry", key)
	}
	if p.Epoch == 0 || (p.State != PlaceActive && p.State != PlaceMoving) {
		return cur, errs.Wrap(module, ErrInvalid, "cannot claim %q: authority returned a malformed row %+v", key, p)
	}
	switch {
	case p.Owner != t.cfg.Self:
		return cur, errs.Wrap(module, ErrWrongOwner, "cannot claim %q: owned by %q at epoch %d; nothing changed", key, p.Owner, p.Epoch)
	case p.State == PlaceMoving:
		return cur, errs.Wrap(module, ErrMoving, "cannot claim %q: a move is in progress; retry after it", key)
	}
	if _, err := Step(cur, Event{Kind: EvClaim, Epoch: p.Epoch, Gen: p.Gen}); err != nil {
		return cur, errs.Wrap(module, err, "cannot claim %q at epoch %d from %s; nothing changed", key, p.Epoch, cur)
	}
	if cur.State == Active && cur.Epoch == p.Epoch {
		return cur, nil
	}
	gen, err := t.stamp(ctx, key, p)
	if err != nil {
		return cur, err
	}
	if err := t.cfg.Store.Put(ctx, Record{Key: key, State: Active, Epoch: p.Epoch, Gen: gen}); err != nil {
		return cur, errs.Wrap(module, err, "cannot claim %q: local record not written; not admitting, safe to retry", key)
	}
	l, err := s.apply(Event{Kind: EvClaim, Epoch: p.Epoch, Gen: gen})
	assert.That(err == nil, "owner: claim of %q passed Step and then failed: %v", key, err)
	return l, nil
}

// errStorageNewer marks a stamp refused because storage carries an epoch above p.Epoch (with ErrUnavailable):
// Claim refuses, Reconcile supersedes the key.
var errStorageNewer = errors.New("owner: storage has a newer epoch")

// stamp writes p.Epoch into the manifest and returns the generation it stamped. It refuses, admitting nothing,
// if storage shows a newer epoch (errStorageNewer) or a generation behind p.Gen (no manifest counts as 0).
func (t *Table) stamp(ctx context.Context, key string, p Place) (Generation, error) {
	var last error
	for range t.cfg.StampRetries {
		m, err := t.cfg.WriteBack.Head(ctx, key)
		switch {
		case errors.Is(err, ErrNoManifest):
			m = Manifest{}
		case err != nil:
			return 0, errs.Wrap(module, fmt.Errorf("%w: %w", ErrUnavailable, err), "cannot stamp %q: manifest unreadable; nothing changed, safe to retry", key)
		}
		if m.Epoch > p.Epoch {
			return 0, errs.Wrap(module, fmt.Errorf("%w: %w", ErrUnavailable, errStorageNewer), "cannot stamp %q: storage has epoch %d, newer than ours %d; not admitting", key, m.Epoch, p.Epoch)
		}
		if m.Gen < p.Gen {
			return 0, errs.Wrap(module, ErrUnavailable, "cannot stamp %q: storage generation %d is behind the known %d; not admitting", key, m.Gen, p.Gen)
		}
		err = t.cfg.WriteBack.Stamp(ctx, key, p.Epoch, m.Gen)
		if err == nil {
			return m.Gen, nil
		}
		if !errors.Is(err, ErrStale) {
			return 0, errs.Wrap(module, err, "cannot stamp %q: stamp failed; not admitting, safe to retry", key)
		}
		last = err
	}
	return 0, errs.Wrap(module, last, "cannot stamp %q: the manifest kept changing during %d stamps; not admitting, safe to retry", key, t.cfg.StampRetries)
}

// drainWait waits until no permit is outstanding.
//
//	pre: the slot is Fencing (so no new permit can be granted)
func (s *slot) drainWait(ctx context.Context) error {
	v := s.view.Load()
	if v.drain == nil || s.inflight.Load() == 0 {
		return nil
	}
	select {
	case <-v.drain.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Fence hands key off: stop admitting, wait for every permit, run the final write-back at the local epoch,
// record FENCED durably, and return the acknowledgement. The control plane may flip the owner only after it
// has an Ack.
//
// Failure policy: if draining is cancelled, the write-back fails or the record cannot be written, the key
// stays Fencing (admits nothing, not writable) and Fence returns an error that says so; calling Fence again
// resumes where it stopped. Nothing is ever reopened at this epoch: only a Claim at a higher epoch can. If the
// write-back finds a higher epoch (ErrStale) the key is fenced as superseded and Fence returns ErrSuperseded,
// with no Ack: the caller must not flip on it.
//
//	post: err == nil ⇒ State(key) is Fenced(ack.Epoch, ack.Gen), durable, and every permit admitted at
//	      ack.Epoch finished before the write-back of ack.Gen started
//	Idempotent: a fenced key returns the same Ack again. Safe to call concurrently with Admit and with itself.
func (t *Table) Fence(ctx context.Context, key string) (Ack, error) {
	s := t.lookup(key)
	if s == nil {
		return Ack{}, errs.Wrap(module, ErrWrongOwner, "cannot fence %q: not held here; nothing changed", key)
	}
	if err := lock(ctx, s.life); err != nil {
		return Ack{}, errs.Wrap(module, err, "cannot fence %q: cancelled while waiting; nothing changed, safe to retry", key)
	}
	defer unlock(s.life)

	l, err := s.apply(Event{Kind: EvStop})
	if err != nil {
		return Ack{}, errs.Wrap(module, err, "cannot fence %q: %s; nothing changed", key, l)
	}
	if l.State == Fenced {
		return t.fencedResult(ctx, s, l)
	}
	if err := s.drainWait(ctx); err != nil {
		return Ack{}, errs.Wrap(module, err, "cannot fence %q: in-flight requests did not finish; admission stays stopped, retry Fence", key)
	}
	// A publish that was in flight may have found a higher epoch while we drained.
	if l = s.view.Load().l; l.State == Fenced {
		return t.fencedResult(ctx, s, l)
	}
	if !s.finalOK || s.finalEpoch != l.Epoch {
		if err := lock(ctx, s.pub); err != nil {
			return Ack{}, errs.Wrap(module, err, "cannot fence %q: cancelled before write-back; admission stays stopped, retry Fence", key)
		}
		gen, err := t.cfg.WriteBack.Publish(ctx, key, l.Epoch)
		unlock(s.pub)
		if errors.Is(err, ErrStale) {
			s.supersedeNow(l.Epoch)
			return Ack{}, superseded(key, l.Epoch, err, t.persistSupersede(ctx, s, l.Epoch))
		}
		if err != nil {
			return Ack{}, errs.Wrap(module, err, "cannot fence %q: final write-back failed; admission stays stopped, nothing acknowledged, retry Fence", key)
		}
		if gen < l.Gen {
			return Ack{}, errs.Wrap(module, ErrInvalid, "cannot fence %q: write-back returned generation %d, older than %d; admission stays stopped", key, gen, l.Gen)
		}
		s.finalOK, s.finalEpoch, s.finalGen = true, l.Epoch, gen
	}
	if err := t.cfg.Store.Put(ctx, Record{Key: key, State: Fenced, Epoch: l.Epoch, Gen: s.finalGen}); err != nil {
		return Ack{}, errs.Wrap(module, err, "cannot fence %q: write-back done but the fence record was not written; admission stays stopped, retry Fence", key)
	}
	l, err = s.apply(Event{Kind: EvFenced, Gen: s.finalGen})
	assert.That(err == nil && l.State == Fenced, "owner: fence of %q could not install fenced: %s %v", key, l, err)
	s.finalOK = false
	return Ack{Key: key, Epoch: l.Epoch, Gen: l.Gen}, nil
}

// fencedResult is Fence's answer for a key that is already fenced.
//
//	pre: the caller holds s.life
func (t *Table) fencedResult(ctx context.Context, s *slot, l Local) (Ack, error) {
	if l.Superseded {
		return Ack{}, superseded(s.key, l.Epoch, ErrFenced, t.persistSupersede(ctx, s, l.Epoch))
	}
	return Ack{Key: s.key, Epoch: l.Epoch, Gen: l.Gen}, nil
}

// Reconcile settles a key loaded as Closed with the authority (reconcile table) and storage: reopen it (after
// re-stamping the manifest at the local epoch), keep it stopped for a move in progress, or fence it. If either
// cannot be read, or the reopen stamp fails, the key stays Closed. Storage at a higher epoch than the local one
// supersedes the key even if the authority says reopen (defense in depth against a stale or broken authority
// read). A local record with generation > 0 whose manifest is missing (or behind it) stays Closed: the stamp
// refuses to start from less than this process knows was published.
// Keys in any other state are left alone.
//
//	post: err != nil ⇒ the key admits nothing
func (t *Table) Reconcile(ctx context.Context, key string) error {
	s := t.lookup(key)
	if s == nil {
		return nil
	}
	if err := lock(ctx, s.life); err != nil {
		return errs.Wrap(module, err, "cannot reconcile %q: cancelled; stays closed, safe to retry", key)
	}
	defer unlock(s.life)
	l := s.view.Load().l
	if l.State != Closed {
		return nil
	}
	p, err := t.cfg.Placement.Get(ctx, key)
	found := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return errs.Wrap(module, fmt.Errorf("%w: %w", ErrUnavailable, err), "cannot reconcile %q: authority unreadable; stays closed, safe to retry", key)
	}
	ev, ok, err := reconcileEvent(l, p, found, t.cfg.Self)
	if err != nil {
		return errs.Wrap(module, err, "cannot reconcile %q; stays closed", key)
	}
	if !ok {
		return nil
	}
	if ev.Kind == EvReopen {
		// Re-stamp at our own epoch before admitting anything: the stamp is a new manifest version, so every
		// publish the crashed incarnation still has in flight (conditional on an older version) fails, and one
		// that landed already is the generation we start from. Without it such a publish could land after we
		// read the manifest, and our next publish would take it for another writer's (DST-1, zombie
		// publishes). Conditional and fenced like any stamp: its Head is also the defense-in-depth check, so a
		// higher stored epoch (a stale or broken authority read) supersedes the key instead.
		// The stamped generation (>= l.Gen, checked) is not recorded: l.Gen stays the floor CheckActivation
		// enforces, and the host activates from the manifest itself.
		_, err := t.stamp(ctx, key, Place{Owner: t.cfg.Self, Epoch: l.Epoch, Gen: l.Gen, State: PlaceActive})
		switch {
		case errors.Is(err, errStorageNewer):
			ev = Event{Kind: EvSupersede}
		case err != nil:
			return errs.Wrap(module, err, "cannot reconcile %q: the reopen stamp failed; stays closed, safe to retry", key)
		}
	}
	if ev.Kind == EvSupersede {
		if err := t.cfg.Store.Put(ctx, Record{Key: key, State: Fenced, Epoch: l.Epoch, Gen: l.Gen, Superseded: true}); err != nil {
			return errs.Wrap(module, err, "cannot reconcile %q: fence record not written; stays closed, safe to retry", key)
		}
	}
	_, err = s.apply(ev)
	assert.That(err == nil, "owner: reconcile event %s rejected on %s: %v", ev.Kind, l, err)
	return nil
}

// Forget drops a fenced key's durable record (after the control plane cleaned the source). Absent keys are
// fine; any other state is ErrIllegal.
func (t *Table) Forget(ctx context.Context, key string) error {
	s := t.lookup(key)
	if s == nil {
		return nil
	}
	if err := lock(ctx, s.life); err != nil {
		return errs.Wrap(module, err, "cannot forget %q: cancelled; nothing changed", key)
	}
	defer unlock(s.life)
	l := s.view.Load().l
	if _, err := Step(l, Event{Kind: EvForget}); err != nil {
		return errs.Wrap(module, err, "cannot forget %q in state %s; fence it first", key, l)
	}
	if err := t.cfg.Store.Delete(ctx, key); err != nil {
		return errs.Wrap(module, err, "cannot forget %q: record not deleted; still fenced, safe to retry", key)
	}
	_, err := s.apply(Event{Kind: EvForget})
	assert.That(err == nil, "owner: forget of %q failed after Step allowed it: %v", key, err)
	return nil
}

// CheckActivation is the guard the host calls before a database activation (vactor) fetches manifest m for
// key at epoch: the key must be Active here at epoch, storage must carry exactly this epoch (Claim stamped
// it) and must not be behind the generation this process knows. No I/O unless it finds a higher epoch, in
// which case it fences the key durably and returns ErrSuperseded.
func (t *Table) CheckActivation(ctx context.Context, key string, epoch Epoch, m Manifest) error {
	s := t.lookup(key)
	if s == nil {
		return errs.Wrap(module, ErrWrongOwner, "cannot activate %q: not held here", key)
	}
	l := s.view.Load().l
	if err := decide(l, epoch); err != nil {
		return errs.Wrap(module, err, "cannot activate %q at epoch %d: local state %s", key, epoch, l)
	}
	switch {
	case m.Epoch > epoch:
		return superseded(key, epoch, ErrStale, t.supersede(ctx, s, epoch))
	case m.Epoch < epoch:
		return errs.Wrap(module, ErrUnavailable, "cannot activate %q: manifest epoch %d lacks this owner's stamp %d (storage went back?); not activating", key, m.Epoch, epoch)
	case m.Gen < l.Gen:
		return errs.Wrap(module, ErrUnavailable, "cannot activate %q: manifest generation %d is behind the %d this owner published; not activating", key, m.Gen, l.Gen)
	}
	return nil
}
