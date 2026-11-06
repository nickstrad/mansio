package pg

import (
	"errors"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/tenancy"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancytest"
)

func TestTenancyStoreContract(t *testing.T) {
	tenancytest.RunStoreSuite(t, func(t *testing.T) tenancytest.Subject {
		p, schema := newPlacement(t, nil)
		c := adminConn(t, schema)
		return tenancytest.Subject{Store: p.Tenancy(), AddLegacy: func(t *testing.T, id, tenant string, hash [32]byte) {
			t.Helper()
			if _, err := c.Exec(ctx, `insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms, legacy, name)
				values ($1, $2, $1, $3, 3, true, 'legacy')`, id, tenant, hash[:]); err != nil {
				t.Fatal(err)
			}
		}}
	})
}

// An unreachable Postgres is tenancy.ErrUnavailable (503, retry), never a refusal or a bug.
func TestTenancyStoreUnavailable(t *testing.T) {
	pgURL(t)
	p, err := Open(Config{DSN: "postgres://nobody@127.0.0.1:1/none?sslmode=disable", ConnectTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	s := p.Tenancy()
	if _, err := s.KeyByID(ctx, "0123456789abcdef"); !errors.Is(err, tenancy.ErrUnavailable) || errors.Is(err, tenancy.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.SessionTenant(ctx, [32]byte{}, time.Now()); !errors.Is(err, tenancy.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
