package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/faultnet"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/front/fronttest"
	"github.com/nickstrad/ts-db-go/internal/objstore"
	"github.com/nickstrad/ts-db-go/internal/objstore/s3test"
	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/placement/file"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

const controlToken = "test-control-token-0123456789"

// durableRig is one agent's durable-mode setup: a config over the in-process misbehaving S3 (s3test), reached
// through a faultnet proxy, or over a real S3 (COLDLOAD_S3). Each start runs agentd on the same directories,
// so a restart keeps its owner records and placement rows (the cache directory is emptied, as on a real start).
type durableRig struct {
	dir, cfg, prefix string
	fake             *s3test.Server // nil against a real S3
	proxy            *faultnet.Proxy
	bucket           string
	endpoint         string // the S3 behind the proxy
	access, secret   string
}

// newDurableRig writes keys, secrets and a config. extra is appended to the config object's fields.
func newDurableRig(t *testing.T, extra string) *durableRig {
	t.Helper()
	r := &durableRig{dir: t.TempDir(), bucket: "bkt", prefix: "agentd-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "/"}
	endpoint, access, secret := "", "a", "b"
	if ep := os.Getenv("AGENTD_REAL_S3"); ep != "" { // set by s3Rig for the gated runs
		endpoint, access, secret, r.bucket = ep, "coldload", "coldload-secret", "coldload"
		t.Cleanup(func() {
			if st, err := objstore.New(objstore.Config{Endpoint: ep, AccessKey: access, SecretKey: secret, Bucket: r.bucket}); err == nil {
				st.DeletePrefix(context.Background(), r.prefix)
			}
		})
	} else {
		r.fake = s3test.New()
		t.Cleanup(r.fake.Close)
		endpoint = r.fake.Endpoint()
	}
	r.endpoint, r.access, r.secret = endpoint, access, secret
	var err error
	if r.proxy, err = faultnet.New(endpoint); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.proxy.Close() })
	keys, _ := json.Marshal(ticket.PublicKeyFile{Keys: []ticket.PublicKeyEntry{{KID: "k1", PublicKey: ticket.EncodeKey(iss.Pub)}}})
	os.WriteFile(filepath.Join(r.dir, "keys.json"), keys, 0o644)
	sec, _ := json.Marshal(Secrets{S3AccessKey: access, S3SecretKey: secret, ControlToken: controlToken})
	os.WriteFile(filepath.Join(r.dir, "secrets.json"), sec, 0o600)
	cfg := fmt.Sprintf(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","vm":"vm-1","keys_file":"keys.json","log_file":"agentd.log",
		"s3":{"endpoint":%q,"bucket":%q,"prefix":%q,"op_timeout":"200ms","stall_timeout":"200ms","breaker":{"min_calls":3,"window":"1s","cool_down":"3s"}},
		"secrets_file":"secrets.json","cache_dir":"cache","owner_dir":"owner","placement_dir":"placement",
		"ready_interval":"50ms","ready_timeout":"40ms","default_timeout":"8s","max_timeout":"10s","statement_timeout":"2s",
		"maintenance":{"interval":"50ms"}%s}`, r.proxy.Addr(), r.bucket, r.prefix, extra)
	r.cfg = filepath.Join(r.dir, "agentd.json")
	os.WriteFile(r.cfg, []byte(cfg), 0o644)
	return r
}

// running is one started agent.
type running struct {
	addr, life string
	stderr     *syncBuf
	stop       func() int // cancel (in-process) or SIGTERM (process) and wait; returns the exit code
	kill       func()     // SIGKILL (process only)
}

// start runs agentd in-process and waits until it serves.
func (r *durableRig) start(t *testing.T) *running {
	t.Helper()
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &syncBuf{}
	done := make(chan int, 1)
	go func() { done <- realMain(ctx, []string{"-config", r.cfg, "-addr-file", addrFile}, stderr) }()
	addr := waitAddr(t, addrFile, done)
	run := &running{addr: addr, life: lifecycleAddr(t, addrFile), stderr: stderr}
	stopped := false
	run.stop = func() int {
		if stopped {
			return -1
		}
		stopped = true
		cancel()
		return <-done
	}
	t.Cleanup(func() { run.stop() })
	return run
}

// startProcess runs agentd as a child process (the test binary through TestHelperAgentd).
func (r *durableRig) startProcess(t *testing.T) *running {
	t.Helper()
	addrFile := filepath.Join(t.TempDir(), "addr")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAgentd$")
	args, _ := json.Marshal([]string{"-config", r.cfg, "-addr-file", addrFile})
	cmd.Env = append(os.Environ(), "AGENTD_HELPER=1", "AGENTD_ARGS="+string(args))
	stderr := &syncBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill(); <-exited })
	run := &running{addr: waitAddr(t, addrFile, exited), life: lifecycleAddr(t, addrFile), stderr: stderr}
	run.stop = func() int {
		cmd.Process.Signal(os.Interrupt)
		<-exited
		exited <- nil // for Cleanup
		return cmd.ProcessState.ExitCode()
	}
	run.kill = func() {
		cmd.Process.Kill()
		<-exited
		exited <- nil
	}
	return run
}

// control calls a lifecycle endpoint with the control token.
func (a *running) control(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	method, rd := http.MethodGet, (*bytes.Reader)(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		method, rd = http.MethodPost, bytes.NewReader(b)
	}
	var req *http.Request
	if rd != nil {
		req, _ = http.NewRequest(method, "http://"+a.life+path, rd)
	} else {
		req, _ = http.NewRequest(method, "http://"+a.life+path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+controlToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// claim creates (if needed) and claims db, failing the test unless it is active.
func (a *running) claim(t *testing.T, db string) uint64 {
	t.Helper()
	code, out := a.control(t, claimPath, map[string]any{"db": db, "create": true})
	if code != http.StatusOK || out["state"] != "active" {
		t.Fatalf("claim %s: %d %v", db, code, out)
	}
	return uint64(out["epoch"].(float64))
}

var reqSeq atomic.Int64

// answer is one query's outcome.
type answer struct {
	status     int
	retryAfter string
	front.Response
}

// query sends one statement for db at epoch (writes get a fresh request id).
func (a *running) query(t *testing.T, db string, epoch uint64, kind front.Kind, sql string, args ...any) answer {
	t.Helper()
	id := ""
	if kind == front.KindWrite {
		id = "r-" + strconv.FormatInt(reqSeq.Add(1), 10)
	}
	return a.queryID(t, db, epoch, id, kind, sql, args...)
}

// queryID sends one statement with the given request id ("" = none).
func (a *running) queryID(t *testing.T, db string, epoch uint64, id string, kind front.Kind, sql string, args ...any) answer {
	t.Helper()
	if args == nil {
		args = []any{}
	}
	body := map[string]any{"db": db, "kind": kind, "sql": sql, "args": args}
	if id != "" {
		body["request_id"] = id
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "http://"+a.addr+front.QueryPath, bytes.NewReader(b))
	c := iss.Claims(db, "vm-1", time.Now(), time.Minute)
	c.Epoch = epoch
	req.Header.Set("Authorization", "Bearer "+iss.Mint(t, c))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out answer
	json.NewDecoder(res.Body).Decode(&out.Response)
	out.status, out.retryAfter = res.StatusCode, res.Header.Get("Retry-After")
	return out
}

// must fails the test unless the answer is ok.
func (ans answer) must(t *testing.T) answer {
	t.Helper()
	if ans.Code != front.OK {
		t.Fatalf("answer %d %+v", ans.status, ans.Response)
	}
	return ans
}

// metric returns the value of the first /metrics line starting with series ("" if none).
func (a *running) metric(t *testing.T, series string) string {
	t.Helper()
	res, err := http.Get("http://" + a.life + metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, series+" ") {
			return strings.TrimPrefix(line, series+" ")
		}
	}
	return ""
}

// log returns the agent log so far.
func (r *durableRig) log() string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "agentd.log"))
	return string(b)
}

// manifest reads db's stored manifest straight from S3 (past the proxy).
func (r *durableRig) manifest(t *testing.T, db string) durable.Manifest {
	t.Helper()
	st, err := objstore.New(objstore.Config{Endpoint: r.endpoint, AccessKey: r.access, SecretKey: r.secret, Bucket: r.bucket})
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := objstore.NewDurable(st, r.prefix).ReadManifest(context.Background(), db)
	if err != nil {
		t.Fatalf("manifest of %s: %v", db, err)
	}
	return m
}

// The kill -9 e2e and the lifecycle endpoints against a real S3 (SeaweedFS), when COLDLOAD_S3 is set.
func TestDurableAgainstRealS3(t *testing.T) {
	ep := os.Getenv("COLDLOAD_S3")
	if ep == "" {
		t.Skip("set COLDLOAD_S3=host:port to run agentd against a real S3")
	}
	t.Setenv("AGENTD_REAL_S3", ep)
	t.Run("kill -9", TestKill9RestartReconcile)
	t.Run("lifecycle", TestDurableLifecycleEndpoints)
	t.Run("restart", TestRestartServesKeysAsTheyReconcile)
	t.Run("sigterm", TestSIGTERMWithDirtyKeys)
}

// writeRows creates the kv table (if needed) and inserts rows from..to-1 at epoch.
func writeRows(t *testing.T, a *running, db string, epoch uint64, from, to int) {
	t.Helper()
	a.query(t, db, epoch, front.KindWrite, `CREATE TABLE IF NOT EXISTS kv (k INTEGER PRIMARY KEY, v TEXT NOT NULL)`).must(t)
	for i := from; i < to; i++ {
		a.query(t, db, epoch, front.KindWrite, fronttest.PutSQL, i, fmt.Sprintf("v%d", i)).must(t)
	}
}

// count returns how many kv rows db holds at epoch.
func count(t *testing.T, a *running, db string, epoch uint64) int64 {
	t.Helper()
	return a.query(t, db, epoch, front.KindRead, `SELECT k FROM kv`).must(t).Count
}

// Flow B over HTTP: claim (create) → write → read → health → fence → forget, with the identity check and the
// error codes the control plane relies on.
func TestDurableLifecycleEndpoints(t *testing.T) {
	r := newDurableRig(t, "")
	a := r.start(t)
	waitReady(t, a.life)
	if e := a.claim(t, "db-1"); e != 1 {
		t.Fatalf("epoch %d", e)
	}
	if e := a.claim(t, "db-1"); e != 1 { // idempotent
		t.Fatalf("second claim: epoch %d", e)
	}
	writeRows(t, a, "db-1", 1, 0, 3)
	if n := count(t, a, "db-1", 1); n != 3 {
		t.Fatalf("%d rows", n)
	}
	if ans := a.query(t, "db-1", 2, front.KindRead, `SELECT 1`); ans.Code != front.WrongOwner {
		t.Fatalf("epoch 2: %+v", ans)
	}
	if ans := a.query(t, "db-2", 1, front.KindRead, `SELECT 1`); ans.Code != front.WrongOwner {
		t.Fatalf("unclaimed db: %+v", ans)
	}
	code, h := a.control(t, healthPath, nil)
	if code != http.StatusOK || h["active"].(float64) != 1 || h["owned"].(map[string]any)["active"].(float64) != 1 || h["bytes"].(float64) <= 0 {
		t.Fatalf("health %d %v", code, h)
	}

	// Errors: identity, bad input, illegal transitions.
	req, _ := http.NewRequest(http.MethodPost, "http://"+a.life+fencePath, strings.NewReader(`{"db":"db-1"}`))
	req.Header.Set("Authorization", "Bearer wrong-token-0123456789")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %v %v", res.StatusCode, err)
	}
	for _, tc := range []struct {
		path string
		body any
		code int
		want string
	}{
		{claimPath, map[string]any{"db": "~canary"}, 400, "bad_request"},
		{claimPath, map[string]any{"db": "db-1", "x": 1}, 400, "bad_request"},
		{claimPath, map[string]any{"db": "db-9"}, 503, "unavailable"}, // no placement row and no create: authority says not found
		{forgetPath, map[string]any{"db": "db-1"}, 409, "illegal"},
		{fencePath, map[string]any{"db": "db-9"}, 409, "wrong_owner"},
	} {
		if code, out := a.control(t, tc.path, tc.body); code != tc.code || out["code"] != tc.want {
			t.Errorf("%s %v: %d %v, want %d %s", tc.path, tc.body, code, out, tc.code, tc.want)
		}
	}

	code, out := a.control(t, fencePath, map[string]any{"db": "db-1"})
	if code != http.StatusOK || out["epoch"].(float64) != 1 || out["gen"].(float64) < 1 {
		t.Fatalf("fence %d %v", code, out)
	}
	if m := r.manifest(t, "db-1"); m.Generation != uint64(out["gen"].(float64)) || m.Epoch != 1 {
		t.Fatalf("manifest %+v after fence %v", m, out)
	}
	if ans := a.query(t, "db-1", 1, front.KindRead, `SELECT 1`); ans.Code != front.Fenced {
		t.Fatalf("after fence: %+v", ans)
	}
	if code, out := a.control(t, claimPath, map[string]any{"db": "db-1"}); code != http.StatusConflict || out["code"] != "fenced" {
		t.Fatalf("claim of a fenced key at its epoch: %d %v", code, out)
	}
	if code, out := a.control(t, forgetPath, map[string]any{"db": "db-1"}); code != http.StatusOK {
		t.Fatalf("forget %d %v", code, out)
	}
	if code := a.stop(); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, a.stderr.String())
	}
	for _, ev := range []string{"OWNER.CLAIMED", "OWNER.FENCED", "OWNER.FORGOTTEN", "VACTOR.SHUTDOWN", "OWNER.RECONCILE_DONE"} {
		if !strings.Contains(r.log(), ev) {
			t.Errorf("log lacks %s", ev)
		}
	}
}

// CP-1 gate, RI-07: after a restart every key is refused unavailable until its own reconcile finishes, the
// first key serves before the last one is reconciled, and every write that was written back is there.
func TestRestartServesKeysAsTheyReconcile(t *testing.T) {
	const keys = 12
	r := newDurableRig(t, `,"startup_workers":1`)
	a := r.start(t)
	for i := range keys {
		db := fmt.Sprintf("db-%02d", i)
		a.claim(t, db)
		writeRows(t, a, db, 1, 0, 2)
	}
	if code := a.stop(); code != exitOK { // the shutdown writes every dirty key back
		t.Fatalf("exit %d:\n%s", code, a.stderr.String())
	}

	r.proxy.Set(faultnet.Delayed(40*time.Millisecond), 0) // every S3 exchange is slow: reconcile takes a while
	before := len(r.log())
	reconciled := func() bool { return strings.Contains(r.log()[before:], "OWNER.RECONCILE_DONE") }
	t0 := time.Now()
	b := r.start(t)
	var first, all time.Duration
	served := map[string]bool{}
	for time.Since(t0) < 20*time.Second && len(served) < keys {
		for i := range keys {
			db := fmt.Sprintf("db-%02d", i)
			if served[db] {
				continue
			}
			ans := b.query(t, db, 1, front.KindRead, `SELECT k FROM kv`)
			switch {
			case ans.Code == front.OK:
				if ans.Count != 2 {
					t.Fatalf("%s: %d rows after restart, want 2", db, ans.Count)
				}
				served[db] = true
				if first == 0 {
					first = time.Since(t0)
				}
			case ans.Code != front.Unavailable || ans.retryAfter == "":
				t.Fatalf("%s before its reconcile: %d %+v", db, ans.status, ans.Response)
			}
		}
		if all == 0 && reconciled() {
			all = time.Since(t0)
		}
	}
	for all == 0 && time.Since(t0) < 20*time.Second {
		if reconciled() {
			all = time.Since(t0)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("time to first served key %v, to all reconciled %v (%d keys, 1 worker, 40 ms per S3 exchange)", first, all, keys)
	if len(served) != keys || first == 0 || all == 0 || first >= all {
		t.Fatalf("served %d/%d, first %v, all reconciled %v: want first < all", len(served), keys, first, all)
	}
}

// CP-1 gate, Release It H4 at the agent: with S3 refusing or black-holed, cold activations answer unavailable
// with Retry-After, a warm key keeps serving reads and writes, /readyz stays 200, the S3 breaker opens and
// /metrics shows it; then requests fail fast without reaching S3, and after a heal and a cool-down the cold
// keys activate.
func TestS3OutageAtTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault faultnet.Fault
	}{{"refuse", faultnet.RefuseConn()}, {"blackhole", faultnet.Hole()}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDurableRig(t, "")
			a := r.start(t)
			const cold = 6
			for i := range cold + 1 {
				db := fmt.Sprintf("db-%d", i)
				a.claim(t, db)
				writeRows(t, a, db, 1, 0, 1)
			}
			a.stop() // written back; the restart below starts with every key cold
			b := r.start(t)
			waitReady(t, b.life)
			count(t, b, "db-0", 1) // db-0 is warm from here on

			r.proxy.Set(faultnet.Frozen(), 0) // also freezes the pooled connections
			r.proxy.Set(tc.fault, 0)
			done := make(chan answer, cold)
			for i := 1; i <= cold; i++ {
				go func() { done <- b.query(t, fmt.Sprintf("db-%d", i), 1, front.KindRead, `SELECT k FROM kv`) }()
			}
			for range cold {
				if ans := <-done; ans.Code != front.Unavailable || ans.status != 503 || ans.retryAfter == "" {
					t.Fatalf("cold key under %s: %d %+v (Retry-After %q)", tc.name, ans.status, ans.Response, ans.retryAfter)
				}
			}
			if v := b.metric(t, `tsdb_breaker_state{dep="s3",state="open"}`); v != "1" {
				t.Fatalf("breaker open gauge = %q", v)
			}
			t0 := time.Now()
			ans := b.query(t, "db-1", 1, front.KindRead, `SELECT k FROM kv`)
			if ans.Code != front.Unavailable || time.Since(t0) > 500*time.Millisecond {
				t.Fatalf("with the breaker open: %+v after %v", ans.Response, time.Since(t0))
			}
			if ra, _ := strconv.Atoi(ans.retryAfter); ra < 1 {
				t.Fatalf("Retry-After %q", ans.retryAfter)
			}
			writeRows(t, b, "db-0", 1, 1, 3) // warm writes are local
			if n := count(t, b, "db-0", 1); n != 3 {
				t.Fatalf("warm key: %d rows", n)
			}
			if code, _ := getStatus(t, "http://"+b.life+front.ReadyPath); code != http.StatusOK {
				t.Fatalf("readyz %d during the S3 outage", code)
			}
			if v := b.metric(t, "tsdb_s3_probe_ok"); v != "0" {
				t.Errorf("probe gauge %q during the outage", v)
			}

			r.proxy.Heal()
			deadline := time.Now().Add(15 * time.Second)
			for {
				ans := b.query(t, "db-1", 1, front.KindRead, `SELECT k FROM kv`)
				if ans.Code == front.OK {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("db-1 after the heal: %+v", ans.Response)
				}
				time.Sleep(200 * time.Millisecond)
			}
			if !strings.Contains(r.log(), "S3.BREAKER_OPEN") || !strings.Contains(r.log(), "S3.BREAKER_CLOSED") {
				t.Errorf("breaker transitions not logged")
			}
		})
	}
}

// CP-1 gate, H4 "applied, reply lost" on a publish: the fence's generation upload and manifest publish both
// apply and lose their replies; each is resolved by a re-read, so the fence acknowledges, not misreports.
func TestFenceWithLostPublishReplies(t *testing.T) {
	r := newDurableRig(t, "")
	a := r.start(t)
	a.claim(t, "db-1")
	writeRows(t, a, "db-1", 1, 0, 5)
	r.fake.Inject(s3test.Fault{Mode: s3test.ApplyThenDropReply, Method: http.MethodPut, Times: 2})
	code, out := a.control(t, fencePath, map[string]any{"db": "db-1"})
	if code != http.StatusOK {
		t.Fatalf("fence with lost replies: %d %v", code, out)
	}
	if m := r.manifest(t, "db-1"); m.Generation != uint64(out["gen"].(float64)) || m.Generation < 1 {
		t.Fatalf("manifest %+v, ack %v", m, out)
	}
	puts := 0
	for _, q := range r.fake.Requests() {
		if q.Fault == s3test.ApplyThenDropReply {
			puts++
		}
	}
	if puts != 2 {
		t.Fatalf("%d PUTs lost their reply, want 2", puts)
	}
}

// CP-1 gate: SIGTERM with dirty keys. With S3 healthy the shutdown writes every key back within
// shutdown_budget (exit 0) and a restart reads the writes back; with S3 black-holed it gives up at the budget,
// keeps the copies locally and says so (exit 1).
func TestSIGTERMWithDirtyKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	const budget = 3 * time.Second
	extra := `,"shutdown_budget":"3s","drain_timeout":"1s","maintenance":{"interval":"50ms","write_back_every":"1h","idle_after":"1h","write_back_timeout":"10s"}`
	for _, tc := range []struct {
		name string
		hole bool
		code int
	}{{"s3 healthy", false, exitOK}, {"s3 black-holed", true, exitError}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDurableRig(t, extra)
			a := r.startProcess(t)
			const keys = 6
			for i := range keys {
				db := fmt.Sprintf("db-%d", i)
				a.claim(t, db)
				writeRows(t, a, db, 1, 0, 3)
			}
			if tc.hole {
				r.proxy.Set(faultnet.Frozen(), 0)
				r.proxy.Set(faultnet.Hole(), 0)
			}
			t0 := time.Now()
			code := a.stop()
			took := time.Since(t0)
			if code != tc.code || took > budget+time.Second {
				t.Fatalf("exit %d (want %d) after %v (budget %v); stderr:\n%s", code, tc.code, took, budget, a.stderr.String())
			}
			log := r.log()
			if !strings.Contains(log, "VACTOR.SHUTDOWN") || !strings.Contains(log, "AGENT.SHUTDOWN_DONE") {
				t.Fatalf("log:\n%s", log)
			}
			if tc.hole {
				if !strings.Contains(log, `"kept":6`) && !strings.Contains(log, "kept_keys") {
					t.Fatalf("kept copies not reported:\n%s", log)
				}
				return
			}
			if !strings.Contains(log, `"written_back":6`) {
				t.Fatalf("not every dirty key written back:\n%s", log)
			}
			b := r.start(t)
			waitReady(t, b.life)
			for i := range keys {
				if n := count(t, b, fmt.Sprintf("db-%d", i), 1); n != 3 {
					t.Fatalf("db-%d: %d rows after the restart, want 3", i, n)
				}
			}
		})
	}
}

// CP-1 gate, the e2e: claim → write over HTTP → passivate → kill -9 → restart → reconcile → read back. Every
// acknowledged write that was written back is present. Then the VM is declared dead (M5) while down: the
// operator moves the placement row to a new epoch; the restarted agent fences the old epoch (superseded),
// a claim at the new epoch serves the same data, and the old epoch gets wrong_owner.
func TestKill9RestartReconcile(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	r := newDurableRig(t, `,"maintenance":{"interval":"50ms","idle_after":"300ms","write_back_every":"1h"}`)
	a := r.startProcess(t)
	a.claim(t, "db-1")
	writeRows(t, a, "db-1", 1, 0, 10)
	const retried = "retried-after-restart" // CP-2's HTTP gate: the same id after a restart applies once
	if ans := a.queryID(t, "db-1", 1, retried, front.KindWrite, fronttest.PutSQL, 100, "once").must(t); ans.Duplicate {
		t.Fatal("first attempt answered duplicate")
	}
	waitFor(t, 10*time.Second, "passivation", func() bool {
		_, h := a.control(t, healthPath, nil)
		return h["active"].(float64) == 0
	})
	written := r.manifest(t, "db-1").Generation
	if written == 0 {
		t.Fatal("passivation did not write back")
	}
	a.kill()

	b := r.startProcess(t)
	waitReady(t, b.life)
	if n := count(t, b, "db-1", 1); n != 11 {
		t.Fatalf("after kill -9 and reconcile: %d rows, want 11", n)
	}
	if ans := b.queryID(t, "db-1", 1, retried, front.KindWrite, fronttest.PutSQL, 100, "once").must(t); !ans.Duplicate || ans.Count != 1 {
		t.Fatalf("retry after the restart: %+v, want ok + duplicate", ans.Response)
	}
	writeRows(t, b, "db-1", 1, 10, 15) // acknowledged but never written back (idle_after has not passed)
	b.kill()

	// M5: the operator attests the VM is dead and moves the row to epoch 2 (same VM: a restore in place).
	pl, err := file.Open(filepath.Join(r.dir, "placement"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := pl.Get(context.Background(), "db-1")
	if err != nil {
		t.Fatal(err)
	}
	next := owner.Place{Owner: "vm-1", Epoch: row.Epoch + 1, Gen: owner.Generation(written), State: owner.PlaceActive}
	if err := pl.CAS(context.Background(), "db-1", row, next); err != nil {
		t.Fatal(err)
	}

	c := r.startProcess(t)
	waitReady(t, c.life)
	if ans := c.query(t, "db-1", 1, front.KindRead, `SELECT 1`); ans.Code != front.Fenced {
		t.Fatalf("old epoch after the restart: %+v (want fenced: superseded by the reconcile)", ans.Response)
	}
	if e := c.claim(t, "db-1"); e != 2 {
		t.Fatalf("claim at epoch %d, want 2", e)
	}
	n := count(t, c, "db-1", 2)
	if n < 11 {
		t.Fatalf("epoch 2 reads %d rows: written-back writes lost", n)
	}
	if ans := c.queryID(t, "db-1", 2, retried, front.KindWrite, fronttest.PutSQL, 100, "once").must(t); !ans.Duplicate {
		t.Fatalf("retry at the new epoch: %+v, want ok + duplicate", ans.Response)
	}
	t.Logf("after the kill -9: %d rows (11 written back, 5 acknowledged within the RPO window)", n)
	if ans := c.query(t, "db-1", 1, front.KindRead, `SELECT 1`); ans.Code != front.WrongOwner {
		t.Fatalf("old epoch: %+v, want wrong_owner", ans.Response)
	}
	if m := r.manifest(t, "db-1"); m.Epoch != 2 {
		t.Fatalf("storage epoch %d, want 2", m.Epoch)
	}
	if !strings.Contains(r.log(), "OWNER.SUPERSEDED") {
		t.Error("supersede not logged")
	}
}

// waitFor polls cond until it holds or d passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
