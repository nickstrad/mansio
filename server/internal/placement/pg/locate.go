package pg

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/owner"
)

// The locator's reads and the API-key registry (CP-5; schema 0003, 0004).

// Location is one database as the locator sees it: registry state, placement row and owner endpoint.
type Location struct {
	Intent   string // the registry state, "" without a registry row
	Target   string // the registry's move target (Intent "moving"), else ""
	Found    bool   // a placement row exists; the fields below are set only then
	Place    owner.Place
	Endpoint string // the owner VM's query endpoint
}

// Locate reads key's location (locate.sql). Read on whatever server this adapter's DSN names: open a second
// adapter on a replica's DSN for replica reads.
func (p *Placement) Locate(ctx context.Context, key string) (Location, error) {
	var l Location
	err := p.call(ctx, "locate", key, retryRead, func(ctx context.Context) error {
		var vm, endpoint, state *string
		var epoch, gen *int64
		if err := p.pool.QueryRow(ctx, q.locate, key).Scan(&l.Intent, &l.Target, &vm, &endpoint, &epoch, &gen, &state); err != nil {
			return err
		}
		if vm == nil {
			return nil
		}
		pl, err := decodePlace(*vm, *epoch, *gen, *state)
		if err != nil {
			return err
		}
		l.Found, l.Place = true, pl
		if endpoint != nil {
			l.Endpoint = *endpoint
		}
		return nil
	})
	return l, err
}

// APIKey is one row of api_keys.
type APIKey struct {
	ID        string
	Tenant    string
	Principal string
	Perms     uint8 // ticket.Perm
	Hash      [32]byte
	Revoked   bool
}

// GetAPIKey reads key id's row; a missing id matches owner.ErrNotFound.
func (p *Placement) GetAPIKey(ctx context.Context, id string) (APIKey, error) {
	k := APIKey{ID: id}
	err := p.call(ctx, "read api key", id, retryRead, func(ctx context.Context) error {
		var perms int16
		var hash []byte
		err := p.pool.QueryRow(ctx, q.apiKey, id).Scan(&k.Tenant, &k.Principal, &perms, &hash, &k.Revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return owner.ErrNotFound
		}
		if err != nil {
			return err
		}
		if len(hash) != sha256.Size {
			return fmt.Errorf("%w: stored hash is %d bytes", ErrInvariant, len(hash))
		}
		k.Perms, k.Hash = uint8(perms), [32]byte(hash)
		return nil
	})
	return k, err
}

// PrincipalPattern is the api_keys.principal check (0004_api_keys.sql).
const PrincipalPattern = `^[a-z0-9][a-z0-9._@-]{0,127}$`

var principalRE = regexp.MustCompile(PrincipalPattern)

// ValidPrincipal reports whether s is a valid principal name.
func ValidPrincipal(s string) bool { return principalRE.MatchString(s) }

// CreateAPIKey stores a key's row (creating the tenant if new). Invalid fields match owner.ErrInvalid
// (nothing sent); a duplicate id matches ErrInvariant.
func (p *Placement) CreateAPIKey(ctx context.Context, k APIKey) error {
	if !ValidTenant(k.Tenant) || !ValidPrincipal(k.Principal) || k.Perms < 1 || k.Perms > 3 || len(k.ID) != 16 {
		return errs.Wrap(module, owner.ErrInvalid, "cannot create api key: tenant must match %s, principal %s, perms 1..3; nothing sent",
			tenantPattern, PrincipalPattern)
	}
	return p.call(ctx, "create api key", k.ID, retryWrite, func(ctx context.Context) error {
		var id string
		return p.pool.QueryRow(ctx, q.createAPIKey, k.ID, k.Tenant, k.Principal, k.Hash[:], int16(k.Perms)).Scan(&id)
	})
}

// RevokeAPIKey revokes key id (idempotent); a missing id matches owner.ErrNotFound.
func (p *Placement) RevokeAPIKey(ctx context.Context, id string) error {
	return p.call(ctx, "revoke api key", id, "safe to retry", func(ctx context.Context) error {
		var got string
		err := p.pool.QueryRow(ctx, q.revokeAPIKey, id).Scan(&got)
		if errors.Is(err, pgx.ErrNoRows) {
			return owner.ErrNotFound
		}
		return err
	})
}
