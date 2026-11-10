package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/load"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
	"github.com/nickstrad/mansio/server/internal/placement/file"
	"github.com/nickstrad/mansio/server/internal/schemareg"
)

const appToken = "app-token-0123456789"

// tenancyFixture is fixture with a tenancy section (off mode, its secrets file at perm) and create_dbs.
func tenancyFixture(t *testing.T, perm os.FileMode, section string) string {
	t.Helper()
	cfg := fixture(t, nil, `,"create_dbs":true,"schemas_dir":"schemas","tenancy":`+section)
	os.MkdirAll(filepath.Join(filepath.Dir(cfg), "schemas"), 0o755)
	sec, _ := json.Marshal(TenancySecrets{AdminToken: "admin-token-0123456789", AppTokens: map[string]string{"notes": appToken}})
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg), "tenancy.json"), sec, perm); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestTenancyConfigRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		cfg, stderr string
	}{
		"secrets readable by others": {tenancyFixture(t, 0o644, `{"mode":"off","secrets_file":"tenancy.json"}`), "make it 0600"},
		"unknown mode":               {tenancyFixture(t, 0o600, `{"mode":"better-auth","secrets_file":"tenancy.json"}`), "want off, api-key or session"},
		"no secrets file":            {tenancyFixture(t, 0o600, `{"mode":"off"}`), "tenancy.secrets_file is required"},
		"api-key without pg_dsn":     {tenancyFixture(t, 0o600, `{"mode":"api-key","apps":["notes"],"secrets_file":"tenancy.json"}`), "need pg_dsn"},
		"api-key, no app registered": {tenancyFixture(t, 0o600, `{"mode":"api-key","secrets_file":"tenancy.json"}`), "registers no app"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr syncBuf
			if code := realMain(context.Background(), []string{"-config", c.cfg}, &stderr); code != exitError || !strings.Contains(stderr.String(), c.stderr) {
				t.Fatalf("exit %d, stderr:\n%s", code, stderr.String())
			}
		})
	}
}

// Off mode end to end: the app token reaches default:<app>, but an unregistered schema is refused before
// anything is created; another token is AuthError; the admin gate answers without a store.
func TestTenancyOffMode(t *testing.T) {
	cfg := tenancyFixture(t, 0o600, `{"mode":"off","secrets_file":"tenancy.json"}`)
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	addr := waitAddr(t, addrFile, done)
	waitReady(t, addr)
	post := func(path, token, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	body := `{"v":1,"db":{"tenant":"forged","app":"notes"},"kind":"query","schema":"sha256:` + strings.Repeat("0", 64) +
		`","ops":[{"op":"get","table":"notes","id":"x"}]}`
	if code, out := post("/v1/ops", "wrong-token-0123456789", body); code != 401 || !strings.Contains(out, `"tag":"AuthError"`) {
		t.Fatalf("wrong token: %d %s", code, out)
	}
	if code, out := post("/v1/ops", appToken, body); code != 409 || !strings.Contains(out, "SchemaDriftError") {
		t.Fatalf("app token: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "dbs", "default:notes.db")); !os.IsNotExist(err) {
		t.Fatalf("a database was created for an unregistered schema: %v", err)
	}
	if code, _ := post("/admin/tenants", "", "{}"); code != 401 {
		t.Fatalf("admin without a token: %d", code)
	}
	if code, _ := post("/admin/tenants", "admin-token-0123456789", "{}"); code != 404 {
		t.Fatalf("off mode has no tenant routes: %d", code)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr.String())
	}
}

// create_dbs is capped per agent and per tenant (counting databases found at start); a refusal creates nothing.
func TestCreatingHostCaps(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DBDir: dir, MaxDBs: 3, MaxDBsPerTenant: 2}
	host := front.NewStaticHost(nil)
	defer host.Close()
	h := newCreatingHost(host, cfg, []string{"a:found"}) // found at start; not served by this test host
	ok := func(db string) {
		t.Helper()
		if _, err := h.Lookup(context.Background(), db); err != nil {
			t.Fatalf("%s: %v", db, err)
		}
	}
	refused := func(db, scope string) {
		t.Helper()
		_, err := h.Lookup(context.Background(), db)
		var q *front.QuotaError
		if !errors.As(err, &q) || q.Scope != scope {
			t.Fatalf("%s: err = %v, want a %s quota", db, err, scope)
		}
		if _, err := os.Stat(dbPath(cfg, db)); !os.IsNotExist(err) {
			t.Fatalf("%s: a refused database left a file (%v)", db, err)
		}
	}
	ok("a:x")
	ok("a:x") // already served: no new count
	refused("a:y", "tenant")
	ok("b:x")
	refused("c:x", "agent")
}

// A database that cannot be opened is not left behind: the empty file this call made is removed, and the
// counts and ids are not spent.
func TestCreatingHostCleansUpAFailedOpen(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DBDir: dir, MaxDBs: 3, MaxDBsPerTenant: 3}
	host := front.NewStaticHost(nil)
	defer host.Close()
	h := newCreatingHost(host, cfg, nil)
	// A directory where the file would be: the actor cannot open it.
	if err := os.Mkdir(dbPath(cfg, "a:dir.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Lookup(context.Background(), "a:dir.db"); !errors.Is(err, front.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if fi, err := os.Stat(dbPath(cfg, "a:dir.db")); err != nil || !fi.IsDir() {
		t.Fatalf("a path this call did not create was removed: %v", err)
	}
	if h.next != 0 || h.total != 0 {
		t.Fatalf("a failed create advanced next=%d total=%d", h.next, h.total)
	}
}

// IR-5: tenancy in single-node durable mode. A credential request runs at its database's owner-table epoch;
// with create_dbs the claim path creates a missing database (placement row, then claim), within the caps;
// after a restart the same credential reaches the same data (the key reconciles or is claimed again). With
// placement_pg the section is refused: multi-node authenticates at controld.
func TestTenancyDurableClaimCreates(t *testing.T) {
	const notes = `{"notes":{"fields":{"text":{"node":{"kind":"string"}}},"storage":"columns","indexes":[]}}`
	r := newDurableRig(t, `,"schemas_dir":"schemas","create_dbs":true,"max_dbs_per_tenant":1,`+
		`"tenancy":{"mode":"off","secrets_file":"tenancy.json"}`)
	for _, app := range []string{"notes", "other"} {
		os.MkdirAll(filepath.Join(r.dir, "schemas", app), 0o755)
		os.WriteFile(filepath.Join(r.dir, "schemas", app, "v1.json"), []byte(notes), 0o644)
	}
	sec, _ := json.Marshal(TenancySecrets{AdminToken: "admin-token-0123456789",
		AppTokens: map[string]string{"notes": appToken, "other": appToken + "-other"}})
	os.WriteFile(filepath.Join(r.dir, "tenancy.json"), sec, 0o600)
	fp := schemareg.Fingerprint([]byte(notes))
	post := func(a *running, token, app, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://"+a.addr+"/v1/ops", strings.NewReader(
			`{"v":1,"db":{"tenant":"forged","app":"`+app+`"},"schema":"`+fp+`",`+body+`}`))
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	const id = "01900000-0000-7000-8000-000000000001"
	a := r.start(t)
	waitReady(t, a.life)
	if code, out := post(a, appToken, "notes", `"kind":"mutation","requestId":"r1","ops":[{"op":"insert","table":"notes","id":"`+id+
		`","value":{"text":"hi"}}]`); code != 200 {
		t.Fatalf("first use: %d %s", code, out)
	}
	if code, out := post(a, appToken+"-other", "other", `"kind":"query","ops":[{"op":"get","table":"notes","id":"x"}]`); code != 429 ||
		!strings.Contains(out, "QuotaExceeded") {
		t.Fatalf("past max_dbs_per_tenant: %d %s", code, out)
	}
	pl, err := file.Open(filepath.Join(r.dir, "placement"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := pl.Get(context.Background(), "default:notes"); err != nil || p.Owner != "vm-1" || p.Epoch != 1 {
		t.Fatalf("placement row: %+v %v", p, err)
	}
	if _, err := pl.Get(context.Background(), "default:other"); !errors.Is(err, owner.ErrNotFound) {
		t.Fatalf("a refused create wrote a row: %v", err)
	}
	if code := a.stop(); code != exitOK {
		t.Fatalf("stop: %d\n%s", code, a.stderr.String())
	}
	b := r.start(t)
	waitReady(t, b.life)
	waitUntil(t, func() bool {
		code, out := post(b, appToken, "notes", `"kind":"query","ops":[{"op":"get","table":"notes","id":"`+id+`"}]`)
		return code == 200 && strings.Contains(out, `"text":"hi"`)
	})

}

// newClaims is a claimingPlace over a fake placement authority and a real owner table.
func newClaims(t *testing.T, cfg Config) (*claimingPlace, *ownertest.Placement) {
	t.Helper()
	pl := ownertest.NewPlacement()
	tab, err := owner.Open(context.Background(), owner.Config{Self: cfg.VM, Store: ownertest.NewMemStore(), Placement: pl,
		WriteBack: &ownertest.WriteBack{Objects: ownertest.NewObjects(), Disk: ownertest.NewDisk()}})
	if err != nil {
		t.Fatal(err)
	}
	return newClaimingPlace(&durableAgent{cfg: &cfg, tab: tab, place: pl}), pl
}

// B6's overload scenario for the claim path: a first-use storm against a slow placement store. Before, every
// credential request for a database not yet active queued on one process-wide mutex, held across the authority
// calls: unbounded in number, outside every limit, and deaf to the request's deadline. Now each database has
// one flight, at most lifecycle_concurrency flights run, other databases are shed at once, and a waiter leaves
// at its own deadline while the flight goes on for the rest.
func TestClaimStormIsBoundedAndObeysDeadlines(t *testing.T) {
	p, pl := newClaims(t, Config{VM: "vm-1", CreateDBs: true, MaxDBs: 100, MaxDBsPerTenant: 100, LifecycleConcurrency: 2,
		LifecycleTimeout: load.Duration{Duration: 10 * time.Second}})
	pl.SetDelay(300 * time.Millisecond) // every authority call is slow
	ctx := context.Background()
	type placed struct {
		epoch uint64
		err   error
	}
	call := func(ctx context.Context, db string) chan placed {
		c := make(chan placed, 1)
		go func() { e, _, err := p.place(ctx, db); c <- placed{e, err} }()
		return c
	}
	a, b := call(ctx, "t1:a"), call(ctx, "t2:b")
	waitUntil(t, func() bool { return len(p.sem) == 2 })

	// A third database: refused at once, nothing started for it.
	t0 := time.Now()
	if _, _, err := p.place(ctx, "t3:c"); !errors.Is(err, front.ErrShed) || time.Since(t0) > 100*time.Millisecond {
		t.Fatalf("a claim beyond lifecycle_concurrency: %v after %v, want shed at once", err, time.Since(t0))
	}
	if _, ok := pl.Row("t3:c"); ok || p.shed.Load() != 1 {
		t.Fatalf("the shed claim wrote a placement row or was not counted (shed %d)", p.shed.Load())
	}
	// More requests for a database already being claimed join its flight: none is shed, none starts another.
	joined := []chan placed{call(ctx, "t1:a"), call(ctx, "t1:a"), call(ctx, "t1:a")}
	// One of them has a short deadline: it leaves on time, and the flight is not cancelled for the others.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	t0 = time.Now()
	if _, _, err := p.place(short, "t1:a"); !errors.Is(err, context.DeadlineExceeded) || time.Since(t0) > 250*time.Millisecond {
		t.Fatalf("a waiter with a 50 ms deadline: %v after %v", err, time.Since(t0))
	}
	for _, c := range append(joined, a, b) {
		if r := <-c; r.err != nil || r.epoch != 1 {
			t.Fatalf("a request that waited for its database's flight: %+v", r)
		}
	}
	if p.shed.Load() != 1 || len(p.sem) != 0 {
		t.Fatalf("after the storm: shed %d (want 1), flights in flight %d (want 0)", p.shed.Load(), len(p.sem))
	}
	// Warm now: no authority call, so the store's delay is not paid.
	t0 = time.Now()
	if e, _, err := p.place(ctx, "t1:a"); err != nil || e != 1 || time.Since(t0) > 100*time.Millisecond {
		t.Fatalf("warm place: epoch %d, %v after %v", e, err, time.Since(t0))
	}
	if e, _, err := p.place(ctx, "t3:c"); err != nil || e != 1 {
		t.Fatalf("the shed database on retry: epoch %d, %v", e, err)
	}
}

// Creates stay exact against the caps although claims now run in parallel: of four first uses for one tenant
// capped at two databases, exactly two create.
func TestConcurrentCreatesRespectTheTenantCap(t *testing.T) {
	p, pl := newClaims(t, Config{VM: "vm-1", CreateDBs: true, MaxDBs: 100, MaxDBsPerTenant: 2, LifecycleConcurrency: 4,
		LifecycleTimeout: load.Duration{Duration: 10 * time.Second}})
	pl.SetDelay(20 * time.Millisecond)
	errc := make(chan error, 4)
	for i := range 4 {
		go func() { _, _, err := p.place(context.Background(), fmt.Sprintf("acme:db%d", i)); errc <- err }()
	}
	var created, refused int
	for range 4 {
		var q *front.QuotaError
		switch err := <-errc; {
		case err == nil:
			created++
		case errors.As(err, &q) && q.Scope == "tenant" && q.Limit == 2:
			refused++
		default:
			t.Fatalf("a first use: %v", err)
		}
	}
	if created != 2 || refused != 2 {
		t.Fatalf("%d created, %d refused; want 2 and 2", created, refused)
	}
}
