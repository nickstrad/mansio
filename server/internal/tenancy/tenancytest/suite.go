package tenancytest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/tenancy"
)

// Subject is one Store under test, with a way to add a legacy key (the migration's job, not the Store's).
type Subject struct {
	Store     tenancy.Store
	AddLegacy func(t *testing.T, id, tenant string, hash [32]byte)
}

// RunStoreSuite checks the tenancy.Store contract against fresh subjects (one per subtest).
func RunStoreSuite(t *testing.T, fresh func(t *testing.T) Subject) {
	ctx := context.Background()
	n := 0
	uuid := func() string { n++; return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
	newTenant := func(t *testing.T, s tenancy.Store, slug string) tenancy.Tenant {
		t.Helper()
		tn, err := s.CreateTenant(ctx, tenancy.Tenant{ID: uuid(), Slug: slug, Name: "Name " + slug, Active: true})
		if err != nil {
			t.Fatalf("CreateTenant: %v", err)
		}
		return tn
	}
	is := func(t *testing.T, err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	}
	ok := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Run("tenants", func(t *testing.T) {
		s := fresh(t).Store
		a := newTenant(t, s, "alpha")
		if !a.Active || a.Slug != "alpha" || a.Name != "Name alpha" || a.Created.IsZero() {
			t.Fatalf("created %+v", a)
		}
		_, err := s.CreateTenant(ctx, tenancy.Tenant{ID: uuid(), Slug: "alpha", Name: "dup"})
		is(t, err, tenancy.ErrConflict)
		b := newTenant(t, s, "beta")
		got, err := s.GetTenant(ctx, a.ID)
		ok(t, err)
		if got.ID != a.ID || got.Slug != "alpha" {
			t.Fatalf("got %+v", got)
		}
		_, err = s.GetTenant(ctx, uuid())
		is(t, err, tenancy.ErrNotFound)
		list, err := s.ListTenants(ctx, 10)
		ok(t, err)
		if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
			t.Fatalf("list %+v", list)
		}
		if list, _ := s.ListTenants(ctx, 1); len(list) != 1 {
			t.Fatalf("limit 1: %d", len(list))
		}
		d, err := s.DeactivateTenant(ctx, a.ID)
		ok(t, err)
		if d.Active {
			t.Fatal("still active")
		}
		_, err = s.DeactivateTenant(ctx, uuid())
		is(t, err, tenancy.ErrNotFound)
	})

	t.Run("keys", func(t *testing.T) {
		sub := fresh(t)
		s := sub.Store
		a, b := newTenant(t, s, "a"), newTenant(t, s, "b")
		h := sha256.Sum256([]byte("secret"))
		k, err := s.CreateKey(ctx, tenancy.NewKey{Key: tenancy.Key{ID: "0123456789abcdef", Tenant: a.ID, Name: "default"}, Hash: h})
		ok(t, err)
		if k.ID != "0123456789abcdef" || k.Tenant != a.ID || k.Name != "default" || k.Created.IsZero() {
			t.Fatalf("key %+v", k)
		}
		_, err = s.CreateKey(ctx, tenancy.NewKey{Key: tenancy.Key{ID: "0123456789abcdef", Tenant: a.ID, Name: "x"}, Hash: h})
		is(t, err, tenancy.ErrConflict)
		_, err = s.CreateKey(ctx, tenancy.NewKey{Key: tenancy.Key{ID: "1123456789abcdef", Tenant: uuid(), Name: "x"}, Hash: h})
		is(t, err, tenancy.ErrNotFound)
		got, err := s.KeyByID(ctx, k.ID)
		ok(t, err)
		if got.Tenant != a.ID || got.Hash != h || got.Revoked || !got.TenantActive {
			t.Fatalf("stored %+v", got)
		}
		_, err = s.KeyByID(ctx, "ffffffffffffffff")
		is(t, err, tenancy.ErrNotFound)
		ok(t, s.RevokeKey(ctx, b.ID, k.ID)) // another tenant's: no change
		if got, _ := s.KeyByID(ctx, k.ID); got.Revoked {
			t.Fatal("revoked through another tenant")
		}
		ok(t, s.RevokeKey(ctx, a.ID, k.ID))
		ok(t, s.RevokeKey(ctx, a.ID, k.ID)) // idempotent
		ok(t, s.RevokeKey(ctx, a.ID, "eeeeeeeeeeeeeeee"))
		if got, _ := s.KeyByID(ctx, k.ID); !got.Revoked {
			t.Fatal("not revoked")
		}
		k2, err := s.CreateKey(ctx, tenancy.NewKey{Key: tenancy.Key{ID: "2123456789abcdef", Tenant: b.ID, Name: "second"}, Hash: h})
		ok(t, err)
		legacy := sha256.Sum256([]byte("mansio_legacy"))
		lid := uuid()
		sub.AddLegacy(t, lid, b.ID, legacy)
		lk, err := s.KeyByLegacyHash(ctx, legacy)
		ok(t, err)
		if lk.Tenant != b.ID || lk.Hash != legacy || lk.Revoked || !lk.TenantActive {
			t.Fatalf("legacy %+v", lk)
		}
		_, err = s.KeyByLegacyHash(ctx, h)
		is(t, err, tenancy.ErrNotFound)
		_, err = s.KeyByID(ctx, lid)
		is(t, err, tenancy.ErrNotFound)
		_, err = s.DeactivateTenant(ctx, b.ID)
		ok(t, err)
		for _, f := range []func() (tenancy.StoredKey, error){
			func() (tenancy.StoredKey, error) { return s.KeyByID(ctx, k2.ID) },
			func() (tenancy.StoredKey, error) { return s.KeyByLegacyHash(ctx, legacy) },
		} {
			got, err := f()
			ok(t, err)
			if !got.Revoked || got.TenantActive {
				t.Fatalf("after deactivate %+v", got)
			}
		}
		_, err = s.CreateKey(ctx, tenancy.NewKey{Key: tenancy.Key{ID: "3123456789abcdef", Tenant: b.ID, Name: "x"}, Hash: h})
		is(t, err, tenancy.ErrDeactivated)
	})

	t.Run("users and sessions", func(t *testing.T) {
		s := fresh(t).Store
		a, b := newTenant(t, s, "a"), newTenant(t, s, "b")
		now := time.Now().Truncate(time.Microsecond)
		hash := "00000000000000000000000000000000:" + fmt.Sprintf("%0128d", 7)
		u := tenancy.User{ID: "user1", Email: "one@example.test", Name: "One", Created: now, Updated: now}
		s1 := tenancy.Session{Hash: sha256.Sum256([]byte("token1")), User: "user1", Expires: now.Add(time.Hour)}
		ok(t, s.CreateUser(ctx, u, hash, s1))
		err := s.CreateUser(ctx, tenancy.User{ID: "user2", Email: "one@example.test", Name: "Two", Created: now, Updated: now}, hash,
			tenancy.Session{Hash: sha256.Sum256([]byte("token2")), User: "user2", Expires: now.Add(time.Hour)})
		is(t, err, tenancy.ErrConflict)
		got, h, err := s.Password(ctx, "one@example.test")
		ok(t, err)
		if got.ID != "user1" || got.Email != u.Email || got.Name != "One" || h != hash {
			t.Fatalf("password %+v %q", got, h)
		}
		_, _, err = s.Password(ctx, "nobody@example.test")
		is(t, err, tenancy.ErrNotFound)
		_, err = s.SessionTenant(ctx, s1.Hash, now)
		is(t, err, tenancy.ErrNotFound) // no membership
		is(t, s.Link(ctx, "nobody@example.test", a.ID), tenancy.ErrNotFound)
		is(t, s.Link(ctx, u.Email, uuid()), tenancy.ErrNotFound)
		ok(t, s.Link(ctx, u.Email, a.ID))
		ok(t, s.Link(ctx, u.Email, a.ID)) // idempotent
		tn, err := s.SessionTenant(ctx, s1.Hash, now)
		ok(t, err)
		if tn != a.ID {
			t.Fatalf("tenant %s", tn)
		}
		_, err = s.SessionTenant(ctx, s1.Hash, now.Add(time.Hour)) // expired: the bound is exclusive
		is(t, err, tenancy.ErrNotFound)
		_, err = s.SessionTenant(ctx, sha256.Sum256([]byte("other")), now)
		is(t, err, tenancy.ErrNotFound)
		s2 := tenancy.Session{Hash: sha256.Sum256([]byte("token3")), User: "user1", Expires: now.Add(time.Hour)}
		ok(t, s.CreateSession(ctx, s2))
		if tn, err := s.SessionTenant(ctx, s2.Hash, now); err != nil || tn != a.ID {
			t.Fatalf("second session: %s %v", tn, err)
		}
		ok(t, s.Link(ctx, u.Email, b.ID)) // two memberships: refused
		_, err = s.SessionTenant(ctx, s1.Hash, now)
		is(t, err, tenancy.ErrNotFound)
		_, err = s.DeactivateTenant(ctx, b.ID)
		ok(t, err)
		is(t, s.Link(ctx, u.Email, b.ID), tenancy.ErrDeactivated)
	})
}
