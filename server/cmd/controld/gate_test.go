package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/ticket/tickettest"
)

// The CP-4 gate (control-plane plan §3): two agentd processes on one host and controld, over Postgres and a
// real S3. Gated on MANSIO_PG_URL and COLDLOAD_S3 (deploy/postgres, deploy/seaweedfs).
//
//	MANSIO_PG_URL='postgres://mansio:mansio-dev@localhost:5433/mansio?sslmode=disable' COLDLOAD_S3=localhost:8333 \
//	  go test -count=1 -run TestGate -v ./cmd/controld

const (
	controlToken = "gate-control-token-0123456789"
	adminToken   = "gate-admin-token-0123456789"
	edgeToken    = "gate-edge-token-0123456789"
)

var iss = tickettest.NewIssuer("k1")

// cluster is the gate's world: a fresh Postgres schema, an S3 prefix, two agents and controld.
type cluster struct {
	t        *testing.T
	dir      string
	dsn      string
	schema   string
	s3       string
	prefix   string
	agentBin string
	agents   map[string]*agentProc
	api      string // controld's base URL
	pl       *pg.Placement
	secrets  string // controld's secrets file

	// The CP-5 gate: agents fetch ticket keys from controld (keys_url) at a fixed address, chosen before the
	// agents start; extra is added to controld's config.
	locator      bool
	controldAddr string
	extra        string
	stopCtl      func() // stops controld and waits for it (once)

	// The Jepsen-style run (jepsen_test.go): fixed agent listeners (so a restarted agent comes back where the
	// proxies point), the addresses controld and the agents are given for each other (faultnet proxies), and
	// schemas from controld (IR requests). Zero values: the gates' defaults.
	agentListen map[string][3]string // vm → query, health, admin listen addresses
	agentSeen   map[string][2]string // vm → the endpoint and lifecycle host:port controld records
	ctlSeen     string               // controld's address as the agents see it (keys_url, schemas_url)
	schemas     bool                 // agents fetch app schemas from controld (schemas_url; needs locator)
	agentExtra  string               // added to each agent's config (",\"key\":value...")
}

type agentProc struct {
	vm, query, life string
	admin           string // admin_listen (runtime controls: breaker trip, passivate)
	cmd             *exec.Cmd
	exited          chan error
}

func newCluster(t *testing.T) *cluster {
	dsn, s3 := os.Getenv("MANSIO_PG_URL"), os.Getenv("COLDLOAD_S3")
	if dsn == "" || s3 == "" {
		t.Skip("set MANSIO_PG_URL and COLDLOAD_S3 (deploy/postgres, deploy/seaweedfs) to run the CP-4 gate")
	}
	b := make([]byte, 5)
	rand.Read(b)
	c := &cluster{t: t, dir: t.TempDir(), dsn: dsn, s3: s3, schema: "gate_" + hex.EncodeToString(b),
		prefix: "gate-" + hex.EncodeToString(b) + "/", agents: map[string]*agentProc{}}
	admin, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(context.Background(), "create schema "+c.schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "drop schema "+c.schema+" cascade")
		admin.Close(context.Background())
		if st, err := objstore.New(objstore.Config{Endpoint: s3, Bucket: "coldload", AccessKey: "coldload", SecretKey: "coldload-secret"}); err == nil {
			st.DeletePrefix(context.Background(), c.prefix)
		}
	})
	c.agentBin = filepath.Join(c.dir, "agentd")
	if out, err := exec.Command("go", "build", "-o", c.agentBin, "../agentd").CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v\n%s", err, out)
	}
	keys, _ := json.Marshal(ticket.PublicKeyFile{Keys: []ticket.PublicKeyEntry{{KID: "k1", PublicKey: ticket.EncodeKey(iss.Pub)}}})
	os.WriteFile(filepath.Join(c.dir, "keys.json"), keys, 0o644)
	if c.pl, err = pg.Open(pg.Config{DSN: dsn, Schema: c.schema, AppName: "test/gate"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.pl.Close)
	return c
}

// startAgent runs agentd vm as a child process (durable mode over Postgres placement, -dev for plain-HTTP
// S3) and waits until it serves.
func (c *cluster) startAgent(vm string) *agentProc {
	t := c.t
	dir := filepath.Join(c.dir, vm)
	os.MkdirAll(dir, 0o755)
	sec, _ := json.Marshal(map[string]string{"s3_access_key": "agent", "s3_secret_key": "agent-secret-dev",
		"control_token": controlToken, "pg_dsn": c.dsn})
	os.WriteFile(filepath.Join(dir, "secrets.json"), sec, 0o600)
	keys := `"keys_file":"../keys.json"`
	if c.locator {
		ctl := c.controldAddr
		if c.ctlSeen != "" {
			ctl = c.ctlSeen
		}
		keys = fmt.Sprintf(`"keys_url":"http://%s/v1/keys","keys_file":"ticket-keys.json","keys_refresh":"200ms","keys_timeout":"150ms","clock_skew":"1s"`, ctl)
		if c.schemas {
			keys += fmt.Sprintf(`,"schemas_url":"http://%s"`, ctl)
		}
	}
	keys += c.agentExtra
	listen := [3]string{"127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"}
	if l, ok := c.agentListen[vm]; ok {
		listen = l
	}
	cfg := fmt.Sprintf(`{"listen":%q,"health_listen":%q,"admin_listen":%q,"vm":%q,%s,"log_file":"agentd.log",
		"s3":{"endpoint":%q,"bucket":"coldload","prefix":%q},
		"secrets_file":"secrets.json","cache_dir":"cache","owner_dir":"owner","placement_pg":{"schema":%q},
		"default_timeout":"5s","max_timeout":"10s","lifecycle_timeout":"20s",
		"maintenance":{"interval":"50ms","write_back_every":"200ms"}}`, listen[0], listen[1], listen[2], vm, keys, c.s3, c.prefix, c.schema)
	os.WriteFile(filepath.Join(dir, "agentd.json"), []byte(cfg), 0o644)
	addrFile := filepath.Join(dir, "addr")
	os.Remove(addrFile)
	cmd := exec.Command(c.agentBin, "-dev", "-config", filepath.Join(dir, "agentd.json"), "-addr-file", addrFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	a := &agentProc{vm: vm, cmd: cmd, exited: make(chan error, 1)}
	go func() { a.exited <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })
	deadline := time.Now().Add(15 * time.Second)
	for {
		if b, err := os.ReadFile(addrFile); err == nil {
			if lines := strings.Fields(string(b)); len(lines) == 3 {
				a.query, a.life, a.admin = lines[0], lines[1], lines[2]
				break
			}
		}
		select {
		case err := <-a.exited:
			t.Fatalf("agentd %s exited: %v\n%s", vm, err, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("agentd %s did not start", vm)
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.agents[vm] = a
	return a
}

// kill is kill -9.
func (a *agentProc) kill() {
	a.cmd.Process.Kill()
	<-a.exited
}

// startControld runs controld in-process over the started agents and waits until it is ready.
func (c *cluster) startControld() {
	t := c.t
	var agents []string
	for _, vm := range []string{"vm-a", "vm-b"} {
		a := c.agents[vm]
		endpoint, life := a.query, a.life
		if seen, ok := c.agentSeen[vm]; ok {
			endpoint, life = seen[0], seen[1]
		}
		agents = append(agents, fmt.Sprintf(`{"vm":%q,"endpoint":%q,"lifecycle":"http://%s"}`, vm, endpoint, life))
	}
	sec, _ := json.Marshal(Secrets{PGDSN: c.dsn, ControlToken: controlToken, AdminToken: adminToken, S3AccessKey: "gc", S3SecretKey: "gc-secret-dev",
		EdgeToken: edgeToken})
	c.secrets = filepath.Join(c.dir, "controld-secrets.json")
	os.WriteFile(c.secrets, sec, 0o600)
	listen := c.controldAddr
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	cfg := fmt.Sprintf(`{"listen":%q,"secrets_file":"controld-secrets.json","log_file":"controld.log","pg":{"schema":%q},
		"s3":{"endpoint":%q,"bucket":"coldload","prefix":%q},"agents":[%s],"ticket_keys":{"dir":"ticket-keys"},
		"heartbeat_interval":"200ms","heartbeat_timeout":"150ms","resume_interval":"500ms","resume_after":"2s",
		"agent_timeout":"20s","per_key_timeout":"30s"%s}`, listen, c.schema, c.s3, c.prefix, strings.Join(agents, ","), c.extra)
	path := filepath.Join(c.dir, "controld.json")
	os.WriteFile(path, []byte(cfg), 0o644)
	addrFile := filepath.Join(c.dir, "controld.addr")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stderr bytes.Buffer
	go func() {
		done <- realMain(ctx, []string{"-dev", "-config", path, "-addr-file", addrFile}, os.Stdout, &stderr)
	}()
	var once sync.Once
	c.stopCtl = func() {
		once.Do(func() {
			cancel()
			if code := <-done; code != exitOK {
				t.Errorf("controld exited %d: %s", code, stderr.String())
			}
		})
	}
	t.Cleanup(c.stopCtl)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(addrFile); err == nil && len(b) > 0 {
			c.api = "http://" + strings.TrimSpace(string(b))
			if res, err := http.Get(c.api + "/readyz"); err == nil {
				res.Body.Close()
				if res.StatusCode == 200 {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("controld not ready: %s", stderr.String())
}

// call makes one API call and decodes the answer into out (if not nil); it returns the status.
func (c *cluster) call(method, path string, body, out any) int {
	c.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.api+path, rd)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

// query sends one statement to db's current owner as the authority says (a client's resolve), with a
// ticket at that epoch. It returns the wire answer.
func (c *cluster) query(db string, kind front.Kind, id, sql string, args ...any) (front.Response, error) {
	p, err := c.pl.Get(context.Background(), db)
	if err != nil {
		return front.Response{}, err
	}
	a := c.agents[p.Owner]
	if a == nil {
		return front.Response{}, fmt.Errorf("owner %s not running", p.Owner)
	}
	if args == nil {
		args = []any{}
	}
	body := map[string]any{"db": db, "kind": kind, "sql": sql, "args": args}
	if id != "" {
		body["request_id"] = id
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "http://"+a.query+front.QueryPath, bytes.NewReader(b))
	cl := iss.Claims(db, p.Owner, time.Now(), time.Minute)
	cl.Epoch = uint64(p.Epoch)
	req.Header.Set("Authorization", "Bearer "+iss.Mint(c.t, cl))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return front.Response{}, err
	}
	defer res.Body.Close()
	var out front.Response
	err = json.NewDecoder(res.Body).Decode(&out)
	return out, err
}

// mustWrite retries one write with the same request id, re-resolving after every non-ok answer, until ok
// (Release It R2: the id makes the retry safe across a move).
func (c *cluster) mustWrite(db, id, sql string, args ...any) front.Response {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := c.query(db, front.KindWrite, id, sql, args...)
		if err == nil && r.Code == front.OK {
			return r
		}
		if err == nil && r.Code == front.BadRequest {
			c.t.Fatalf("write %s to %s: %+v", id, db, r)
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("write %s to %s never succeeded: %+v %v", id, db, r, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *cluster) count(db, sql string) int64 {
	c.t.Helper()
	r, err := c.query(db, front.KindRead, "", sql)
	if err != nil || r.Code != front.OK || len(r.Rows) != 1 {
		c.t.Fatalf("read %s: %+v %v", db, r, err)
	}
	var n int64
	switch v := r.Rows[0][0].(type) {
	case float64:
		n = int64(v)
	case json.Number:
		n, _ = v.Int64()
	default:
		c.t.Fatalf("count is %T %v", v, v)
	}
	return n
}

func TestGate(t *testing.T) {
	c := newCluster(t)
	c.startAgent("vm-a")
	c.startAgent("vm-b")
	c.startControld()

	// 1. Create 100 databases across both agents (8 at a time).
	t0 := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	var failed atomic.Int64
	for i := range 100 {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			var d map[string]any
			if code := c.call("POST", "/v1/databases", map[string]string{"tenant": "gate", "name": fmt.Sprintf("db%03d", i)}, &d); code != 200 || d["state"] != "active" {
				t.Errorf("create db%03d: %d %v", i, code, d)
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	if failed.Load() > 0 {
		t.FailNow()
	}
	per := map[string]int{}
	owners := map[string]string{}
	for i := range 100 {
		key := fmt.Sprintf("gate:db%03d", i)
		p, err := c.pl.Get(context.Background(), key)
		if err != nil || p.Epoch != 1 || p.State != owner.PlaceActive {
			t.Fatalf("%s: %+v %v", key, p, err)
		}
		per[p.Owner]++
		owners[key] = p.Owner
	}
	t.Logf("created 100 databases in %v: %v", time.Since(t0), per)
	if per["vm-a"] < 35 || per["vm-b"] < 35 {
		t.Fatalf("uneven spread %v", per)
	}
	for _, key := range []string{"gate:db000", "gate:db099"} {
		c.mustWrite(key, "init-"+key, "create table t (id integer not null)")
	}

	// 2. Move one database under write load: no acknowledged write lost, none applied twice.
	var moving string
	for i := range 100 {
		if k := fmt.Sprintf("gate:db%03d", i); owners[k] == "vm-a" {
			moving = k
			break
		}
	}
	c.mustWrite(moving, "init-"+moving, "create table if not exists t (id integer not null)")
	stop := make(chan struct{})
	var acked atomic.Int64
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c.mustWrite(moving, "w-"+strconv.Itoa(i), "insert into t (id) values (?)", i)
			acked.Add(1)
		}
	}()
	time.Sleep(300 * time.Millisecond)
	before := acked.Load()
	var moved map[string]any
	t1 := time.Now()
	if code := c.call("POST", "/v1/databases/"+moving+"/move", map[string]string{"to": "vm-b"}, &moved); code != 200 || moved["state"] != "active" {
		t.Fatalf("move: %d %v", code, moved)
	}
	moveTook := time.Since(t1)
	time.Sleep(300 * time.Millisecond)
	close(stop)
	<-writerDone
	p, _ := c.pl.Get(context.Background(), moving)
	if p.Owner != "vm-b" || p.Epoch != 2 {
		t.Fatalf("after move: %+v", p)
	}
	n := acked.Load()
	if rows := c.count(moving, "select count(*) from t"); rows != n {
		t.Fatalf("after the move: %d rows, %d acknowledged writes", rows, n)
	}
	if dups := c.count(moving, "select count(*) from (select id from t group by id having count(*) > 1)"); dups != 0 {
		t.Fatalf("%d ids applied twice", dups)
	}
	if distinct := c.count(moving, "select count(distinct id) from t"); distinct != n {
		t.Fatalf("distinct ids %d, acknowledged %d", distinct, n)
	}
	t.Logf("move under load: %v; %d writes acknowledged (%d before the move started), all present once", moveTook, n, before)

	// 3. Kill vm-b; restore its databases on vm-a after an operator attests it dead (M5).
	var onB []string
	for i := range 100 {
		k := fmt.Sprintf("gate:db%03d", i)
		if p, _ := c.pl.Get(context.Background(), k); p.Owner == "vm-b" {
			onB = append(onB, k)
		}
	}
	for _, k := range onB[:5] {
		c.mustWrite(k, "pre-kill-table-"+k, "create table if not exists t (id integer not null)")
		c.mustWrite(k, "pre-kill-"+k, "insert into t (id) values (?)", -7)
	}
	time.Sleep(time.Second) // write_back_every 200 ms: the rows are published
	c.agents["vm-b"].kill()
	var out bytes.Buffer
	if code := realMain(context.Background(), []string{"restore", "-addr", c.api, "-secrets", c.secrets, "-vm", "vm-b"}, &out, os.Stderr); code != exitError ||
		!strings.Contains(out.String(), "conflict") {
		t.Fatalf("restore before attestation: exit %d %s", code, out.String())
	}
	out.Reset()
	if code := realMain(context.Background(), []string{"attest-dead", "-addr", c.api, "-secrets", c.secrets, "-vm", "vm-b", "-by", "gate-operator"}, &out, os.Stderr); code != exitOK {
		t.Fatalf("attest-dead: exit %d %s", code, out.String())
	}
	out.Reset()
	t2 := time.Now()
	if code := realMain(context.Background(), []string{"restore", "-addr", c.api, "-secrets", c.secrets, "-vm", "vm-b"}, &out, os.Stderr); code != exitOK {
		t.Fatalf("restore: exit %d %s", code, out.String())
	}
	var rep struct {
		Moved  map[string]string `json:"moved"`
		Failed map[string]string `json:"failed"`
	}
	json.Unmarshal(out.Bytes(), &rep)
	if len(rep.Moved) != len(onB) || len(rep.Failed) != 0 {
		t.Fatalf("restore moved %d of %d, failed %v", len(rep.Moved), len(onB), rep.Failed)
	}
	t.Logf("restored %d databases from vm-b in %v", len(onB), time.Since(t2))
	for _, k := range onB {
		p, err := c.pl.Get(context.Background(), k)
		if err != nil || p.Owner != "vm-a" || p.State != owner.PlaceActive {
			t.Fatalf("%s after restore: %+v %v", k, p, err)
		}
	}
	for _, k := range onB[:5] {
		if n := c.count(k, "select count(*) from t where id = -7"); n != 1 {
			t.Fatalf("%s: the written-back row is gone (%d)", k, n)
		}
	}
	if n := c.count(moving, "select count(*) from t where id >= 0"); n != acked.Load() {
		t.Fatalf("the moved database after restore: %d rows, want %d", n, acked.Load())
	}
	var d map[string]any
	if code := c.call("POST", "/v1/databases", map[string]string{"tenant": "gate", "name": "after"}, &d); code != 200 {
		t.Fatalf("create after restore: %d %v", code, d)
	}
	if p, _ := c.pl.Get(context.Background(), "gate:after"); p.Owner != "vm-a" {
		t.Fatalf("placed on the dead vm: %+v", p)
	}
	// Drop one: tombstone, objects gone, name never reused.
	if code := c.call("POST", "/v1/databases/gate:db000/drop", nil, &d); code != 200 || d["state"] != "dropped" {
		t.Fatalf("drop: %d %v", code, d)
	}
	if code := c.call("POST", "/v1/databases", map[string]string{"tenant": "gate", "name": "db000"}, &d); code != 409 {
		t.Fatalf("re-create a dropped name: %d %v", code, d)
	}
}
