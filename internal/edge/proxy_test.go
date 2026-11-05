package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/metrics"
	"github.com/nickstrad/ts-db-go/internal/ratelimit"
)

// fakeAgent counts calls and answers with its handler (default: 200 ok naming itself).
type fakeAgent struct {
	*httptest.Server
	name  string
	calls atomic.Int64
	last  atomic.Pointer[http.Request]
	body  atomic.Pointer[[]byte]
}

func newFakeAgent(t *testing.T, name string, h http.HandlerFunc) *fakeAgent {
	a := &fakeAgent{name: name}
	if h == nil {
		h = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"code": "ok", "agent": name})
		}
	}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		a.last.Store(r.Clone(context.Background()))
		a.body.Store(&b)
		h(w, r)
	}))
	t.Cleanup(a.Close)
	return a
}

// dialCounter is a transport that counts dials, so tests can prove an agent was not contacted.
func transport(dials *atomic.Int64) *http.Transport {
	d := &net.Dialer{Timeout: time.Second}
	return &http.Transport{MaxIdleConnsPerHost: 64, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dials != nil {
			dials.Add(1)
		}
		return d.DialContext(ctx, network, addr)
	}}
}

type fixture struct {
	e     *Edge
	is    *issuer
	dials atomic.Int64
}

func newFixture(t *testing.T, routes StaticRoutes, mod func(*Config)) *fixture {
	f := &fixture{is: newIssuer(t)}
	tr := transport(&f.dials)
	t.Cleanup(tr.CloseIdleConnections)
	cfg := Config{Resolver: routes, Gate: NewGate(f.is.ring, time.Second, 10*time.Minute, 1000),
		Limiter: ratelimit.NewSourceLimiter(100, 100, 1000), Transport: tr, DefaultTimeout: 2 * time.Second, MaxTimeout: 5 * time.Second,
		Grace: 200 * time.Millisecond, BodyReadTimeout: time.Second, MaxBody: 1 << 10}
	if mod != nil {
		mod(&cfg)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.e = e
	return f
}

// result is one answer as a client sees it.
type result struct {
	status int
	h      http.Header
	resp   map[string]any
}

func (r result) code() string { s, _ := r.resp["code"].(string); return s }

// do sends one query from src with token (empty: no Authorization header).
func (f *fixture) do(t testing.TB, src, token string, hdr map[string]string) result {
	req := httptest.NewRequest(http.MethodPost, front.QueryPath, strings.NewReader(`{"db":"db-1","kind":"read","sql":"SELECT 1"}`))
	req.RemoteAddr = src + ":5555"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.e.ServeHTTP(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("answer is not JSON: %q", w.Body.String())
	}
	return result{w.Code, w.Result().Header, out}
}

// Each ticket goes to the agent its vm claim names, with the client's ticket, body and a shrunk deadline.
func TestRoutesOnVMClaim(t *testing.T) {
	a, b := newFakeAgent(t, "a", nil), newFakeAgent(t, "b", nil)
	f := newFixture(t, StaticRoutes{"vm-a": a.URL, "vm-b": b.URL + "/"}, nil)
	now := time.Now()
	ta, tb := f.is.mint(t, "vm-a", "db-1", now, time.Minute), f.is.mint(t, "vm-b", "db-1", now, time.Minute)
	for range 3 {
		if r := f.do(t, "192.0.2.1", ta, map[string]string{front.TimeoutHeader: "800"}); r.status != 200 || r.resp["agent"] != "a" {
			t.Fatalf("vm-a ticket: %d %v", r.status, r.resp)
		}
		if r := f.do(t, "192.0.2.1", tb, nil); r.status != 200 || r.resp["agent"] != "b" {
			t.Fatalf("vm-b ticket: %d %v", r.status, r.resp)
		}
	}
	if a.calls.Load() != 3 || b.calls.Load() != 3 {
		t.Fatalf("calls a=%d b=%d, want 3 each", a.calls.Load(), b.calls.Load())
	}
	r := a.last.Load()
	ms, _ := strconv.Atoi(r.Header.Get(front.TimeoutHeader))
	if r.URL.Path != front.QueryPath || r.Header.Get("Authorization") != "Bearer "+ta || ms <= 0 || ms > 800 ||
		r.Header.Get("X-Forwarded-For") != "192.0.2.1" {
		t.Fatalf("agent saw %s %v", r.URL.Path, r.Header)
	}
	if got := string(*a.body.Load()); got != `{"db":"db-1","kind":"read","sql":"SELECT 1"}` {
		t.Fatalf("body %q", got)
	}
	if ms, _ := strconv.Atoi(b.last.Load().Header.Get(front.TimeoutHeader)); ms <= 0 || ms > 2000 { // default_timeout
		t.Fatalf("default deadline forwarded as %d ms", ms)
	}
	if f.e.st.hits.Load() != 4 || f.e.st.misses.Load() != 2 {
		t.Fatalf("hits %d misses %d: want one verify per ticket", f.e.st.hits.Load(), f.e.st.misses.Load())
	}
}

// Missing, malformed, forged, expired and over-long tickets are refused at the edge: no agent is dialed.
func TestBadTicketsNeverReachAgents(t *testing.T) {
	a := newFakeAgent(t, "a", nil)
	f := newFixture(t, StaticRoutes{"vm-a": a.URL}, nil)
	now := time.Now()
	forged := newIssuer(t).mint(t, "vm-a", "db-1", now, time.Minute)
	for name, tok := range map[string]string{
		"none":      "",
		"garbage":   "garbage",
		"forged":    forged,
		"expired":   f.is.mint(t, "vm-a", "db-1", now.Add(-time.Hour), time.Minute),
		"too long":  f.is.mint(t, "vm-a", "db-1", now, time.Hour),
		"truncated": forged[:len(forged)-4],
	} {
		r := f.do(t, "192.0.2.1", tok, nil)
		if r.status != 401 || r.code() != string(front.TicketInvalid) || r.h.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d %v", name, r.status, r.resp)
		}
	}
	// An expired ticket that was cached is refused from the cache.
	short := f.is.mint(t, "vm-a", "db-1", now, 2*time.Second)
	f.do(t, "192.0.2.1", short, nil)
	f.e.cfg.Now = func() time.Time { return now.Add(time.Minute) }
	if r := f.do(t, "192.0.2.1", short, nil); r.status != 401 {
		t.Fatalf("expired cached ticket: %d %v", r.status, r.resp)
	}
	if a.calls.Load() != 1 || f.dials.Load() != 1 {
		t.Fatalf("agent calls %d, dials %d; want only the one valid request", a.calls.Load(), f.dials.Load())
	}
}

// A flood of garbage tickets from one source runs out of tokens after the burst and is shed before verifying,
// while valid traffic, new tickets from other sources and cached tickets from the flooding source, all proceed.
func TestGarbageFloodIsRateLimited(t *testing.T) {
	a := newFakeAgent(t, "a", nil)
	f := newFixture(t, StaticRoutes{"vm-a": a.URL}, func(c *Config) { c.Limiter = ratelimit.NewSourceLimiter(1, 20, 1000) })
	now := time.Now()
	cached := f.is.mint(t, "vm-a", "db-1", now, time.Minute)
	if r := f.do(t, "198.51.100.1", cached, nil); r.status != 200 { // the flooder's one real ticket, verified first
		t.Fatalf("%d %v", r.status, r.resp)
	}
	forged := newIssuer(t).mint(t, "vm-a", "db-1", now, time.Minute)
	var wg sync.WaitGroup
	var shed, invalid, valid, validOK, cachedOK atomic.Int64
	for w := range 8 {
		wg.Go(func() {
			for i := range 250 {
				switch r := f.do(t, "198.51.100.1", forged+strings.Repeat("A", w%2), nil); r.code() {
				case string(front.Shed):
					if r.status != 503 || r.h.Get("Retry-After") == "" {
						t.Errorf("shed without 503 + Retry-After: %d %v", r.status, r.h)
					}
					shed.Add(1)
				case string(front.TicketInvalid):
					invalid.Add(1)
				default:
					t.Errorf("flood %d: %v", i, r.resp)
				}
			}
		})
	}
	for w := range 4 {
		wg.Go(func() {
			src := "203.0.113." + strconv.Itoa(w+1)
			for i := range 50 {
				valid.Add(1)
				tok := f.is.mint(t, "vm-a", "db-"+strconv.Itoa(i%5), now, time.Minute) // 5 tickets per source
				if r := f.do(t, src, tok, nil); r.status == 200 {
					validOK.Add(1)
				}
				if r := f.do(t, "198.51.100.1", cached, nil); r.status == 200 {
					cachedOK.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if invalid.Load() > 21 || shed.Load() < 2000-21 { // burst 20 + at most 1 refilled during the run
		t.Fatalf("flood: %d verified (invalid), %d shed; want <= 21 verified", invalid.Load(), shed.Load())
	}
	if validOK.Load() != valid.Load() || cachedOK.Load() != 200 {
		t.Fatalf("valid traffic: %d/%d new-source ok, %d/200 cached ok", validOK.Load(), valid.Load(), cachedOK.Load())
	}
}

// With every verify slot taken, a miss is shed at once (no queue), while a cached ticket still proceeds.
func TestVerifySlotsBound(t *testing.T) {
	a := newFakeAgent(t, "a", nil)
	f := newFixture(t, StaticRoutes{"vm-a": a.URL}, func(c *Config) { c.VerifySlots = 1 })
	now := time.Now()
	cached := f.is.mint(t, "vm-a", "db-1", now, time.Minute)
	f.do(t, "192.0.2.1", cached, nil)
	f.e.verify <- struct{}{} // a verify in progress
	if r := f.do(t, "192.0.2.2", f.is.mint(t, "vm-a", "db-2", now, time.Minute), nil); r.status != 503 || r.code() != "shed" {
		t.Fatalf("miss with no slot: %d %v", r.status, r.resp)
	}
	if r := f.do(t, "192.0.2.2", cached, nil); r.status != 200 {
		t.Fatalf("hit with no slot: %d %v", r.status, r.resp)
	}
	<-f.e.verify
	if f.e.st.verifyBusy.Load() != 1 {
		t.Fatalf("verify_busy %d", f.e.st.verifyBusy.Load())
	}
}

// An agent that is down: each call fails fast as unavailable (nothing ran); after MinCalls failures the
// agent's breaker opens and calls fail without dialing, with Retry-After from the breaker's cool-down. Another
// agent is unaffected.
func TestAgentDownBreakerUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close() // connection refused from now on
	b := newFakeAgent(t, "b", nil)
	f := newFixture(t, StaticRoutes{"vm-a": dead, "vm-b": b.URL}, func(c *Config) {
		c.Breaker = breaker.Config{MinCalls: 3, CoolDown: time.Minute}
	})
	now := time.Now()
	ta, tb := f.is.mint(t, "vm-a", "db-1", now, time.Minute), f.is.mint(t, "vm-b", "db-1", now, time.Minute)
	for i := range 3 {
		t1 := time.Now()
		r := f.do(t, "192.0.2.1", ta, nil)
		if r.status != 503 || r.code() != "unavailable" || r.h.Get("Retry-After") != "1" || r.resp["ambiguous"] != nil {
			t.Fatalf("call %d to a dead agent: %d %v %v", i, r.status, r.resp, r.h)
		}
		if took := time.Since(t1); took > 500*time.Millisecond {
			t.Fatalf("connection refused took %v", took)
		}
	}
	dials := f.dials.Load()
	r := f.do(t, "192.0.2.1", ta, nil)
	if ra, _ := strconv.Atoi(r.h.Get("Retry-After")); r.code() != "unavailable" || ra < 59 || f.dials.Load() != dials {
		t.Fatalf("breaker open: %v Retry-After %q, dials %d → %d", r.resp, r.h.Get("Retry-After"), dials, f.dials.Load())
	}
	if br, _ := f.e.Breaker("vm-a"); br.State(time.Now()) != breaker.Open {
		t.Fatal("vm-a's breaker is not open")
	}
	if r := f.do(t, "192.0.2.1", tb, nil); r.status != 200 {
		t.Fatalf("vm-b while vm-a is down: %d %v", r.status, r.resp)
	}
}

// An agent that accepts and never answers is cut off at the deadline plus grace: expired, ambiguous (a write
// may have run), and it counts against the breaker.
func TestStuckAgentExpiresAmbiguous(t *testing.T) {
	stuck := newFakeAgent(t, "stuck", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	f := newFixture(t, StaticRoutes{"vm-a": stuck.URL}, func(c *Config) { c.Breaker = breaker.Config{MinCalls: 1, CoolDown: time.Minute} })
	t1 := time.Now()
	r := f.do(t, "192.0.2.1", f.is.mint(t, "vm-a", "db-1", time.Now(), time.Minute), map[string]string{front.TimeoutHeader: "100"})
	took := time.Since(t1)
	if r.status != 504 || r.code() != "expired" || r.resp["ambiguous"] != true || took < 300*time.Millisecond || took > 2*time.Second {
		t.Fatalf("stuck agent: %d %v after %v", r.status, r.resp, took)
	}
	if br, _ := f.e.Breaker("vm-a"); br.State(time.Now()) != breaker.Open {
		t.Fatal("a stuck agent did not count as a failure")
	}
}

// Every agent answer is relayed as is (status, body, Retry-After); an agent's own unavailable or internal answer
// is an answer, so it never trips the agent's breaker at the edge.
func TestAgentAnswersPassThrough(t *testing.T) {
	a := newFakeAgent(t, "a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("X-Agent-Secret", "leak")
		w.WriteHeader(503)
		io.WriteString(w, `{"code":"unavailable","request_id":"r-1","retry_after_ms":7000}`+"\n")
	})
	f := newFixture(t, StaticRoutes{"vm-a": a.URL}, func(c *Config) { c.Breaker = breaker.Config{MinCalls: 2, CoolDown: time.Minute} })
	tok := f.is.mint(t, "vm-a", "db-1", time.Now(), time.Minute)
	for range 5 {
		r := f.do(t, "192.0.2.1", tok, nil)
		if r.status != 503 || r.h.Get("Retry-After") != "7" || r.resp["request_id"] != "r-1" || r.h.Get("X-Agent-Secret") != "" {
			t.Fatalf("relayed: %d %v %v", r.status, r.resp, r.h)
		}
	}
	if br, _ := f.e.Breaker("vm-a"); br.State(time.Now()) != breaker.Closed || a.calls.Load() != 5 {
		t.Fatalf("breaker %v, calls %d", br.State(time.Now()), a.calls.Load())
	}
}

func TestEdgeRefusals(t *testing.T) {
	release := make(chan struct{})
	slow := newFakeAgent(t, "slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		io.WriteString(w, `{"code":"ok"}`)
	})
	f := newFixture(t, StaticRoutes{"vm-a": slow.URL}, func(c *Config) { c.AgentInFlight = 1 })
	tok := f.is.mint(t, "vm-a", "db-1", time.Now(), time.Minute)

	other := f.is.mint(t, "vm-z", "db-1", time.Now(), time.Minute)
	if r := f.do(t, "192.0.2.1", other, nil); r.status != 421 || r.code() != "wrong_owner" {
		t.Errorf("unknown vm: %d %v", r.status, r.resp)
	}
	for _, v := range []string{"0", "-5", "x", "3600001"} {
		if r := f.do(t, "192.0.2.1", tok, map[string]string{front.TimeoutHeader: v}); r.status != 400 {
			t.Errorf("X-Timeout-Ms %q: %d", v, r.status)
		}
	}
	for _, tc := range []struct {
		method, path string
		body         string
		status       int
	}{
		{http.MethodGet, front.QueryPath, "", 405},
		{http.MethodPost, "/v1/other", "", 404},
		{http.MethodPost, front.QueryPath, strings.Repeat("x", 2<<10), 413},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		f.e.ServeHTTP(w, req)
		if w.Code != tc.status || !bytes.Contains(w.Body.Bytes(), []byte(`"bad_request"`)) {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}

	// Bulkhead: one in flight to vm-a, the next is shed at once.
	done := make(chan result)
	go func() { done <- f.do(t, "192.0.2.1", tok, nil) }()
	for slow.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if r := f.do(t, "192.0.2.1", tok, nil); r.status != 503 || r.code() != "shed" {
		t.Errorf("bulkhead full: %d %v", r.status, r.resp)
	}
	close(release)
	if r := <-done; r.status != 200 {
		t.Errorf("first: %d %v", r.status, r.resp)
	}

	f.e.Drain()
	if r := f.do(t, "192.0.2.1", tok, nil); r.status != 503 || r.code() != "shed" {
		t.Errorf("draining: %d %v", r.status, r.resp)
	}
	if slow.calls.Load() != 1 {
		t.Errorf("agent calls %d, want 1", slow.calls.Load())
	}
}

func TestMetrics(t *testing.T) {
	a := newFakeAgent(t, "a", nil)
	f := newFixture(t, StaticRoutes{"vm-a": a.URL}, nil)
	f.do(t, "192.0.2.1", f.is.mint(t, "vm-a", "db-1", time.Now(), time.Minute), nil)
	f.do(t, "192.0.2.1", "garbage", nil)
	reg := metrics.NewRegistry()
	f.e.Register(reg)
	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`tsdb_requests_total{point="edge",class="app_error",code="ticket_invalid"} 1`,
		`tsdb_edge_agent_answers_total{status="2xx"} 1`,
		`tsdb_edge_tickets_total{result="miss"} 2`,
		`tsdb_breaker_state{dep="agent:vm-a",state="closed"} 1`,
		`tsdb_edge_agent_in_flight{vm="vm-a"} 0`,
		`tsdb_in_flight{point="edge"} 0`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %s in\n%s", want, b.String())
		}
	}
}

func TestNewValidates(t *testing.T) {
	is := newIssuer(t)
	ok := Config{Resolver: StaticRoutes{}, Gate: NewGate(is.ring, 0, 0, 0), Limiter: ratelimit.NewSourceLimiter(1, 1, 1),
		Transport: http.DefaultTransport, DefaultTimeout: time.Second, MaxTimeout: time.Second, Grace: time.Second,
		BodyReadTimeout: time.Second, MaxBody: 1}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"no resolver":           func(c *Config) { c.Resolver = nil },
		"default over max":      func(c *Config) { c.MaxTimeout = time.Millisecond },
		"bad breaker threshold": func(c *Config) { c.Breaker.FailureRate = 2 },
	} {
		c := ok
		mod(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
