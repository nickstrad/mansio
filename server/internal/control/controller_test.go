package control_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/control"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
)

var ctx = context.Background()

// registry is an in-memory control.Registry over the fake authority: it computes load and the MOVING scan
// from the authority's rows (no clock: every row counts as old enough).
type registry struct {
	mu   sync.Mutex
	pl   *ownertest.Placement
	dbs  map[string]control.Database
	vms  map[string]*control.VM
	keys []string // every key the authority may hold, for scans
	// dropped records DropPlacement calls (the fake authority cannot delete a row).
	dropped map[string]bool
}

func newRegistry(pl *ownertest.Placement, vms ...string) *registry {
	r := &registry{pl: pl, dbs: map[string]control.Database{}, vms: map[string]*control.VM{}, dropped: map[string]bool{}}
	for _, vm := range vms {
		r.vms[vm] = &control.VM{ID: vm}
	}
	return r
}

func (r *registry) row(key string) (owner.Place, bool) {
	if r.dropped[key] {
		return owner.Place{}, false
	}
	return r.pl.Row(key)
}

func (r *registry) CreateDatabase(_ context.Context, tenant, name string) (control.Database, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := tenant + ":" + name
	if d, ok := r.dbs[key]; ok {
		return d, nil
	}
	d := control.Database{Key: key, Tenant: tenant, Name: name, State: control.StateCreating, Default: true}
	for _, o := range r.dbs {
		if o.Tenant == tenant && o.Default && o.State != control.StateDropped {
			d.Default = false
		}
	}
	r.dbs[key] = d
	r.keys = append(r.keys, key)
	return d, nil
}

func (r *registry) GetDatabase(_ context.Context, key string) (control.Database, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.dbs[key]
	if !ok {
		return d, owner.ErrNotFound
	}
	return d, nil
}

func (r *registry) SetDatabaseState(_ context.Context, key, from, to, target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.dbs[key]
	if !ok || d.State != from {
		return owner.ErrConflict
	}
	if (target != "") != (to == control.StateMoving) {
		return errors.New("databases_target_iff_moving")
	}
	d.State, d.Target = to, target
	r.dbs[key] = d
	return nil
}

func (r *registry) DropPlacement(_ context.Context, key string, expect owner.Place) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.row(key); !ok || p != expect || p.State != owner.PlaceMoving {
		return owner.ErrConflict
	}
	r.dropped[key] = true
	return nil
}

func (r *registry) Load(_ context.Context, limit int) ([]control.VMLoad, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []control.VMLoad
	for _, vm := range r.vms {
		if vm.Dead {
			continue
		}
		l := control.VMLoad{VM: vm.ID}
		for _, k := range r.keys {
			if p, ok := r.row(k); ok && p.Owner == vm.ID && p.State == owner.PlaceActive {
				l.ActiveDBs++
			}
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VM < out[j].VM })
	return out[:min(limit, len(out))], nil
}

func (r *registry) VM(_ context.Context, vm string) (control.VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vms[vm]
	if !ok {
		return control.VM{}, fmt.Errorf("unknown vm %s", vm)
	}
	return *v, nil
}

func (r *registry) AttestDead(_ context.Context, vm, by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vms[vm].Dead = true
	return nil
}

func (r *registry) OwnedBy(_ context.Context, vm, after string, limit int) ([]control.Owned, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := slices.Sorted(slices.Values(r.keys))
	var out []control.Owned
	for _, k := range keys {
		if p, ok := r.row(k); ok && p.Owner == vm && k > after && len(out) < limit {
			out = append(out, control.Owned{Key: k, Place: p})
		}
	}
	return out, nil
}

func (r *registry) Moving(_ context.Context, _ time.Duration, limit int) ([]control.Stuck, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []control.Stuck
	for _, k := range r.keys {
		if p, ok := r.row(k); ok && p.State == owner.PlaceMoving && len(out) < limit {
			d := r.dbs[k]
			out = append(out, control.Stuck{Key: k, Place: p, Intent: d.State, Target: d.Target})
		}
	}
	return out, nil
}

func (r *registry) Pending(_ context.Context, _ time.Duration, limit int) ([]control.Database, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []control.Database
	for _, k := range r.keys {
		if d := r.dbs[k]; (d.State == control.StateCreating || d.State == control.StateMoving || d.State == control.StateDropping) && len(out) < limit {
			out = append(out, d)
		}
	}
	return out, nil
}

// objects records prefix deletes; fail makes the next one fail.
type objects struct {
	mu      sync.Mutex
	deleted []string
	fail    bool
}

func (o *objects) DeletePrefix(_ context.Context, key string) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail {
		o.fail = false
		return 0, errors.New("s3 down")
	}
	o.deleted = append(o.deleted, key)
	return 1, nil
}

// rig is a controller over two (or more) agents, each an owner.Table with its own disk, sharing one fake
// authority and one fake object store: the lifecycle endpoints without HTTP.
type rig struct {
	t     *testing.T
	pl    *ownertest.Placement
	objs  *ownertest.Objects
	reg   *registry
	obj   *objects
	tabs  map[string]*owner.Table
	wbs   map[string]*ownertest.WriteBack
	down  map[string]bool // agents that do not answer
	mu    sync.Mutex
	c     *control.Controller
	calls map[string]int // lifecycle calls per vm
}

func newRig(t *testing.T, vms ...string) *rig {
	r := &rig{t: t, pl: ownertest.NewPlacement(), objs: ownertest.NewObjects(), obj: &objects{},
		tabs: map[string]*owner.Table{}, wbs: map[string]*ownertest.WriteBack{}, down: map[string]bool{}, calls: map[string]int{}}
	r.reg = newRegistry(r.pl, vms...)
	for _, vm := range vms {
		wb := &ownertest.WriteBack{Objects: r.objs, Disk: ownertest.NewDisk()}
		tab, err := owner.Open(ctx, owner.Config{Self: vm, Store: ownertest.NewMemStore(), Placement: r.pl, WriteBack: wb})
		if err != nil {
			t.Fatal(err)
		}
		r.tabs[vm], r.wbs[vm] = tab, wb
	}
	r.c = &control.Controller{Registry: r.reg, Placement: r.pl, Objects: r.obj, Policy: control.Policy{MaxDatabases: 1000},
		Agents: func(vm string) (control.Agent, error) {
			tab, ok := r.tabs[vm]
			if !ok {
				return nil, errors.New("no such agent")
			}
			return agent{r, vm, tab}, nil
		}}
	return r
}

type agent struct {
	r   *rig
	vm  string
	tab *owner.Table
}

var errAgentDown = errors.New("agent down")

func (a agent) up() error {
	a.r.mu.Lock()
	defer a.r.mu.Unlock()
	a.r.calls[a.vm]++
	if a.r.down[a.vm] {
		return errAgentDown
	}
	return nil
}

func (a agent) Claim(ctx context.Context, key string) (owner.Local, error) {
	if err := a.up(); err != nil {
		return owner.Local{}, err
	}
	l, err := a.tab.Claim(ctx, key)
	if err == nil {
		a.r.wbs[a.vm].Fetch(key) // what the first request's activation does
	}
	return l, err
}

func (a agent) Fence(ctx context.Context, key string) (owner.Ack, error) {
	if err := a.up(); err != nil {
		return owner.Ack{}, err
	}
	return a.tab.Fence(ctx, key)
}

func (a agent) Forget(ctx context.Context, key string) error {
	if err := a.up(); err != nil {
		return err
	}
	return a.tab.Forget(ctx, key)
}

func (r *rig) setDown(vm string, v bool) { r.mu.Lock(); r.down[vm] = v; r.mu.Unlock() }

// write appends id to key on its current owner through admission.
func (r *rig) write(key, id string) {
	r.t.Helper()
	p, ok := r.pl.Row(key)
	if !ok {
		r.t.Fatalf("no row for %s", key)
	}
	err := r.tabs[p.Owner].Do(key, p.Epoch, func() error { r.wbs[p.Owner].Disk.Append(key, id); return nil })
	if err != nil {
		r.t.Fatalf("write %s to %s: %v", id, key, err)
	}
}

// published is what storage holds for key.
func (r *rig) published(key string) []string {
	_, _, _, contents, _ := r.objs.Read(key)
	return contents
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (r *rig) state(key string) string {
	d, err := r.reg.GetDatabase(ctx, key)
	must(r.t, err)
	return d.State
}

// Flow B: creates spread over the agents by the policy; each lands ACTIVE at epoch 1 and claimed; the
// tenant's first database is its default; a retried create is a no-op.
func TestCreateSpreadsAndClaims(t *testing.T) {
	r := newRig(t, "vm-a", "vm-b")
	per := map[string]int{}
	for i := range 10 {
		d, err := r.c.Create(ctx, "acme", fmt.Sprintf("db%d", i))
		must(t, err)
		if d.State != control.StateActive || d.Default != (i == 0) {
			t.Fatalf("created %+v", d)
		}
		p, _ := r.pl.Row(d.Key)
		if p.Epoch != 1 || p.State != owner.PlaceActive || r.tabs[p.Owner].State(d.Key).State != owner.Active {
			t.Fatalf("%s: row %+v, local %s", d.Key, p, r.tabs[p.Owner].State(d.Key))
		}
		per[p.Owner]++
	}
	if per["vm-a"] != 5 || per["vm-b"] != 5 {
		t.Fatalf("spread %v, want 5/5", per)
	}
	before := r.calls["vm-a"] + r.calls["vm-b"]
	if d, err := r.c.Create(ctx, "acme", "db0"); err != nil || d.State != control.StateActive || r.calls["vm-a"]+r.calls["vm-b"] != before {
		t.Fatalf("second create = %+v, %v (agent calls %d → %d)", d, err, before, r.calls["vm-a"]+r.calls["vm-b"])
	}
}

// A create whose claim fails stays "creating" with its row placed, and the resumer finishes it on the same
// VM (no second placement).
func TestCreateResumes(t *testing.T) {
	r := newRig(t, "vm-a")
	r.setDown("vm-a", true)
	if _, err := r.c.Create(ctx, "acme", "x"); !errors.Is(err, errAgentDown) {
		t.Fatalf("create with the agent down: %v", err)
	}
	if r.state("acme:x") != control.StateCreating {
		t.Fatal("not creating")
	}
	r.setDown("vm-a", false)
	rep, err := r.c.ResumeStuck(ctx, 0, 10, time.Second)
	if err != nil || rep.Resumed != 1 || len(rep.Failed) != 0 || r.state("acme:x") != control.StateActive {
		t.Fatalf("resume: %+v, %v, state %s", rep, err, r.state("acme:x"))
	}
	if p, _ := r.pl.Row("acme:x"); p != (owner.Place{Owner: "vm-a", Epoch: 1, State: owner.PlaceActive}) {
		t.Fatalf("row %+v", p)
	}
}

// Flow C: a move keeps every acknowledged write; the source answers fenced, the destination owns the key at
// epoch 2. A move whose destination is down stays "moving" and the resumer finishes it.
func TestMoveKeepsWritesAndResumes(t *testing.T) {
	r := newRig(t, "vm-a", "vm-b")
	d, err := r.c.Create(ctx, "acme", "x")
	must(t, err)
	src, _ := r.pl.Row(d.Key)
	dst := map[string]string{"vm-a": "vm-b", "vm-b": "vm-a"}[src.Owner]
	r.write(d.Key, "w1")
	r.write(d.Key, "w2")

	r.setDown(dst, true)
	if _, err := r.c.Move(ctx, d.Key, dst); err == nil {
		t.Fatal("move to a down agent succeeded")
	}
	if r.state(d.Key) != control.StateMoving {
		t.Fatalf("state %s, want moving", r.state(d.Key))
	}
	if _, err := r.c.Move(ctx, d.Key, src.Owner); !errors.Is(err, control.ErrState) {
		t.Fatalf("re-targeting a move in progress: %v", err)
	}
	r.setDown(dst, false)
	rep, err := r.c.ResumeStuck(ctx, 0, 10, time.Second)
	if err != nil || rep.Resumed != 1 {
		t.Fatalf("resume: %+v %v", rep, err)
	}
	p, _ := r.pl.Row(d.Key)
	if p.Owner != dst || p.Epoch != 2 || p.State != owner.PlaceActive || r.state(d.Key) != control.StateActive {
		t.Fatalf("after move: row %+v, state %s", p, r.state(d.Key))
	}
	if got := r.published(d.Key); !slices.Equal(got, []string{"w1", "w2"}) {
		t.Fatalf("published %v", got)
	}
	if err := r.tabs[src.Owner].Do(d.Key, 1, func() error { return nil }); !errors.Is(err, owner.ErrFenced) {
		t.Fatalf("old owner admits: %v", err)
	}
	r.write(d.Key, "w3") // the new owner serves
	// Moving to the policy's pick ("" ) goes to the other VM.
	if d, err = r.c.Move(ctx, d.Key, ""); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.pl.Row(d.Key); p.Owner != src.Owner || p.Epoch != 3 || !slices.Equal(r.published(d.Key), []string{"w1", "w2", "w3"}) {
		t.Fatalf("second move: %+v %v", p, r.published(d.Key))
	}
}

// Suspend fences and leaves the row MOVING (the resumer skips it); resume places it again at a new epoch.
func TestSuspendResume(t *testing.T) {
	r := newRig(t, "vm-a")
	d, err := r.c.Create(ctx, "acme", "x")
	must(t, err)
	r.write(d.Key, "w1")
	if d, err = r.c.Suspend(ctx, d.Key); err != nil || d.State != control.StateSuspended {
		t.Fatalf("suspend: %+v %v", d, err)
	}
	if _, err := r.c.Suspend(ctx, d.Key); err != nil {
		t.Fatalf("second suspend: %v", err)
	}
	if p, _ := r.pl.Row(d.Key); p.State != owner.PlaceMoving || !slices.Equal(r.published(d.Key), []string{"w1"}) {
		t.Fatalf("suspended row %+v, published %v", p, r.published(d.Key))
	}
	if rep, _ := r.c.ResumeStuck(ctx, 0, 10, time.Second); rep.Skipped != 1 || rep.Resumed != 0 {
		t.Fatalf("resumer touched a suspended database: %+v", rep)
	}
	if _, err := r.c.Move(ctx, d.Key, "vm-a"); !errors.Is(err, control.ErrState) {
		t.Fatalf("move of a suspended database: %v", err)
	}
	if d, err = r.c.Resume(ctx, d.Key, ""); err != nil || d.State != control.StateActive {
		t.Fatalf("resume: %+v %v", d, err)
	}
	if p, _ := r.pl.Row(d.Key); p != (owner.Place{Owner: "vm-a", Epoch: 2, Gen: p.Gen, State: owner.PlaceActive}) {
		t.Fatalf("resumed row %+v", p)
	}
	r.write(d.Key, "w2")
}

// Drop: tombstone first; the owner is fenced and forgets the key; the prefix is deleted; the placement row
// goes; the name is never reused. A failed prefix delete leaves "dropping" for the resumer.
func TestDrop(t *testing.T) {
	r := newRig(t, "vm-a")
	d, err := r.c.Create(ctx, "acme", "x")
	must(t, err)
	r.write(d.Key, "w1")
	r.obj.fail = true
	if _, err := r.c.Drop(ctx, d.Key); err == nil {
		t.Fatal("drop with S3 down succeeded")
	}
	if r.state(d.Key) != control.StateDropping {
		t.Fatalf("state %s", r.state(d.Key))
	}
	if err := r.tabs["vm-a"].Do(d.Key, 1, func() error { return nil }); err == nil {
		t.Fatal("a dropping database still admits")
	}
	rep, err := r.c.ResumeStuck(ctx, 0, 10, time.Second)
	if err != nil || rep.Resumed != 1 || r.state(d.Key) != control.StateDropped {
		t.Fatalf("resume drop: %+v %v %s", rep, err, r.state(d.Key))
	}
	if !slices.Equal(r.obj.deleted, []string{d.Key}) || r.tabs["vm-a"].State(d.Key).State != owner.Absent || !r.reg.dropped[d.Key] {
		t.Fatalf("deleted %v, local %s, placement dropped %v", r.obj.deleted, r.tabs["vm-a"].State(d.Key), r.reg.dropped[d.Key])
	}
	if _, err := r.c.Drop(ctx, d.Key); err != nil {
		t.Fatalf("drop again: %v", err)
	}
	if _, err := r.c.Create(ctx, "acme", "x"); !errors.Is(err, control.ErrExists) {
		t.Fatalf("re-create a dropped name: %v", err)
	}
	// A database never placed (create stopped before the CAS) drops without an agent.
	r.reg.CreateDatabase(ctx, "acme", "y")
	if d, err := r.c.Drop(ctx, "acme:y"); err != nil || d.State != control.StateDropped {
		t.Fatalf("drop unplaced: %+v %v", d, err)
	}
}

// M5: a dead VM's databases move only after an operator's attestation, to live VMs, from the last published
// generation, at a higher epoch; the dead agent is never called.
func TestRestoreAfterAttestation(t *testing.T) {
	r := newRig(t, "vm-a", "vm-b", "vm-c")
	var onB []string
	for i := range 9 {
		d, err := r.c.Create(ctx, "acme", fmt.Sprintf("db%d", i))
		must(t, err)
		r.write(d.Key, "w-"+d.Key)
		if p, _ := r.pl.Row(d.Key); p.Owner == "vm-b" {
			onB = append(onB, d.Key)
			_, err := r.tabs["vm-b"].Publish(ctx, d.Key, p.Epoch, func(ctx context.Context) (owner.Generation, error) {
				return r.wbs["vm-b"].Publish(ctx, d.Key, p.Epoch)
			})
			must(t, err)
			r.write(d.Key, "lost-"+d.Key) // never published: inside the RPO
		}
	}
	if len(onB) != 3 {
		t.Fatalf("vm-b holds %v", onB)
	}
	r.setDown("vm-b", true)
	callsB := r.calls["vm-b"]
	if _, err := r.c.Restore(ctx, "vm-b", time.Second); !errors.Is(err, control.ErrNotDead) {
		t.Fatalf("restore without attestation: %v", err)
	}
	must(t, r.c.AttestDead(ctx, "vm-b", "alice"))
	rep, err := r.c.Restore(ctx, "vm-b", time.Second)
	if err != nil || len(rep.Moved) != 3 || len(rep.Failed) != 0 {
		t.Fatalf("restore: %+v %v", rep, err)
	}
	for _, k := range onB {
		p, _ := r.pl.Row(k)
		if p.Owner == "vm-b" || p.Epoch != 2 || p.State != owner.PlaceActive || r.state(k) != control.StateActive {
			t.Fatalf("%s after restore: %+v %s", k, p, r.state(k))
		}
		if got := r.published(k); !slices.Equal(got, []string{"w-" + k}) {
			t.Fatalf("%s restored %v", k, got)
		}
		r.write(k, "after")
	}
	if r.calls["vm-b"] != callsB {
		t.Fatalf("the dead agent was called %d times", r.calls["vm-b"]-callsB)
	}
	// Nothing lands on the dead VM any more.
	for i := range 4 {
		d, err := r.c.Create(ctx, "acme", fmt.Sprintf("new%d", i))
		must(t, err)
		if p, _ := r.pl.Row(d.Key); p.Owner == "vm-b" {
			t.Fatal("placed on a dead vm")
		}
	}
}
