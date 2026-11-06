package pg

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"github.com/nickstrad/mansio/server/internal/tenancy"
)

// testdata/mansio holds copies of the Deno platform's migrations (platform/tenants/migrations 001 and 002).

// Better Auth 1.7.7's hash of "correct horse battery staple" (tenancy's password_test.go).
const betterAuthHash = "5f1a4a39142f8680f6acf0644b323600:ed91a4c3469e54b1200519ddfb9efa577390d464bdec2c8a1d8902c44f44edc9d3aadc845e6f77a4d7b049059f8ef4d232cdcd62f0614d5c6695a41ce3ceb033"

// mansioSource makes a schema with the Deno platform's tables and one of everything in them.
func mansioSource(t *testing.T, legacyKey, session string) string {
	t.Helper()
	schema := freshSchema(t)
	c := adminConn(t, schema)
	for _, f := range []string{"testdata/mansio/001_baseline.up.sql", "testdata/mansio/002_auth.up.sql"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	hash := sha256.Sum256([]byte(legacyKey))
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`insert into tenants (id, slug, name, status) values ('11111111-1111-4111-8111-111111111111', 'acme', 'Acme', 'active'),
		   ('22222222-2222-4222-8222-222222222222', 'gone', 'Gone', 'deactivated')`, nil},
		{`insert into api_keys (id, tenant_id, key_hash, prefix, name) values ('33333333-3333-4333-8333-333333333333',
		   '11111111-1111-4111-8111-111111111111', $1, 'mansio_', 'default')`, []any{hex.EncodeToString(hash[:])}},
		{`insert into api_keys (id, tenant_id, key_hash, prefix, name, revoked_at) values ('44444444-4444-4444-8444-444444444444',
		   '11111111-1111-4111-8111-111111111111', $1, 'mansio_', 'old', now())`, []any{hex.EncodeToString(make([]byte, 32))}},
		{`insert into "user" (id, name, email, "emailVerified") values ('UserIdUserIdUserIdUserIdUserId01', 'Ann', 'Ann@Example.test', false)`, nil},
		{`insert into account (id, "accountId", "providerId", "userId", password, "updatedAt") values
		   ('a1', 'UserIdUserIdUserIdUserIdUserId01', 'credential', 'UserIdUserIdUserIdUserIdUserId01', $1, now())`, []any{betterAuthHash}},
		{`insert into session (id, "expiresAt", token, "updatedAt", "userId") values
		   ('s1', now() + interval '1 day', $1, now(), 'UserIdUserIdUserIdUserIdUserId01'),
		   ('s2', now() - interval '1 day', 'expired-token', now(), 'UserIdUserIdUserIdUserIdUserId01')`, []any{session}},
		{`insert into memberships (user_id, tenant_id) values ('UserIdUserIdUserIdUserIdUserId01', '11111111-1111-4111-8111-111111111111')`, nil},
	} {
		if _, err := c.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}
	return schema
}

func TestImportMansio(t *testing.T) {
	legacy := "mansio_" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ" // 43 base64url characters, canonical
	session := "SessionTokenSessionTokenSession1"
	src := mansioSource(t, legacy, session)
	p, target := newPlacement(t, nil)
	doc, err := ReadMansio(ctx, pgURL(t), src)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.HasAuth {
		t.Fatal("the source's auth tables were not found")
	}
	want := ImportCounts{Tenants: 2, APIKeys: 2, Users: 1, Passwords: 1, Sessions: 1, Memberships: 1}
	dry, err := p.ImportMansio(ctx, doc, true)
	if err != nil || dry != want {
		t.Fatalf("dry run: %+v %v, want %+v", dry, err, want)
	}
	var n int
	if err := adminConn(t, target).QueryRow(ctx, "select count(*) from tenants").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the dry run left %d tenants (%v)", n, err)
	}
	got, err := p.ImportMansio(ctx, doc, false)
	if err != nil || got != want {
		t.Fatalf("import: %+v %v, want %+v", got, err, want)
	}
	if again, err := p.ImportMansio(ctx, doc, false); err != nil || again != (ImportCounts{}) {
		t.Fatalf("re-run: %+v %v, want nothing new", again, err)
	}

	// What was imported works through the Go auth: the legacy key, the password, the session.
	svc, err := tenancy.New(p.Tenancy(), tenancy.Config{Mode: tenancy.Sessions})
	if err != nil {
		t.Fatal(err)
	}
	const acme = "11111111-1111-4111-8111-111111111111"
	if pr, err := svc.Authenticate(ctx, legacy, "app"); err != nil || pr.Tenant != acme {
		t.Fatalf("legacy key: %+v %v", pr, err)
	}
	if pr, err := svc.Authenticate(ctx, session, "app"); err != nil || pr.Tenant != acme {
		t.Fatalf("migrated session: %+v %v", pr, err)
	}
	if _, err := svc.Authenticate(ctx, "expired-token", "app"); err == nil {
		t.Fatal("an expired session was imported")
	}
	u, tok, err := svc.SignIn(ctx, "ann@example.test", "correct horse battery staple")
	if err != nil || u.ID != "UserIdUserIdUserIdUserIdUserId01" || tok == "" {
		t.Fatalf("sign-in with the migrated password: %+v %v", u, err)
	}
	gone, err := svc.GetTenant(ctx, "22222222-2222-4222-8222-222222222222")
	if err != nil || gone.Active || gone.Slug != "gone" {
		t.Fatalf("deactivated tenant: %+v %v", gone, err)
	}
}

// A source without the auth tables (TENANCY=api-key only) imports tenants and keys.
func TestImportMansioWithoutAuth(t *testing.T) {
	schema := freshSchema(t)
	b, err := os.ReadFile("testdata/mansio/001_baseline.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	c := adminConn(t, schema)
	if _, err := c.Exec(ctx, string(b)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `insert into tenants (id, slug) values ('11111111-1111-4111-8111-111111111111', 'acme')`); err != nil {
		t.Fatal(err)
	}
	doc, err := ReadMansio(ctx, pgURL(t), schema)
	if err != nil || doc.HasAuth {
		t.Fatalf("read: %v, has auth %v", err, doc.HasAuth)
	}
	p, _ := newPlacement(t, nil)
	if got, err := p.ImportMansio(ctx, doc, false); err != nil || got != (ImportCounts{Tenants: 1}) {
		t.Fatalf("import: %+v %v", got, err)
	}
}
