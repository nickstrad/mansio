package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/owner"
)

// The CP-5 gate (control-plane plan §3, PG-10): two agentd processes that fetch their ticket keys from controld,
// and clients that only know controld's resolve API. Gated on TSDB_PG_URL and COLDLOAD_S3.
//
//	TSDB_PG_URL='postgres://tsdb:tsdb-dev@localhost:5433/tsdb?sslmode=disable' COLDLOAD_S3=localhost:8333 \
//	  go test -count=1 -run TestLocatorGate -v ./cmd/controld

// route is a resolve answer as a client keeps it.
type route struct {
	Code      string    `json:"code"`
	DB        string    `json:"db"`
	VM        string    `json:"vm"`
	Endpoint  string    `json:"endpoint"`
	Epoch     uint64    `json:"epoch"`
	Ticket    string    `json:"ticket"`
	TTLMs     int64     `json:"ttl_ms"`
	ExpiresAt time.Time `json:"expires_at"`
}

// resolve calls POST /v1/resolve; it returns the status, the Retry-After header and the answer.
func (c *cluster) resolve(apiKey, tenant, db string, minEpoch uint64) (int, string, route) {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"tenant": tenant, "db": db, "min_epoch": minEpoch})
	req, _ := http.NewRequest(http.MethodPost, c.api+"/v1/resolve", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", route{}
	}
	defer res.Body.Close()
	var r route
	json.NewDecoder(res.Body).Decode(&r)
	return res.StatusCode, res.Header.Get("Retry-After"), r
}

// send runs one statement with a route's ticket at the route's endpoint.
func send(r route, kind front.Kind, id, sql string) (front.Response, error) {
	body := map[string]any{"db": r.DB, "kind": kind, "sql": sql, "args": []any{}}
	if id != "" {
		body["request_id"] = id
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "http://"+r.Endpoint+front.QueryPath, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+r.Ticket)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return front.Response{}, err
	}
	defer res.Body.Close()
	var out front.Response
	err = json.NewDecoder(res.Body).Decode(&out)
	return out, err
}

// metric reads one unlabeled sample from controld's /metrics.
func (c *cluster) metric(name string) float64 {
	c.t.Helper()
	res, err := http.Get(c.api + "/metrics")
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), name+" "); ok {
			f, _ := strconv.ParseFloat(v, 64)
			return f
		}
	}
	c.t.Fatalf("metric %s missing", name)
	return 0
}

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestLocatorGate(t *testing.T) {
	c := newCluster(t)
	c.locator, c.controldAddr = true, freeAddr(t)
	c.extra = `,"locator":{"ticket_ttl":"4s","cache_ttl":"10m","listen_ping":"500ms"}`
	c.startAgent("vm-a")
	c.startAgent("vm-b")
	c.startControld()

	var key map[string]string
	if code := c.call("POST", "/v1/tenants/gate/api-keys", map[string]string{"principal": "app"}, &key); code != 200 || key["key"] == "" {
		t.Fatalf("create api key: %d %v", code, key)
	}
	apiKey := key["key"]
	for _, name := range []string{"loc", "mv"} {
		if code := c.call("POST", "/v1/databases", map[string]string{"tenant": "gate", "name": name}, nil); code != 200 {
			t.Fatalf("create %s: %d", name, code)
		}
	}
	waitListening(t, c)

	// Answers other than ok.
	var other map[string]string
	c.call("POST", "/v1/tenants/other/api-keys", map[string]string{"principal": "x", "perms": "read"}, &other)
	for _, tc := range []struct {
		key, tenant, db string
		status          int
	}{
		{"tsdb_nope", "gate", "loc", 401},
		{other["key"], "gate", "loc", 403},
		{apiKey, "gate", "nothing", 404},
		{apiKey, "gate", "Bad Name", 400},
	} {
		if code, _, r := c.resolve(tc.key, tc.tenant, tc.db, 0); code != tc.status {
			t.Fatalf("resolve %s/%s: %d %+v, want %d", tc.tenant, tc.db, code, r, tc.status)
		}
	}
	// A row MOVING answers moving with Retry-After (here forced directly in the authority).
	mv, _ := c.pl.Get(context.Background(), "gate:mv")
	if err := c.pl.CAS(context.Background(), "gate:mv", mv, owner.Place{Owner: mv.Owner, Epoch: mv.Epoch, Gen: mv.Gen, State: owner.PlaceMoving}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "moving answer", func() bool {
		code, ra, _ := c.resolve(apiKey, "gate", "mv", 0)
		return code == 503 && ra == "1"
	})

	// The client's first route; agents verify its ticket with keys fetched from controld.
	_, _, r1 := c.resolve(apiKey, "gate", "loc", 0)
	waitFor(t, "first write through the locator", func() bool {
		res, err := send(r1, front.KindWrite, "init", "create table t (id integer not null)")
		return err == nil && res.Code == front.OK
	})

	// 1. After a move, clients reach the new VM within one re-resolve.
	to := map[string]string{"vm-a": "vm-b", "vm-b": "vm-a"}
	if code := c.call("POST", "/v1/databases/gate:loc/move", map[string]string{"to": to[r1.VM]}, nil); code != 200 {
		t.Fatalf("move: %d", code)
	}
	res, err := send(r1, front.KindWrite, "w1", "insert into t (id) values (1)")
	if err != nil || res.Code == front.OK {
		t.Fatalf("the old route still works after the move: %+v %v", res, err)
	}
	code, _, r2 := c.resolve(apiKey, "gate", "loc", r1.Epoch+1)
	if code != 200 || r2.VM != to[r1.VM] || r2.Epoch != r1.Epoch+1 {
		t.Fatalf("re-resolve after %s: %d %+v", res.Code, code, r2)
	}
	if res, err := send(r2, front.KindWrite, "w1", "insert into t (id) values (1)"); err != nil || res.Code != front.OK {
		t.Fatalf("write after one re-resolve: %+v %v", res, err)
	}
	t.Logf("after the move: old route answered %s; one re-resolve (min_epoch %d) reached %s at epoch %d", res.Code, r1.Epoch+1, r2.VM, r2.Epoch)

	// 2. A cached locator sees a move before its TTL (10 min) through NOTIFY.
	c.resolve(apiKey, "gate", "loc", 0)
	hits := c.metric("controld_locator_hits_total")
	if _, _, r := c.resolve(apiKey, "gate", "loc", 0); r.Epoch != r2.Epoch || c.metric("controld_locator_hits_total") != hits+1 {
		t.Fatalf("not cached: %+v", r)
	}
	if code := c.call("POST", "/v1/databases/gate:loc/move", map[string]string{"to": r1.VM}, nil); code != 200 {
		t.Fatalf("move back: %d", code)
	}
	moved := time.Now()
	var r3 route
	waitFor(t, "cached locator sees the move", func() bool {
		_, _, r3 = c.resolve(apiKey, "gate", "loc", 0)
		return r3.Epoch == r2.Epoch+1
	})
	t.Logf("cached locator saw the move %v after the move returned (cache_ttl 10m)", time.Since(moved))
	if r3.VM != r1.VM {
		t.Fatalf("after the move back: %+v", r3)
	}
	// A re-resolve storm after the move is one load per (database, primary).
	loads := c.metric("controld_locator_loads_total")
	c.call("POST", "/v1/databases/gate:loc/move", map[string]string{"to": r2.VM}, nil)
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() { c.resolve(apiKey, "gate", "loc", r3.Epoch+1) })
	}
	wg.Wait()
	if n := c.metric("controld_locator_loads_total") - loads; n > 4 {
		t.Fatalf("64 concurrent re-resolves made %v loads", n)
	}
	_, _, r4 := c.resolve(apiKey, "gate", "loc", r3.Epoch+1)
	if r4.Epoch != r3.Epoch+1 {
		t.Fatalf("after the storm: %+v", r4)
	}

	// 3. Kill the LISTEN connection while a move runs: the listener reconnects, the cache is reset, and resolve
	// is correct.
	c.resolve(apiKey, "gate", "loc", 0) // cached at r4's epoch
	resets := c.metric("controld_locator_resets_total")
	admin, err := pgx.Connect(context.Background(), c.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	moveDone := make(chan int, 1)
	go func() { moveDone <- c.call("POST", "/v1/databases/gate:loc/move", map[string]string{"to": r3.VM}, nil) }()
	for i := 0; i < 20; i++ { // keep killing it for the length of a move (~30 ms)
		admin.Exec(context.Background(), "select pg_terminate_backend(pid) from pg_stat_activity where application_name = 'controld/listen'")
		time.Sleep(5 * time.Millisecond)
	}
	if code := <-moveDone; code != 200 {
		t.Fatalf("move during listener kills: %d", code)
	}
	var r5 route
	waitFor(t, "resolve after the listener reconnected", func() bool {
		_, _, r5 = c.resolve(apiKey, "gate", "loc", 0)
		return r5.Epoch == r4.Epoch+1 && c.metric("controld_locator_listening") == 1
	})
	if r5.VM != r3.VM || c.metric("controld_locator_resets_total") <= resets {
		t.Fatalf("after the kills: %+v, resets %v → %v", r5, resets, c.metric("controld_locator_resets_total"))
	}
	t.Logf("listener killed during a move: %v reconnects, resolve answered %s at epoch %d", c.metric("controld_locator_resets_total")-resets, r5.VM, r5.Epoch)
	if res, err := send(r5, front.KindRead, "", "select count(*) from t"); err != nil || res.Code != front.OK {
		t.Fatalf("read at the resolved owner: %+v %v", res, err)
	}

	// Resolve latency over HTTP (loopback): cache hits, and primary reads (min_epoch above the cache).
	lat := func(minEpoch uint64) time.Duration {
		var ds []time.Duration
		for range 200 {
			t0 := time.Now()
			c.resolve(apiKey, "gate", "loc", minEpoch)
			ds = append(ds, time.Since(t0))
		}
		slices.Sort(ds)
		return ds[len(ds)/2]
	}
	t.Logf("resolve p50 over HTTP: cache hit %v, primary read %v", lat(0), lat(r5.Epoch+1))

	// 4. With the locator stopped, traffic continues until the tickets expire.
	_, _, r6 := c.resolve(apiKey, "gate", "loc", 0)
	c.stopCtl()
	if code, _, _ := c.resolve(apiKey, "gate", "loc", 0); code != 0 {
		t.Fatalf("controld still answers: %d", code)
	}
	served := 0
	for time.Now().Before(r6.ExpiresAt.Add(-500 * time.Millisecond)) {
		res, err := send(r6, front.KindWrite, fmt.Sprintf("off-%d", served), "insert into t (id) values (2)")
		if err != nil || res.Code != front.OK {
			t.Fatalf("with the locator stopped, before the ticket expires (%v left): %+v %v", time.Until(r6.ExpiresAt), res, err)
		}
		served++
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(time.Until(r6.ExpiresAt.Add(1500 * time.Millisecond))) // agents' clock_skew is 1 s
	if res, err := send(r6, front.KindRead, "", "select 1"); err != nil || res.Code != front.TicketInvalid {
		t.Fatalf("after the ticket expired: %+v %v", res, err)
	}
	t.Logf("locator stopped: %d writes served on the cached ticket until it expired, then %s", served, front.TicketInvalid)
}

func waitListening(t *testing.T, c *cluster) {
	waitFor(t, "placement listener", func() bool { return c.metric("controld_locator_listening") == 1 })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached in 10 s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
