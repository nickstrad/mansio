package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/owner"
)

// Controller runs the database lifecycle against the registries, the authority, the agents and storage.
// Safe for concurrent use on different keys; two calls on one key are safe too (every step is a CAS or
// idempotent) but waste work.
type Controller struct {
	Registry  Registry
	Placement owner.Placement // the authority (linearizable Get, CAS)
	Agents    func(vm string) (Agent, error)
	Objects   Objects
	Policy    Policy
	// LoadLimit bounds the VMs the policy reads, so the result set stays bounded. Default 1000.
	LoadLimit int
	// Clock times the per-key deadlines of Restore and ResumeStuck. Default det.System().
	Clock det.Clock
}

func (c *Controller) loadLimit() int {
	if c.LoadLimit > 0 {
		return c.LoadLimit
	}
	return 1000
}

// pick asks the policy for a VM, never one in exclude.
func (c *Controller) pick(ctx context.Context, exclude ...string) (string, error) {
	loads, err := c.Registry.Load(ctx, c.loadLimit())
	if err != nil {
		return "", err
	}
	return c.Policy.Pick(loads, exclude...)
}

// picker reads the load once and then counts its own picks, so placing many keys (a restore) costs one
// vm_load.sql query, not one per key. The authority's CAS keeps a stale count safe; counting locally spreads
// a burst better than re-reading would.
type picker struct {
	c     *Controller
	loads []VMLoad
}

func (p *picker) pick(ctx context.Context, exclude ...string) (string, error) {
	if p.loads == nil {
		loads, err := p.c.Registry.Load(ctx, p.c.loadLimit())
		if err != nil {
			return "", err
		}
		p.loads = loads
	}
	vm, err := p.c.Policy.Pick(p.loads, exclude...)
	for i := range p.loads {
		if p.loads[i].VM == vm {
			p.loads[i].ActiveDBs++
		}
	}
	return vm, err
}

// agent returns vm's lifecycle client, or, for a VM an operator attested dead, a deadAgent that never calls it: the
// one place the controller treats dead VMs differently.
func (c *Controller) agent(ctx context.Context, vm string) (Agent, error) {
	dead, err := c.dead(ctx, vm)
	if err != nil {
		return nil, err
	}
	if dead {
		return deadAgent{c: c, vm: vm}, nil
	}
	a, err := c.Agents(vm)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrNoAgent, vm, err)
	}
	return a, nil
}

// deadAgent stands in for a VM attested dead. Its fence is the authority's own row: the Ack is the row's
// epoch and generation, and the destination's Claim starts from whatever generation storage holds (≥ the
// row's), stamping a higher epoch so a zombie's later publish fails (OS W). The dead VM's unpublished writes
// are lost: the RPO the attestation accepts. Forget has nothing to do; Claim is refused.
type deadAgent struct {
	c  *Controller
	vm string
}

func (d deadAgent) Fence(ctx context.Context, key string) (owner.Ack, error) {
	p, err := d.c.Placement.Get(ctx, key)
	if err != nil {
		return owner.Ack{}, err
	}
	if p.Owner != d.vm || p.State != owner.PlaceMoving {
		return owner.Ack{}, errs.Wrap(module, owner.ErrConflict, "cannot fence %s on dead vm %s: the row changed to %+v", key, d.vm, p)
	}
	return owner.Ack{Key: key, Epoch: p.Epoch, Gen: p.Gen}, nil
}

func (d deadAgent) Claim(context.Context, string) (owner.Local, error) {
	return owner.Local{}, errs.Wrap(module, ErrNotDead, "cannot claim on vm %s: it is attested dead", d.vm)
}

func (d deadAgent) Forget(context.Context, string) error { return nil }

// move runs owner.Mover for key towards dst.
func (c *Controller) move(ctx context.Context, key, dst string) error {
	_, err := (&owner.Mover{Placement: c.Placement, Agent: c.moverAgent}).Move(ctx, key, dst)
	return err
}

// Create makes a new database tenant:name and places it (flow B). A retried Create resumes; creating a name
// that exists returns its row; a name of a dropped database is ErrExists.
//
//	post: err == nil ⇒ the registry says active, the authority ACTIVE(vm, epoch ≥ 1), and vm claimed it
func (c *Controller) Create(ctx context.Context, tenant, name string) (Database, error) {
	d, err := c.Registry.CreateDatabase(ctx, tenant, name)
	if err != nil {
		return d, errs.Wrap(module, err, "cannot create database %s:%s: not registered; safe to retry", tenant, name)
	}
	switch d.State {
	case StateCreating:
		return c.finishCreate(ctx, d)
	case StateDropping, StateDropped:
		return d, errs.Wrap(module, ErrExists, "cannot create database %s: the name belonged to a dropped database", d.Key)
	}
	return d, nil // created before
}

// finishCreate places a "creating" database: CAS(absent → ACTIVE(pick, 1, 0)) unless a row exists, then the
// owner's Claim, then "active".
func (c *Controller) finishCreate(ctx context.Context, d Database) (Database, error) {
	key := d.Key
	p, err := c.Placement.Get(ctx, key)
	if errors.Is(err, owner.ErrNotFound) {
		vm, perr := c.pick(ctx)
		if perr != nil {
			return d, errs.Wrap(module, perr, "cannot create database %s: no vm to place it on; still creating, resume later", key)
		}
		p = owner.Place{Owner: vm, Epoch: 1, State: owner.PlaceActive}
		err = c.Placement.CAS(ctx, key, owner.Place{}, p)
		if errors.Is(err, owner.ErrConflict) {
			p, err = c.Placement.Get(ctx, key) // placed meanwhile (a resumed create): claim wherever it is
		}
	}
	if err != nil {
		return d, errs.Wrap(module, err, "cannot create database %s: authority unavailable or the placement unknown; still creating, resume later", key)
	}
	if p.State != owner.PlaceActive {
		return d, errs.Wrap(module, ErrState, "cannot create database %s: its placement row is %s; resume after it settles", key, p.State)
	}
	if err := c.claim(ctx, p.Owner, key); err != nil {
		return d, errs.Wrap(module, err, "cannot create database %s: %s did not claim it; still creating, resume later", key, p.Owner)
	}
	return c.setState(ctx, d, StateActive, "")
}

func (c *Controller) claim(ctx context.Context, vm, key string) error {
	_, err := c.moverAgent(vm).Claim(ctx, key)
	return err
}

// setState CASes d's registry state and returns d updated.
func (c *Controller) setState(ctx context.Context, d Database, to, target string) (Database, error) {
	if err := c.Registry.SetDatabaseState(ctx, d.Key, d.State, to, target); err != nil {
		return d, errs.Wrap(module, err, "cannot set database %s from %s to %s; resume to finish", d.Key, d.State, to)
	}
	d.State, d.Target = to, target
	return d, nil
}

// get reads key's registry row.
func (c *Controller) get(ctx context.Context, key string) (Database, error) {
	d, err := c.Registry.GetDatabase(ctx, key)
	if err != nil {
		return d, errs.Wrap(module, err, "cannot read database %s from the registry", key)
	}
	return d, nil
}

// Move moves an active database to dst (flow C; "" lets the policy pick another VM). A move already in
// progress to the same dst resumes; to another dst it is ErrState.
//
//	post: err == nil ⇒ dst owns key at a new epoch and every write the source acknowledged is in the
//	      generation dst started from (owner.Mover)
func (c *Controller) Move(ctx context.Context, key, dst string) (Database, error) {
	d, err := c.get(ctx, key)
	if err != nil {
		return d, err
	}
	switch {
	case d.State == StateMoving && (dst == "" || dst == d.Target):
		return c.finishMove(ctx, d)
	case d.State != StateActive:
		return d, errs.Wrap(module, ErrState, "cannot move database %s: it is %s (target %q)", key, d.State, d.Target)
	}
	p, err := c.Placement.Get(ctx, key)
	if err != nil {
		return d, errs.Wrap(module, err, "cannot move database %s: authority unreadable; nothing changed", key)
	}
	if dst == "" {
		if dst, err = c.pick(ctx, p.Owner); err != nil {
			return d, errs.Wrap(module, err, "cannot move database %s: no other vm to move it to; nothing changed", key)
		}
	}
	if d, err = c.setState(ctx, d, StateMoving, dst); err != nil {
		return d, err
	}
	return c.finishMove(ctx, d)
}

// finishMove runs owner.Mover towards d.Target, then marks the database active.
func (c *Controller) finishMove(ctx context.Context, d Database) (Database, error) {
	if err := c.move(ctx, d.Key, d.Target); err != nil {
		return d, errs.Wrap(module, err, "cannot move database %s to %s; still moving, resume later", d.Key, d.Target)
	}
	return c.setState(ctx, d, StateActive, "")
}

// moverAgent is owner.Mover's view of the agents: each call resolves vm through agent (live or dead).
func (c *Controller) moverAgent(vm string) owner.Agent { return moverAgent{c: c, vm: vm} }

type moverAgent struct {
	c  *Controller
	vm string
}

func (m moverAgent) Fence(ctx context.Context, key string) (owner.Ack, error) {
	a, err := m.c.agent(ctx, m.vm)
	if err != nil {
		return owner.Ack{}, err
	}
	return a.Fence(ctx, key)
}

func (m moverAgent) Claim(ctx context.Context, key string) (owner.Local, error) {
	a, err := m.c.agent(ctx, m.vm)
	if err != nil {
		return owner.Local{}, err
	}
	return a.Claim(ctx, key)
}

// dead reports whether an operator attested vm dead.
func (c *Controller) dead(ctx context.Context, vm string) (bool, error) {
	v, err := c.Registry.VM(ctx, vm)
	if err != nil {
		return false, errs.Wrap(module, err, "cannot read vm %s from the registry", vm)
	}
	return v.Dead, nil
}

// Suspend stops serving an active database: "suspended", the row MOVING (no owner admits it), and the owner
// fenced (its last writes published). Repeating it finishes a suspend that stopped halfway. Resume undoes it.
func (c *Controller) Suspend(ctx context.Context, key string) (Database, error) {
	d, err := c.get(ctx, key)
	if err != nil {
		return d, err
	}
	switch d.State {
	case StateActive:
		if d, err = c.setState(ctx, d, StateSuspended, ""); err != nil {
			return d, err
		}
	case StateSuspended:
	default:
		return d, errs.Wrap(module, ErrState, "cannot suspend database %s: it is %s", key, d.State)
	}
	if _, err := c.stop(ctx, key); err != nil {
		return d, errs.Wrap(module, err, "cannot suspend database %s: not fenced yet; call suspend again", key)
	}
	return d, nil
}

// Resume serves a suspended database again, on dst ("" lets the policy pick): a move from the MOVING row.
func (c *Controller) Resume(ctx context.Context, key, dst string) (Database, error) {
	d, err := c.get(ctx, key)
	if err != nil {
		return d, err
	}
	if d.State != StateSuspended {
		return d, errs.Wrap(module, ErrState, "cannot resume database %s: it is %s", key, d.State)
	}
	if dst == "" {
		if dst, err = c.pick(ctx); err != nil {
			return d, errs.Wrap(module, err, "cannot resume database %s: no vm; still suspended", key)
		}
	}
	if d, err = c.setState(ctx, d, StateMoving, dst); err != nil {
		return d, err
	}
	return c.finishMove(ctx, d)
}

// stop makes key's row MOVING and fences its owner (a dead owner's fence is its row). It returns the MOVING
// row; with no row, an error matching owner.ErrNotFound.
func (c *Controller) stop(ctx context.Context, key string) (owner.Place, error) {
	for range 4 {
		p, err := c.Placement.Get(ctx, key)
		if err != nil {
			return p, err // owner.ErrNotFound included
		}
		if p.State == owner.PlaceActive {
			next := p
			next.State = owner.PlaceMoving
			if err := c.Placement.CAS(ctx, key, p, next); err != nil && !errors.Is(err, owner.ErrConflict) {
				return p, err
			}
			continue // read it again: ours, or someone else's change
		}
		if _, err := c.moverAgent(p.Owner).Fence(ctx, key); err != nil && !errors.Is(err, owner.ErrWrongOwner) {
			return p, err // ErrWrongOwner: the agent holds nothing for key (never claimed, or forgotten)
		}
		return p, nil
	}
	return owner.Place{}, errs.Wrap(module, owner.ErrConflict, "cannot stop %s: its placement row kept changing", key)
}

// Drop deletes a database: the "dropping" tombstone first, then the row MOVING and the owner fenced, then every
// object under its prefix, then the agent's record, then the placement row; the registry row stays as "dropped". A
// move in progress must finish first (ErrState). Repeating Drop resumes it.
func (c *Controller) Drop(ctx context.Context, key string) (Database, error) {
	d, err := c.get(ctx, key)
	if err != nil {
		return d, err
	}
	switch d.State {
	case StateDropped:
		return d, nil
	case StateDropping:
	case StateActive, StateSuspended, StateCreating:
		if d, err = c.setState(ctx, d, StateDropping, ""); err != nil {
			return d, err
		}
	default:
		return d, errs.Wrap(module, ErrState, "cannot drop database %s: it is %s; finish that first", key, d.State)
	}
	return c.finishDrop(ctx, d)
}

func (c *Controller) finishDrop(ctx context.Context, d Database) (Database, error) {
	key := d.Key
	row, err := c.stop(ctx, key)
	placed := !errors.Is(err, owner.ErrNotFound)
	if err != nil && placed {
		return d, errs.Wrap(module, err, "cannot drop database %s: not stopped yet; still dropping, resume later", key)
	}
	if _, err := c.Objects.DeletePrefix(ctx, key); err != nil {
		return d, errs.Wrap(module, err, "cannot drop database %s: objects not all deleted; still dropping, resume later", key)
	}
	if placed {
		a, err := c.agent(ctx, row.Owner)
		if err == nil {
			err = a.Forget(ctx, key) // a dead owner has nothing to forget
		}
		if err != nil {
			return d, errs.Wrap(module, err, "cannot drop database %s: %s did not forget it; still dropping, resume later", key, row.Owner)
		}
		if err := c.Registry.DropPlacement(ctx, key, row); err != nil {
			return d, errs.Wrap(module, err, "cannot drop database %s: placement row not deleted; still dropping, resume later", key)
		}
	}
	return c.setState(ctx, d, StateDropped, "")
}

// AttestDead records an operator's attestation that vm is dead. Nothing moves until Restore.
func (c *Controller) AttestDead(ctx context.Context, vm, by string) error {
	if err := c.Registry.AttestDead(ctx, vm, by); err != nil {
		return errs.Wrap(module, err, "cannot record that vm %s is dead", vm)
	}
	return nil
}

// RestoreReport is what Restore did.
type RestoreReport struct {
	Moved   map[string]string // key → new owner
	Skipped []string          // suspended databases: they stay suspended (Resume places them)
	Failed  map[string]error  // key → why; Restore again to retry them
}

// Restore moves every database a VM attested dead owns to live VMs (policy picks), from the generation storage holds.
// Without an attestation it is ErrNotDead. Drops in progress finish instead. Each key gets perKey (0: no own
// deadline).
func (c *Controller) Restore(ctx context.Context, vm string, perKey time.Duration) (RestoreReport, error) {
	rep := RestoreReport{Moved: map[string]string{}, Failed: map[string]error{}}
	dead, err := c.dead(ctx, vm)
	if err != nil {
		return rep, err
	}
	if !dead {
		return rep, errs.Wrap(module, ErrNotDead, "cannot restore the databases of vm %s: attest it dead first (M5)", vm)
	}
	const page = 100
	after := ""
	pk := &picker{c: c}
	for {
		rows, err := c.Registry.OwnedBy(ctx, vm, after, page)
		if err != nil {
			return rep, errs.Wrap(module, err, "cannot list the databases of vm %s; restore again to continue", vm)
		}
		for _, r := range rows {
			kctx, cancel := c.withTimeout(ctx, perKey)
			to, skipped, err := c.restoreOne(kctx, r.Key, vm, pk)
			cancel()
			switch {
			case err != nil:
				rep.Failed[r.Key] = err
			case skipped:
				rep.Skipped = append(rep.Skipped, r.Key)
			default:
				rep.Moved[r.Key] = to
			}
		}
		if len(rows) < page {
			return rep, nil
		}
		after = rows[len(rows)-1].Key
	}
}

func (c *Controller) restoreOne(ctx context.Context, key, deadVM string, pk *picker) (to string, skipped bool, err error) {
	d, err := c.get(ctx, key)
	if errors.Is(err, owner.ErrNotFound) {
		// A key the registry does not know (placed by hand or by a test): move it all the same.
		dst, err := pk.pick(ctx, deadVM)
		if err != nil {
			return "", false, err
		}
		return dst, false, c.move(ctx, key, dst)
	}
	if err != nil {
		return "", false, err
	}
	switch {
	case d.State == StateSuspended:
		return "", true, nil
	case d.State == StateDropping:
		_, err := c.finishDrop(ctx, d)
		return "", false, err
	case d.State == StateMoving && d.Target != deadVM:
		_, err := c.finishMove(ctx, d)
		return d.Target, false, err
	}
	// Active, creating (placed on the dead VM, maybe never claimed), or moving to the dead VM: re-target.
	dst, err := pk.pick(ctx, deadVM)
	if err != nil {
		return "", false, err
	}
	if d, err = c.setState(ctx, d, StateMoving, dst); err != nil {
		return "", false, err
	}
	_, err = c.finishMove(ctx, d)
	return dst, false, err
}

// ResumeReport is what one ResumeStuck pass did.
type ResumeReport struct {
	Resumed int              // flows finished
	Skipped int              // suspended rows (expected to stay MOVING)
	Failed  map[string]error // key → why; the next pass retries
}

// ResumeStuck finishes flows that stopped halfway (a crash, a timeout, an agent down): placement rows MOVING
// for longer than olderThan (moving.sql) resume their move or drop, and databases "creating", "moving" or
// "dropping" for longer than olderThan (pending.sql) finish (a move whose claim failed after the flip has an
// ACTIVE row, which only pending.sql finds). At most limit of each per pass;
// each key gets perKey. Table scans, never notifications, which can be lost.
func (c *Controller) ResumeStuck(ctx context.Context, olderThan time.Duration, limit int, perKey time.Duration) (ResumeReport, error) {
	rep := ResumeReport{Failed: map[string]error{}}
	stuck, err := c.Registry.Moving(ctx, olderThan, limit)
	if err != nil {
		return rep, errs.Wrap(module, err, "cannot list stuck moves; next pass retries")
	}
	pending, err := c.Registry.Pending(ctx, olderThan, limit)
	if err != nil {
		return rep, errs.Wrap(module, err, "cannot list pending creates and drops; next pass retries")
	}
	done := map[string]bool{}
	run := func(key string, fn func(ctx context.Context) error) {
		if done[key] {
			return
		}
		done[key] = true
		kctx, cancel := c.withTimeout(ctx, perKey)
		defer cancel()
		if err := fn(kctx); err != nil {
			rep.Failed[key] = err
			return
		}
		rep.Resumed++
	}
	for _, s := range stuck {
		switch s.Intent {
		case StateSuspended:
			rep.Skipped++
		case StateMoving, StateDropping:
			run(s.Key, func(ctx context.Context) error { return c.resume(ctx, s.Key) })
		default:
			// MOVING with no intent (a move begun outside the registry): finish it on the same owner, at a
			// new epoch, unless the owner is dead.
			run(s.Key, func(ctx context.Context) error {
				dst := s.Place.Owner
				if dead, err := c.dead(ctx, dst); err != nil {
					return err
				} else if dead {
					if dst, err = c.pick(ctx, dst); err != nil {
						return err
					}
				}
				return c.move(ctx, s.Key, dst)
			})
		}
	}
	for _, d := range pending {
		run(d.Key, func(ctx context.Context) error { return c.resume(ctx, d.Key) })
	}
	return rep, nil
}

// resume re-reads key's registry row and continues its flow.
func (c *Controller) resume(ctx context.Context, key string) error {
	d, err := c.get(ctx, key)
	if err != nil {
		return err
	}
	switch d.State {
	case StateCreating:
		_, err = c.finishCreate(ctx, d)
	case StateMoving:
		_, err = c.finishMove(ctx, d)
	case StateDropping:
		_, err = c.finishDrop(ctx, d)
	}
	return err
}

func (c *Controller) withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return det.WithTimeout(ctx, det.Or(c.Clock), d)
}
