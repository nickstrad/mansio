package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/retry"
)

// fake is a locator and any number of agents on httptest servers. Handlers are scripted per test.
type fake struct {
	t       *testing.T
	loc     *httptest.Server
	resolve func(n int, minEpoch uint64) (status int, retryAfter string, body map[string]any)

	mu       sync.Mutex
	resolves []uint64 // min_epoch of every resolve
	queries  []seen
}

// seen is one query an agent received.
type seen struct {
	agent, ticket, requestID string
	timeoutMS                int64
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t}
	f.loc = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tenant, DB string
			MinEpoch   uint64 `json:"min_epoch"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("Authorization") != "Bearer key" || req.Tenant != "t" {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"code": "unauthorized"})
			return
		}
		f.mu.Lock()
		f.resolves = append(f.resolves, req.MinEpoch)
		n := len(f.resolves)
		f.mu.Unlock()
		status, ra, body := f.resolve(n, req.MinEpoch)
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(f.loc.Close)
	return f
}

// agent starts an agent whose n-th query (1-based) is answered by answer.
func (f *fake) agent(name string, answer func(n int) (status int, retryAfter string, resp front.Response)) string {
	var n atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RequestID string `json:"request_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		ms, _ := strconv.ParseInt(r.Header.Get(front.TimeoutHeader), 10, 64)
		f.mu.Lock()
		f.queries = append(f.queries, seen{name, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), body.RequestID, ms})
		f.mu.Unlock()
		status, ra, resp := answer(int(n.Add(1)))
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	}))
	f.t.Cleanup(s.Close)
	return strings.TrimPrefix(s.URL, "http://")
}

func ok(int) (int, string, front.Response) { return 200, "", front.Response{Code: front.OK, Count: 1} }

func routeTo(vm, endpoint string, epoch uint64) map[string]any {
	return map[string]any{"code": "ok", "db": "t:d", "tenant": "t", "vm": vm, "endpoint": endpoint, "epoch": epoch,
		"ticket": vm + "-ticket-" + strconv.FormatUint(epoch, 10), "ttl_ms": 60_000}
}

// sleeps records the client's waits instead of sleeping.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (f *fake) client(mod func(*Config)) (*Client, *sleeps) {
	cfg := Config{Locator: f.loc.URL, APIKey: "key", Tenant: "t"}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(c.Close)
	s := &sleeps{}
	c.sleep = s.sleep
	return c, s
}

func write(id string) Request {
	return Request{DB: "d", Kind: front.KindWrite, SQL: "insert into t (v) values (?)", Args: []any{int64(1), []byte{1}}, RequestID: id}
}

func TestDoCachesTheRoute(t *testing.T) {
	f := newFake(t)
	a := f.agent("a", ok)
	f.resolve = func(int, uint64) (int, string, map[string]any) { return 200, "", routeTo("vm-a", a, 1) }
	var hits []bool
	c, _ := f.client(func(c *Config) { c.Hooks.Resolve = func(e ResolveEvent) { hits = append(hits, e.Hit) } })
	for range 3 {
		res, err := c.Do(context.Background(), Request{DB: "d", Kind: front.KindRead, SQL: "select 1"})
		if err != nil || res.Code != front.OK || res.VM != "vm-a" || res.Attempts != 1 {
			t.Fatalf("%+v %v", res, err)
		}
	}
	if len(f.resolves) != 1 || hits[0] || !hits[1] || !hits[2] {
		t.Fatalf("resolves %v, hits %v; want one resolve, then hits", f.resolves, hits)
	}
	if q := f.queries[0]; q.ticket != "vm-a-ticket-1" || q.requestID != "" || q.timeoutMS < 4800 || q.timeoutMS > 4900 {
		t.Fatalf("query %+v: want the route's ticket, no request id on a read, X-Timeout-Ms just under 5 s", q)
	}
}

// A cold cache under 64 concurrent calls makes one resolve (single-flight).
func TestResolveIsSingleFlight(t *testing.T) {
	f := newFake(t)
	a := f.agent("a", ok)
	f.resolve = func(int, uint64) (int, string, map[string]any) {
		time.Sleep(50 * time.Millisecond)
		return 200, "", routeTo("vm-a", a, 1)
	}
	c, _ := f.client(nil)
	var wg sync.WaitGroup
	var failed atomic.Int64
	for range 64 {
		wg.Go(func() {
			if _, err := c.Do(context.Background(), write("")); err != nil {
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	if failed.Load() != 0 || len(f.resolves) != 1 {
		t.Fatalf("%d failed, %d resolves; want 0 and 1", failed.Load(), len(f.resolves))
	}
	ids := map[string]bool{}
	for _, q := range f.queries {
		ids[q.requestID] = true
	}
	if len(ids) != 64 {
		t.Fatalf("%d distinct request ids for 64 writes", len(ids))
	}
}

// wrong_owner and fenced: drop the route, re-resolve with min_epoch = epoch + 1, retry the write with the SAME
// request id at the new owner. fenced waits its Retry-After first.
func TestRerouteAfterAMove(t *testing.T) {
	for _, tc := range []struct {
		code front.Code
		ra   string
		wait time.Duration
	}{
		{front.WrongOwner, "", 0},
		{front.Fenced, "1", time.Second},
	} {
		t.Run(string(tc.code), func(t *testing.T) {
			f := newFake(t)
			old := f.agent("old", func(int) (int, string, front.Response) {
				return 421, tc.ra, front.Response{Code: tc.code}
			})
			nu := f.agent("new", ok)
			f.resolve = func(_ int, minEpoch uint64) (int, string, map[string]any) {
				if minEpoch >= 4 {
					return 200, "", routeTo("vm-new", nu, 4)
				}
				return 200, "", routeTo("vm-old", old, 3)
			}
			c, s := f.client(nil)
			res, err := c.Do(context.Background(), write("w-1"))
			if err != nil || res.VM != "vm-new" || res.Epoch != 4 || res.Reroutes != 1 || res.Attempts != 2 {
				t.Fatalf("%+v %v", res, err)
			}
			if len(f.resolves) != 2 || f.resolves[1] != 4 {
				t.Fatalf("resolves min_epoch %v, want [0 4]", f.resolves)
			}
			if f.queries[0].requestID != "w-1" || f.queries[1].requestID != "w-1" || f.queries[1].ticket != "vm-new-ticket-4" {
				t.Fatalf("queries %+v", f.queries)
			}
			if len(s.d) != 1 || s.d[0] != tc.wait {
				t.Fatalf("waits %v, want [%s]", s.d, tc.wait)
			}
			if _, r := c.Budget().Counts(); r != 0 {
				t.Fatalf("a reroute spent the retry budget (%d)", r)
			}
			// The cache now holds the new route.
			if _, err := c.Do(context.Background(), write("w-2")); err != nil || len(f.resolves) != 2 {
				t.Fatalf("after the reroute: %v, %d resolves", err, len(f.resolves))
			}
		})
	}
}

// ticket_invalid and a VM that does not answer re-resolve without min_epoch; a generated request id stays the
// same across the attempts.
func TestRerouteOnTicketInvalidAndConnectFailure(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	for _, tc := range []struct {
		name  string
		first func(f *fake) string
		code  string
	}{
		{"ticket_invalid", func(f *fake) string {
			return f.agent("a", func(n int) (int, string, front.Response) {
				if n == 1 {
					return 401, "", front.Response{Code: front.TicketInvalid}
				}
				return ok(n)
			})
		}, "ticket_invalid"},
		{"connect refused", func(*fake) string { return dead }, "transport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			first, good := tc.first(f), f.agent("good", ok)
			f.resolve = func(n int, _ uint64) (int, string, map[string]any) {
				if n == 1 {
					return 200, "", routeTo("vm-a", first, 2)
				}
				return 200, "", routeTo("vm-a", good, 2)
			}
			var codes []string
			c, _ := f.client(func(c *Config) { c.Hooks.Retry = func(e RetryEvent) { codes = append(codes, e.Code) } })
			res, err := c.Do(context.Background(), write(""))
			if err != nil || res.Reroutes != 1 {
				t.Fatalf("%+v %v", res, err)
			}
			if len(f.resolves) != 2 || f.resolves[1] != 0 || len(codes) != 1 || codes[0] != tc.code {
				t.Fatalf("resolves %v, retry codes %v", f.resolves, codes)
			}
			last := f.queries[len(f.queries)-1].requestID
			if last == "" || len(f.queries) == 2 && f.queries[0].requestID != last {
				t.Fatalf("request ids %+v: want one generated id on every attempt", f.queries)
			}
		})
	}
}

// RI-10: unavailable is retried by default, the first retry waits at least the Retry-After (header seconds, or
// the body's ms), and it spends the budget.
func TestRetryHonorsRetryAfter(t *testing.T) {
	f := newFake(t)
	a := f.agent("a", func(n int) (int, string, front.Response) {
		switch n {
		case 1:
			return 503, "2", front.Response{Code: front.Unavailable}
		case 2:
			return 503, "1", front.Response{Code: front.Shed, RetryAfterMS: 1000}
		}
		return ok(n)
	})
	f.resolve = func(int, uint64) (int, string, map[string]any) { return 200, "", routeTo("vm-a", a, 1) }
	var events []RetryEvent
	c, s := f.client(func(c *Config) { c.Hooks.Retry = func(e RetryEvent) { events = append(events, e) } })
	res, err := c.Do(context.Background(), write(""))
	if err != nil || res.Attempts != 3 || res.Reroutes != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(s.d) != 2 || s.d[0] < 2*time.Second || s.d[1] < time.Second {
		t.Fatalf("waits %v: want >= 2 s, then >= 1 s", s.d)
	}
	if events[0].Code != "unavailable" || events[0].Hint != 2*time.Second || events[0].Delay < events[0].Hint {
		t.Fatalf("first retry %+v", events[0])
	}
	if firsts, retries := c.Budget().Counts(); firsts != 1 || retries != 2 {
		t.Fatalf("budget counts %d firsts, %d retries", firsts, retries)
	}
	if f.queries[0].requestID != f.queries[2].requestID {
		t.Fatal("a retried write changed its request id")
	}
}

// Failures that end the call: terminal codes, a spent budget, MaxReroutes, a wait past the deadline.
func TestDoGivesUp(t *testing.T) {
	answerAlways := func(status int, code front.Code, ra string) func(int) (int, string, front.Response) {
		return func(int) (int, string, front.Response) { return status, ra, front.Response{Code: code} }
	}
	tests := []struct {
		name     string
		agent    func(int) (int, string, front.Response)
		resolve  func(n int) (int, string, map[string]any)
		mod      func(*Config)
		ctx      time.Duration
		code     string
		attempts int
		waits    int
	}{
		{name: "bad_request is final", agent: answerAlways(400, front.BadRequest, ""), code: "bad_request", attempts: 1},
		{name: "stmt_timeout not retried by default", agent: answerAlways(422, front.StmtTimeout, ""), code: "stmt_timeout", attempts: 1},
		{name: "internal not retried by default", agent: answerAlways(500, front.Internal, ""), code: "internal", attempts: 1},
		{name: "max attempts (default 3)", agent: answerAlways(503, front.Shed, "1"), code: "shed", attempts: 3, waits: 2},
		{name: "budget spent", agent: answerAlways(503, front.Shed, "1"),
			mod: func(c *Config) { c.Budget = &retry.Budget{Ratio: 0.01, Burst: 1} }, code: "shed", attempts: 2, waits: 1},
		{name: "max reroutes", agent: answerAlways(421, front.WrongOwner, ""), mod: func(c *Config) { c.MaxReroutes = 2 },
			code: "wrong_owner", attempts: 3, waits: 2},
		{name: "Retry-After past the deadline: no wait", agent: answerAlways(503, front.Unavailable, "5"), ctx: time.Second,
			code: "unavailable", attempts: 1},
		{name: "locator not_found", resolve: func(int) (int, string, map[string]any) {
			return 404, "", map[string]any{"code": "not_found", "message": "no such database"}
		}, code: "resolve:not_found"},
		{name: "locator moving until reroutes run out", resolve: func(int) (int, string, map[string]any) {
			return 503, "1", map[string]any{"code": "moving"}
		}, code: "resolve:moving", waits: DefaultMaxReroutes},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			ag := tc.agent
			if ag == nil {
				ag = ok
			}
			a := f.agent("a", ag)
			f.resolve = func(n int, _ uint64) (int, string, map[string]any) {
				if tc.resolve != nil {
					return tc.resolve(n)
				}
				return 200, "", routeTo("vm-a", a, 1)
			}
			c, s := f.client(tc.mod)
			ctx := context.Background()
			if tc.ctx > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctx)
				defer cancel()
			}
			res, err := c.Do(ctx, write(""))
			if got := CodeOf(err); got != tc.code || res.Attempts != tc.attempts || len(s.d) != tc.waits {
				t.Fatalf("code %s, %d attempts, waits %v; want %s, %d, %d (%v)", got, res.Attempts, s.d, tc.code, tc.attempts, tc.waits, err)
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("%v is not a *client.Error", err)
			}
		})
	}
}

// The locator's "moving" while a new owner's claim lands carries a millisecond hint in the body: the client waits
// that, not the header's whole second.
func TestLocatorMovingHintInMilliseconds(t *testing.T) {
	f := newFake(t)
	a := f.agent("a", ok)
	f.resolve = func(n int, _ uint64) (int, string, map[string]any) {
		if n == 1 {
			return 503, "1", map[string]any{"code": "moving", "retry_after_ms": 50}
		}
		return 200, "", routeTo("vm-a", a, 1)
	}
	c, s := f.client(nil)
	res, err := c.Do(context.Background(), write(""))
	if err != nil || res.Reroutes != 1 || len(s.d) != 1 || s.d[0] != 50*time.Millisecond {
		t.Fatalf("%+v %v, waits %v", res, err, s.d)
	}
}

// A write that got no answer may have applied: the error says so, and the request id is safe to reuse.
func TestMayHaveApplied(t *testing.T) {
	f := newFake(t)
	a := f.agent("a", func(int) (int, string, front.Response) {
		return 504, "", front.Response{Code: front.Expired, Ambiguous: true}
	})
	f.resolve = func(int, uint64) (int, string, map[string]any) { return 200, "", routeTo("vm-a", a, 1) }
	c, _ := f.client(nil)
	_, err := c.Do(context.Background(), write("w"))
	var e *Error
	if !errors.As(err, &e) || e.Code != "expired" || !e.MayHaveApplied {
		t.Fatalf("%v", err)
	}
	_, err = c.Do(context.Background(), Request{DB: "d", Kind: front.KindRead, SQL: "select 1"})
	if !errors.As(err, &e) || e.MayHaveApplied {
		t.Fatalf("a read never may-have-applied: %v", err)
	}
}

func TestBadInput(t *testing.T) {
	f := newFake(t)
	c, _ := f.client(nil)
	for _, r := range []Request{{DB: "d", Kind: "delete"}, {Kind: front.KindRead}} {
		if _, err := c.Do(context.Background(), r); CodeOf(err) != "bad_request" || len(f.resolves) != 0 {
			t.Fatalf("%+v: %v", r, err)
		}
	}
	for _, cfg := range []Config{{}, {Locator: "x", APIKey: "k", Tenant: "t", Scheme: "ftp"},
		{Locator: "x", APIKey: "k", Tenant: "t", RefreshAhead: 0.95}, {Locator: "x", APIKey: "k", Tenant: "t", CAFile: "/nonexistent"}} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) accepted", cfg)
		}
	}
}

func TestEndpointURL(t *testing.T) {
	c := &Client{cfg: Config{Scheme: "https"}}
	for in, want := range map[string]string{"h:1": "https://h:1", "http://h:2/": "http://h:2", "https://edge:443": "https://edge:443"} {
		if got := c.url(in); got != want {
			t.Errorf("url(%q) = %q, want %q", in, got, want)
		}
	}
}

// With Edge set, every query goes to the edge with the route's ticket; resolves still go to the locator.
func TestQueriesGoThroughTheEdge(t *testing.T) {
	f := newFake(t)
	f.resolve = func(int, uint64) (int, string, map[string]any) { return 200, "", routeTo("vm-a", "10.255.0.1:1", 1) }
	edge := f.agent("edge", ok)
	c, _ := f.client(func(cfg *Config) { cfg.Edge = edge })
	res, err := c.Do(context.Background(), write("w1"))
	if err != nil || res.Code != front.OK || res.VM != "vm-a" {
		t.Fatalf("Do: %+v %v", res, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) != 1 || f.queries[0].agent != "edge" || f.queries[0].ticket != "vm-a-ticket-1" {
		t.Fatalf("queries %+v: want one at the edge with vm-a's ticket", f.queries)
	}
}
