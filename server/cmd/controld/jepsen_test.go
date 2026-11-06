package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/faultnet"
	"github.com/nickstrad/mansio/server/internal/linearize"
	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// The Jepsen-style system run (IR protocol plan §6.5; IR-5): real agentd (two, durable over Postgres placement
// and S3), controld (in-process, as in the gates) and the edge, with faultnet proxies on every link between
// them (client → edge, client/agents/edge → controld, edge → each agent's query listener, controld → each
// agent's lifecycle listener). A nemesis injects, one at a time: proxy faults (latency, refused and silent
// connections, frozen links), kill -9 of an agent (restarted on the same files and ports), a move of a
// database, and `docker pause` of Postgres and SeaweedFS. Clients send IR requests (/v1/ops) through the edge
// with tickets from controld's resolve, retrying with the same request id, and record a history:
//
//   - registers: a database of documents written (patch) and read (get); each document's history must be
//     linearizable (internal/linearize);
//   - bank: a database of accounts; a transfer reads two accounts and patches both with version
//     preconditions; reads of all accounts must always sum to the total (conservation) and the history must
//     be linearizable.
//
// An operation whose outcome is unknown (no answer, or an ambiguous one, by its deadline) may have taken
// effect at any time after its call. Gated (it pauses the shared dev containers):
//
//	MANSIO_JEPSEN=1 MANSIO_PG_URL='postgres://mansio:mansio-dev@localhost:5433/mansio?sslmode=disable' \
//	  COLDLOAD_S3=localhost:8333 go test -count=1 -run TestJepsen -v -timeout 20m ./cmd/controld
//
// MANSIO_JEPSEN_DURATION (default 2m) is the workload's length; MANSIO_JEPSEN_SEED fixes the nemesis and
// client choices; MANSIO_JEPSEN_DOCKER=0 skips the docker pauses; MANSIO_JEPSEN_PG_CONTAINER and
// MANSIO_JEPSEN_S3_CONTAINER name them (default mansio-postgres, coldload-seaweedfs).
func TestJepsen(t *testing.T) {
	if os.Getenv("MANSIO_JEPSEN") == "" {
		t.Skip("set MANSIO_JEPSEN=1 (with MANSIO_PG_URL and COLDLOAD_S3) for the Jepsen-style run")
	}
	duration := 2 * time.Minute
	if v := os.Getenv("MANSIO_JEPSEN_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
		duration = d
	}
	seed := uint64(time.Now().UnixNano())
	if v := os.Getenv("MANSIO_JEPSEN_SEED"); v != "" {
		seed, _ = strconv.ParseUint(v, 10, 64)
	}
	t.Logf("seed %d, duration %v (replay: MANSIO_JEPSEN_SEED=%d)", seed, duration, seed)
	w := newJepsenWorld(t, seed)
	w.setup()

	ctx, stop := context.WithTimeout(context.Background(), duration)
	defer stop()
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() { w.registerClient(ctx, i) })
		wg.Go(func() { w.bankClient(ctx, i) })
	}
	wg.Go(func() { w.nemesis(ctx) })
	wg.Wait()
	w.heal()
	t.Logf("nemesis: %s", strings.Join(w.faults, ", "))

	// Settle, then read everything once more (part of the histories).
	settle := time.Now().Add(30 * time.Second)
	for _, ok := w.readAll(context.Background(), 0); !ok && time.Now().Before(settle); _, ok = w.readAll(context.Background(), 0) {
		time.Sleep(200 * time.Millisecond)
	}
	for _, k := range w.regKeys {
		for !w.readRegister(context.Background(), 0, k) && time.Now().Before(settle) {
			time.Sleep(200 * time.Millisecond)
		}
	}
	w.check()
}

// --- The world -----------------------------------------------------------------------------------------------

const jepsenSchema = `{"registers":{"fields":{"value":{"node":{"kind":"integer"}}},"storage":"columns","indexes":[]},` +
	`"accounts":{"fields":{"balance":{"node":{"kind":"integer"}}},"storage":"columns","indexes":[]}}`

const (
	jTenant   = "jepsen"
	jRegApp   = "jreg"
	jBankApp  = "jbank"
	nAccounts = 5
	balance0  = 100
)

type jepsenWorld struct {
	t       *testing.T
	c       *cluster
	rng     *rand.Rand
	rngMu   sync.Mutex
	fp      string
	apiKey  string
	edgeURL string // through its proxy
	ctlURL  string // through its proxy
	proxies map[string]*faultnet.Proxy
	edgeCmd *exec.Cmd
	http    *http.Client
	routes  sync.Map // app → *jRoute
	start   time.Time
	ids     atomic.Int64

	regKeys  []string
	accounts []string

	mu      sync.Mutex
	regOps  []linearize.Op[regIn, regOut]
	bankOps []linearize.Op[bankIn, bankOut]
	sums    []int64 // every full read's total
	// crashes are the kill -9 times. A restarted agent serves each database from its last published
	// generation: acknowledged writes inside the write-back window (the RPO) may be lost (by design:
	// knowledge/writeback, DST-1's RPO rule), so each database's history is checked per crash-free segment.
	crashes []int64
	faults  []string
	stats   map[string]int
}

type jRoute struct {
	ticket string
	epoch  uint64
	until  time.Time
}

func newJepsenWorld(t *testing.T, seed uint64) *jepsenWorld {
	c := newCluster(t)
	w := &jepsenWorld{t: t, c: c, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), proxies: map[string]*faultnet.Proxy{},
		start: time.Now(), stats: map[string]int{},
		http: &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext}}}
	proxy := func(name, upstream string) *faultnet.Proxy {
		p, err := faultnet.New(upstream)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		w.proxies[name] = p
		return p
	}
	c.locator, c.schemas, c.controldAddr = true, true, freeAddr(t)
	c.ctlSeen = proxy("controld", c.controldAddr).Addr()
	c.agentListen, c.agentSeen = map[string][3]string{}, map[string][2]string{}
	for _, vm := range []string{"vm-a", "vm-b"} {
		l := [3]string{freeAddr(t), freeAddr(t), freeAddr(t)}
		c.agentListen[vm] = l
		c.agentSeen[vm] = [2]string{proxy(vm+"/query", l[0]).Addr(), proxy(vm+"/lifecycle", l[1]).Addr()}
	}
	c.agentExtra = `,"clock_skew":"2s"`
	c.extra = `,"locator":{"ticket_ttl":"60s"}`
	c.startAgent("vm-a")
	c.startAgent("vm-b")
	c.startControld()
	waitListening(t, c)
	w.ctlURL = "http://" + c.ctlSeen
	w.startEdge()
	return w
}

// startEdge builds and runs the edge (routes from controld's registry, keys from controld, both through the
// controld proxy) behind its own proxy.
func (w *jepsenWorld) startEdge() {
	t, c := w.t, w.c
	bin := filepath.Join(c.dir, "edge")
	if out, err := exec.Command("go", "build", "-o", bin, "../edge").CombinedOutput(); err != nil {
		t.Fatalf("build edge: %v\n%s", err, out)
	}
	dir := filepath.Join(c.dir, "edge-run")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "edge-token"), []byte(edgeToken), 0o600)
	cfg := fmt.Sprintf(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","keys_url":"http://%s/v1/keys","keys_file":"keys.json",
		"keys_refresh":"500ms","keys_timeout":"400ms","clock_skew":"2s",
		"registry":{"url":"http://%s/v1/agents","token_file":"edge-token","refresh":"300ms","timeout":"250ms","agent_scheme":"http"},
		"rate_limit":{"verifies_per_second":1000,"burst":1000},"default_timeout":"3s","max_timeout":"10s","log_file":"edge.log"}`,
		c.ctlSeen, c.ctlSeen)
	os.WriteFile(filepath.Join(dir, "edge.json"), []byte(cfg), 0o644)
	addrFile := filepath.Join(dir, "addr")
	cmd := exec.Command(bin, "-dev", "-config", filepath.Join(dir, "edge.json"), "-addr-file", addrFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	w.edgeCmd = cmd
	deadline := time.Now().Add(15 * time.Second)
	for {
		if b, err := os.ReadFile(addrFile); err == nil && strings.Count(string(b), "\n") >= 2 {
			p, err := faultnet.New(strings.Fields(string(b))[0])
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { p.Close() })
			w.proxies["edge"] = p
			w.edgeURL = "http://" + p.Addr()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("edge did not start: %s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// setup registers the schema, issues an API key, creates both databases and their documents.
func (w *jepsenWorld) setup() {
	t, c := w.t, w.c
	w.fp = schemareg.Fingerprint([]byte(jepsenSchema))
	for _, app := range []string{jRegApp, jBankApp} {
		if code := c.call("PUT", "/v1/apps/"+app+"/schema", map[string]string{"fingerprint": w.fp, "schema": jepsenSchema}, nil); code != 200 {
			t.Fatalf("register %s: %d", app, code)
		}
		if code := c.call("POST", "/v1/apps/"+app+"/databases", map[string]string{"tenant": jTenant}, nil); code != 200 {
			t.Fatalf("create %s: %d", app, code)
		}
	}
	var key map[string]string
	if code := c.call("POST", "/v1/tenants/"+jTenant+"/api-keys", map[string]string{"principal": "jepsen"}, &key); code != 200 {
		t.Fatalf("api key: %d", code)
	}
	w.apiKey = key["key"]
	id := func(n int) string { return fmt.Sprintf("01900000-0000-7000-8000-%012d", n) }
	var regOps, bankOps []map[string]any
	for i := range 3 {
		w.regKeys = append(w.regKeys, id(100+i))
		regOps = append(regOps, map[string]any{"op": "insert", "table": "registers", "id": id(100 + i), "value": map[string]any{"value": 0}})
	}
	for i := range nAccounts {
		w.accounts = append(w.accounts, id(200+i))
		bankOps = append(bankOps, map[string]any{"op": "insert", "table": "accounts", "id": id(200 + i), "value": map[string]any{"balance": balance0}})
	}
	for app, ops := range map[string][]map[string]any{jRegApp: regOps, jBankApp: bankOps} {
		rid := "setup-" + app
		deadline := time.Now().Add(30 * time.Second)
		for {
			a := w.send(context.Background(), app, "mutation", rid, ops)
			if a.ok() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("setup %s: %+v", app, a)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// Initial states are part of the histories (the models start at them).
	w.start = time.Now()
}

// now is the history's clock: monotonic nanoseconds since setup.
func (w *jepsenWorld) now() int64 { return int64(time.Since(w.start)) }

func (w *jepsenWorld) intn(n int) int {
	w.rngMu.Lock()
	defer w.rngMu.Unlock()
	return w.rng.IntN(n)
}

// --- The IR client ---------------------------------------------------------------------------------------------

// jAnswer is one request's final answer as the client learned it.
type jAnswer struct {
	result  []json.RawMessage
	tag     string // "" on success
	unknown bool   // a mutation whose outcome is not known (no answer, or ambiguous, with no later definite one)
}

func (a jAnswer) ok() bool { return a.tag == "" && !a.unknown && a.result != nil }

// route returns app's cached ticket, resolving it (through the controld proxy) when missing or near expiry.
func (w *jepsenWorld) route(ctx context.Context, app string, minEpoch uint64) (*jRoute, error) {
	if v, ok := w.routes.Load(app); ok && minEpoch == 0 {
		if r := v.(*jRoute); time.Now().Before(r.until) {
			return r, nil
		}
	}
	b, _ := json.Marshal(map[string]any{"tenant": jTenant, "app": app, "min_epoch": minEpoch})
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodPost, w.ctlURL+"/v1/resolve", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+w.apiKey)
	res, err := w.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out route
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != 200 {
		return nil, fmt.Errorf("resolve %s: %d %s %v", app, res.StatusCode, out.Code, err)
	}
	r := &jRoute{ticket: out.Ticket, epoch: out.Epoch, until: time.Now().Add(time.Duration(out.TTLMs)*time.Millisecond - 10*time.Second)}
	w.routes.Store(app, r)
	return r, nil
}

// send runs one request until a definite answer or ctx (each attempt ≤ 2 s), with the same request id on every
// attempt (a commit that landed answers duplicate). The rules are the SDK's (sdk/retry.ts decide): reroute on
// wrong_owner, fenced, ticket_invalid; retry shed, unavailable, aborted, expired and no answer; anything else
// is definite.
func (w *jepsenWorld) send(ctx context.Context, app, kind, rid string, ops []map[string]any) jAnswer {
	body := map[string]any{"v": 1, "db": map[string]string{"tenant": jTenant, "app": app}, "kind": kind, "schema": w.fp, "ops": ops}
	if kind == "mutation" {
		body["requestId"] = rid
	}
	b, _ := json.Marshal(body)
	maybe := false // an earlier attempt of this mutation may have applied
	var minEpoch uint64
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return jAnswer{tag: "deadline", unknown: maybe}
		}
		if attempt > 0 {
			time.Sleep(time.Duration(10+w.intn(40*min(attempt, 8))) * time.Millisecond)
		}
		r, err := w.route(ctx, app, minEpoch)
		if err != nil {
			w.count("resolve-failed")
			continue
		}
		minEpoch = 0
		actx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, _ := http.NewRequestWithContext(actx, http.MethodPost, w.edgeURL+"/v1/ops", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+r.ticket)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Timeout-Ms", "1800")
		res, err := w.http.Do(req)
		if err != nil {
			cancel()
			w.count("no-answer")
			maybe = maybe || kind == "mutation"
			continue
		}
		var out struct {
			Result []json.RawMessage `json:"result"`
			Error  *struct {
				Tag    string         `json:"tag"`
				Fields map[string]any `json:"fields"`
			} `json:"error"`
		}
		err = json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		cancel()
		switch {
		case err != nil:
			w.count("unreadable")
			maybe = maybe || kind == "mutation"
			continue
		case out.Error == nil && res.StatusCode == 200:
			return jAnswer{result: out.Result}
		case out.Error == nil:
			w.count("untagged")
			maybe = maybe || kind == "mutation"
			continue
		}
		tag := out.Error.Tag
		w.count(tag)
		switch tag {
		case "wrong_owner", "fenced":
			w.routes.Delete(app)
			minEpoch = r.epoch + 1
		case "ticket_invalid":
			w.routes.Delete(app)
		case "expired":
			if kind == "mutation" {
				maybe = true // the deadline may have landed after the commit
			}
		case "shed", "unavailable", "aborted":
		default:
			return jAnswer{tag: tag} // definite: the duplicate check ran first, so this attempt says it never applied
		}
	}
}

func (w *jepsenWorld) count(what string) {
	w.mu.Lock()
	w.stats[what]++
	w.mu.Unlock()
}

// --- Registers ---------------------------------------------------------------------------------------------------

type regIn struct {
	key   string
	write bool
	value int64
}

type regOut struct {
	value int64
}

// pace spaces a client's operations (a few hundred per second in all: histories stay checkable).
func (w *jepsenWorld) pace() { time.Sleep(time.Duration(2+w.intn(8)) * time.Millisecond) }

func (w *jepsenWorld) registerClient(ctx context.Context, client int) {
	for ; ctx.Err() == nil; w.pace() {
		k := w.regKeys[w.intn(len(w.regKeys))]
		if w.intn(2) == 0 {
			w.readRegister(ctx, client, k)
			continue
		}
		v := w.ids.Add(1)
		rid := fmt.Sprintf("reg-%d-%d", client, v)
		call := w.now()
		octx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		a := w.send(octx, jRegApp, "mutation", rid, []map[string]any{{"op": "patch", "table": "registers", "id": k, "value": map[string]any{"value": v}}})
		cancel()
		op := linearize.Op[regIn, regOut]{Call: call, Return: w.now(), Input: regIn{key: k, write: true, value: v}}
		switch {
		case a.ok():
		case a.unknown:
			op.Return = linearize.Unknown
		default:
			continue // definitely not applied: not part of the history
		}
		w.mu.Lock()
		w.regOps = append(w.regOps, op)
		w.mu.Unlock()
	}
}

// readRegister reads k once (a failed read is left out of the history); it reports whether it answered.
func (w *jepsenWorld) readRegister(ctx context.Context, client int, k string) bool {
	call := w.now()
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	a := w.send(octx, jRegApp, "query", "", []map[string]any{{"op": "get", "table": "registers", "id": k}})
	if !a.ok() || len(a.result) != 1 {
		return false
	}
	var doc struct{ Value int64 }
	if err := json.Unmarshal(a.result[0], &doc); err != nil {
		w.t.Errorf("register read: %s: %v", a.result[0], err)
		return false
	}
	w.mu.Lock()
	w.regOps = append(w.regOps, linearize.Op[regIn, regOut]{Call: call, Return: w.now(), Input: regIn{key: k}, Output: regOut{doc.Value}})
	w.mu.Unlock()
	return true
}

// regState is a register's value; known is false at the start of a segment after a crash (any value).
type regState struct {
	known bool
	v     int64
}

// registerModel starts from init (the setup's 0, or unknown after a crash).
func registerModel(init regState) linearize.Model[regState, regIn, regOut] {
	return linearize.Model[regState, regIn, regOut]{
		Init: func() regState { return init },
		Step: func(s regState, in regIn, out regOut, unknown bool) (bool, regState) {
			if in.write {
				return true, regState{true, in.value}
			}
			return !s.known || out.value == s.v, regState{true, out.value}
		},
		Key: func(s regState) string { return fmt.Sprint(s.known, s.v) },
	}
}

// --- Bank ----------------------------------------------------------------------------------------------------------

// bankIn is a transfer (from, to, amount, at the versions read) or a read of some accounts.
type bankIn struct {
	transfer       bool
	from, to       int
	amount         int64
	vFrom, vTo     int64 // the versions the transfer requires
	newFrom, newTo int64 // the balances it writes
	accounts       []int // a read's accounts
}

// bankOut is a read's balances and versions (by account in bankIn.accounts order).
type bankOut struct {
	balances, versions []int64
}

// bankState is every account's balance and version; known[i] is false for an account not seen yet in a
// segment after a crash.
type bankState struct {
	known              [nAccounts]bool
	balances, versions [nAccounts]int64
}

// bankModel starts from the setup's accounts (known) or from nothing known (after a crash).
func bankModel(known bool) linearize.Model[bankState, bankIn, bankOut] {
	return linearize.Model[bankState, bankIn, bankOut]{
		Init: func() bankState {
			var s bankState
			for i := range nAccounts {
				s.known[i], s.balances[i], s.versions[i] = known, balance0, 1
			}
			return s
		},
		Step: func(s bankState, in bankIn, out bankOut, unknown bool) (bool, bankState) {
			if in.transfer {
				for _, a := range [2]struct {
					i      int
					v, bal int64
				}{{in.from, in.vFrom, in.newFrom}, {in.to, in.vTo, in.newTo}} {
					if s.known[a.i] && s.versions[a.i] != a.v {
						return false, s // it could not have applied here
					}
					s.known[a.i], s.balances[a.i], s.versions[a.i] = true, a.bal, a.v+1
				}
				return true, s
			}
			for i, a := range in.accounts {
				if s.known[a] && (s.balances[a] != out.balances[i] || s.versions[a] != out.versions[i]) {
					return false, s
				}
				s.known[a], s.balances[a], s.versions[a] = true, out.balances[i], out.versions[i]
			}
			return true, s
		},
		Key: func(s bankState) string { return fmt.Sprint(s.known, s.balances, s.versions) },
	}
}

// readAccounts reads accounts in one query; it records the read and answers what it saw.
func (w *jepsenWorld) readAccounts(ctx context.Context, accounts []int) (bankOut, bool) {
	ops := make([]map[string]any, len(accounts))
	for i, a := range accounts {
		ops[i] = map[string]any{"op": "get", "table": "accounts", "id": w.accounts[a]}
	}
	call := w.now()
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	a := w.send(octx, jBankApp, "query", "", ops)
	if !a.ok() || len(a.result) != len(accounts) {
		return bankOut{}, false
	}
	var out bankOut
	for _, raw := range a.result {
		var doc struct {
			Balance int64
			Version int64 `json:"_version"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			w.t.Errorf("account read: %s: %v", raw, err)
			return bankOut{}, false
		}
		out.balances, out.versions = append(out.balances, doc.Balance), append(out.versions, doc.Version)
	}
	w.mu.Lock()
	w.bankOps = append(w.bankOps, linearize.Op[bankIn, bankOut]{Call: call, Return: w.now(), Input: bankIn{accounts: slices.Clone(accounts)}, Output: out})
	w.mu.Unlock()
	return out, true
}

// readAll reads every account and records the total (conservation).
func (w *jepsenWorld) readAll(ctx context.Context, _ int) (bankOut, bool) {
	all := make([]int, nAccounts)
	for i := range all {
		all[i] = i
	}
	out, ok := w.readAccounts(ctx, all)
	if ok {
		var sum int64
		for _, b := range out.balances {
			sum += b
		}
		w.mu.Lock()
		w.sums = append(w.sums, sum)
		w.mu.Unlock()
	}
	return out, ok
}

func (w *jepsenWorld) bankClient(ctx context.Context, client int) {
	for n := 0; ctx.Err() == nil; n++ {
		w.pace()
		if w.intn(4) == 0 {
			w.readAll(ctx, client)
			continue
		}
		from := w.intn(nAccounts)
		to := (from + 1 + w.intn(nAccounts-1)) % nAccounts
		seen, ok := w.readAccounts(ctx, []int{from, to})
		if !ok {
			continue
		}
		amount := int64(1 + w.intn(10))
		in := bankIn{transfer: true, from: from, to: to, amount: amount, vFrom: seen.versions[0], vTo: seen.versions[1],
			newFrom: seen.balances[0] - amount, newTo: seen.balances[1] + amount}
		rid := fmt.Sprintf("bank-%d-%d", client, n)
		ops := []map[string]any{
			{"op": "patch", "table": "accounts", "id": w.accounts[from], "value": map[string]any{"balance": in.newFrom},
				"expect": map[string]any{"version": in.vFrom}},
			{"op": "patch", "table": "accounts", "id": w.accounts[to], "value": map[string]any{"balance": in.newTo},
				"expect": map[string]any{"version": in.vTo}},
		}
		call := w.now()
		octx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		a := w.send(octx, jBankApp, "mutation", rid, ops)
		cancel()
		op := linearize.Op[bankIn, bankOut]{Call: call, Return: w.now(), Input: in}
		switch {
		case a.ok():
			w.count("transfer-ok")
		case a.unknown:
			op.Return = linearize.Unknown
			w.count("transfer-unknown")
		case a.tag == "ConflictError":
			w.count("transfer-conflict")
			continue // a precondition failed: nothing applied
		default:
			w.count("transfer-failed")
			continue
		}
		w.mu.Lock()
		w.bankOps = append(w.bankOps, op)
		w.mu.Unlock()
	}
}

// --- Nemesis -----------------------------------------------------------------------------------------------------

func (w *jepsenWorld) nemesis(ctx context.Context) {
	docker := os.Getenv("MANSIO_JEPSEN_DOCKER") != "0"
	if docker {
		if err := exec.Command("docker", "version").Run(); err != nil {
			w.t.Logf("docker unavailable (%v): no pauses", err)
			docker = false
		}
	}
	container := func(env, def string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return def
	}
	pgC, s3C := container("MANSIO_JEPSEN_PG_CONTAINER", "mansio-postgres"), container("MANSIO_JEPSEN_S3_CONTAINER", "coldload-seaweedfs")
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range w.proxies {
			if !yield(n) {
				return
			}
		}
	})
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	for sleep(time.Duration(2000+w.intn(2000)) * time.Millisecond) {
		hold := time.Duration(1000+w.intn(3000)) * time.Millisecond
		switch k := w.intn(10); {
		case k < 5: // a proxy fault
			name := names[w.intn(len(names))]
			faults := []struct {
				what string
				f    faultnet.Fault
			}{{"latency", faultnet.Delayed(150 * time.Millisecond)}, {"refuse", faultnet.RefuseConn()},
				{"silence", faultnet.Silence()}, {"frozen", faultnet.Frozen()}, {"reset", faultnet.ResetAfterBytes(64)}}
			f := faults[w.intn(len(faults))]
			w.note("%s:%s", name, f.what)
			w.proxies[name].Set(f.f, 0)
			sleep(hold)
			w.proxies[name].Heal()
		case k < 7: // kill -9 an agent; restart it on the same files and ports
			vm := []string{"vm-a", "vm-b"}[w.intn(2)]
			w.note("kill-9:%s", vm)
			w.mu.Lock()
			w.crashes = append(w.crashes, w.now())
			w.mu.Unlock()
			w.c.agents[vm].kill()
			time.Sleep(hold)
			w.c.startAgent(vm)
		case k < 8: // move a database to the other agent
			app := []string{jRegApp, jBankApp}[w.intn(2)]
			var d map[string]any
			code := w.c.call("GET", "/v1/databases/"+jTenant+":"+app, nil, &d)
			w.note("move:%s(%d)", app, code)
			mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			w.move(mctx, jTenant+":"+app)
			cancel()
		case docker && k < 9:
			w.note("pause:%s", pgC)
			w.pause(pgC, hold)
		case docker:
			w.note("pause:%s", s3C)
			w.pause(s3C, hold)
		}
	}
}

// move moves key to the agent that does not own it now (controld's admin API, not through a proxy).
func (w *jepsenWorld) move(ctx context.Context, key string) {
	p, err := w.c.pl.Get(ctx, key)
	if err != nil {
		w.note("move:%s:unreadable", key)
		return
	}
	to := "vm-a"
	if p.Owner == "vm-a" {
		to = "vm-b"
	}
	var out map[string]any
	if code := w.c.call("POST", "/v1/databases/"+key+"/move", map[string]string{"to": to}, &out); code != 200 {
		w.note("move:%s:%d:%v", key, code, out["code"])
	}
}

func (w *jepsenWorld) pause(container string, d time.Duration) {
	if out, err := exec.Command("docker", "pause", container).CombinedOutput(); err != nil {
		w.note("pause-failed:%s:%s", container, strings.TrimSpace(string(out)))
		return
	}
	time.Sleep(d)
	if out, err := exec.Command("docker", "unpause", container).CombinedOutput(); err != nil {
		w.t.Errorf("docker unpause %s: %v %s (unpause it by hand)", container, err, out)
	}
}

func (w *jepsenWorld) note(format string, args ...any) {
	w.mu.Lock()
	w.faults = append(w.faults, fmt.Sprintf("%.1fs ", time.Since(w.start).Seconds())+fmt.Sprintf(format, args...))
	w.mu.Unlock()
}

// heal ends every fault: proxies healed, both agents running.
func (w *jepsenWorld) heal() {
	for _, p := range w.proxies {
		p.Heal()
	}
	for _, vm := range []string{"vm-a", "vm-b"} {
		a := w.c.agents[vm]
		select {
		case <-a.exited:
			w.c.startAgent(vm)
		default:
		}
	}
}

// --- Checks ------------------------------------------------------------------------------------------------------

func (w *jepsenWorld) check() {
	t := w.t
	w.mu.Lock()
	defer w.mu.Unlock()
	t.Logf("answers: %v", w.stats)
	known := func(n int, unknown int) string { return fmt.Sprintf("%d ops (%d unknown)", n, unknown) }
	unknownOf := func(rets []int64) (n int) {
		for _, r := range rets {
			if r == linearize.Unknown {
				n++
			}
		}
		return n
	}
	var regRets []int64
	for _, o := range w.regOps {
		regRets = append(regRets, o.Return)
	}
	t.Logf("registers: %s; %d crash-free segments; acknowledged writes lost to crashes (RPO): %v", known(len(w.regOps), unknownOf(regRets)),
		len(w.crashes)+1, w.lostToCrashes())
	for k, all := range linearize.Partition(w.regOps, func(in regIn) string { return in.key }) {
		for seg, ops := range segments(all, w.crashes, func(in regIn) bool { return in.write }) {
			res := linearize.Check(registerModel(regState{known: seg == 0}), ops)
			if res.OK {
				continue
			}
			t.Errorf("register %s, segment %d: not linearizable (gave up: %v; longest prefix %d of %d ops)\n%s", k, seg, res.GaveUp, len(res.Longest), len(ops),
				around(ops, res.Longest, func(o linearize.Op[regIn, regOut]) string {
					if o.Input.write {
						return fmt.Sprintf("write %d", o.Input.value)
					}
					return fmt.Sprintf("read → %d", o.Output.value)
				}))
		}
	}
	var bankRets []int64
	writes := 0
	for _, o := range w.bankOps {
		bankRets = append(bankRets, o.Return)
		if o.Input.transfer {
			writes++
		}
	}
	t.Logf("bank: %s, %d transfers, %d full reads", known(len(w.bankOps), unknownOf(bankRets)), writes, len(w.sums))
	for i, s := range w.sums {
		if s != nAccounts*balance0 {
			t.Errorf("conservation: full read %d sums to %d, want %d", i, s, nAccounts*balance0)
		}
	}
	if len(w.sums) == 0 {
		t.Error("no full bank read answered")
	}
	for seg, ops := range segments(w.bankOps, w.crashes, func(in bankIn) bool { return in.transfer }) {
		res := linearize.Check(bankModel(seg == 0), ops)
		if res.OK {
			continue
		}
		t.Errorf("bank, segment %d: not linearizable (gave up: %v; longest prefix %d of %d ops)\n%s", seg, res.GaveUp, len(res.Longest), len(ops),
			around(ops, res.Longest, func(o linearize.Op[bankIn, bankOut]) string {
				if in := o.Input; in.transfer {
					return fmt.Sprintf("transfer %d→%d: a%d=%d@v%d a%d=%d@v%d", in.from, in.to, in.from, in.newFrom, in.vFrom, in.to, in.newTo, in.vTo)
				}
				return fmt.Sprintf("read %v → %v @ %v", o.Input.accounts, o.Output.balances, o.Output.versions)
			}))
	}
	if writes == 0 || len(w.regOps) == 0 {
		t.Error("the workload made no progress")
	}
}

// around describes a failing history for the report: the first operation that returned and could not be
// linearized after the longest linearizable prefix, and the operations overlapping its neighbourhood in time.
func around[I, O any](ops []linearize.Op[I, O], longest []int, show func(linearize.Op[I, O]) string) string {
	in := make([]bool, len(ops))
	for _, i := range longest {
		in[i] = true
	}
	stuck := -1
	for i, o := range ops {
		if !in[i] && o.Return != linearize.Unknown && (stuck < 0 || o.Return < ops[stuck].Return) {
			stuck = i
		}
	}
	if stuck < 0 {
		return "(no stuck operation)"
	}
	lo, hi := ops[stuck].Call-int64(300*time.Millisecond), ops[stuck].Return+int64(50*time.Millisecond)
	var idx []int
	for i, o := range ops {
		if o.Call <= hi && (o.Return >= lo) {
			idx = append(idx, i)
		}
	}
	slices.SortFunc(idx, func(a, b int) int { return int(ops[a].Call - ops[b].Call) })
	if len(idx) > 40 {
		idx = idx[len(idx)-40:]
	}
	var b strings.Builder
	sec := func(t int64) string {
		if t == linearize.Unknown {
			return "?"
		}
		return fmt.Sprintf("%.4f", float64(t)/1e9)
	}
	for _, i := range idx {
		mark := " "
		switch {
		case i == stuck:
			mark = "!"
		case in[i]:
			mark = "+"
		}
		fmt.Fprintf(&b, "  %s [%s, %s] %s\n", mark, sec(ops[i].Call), sec(ops[i].Return), show(ops[i]))
	}
	return b.String()
}

// segments splits a history at the crash times. An operation called and answered between the same two
// crashes belongs to that segment. One in flight across a crash, or never answered: a read is left out; a
// write goes, with an unknown outcome, to every segment its interval reaches (it may have applied before the
// crash and been lost with the write-back window, or been seen before it, or applied after the restart, or
// not at all).
func segments[I, O any](ops []linearize.Op[I, O], crashes []int64, write func(I) bool) [][]linearize.Op[I, O] {
	out := make([][]linearize.Op[I, O], len(crashes)+1)
	segOf := func(t int64) int { // the segment time t is in: crashes before it
		n := 0
		for n < len(crashes) && t > crashes[n] {
			n++
		}
		return n
	}
	for _, o := range ops {
		sc, sr := segOf(o.Call), segOf(o.Return)
		if sc == sr && o.Return != linearize.Unknown {
			out[sc] = append(out[sc], o)
			continue
		}
		if !write(o.Input) {
			continue
		}
		o.Return = linearize.Unknown
		for seg := sc; seg <= sr; seg++ {
			out[seg] = append(out[seg], o)
		}
	}
	return out
}

// lostToCrashes estimates, per crash, the acknowledged register writes the restart rolled back: those answered
// ok before the crash after the write whose value the first read after it saw (a measure of the RPO in
// practice; not a check).
//
//	pre: w.mu held
func (w *jepsenWorld) lostToCrashes() []int {
	lost := make([]int, len(w.crashes))
	for i, k := range w.crashes {
		for _, key := range w.regKeys {
			var acked []linearize.Op[regIn, regOut]
			seen, found := int64(0), false
			first := linearize.Unknown
			for _, o := range w.regOps {
				switch {
				case o.Input.key != key:
				case o.Input.write && o.Return != linearize.Unknown && o.Return < k:
					acked = append(acked, o)
				case !o.Input.write && o.Call > k && o.Call < first:
					first, seen, found = o.Call, o.Output.value, true
				}
			}
			if !found {
				continue
			}
			slices.SortFunc(acked, func(a, b linearize.Op[regIn, regOut]) int { return int(a.Return - b.Return) })
			for j := len(acked) - 1; j >= 0 && acked[j].Input.value != seen; j-- {
				if acked[j].Input.value > seen { // written after the value that survived (values only rise)
					lost[i]++
				}
			}
		}
	}
	return lost
}
