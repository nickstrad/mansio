package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/load"
	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// The schema registry routes (schemas.go): spec tests against a fake store, then the agent's client
// (schemareg.Client) against the real routes, over the fake and over Postgres (gated on MANSIO_PG_URL): the
// same contract the client's own tests check against schemaregtest.

// memSchemas is a schemaStore in memory, with the pg adapter's error answers.
type memSchemas struct {
	mu   sync.Mutex
	regs map[[2]string][]byte
	fail error // every call fails with this (e.g. pg.ErrUnavailable)
}

func (m *memSchemas) RegisterSchema(_ context.Context, app, fp string, text []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return false, m.fail
	}
	if err := schemareg.Check(fp, text); err != nil || !pg.ValidName(app) {
		return false, owner.ErrInvalid
	}
	k := [2]string{app, fp}
	_, had := m.regs[k]
	m.regs[k] = append([]byte(nil), text...)
	return !had, nil
}

func (m *memSchemas) GetSchema(_ context.Context, app, fp string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	t, ok := m.regs[[2]string{app, fp}]
	if !ok {
		return nil, owner.ErrNotFound
	}
	return t, nil
}

func (m *memSchemas) AppRegistered(_ context.Context, app string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.regs {
		if k[0] == app {
			return true, m.fail
		}
	}
	return false, m.fail
}

// schemaAPI serves controld's routes over store (no Postgres, agents or locator behind the other routes).
func schemaAPI(t *testing.T, store schemaStore) *httptest.Server {
	t.Helper()
	srv, _ := schemaDaemon(t, store)
	return srv
}

func schemaDaemon(t *testing.T, store schemaStore) (*httptest.Server, *daemon) {
	t.Helper()
	cfg := defaults()
	cfg.APITimeout = load.Duration{Duration: 5 * time.Second}
	d := &daemon{cfg: &cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), token: adminToken, control: controlToken,
		sem: make(chan struct{}, 4), resolveSem: make(chan struct{}, 4), schemas: store}
	d.ready.Store(true)
	srv := httptest.NewServer(d.routes(metrics.NewRegistry()))
	t.Cleanup(srv.Close)
	return srv, d
}

// The agents' schema read has its own bulkhead (the locator's): it is served while admin calls fill theirs.
func TestSchemaReadDoesNotQueueBehindAdminCalls(t *testing.T) {
	store := &memSchemas{regs: map[[2]string][]byte{}}
	srv, d := schemaDaemon(t, store)
	fp := schemareg.Fingerprint([]byte(schemaText))
	store.RegisterSchema(context.Background(), "tic-tac-toe", fp, []byte(schemaText))
	for range cap(d.sem) {
		d.sem <- struct{}{}
	}
	if code, out := do(t, srv, http.MethodGet, "/v1/apps/tic-tac-toe/schemas/"+fp, controlToken, nil); code != 200 {
		t.Fatalf("GET with the admin bulkhead full: %d %v", code, out)
	}
	body, _ := json.Marshal(map[string]string{"fingerprint": fp, "schema": schemaText})
	if code, _ := do(t, srv, http.MethodPut, "/v1/apps/tic-tac-toe/schema", adminToken, body); code != 503 {
		t.Fatalf("PUT with the admin bulkhead full: %d, want 503", code)
	}
}

// do sends one request with token and decodes the answer into a map.
func do(t *testing.T, srv *httptest.Server, method, path, token string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

const schemaText = `{"games":{"fields":{"board":{"node":{"kind":"array","item":{"kind":"string"},"length":9}}},"storage":"columns","indexes":[]}}`

func TestRegisterSchema(t *testing.T) {
	srv := schemaAPI(t, &memSchemas{regs: map[[2]string][]byte{}})
	fp := schemareg.Fingerprint([]byte(schemaText))
	asString, _ := json.Marshal(map[string]string{"fingerprint": fp, "schema": schemaText})
	asObject := []byte(`{"fingerprint":"` + fp + `","schema":` + schemaText + `}`)
	put := func(body []byte, token string) (int, map[string]any) {
		return do(t, srv, http.MethodPut, "/v1/apps/tic-tac-toe/schema", token, body)
	}
	if code, out := put(asObject, adminToken); code != 200 || out["created"] != true || out["fingerprint"] != fp {
		t.Fatalf("first PUT (object): %d %v", code, out)
	}
	if code, out := put(asString, adminToken); code != 200 || out["created"] != false {
		t.Fatalf("second PUT (string form, idempotent): %d %v", code, out)
	}
	indented := []byte(`{"fingerprint":"` + fp + `","schema": ` + strings.ReplaceAll(schemaText, ",", ", ") + `}`)
	for _, tc := range []struct {
		name, path, token string
		body              []byte
		code              int
	}{
		{"control token cannot register", "/v1/apps/tic-tac-toe/schema", controlToken, asObject, 401},
		{"no token", "/v1/apps/tic-tac-toe/schema", "", asObject, 401},
		{"bad app", "/v1/apps/Tic/schema", adminToken, asObject, 400},
		{"app with a colon", "/v1/apps/a:b/schema", adminToken, asObject, 400},
		{"reformatted text (the fingerprint is over the exact bytes)", "/v1/apps/x/schema", adminToken, indented, 400},
		{"unknown member", "/v1/apps/x/schema", adminToken, []byte(`{"fingerprint":"` + fp + `","schema":{},"x":1}`), 400},
		{"not an object", "/v1/apps/x/schema", adminToken, []byte(`{"fingerprint":"` + schemareg.Fingerprint([]byte("[]")) + `","schema":[]}`), 400},
		{"over the body cap", "/v1/apps/x/schema", adminToken, []byte(`{"fingerprint":"","schema":"` + strings.Repeat("x", maxSchemaBody) + `"}`), 413},
		{"trailing data", "/v1/apps/x/schema", adminToken, append(append([]byte{}, asObject...), []byte(` {}`)...), 400},
		{"trailing whitespace is fine (on point)", "/v1/apps/x/schema", adminToken, append(append([]byte{}, asObject...), []byte(" \n")...), 200},
	} {
		if code, out := do(t, srv, http.MethodPut, tc.path, tc.token, tc.body); code != tc.code {
			t.Errorf("%s: %d %v, want %d", tc.name, code, out, tc.code)
		}
	}
}

func TestGetSchemaRoute(t *testing.T) {
	store := &memSchemas{regs: map[[2]string][]byte{}}
	srv := schemaAPI(t, store)
	fp := schemareg.Fingerprint([]byte(schemaText))
	store.RegisterSchema(context.Background(), "tic-tac-toe", fp, []byte(schemaText))
	for _, tok := range []string{controlToken, adminToken} {
		if code, out := do(t, srv, http.MethodGet, "/v1/apps/tic-tac-toe/schemas/"+fp, tok, nil); code != 200 || out["schema"] != schemaText {
			t.Fatalf("GET with %s: %d %v", tok, code, out)
		}
	}
	for _, tc := range []struct {
		path, token string
		code        int
	}{
		{"/v1/apps/chess/schemas/" + fp, controlToken, 404},
		{"/v1/apps/tic-tac-toe/schemas/sha256:nope", controlToken, 400},
		{"/v1/apps/tic-tac-toe/schemas/" + fp, edgeToken, 401},
	} {
		if code, out := do(t, srv, http.MethodGet, tc.path, tc.token, nil); code != tc.code {
			t.Errorf("GET %s: %d %v, want %d", tc.path, code, out, tc.code)
		}
	}
	store.fail = pg.ErrUnavailable
	if code, out := do(t, srv, http.MethodGet, "/v1/apps/tic-tac-toe/schemas/"+fp, controlToken, nil); code != 503 || out["code"] != "unavailable" {
		t.Fatalf("store down: %d %v", code, out)
	}
}

// An app database needs a registered schema; resolve takes exactly one of db and app.
func TestAppDatabaseRefusals(t *testing.T) {
	srv := schemaAPI(t, &memSchemas{regs: map[[2]string][]byte{}})
	if code, out := do(t, srv, http.MethodPost, "/v1/apps/unknown/databases", adminToken, []byte(`{"tenant":"acme"}`)); code != 404 {
		t.Fatalf("create for an unregistered app: %d %v", code, out)
	}
	if code, out := do(t, srv, http.MethodPost, "/v1/apps/x/databases", adminToken, []byte(`{"tenant":"Acme"}`)); code != 400 {
		t.Fatalf("bad tenant: %d %v", code, out)
	}
	for _, body := range []string{`{"tenant":"acme"}`, `{"tenant":"acme","db":"a","app":"a"}`, `{"tenant":"acme","db":"a","create":true}`} {
		if code, out := do(t, srv, http.MethodPost, "/v1/resolve", "mansio_x", []byte(body)); code != 400 {
			t.Errorf("resolve %s: %d %v, want 400", body, code, out)
		}
	}
}

// The agent's client against the real route: the fake store here, Postgres below.
func runClientContract(t *testing.T, store schemaStore) {
	srv := schemaAPI(t, store)
	c, err := schemareg.NewClient(schemareg.Config{URL: srv.URL, Token: controlToken})
	if err != nil {
		t.Fatal(err)
	}
	fp := schemareg.Fingerprint([]byte(schemaText))
	if _, err := c.Get(context.Background(), "tic-tac-toe", fp); !errors.Is(err, schemareg.ErrNotFound) {
		t.Fatalf("before registering: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"fingerprint": fp, "schema": schemaText})
	if code, out := do(t, srv, http.MethodPut, "/v1/apps/tic-tac-toe/schema", adminToken, body); code != 200 {
		t.Fatalf("PUT: %d %v", code, out)
	}
	got, err := c.Get(context.Background(), "tic-tac-toe", fp)
	if err != nil || got != schemaText {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if _, err := c.Get(context.Background(), "chess", fp); !errors.Is(err, schemareg.ErrNotFound) {
		t.Fatalf("another app: %v", err)
	}
}

func TestSchemaClientContractFake(t *testing.T) {
	runClientContract(t, &memSchemas{regs: map[[2]string][]byte{}})
}

func TestSchemaClientContractPostgres(t *testing.T) {
	url := os.Getenv("MANSIO_PG_URL")
	if url == "" {
		t.Skip("MANSIO_PG_URL not set")
	}
	b := make([]byte, 6)
	rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	admin, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(context.Background(), "create schema "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "drop schema "+schema+" cascade")
	p, err := pg.Open(pg.Config{DSN: url, Schema: schema, AppName: "test/controld-schemas"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runClientContract(t, p)
}
