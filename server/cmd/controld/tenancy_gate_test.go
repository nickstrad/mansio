package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// The IR-5 auth gate (ADR 0012, multi-node half): with a "tenancy" section controld resolves app databases
// for every credential mansio's tenancy accepts: a tenant's new API key, a legacy Deno-platform key and a user
// session. The tenant comes from the credential (an omitted tenant takes it, another is forbidden); a
// deactivated tenant's credentials stop working and it gets no new key; create on first use stops at the
// per-tenant cap. Gated on MANSIO_PG_URL and COLDLOAD_S3, like TestAppIdentityGate.
//
//	MANSIO_PG_URL='postgres://mansio:mansio-dev@localhost:5433/mansio?sslmode=disable' COLDLOAD_S3=localhost:8333 \
//	  go test -count=1 -run TestTenancyResolveGate -v ./cmd/controld
func TestTenancyResolveGate(t *testing.T) {
	c := newCluster(t)
	c.locator, c.controldAddr = true, freeAddr(t)
	c.extra = `,"tenancy":{"mode":"session","cache_ttl":"10ms"},"locator":{"max_dbs_per_tenant":2,"auth_ttl":"10ms"}`
	c.startAgent("vm-a")
	c.startAgent("vm-b")
	c.startControld()
	waitListening(t, c)

	post := func(path, token string, body, out any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, c.api+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if out != nil {
			json.NewDecoder(res.Body).Decode(out)
		}
		return res.StatusCode
	}
	for _, app := range []string{"tic-tac-toe", "notes", "chess"} {
		fp := schemareg.Fingerprint([]byte(schemaText))
		if code := c.call("PUT", "/v1/apps/"+app+"/schema", map[string]string{"fingerprint": fp, "schema": schemaText}, nil); code != 200 {
			t.Fatalf("register %s: %d", app, code)
		}
	}
	// The tenant admin API is controld's, under its admin token.
	var created struct {
		Result struct {
			Tenant struct{ ID string }
			APIKey struct{ Plaintext string }
		}
	}
	if code := post("/admin/tenants", adminToken, map[string]string{"name": "Acme", "slug": "acme"}, &created); code != 200 {
		t.Fatalf("create tenant: %d", code)
	}
	tenant, newKey := created.Result.Tenant.ID, created.Result.APIKey.Plaintext
	// A legacy key, as `controld import-mansio` stores one: the whole key's hash under a uuid id.
	legacy := "mansio_" + strings.Repeat("Q", 42) + "A"
	h := sha256.Sum256([]byte(legacy))
	conn, err := pgx.Connect(context.Background(), c.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), `insert into `+c.schema+`.api_keys (key_id, tenant_id, principal, secret_sha256, perms, legacy, name)
		values ('00000000-0000-4000-8000-0000000000aa', $1, '00000000-0000-4000-8000-0000000000aa', $2, 3, true, 'legacy')`, tenant, h[:]); err != nil {
		t.Fatal(err)
	}
	// A session of a user linked to the tenant.
	var signed struct{ Token string }
	if code := post("/api/auth/sign-up/email", "", map[string]string{"name": "Ann", "email": "ann@acme.test", "password": "correct horse battery"}, &signed); code != 200 || signed.Token == "" {
		t.Fatalf("sign up: %d", code)
	}
	if code := post("/admin/users/link", adminToken, map[string]string{"email": "ann@acme.test", "tenantId": tenant}, nil); code != 200 {
		t.Fatalf("link: %d", code)
	}

	resolve := func(token string, body map[string]any) (int, route) {
		t.Helper()
		var r route
		code := post("/v1/resolve", token, body, &r)
		return code, r
	}
	for name, cred := range map[string]string{"new key": newKey, "legacy key": legacy, "session": signed.Token} {
		var r route
		waitFor(t, name+": resolve with create", func() bool {
			code, got := resolve(cred, map[string]any{"app": "tic-tac-toe", "create": true})
			r = got
			return code == 200
		})
		if r.DB != tenant+":tic-tac-toe" || r.Ticket == "" {
			t.Fatalf("%s: route %+v", name, r)
		}
		if code, _ := resolve(cred, map[string]any{"tenant": "globex", "app": "tic-tac-toe"}); code != 403 {
			t.Fatalf("%s, another tenant: %d", name, code)
		}
	}
	if code, _ := resolve(signed.Token, map[string]any{"tenant": tenant, "db": "tic-tac-toe"}); code != 401 {
		t.Fatalf("a session on a raw database resolve: %d", code)
	}
	if code, _ := resolve("garbage-session-token-xyz", map[string]any{"app": "tic-tac-toe"}); code != 401 {
		t.Fatalf("garbage: %d", code)
	}

	// The cap: the tenant has one database; a second is created, a third is refused and not created.
	waitFor(t, "second database", func() bool {
		code, _ := resolve(newKey, map[string]any{"app": "notes", "create": true})
		return code == 200
	})
	var refused struct{ Code string }
	if code := post("/v1/resolve", newKey, map[string]any{"app": "chess", "create": true}, &refused); code != 429 || refused.Code != "quota_exceeded" {
		t.Fatalf("past the cap: %d %+v", code, refused)
	}
	if code, _ := resolve(newKey, map[string]any{"app": "ghost", "create": true}); code != 404 {
		t.Fatalf("an unregistered app past the cap: %d, want 404 (checked before the caps)", code)
	}
	if code := c.call("GET", "/v1/databases/"+tenant+":chess", nil, nil); code != 404 {
		t.Fatalf("a refused create left a row: %d", code)
	}

	// Deactivation: every credential stops, and no key can be created for the tenant.
	req, _ := http.NewRequest(http.MethodDelete, c.api+"/admin/tenants/"+tenant, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("deactivate: %v %v", res, err)
	}
	for name, cred := range map[string]string{"new key": newKey, "legacy key": legacy, "session": signed.Token} {
		waitFor(t, name+" refused after deactivation", func() bool {
			code, _ := resolve(cred, map[string]any{"app": "tic-tac-toe"})
			return code == 401
		})
	}
	if code := post("/v1/tenants/"+tenant+"/api-keys", adminToken, map[string]string{"principal": "late"}, nil); code != 409 {
		t.Fatalf("api key for a deactivated tenant: %d", code)
	}
}
