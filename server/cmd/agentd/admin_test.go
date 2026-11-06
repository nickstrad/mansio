package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/objstore/s3test"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/file"
)

// adminAddr is the addr-file's third line (the admin listener), "" if there is none.
func adminAddr(path string) string {
	b, _ := os.ReadFile(path)
	if f := strings.Fields(string(b)); len(f) >= 3 {
		return f[2]
	}
	return ""
}

// adminCall posts to an admin endpoint and returns the status and the JSON answer.
func adminCall(t *testing.T, addr, pathQuery string) (int, map[string]any) {
	t.Helper()
	res, err := http.Post("http://"+addr+pathQuery, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("%s: answer is not JSON: %v", pathQuery, err)
	}
	return res.StatusCode, out
}

// mustAdmin fails the test unless the admin call answers ok.
func (a *running) mustAdmin(t *testing.T, pathQuery string) map[string]any {
	t.Helper()
	code, out := adminCall(t, a.admin, pathQuery)
	if code != http.StatusOK || out["code"] != "ok" {
		t.Fatalf("%s: %d %v", pathQuery, code, out)
	}
	return out
}

const adminExtra = `,"admin_listen":"127.0.0.1:0"`

// coldKeys claims dbs on a first agent, writes a row to each and stops it (everything written back), then
// starts a second one: every key is cold there.
func coldKeys(t *testing.T, r *durableRig, dbs ...string) *running {
	t.Helper()
	a := r.start(t)
	for _, db := range dbs {
		a.claim(t, db)
		writeRows(t, a, db, 1, 0, 1)
	}
	if code := a.stop(); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, a.stderr.String())
	}
	b := r.start(t)
	waitReady(t, b.life)
	return b
}

// keyRequests counts the S3 requests the fake saw for db's prefix.
func keyRequests(r *durableRig, db string) int {
	n := 0
	for _, q := range r.fake.Requests() {
		if strings.Contains(q.Path, durable.KeyPrefix(db)) {
			n++
		}
	}
	return n
}

// RI-16 gate: tripping the S3 breaker through the admin endpoint makes cold keys answer unavailable +
// Retry-After without a single S3 request for them, while warm keys keep serving reads and writes; the trip
// holds past the cool-down; a reset restores cold activations. Both are logged (ADMIN.*, S3.BREAKER_*).
func TestAdminBreakerTripFailsFastWithoutS3Calls(t *testing.T) {
	r := newDurableRig(t, adminExtra)
	b := coldKeys(t, r, "db-0", "db-1")
	if b.admin == "" {
		t.Fatal("no admin listener address in the addr-file")
	}
	count(t, b, "db-0", 1) // warm from here on

	out := b.mustAdmin(t, "/admin/breaker/s3:trip")
	if out["state"] != "open" || out["forced"] != true || out["changed"] != true {
		t.Fatalf("trip answer %v", out)
	}
	if out := b.mustAdmin(t, "/admin/breaker/s3:trip"); out["changed"] != false {
		t.Fatalf("second trip %v, want changed false", out)
	}
	if v := b.metric(t, `mansio_breaker_state{dep="s3",state="open"}`); v != "1" {
		t.Fatalf("breaker open gauge %q", v)
	}
	before := keyRequests(r, "db-1")
	for range 3 { // the rig's cool-down is 3 s; a forced trip ignores it
		ans := b.query(t, "db-1", 1, front.KindRead, `SELECT k FROM kv`)
		if ans.Code != front.Unavailable || ans.status != http.StatusServiceUnavailable {
			t.Fatalf("cold key with the breaker tripped: %d %+v", ans.status, ans.Response)
		}
		if ra, _ := strconv.Atoi(ans.retryAfter); ra < 1 {
			t.Fatalf("Retry-After %q", ans.retryAfter)
		}
	}
	if n := keyRequests(r, "db-1") - before; n != 0 {
		t.Fatalf("%d S3 requests for the cold key while the breaker was tripped, want 0", n)
	}
	writeRows(t, b, "db-0", 1, 1, 2) // warm writes are local
	if n := count(t, b, "db-0", 1); n != 2 {
		t.Fatalf("warm key: %d rows", n)
	}

	if out := b.mustAdmin(t, "/admin/breaker/s3:reset"); out["state"] != "closed" || out["forced"] != false {
		t.Fatalf("reset answer %v", out)
	}
	if n := count(t, b, "db-1", 1); n != 1 {
		t.Fatalf("cold key after reset: %d rows", n)
	}
	if keyRequests(r, "db-1") == before {
		t.Fatal("no S3 request for the cold key after the reset")
	}
	log := r.log()
	for _, ev := range []string{"ADMIN.BREAKER_TRIP", "ADMIN.BREAKER_RESET", "S3.BREAKER_OPEN", "S3.BREAKER_CLOSED", `"manual":true`} {
		if !strings.Contains(log, ev) {
			t.Errorf("log lacks %s", ev)
		}
	}
}

// RI-16 gate: lowering fetch:limit at run time applies to the next fetches. With the limit at 1 and slow
// fetches, three cold keys queried at once fetch one at a time (the others wait in the bulkhead's queue, then
// all answer ok); raised to 3, they fetch together.
func TestAdminFetchLimitAppliesToTheNextFetches(t *testing.T) {
	// Generous S3 deadlines: each fetch is dribbled out at 16 KiB/s on purpose.
	r := newDurableRig(t, adminExtra+`,"s3":{"op_timeout":"5s","stall_timeout":"5s"}`)
	dbs := []string{"db-1", "db-2", "db-3"}
	b := coldKeys(t, r, dbs...)
	if out := b.mustAdmin(t, "/admin/fetch:limit?n=1"); out["fetch_limit"] != 1.0 || out["previous"] != 16.0 {
		t.Fatalf("fetch:limit answer %v", out)
	}
	r.fake.Inject(s3test.Fault{Mode: s3test.Dribble, Rate: 16 << 10, Method: http.MethodGet})

	// peaks queries every key at once and returns the most fetches in flight and queued seen meanwhile.
	peaks := func() (inFlight, queued int) {
		var wg sync.WaitGroup
		for _, db := range dbs {
			wg.Go(func() { count(t, b, db, 1) })
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		for {
			select {
			case <-done:
				return inFlight, queued
			default:
			}
			n, _ := strconv.Atoi(b.metric(t, `mansio_vactor{stat="fetches_in_flight"}`))
			q, _ := strconv.Atoi(b.metric(t, `mansio_vactor{stat="fetch_queued"}`))
			inFlight, queued = max(inFlight, n), max(queued, q)
			time.Sleep(5 * time.Millisecond)
		}
	}
	n, q := peaks()
	t.Logf("limit 1: peak %d fetches in flight, %d queued", n, q)
	if n != 1 || q < 1 {
		t.Fatalf("limit 1: peak %d fetches in flight and %d queued, want 1 and >= 1", n, q)
	}

	b.mustAdmin(t, "/admin/fetch:limit?n=3")
	for _, db := range dbs { // cold again (reads left them clean: nothing to upload)
		b.mustAdmin(t, "/admin/passivate?key="+db)
	}
	n, _ = peaks()
	t.Logf("limit 3: peak %d fetches in flight", n)
	if n < 2 {
		t.Fatalf("limit 3: peak %d fetches in flight, want the raise to let them overlap", n)
	}
	if !strings.Contains(r.log(), "ADMIN.FETCH_LIMIT") || !strings.Contains(r.log(), "ADMIN.PASSIVATE") {
		t.Error("admin actions not logged")
	}
}

// The other controls and every error answer: passivate writes back and drops a warm key; loglevel changes
// what is logged; drain sheds queries and turns /readyz and /healthz to draining; bad input is bad_request,
// unknown breakers and paths not_found.
func TestAdminControlsAndErrors(t *testing.T) {
	r := newDurableRig(t, adminExtra)
	a := r.start(t)
	a.claim(t, "db-1")
	writeRows(t, a, "db-1", 1, 0, 3)

	a.mustAdmin(t, "/admin/passivate?key=db-1")
	if _, h := a.control(t, healthPath, nil); h["active"].(float64) != 0 {
		t.Fatalf("still warm after passivate: %v", h)
	}
	if m := r.manifest(t, "db-1"); m.Generation < 1 {
		t.Fatalf("passivate did not write back: %+v", m)
	}
	if n := count(t, a, "db-1", 1); n != 3 {
		t.Fatalf("after passivate: %d rows", n)
	}

	if out := a.mustAdmin(t, "/admin/loglevel?level=debug"); out["level"] != "debug" || out["previous"] != "info" {
		t.Fatalf("loglevel answer %v", out)
	}
	a.mustAdmin(t, "/admin/loglevel?level=warn")

	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{"/admin/fetch:limit?n=0", "bad_request", 400}, // off point: 1 is the least
		{"/admin/fetch:limit?n=x", "bad_request", 400},
		{"/admin/fetch:limit", "bad_request", 400},
		{"/admin/passivate?key=", "bad_request", 400},
		{"/admin/loglevel?level=trace", "bad_request", 400},
		{"/admin/breaker/s3:open", "bad_request", 400},
		{"/admin/breaker/s3", "bad_request", 400},
		{"/admin/breaker/nope:trip", "not_found", 404},
		{"/admin/nothing", "not_found", 404},
	} {
		if code, out := adminCall(t, a.admin, tc.path); code != tc.status || out["code"] != tc.want {
			t.Errorf("%s: %d %v, want %d %s", tc.path, code, out, tc.status, tc.want)
		}
	}
	if code, _ := adminCall(t, a.admin, "/admin/fetch:limit?n=1"); code != http.StatusOK {
		t.Fatalf("on point n=1: %d", code)
	}
	if res, err := http.Get("http://" + a.admin + "/admin/drain"); err != nil || res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET on a control: %v %v, want 404 (controls are POST only)", res.StatusCode, err)
	}

	a.mustAdmin(t, "/admin/drain")
	if ans := a.query(t, "db-1", 1, front.KindRead, `SELECT 1`); ans.Code != front.Shed {
		t.Fatalf("query while draining: %+v", ans.Response)
	}
	if code, body := getStatus(t, "http://"+a.life+front.ReadyPath); code != http.StatusServiceUnavailable || body["status"] != "draining" {
		t.Fatalf("readyz while draining: %d %v", code, body)
	}
	if code := a.stop(); code != exitOK { // the shutdown's drain finds it done
		t.Fatalf("exit %d after an admin drain:\n%s", code, a.stderr.String())
	}
	log := r.log()
	if !strings.Contains(log, "ADMIN.LOGLEVEL") || !strings.Contains(log, "ADMIN.DRAIN") || !strings.Contains(log, "ADMIN.REFUSED") {
		t.Errorf("admin actions not logged:\n%s", log)
	}
}

// Static mode serves drain and loglevel; the durable-only controls answer not_found.
func TestAdminStaticMode(t *testing.T) {
	cfg := fixture(t, []string{"db-1"}, `,"health_listen":"127.0.0.1:0"`+adminExtra)
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	waitAddr(t, addrFile, done)
	defer func() { cancel(); <-done }()
	admin := adminAddr(addrFile)
	for path, want := range map[string]int{"/admin/loglevel?level=debug": 200, "/admin/breaker/s3:trip": 404,
		"/admin/fetch:limit?n=2": 404, "/admin/passivate?key=db-1": 404, "/admin/backup?key=db-1": 404, "/admin/drain": 200} {
		if code, out := adminCall(t, admin, path); code != want {
			t.Errorf("%s: %d %v, want %d", path, code, out, want)
		}
	}
}

// OS-11: durable mode over TLS without -dev. S3 is reached over HTTPS with the private CA of ca_file; the
// claim's create stand-in is refused (controld creates); a claim of a row the control plane wrote serves.
func TestDurableOverTLSWithoutDev(t *testing.T) {
	r := newDurableRig(t, "")
	tlsS3 := s3test.NewTLS()
	t.Cleanup(tlsS3.Close)
	os.WriteFile(filepath.Join(r.dir, "ca.pem"), tlsS3.CAPEM(), 0o644)
	cfg, _ := os.ReadFile(r.cfg)
	patched := strings.Replace(strings.Replace(string(cfg), r.proxy.Addr(), tlsS3.Endpoint(), 1),
		`"s3":{`, `"s3":{"secure":true,"ca_file":"ca.pem",`, 1)
	os.WriteFile(r.cfg, []byte(patched), 0o644)
	r.noDev = true
	a := r.start(t)
	waitReady(t, a.life)

	code, out := a.control(t, claimPath, map[string]any{"db": "db-1", "create": true})
	if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["message"]), "-dev only") {
		t.Fatalf("create without -dev: %d %v", code, out)
	}
	pl, err := file.Open(filepath.Join(r.dir, "placement"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pl.CAS(context.Background(), "db-1", owner.Place{}, owner.Place{Owner: "vm-1", Epoch: 1, State: owner.PlaceActive}); err != nil {
		t.Fatal(err)
	}
	if code, out := a.control(t, claimPath, map[string]any{"db": "db-1"}); code != http.StatusOK || out["state"] != "active" {
		t.Fatalf("claim of the control plane's row: %d %v", code, out)
	}
	writeRows(t, a, "db-1", 1, 0, 2)
	if code, out := a.control(t, fencePath, map[string]any{"db": "db-1"}); code != http.StatusOK {
		t.Fatalf("fence: %d %v", code, out)
	}
	if tlsS3.Count(http.MethodPut) < 2 {
		t.Fatalf("%d PUTs reached the TLS S3, want the manifest and a generation", tlsS3.Count(http.MethodPut))
	}
}
