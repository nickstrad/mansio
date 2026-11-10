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

	"github.com/nickstrad/mansio/server/internal/control/locator"
	"github.com/nickstrad/mansio/server/internal/load"
	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/schemareg"
	"github.com/nickstrad/mansio/server/internal/slots"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// The schema registry routes (schemas.go): spec tests against a fake store, then the agent's client
// (schemareg.Client) against the real routes, over the fake and over Postgres (gated on MANSIO_PG_URL): the
// same contract the client's own tests check against schemaregtest.

// memSchemas is a schemaStore in memory, with the pg adapter's error answers.
type memSchemas struct {
	mu   sync.Mutex
	regs map[[2]string][]byte
	fail error // every call fails with this (e.g. pg.ErrUnavailable)
	apps int   // AppRegistered calls
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
	m.apps++
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
		api: slots.New(4), resolves: slots.New(4), loads: slots.New(4), creates: slots.New(2),
		schemas: store}
	d.ready.Store(true)
	h, err := d.routes(metrics.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, d
}

// The agents' schema read has its own bulkhead (the locator's): it is served while admin calls fill theirs.
func TestSchemaReadDoesNotQueueBehindAdminCalls(t *testing.T) {
	store := &memSchemas{regs: map[[2]string][]byte{}}
	srv, d := schemaDaemon(t, store)
	fp := schemareg.Fingerprint([]byte(schemaText))
	store.RegisterSchema(context.Background(), "tic-tac-toe", fp, []byte(schemaText))
	for range d.api.Limit() {
		d.api.TryAcquire()
	}
	if code, out := do(t, srv, http.MethodGet, "/v1/apps/tic-tac-toe/schemas/"+fp, controlToken, nil); code != 200 {
		t.Fatalf("GET with the admin bulkhead full: %d %v", code, out)
	}
	body, _ := json.Marshal(map[string]string{"fingerprint": fp, "schema": schemaText})
	if code, _ := do(t, srv, http.MethodPut, "/v1/apps/tic-tac-toe/schema", adminToken, body); code != 503 {
		t.Fatalf("PUT with the admin bulkhead full: %d, want 503", code)
	}
	if d.api.Shed() != 1 || d.resolves.Shed() != 0 || d.loads.Shed() != 0 { // B1: counted against its own pool
		t.Fatalf("refusals counted: api %d resolve %d load %d, want 1, 0 and 0", d.api.Shed(), d.resolves.Shed(), d.loads.Shed())
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

// With the docschema check (serve's checkSchema), only valid schemas in fingerprint form are stored: a text
// with whitespace, or one that is no schema, hashes fine but is refused.
func TestRegisterSchemaChecksFingerprintForm(t *testing.T) {
	store := &memSchemas{regs: map[[2]string][]byte{}}
	srv, d := schemaDaemon(t, store)
	d.checkSchema = canonicalSchema
	for _, tc := range []struct {
		text string
		code int
	}{
		{schemaText, 200},
		{" " + schemaText, 400},
		{`{"games":{}}`, 400},
		{`{"games":{"storage":"columns","fields":{},"indexes":[]}}`, 400}, // members out of order
	} {
		raw, _ := json.Marshal(tc.text)
		body := []byte(`{"fingerprint":"` + schemareg.Fingerprint([]byte(tc.text)) + `","schema":` + string(raw) + `}`)
		if code, out := do(t, srv, http.MethodPut, "/v1/apps/tic-tac-toe/schema", adminToken, body); code != tc.code {
			t.Errorf("%q: %d %v, want %d", tc.text, code, out, tc.code)
		}
	}
	if len(store.regs) != 1 {
		t.Fatalf("stored %d schemas, want 1", len(store.regs))
	}
}

// creatableStore is a locator.Store with one API key and no databases: every app resolve with "create" is
// creatable.
type creatableStore struct {
	id  string
	key locator.StoredKey
}

func (s creatableStore) Locate(context.Context, string, bool) (locator.Placement, error) {
	return locator.Placement{}, nil
}

func (s creatableStore) APIKey(_ context.Context, id string) (locator.StoredKey, error) {
	if id != s.id {
		return locator.StoredKey{}, locator.ErrNotFound
	}
	return s.key, nil
}

type noSigner struct{}

func (noSigner) Mint(ticket.Claims) (string, error) { return "", errors.New("not reached") }

// B4: create on first use has its own small bulkhead. With every create slot taken, a creating resolve is
// refused shed with Retry-After before it reads the schema registry, counts databases or calls an agent.
func TestCreateOnFirstUseIsBoundedByItsOwnSlots(t *testing.T) {
	store := &memSchemas{regs: map[[2]string][]byte{}}
	srv, d := schemaDaemon(t, store)
	key, id, hash, err := locator.NewAPIKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d.loc = locator.New(creatableStore{id, locator.StoredKey{Tenant: "acme", Principal: "app", Perms: ticket.Read | ticket.Write, Hash: hash}},
		noSigner{}, locator.Config{LoadSlots: d.loads})
	for range d.creates.Limit() {
		d.creates.TryAcquire()
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/resolve", strings.NewReader(`{"tenant":"acme","app":"tic-tac-toe","create":true}`))
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != 503 || out["code"] != "shed" || res.Header.Get("Retry-After") == "" {
		t.Fatalf("creating resolve with the create slots taken: %d %v (Retry-After %q)", res.StatusCode, out, res.Header.Get("Retry-After"))
	}
	if d.creates.Shed() != 1 || store.appReads() != 0 {
		t.Fatalf("create refusals %d (want 1), schema registry reads %d (want 0: refused before any work)", d.creates.Shed(), store.appReads())
	}
}

func (m *memSchemas) appReads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.apps
}

// B11: the pools in front of Postgres have a runtime lever. Lowering resolve_load applies to the next read; the
// pool budget (load + create + api below pg.max_conns) cannot be broken at run time either.
func TestLimitLever(t *testing.T) {
	store := &memSchemas{regs: map[[2]string][]byte{}}
	srv, d := schemaDaemon(t, store) // api 4, loads 4, creates 2; pg.max_conns 32
	fp := schemareg.Fingerprint([]byte(schemaText))
	store.RegisterSchema(context.Background(), "tic-tac-toe", fp, []byte(schemaText))
	limit := func(queue string, n int) (int, map[string]any) {
		body, _ := json.Marshal(map[string]int{"limit": n})
		return do(t, srv, http.MethodPost, "/v1/limits/"+queue, adminToken, body)
	}
	if code, out := limit("resolve_load", 1); code != 200 || out["previous"] != 4.0 || out["limit"] != 1.0 || d.loads.Limit() != 1 {
		t.Fatalf("lower resolve_load to 1 (on point): %d %v, pool limit %d", code, out, d.loads.Limit())
	}
	d.loads.TryAcquire() // one store read in flight: the lowered pool is full
	if code, out := do(t, srv, http.MethodGet, "/v1/apps/tic-tac-toe/schemas/"+fp, controlToken, nil); code != 503 {
		t.Fatalf("a schema read beyond the lowered limit: %d %v, want 503", code, out)
	}
	d.loads.Release()
	if code, out := do(t, srv, http.MethodGet, "/v1/apps/tic-tac-toe/schemas/"+fp, controlToken, nil); code != 200 {
		t.Fatalf("a schema read within the lowered limit: %d %v", code, out)
	}
	for _, tc := range []struct {
		queue  string
		n      int
		status int
		code   string
	}{
		{"resolve_load", 0, 400, "bad_request"},   // off point
		{"resolve_load", 26, 400, "bad_request"},  // 26 + 2 + 4 = 32: not below pg.max_conns (off point)
		{"resolve_load", 25, 200, "ok"},           // 31: the on point
		{"resolve_create", 3, 400, "bad_request"}, // 25 + 3 + 4 = 32
		{"resolve", 2, 404, "not_found"},          // the wide gate has no lever: it protects no dependency
		{"controld_api", 2, 200, "ok"},
		{"resolve_create", 1, 200, "ok"},
	} {
		if code, out := limit(tc.queue, tc.n); code != tc.status || out["code"] != tc.code {
			t.Errorf("limit %s %d: %d %v, want %d %s", tc.queue, tc.n, code, out, tc.status, tc.code)
		}
	}
	if d.loads.Limit() != 25 || d.creates.Limit() != 1 || d.api.Limit() != 2 {
		t.Fatalf("pool limits %d, %d and %d; want 25, 1 and 2", d.loads.Limit(), d.creates.Limit(), d.api.Limit())
	}
}
