package sim

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/control"
	"github.com/nickstrad/mansio/server/internal/control/locator"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/owner"
)

// controld in the simulation: the real control.Controller, locator.Locator and ticket.Signer (rotated by
// ticket.Rotate), over fakes for what controld keeps in Postgres (the registry below; the placement authority
// is ownertest.Placement, a linearizable CAS). Calls to Postgres fail while it is down or partitioned from the
// caller; a placement or registry change notifies the locator (the placement listener) after a small delay,
// and a notification may be lost (the cache then serves a stale row until wrong_owner reroutes past it).

var errPGDown = errors.New("sim: the placement authority (Postgres) is unreachable")

// pgUp reports whether node can reach Postgres now.
func (w *cworld) pgUp(node string) bool { return w.pgDown.Load() == 0 && !w.net.blocked(node, nodePG) }

// authority is one node's view of the placement authority.
type authority struct {
	w    *cworld
	from string
}

var _ owner.Placement = authority{}

func (a authority) Get(ctx context.Context, key string) (owner.Place, error) {
	if !a.w.pgUp(a.from) {
		return owner.Place{}, errPGDown
	}
	return a.w.place.Get(ctx, key)
}

func (a authority) CAS(ctx context.Context, key string, expect, next owner.Place) error {
	if !a.w.pgUp(a.from) {
		return errPGDown
	}
	err := a.w.place.CAS(ctx, key, expect, next)
	switch {
	case err == nil:
		a.w.tr.add("pg/"+key, "%s %s -> %s", a.from, placeString(expect), placeString(next))
		a.w.notify(key, fmt.Sprintf("%s/%+v", key, next), func() { a.w.loc.Invalidate(key, uint64(next.Epoch)) })
	case strings.Contains(err.Error(), "breaks the authority's invariants"):
		a.w.model.violateNow(key, "%s asked the authority for an illegal transition %s -> %s", a.from, placeString(expect), placeString(next))
	}
	return err
}

func placeString(p owner.Place) string {
	if p.Epoch == 0 {
		return "none"
	}
	return fmt.Sprintf("%s(%s,e%d,g%d)", p.State, p.Owner, p.Epoch, p.Gen)
}

// notify delivers a change notification to the locator after 1–50 ms, or loses it (Faults.Notify). The draw
// comes from the change itself (what), so it does not depend on which goroutine made it.
func (w *cworld) notify(key, what string, fn func()) {
	h := sha256.Sum256([]byte(what))
	r := stream(w.s.Seed^uint64(h[0])<<56^uint64(h[1])<<48^uint64(h[2])<<40^uint64(h[3])<<32, "notify")
	d, lost := time.Millisecond+time.Duration(r.Int64N(int64(49*time.Millisecond))), r.Float64() < w.s.faults.Notify
	if lost && w.faultsOn.Load() || !w.pgUp(nodeControld) {
		w.tr.add("pg/"+key, "notification lost")
		return
	}
	w.clk.AfterFunc(d, fn)
}

// registry is an in-memory control.Registry over the authority (as control's controller_test fake): load and
// the scans are computed from the authority's rows; no ages (every row counts as old enough).
type registry struct {
	w    *cworld
	mu   sync.Mutex
	dbs  map[string]control.Database
	vms  map[string]*control.VM
	keys []string
}

var _ control.Registry = (*registry)(nil)

func newRegistry(w *cworld) *registry {
	r := &registry{w: w, dbs: map[string]control.Database{}, vms: map[string]*control.VM{}}
	for _, vm := range w.vms {
		r.vms[vm] = &control.VM{ID: vm}
	}
	return r
}

func (r *registry) up() error {
	if !r.w.pgUp(nodeControld) {
		return errPGDown
	}
	return nil
}

func (r *registry) CreateDatabase(_ context.Context, tenant, name string) (control.Database, error) {
	if err := r.up(); err != nil {
		return control.Database{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := tenant + ":" + name
	if d, ok := r.dbs[key]; ok {
		return d, nil
	}
	d := control.Database{Key: key, Tenant: tenant, Name: name, State: control.StateCreating}
	r.dbs[key] = d
	r.keys = append(r.keys, key)
	return d, nil
}

func (r *registry) GetDatabase(_ context.Context, key string) (control.Database, error) {
	if err := r.up(); err != nil {
		return control.Database{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.dbs[key]
	if !ok {
		return d, owner.ErrNotFound
	}
	return d, nil
}

func (r *registry) SetDatabaseState(_ context.Context, key, from, to, target string) error {
	if err := r.up(); err != nil {
		return err
	}
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
	r.w.tr.add("pg/"+key, "registry %s -> %s %s", from, to, target)
	r.w.notify(key, fmt.Sprintf("%s/%s/%s/%s", key, from, to, target), func() { r.w.loc.InvalidateState(key) })
	return nil
}

func (r *registry) DropPlacement(context.Context, string, owner.Place) error {
	return errors.New("sim: drops are not simulated")
}

func (r *registry) Load(_ context.Context, limit int) ([]control.VMLoad, error) {
	if err := r.up(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []control.VMLoad
	for _, vm := range slices.Sorted(func(yield func(string) bool) {
		for k := range r.vms {
			if !yield(k) {
				return
			}
		}
	}) {
		if r.vms[vm].Dead {
			continue
		}
		l := control.VMLoad{VM: vm, HeartbeatAge: r.w.heartbeatAge(vm)}
		for _, k := range r.keys {
			if p, ok := r.w.place.Row(k); ok && p.Owner == vm && p.State == owner.PlaceActive {
				l.ActiveDBs++
			}
		}
		out = append(out, l)
	}
	return out[:min(limit, len(out))], nil
}

func (r *registry) VM(_ context.Context, vm string) (control.VM, error) {
	if err := r.up(); err != nil {
		return control.VM{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vms[vm]
	if !ok {
		return control.VM{}, fmt.Errorf("sim: unknown vm %s", vm)
	}
	return *v, nil
}

func (r *registry) AttestDead(_ context.Context, vm, _ string) error {
	if err := r.up(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vms[vm].Dead = true
	return nil
}

func (r *registry) OwnedBy(_ context.Context, vm, after string, limit int) ([]control.Owned, error) {
	if err := r.up(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []control.Owned
	for _, k := range slices.Sorted(slices.Values(r.keys)) {
		if p, ok := r.w.place.Row(k); ok && p.Owner == vm && k > after && len(out) < limit {
			out = append(out, control.Owned{Key: k, Place: p})
		}
	}
	return out, nil
}

func (r *registry) Moving(_ context.Context, _ time.Duration, limit int) ([]control.Stuck, error) {
	if err := r.up(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []control.Stuck
	for _, k := range r.keys {
		if p, ok := r.w.place.Row(k); ok && p.State == owner.PlaceMoving && len(out) < limit {
			d := r.dbs[k]
			out = append(out, control.Stuck{Key: k, Place: p, Intent: d.State, Target: d.Target})
		}
	}
	return out, nil
}

func (r *registry) Pending(_ context.Context, _ time.Duration, limit int) ([]control.Database, error) {
	if err := r.up(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []control.Database
	for _, k := range r.keys {
		if d := r.dbs[k]; (d.State == control.StateCreating || d.State == control.StateMoving) && len(out) < limit {
			out = append(out, d)
		}
	}
	return out, nil
}

// state is key's registry state ("" when unknown), for the locator's store.
func (r *registry) state(key string) (control.Database, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.dbs[key]
	return d, ok
}

// locStore is the locator's store: placement rows and registry state from the fakes, one API key.
type locStore struct{ w *cworld }

var _ locator.Store = locStore{}

func (s locStore) Locate(_ context.Context, key string, _ bool) (locator.Placement, error) {
	if !s.w.pgUp(nodeControld) {
		return locator.Placement{}, errPGDown
	}
	var pl locator.Placement
	if d, ok := s.w.reg.state(key); ok {
		pl.Intent, pl.Target = d.State, d.Target
	}
	if p, ok := s.w.place.Row(key); ok {
		pl.Found, pl.VM, pl.Endpoint, pl.Epoch, pl.Gen, pl.Moving = true, p.Owner, p.Owner, uint64(p.Epoch), uint64(p.Gen), p.State == owner.PlaceMoving
	}
	return pl, nil
}

func (s locStore) APIKey(_ context.Context, id string) (locator.StoredKey, error) {
	if !s.w.pgUp(nodeControld) {
		return locator.StoredKey{}, errPGDown
	}
	if id != s.w.apiKeyID {
		return locator.StoredKey{}, locator.ErrNotFound
	}
	return s.w.apiKeyRow, nil
}

// lifecycleClient is controld's lifecycle client for one VM (control/agenthttp's role) over the simulated
// network: the agent answers with agentglue.Lifecycle; a lost message or a down agent is an error of unknown
// outcome, which the controller resumes later.
type lifecycleClient struct {
	w  *cworld
	vm string
}

var _ control.Agent = lifecycleClient{}

// callAnswer is a handler's result carried back over the network.
type callAnswer struct {
	v   any
	err error
}

func (l lifecycleClient) call(ctx context.Context, what, key string, fn func(context.Context, *cinc) (any, error)) (any, error) {
	r := ctxRand(ctx)
	if r == nil {
		panic("sim: a lifecycle call without an operation stream")
	}
	cctx, cancel := det.WithTimeout(ctx, l.w.clk, 5*time.Second)
	defer cancel()
	v, err := l.w.net.call(cctx, r, nodeControld, l.vm, nil, nil, func(sctx context.Context) any {
		a := l.w.inc(l.vm)
		if a == nil {
			return callAnswer{err: errRefused}
		}
		c, cancel := det.WithTimeout(sctx, l.w.clk, 10*time.Second)
		defer cancel()
		x, err := fn(c, a)
		// Two calls for one key may finish in one step (one waited on the other's lock): each its own stream.
		l.w.tr.add(fmt.Sprintf("life/%s/%04x", key, ctxRand(sctx).Uint32()&0xffff), "%s on %s -> %s", what, l.vm, lifeClass(err))
		return callAnswer{x, err}
	})
	if err != nil {
		return nil, fmt.Errorf("sim: lifecycle %s %s on %s: %w", what, key, l.vm, err)
	}
	a := v.(callAnswer)
	return a.v, a.err
}

func (l lifecycleClient) Claim(ctx context.Context, key string) (owner.Local, error) {
	v, err := l.call(ctx, "claim", key, func(c context.Context, a *cinc) (any, error) { return a.life.Claim(c, key) })
	loc, _ := v.(owner.Local)
	return loc, err
}

func (l lifecycleClient) Fence(ctx context.Context, key string) (owner.Ack, error) {
	start := l.w.clk.Now()
	v, err := l.call(ctx, "fence", key, func(c context.Context, a *cinc) (any, error) {
		ack, err := a.life.Fence(c, key)
		if err == nil && a.g.alive() {
			l.w.model.durable(key, start) // a fence acknowledges every admitted write as published
		}
		return ack, err
	})
	ack, _ := v.(owner.Ack)
	return ack, err
}

func (l lifecycleClient) Forget(ctx context.Context, key string) error {
	_, err := l.call(ctx, "forget", key, func(c context.Context, a *cinc) (any, error) { return nil, a.life.Forget(c, key) })
	return err
}

// lifeClass names a lifecycle answer by its sentinel.
func lifeClass(err error) string {
	for _, s := range []struct {
		err  error
		name string
	}{{owner.ErrWrongOwner, "wrong_owner"}, {owner.ErrSuperseded, "superseded"}, {owner.ErrFenced, "fenced"}, {owner.ErrMoving, "moving"},
		{owner.ErrUnavailable, "unavailable"}, {owner.ErrConflict, "conflict"}, {owner.ErrIllegal, "illegal"}} {
		if errors.Is(err, s.err) {
			return s.name
		}
	}
	return errClass(err)
}
