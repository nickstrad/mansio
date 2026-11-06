// Package agentglue is the glue between agentd's durable-mode parts (control-plane plan CP-1): the owner table,
// the virtual-actor host and the front pipeline. Each type is a thin adapter that translates sentinels between
// packages and decides nothing itself. cmd/agentd wires it, and so does the deterministic simulation
// (internal/sim), so the two cannot drift apart.
//
// Role: adapter (no I/O of its own beyond the ports it is given). Error module: "agentglue".
package agentglue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/actordb"
)

const module = "agentglue"

// OwnerErr translates durable's answers to the owner.WriteBack contract: a failed condition is owner.ErrStale
// (since OS R4 durable.ErrStale always means another writer), a missing manifest owner.ErrNoManifest. The
// chain is kept for the log.
func OwnerErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, durable.ErrStale):
		return fmt.Errorf("%w: %w", owner.ErrStale, err)
	case errors.Is(err, durable.ErrNoManifest):
		return fmt.Errorf("%w: %w", owner.ErrNoManifest, err)
	}
	return err
}

// WriteBack implements owner.WriteBack (contract 1) over the vactor host and its store.
type WriteBack struct {
	Host  *vactor.Host
	Store durable.Store // the host's own store (behind the same S3 breaker)
}

var _ owner.WriteBack = WriteBack{}

// Publish writes key back at epoch if it is warm and dirty. A key that is not warm here has nothing
// unpublished (passivation writes back before it drops a copy, and a restart starts from storage), so its
// generation is the manifest's, provided the manifest still carries this epoch.
func (w WriteBack) Publish(ctx context.Context, key string, epoch owner.Epoch) (owner.Generation, error) {
	res, err := w.Host.WriteBackAt(ctx, key, uint64(epoch))
	if err != nil {
		return 0, OwnerErr(err)
	}
	if res.Wrote || res.Generation > 0 {
		return owner.Generation(res.Generation), nil
	}
	m, err := w.Head(ctx, key)
	switch {
	case errors.Is(err, owner.ErrNoManifest):
		return 0, errs.Wrap(module, err, "cannot write back %s at epoch %d: no manifest, though the claim stamped one; nothing published", key, epoch)
	case err != nil:
		return 0, err
	case m.Epoch > epoch:
		return 0, errs.Wrap(module, owner.ErrStale, "cannot write back %s at epoch %d: storage is at epoch %d", key, epoch, m.Epoch)
	case m.Epoch < epoch:
		return 0, errs.Wrap(module, nil, "cannot write back %s at epoch %d: storage lacks this owner's stamp (epoch %d); nothing published, re-check storage", key, epoch, m.Epoch)
	}
	return m.Gen, nil
}

// Head reads the stored manifest.
func (w WriteBack) Head(ctx context.Context, key string) (owner.Manifest, error) {
	m, _, err := w.Store.ReadManifest(ctx, key)
	if err != nil {
		return owner.Manifest{}, OwnerErr(err)
	}
	return owner.Manifest{Gen: owner.Generation(m.Generation), Epoch: owner.Epoch(m.Epoch)}, nil
}

// Stamp re-publishes the manifest at epoch (through the host, so a warm copy adopts the epoch).
func (w WriteBack) Stamp(ctx context.Context, key string, epoch owner.Epoch, gen owner.Generation) error {
	_, err := w.Host.Stamp(ctx, key, uint64(epoch), uint64(gen))
	return OwnerErr(err)
}

// CheckActivationTimeout bounds the only I/O CheckActivation may do: recording a supersede durably.
const CheckActivationTimeout = 5 * time.Second

// ActivationCheck is vactor.Options.CheckActivation: the owner table's guard before an activation fetches.
// The host's hook has no ctx, so it derives its own bounded one on clk (Release It T1; nil clk = real time).
func ActivationCheck(clk det.Clock, tab func() *owner.Table) func(key string, epoch uint64, m durable.Manifest) error {
	clk = det.Or(clk)
	return func(key string, epoch uint64, m durable.Manifest) error {
		ctx, cancel := det.WithTimeout(context.Background(), clk, CheckActivationTimeout)
		defer cancel()
		return tab().CheckActivation(ctx, key, owner.Epoch(epoch), owner.Manifest{Gen: owner.Generation(m.Generation), Epoch: owner.Epoch(m.Epoch)})
	}
}

// PublishGuard is vactor.Options.Guard: every write-back and passivation a maintenance tick starts runs under
// owner.Table.Publish, so a fence drains it, and an ErrStale fences the key durably as superseded. The warm
// copy of a key this process no longer owns is dropped without a write-back (its writes must not survive, or
// the fence already published them).
func PublishGuard(tab func() *owner.Table, host func() *vactor.Host) func(context.Context, string, uint64, func(context.Context) error) error {
	return func(ctx context.Context, key string, epoch uint64, do func(context.Context) error) error {
		_, err := tab().Publish(ctx, key, owner.Epoch(epoch), func(ctx context.Context) (owner.Generation, error) {
			return 0, OwnerErr(do(ctx))
		})
		if err != nil && notOwned(tab(), key, err) {
			host().Discard(key)
		}
		return err
	}
}

// notOwned reports whether err says key is fenced or no longer held here, so a warm copy may be discarded: a
// fenced key's writes were published by its fence (or must not survive, if superseded); an absent one admits
// nothing. A key that is fencing, closed or active at another epoch is kept.
func notOwned(tab *owner.Table, key string, err error) bool {
	if errors.Is(err, owner.ErrSuperseded) {
		return true
	}
	st := tab.State(key).State
	return (errors.Is(err, owner.ErrFenced) || errors.Is(err, owner.ErrWrongOwner)) && (st == owner.Fenced || st == owner.Absent)
}

// Lifecycle is an agent's side of the control plane's lifecycle calls (agentd's /v1/lifecycle/{claim,fence,
// forget}, option 4 §3.4) over the owner table and the host. It implements control.Agent and owner.Agent.
type Lifecycle struct {
	Tab  *owner.Table
	Host *vactor.Host
}

// Claim claims key here (owner.Table.Claim).
func (l Lifecycle) Claim(ctx context.Context, key string) (owner.Local, error) {
	return l.Tab.Claim(ctx, key)
}

// Fence fences key (owner.Table.Fence) and drops its warm copy once the fence published it, or once storage
// shows another owner superseded this one (its writes must not survive).
func (l Lifecycle) Fence(ctx context.Context, key string) (owner.Ack, error) {
	ack, err := l.Tab.Fence(ctx, key)
	if err == nil || errors.Is(err, owner.ErrSuperseded) {
		l.Host.Discard(key)
	}
	return ack, err
}

// Forget drops key's local record (owner.Table.Forget).
func (l Lifecycle) Forget(ctx context.Context, key string) error { return l.Tab.Forget(ctx, key) }

// Admitter is the front's Admitter over the owner table. Errors are bare sentinels (the hot path); a key that
// is not reconciled yet answers owner.ErrUnavailable, which DepAdmitter turns into front.ErrUnavailable.
type Admitter struct{ T *owner.Table }

// Admit admits one request for db at epoch.
func (a Admitter) Admit(db string, epoch uint64) (func(), error) {
	p, err := a.T.Admit(db, owner.Epoch(epoch))
	switch {
	case err == nil:
		return p.Done, nil
	case errors.Is(err, owner.ErrWrongOwner):
		return nil, front.ErrWrongOwner
	case errors.Is(err, owner.ErrFenced), errors.Is(err, owner.ErrMoving):
		return nil, front.ErrFenced
	}
	return nil, err
}

// Executor is the front's Executor over the virtual-actor host: GetAt (activate if cold) at the ticket's
// epoch, then one statement on the database's actor. front.CanaryDB goes to Canary instead (nil: no canary).
type Executor struct {
	Host   *vactor.Host
	Grace  time.Duration
	Canary front.Executor
	// Order, if set, runs once the database is active and before its statement is submitted; an error ends
	// the call with it (nothing submitted). The simulation orders submissions with it (several requests woken
	// by one activation would otherwise race to the actor); agentd leaves it nil.
	Order func(ctx context.Context, db string) error
}

// Exec implements front.Executor.
func (e Executor) Exec(ctx context.Context, c front.Call) (front.Reply, error) {
	if c.DB == front.CanaryDB && e.Canary != nil {
		return e.Canary.Exec(ctx, c)
	}
	res, err := e.Host.GetAt(ctx, c.DB, c.Epoch)
	if err != nil {
		return front.Reply{}, ActivationError(ctx, err)
	}
	db, ok := res.DB.(*actordb.DB)
	if !ok {
		return front.Reply{}, errs.Wrap(module, nil, "host served %s as %T, not an actor database (a wiring bug)", c.DB, res.DB)
	}
	if e.Order != nil {
		if err := e.Order(ctx, c.DB); err != nil {
			return front.Reply{}, err
		}
	}
	return front.ActorExecutor{Host: OneActor{db.Actor()}, Grace: e.Grace}.Exec(ctx, c)
}

// ActivationError maps a failed GetAt to the front's sentinels, keeping the chain (and any retry hint).
// Dependency failures (vactor.ErrBusy, durable.ErrUnavailable, owner.ErrUnavailable) pass through for
// DepExecutor; anything unexpected (a corrupt generation, say) stays internal.
func ActivationError(ctx context.Context, err error) error {
	var to error
	switch {
	case errors.Is(err, owner.ErrSuperseded), errors.Is(err, owner.ErrFenced), errors.Is(err, owner.ErrMoving):
		to = front.ErrFenced
	case errors.Is(err, owner.ErrWrongOwner), errors.Is(err, durable.ErrStale):
		to = front.ErrWrongOwner // a higher epoch owns it: re-resolve
	case errors.Is(err, vactor.ErrNotFound):
		to = front.ErrUnknownDB
	case errors.Is(err, vactor.ErrClosed):
		to = front.ErrShed
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		to = front.ErrExpired // this caller stopped waiting; nothing ran for it
	default:
		return err
	}
	return fmt.Errorf("%w: %w", to, err)
}

// OneActor is a front.Host serving one actor under every name.
type OneActor struct{ A *actor.Actor }

// Lookup returns the actor.
func (o OneActor) Lookup(context.Context, string) (*actor.Actor, error) { return o.A, nil }
