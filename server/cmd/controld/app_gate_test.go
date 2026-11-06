package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nickstrad/mansio/server/internal/control"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// The IR-2 identity gate (ADR 0011): an app registers its schema; a client that knows only (tenant, app) and an
// API key resolves it, creating the database on first use, and writes through the ticket; an agent fetches the
// schema by fingerprint with the control token. Gated on MANSIO_PG_URL and COLDLOAD_S3, like TestLocatorGate.
//
//	MANSIO_PG_URL='postgres://mansio:mansio-dev@localhost:5433/mansio?sslmode=disable' COLDLOAD_S3=localhost:8333 \
//	  go test -count=1 -run TestAppIdentityGate -v ./cmd/controld
func TestAppIdentityGate(t *testing.T) {
	c := newCluster(t)
	c.locator, c.controldAddr = true, freeAddr(t)
	c.startAgent("vm-a")
	c.startAgent("vm-b")
	c.startControld()
	waitListening(t, c)

	var key map[string]string
	if code := c.call("POST", "/v1/tenants/acme/api-keys", map[string]string{"principal": "app"}, &key); code != 200 {
		t.Fatalf("create api key: %d %v", code, key)
	}
	var ro map[string]string
	c.call("POST", "/v1/tenants/acme/api-keys", map[string]string{"principal": "reader", "perms": "read"}, &ro)
	resolveApp := func(apiKey, app string, create bool) (int, route) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"tenant": "acme", "app": app, "create": create})
		req, _ := http.NewRequest(http.MethodPost, c.api+"/v1/resolve", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var r route
		json.NewDecoder(res.Body).Decode(&r)
		return res.StatusCode, r
	}

	// Before registering: no database, and none is created.
	if code, _ := resolveApp(key["key"], "tic-tac-toe", true); code != 404 {
		t.Fatalf("unregistered app, create: %d", code)
	}
	fp := schemareg.Fingerprint([]byte(schemaText))
	var reg map[string]any
	if code := c.call("PUT", "/v1/apps/tic-tac-toe/schema", map[string]string{"fingerprint": fp, "schema": schemaText}, &reg); code != 200 || reg["created"] != true {
		t.Fatalf("register: %d %v", code, reg)
	}
	if code, _ := resolveApp(key["key"], "tic-tac-toe", false); code != 404 {
		t.Fatalf("without create: %d", code)
	}
	if code, _ := resolveApp(ro["key"], "tic-tac-toe", true); code != 404 {
		t.Fatalf("read-only key, create: %d", code)
	}

	// First use creates acme:tic-tac-toe; a resolve may answer moving while its owner claims it.
	var r route
	waitFor(t, "resolve with create on first use", func() bool {
		code, got := resolveApp(key["key"], "tic-tac-toe", true)
		r = got
		return code == 200
	})
	if r.DB != "acme:tic-tac-toe" || r.VM == "" || r.Epoch < 1 || r.Ticket == "" {
		t.Fatalf("route %+v", r)
	}
	var d control.Database
	if code := c.call("GET", "/v1/databases/acme:tic-tac-toe", nil, &d); code != 200 || d.State != control.StateActive || d.Name != "tic-tac-toe" {
		t.Fatalf("registry row: %d %+v", code, d)
	}
	waitFor(t, "write through the app route", func() bool {
		res, err := send(r, front.KindWrite, "init", "create table t (id integer not null)")
		return err == nil && res.Code == front.OK
	})
	// The same (tenant, app) by its raw key resolves to the same database (the existing path is unchanged).
	if code, _, r2 := c.resolve(key["key"], "acme", "tic-tac-toe", 0); code != 200 || r2.DB != r.DB || r2.Epoch != r.Epoch {
		t.Fatalf("raw-key resolve: %d %+v", code, r2)
	}

	// Explicit create of another tenant's database of the same app.
	var d2 control.Database
	if code := c.call("POST", "/v1/apps/tic-tac-toe/databases", map[string]string{"tenant": "globex"}, &d2); code != 200 || d2.Key != "globex:tic-tac-toe" || d2.State != control.StateActive {
		t.Fatalf("explicit create: %d %+v", code, d2)
	}

	// An agent fetches the schema by fingerprint with the control token.
	cl, err := schemareg.NewClient(schemareg.Config{URL: c.api, Token: controlToken})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cl.Get(context.Background(), "tic-tac-toe", fp); err != nil || got != schemaText {
		t.Fatalf("agent fetch: %q, %v", got, err)
	}
}
