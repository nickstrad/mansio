package main

import (
	"context"
	"time"

	"github.com/nickstrad/mansio/server/internal/control"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
)

// registry adapts the Postgres adapter to control.Registry (type translation only).
type registry struct{ p *pg.Placement }

var _ control.Registry = registry{}

func toDatabase(d pg.Database) control.Database {
	return control.Database{Key: d.Key, Tenant: d.Tenant, Name: d.Name, Default: d.Default, State: d.State, Target: d.Target}
}

func (r registry) CreateDatabase(ctx context.Context, tenant, name string) (control.Database, error) {
	d, err := r.p.CreateDatabase(ctx, tenant, name)
	return toDatabase(d), err
}

func (r registry) GetDatabase(ctx context.Context, key string) (control.Database, error) {
	d, err := r.p.GetDatabase(ctx, key)
	return toDatabase(d), err
}

func (r registry) SetDatabaseState(ctx context.Context, key, from, to, target string) error {
	return r.p.SetDatabaseState(ctx, key, from, to, target)
}

func (r registry) DropPlacement(ctx context.Context, key string, expect owner.Place) error {
	return r.p.DropPlacement(ctx, key, expect)
}

func (r registry) Load(ctx context.Context, limit int) ([]control.VMLoad, error) {
	ls, err := r.p.Load(ctx, limit)
	out := make([]control.VMLoad, len(ls))
	for i, l := range ls {
		out[i] = control.VMLoad{VM: l.VM, ActiveDBs: l.ActiveDBs, Bytes: l.Bytes, HeartbeatAge: time.Duration(l.HeartbeatAge * float64(time.Second))}
	}
	return out, err
}

func (r registry) VM(ctx context.Context, vm string) (control.VM, error) {
	v, err := r.p.VM(ctx, vm)
	return control.VM{ID: v.ID, Dead: v.Dead}, err
}

func (r registry) AttestDead(ctx context.Context, vm, by string) error {
	return r.p.AttestDead(ctx, vm, by)
}

func (r registry) OwnedBy(ctx context.Context, vm, after string, limit int) ([]control.Owned, error) {
	os, err := r.p.OwnedBy(ctx, vm, after, limit)
	out := make([]control.Owned, len(os))
	for i, o := range os {
		out[i] = control.Owned{Key: o.Key, Place: o.Place}
	}
	return out, err
}

func (r registry) Moving(ctx context.Context, olderThan time.Duration, limit int) ([]control.Stuck, error) {
	ss, err := r.p.Moving(ctx, olderThan, limit)
	out := make([]control.Stuck, len(ss))
	for i, s := range ss {
		out[i] = control.Stuck{Key: s.Key, Place: owner.Place{Owner: s.Owner, Epoch: s.Epoch, Gen: s.Gen, State: owner.PlaceMoving},
			Intent: s.Intent, Target: s.Target}
	}
	return out, err
}

func (r registry) Pending(ctx context.Context, olderThan time.Duration, limit int) ([]control.Database, error) {
	ds, err := r.p.Pending(ctx, olderThan, limit)
	out := make([]control.Database, len(ds))
	for i, d := range ds {
		out[i] = toDatabase(d)
	}
	return out, err
}

// objects deletes a database's prefix: root + durable.KeyPrefix(key), which ends in "/", so it never
// reaches another key's objects: no database's prefix contains another's.
type objects struct {
	s    *objstore.Store
	root string
}

func (o objects) DeletePrefix(ctx context.Context, key string) (int, error) {
	return o.s.DeletePrefix(ctx, o.root+durable.KeyPrefix(key))
}
