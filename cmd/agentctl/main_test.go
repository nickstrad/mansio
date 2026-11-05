package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeAgent records the requests it gets and answers with status and body.
type fakeAgent struct {
	srv          *httptest.Server
	method, path string
	timeoutMs    string
}

func newFake(t *testing.T, status int, body string, delay time.Duration) *fakeAgent {
	f := &fakeAgent{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.method, f.path, f.timeoutMs = r.Method, r.URL.RequestURI(), r.Header.Get("X-Timeout-Ms")
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

func run(ctx context.Context, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := realMain(ctx, args, func(string) string { return "" }, &out, &errb)
	return code, out.String(), errb.String()
}

// Each command maps to its endpoint; an ok answer prints the JSON and exits 0.
func TestCommandsHitTheirEndpoints(t *testing.T) {
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"breaker", "s3", "trip"}, "/admin/breaker/s3:trip"},
		{[]string{"breaker", "placement", "reset"}, "/admin/breaker/placement:reset"},
		{[]string{"fetch-limit", "1"}, "/admin/fetch:limit?n=1"}, // on point: the least limit
		{[]string{"drain"}, "/admin/drain"},
		{[]string{"passivate", "db 1"}, "/admin/passivate?key=db+1"},
		{[]string{"loglevel", "debug"}, "/admin/loglevel?level=debug"},
	} {
		f := newFake(t, 200, `{"code":"ok","x":1}`+"\n", 0)
		code, out, errb := run(context.Background(), append([]string{"-addr", f.addr(), "-timeout", "3s"}, tc.args...)...)
		if code != exitOK || out != `{"code":"ok","x":1}`+"\n" || errb != "" {
			t.Errorf("%v: exit %d out %q err %q", tc.args, code, out, errb)
		}
		if f.method != http.MethodPost || f.path != tc.path || f.timeoutMs != "3000" {
			t.Errorf("%v: sent %s %s (X-Timeout-Ms %q), want POST %s (3000)", tc.args, f.method, f.path, f.timeoutMs, tc.path)
		}
	}
}

// A non-ok answer, no answer, a non-JSON answer and a timeout exit 1 with a message; bad usage exits 2 before
// sending anything; an interrupt exits 130.
func TestFailuresAndUsage(t *testing.T) {
	bad := newFake(t, 400, `{"code":"bad_request","message":"n must be an integer >= 1"}`, 0)
	if code, out, errb := run(context.Background(), "-addr", bad.addr(), "drain"); code != exitError || !strings.Contains(out, "bad_request") ||
		!strings.Contains(errb, "n must be") {
		t.Errorf("non-ok answer: exit %d out %q err %q", code, out, errb)
	}
	html := newFake(t, 502, `<html>bad gateway</html>`, 0)
	if code, _, errb := run(context.Background(), "-addr", html.addr(), "drain"); code != exitError || !strings.Contains(errb, "no JSON code") {
		t.Errorf("non-JSON answer: exit %d err %q", code, errb)
	}
	if code, _, errb := run(context.Background(), "-addr", "127.0.0.1:1", "drain"); code != exitError || !strings.Contains(errb, "no answer") {
		t.Errorf("nothing listening: exit %d err %q", code, errb)
	}
	slow := newFake(t, 200, `{"code":"ok"}`, time.Minute)
	t0 := time.Now()
	if code, _, _ := run(context.Background(), "-addr", slow.addr(), "-timeout", "100ms", "drain"); code != exitError || time.Since(t0) > 5*time.Second {
		t.Errorf("slow agent: exit %d after %v, want 1 at the timeout", code, time.Since(t0))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, _, _ := run(ctx, "-addr", slow.addr(), "drain"); code != exitInterrupted {
		t.Errorf("interrupted: exit %d", code)
	}

	unused := newFake(t, 200, `{"code":"ok"}`, 0)
	for _, args := range [][]string{
		{}, {"nope"}, {"breaker", "s3"}, {"breaker", "s3", "open"}, {"fetch-limit", "0"}, {"fetch-limit", "x"},
		{"drain", "now"}, {"passivate"}, {"loglevel"}, {"-timeout", "0s", "drain"}, {"-bogus"},
	} {
		if code, _, _ := run(context.Background(), append([]string{"-addr", unused.addr()}, args...)...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
	if unused.path != "" {
		t.Errorf("bad usage sent %s", unused.path)
	}
}

// -addr defaults to $AGENTCTL_ADDR.
func TestAddrFromEnvironment(t *testing.T) {
	f := newFake(t, 200, `{"code":"ok"}`, 0)
	var out bytes.Buffer
	env := func(k string) string {
		if k == "AGENTCTL_ADDR" {
			return f.addr()
		}
		return ""
	}
	if code := realMain(context.Background(), []string{"drain"}, env, &out, &out); code != exitOK || f.path != "/admin/drain" {
		t.Fatalf("exit %d, sent %q: %s", code, f.path, out.String())
	}
}
