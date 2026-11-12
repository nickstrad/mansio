package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgent records the requests it gets and answers with status and body.
type fakeAgent struct {
	srv                     *httptest.Server
	mu                      sync.Mutex
	method, path, timeoutMs string
	auth                    string
}

func newFakeAgent(t *testing.T, status int, body string, delay time.Duration) *fakeAgent {
	f := &fakeAgent{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.method, f.path, f.timeoutMs, f.auth = r.Method, r.URL.RequestURI(), r.Header.Get("X-Timeout-Ms"), r.Header.Get("Authorization")
		f.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAgent) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeAgent) sent() (method, path, timeoutMs, auth string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.method, f.path, f.timeoutMs, f.auth
}

// runAgent runs mansioctl with an admin token in the environment (which the agent group must not send).
func runAgent(ctx context.Context, args ...string) (int, string, string) {
	return runEnv(ctx, env(map[string]string{"ADMIN_TOKEN": adminTok}), args...)
}

// Each control maps to its endpoint; an ok answer prints the JSON and exits 0.
func TestAgentCommandsHitTheirEndpoints(t *testing.T) {
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"breaker", "s3", "trip"}, "/admin/breaker/s3:trip"},
		{[]string{"breaker", "placement", "reset"}, "/admin/breaker/placement:reset"},
		{[]string{"fetch-limit", "1"}, "/admin/fetch:limit?n=1"}, // on point: the least limit
		{[]string{"limit", "verify", "1"}, "/admin/limit?queue=verify&n=1"},
		{[]string{"limit", "credential_place", "32"}, "/admin/limit?queue=credential_place&n=32"},
		{[]string{"drain"}, "/admin/drain"},
		{[]string{"passivate", "db 1"}, "/admin/passivate?key=db+1"},
		{[]string{"backup", "acme:ttt"}, "/admin/backup?key=acme%3Attt"},
		{[]string{"loglevel", "debug"}, "/admin/loglevel?level=debug"},
	} {
		f := newFakeAgent(t, 200, `{"code":"ok","x":1}`+"\n", 0)
		code, out, errb := runAgent(context.Background(), append([]string{"-agent", f.addr(), "-timeout", "3s", "agent"}, tc.args...)...)
		if code != exitOK || out != `{"code":"ok","x":1}`+"\n" || errb != "" {
			t.Errorf("%v: exit %d out %q err %q", tc.args, code, out, errb)
		}
		if method, path, ms, auth := f.sent(); method != http.MethodPost || path != tc.path || ms != "3000" || auth != "" {
			t.Errorf("%v: sent %s %s (X-Timeout-Ms %q, Authorization %q), want POST %s (3000, none)", tc.args, method, path, ms, auth, tc.path)
		}
	}
}

// A non-ok answer, no answer, a non-JSON answer and a timeout exit 1 with a message; bad usage exits 2 before
// sending anything; an interrupt exits 130.
func TestAgentFailuresAndUsage(t *testing.T) {
	bad := newFakeAgent(t, 400, `{"code":"bad_request","message":"n must be an integer >= 1"}`, 0)
	if code, out, errb := runAgent(context.Background(), "-agent", bad.addr(), "agent", "drain"); code != exitError || !strings.Contains(out, "bad_request") ||
		!strings.Contains(errb, "n must be") {
		t.Errorf("non-ok answer: exit %d out %q err %q", code, out, errb)
	}
	html := newFakeAgent(t, 502, `<html>bad gateway</html>`, 0)
	if code, _, errb := runAgent(context.Background(), "-agent", html.addr(), "agent", "drain"); code != exitError || !strings.Contains(errb, "no JSON code") {
		t.Errorf("non-JSON answer: exit %d err %q", code, errb)
	}
	if code, _, errb := runAgent(context.Background(), "-agent", "127.0.0.1:1", "agent", "drain"); code != exitError || !strings.Contains(errb, "no answer") {
		t.Errorf("nothing listening: exit %d err %q", code, errb)
	}
	slow := newFakeAgent(t, 200, `{"code":"ok"}`, time.Minute)
	t0 := time.Now()
	if code, _, _ := runAgent(context.Background(), "-agent", slow.addr(), "-timeout", "100ms", "agent", "drain"); code != exitError || time.Since(t0) > 5*time.Second {
		t.Errorf("slow agent: exit %d after %v, want 1 at the timeout", code, time.Since(t0))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, _, _ := runAgent(ctx, "-agent", slow.addr(), "agent", "drain"); code != exitInterrupted {
		t.Errorf("interrupted: exit %d", code)
	}

	unused := newFakeAgent(t, 200, `{"code":"ok"}`, 0)
	for _, args := range [][]string{
		{"agent"}, {"agent", "nope"}, {"agent", "breaker", "s3"}, {"agent", "breaker", "s3", "open"}, {"agent", "fetch-limit", "0"},
		{"agent", "fetch-limit", "x"}, {"agent", "limit", "verify"}, {"agent", "limit", "verify", "0"}, {"agent", "limit", "verify", "x"},
		{"agent", "drain", "now"}, {"agent", "passivate"}, {"agent", "backup"}, {"agent", "loglevel"}, {"-timeout", "0s", "agent", "drain"},
		{"-bogus", "agent", "drain"}, {"agent", "drain", "-bogus"},
		// The API flags are refused, not ignored: -addr here would have meant the agent.
		{"-addr", "http://" + unused.addr(), "agent", "drain"}, {"-token-file", "/dev/null", "agent", "drain"},
		{"-ca", "/dev/null", "agent", "drain"}, {"-format", "table", "agent", "drain"},
	} {
		if code, _, errb := runAgent(context.Background(), append([]string{"-agent", unused.addr()}, args...)...); code != exitUsage || errb == "" {
			t.Errorf("%v: exit %d, want %d with a message", args, code, exitUsage)
		}
	}
	for _, a := range []string{"http://127.0.0.1:7072", "127.0.0.1"} {
		if code, _, _ := runAgent(context.Background(), "-agent", a, "agent", "drain"); code != exitUsage {
			t.Errorf("-agent %s: exit %d, want %d", a, code, exitUsage)
		}
	}
	if _, path, _, _ := unused.sent(); path != "" {
		t.Errorf("bad usage sent %s", path)
	}
}

// -agent defaults to $MANSIOCTL_AGENT_ADDR; -addr and $MANSIOCTL_ADDR do not reach the agent group.
func TestAgentAddrFromEnvironment(t *testing.T) {
	f := newFakeAgent(t, 200, `{"code":"ok"}`, 0)
	other := newFakeAgent(t, 200, `{"code":"ok"}`, 0)
	e := env(map[string]string{"MANSIOCTL_AGENT_ADDR": f.addr(), "MANSIOCTL_ADDR": other.srv.URL})
	if code, out, errb := runEnv(context.Background(), e, "agent", "drain"); code != exitOK {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	if _, path, _, _ := f.sent(); path != "/admin/drain" {
		t.Errorf("sent %q to $MANSIOCTL_AGENT_ADDR", path)
	}
	if _, path, _, _ := other.sent(); path != "" {
		t.Errorf("the agent group used $MANSIOCTL_ADDR: %s", path)
	}
}
