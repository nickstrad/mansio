package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/actordb"
)

// The glue between the owner table, the virtual-actor host and the front (control-plane plan CP-1). Each type
// here is a thin adapter: it translates sentinels between packages and decides nothing itself.

// ownerErr translates durable's answers to the owner.WriteBack contract: a failed condition is owner.ErrStale
// (since OS R4 durable.ErrStale always means another writer), a missing manifest owner.ErrNoManifest. The
// chain is kept for the log.
func ownerErr(err error) error {
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

// hostWriteBack implements owner.WriteBack (contract 1) over the vactor host and its store.
type hostWriteBack struct {
	host  *vactor.Host
	store durable.Store // the host's own store (behind the same S3 breaker)
}

var _ owner.WriteBack = hostWriteBack{}

// Publish writes key back at epoch if it is warm and dirty. A key that is not warm here has nothing
// unpublished (passivation writes back before it drops a copy, and a restart starts from storage), so its
// generation is the manifest's, provided the manifest still carries this epoch.
func (w hostWriteBack) Publish(ctx context.Context, key string, epoch owner.Epoch) (owner.Generation, error) {
	res, err := w.host.WriteBackAt(ctx, key, uint64(epoch))
	if err != nil {
		return 0, ownerErr(err)
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
func (w hostWriteBack) Head(ctx context.Context, key string) (owner.Manifest, error) {
	m, _, err := w.store.ReadManifest(ctx, key)
	if err != nil {
		return owner.Manifest{}, ownerErr(err)
	}
	return owner.Manifest{Gen: owner.Generation(m.Generation), Epoch: owner.Epoch(m.Epoch)}, nil
}

// Stamp re-publishes the manifest at epoch (through the host, so a warm copy adopts the epoch).
func (w hostWriteBack) Stamp(ctx context.Context, key string, epoch owner.Epoch, gen owner.Generation) error {
	_, err := w.host.Stamp(ctx, key, uint64(epoch), uint64(gen))
	return ownerErr(err)
}

// checkActivationTimeout bounds the only I/O CheckActivation may do: recording a supersede durably.
const checkActivationTimeout = 5 * time.Second

// activationCheck is vactor.Options.CheckActivation: the owner table's guard before an activation fetches.
// The host's hook has no ctx, so it derives its own bounded one (Release It T1).
func activationCheck(tab func() *owner.Table) func(key string, epoch uint64, m durable.Manifest) error {
	return func(key string, epoch uint64, m durable.Manifest) error {
		ctx, cancel := context.WithTimeout(context.Background(), checkActivationTimeout)
		defer cancel()
		return tab().CheckActivation(ctx, key, owner.Epoch(epoch), owner.Manifest{Gen: owner.Generation(m.Generation), Epoch: owner.Epoch(m.Epoch)})
	}
}

// publishGuard is vactor.Options.Guard: every write-back and passivation a maintenance tick starts runs under
// owner.Table.Publish, so a fence drains it, and an ErrStale fences the key durably as superseded. The warm
// copy of a key this process no longer owns is dropped without a write-back (its writes must not survive, or
// the fence already published them).
func publishGuard(tab func() *owner.Table, host func() *vactor.Host) func(context.Context, string, uint64, func(context.Context) error) error {
	return func(ctx context.Context, key string, epoch uint64, do func(context.Context) error) error {
		_, err := tab().Publish(ctx, key, owner.Epoch(epoch), func(ctx context.Context) (owner.Generation, error) {
			return 0, ownerErr(do(ctx))
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

// ownerAdmitter is the front's Admitter over the owner table. Errors are bare sentinels (the hot path); a key
// that is not reconciled yet answers owner.ErrUnavailable, which depAdmitter turns into front.ErrUnavailable.
type ownerAdmitter struct{ t *owner.Table }

func (a ownerAdmitter) Admit(db string, epoch uint64) (func(), error) {
	p, err := a.t.Admit(db, owner.Epoch(epoch))
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

// hostExecutor is the front's Executor over the virtual-actor host: GetAt (activate if cold) at the ticket's
// epoch, then one statement on the database's actor. front.CanaryDB goes to the canary executor instead.
type hostExecutor struct {
	host   *vactor.Host
	grace  time.Duration
	canary front.Executor
}

func (e hostExecutor) Exec(ctx context.Context, c front.Call) (front.Reply, error) {
	if c.DB == front.CanaryDB {
		return e.canary.Exec(ctx, c)
	}
	res, err := e.host.GetAt(ctx, c.DB, c.Epoch)
	if err != nil {
		return front.Reply{}, activationError(ctx, err)
	}
	db, ok := res.DB.(*actordb.DB)
	if !ok {
		return front.Reply{}, errs.Wrap(module, nil, "host served %s as %T, not an actor database (a wiring bug)", c.DB, res.DB)
	}
	return front.ActorExecutor{Host: oneActor{db.Actor()}, Grace: e.grace}.Exec(ctx, c)
}

// activationError maps a failed GetAt to the front's sentinels, keeping the chain (and any retry hint).
// Dependency failures (vactor.ErrBusy, durable.ErrUnavailable, owner.ErrUnavailable) pass through for
// depExecutor; anything unexpected (a corrupt generation, say) stays internal.
func activationError(ctx context.Context, err error) error {
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

// oneActor is a front.Host serving one actor under every name.
type oneActor struct{ a *actor.Actor }

func (o oneActor) Lookup(context.Context, string) (*actor.Actor, error) { return o.a, nil }
