package pg

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
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
	const acme = "11111111-1111-4111-8111-111111111111"
	svc, err := tenancy.New(p.Tenancy(), tenancy.Config{Mode: tenancy.Sessions, Apps: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	// A key issued here (new format) for acme, to see what a carried deactivation does to it.
	native, err := svc.IssueKey(ctx, acme, "native")
	if err != nil {
		t.Fatal(err)
	}

	// What was imported works through the Go auth: the legacy key, the password, the session.
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

	// One-way changes at the source reach a re-run: a revoked key, a changed password, a deactivated tenant
	// (which also revokes the keys issued here).
	sc := adminConn(t, src)
	newHash, err := tenancy.HashPassword(nil, "a brand new password")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`update api_keys set revoked_at = now() where id = '33333333-3333-4333-8333-333333333333'`, nil},
		{`update account set password = $1 where id = 'a1'`, []any{newHash}},
	} {
		if _, err := sc.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	doc, err = ReadMansio(ctx, pgURL(t), src)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := p.ImportMansio(ctx, doc, false); err != nil || got != (ImportCounts{KeysRevoked: 1, PasswordsChanged: 1}) {
		t.Fatalf("re-run after revoke and password change: %+v %v", got, err)
	}
	svc, _ = tenancy.New(p.Tenancy(), tenancy.Config{Mode: tenancy.Sessions, Apps: []string{"app"}}) // another process: no cache
	if _, err := svc.Authenticate(ctx, legacy, "app"); err == nil {
		t.Fatal("the key revoked at the source still works")
	}
	if _, _, err := svc.SignIn(ctx, "ann@example.test", "a brand new password"); err != nil {
		t.Fatalf("the changed password: %v", err)
	}
	if _, err := sc.Exec(ctx, `update tenants set status = 'deactivated' where id = $1`, acme); err != nil {
		t.Fatal(err)
	}
	doc, _ = ReadMansio(ctx, pgURL(t), src)
	if got, err := p.ImportMansio(ctx, doc, false); err != nil || got != (ImportCounts{TenantsDeactivated: 1, KeysRevoked: 1}) {
		t.Fatalf("re-run after deactivation: %+v %v", got, err)
	}
	svc, _ = tenancy.New(p.Tenancy(), tenancy.Config{Mode: tenancy.Sessions, Apps: []string{"app"}})
	if tn, _ := svc.GetTenant(ctx, acme); tn.Active {
		t.Fatal("the tenant deactivated at the source is still active")
	}
	if _, err := svc.Authenticate(ctx, native.Plaintext, "app"); err == nil {
		t.Fatal("a key of the deactivated tenant still works")
	}
	// Never the other way: reactivating at the source does not reactivate here.
	sc.Exec(ctx, `update tenants set status = 'active' where id = $1`, acme)
	doc, _ = ReadMansio(ctx, pgURL(t), src)
	if got, err := p.ImportMansio(ctx, doc, false); err != nil || got != (ImportCounts{}) {
		t.Fatalf("re-run after reactivation: %+v %v", got, err)
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

// Source rows the target cannot hold are reported by name, and nothing is written.
func TestImportMansioRefusals(t *testing.T) {
	src := mansioSource(t, "mansio_"+"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ", "SessionTokenSessionTokenSession1")
	p, target := newPlacement(t, nil)
	sc := adminConn(t, src)
	// A name too long is cut at a character boundary (é is 2 bytes: 150 of them are 300 bytes).
	if _, err := sc.Exec(ctx, `update tenants set name = repeat('é', 150) where slug = 'acme'`); err != nil {
		t.Fatal(err)
	}
	// Another user here already has Ann's email.
	if _, err := adminConn(t, target).Exec(ctx, `insert into auth_users (user_id, email, name) values ('other', 'ann@example.test', 'Other')`); err != nil {
		t.Fatal(err)
	}
	doc, err := ReadMansio(ctx, pgURL(t), src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ImportMansio(ctx, doc, false); err == nil || !strings.Contains(err.Error(), "ann@example.test") {
		t.Fatalf("email taken: %v", err)
	}
	adminConn(t, target).Exec(ctx, `delete from auth_users where user_id = 'other'`)
	if _, err := p.ImportMansio(ctx, doc, false); err != nil {
		t.Fatal(err)
	}
	var name string
	adminConn(t, target).QueryRow(ctx, `select name from tenants where slug = 'acme'`).Scan(&name)
	if len(name) != 200 || name != strings.Repeat("é", 100) {
		t.Fatalf("name cut to %d bytes", len(name))
	}
	if _, err := sc.Exec(ctx, `update tenants set slug = repeat('s', 201) where slug = 'gone'`); err != nil {
		t.Fatal(err)
	}
	doc, _ = ReadMansio(ctx, pgURL(t), src)
	if _, err := p.ImportMansio(ctx, doc, false); err == nil || !strings.Contains(err.Error(), "22222222-2222-4222-8222-222222222222") {
		t.Fatalf("long slug: %v", err)
	}
}
