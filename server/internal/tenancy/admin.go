package tenancy

import (
	"context"
	crand "crypto/rand"
	"io"
	"regexp"

	"github.com/nickstrad/mansio/server/internal/control/locator"
	"github.com/nickstrad/mansio/server/internal/errs"
)

// The admin operations behind /admin/tenants (scripts/tenants.ts's API). Each validates its input, then makes
// one Store call; every change clears the credential cache, whatever its outcome.

// IssuedKey is a new key: its metadata and its plaintext, returned once and never stored.
type IssuedKey struct {
	Key
	Plaintext string
}

// CreateTenant creates an active tenant and its first key, "default".
func (s *Service) CreateTenant(ctx context.Context, name, slug string) (Tenant, IssuedKey, error) {
	if err := checkText("name", name); err != nil {
		return Tenant{}, IssuedKey{}, err
	}
	if err := checkText("slug", slug); err != nil {
		return Tenant{}, IssuedKey{}, err
	}
	id, err := s.newUUID()
	if err != nil {
		return Tenant{}, IssuedKey{}, err
	}
	t, err := s.store.CreateTenant(ctx, Tenant{ID: id, Slug: slug, Name: name, Active: true})
	if err != nil {
		return Tenant{}, IssuedKey{}, storeError(err, "cannot create tenant")
	}
	k, err := s.IssueKey(ctx, t.ID, "default")
	if err != nil {
		return Tenant{}, IssuedKey{}, errs.Wrap(module, err, "tenant %s was created without a key; issue one", t.ID)
	}
	return t, k, nil
}

// ListTenants returns every tenant (up to MaxTenants), oldest first.
func (s *Service) ListTenants(ctx context.Context) ([]Tenant, error) {
	ts, err := s.store.ListTenants(ctx, s.cfg.MaxTenants)
	if err != nil {
		return nil, storeError(err, "cannot list tenants")
	}
	return ts, nil
}

// GetTenant returns one tenant.
func (s *Service) GetTenant(ctx context.Context, id string) (Tenant, error) {
	if err := checkTenantID(id); err != nil {
		return Tenant{}, err
	}
	t, err := s.store.GetTenant(ctx, id)
	if err != nil {
		return Tenant{}, storeError(err, "cannot read tenant")
	}
	return t, nil
}

// DeactivateTenant deactivates a tenant and revokes its keys (idempotent).
func (s *Service) DeactivateTenant(ctx context.Context, id string) (Tenant, error) {
	if err := checkTenantID(id); err != nil {
		return Tenant{}, err
	}
	defer s.invalidate()
	t, err := s.store.DeactivateTenant(ctx, id)
	if err != nil {
		return Tenant{}, storeError(err, "cannot deactivate tenant")
	}
	return t, nil
}

// IssueKey issues a new-format key for an active tenant.
func (s *Service) IssueKey(ctx context.Context, tenant, name string) (IssuedKey, error) {
	if err := checkTenantID(tenant); err != nil {
		return IssuedKey{}, err
	}
	if err := checkText("name", name); err != nil {
		return IssuedKey{}, err
	}
	plain, id, hash, err := locator.NewAPIKey(s.cfg.Rand)
	if err != nil {
		return IssuedKey{}, errs.Wrap(module, err, "cannot issue a key; nothing changed")
	}
	defer s.invalidate()
	k, err := s.store.CreateKey(ctx, NewKey{Key: Key{ID: id, Tenant: tenant, Name: name}, Hash: hash})
	if err != nil {
		return IssuedKey{}, storeError(err, "cannot issue a key")
	}
	return IssuedKey{Key: k, Plaintext: plain}, nil
}

// RevokeKey revokes key id if it belongs to tenant. Idempotent: a key of another tenant, or none, is left as is,
// so a caller that does not know the key's tenant may try each (scripts/tenants.ts revoke-key).
func (s *Service) RevokeKey(ctx context.Context, tenant, id string) error {
	if err := checkTenantID(tenant); err != nil {
		return err
	}
	if !keyIDRE.MatchString(id) {
		return errs.Wrap(module, ErrInvalid, "key id must be 16 hex digits or a lowercase uuid")
	}
	if _, err := s.GetTenant(ctx, tenant); err != nil { // the URL's tenant must exist, as on the Deno platform
		return err
	}
	defer s.invalidate()
	if err := s.store.RevokeKey(ctx, tenant, id); err != nil {
		return storeError(err, "cannot revoke a key")
	}
	return nil
}

// Link makes the user with email a member of tenant (scripts/auth.ts users link).
func (s *Service) Link(ctx context.Context, email, tenant string) error {
	if err := checkTenantID(tenant); err != nil {
		return err
	}
	email, err := checkEmail(email)
	if err != nil {
		return err
	}
	defer s.invalidate()
	if err := s.store.Link(ctx, email, tenant); err != nil {
		return storeError(err, "cannot link the user")
	}
	return nil
}

var keyIDRE = regexp.MustCompile(`^(?:[0-9a-f]{16}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

func checkTenantID(id string) error {
	if !ValidTenantID(id) {
		return errs.Wrap(module, ErrInvalid, "tenant id must be a canonical lowercase uuid")
	}
	return nil
}

// HashPassword returns password's Better Auth hash, salted from rand (nil: crypto/rand). For tools and tests that
// write password rows (the import's tests); the Service hashes its own.
func HashPassword(rand io.Reader, password string) (string, error) {
	if rand == nil {
		rand = crand.Reader
	}
	return hashPassword(rand, password)
}
