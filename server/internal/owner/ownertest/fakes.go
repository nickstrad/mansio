// Package ownertest holds fakes for the owner package's ports, with fault switches, and the LocalStore
// contract suite. All fakes are safe for concurrent use.
//
//   - MemStore: LocalStore in memory (the "disk" survives a simulated process restart).
//   - Placement: the authority, with Down, Delay, Stale and LoseReply switches.
//   - Objects: object storage implementing contract 1 exactly (conditional manifest publish, ErrStale).
//   - Disk + WriteBack: one process's local database copies and its write-back into Objects.
package ownertest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/owner"
)

// ErrDown is what a fake returns while its Down switch is on.
var ErrDown = errors.New("ownertest: down")

// MemStore is an in-memory LocalStore. FailPut, FailDelete and FailLoad make the next calls fail with ErrDown
// (they stay on until cleared); Puts counts successful Puts.
type MemStore struct {
	mu         sync.Mutex
	recs       map[string]owner.Record
	FailPut    bool
	FailDelete bool
	FailLoad   bool
	Puts       int
}

func NewMemStore() *MemStore { return &MemStore{recs: map[string]owner.Record{}} }

func (m *MemStore) Put(ctx context.Context, r owner.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailPut {
		return ErrDown
	}
	if err := r.Validate(); err != nil {
		return err
	}
	m.recs[r.Key] = r
	m.Puts++
	return nil
}

func (m *MemStore) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailDelete {
		return ErrDown
	}
	delete(m.recs, key)
	return nil
}

func (m *MemStore) Load(ctx context.Context) ([]owner.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailLoad {
		return nil, ErrDown
	}
	out := make([]owner.Record, 0, len(m.recs))
	for _, r := range m.recs {
		out = append(out, r)
	}
	return out, nil
}

// Set switches a fault on or off under the lock (tests running goroutines must not write fields directly).
func (m *MemStore) Set(f func(m *MemStore)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m)
}

// Get returns key's stored record.
func (m *MemStore) Get(key string) (owner.Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[key]
	return r, ok
}

// Placement is the fake authority. It enforces the authority's invariants on CAS (epochs never decrease; a
// new owner only with a higher epoch) and returns an error for a CAS that breaks them.
//
// Switches: Down (every call fails with ErrDown), Delay (every call waits, honoring ctx), KeyDelay (calls for
// one key wait, honoring ctx: one slow row), Stale (Get returns
// the row as it was when Stale was switched on), LoseReply (the next CAS is applied but reports ErrDown, like
// a lost response).
type Placement struct {
	mu        sync.Mutex
	rows      map[string]owner.Place
	frozen    map[string]owner.Place // snapshot served while Stale
	down      bool
	stale     bool
	delay     time.Duration
	keyDelay  map[string]time.Duration
	loseReply bool
	clk       det.Clock // Delay and KeyDelay wait on it
	CASes     int
}

func NewPlacement() *Placement { return &Placement{rows: map[string]owner.Place{}, clk: det.System()} }

// UseClock makes Delay and KeyDelay wait on c (default real time): a simulation's stalls end in simulated time.
func (p *Placement) UseClock(c det.Clock) { p.mu.Lock(); p.clk = det.Or(c); p.mu.Unlock() }

// Create sets a row directly (test setup).
func (p *Placement) Create(key string, pl owner.Place) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows[key] = pl
}

// Row returns the current row, ignoring every switch.
func (p *Placement) Row(key string) (owner.Place, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.rows[key]
	return r, ok
}

func (p *Placement) SetDown(v bool) { p.mu.Lock(); p.down = v; p.mu.Unlock() }
func (p *Placement) SetDelay(d time.Duration) {
	p.mu.Lock()
	p.delay = d
	p.mu.Unlock()
}

// SetKeyDelay makes every call for key wait d more (0 removes it).
func (p *Placement) SetKeyDelay(key string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.keyDelay == nil {
		p.keyDelay = map[string]time.Duration{}
	}
	p.keyDelay[key] = d
}

func (p *Placement) SetLoseReply(v bool) { p.mu.Lock(); p.loseReply = v; p.mu.Unlock() }

// SetStale freezes (true) or unfreezes (false) what Get returns.
func (p *Placement) SetStale(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stale = v
	if v {
		p.frozen = maps.Clone(p.rows)
	}
}

func (p *Placement) wait(ctx context.Context, key string) error {
	p.mu.Lock()
	d, down, clk := p.delay+p.keyDelay[key], p.down, p.clk
	p.mu.Unlock()
	if d > 0 {
		if err := det.Sleep(ctx, clk, d); err != nil {
			return err
		}
	}
	if down {
		return ErrDown
	}
	return nil
}

func (p *Placement) Get(ctx context.Context, key string) (owner.Place, error) {
	if err := p.wait(ctx, key); err != nil {
		return owner.Place{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	rows := p.rows
	if p.stale {
		rows = p.frozen
	}
	r, ok := rows[key]
	if !ok {
		return owner.Place{}, owner.ErrNotFound
	}
	return r, nil
}

func (p *Placement) CAS(ctx context.Context, key string, expect, next owner.Place) error {
	if err := p.wait(ctx, key); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.rows[key]
	if !ok {
		cur = owner.Place{}
	}
	if cur != expect {
		return fmt.Errorf("%w: have %+v, expected %+v", owner.ErrConflict, cur, expect)
	}
	if next.Epoch < cur.Epoch || (next.Owner != cur.Owner && cur.Epoch != 0 && next.Epoch <= cur.Epoch) || next.Epoch == 0 {
		return fmt.Errorf("ownertest: CAS breaks the authority's invariants: %+v → %+v", cur, next)
	}
	p.rows[key] = next
	p.CASes++
	if p.loseReply {
		p.loseReply = false
		return ErrDown
	}
	return nil
}

// manifest is one stored manifest (contract 1's generation and epoch) plus a version that stands in for the
// S3 ETag. Generation contents (lists of write ids) live in Objects.gens.
type manifest struct {
	gen     owner.Generation
	epoch   owner.Epoch
	version uint64 // bumps on every successful publish or stamp: "changed since read"
}

// Objects is object storage implementing contract 1 exactly: immutable generations, a manifest per key, and
// conditional publish that fails with owner.ErrStale if the manifest changed since it was read or its epoch is
// greater than the caller's. Down fails every call with ErrDown.
type Objects struct {
	mu        sync.Mutex
	manifests map[string]manifest
	gens      map[string]map[owner.Generation][]string
	history   map[string][][]string // contents of every successful publish, in order (for invariants)
	down      bool
	lostReply bool // the next successful Publish reports ErrStale (applied, then the reply was lost)
	Publishes int
	Stale     int // publishes and stamps refused with ErrStale
}

func NewObjects() *Objects {
	return &Objects{manifests: map[string]manifest{}, gens: map[string]map[owner.Generation][]string{}, history: map[string][][]string{}}
}

func (o *Objects) SetDown(v bool) { o.mu.Lock(); o.down = v; o.mu.Unlock() }

// LoseNextReply makes the next successful Publish apply and then report ErrStale: what a re-send of a
// conditional PUT whose reply was lost gets (object-storage plan G2, OS-02).
func (o *Objects) LoseNextReply() { o.mu.Lock(); o.lostReply = true; o.mu.Unlock() }

// PublishCount is the number of successful publishes that changed a generation.
func (o *Objects) PublishCount() int { o.mu.Lock(); defer o.mu.Unlock(); return o.Publishes }

// Read returns the manifest's (gen, epoch, version) and the contents of its generation.
func (o *Objects) Read(key string) (gen owner.Generation, epoch owner.Epoch, version uint64, contents []string, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	m, ok := o.manifests[key]
	if !ok {
		return 0, 0, 0, nil, false
	}
	return m.gen, m.epoch, m.version, slices.Clone(o.gens[key][m.gen]), true
}

// History returns the contents of every successful publish for key, oldest first.
func (o *Objects) History(key string) [][]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.history[key])
}

// Upload stores contents as a new immutable generation and returns its number (not yet published).
func (o *Objects) Upload(key string, contents []string) (owner.Generation, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.down {
		return 0, ErrDown
	}
	if o.gens[key] == nil {
		o.gens[key] = map[owner.Generation][]string{0: nil}
	}
	g := owner.Generation(len(o.gens[key]))
	o.gens[key][g] = slices.Clone(contents)
	return g, nil
}

// Publish sets key's manifest to (gen, epoch) iff the stored manifest's version is still readVersion (0 = no
// manifest) and its epoch ≤ epoch. Contract 1.
func (o *Objects) Publish(key string, readVersion uint64, gen owner.Generation, epoch owner.Epoch) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.down {
		return ErrDown
	}
	m := o.manifests[key]
	if m.version != readVersion || m.epoch > epoch {
		o.Stale++
		return fmt.Errorf("%w: manifest %+v, caller read v%d at epoch %d", owner.ErrStale, m, readVersion, epoch)
	}
	if _, ok := o.gens[key][gen]; !ok && gen != 0 {
		return fmt.Errorf("ownertest: generation %d of %q was never uploaded", gen, key)
	}
	o.manifests[key] = manifest{gen: gen, epoch: epoch, version: m.version + 1}
	if gen != m.gen || m.version == 0 {
		o.history[key] = append(o.history[key], slices.Clone(o.gens[key][gen]))
		o.Publishes++
	}
	if o.lostReply {
		o.lostReply = false
		return fmt.Errorf("%w: reply lost, the re-send found the manifest moved", owner.ErrStale)
	}
	return nil
}

// publishResolved is Publish followed, on failure, by the re-read a real WriteBack does (guide R3): if the
// stored manifest is the one we asked for, the publish applied.
func (o *Objects) publishResolved(key string, readVersion uint64, gen owner.Generation, epoch owner.Epoch) error {
	err := o.Publish(key, readVersion, gen, epoch)
	if err == nil {
		return nil
	}
	if g, e, v, _, ok := o.Read(key); ok && g == gen && e == epoch && v == readVersion+1 {
		return nil
	}
	return err
}

// Disk is one process's local database copies: key → contents (write ids). It survives a process crash
// (it is the disk).
type Disk struct {
	mu  sync.Mutex
	dbs map[string][]string
}

func NewDisk() *Disk { return &Disk{dbs: map[string][]string{}} }

func (d *Disk) Append(key, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dbs[key] = append(d.dbs[key], id)
}

func (d *Disk) Contents(key string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.dbs[key])
}

func (d *Disk) Set(key string, contents []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dbs[key] = slices.Clone(contents)
}

// WriteBack is one process's owner.WriteBack: it publishes its Disk's copy into Objects under contract 1.
// FailPublish makes Publish fail with ErrDown (nothing changes); OnPublish, if set, runs at the start of every
// Publish (tests use it to check that no permit is outstanding during the final write-back).
type WriteBack struct {
	Objects *Objects
	Disk    *Disk

	mu          sync.Mutex
	FailPublish bool
	FailHead    bool
	OnPublish   func(key string, epoch owner.Epoch)
	Calls       int
}

func (w *WriteBack) Set(f func(w *WriteBack)) { w.mu.Lock(); f(w); w.mu.Unlock() }

func (w *WriteBack) Publish(ctx context.Context, key string, epoch owner.Epoch) (owner.Generation, error) {
	w.mu.Lock()
	fail, hook := w.FailPublish, w.OnPublish
	w.Calls++
	w.mu.Unlock()
	if hook != nil {
		hook(key, epoch)
	}
	if fail {
		return 0, ErrDown
	}
	_, _, v, _, _ := w.Objects.Read(key)
	g, err := w.Objects.Upload(key, w.Disk.Contents(key))
	if err != nil {
		return 0, err
	}
	if err := w.Objects.publishResolved(key, v, g, epoch); err != nil {
		return 0, err
	}
	return g, nil
}

func (w *WriteBack) Head(ctx context.Context, key string) (owner.Manifest, error) {
	w.mu.Lock()
	fail := w.FailHead
	w.mu.Unlock()
	if fail {
		return owner.Manifest{}, ErrDown
	}
	w.Objects.mu.Lock()
	down := w.Objects.down
	w.Objects.mu.Unlock()
	if down {
		return owner.Manifest{}, ErrDown
	}
	g, e, _, _, ok := w.Objects.Read(key)
	if !ok {
		return owner.Manifest{}, owner.ErrNoManifest
	}
	return owner.Manifest{Gen: g, Epoch: e}, nil
}

func (w *WriteBack) Stamp(ctx context.Context, key string, epoch owner.Epoch, gen owner.Generation) error {
	g, _, v, _, ok := w.Objects.Read(key)
	if (ok && g != gen) || (!ok && gen != 0) {
		return fmt.Errorf("%w: stored generation %d, caller expected %d", owner.ErrStale, g, gen)
	}
	return w.Objects.publishResolved(key, v, gen, epoch)
}

// Fetch copies key's current published generation into the Disk (what a host's activation does) and
// returns its manifest.
func (w *WriteBack) Fetch(key string) owner.Manifest {
	g, e, _, contents, _ := w.Objects.Read(key)
	w.Disk.Set(key, contents)
	return owner.Manifest{Gen: g, Epoch: e}
}
