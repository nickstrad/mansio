package pg

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/owner"
)

// The control plane's registries (schema 0002): tenants, databases and the VM registry's heartbeats. Every method is
// one statement under the same breaker and deadlines as the placement port.

// VM is one row of the VM registry.
type VM struct {
	ID             string
	Endpoint       string
	Dead           bool // an operator attested it dead
	DeadAttestedBy string
	Bytes          int64
	HeartbeatAge   float64 // seconds since its last heartbeat
}

// Heartbeat records that vm answered its health call just now, reporting bytes. Unknown vm ⇒ ErrUnknownVM.
func (p *Placement) Heartbeat(ctx context.Context, vm string, bytes int64) error {
	return p.call(ctx, "record the heartbeat of vm", vm, "safe to retry", func(ctx context.Context) error {
		var id string
		err := p.pool.QueryRow(ctx, q.heartbeat, vm, max(bytes, 0)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownVM
		}
		return err
	})
}

// AttestDead records an operator's attestation that vm is dead. Idempotent: the first attestation stays. Unknown vm ⇒
// ErrUnknownVM.
func (p *Placement) AttestDead(ctx context.Context, vm, by string) error {
	if by == "" {
		return errs.Wrap(module, owner.ErrInvalid, "cannot attest vm %q dead: the attester is required; nothing sent", vm)
	}
	return p.call(ctx, "attest dead vm", vm, "safe to retry", func(ctx context.Context) error {
		var who string
		err := p.pool.QueryRow(ctx, q.attestDead, vm, by).Scan(&who)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownVM
		}
		return err
	})
}

// VM returns vm's registry row. Unknown vm ⇒ ErrUnknownVM.
func (p *Placement) VM(ctx context.Context, vm string) (VM, error) {
	var v VM
	err := p.call(ctx, "read vm", vm, retryRead, func(ctx context.Context) error {
		err := p.pool.QueryRow(ctx, q.vm, vm).Scan(&v.ID, &v.Endpoint, &v.Dead, &v.DeadAttestedBy, &v.Bytes, &v.HeartbeatAge)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownVM
		}
		return err
	})
	return v, err
}

// Owned is a placement row with its key.
type Owned struct {
	Key   string
	Place owner.Place
}

// OwnedBy returns up to limit placement rows vm owns with keys after `after`, in key order (owned_by.sql).
func (p *Placement) OwnedBy(ctx context.Context, vm, after string, limit int) ([]Owned, error) {
	return list(p, ctx, "list databases owned by "+vm, q.ownedBy, func(row pgx.CollectableRow) (Owned, error) {
		var o Owned
		var vmID, state string
		var e, g int64
		if err := row.Scan(&o.Key, &vmID, &e, &g, &state); err != nil {
			return o, err
		}
		pl, err := decodePlace(vmID, e, g, state)
		o.Place = pl
		return o, err
	}, vm, limit, after)
}

// Database is one row of the database registry.
type Database struct {
	Key     string // tenant ":" name: the placement key
	Tenant  string
	Name    string
	Default bool
	State   string // creating | active | moving | suspended | dropping | dropped
	Target  string // the move's destination while State is "moving"
}

func scanDatabase(row pgx.CollectableRow) (Database, error) {
	var d Database
	err := row.Scan(&d.Key, &d.Tenant, &d.Name, &d.Default, &d.State, &d.Target)
	return d, err
}

// CreateDatabase registers tenant:name in state "creating" (creating the tenant if new) and returns its row;
// an existing row comes back unchanged, so a retried create resumes.
// Invalid names match owner.ErrInvalid (nothing sent). Two first creates racing for one tenant's default both
// compute is_default = true; the loser hits databases_one_default after the winner commits, and is run again
// here (its statement changed nothing; the second run sees the winner's default), at most defaultRetries times.
func (p *Placement) CreateDatabase(ctx context.Context, tenant, name string) (Database, error) {
	key := tenant + ":" + name
	if !ValidTenant(tenant) || !ValidName(name) {
		return Database{}, errs.Wrap(module, owner.ErrInvalid,
			"cannot register database %q: tenant must match %s and name %s; nothing sent", key, tenantPattern, namePattern)
	}
	var d Database
	err := p.call(ctx, "register database", key, retryWrite, func(ctx context.Context) error {
		for attempt := 0; ; attempt++ {
			rows, err := p.pool.Query(ctx, q.createDatabase, key, tenant, name)
			if err == nil {
				d, err = pgx.CollectExactlyOneRow(rows, scanDatabase)
			}
			if !uniqueOn(err, "databases_one_default") || attempt == defaultRetries {
				return err
			}
		}
	})
	return d, err
}

// defaultRetries bounds CreateDatabase's re-runs after losing the race for a tenant's default database (one
// re-run settles it: the winner has committed; more only under a stream of concurrent first creates).
const defaultRetries = 3

// The registry's name domains (0002_registry.sql); TestRegistryNamesMatchTheDomains keeps them in step.
const (
	tenantPattern = `^[a-z0-9][a-z0-9-]{0,62}$`
	namePattern   = `^[a-z0-9][a-z0-9_-]{0,55}$`
)

// NamePattern is the database_name domain: a database's name inside its tenant, and an app's name (an app's
// database is tenant ":" app; ADR 0011).
const NamePattern = namePattern

var (
	tenantRE = regexp.MustCompile(tenantPattern)
	nameRE   = regexp.MustCompile(namePattern)
)

// ValidTenant reports whether s is a valid tenant id (the tenant_id domain).
func ValidTenant(s string) bool { return tenantRE.MatchString(s) }

// ValidName reports whether s is a valid database name inside a tenant (the database_name domain).
func ValidName(s string) bool { return nameRE.MatchString(s) }

// GetDatabase returns key's registry row; a missing key matches owner.ErrNotFound.
func (p *Placement) GetDatabase(ctx context.Context, key string) (Database, error) {
	var d Database
	err := p.call(ctx, "read database", key, retryRead, func(ctx context.Context) error {
		rows, err := p.pool.Query(ctx, q.getDatabase, key)
		if err != nil {
			return err
		}
		d, err = pgx.CollectExactlyOneRow(rows, scanDatabase)
		if errors.Is(err, pgx.ErrNoRows) {
			return owner.ErrNotFound
		}
		return err
	})
	return d, err
}

// SetDatabaseState moves key from state `from` to `to` (target: the move's destination, only with "moving").
// The row not being in `from` matches owner.ErrConflict.
func (p *Placement) SetDatabaseState(ctx context.Context, key, from, to, target string) error {
	return p.call(ctx, "set the lifecycle state of", key, retryWrite, func(ctx context.Context) error {
		var k string
		err := p.pool.QueryRow(ctx, q.setDatabaseState, key, from, to, target).Scan(&k)
		if errors.Is(err, pgx.ErrNoRows) {
			return owner.ErrConflict
		}
		return err
	})
}

// Pending returns up to limit databases in "creating", "moving" or "dropping" for longer than olderThan,
// oldest first (pending.sql): unfinished flows to resume.
func (p *Placement) Pending(ctx context.Context, olderThan time.Duration, limit int) ([]Database, error) {
	return list(p, ctx, "list pending creates and drops", q.pending, scanDatabase, olderThan, limit)
}

// DropPlacement deletes key's placement row and its moves rows iff the row is still expect (a MOVING row).
// Otherwise owner.ErrConflict and nothing changes.
func (p *Placement) DropPlacement(ctx context.Context, key string, expect owner.Place) error {
	return p.call(ctx, "drop the placement of", key, retryWrite, func(ctx context.Context) error {
		var n int64
		if err := p.pool.QueryRow(ctx, q.dropPlacement, key, expect.Owner, int64(expect.Epoch), int64(expect.Gen)).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return owner.ErrConflict
		}
		return nil
	})
}
