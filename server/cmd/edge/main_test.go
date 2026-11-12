package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/edge/edgetest"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/ticket/tickettest"
)

// keys writes a one-key public key file and returns its path and a minting function.
func keys(t *testing.T, dir string) (string, func(vm string) string) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	b, _ := json.Marshal(ticket.PublicKeyFile{Keys: []ticket.PublicKeyEntry{{KID: "k1", PublicKey: ticket.EncodeKey(pub)}}})
	path := filepath.Join(dir, "keys.json")
	os.WriteFile(path, b, 0o644)
	return path, func(vm string) string {
		now := time.Now().Unix()
		tok, err := ticket.Mint(priv, ticket.Claims{KID: "k1", TID: "t", DBID: "db-1", VM: vm, Epoch: 1, Sub: "u",
			Perms: ticket.Read, IAT: now - 1, EXP: now + 60})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
}

// running is an edge started by realMain in this process.
type running struct {
	addr, health string
	stop         func() int
	stderr       *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func start(t *testing.T, dir, cfg string, args ...string) *running {
	t.Helper()
	path := filepath.Join(dir, "edge.json")
	os.WriteFile(path, []byte(cfg), 0o644)
	addrFile := filepath.Join(dir, "addr")
	os.Remove(addrFile)
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{stderr: &syncBuf{}}
	code := make(chan int, 1)
	go func() {
		code <- realMain(ctx, append([]string{"-config", path, "-addr-file", addrFile}, args...), r.stderr)
	}()
	r.stop = func() int { cancel(); return <-code }
	for range 500 {
		if b, err := os.ReadFile(addrFile); err == nil && strings.Count(string(b), "\n") == 2 {
			lines := strings.Fields(string(b))
			r.addr, r.health = lines[0], lines[1]
			t.Cleanup(func() { cancel() })
			return r
		}
		select {
		case c := <-code:
			t.Fatalf("edge exited %d:\n%s", c, r.stderr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("edge did not start:\n%s", r.stderr)
	return nil
}

// tlsAgent is an agent stand-in on TLS with a certificate from ca, answering with what it saw.
func tlsAgent(t *testing.T, ca *edgetest.CA) *httptest.Server {
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3")
		json.NewEncoder(w).Encode(map[string]any{"code": "ok", "proto": r.Proto, "tls": r.TLS != nil,
			"timeout": r.Header.Get(front.TimeoutHeader)})
	}))
	s.EnableHTTP2 = true
	s.TLS = &tls.Config{Certificates: []tls.Certificate{ca.TLS(t, "127.0.0.1")}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func query(t *testing.T, c *http.Client, url, tok string) (int, map[string]any, http.Header, error) {
	req, _ := http.NewRequest(http.MethodPost, url+front.QueryPath, strings.NewReader(`{"db":"db-1","kind":"read","sql":"SELECT 1"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(front.TimeoutHeader, "1500")
	res, err := c.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer res.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("answer %q", b)
	}
	return res.StatusCode, out, res.Header, nil
}

// TLS end to end with a private CA: client → edge (the edge's certificate) → agent (the agent's certificate,
// checked against agent_tls.ca_file), HTTP/2 on both hops; no -dev. A client that does not trust the CA cannot
// connect, and an edge that does not trust the agent's CA answers unavailable (nothing ran).
func TestTLSEndToEnd(t *testing.T) {
	dir := t.TempDir()
	ca := edgetest.NewCA(t)
	caFile, certFile, keyFile := ca.WriteFiles(t, dir, "edge", "127.0.0.1", "localhost")
	keysFile, mint := keys(t, dir)
	agent := tlsAgent(t, ca)
	strangerCA := edgetest.NewCA(t)
	stranger := tlsAgent(t, strangerCA)
	cfg := fmt.Sprintf(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","tls":{"cert_file":%q,"key_file":%q},
		"keys_file":%q,"agents":{"vm-a":%q,"vm-x":%q},"agent_tls":{"ca_file":%q},"log_file":"edge.log"}`,
		certFile, keyFile, keysFile, agent.URL, stranger.URL, caFile)
	e := start(t, dir, cfg)

	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}, ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	status, out, h, err := query(t, client, "https://"+e.addr, mint("vm-a"))
	if err != nil || status != 200 || out["tls"] != true || out["proto"] != "HTTP/2.0" || h.Get("Retry-After") != "3" {
		t.Fatalf("through the edge: %d %v %v %v", status, out, h, err)
	}
	if ms, _ := strconv.Atoi(out["timeout"].(string)); ms <= 0 || ms > 1500 {
		t.Fatalf("X-Timeout-Ms at the agent: %v", out["timeout"])
	}
	res, _ := client.Get("https://" + e.addr + front.QueryPath)
	if res.ProtoMajor != 2 {
		t.Fatalf("client → edge is %s, want HTTP/2", res.Proto)
	}
	res.Body.Close()

	status, out, _, _ = query(t, client, "https://"+e.addr, mint("vm-x"))
	if status != 503 || out["code"] != "unavailable" {
		t.Fatalf("agent with an untrusted certificate: %d %v", status, out)
	}
	untrusting := &http.Client{Timeout: 5 * time.Second}
	if _, _, _, err := query(t, untrusting, "https://"+e.addr, mint("vm-a")); err == nil {
		t.Fatal("a client without the private CA connected")
	}
	if res, err := client.Post("http://"+e.addr+front.QueryPath, "application/json", nil); err == nil {
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("plain HTTP to a TLS edge: %d", res.StatusCode)
		}
	}
	if c := e.stop(); c != exitOK {
		t.Fatalf("stop: exit %d\n%s", c, e.stderr)
	}
}

// The lifecycle listener: live, ready until draining, metrics; SIGTERM drains and exits 0.
func TestLifecycleAndShutdown(t *testing.T) {
	dir := t.TempDir()
	keysFile, mint := keys(t, dir)
	release := make(chan struct{})
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.WriteString(w, `{"code":"ok"}`)
	}))
	defer agent.Close()
	e := start(t, dir, fmt.Sprintf(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","keys_file":%q,"agents":{"vm-a":%q},
		"drain_timeout":"3s","log_file":"edge.log"}`, keysFile, agent.URL), "-dev")
	get := func(path string) (int, string) {
		res, err := http.Get("http://" + e.health + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if c, _ := get(front.LivePath); c != 200 {
		t.Fatalf("livez %d", c)
	}
	if c, _ := get(front.ReadyPath); c != 200 {
		t.Fatalf("readyz %d", c)
	}
	// A request in flight at shutdown finishes; the process exits 0.
	done := make(chan int)
	go func() {
		status, _, _, err := query(t, http.DefaultClient, "http://"+e.addr, mint("vm-a"))
		if err != nil {
			t.Error(err)
		}
		done <- status
	}()
	for {
		if _, body := get("/metrics"); strings.Contains(body, `mansio_edge_agent_in_flight{vm="vm-a"} 1`) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	code := make(chan int)
	go func() { code <- e.stop() }()
	for {
		if c, _ := get(front.ReadyPath); c == 503 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if s := <-done; s != 200 {
		t.Fatalf("in-flight request at shutdown: %d", s)
	}
	if c := <-code; c != exitOK {
		t.Fatalf("exit %d\n%s", c, e.stderr)
	}
}

func TestExitCodes(t *testing.T) {
	dir := t.TempDir()
	keysFile, _ := keys(t, dir)
	write := func(s string) string {
		p := filepath.Join(dir, fmt.Sprintf("c%d.json", time.Now().UnixNano()))
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	plain := fmt.Sprintf(`{"keys_file":%q,"agents":{"vm-a":"http://127.0.0.1:7070"}}`, keysFile)
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no config", nil, exitUsage, "usage"},
		{"bad config", []string{"-config", write(`{"listen":"","agents":{"vm-a":"ftp://x"},"log_level":"loud"}`)}, exitError, "scheme must be https"},
		{"plain http without -dev", []string{"-config", write(plain), "-check"}, exitError, "plain HTTP to clients"},
		{"plain http with -dev", []string{"-config", write(plain), "-check", "-dev"}, exitOK, "config ok: 1 agents (vm-a)"},
		{"missing keys", []string{"-config", write(`{"keys_file":"nope.json","agents":{"vm-a":"http://127.0.0.1:1"}}`), "-dev"}, exitError, "keys_file"},
	} {
		var b strings.Builder
		if c := realMain(context.Background(), tc.args, &b); c != tc.code || !strings.Contains(b.String(), tc.want) {
			t.Errorf("%s: exit %d, %q; want %d, %q", tc.name, c, b.String(), tc.code, tc.want)
		}
	}
}

// With keys_url the edge follows the issuer's key set (internal/keyfetch, as agentd): after a rotation to a new
// key, tickets signed by it pass the edge, and the old key's are refused once it is withdrawn.
func TestKeysURLRotation(t *testing.T) {
	dir := t.TempDir()
	k1, k2 := tickettest.NewIssuer("k1"), tickettest.NewIssuer("k2")
	var mu sync.Mutex
	published := []tickettest.Issuer{k1}
	keySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		f := ticket.PublicKeyFile{}
		for _, k := range published {
			f.Keys = append(f.Keys, ticket.PublicKeyEntry{KID: k.KID, PublicKey: ticket.EncodeKey(k.Pub)})
		}
		json.NewEncoder(w).Encode(f)
	}))
	defer keySrv.Close()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"code":"ok"}`) }))
	defer agent.Close()
	e := start(t, dir, fmt.Sprintf(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","keys_url":%q,"keys_file":"keys-cache.json",
		"keys_refresh":"50ms","keys_timeout":"40ms","agents":{"vm-a":%q},"log_file":"edge.log"}`, keySrv.URL+"/v1/keys", agent.URL), "-dev")
	tok := func(is tickettest.Issuer) string {
		return is.Mint(t, is.Claims("db-1", "vm-a", time.Now(), time.Minute))
	}
	status := func(tk string) int { s, _, _, _ := query(t, http.DefaultClient, "http://"+e.addr, tk); return s }
	if s := status(tok(k1)); s != 200 {
		t.Fatalf("k1 before rotation: %d", s)
	}
	mu.Lock()
	published = []tickettest.Issuer{k2}
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for status(tok(k2)) != 200 {
		if time.Now().After(deadline) {
			t.Fatalf("k2 never accepted:\n%s", e.stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s := status(tok(k1)); s != 401 {
		t.Fatalf("withdrawn k1: %d", s)
	}
	if c := e.stop(); c != exitOK {
		t.Fatalf("exit %d\n%s", c, e.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "keys-cache.json")); err != nil {
		t.Fatalf("key cache not written: %v", err)
	}
}

// Routes from controld's VM registry over TLS (control_ca_file, the edge token): a VM registered after start is
// learned at the first ticket that names it (wrong_owner, then an early fetch), and a failing registry keeps the
// last routes.
func TestRegistryRoutes(t *testing.T) {
	dir := t.TempDir()
	ca := edgetest.NewCA(t)
	caFile, certFile, keyFile := ca.WriteFiles(t, dir, "edge", "127.0.0.1")
	keysFile, mint := keys(t, dir)
	a, b := tlsAgent(t, ca), tlsAgent(t, ca)
	const token = "edge-token-0123456789"
	var mu sync.Mutex
	agents, failing, gets := map[string]string{"vm-a": strings.TrimPrefix(a.URL, "https://")}, false, 0
	reg := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gets++
		switch {
		case r.URL.Path != "/v1/agents" || r.Header.Get("Authorization") != "Bearer "+token:
			w.WriteHeader(http.StatusUnauthorized)
		case failing:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": "ok", "agents": agents})
		}
	}))
	reg.TLS = &tls.Config{Certificates: []tls.Certificate{ca.TLS(t, "127.0.0.1")}}
	reg.StartTLS()
	defer reg.Close()
	tokenFile := filepath.Join(dir, "edge-token")
	os.WriteFile(tokenFile, []byte(token+"\n"), 0o600)
	e := start(t, dir, fmt.Sprintf(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","tls":{"cert_file":%q,"key_file":%q},
		"keys_file":%q,"control_ca_file":%q,"agent_tls":{"ca_file":%q},"log_file":"edge.log",
		"registry":{"url":%q,"token_file":"edge-token","refresh":"1h","timeout":"2s"}}`, certFile, keyFile, keysFile, caFile, caFile, reg.URL+"/v1/agents"))

	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}, ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	status := func(vm string) (int, any) {
		s, out, _, err := query(t, client, "https://"+e.addr, mint(vm))
		if err != nil {
			t.Fatal(err)
		}
		return s, out["code"]
	}
	if s, code := status("vm-a"); s != 200 {
		t.Fatalf("vm-a from the registry: %d %v\n%s", s, code, e.stderr)
	}
	if s, code := status("vm-b"); s != 421 || code != "wrong_owner" {
		t.Fatalf("vm-b before it is registered: %d %v", s, code)
	}
	mu.Lock()
	agents["vm-b"] = strings.TrimPrefix(b.URL, "https://")
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second) // MinGap 1 s after the last fetch; refresh is an hour
	for {
		s, _ := status("vm-b")
		if s == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("vm-b never learned:\n%s", e.stderr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	failing = true
	mu.Unlock()
	status("vm-c") // asks for an early fetch, which fails
	time.Sleep(1500 * time.Millisecond)
	if s, _ := status("vm-a"); s != 200 {
		t.Fatalf("vm-a while the registry fails: %d", s)
	}
	mu.Lock()
	n := gets
	mu.Unlock()
	if n < 3 || n > 4 {
		t.Fatalf("%d registry fetches, want 3 or 4: start, vm-b's miss, vm-c's miss (kicks coalesce)", n)
	}
	if c := e.stop(); c != exitOK {
		t.Fatalf("exit %d\n%s", c, e.stderr)
	}
}

// With drain_delay, shutdown fails /readyz first and keeps serving for the delay, so a load balancer
// polling readiness stops routing before the listener closes. Before, readiness flipped and the listener
// closed in consecutive statements: a request sent right after the flip was refused.
func TestDrainDelayServesWhileNotReady(t *testing.T) {
	dir := t.TempDir()
	keysFile, mint := keys(t, dir)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"code":"ok"}`) }))
	defer agent.Close()
	const delay = 600 * time.Millisecond
	e := start(t, dir, fmt.Sprintf(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","keys_file":%q,"agents":{"vm-a":%q},
		"drain_delay":"600ms","drain_timeout":"3s","log_file":"edge.log"}`, keysFile, agent.URL), "-dev")
	ready := func() int {
		res, err := http.Get("http://" + e.health + front.ReadyPath)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if ready() != 200 {
		t.Fatal("not ready before shutdown")
	}
	t0 := time.Now()
	code := make(chan int)
	go func() { code <- e.stop() }()
	for ready() != 503 {
		time.Sleep(2 * time.Millisecond)
	}
	// Not ready, still serving: new connections included (a fresh client has no pooled connection).
	for i := range 3 {
		status, out, _, err := query(t, &http.Client{Transport: &http.Transport{}}, "http://"+e.addr, mint("vm-a"))
		if err != nil || status != 200 {
			t.Fatalf("request %d during drain_delay (%v after shutdown began): %d %v %v", i, time.Since(t0), status, out, err)
		}
	}
	if c := <-code; c != exitOK {
		t.Fatalf("exit %d\n%s", c, e.stderr)
	}
	if took := time.Since(t0); took < delay || took > delay+5*time.Second {
		t.Fatalf("shutdown took %v, want drain_delay (%v) and little more", took, delay)
	}
	if _, _, _, err := query(t, &http.Client{Transport: &http.Transport{}}, "http://"+e.addr, mint("vm-a")); err == nil {
		t.Fatal("the listener still accepts after shutdown")
	}
}

// The per-agent bulkhead has a runtime lever on the loopback admin listener. Lowered to 1 with one
// request in flight, the next request to that agent is shed; the capacity gauge shows the new limit.
func TestAdminAgentLimit(t *testing.T) {
	dir := t.TempDir()
	keysFile, mint := keys(t, dir)
	release := make(chan struct{})
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.WriteString(w, `{"code":"ok"}`)
	}))
	defer agent.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0") // a free port for the admin listener
	if err != nil {
		t.Fatal(err)
	}
	adminAddr := l.Addr().String()
	l.Close()
	e := start(t, dir, fmt.Sprintf(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","admin_listen":%q,"keys_file":%q,
		"agents":{"vm-a":%q},"log_file":"edge.log"}`, adminAddr, keysFile, agent.URL), "-dev")
	post := func(path string) (int, map[string]any) {
		res, err := http.Post("http://"+adminAddr+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	for _, bad := range []string{"/admin/agent:limit?n=0", "/admin/agent:limit", "/admin/agent:limit?n=x"} {
		if code, out := post(bad); code != 400 || out["code"] != "bad_request" {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
	if code, out := post("/admin/nothing"); code != 404 {
		t.Errorf("unknown admin path: %d %v", code, out)
	}
	if code, out := post("/admin/agent:limit?n=1"); code != 200 || out["agent_in_flight"] != 1.0 || out["previous"] != 4096.0 {
		t.Fatalf("agent:limit n=1 (on point): %d %v", code, out)
	}
	first := make(chan int)
	go func() {
		status, _, _, _ := query(t, http.DefaultClient, "http://"+e.addr, mint("vm-a"))
		first <- status
	}()
	metric := func() string {
		res, err := http.Get("http://" + e.health + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	for !strings.Contains(metric(), `mansio_edge_agent_in_flight{vm="vm-a"} 1`) {
		time.Sleep(5 * time.Millisecond)
	}
	if status, out, _, err := query(t, http.DefaultClient, "http://"+e.addr, mint("vm-a")); err != nil || status != 503 || out["code"] != "shed" {
		t.Fatalf("a second request with the bulkhead lowered to 1: %d %v %v", status, out, err)
	}
	if m := metric(); !strings.Contains(m, `mansio_queue_capacity{queue="edge_agent"} 1`) || !strings.Contains(m, `mansio_queue_depth{queue="edge_agent"} 1`) {
		t.Fatalf("the capacity and depth gauges do not show the lowered bulkhead:\n%s", m)
	}
	close(release)
	if s := <-first; s != 200 {
		t.Fatalf("the request in flight: %d", s)
	}
	if c := e.stop(); c != exitOK {
		t.Fatalf("exit %d\n%s", c, e.stderr)
	}
}
