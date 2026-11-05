package owner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/owner/ownertest"
)

// world is N simulated processes sharing one fake authority and one fake object store. Each process has its
// own disk (MemStore for ownership, Disk for database copies) that survives a crash.
type world struct {
	t     testing.TB
	pl    *ownertest.Placement
	objs  *ownertest.Objects
	procs map[string]*proc
}

type proc struct {
	name   string
	store  *ownertest.MemStore
	disk   *ownertest.Disk
	wb     *ownertest.WriteBack
	tab    *owner.Table // nil while crashed
	pl     *partitioned
	cfgMod func(*owner.Config)
}

// partitioned is a process's view of the authority; Cut makes it unreachable from this process only.
type partitioned struct {
	owner.Placement
	cut bool
}

func (p *partitioned) Get(ctx context.Context, key string) (owner.Place, error) {
	if p.cut {
		return owner.Place{}, ownertest.ErrDown
	}
	return p.Placement.Get(ctx, key)
}

func (p *partitioned) CAS(ctx context.Context, key string, expect, next owner.Place) error {
	if p.cut {
		return ownertest.ErrDown
	}
	return p.Placement.CAS(ctx, key, expect, next)
}

func newWorld(t testing.TB, names ...string) *world {
	w := &world{t: t, pl: ownertest.NewPlacement(), objs: ownertest.NewObjects(), procs: map[string]*proc{}}
	for _, n := range names {
		disk := ownertest.NewDisk()
		p := &proc{name: n, store: ownertest.NewMemStore(), disk: disk,
			wb: &ownertest.WriteBack{Objects: w.objs, Disk: disk}, pl: &partitioned{Placement: w.pl}}
		w.procs[n] = p
		w.start(n)
	}
	return w
}

func (w *world) start(name string) *owner.Table {
	p := w.procs[name]
	cfg := owner.Config{Self: name, Store: p.store, Placement: p.pl, WriteBack: p.wb}
	if p.cfgMod != nil {
		p.cfgMod(&cfg)
	}
	tab, err := owner.Open(context.Background(), cfg)
	if err != nil {
		w.t.Fatalf("Open(%s): %v", name, err)
	}
	p.tab = tab
	return tab
}

func (w *world) tab(name string) *owner.Table { return w.procs[name].tab }

// place creates key on owner at epoch 1 (what the control plane's create-new does) and claims it there.
func (w *world) place(key, ownerName string) {
	w.t.Helper()
	w.pl.Create(key, owner.Place{Owner: ownerName, Epoch: 1, State: owner.PlaceActive})
	if _, err := w.tab(ownerName).Claim(context.Background(), key); err != nil {
		w.t.Fatalf("Claim(%s on %s): %v", key, ownerName, err)
	}
	w.procs[ownerName].wb.Fetch(key)
}

// write is one client write through admission: append id to the process's local copy.
func (w *world) write(name, key string, epoch owner.Epoch, id string) error {
	p := w.procs[name]
	if p.tab == nil {
		return ownertest.ErrDown
	}
	return p.tab.Do(key, epoch, func() error { p.disk.Append(key, id); return nil })
}

func mustIs(t testing.TB, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func contextCanceled() (context.Context, context.CancelFunc) {
	c, cancel := context.WithCancel(context.Background())
	cancel()
	return c, cancel
}
