package front_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/front/fronttest"
	"github.com/nickstrad/ts-db-go/internal/logx"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newHTTP(t *testing.T, cfg front.Config) (*rig, *httptest.Server, *syncBuf) {
	r := newRig(t, cfg)
	var log syncBuf
	srv := httptest.NewServer(front.NewHandler(r.s, logx.New(&log, slog.LevelDebug)))
	t.Cleanup(srv.Close)
	return r, srv, &log
}

func post(t *testing.T, url, auth string, body []byte, hdr map[string]string) (*http.Response, front.Response) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+front.QueryPath, bytes.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out front.Response
	b, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("body is not JSON: %q", b)
	}
	return res, out
}

func TestHTTPStatusesAndHeaders(t *testing.T) {
	r, srv, log := newHTTP(t, front.Config{Limits: front.Limits{MaxBody: 512}})
	bearer := "Bearer " + r.tok(t, "db-1", nil)
	tests := []struct {
		name   string
		auth   string
		body   []byte
		hdr    map[string]string
		setup  func()
		status int
		code   front.Code
		header map[string]string
	}{
		{"ok", bearer, get(1), nil, nil, 200, front.OK, map[string]string{"Content-Type": "application/json"}},
		{"bearer is case-insensitive", "bearer " + r.tok(t, "db-1", nil), get(1), nil, nil, 200, front.OK, nil},
		{"no ticket", "", get(1), nil, nil, 401, front.TicketInvalid, map[string]string{"WWW-Authenticate": `Bearer realm="ts-db agent"`}},
		{"basic auth is not a ticket", "Basic abc", get(1), nil, nil, 401, front.TicketInvalid, nil},
		{"body at limit + 1", bearer, bytes.Repeat([]byte(" "), 513), nil, nil, 413, front.BadRequest, nil},
		{"bad timeout header", bearer, get(1), map[string]string{front.TimeoutHeader: "-5"}, nil, 400, front.BadRequest, nil},
		{"timeout header over an hour", bearer, get(1), map[string]string{front.TimeoutHeader: "3600001"}, nil, 400, front.BadRequest, nil},
		{"timeout header ok", bearer, get(1), map[string]string{front.TimeoutHeader: "500"}, nil, 200, front.OK, nil},
		{"shed has Retry-After", bearer, get(1), nil, func() { r.ex.Set(func(e *fronttest.Executor) { e.Err = front.ErrShed }) },
			503, front.Shed, map[string]string{"Retry-After": "1"}},
		{"unavailable has Retry-After", bearer, get(1), nil, func() { r.ex.Set(func(e *fronttest.Executor) { e.Err = front.ErrUnavailable }) },
			503, front.Unavailable, map[string]string{"Retry-After": "1"}},
		{"unavailable with a breaker hint: computed Retry-After", bearer, get(1), nil, func() {
			r.ex.Set(func(e *fronttest.Executor) { e.Err = front.WithRetryAfter(front.ErrUnavailable, 2500*time.Millisecond) })
		}, 503, front.Unavailable, map[string]string{"Retry-After": "3"}},
		{"panic in executor", bearer, get(1), nil, func() { r.ex.Set(func(e *fronttest.Executor) { e.Err, e.Panic = nil, true }) },
			500, front.Internal, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			res, out := post(t, srv.URL, tc.auth, tc.body, tc.hdr)
			if res.StatusCode != tc.status || out.Code != tc.code {
				t.Fatalf("status %d code %s (%s), want %d %s", res.StatusCode, out.Code, out.Message, tc.status, tc.code)
			}
			for k, v := range tc.header {
				if got := res.Header.Get(k); got != v {
					t.Errorf("header %s = %q, want %q", k, got, v)
				}
			}
			if out.Code == front.Internal {
				if out.LogID == "" || !strings.Contains(log.String(), `"log_id":"`+out.LogID+`"`) ||
					!strings.Contains(log.String(), `"stack"`) {
					t.Errorf("internal answer's log ID %q not logged with a stack:\n%s", out.LogID, log.String())
				}
				if strings.Contains(out.Message, "purpose") {
					t.Errorf("panic text leaked: %q", out.Message)
				}
			}
		})
	}
}

// A dependency outage fails every request: the log gets WARN lines at a bounded rate (with a count of the
// suppressed ones), never one ERROR per request (Release It F2).
func TestHTTPUnavailableLogsSampledWarn(t *testing.T) {
	r, srv, log := newHTTP(t, front.Config{})
	r.ex.Set(func(e *fronttest.Executor) { e.Err = fmt.Errorf("%w: s3 down", front.ErrUnavailable) })
	bearer := "Bearer " + r.tok(t, "db-1", nil)
	const n = 20
	for range n {
		if res, out := post(t, srv.URL, bearer, get(1), nil); res.StatusCode != 503 || out.Code != front.Unavailable {
			t.Fatalf("status %d code %s", res.StatusCode, out.Code)
		}
	}
	r.clock.Advance(time.Second) // next window: one more line, carrying the count suppressed in the last one
	post(t, srv.URL, bearer, get(1), nil)
	var warns []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		switch m["level"] {
		case "ERROR":
			t.Fatalf("unavailable logged at ERROR: %s", line)
		case "WARN":
			warns = append(warns, m)
		}
	}
	if len(warns) != front.UnavailableLogPerSecond+1 {
		t.Fatalf("%d WARN lines for %d requests over two seconds, want %d:\n%s", len(warns), n+1, front.UnavailableLogPerSecond+1, log.String())
	}
	if w := warns[len(warns)-1]; w["event"] != "FRONT.UNAVAILABLE" || w["suppressed"] != float64(n-front.UnavailableLogPerSecond) ||
		!strings.Contains(w["error"].(string), "s3 down") {
		t.Fatalf("last WARN line %v", w)
	}
}

// A client that dribbles its body (1 byte per 50 ms) is cut at BodyReadTimeout with 408, and the connection is
// usable for nothing else; the handler does not wait for the body forever (Release It T3).
func TestSlowBodyIsCut(t *testing.T) {
	const timeout = 200 * time.Millisecond
	r, srv, _ := newHTTP(t, front.Config{BodyReadTimeout: timeout})
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := get(1)
	start := time.Now()
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n",
		front.QueryPath, r.tok(t, "db-1", nil), len(body))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for _, b := range body {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if _, err := conn.Write([]byte{b}); err != nil {
				return
			}
		}
	}()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	defer res.Body.Close()
	var out front.Response
	json.NewDecoder(res.Body).Decode(&out)
	if el := time.Since(start); res.StatusCode != http.StatusRequestTimeout || out.Code != front.BadRequest || el > timeout+time.Second {
		t.Fatalf("status %d code %s after %v; want 408 bad_request within %v", res.StatusCode, out.Code, el, timeout+time.Second)
	}
}

// A body that arrives in time is unaffected by the deadlines, and later requests on the same keep-alive
// connection, after the earlier deadlines have passed, still succeed.
func TestDeadlinesAreFreshPerRequest(t *testing.T) {
	r, srv, _ := newHTTP(t, front.Config{BodyReadTimeout: 100 * time.Millisecond, WriteGrace: 100 * time.Millisecond,
		DefaultTimeout: 100 * time.Millisecond})
	bearer := "Bearer " + r.tok(t, "db-1", nil)
	for i := range 2 {
		if res, out := post(t, srv.URL, bearer, get(1), nil); res.StatusCode != 200 {
			t.Fatalf("request %d: %d %s", i, res.StatusCode, out.Message)
		}
		time.Sleep(400 * time.Millisecond) // past the first request's read and write deadlines
		r.clock.Advance(400 * time.Millisecond)
	}
	res, err := http.Get(srv.URL + front.HealthPath)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("health on a reused connection: %v %v", res, err)
	}
	res.Body.Close()
}

func TestHTTPMethodPathAndHealth(t *testing.T) {
	r, srv, _ := newHTTP(t, front.Config{})
	res, _ := http.Get(srv.URL + front.QueryPath)
	if res.StatusCode != 405 || res.Header.Get("Allow") != "POST" {
		t.Fatalf("GET query: %d", res.StatusCode)
	}
	res.Body.Close()
	res, _ = http.Get(srv.URL + "/nope")
	if res.StatusCode != 404 {
		t.Fatalf("unknown path: %d", res.StatusCode)
	}
	res.Body.Close()

	// /healthz answers while every query slot is blocked, and turns 503 when draining.
	r.ex.Set(func(e *fronttest.Executor) { e.Delay = 300 * time.Millisecond })
	tok := "Bearer " + r.tok(t, "db-1", nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { post(t, srv.URL, tok, get(1), nil) })
	}
	for r.s.InFlight() < 4 {
		time.Sleep(time.Millisecond)
	}
	t0 := time.Now()
	res, err := http.Get(srv.URL + front.HealthPath)
	if err != nil || res.StatusCode != 200 || time.Since(t0) > 100*time.Millisecond {
		t.Fatalf("healthz under load: %v %v after %v", res, err, time.Since(t0))
	}
	var h map[string]any
	json.NewDecoder(res.Body).Decode(&h)
	res.Body.Close()
	if h["status"] != "ok" || h["in_flight"].(float64) < 4 {
		t.Fatalf("healthz body %v", h)
	}
	r.s.Drain()
	res, _ = http.Get(srv.URL + front.HealthPath)
	res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatalf("healthz while draining: %d", res.StatusCode)
	}
	wg.Wait()
}

// Rows with blobs come back as {"b64": ...}; a REAL that JSON cannot carry is internal, not a broken body.
func TestHTTPEncodesValues(t *testing.T) {
	r := newRig(t, front.Config{})
	s := front.NewServer(front.Config{}, r.tv, r.adm, valueExec{}, r.clock.Now)
	srv := httptest.NewServer(front.NewHandler(s, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	res, out := post(t, srv.URL, "Bearer "+r.tok(t, "db-1", nil), get(1), nil)
	if res.StatusCode != 200 || len(out.Rows) != 1 {
		t.Fatalf("%d %+v", res.StatusCode, out)
	}
	if b, _ := json.Marshal(out.Rows[0]); string(b) != `[1,"s",{"b64":"AP8="},null]` {
		t.Fatalf("row %s", b)
	}
	res, out = post(t, srv.URL, "Bearer "+r.tok(t, "db-1", nil), body("db-1", front.KindRead, "SELECT inf"), nil)
	if res.StatusCode != 500 || out.Code != front.Internal || out.LogID == "" {
		t.Fatalf("inf: %d %+v", res.StatusCode, out)
	}
}

type valueExec struct{}

func (valueExec) Exec(_ context.Context, c front.Call) (front.Reply, error) {
	if c.SQL == "SELECT inf" {
		return front.Reply{Columns: []string{"x"}, Rows: [][]any{{posInf()}}, Count: 1}, nil
	}
	return front.Reply{Columns: []string{"i", "s", "b", "n"}, Rows: [][]any{{int64(1), "s", []byte{0, 255}, nil}}, Count: 1}, nil
}

func posInf() float64 { z := 0.0; return 1 / z }

// h2c: the same handler over unencrypted HTTP/2.
func TestHTTP2Cleartext(t *testing.T) {
	r := newRig(t, front.Config{})
	srv := httptest.NewUnstartedServer(front.NewHandler(r.s, slog.New(slog.DiscardHandler)))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	defer srv.Close()
	client := H2CClient()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+front.QueryPath, bytes.NewReader(get(1)))
	req.Header.Set("Authorization", "Bearer "+r.tok(t, "db-1", nil))
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.ProtoMajor != 2 || res.StatusCode != 200 {
		t.Fatalf("proto %s status %d", res.Proto, res.StatusCode)
	}
}

// On h2c the body deadline is per stream: a stalled body is cut with 408 and the connection keeps serving.
func TestSlowBodyIsCutOnH2C(t *testing.T) {
	const timeout = 200 * time.Millisecond
	r := newRig(t, front.Config{BodyReadTimeout: timeout})
	srv := httptest.NewUnstartedServer(front.NewHandler(r.s, slog.New(slog.DiscardHandler)))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	defer srv.Close()
	client := H2CClient()
	pr, pw := io.Pipe()
	defer pw.Close()
	go pw.Write(get(1)[:5]) // a few bytes, then nothing
	req, _ := http.NewRequest(http.MethodPost, srv.URL+front.QueryPath, pr)
	req.Header.Set("Authorization", "Bearer "+r.tok(t, "db-1", nil))
	start := time.Now()
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if el := time.Since(start); res.ProtoMajor != 2 || res.StatusCode != http.StatusRequestTimeout || el > timeout+time.Second {
		t.Fatalf("proto %s status %d after %v", res.Proto, res.StatusCode, el)
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL+front.QueryPath, bytes.NewReader(get(1)))
	req.Header.Set("Authorization", "Bearer "+r.tok(t, "db-1", nil))
	if res, err = client.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("next stream: %v %v", res, err)
	}
	res.Body.Close()
}

// H2CClient speaks HTTP/2 without TLS.
func H2CClient() *http.Client {
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: tr}
}

// BenchmarkHTTP: the whole hop over loopback (client encode, TCP, server pipeline with a cached ticket and a
// no-op executor, JSON answer, client decode), per protocol.
func BenchmarkHTTP(b *testing.B) {
	for _, proto := range []string{"h1", "h2c"} {
		b.Run(proto, func(b *testing.B) {
			r := newRig(b, front.Config{Admission: front.AdmissionLimits{TenantInFlight: 4096, DBInFlight: 4096}})
			s := front.NewServer(r.s.Config(), r.tv, r.adm, noopExec{}, nil)
			srv := httptest.NewUnstartedServer(front.NewHandler(s, slog.New(slog.DiscardHandler)))
			client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
			if proto == "h2c" {
				srv.Config.Protocols = new(http.Protocols)
				srv.Config.Protocols.SetUnencryptedHTTP2(true)
				client = H2CClient()
			}
			srv.Start()
			defer srv.Close()
			auth := "Bearer " + r.iss.Mint(b, r.iss.Claims("db-1", vm, time.Now(), time.Hour))
			payload := get(1)
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					req, _ := http.NewRequest(http.MethodPost, srv.URL+front.QueryPath, bytes.NewReader(payload))
					req.Header.Set("Authorization", auth)
					res, err := client.Do(req)
					if err != nil {
						b.Fatal(err)
					}
					var out front.Response
					json.NewDecoder(res.Body).Decode(&out)
					res.Body.Close()
					if out.Code != front.OK {
						b.Fatal(out.Code)
					}
				}
			})
		})
	}
}
