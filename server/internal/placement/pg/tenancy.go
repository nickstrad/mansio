package pg

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nickstrad/mansio/server/internal/tenancy"
)

// The tenancy store (schema 0007): tenancy.Store over the control plane's tables. Errors match the tenancy sentinels:
// an unavailable authority is tenancy.ErrUnavailable (and still ErrUnavailable).

// Tenancy adapts p to tenancy.Store.
func (p *Placement) Tenancy() tenancy.Store { return tenancyStore{p} }

type tenancyStore struct{ p *Placement }

var _ tenancy.Store = tenancyStore{}

// tcall is p.call with the result marked for package tenancy.
func (s tenancyStore) tcall(ctx context.Context, what, key, retry string, fn func(ctx context.Context) error) error {
	err := s.p.call(ctx, what, key, retry, fn)
	if errors.Is(err, ErrUnavailable) {
		return marked{err, tenancy.ErrUnavailable}
	}
	return err
}

// uniqueOn reports whether err is a unique violation of constraint.
func uniqueOn(err error, constraint string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == constraint
}

func scanTenant(row pgx.Row) (tenancy.Tenant, error) {
	var t tenancy.Tenant
	err := row.Scan(&t.ID, &t.Slug, &t.Name, &t.Active, &t.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, tenancy.ErrNotFound
	}
	return t, err
}

func (s tenancyStore) CreateTenant(ctx context.Context, t tenancy.Tenant) (tenancy.Tenant, error) {
	var out tenancy.Tenant
	err := s.tcall(ctx, "create tenant", t.ID, retryWrite, func(ctx context.Context) error {
		var err error
		out, err = scanTenant(s.p.pool.QueryRow(ctx, q.tenancyCreateTenant, t.ID, t.Slug, t.Name))
		if uniqueOn(err, "tenants_slug_key") {
			return fmt.Errorf("%w: slug %q is taken", tenancy.ErrConflict, t.Slug)
		}
		return err
	})
	return out, err
}

func (s tenancyStore) ListTenants(ctx context.Context, limit int) ([]tenancy.Tenant, error) {
	var out []tenancy.Tenant
	err := s.tcall(ctx, "list tenants", "", retryRead, func(ctx context.Context) error {
		rows, err := s.p.pool.Query(ctx, q.tenancyListTenants, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (tenancy.Tenant, error) { return scanTenant(r) })
		return err
	})
	return out, err
}

func (s tenancyStore) GetTenant(ctx context.Context, id string) (tenancy.Tenant, error) {
	var out tenancy.Tenant
	err := s.tcall(ctx, "read tenant", id, retryRead, func(ctx context.Context) (err error) {
		out, err = scanTenant(s.p.pool.QueryRow(ctx, q.tenancyGetTenant, id))
		return err
	})
	return out, err
}

func (s tenancyStore) DeactivateTenant(ctx context.Context, id string) (tenancy.Tenant, error) {
	var out tenancy.Tenant
	err := s.tcall(ctx, "deactivate tenant", id, "it is idempotent: safe to retry", func(ctx context.Context) (err error) {
		out, err = scanTenant(s.p.pool.QueryRow(ctx, q.tenancyDeactivateTenant, id))
		return err
	})
	return out, err
}

func (s tenancyStore) CreateKey(ctx context.Context, k tenancy.NewKey) (tenancy.Key, error) {
	out := tenancy.Key{}
	err := s.tcall(ctx, "create api key for tenant", k.Tenant, retryWrite, func(ctx context.Context) error {
		var status, id, tenant, name *string
		var created *time.Time
		err := s.p.pool.QueryRow(ctx, q.tenancyCreateKey, k.ID, k.Tenant, k.Hash[:], k.Name).Scan(&status, &id, &tenant, &name, &created)
		switch {
		case uniqueOn(err, "api_keys_pkey"):
			return fmt.Errorf("%w: key id %s is taken", tenancy.ErrConflict, k.ID)
		case err != nil:
			return err
		case status == nil:
			return tenancy.ErrNotFound
		case *status != "active":
			return tenancy.ErrDeactivated
		case id == nil || tenant == nil || name == nil || created == nil:
			return fmt.Errorf("%w: an active tenant's key was not inserted", ErrInvariant)
		}
		out = tenancy.Key{ID: *id, Tenant: *tenant, Name: *name, Created: *created}
		return nil
	})
	return out, err
}

func (s tenancyStore) RevokeKey(ctx context.Context, tenant, id string) error {
	return s.tcall(ctx, "revoke api key", id, "it is idempotent: safe to retry", func(ctx context.Context) error {
		_, err := s.p.pool.Exec(ctx, q.tenancyRevokeKey, id, tenant)
		return err
	})
}

func (s tenancyStore) scanKey(ctx context.Context, query, what string, arg any) (tenancy.StoredKey, error) {
	var k tenancy.StoredKey
	err := s.tcall(ctx, what, "", retryRead, func(ctx context.Context) error {
		var hash []byte
		err := s.p.pool.QueryRow(ctx, query, arg).Scan(&k.Tenant, &hash, &k.Revoked, &k.TenantActive)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return tenancy.ErrNotFound
		case err != nil:
			return err
		case len(hash) != sha256.Size:
			return fmt.Errorf("%w: stored hash is %d bytes", ErrInvariant, len(hash))
		}
		k.Hash = [32]byte(hash)
		return nil
	})
	return k, err
}

func (s tenancyStore) KeyByID(ctx context.Context, id string) (tenancy.StoredKey, error) {
	return s.scanKey(ctx, q.tenancyKeyByID, "read api key", id)
}

func (s tenancyStore) KeyByLegacyHash(ctx context.Context, hash [32]byte) (tenancy.StoredKey, error) {
	return s.scanKey(ctx, q.tenancyKeyByLegacyHash, "read legacy api key", hash[:])
}

func (s tenancyStore) CreateUser(ctx context.Context, u tenancy.User, password string, sess tenancy.Session) error {
	return s.tcall(ctx, "create user", "", retryWrite, func(ctx context.Context) error {
		var id string
		err := s.p.pool.QueryRow(ctx, q.tenancyCreateUser, u.ID, u.Email, u.Name, u.Created, password, sess.Hash[:], sess.Expires).Scan(&id)
		if uniqueOn(err, "auth_users_email_key") {
			return fmt.Errorf("%w: the email is taken", tenancy.ErrConflict)
		}
		return err
	})
}

func (s tenancyStore) Password(ctx context.Context, email string) (tenancy.User, string, error) {
	var u tenancy.User
	var hash string
	err := s.tcall(ctx, "read password", "", retryRead, func(ctx context.Context) error {
		err := s.p.pool.QueryRow(ctx, q.tenancyPassword, email).Scan(&u.ID, &u.Email, &u.Name, &u.Verified, &u.Created, &u.Updated, &hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return tenancy.ErrNotFound
		}
		return err
	})
	return u, hash, err
}

func (s tenancyStore) CreateSession(ctx context.Context, sess tenancy.Session) error {
	return s.tcall(ctx, "create session", "", retryWrite, func(ctx context.Context) error {
		_, err := s.p.pool.Exec(ctx, q.tenancyCreateSession, sess.Hash[:], sess.User, sess.Expires)
		return err
	})
}

func (s tenancyStore) SessionTenant(ctx context.Context, hash [32]byte, now time.Time) (string, error) {
	var tenant string
	err := s.tcall(ctx, "read session", "", retryRead, func(ctx context.Context) error {
		var id *string
		var n int64
		var active bool
		if err := s.p.pool.QueryRow(ctx, q.tenancySessionTenant, hash[:], now).Scan(&id, &n, &active); err != nil {
			return err
		}
		if n != 1 || !active || id == nil {
			return tenancy.ErrNotFound
		}
		tenant = *id
		return nil
	})
	return tenant, err
}

func (s tenancyStore) Link(ctx context.Context, email, tenant string) error {
	return s.tcall(ctx, "link user to tenant", tenant, "it is idempotent: safe to retry", func(ctx context.Context) error {
		var users int64
		var status *string
		if err := s.p.pool.QueryRow(ctx, q.tenancyLink, email, tenant).Scan(&users, &status); err != nil {
			return err
		}
		switch {
		case users == 0 || status == nil:
			return tenancy.ErrNotFound
		case *status != "active":
			return tenancy.ErrDeactivated
		}
		return nil
	})
}
