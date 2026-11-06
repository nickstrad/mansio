package sim

// The glue between the owner table, the virtual-actor host and the front, as cmd/agentd wires durable mode.
// This file MIRRORS cmd/agentd/glue.go and cmd/agentd/unavailable.go (at 7dc3851, IR-2a) so the sim runs the
// same translations; the only differences are a det.Clock for the activation check's deadline, no canary and
// no Postgres sentinel. Known gap (DST-2): lift these adapters into a package that cmd/agentd and the sim both
// import, so they cannot drift (left for after IR-2b, which is editing cmd/agentd).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/actordb"
)

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

// hostWriteBack implements owner.WriteBack over the vactor host and its store (agentd's hostWriteBack).
type hostWriteBack struct {
	host  *vactor.Host
	store durable.Store
}

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
		return 0, fmt.Errorf("sim: cannot write back %s at epoch %d: no manifest: %w", key, epoch, err)
	case err != nil:
		return 0, err
	case m.Epoch > epoch:
		return 0, fmt.Errorf("sim: cannot write back %s at epoch %d: storage is at epoch %d: %w", key, epoch, m.Epoch, owner.ErrStale)
	case m.Epoch < epoch:
		return 0, fmt.Errorf("sim: cannot write back %s at epoch %d: storage lacks this owner's stamp (epoch %d)", key, epoch, m.Epoch)
	}
	return m.Gen, nil
}

func (w hostWriteBack) Head(ctx context.Context, key string) (owner.Manifest, error) {
	m, _, err := w.store.ReadManifest(ctx, key)
	if err != nil {
		return owner.Manifest{}, ownerErr(err)
	}
	return owner.Manifest{Gen: owner.Generation(m.Generation), Epoch: owner.Epoch(m.Epoch)}, nil
}

func (w hostWriteBack) Stamp(ctx context.Context, key string, epoch owner.Epoch, gen owner.Generation) error {
	_, err := w.host.Stamp(ctx, key, uint64(epoch), uint64(gen))
	return ownerErr(err)
}

const checkActivationTimeout = 5 * time.Second

func activationCheck(clk det.Clock, tab func() *owner.Table) func(key string, epoch uint64, m durable.Manifest) error {
	return func(key string, epoch uint64, m durable.Manifest) error {
		ctx, cancel := det.WithTimeout(context.Background(), clk, checkActivationTimeout)
		defer cancel()
		return tab().CheckActivation(ctx, key, owner.Epoch(epoch), owner.Manifest{Gen: owner.Generation(m.Generation), Epoch: owner.Epoch(m.Epoch)})
	}
}

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

func notOwned(tab *owner.Table, key string, err error) bool {
	if errors.Is(err, owner.ErrSuperseded) {
		return true
	}
	st := tab.State(key).State
	return (errors.Is(err, owner.ErrFenced) || errors.Is(err, owner.ErrWrongOwner)) && (st == owner.Fenced || st == owner.Absent)
}

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

type hostExecutor struct {
	host  *vactor.Host
	grace time.Duration
}

func (e hostExecutor) Exec(ctx context.Context, c front.Call) (front.Reply, error) {
	res, err := e.host.GetAt(ctx, c.DB, c.Epoch)
	if err != nil {
		return front.Reply{}, activationError(ctx, err)
	}
	db, ok := res.DB.(*actordb.DB)
	if !ok {
		return front.Reply{}, fmt.Errorf("sim: host served %s as %T, not an actor database (a wiring bug)", c.DB, res.DB)
	}
	return front.ActorExecutor{Host: oneActor{db.Actor()}, Grace: e.grace}.Exec(ctx, c)
}

func activationError(ctx context.Context, err error) error {
	var to error
	switch {
	case errors.Is(err, owner.ErrSuperseded), errors.Is(err, owner.ErrFenced), errors.Is(err, owner.ErrMoving):
		to = front.ErrFenced
	case errors.Is(err, owner.ErrWrongOwner), errors.Is(err, durable.ErrStale):
		to = front.ErrWrongOwner
	case errors.Is(err, vactor.ErrNotFound):
		to = front.ErrUnknownDB
	case errors.Is(err, vactor.ErrClosed):
		to = front.ErrShed
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		to = front.ErrExpired
	default:
		return err
	}
	return fmt.Errorf("%w: %w", to, err)
}

type oneActor struct{ a *actor.Actor }

func (o oneActor) Lookup(context.Context, string) (*actor.Actor, error) { return o.a, nil }

var unavailableCauses = []error{owner.ErrUnavailable, vactor.ErrBusy, vactor.ErrInDoubt, durable.ErrUnavailable}

func asUnavailable(err error) error {
	if err == nil || errors.Is(err, front.ErrUnavailable) {
		return err
	}
	for _, c := range unavailableCauses {
		if errors.Is(err, c) {
			return fmt.Errorf("%w: %w", front.ErrUnavailable, err)
		}
	}
	return err
}

type depAdmitter struct{ front.Admitter }

func (a depAdmitter) Admit(db string, epoch uint64) (func(), error) {
	release, err := a.Admitter.Admit(db, epoch)
	return release, asUnavailable(err)
}

type depExecutor struct{ front.Executor }

func (e depExecutor) Exec(ctx context.Context, c front.Call) (front.Reply, error) {
	r, err := e.Executor.Exec(ctx, c)
	return r, asUnavailable(err)
}
