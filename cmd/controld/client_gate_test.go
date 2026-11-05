package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/client"
	"github.com/nickstrad/ts-db-go/internal/front"
)

// The CP-6 gate (control-plane plan §3): clients that know only controld, through internal/client.
//
//  1. frontbench's locator mode (the real binary) with a move every second: resolve hit/miss latency, and the
//     error rate of requests that overlapped a move.
//  2. With the owner's S3 breaker tripped (admin endpoint) and the database cold, the client's first retry
//     waits at least the agent's Retry-After, and the call succeeds after the breaker is reset.
//
// Gated on TSDB_PG_URL and COLDLOAD_S3:
//
//	TSDB_PG_URL='postgres://tsdb:tsdb-dev@localhost:5433/tsdb?sslmode=disable' COLDLOAD_S3=localhost:8333 \
//	  go test -count=1 -run TestClientGate -v ./cmd/controld
func TestClientGate(t *testing.T) {
	c := newCluster(t)
	c.locator, c.controldAddr = true, freeAddr(t)
	c.extra = `,"locator":{"ticket_ttl":"5s","cache_ttl":"10m","listen_ping":"500ms"}`
	c.startAgent("vm-a")
	c.startAgent("vm-b")
	c.startControld()
	waitListening(t, c)

	// 1. frontbench locator mode: setup (databases, table, an API key), then 8 s measured with a move each second.
	fb := filepath.Join(c.dir, "frontbench")
	if out, err := exec.Command("go", "build", "-o", fb, "../frontbench").CombinedOutput(); err != nil {
		t.Fatalf("build frontbench: %v\n%s", err, out)
	}
	sumPath := filepath.Join(c.dir, "locator.json")
	cmd := exec.Command(fb, "locator", "-locator", c.api, "-tenant", "fb", "-dbs", "4", "-conc", "16", "-write", "0.3",
		"-warmup", "1s", "-duration", "8s", "-move-every", "1s", "-timeout", "5s", "-setup", "-out", sumPath)
	cmd.Env = append(os.Environ(), "TSDB_ADMIN_TOKEN="+adminToken, "TSDB_API_KEY=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("frontbench locator: %v\n%s", err, out)
	}
	t.Logf("frontbench: %s", strings.TrimSpace(string(out)))
	t.Logf("controld answered %v resolves from its cache and loaded %v", c.metric("controld_locator_hits_total"), c.metric("controld_locator_loads_total"))
	var sum struct {
		Requests   int
		Codes      map[string]int
		Moves      int
		MoveFailed int `json:"move_failed"`
		Resolve    struct{ Hits, Misses int }
		DuringMove struct{ Requests, Failed int } `json:"during_move"`
	}
	b, _ := os.ReadFile(sumPath)
	if err := json.Unmarshal(b, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Moves < 4 || sum.MoveFailed != 0 || sum.DuringMove.Requests == 0 || sum.Resolve.Misses == 0 || sum.Resolve.Hits == 0 {
		t.Fatalf("run did not exercise moves and resolves: %s", b)
	}
	if sum.DuringMove.Failed != 0 || sum.Codes["ok"] != sum.Requests {
		t.Fatalf("requests failed during moves: %s", b)
	}

	// 2. The S3 breaker tripped on the owner of a cold database.
	var key map[string]string
	if code := c.call("POST", "/v1/tenants/brk/api-keys", map[string]string{"principal": "gate"}, &key); code != 200 {
		t.Fatalf("api key: %d", code)
	}
	if code := c.call("POST", "/v1/databases", map[string]string{"tenant": "brk", "name": "cold"}, nil); code != 200 {
		t.Fatalf("create: %d", code)
	}
	var mu sync.Mutex
	var retries []client.RetryEvent
	cl, err := client.New(client.Config{Locator: c.api, APIKey: key["key"], Tenant: "brk", Timeout: 20 * time.Second, MaxReroutes: 10,
		Hooks: client.Hooks{Retry: func(e client.RetryEvent) { mu.Lock(); retries = append(retries, e); mu.Unlock() }}})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx := context.Background()
	if _, err := cl.Do(ctx, client.Request{DB: "cold", Kind: front.KindWrite, SQL: "create table t (id integer not null)"}); err != nil {
		t.Fatal(err)
	}
	res, err := cl.Do(ctx, client.Request{DB: "cold", Kind: front.KindWrite, SQL: "insert into t (id) values (1)"})
	if err != nil {
		t.Fatal(err)
	}
	owner := c.agents[res.VM]
	admin := func(path string) {
		t.Helper()
		r, err := http.Post("http://"+owner.admin+path, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("%s: HTTP %d", path, r.StatusCode)
		}
	}
	admin("/admin/passivate?key=brk:cold")
	admin("/admin/breaker/s3:trip")
	mu.Lock()
	retries = nil
	mu.Unlock()
	go func() { time.Sleep(1500 * time.Millisecond); admin("/admin/breaker/s3:reset") }()
	t0 := time.Now()
	res, err = cl.Do(ctx, client.Request{DB: "cold", Kind: front.KindRead, SQL: "select count(*) from t"})
	took := time.Since(t0)
	if err != nil || res.Code != front.OK {
		t.Fatalf("read after the breaker reset: %+v %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(retries) == 0 {
		t.Fatal("no retry: the cold read did not meet the tripped breaker")
	}
	first := retries[0]
	if first.Code != string(front.Unavailable) || first.Hint < time.Second || first.Delay < first.Hint || took < first.Hint {
		t.Fatalf("first retry %+v (call took %v): want unavailable, a Retry-After >= 1 s, and a wait >= it", first, took)
	}
	t.Logf("S3 breaker tripped: first answer %s with Retry-After %v; the client waited %v; the call succeeded after %v (%d attempts)",
		first.Code, first.Hint, first.Delay, took, res.Attempts)
}
