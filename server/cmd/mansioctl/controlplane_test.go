package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/edge/edgetest"
)

// fakeControld records requests and answers status with body.
func fakeControld(t *testing.T, status int, body string) *rig {
	t.Helper()
	r := &rig{}
	r.srv = httptest.NewServer(r.handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})))
	t.Cleanup(r.srv.Close)
	return r
}

// controldSecrets writes a controld secrets file (mode 0600) holding the admin token.
func controldSecrets(t *testing.T) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"pg_dsn": "postgres://x", "control_token": "c", "admin_token": adminTok,
		"s3_access_key": "a", "s3_secret_key": "s"})
	p := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Each controld command sends its request with the admin token from -secrets and prints the answer as it came.
func TestControldCommands(t *testing.T) {
	secrets := controldSecrets(t)
	for _, tc := range []struct {
		args               []string
		method, path, body string
	}{
		{[]string{"databases", "create", "-tenant", "t1", "-name", "db"}, "POST", "/v1/databases", `{"name":"db","tenant":"t1"}`},
		{[]string{"databases", "get", "-db", "t1:db"}, "GET", "/v1/databases/t1:db", ""},
		{[]string{"databases", "move", "-db", "t1:db", "-to", "vm-b"}, "POST", "/v1/databases/t1:db/move", `{"to":"vm-b"}`},
		{[]string{"databases", "move", "-db", "t1:db"}, "POST", "/v1/databases/t1:db/move", `{"to":""}`},
		{[]string{"databases", "suspend", "-db", "t1:db"}, "POST", "/v1/databases/t1:db/suspend", ""},
		{[]string{"databases", "resume", "-db", "t1:db"}, "POST", "/v1/databases/t1:db/resume", `{"to":""}`},
		{[]string{"databases", "drop", "-db", "a b/c"}, "POST", "/v1/databases/a%20b%2Fc/drop", ""},
		{[]string{"vms", "list"}, "GET", "/v1/vms", ""},
		{[]string{"vms", "attest-dead", "-vm", "vm-b", "-by", "ops"}, "POST", "/v1/vms/vm-b/attest-dead", `{"by":"ops"}`},
		{[]string{"vms", "restore", "-vm", "vm-b"}, "POST", "/v1/vms/vm-b/restore", ""},
		{[]string{"keys", "create", "-tenant", "t1", "-principal", "me"}, "POST", "/v1/tenants/t1/api-keys", `{"perms":"read|write","principal":"me"}`},
		{[]string{"keys", "create", "-tenant", "t1", "-principal", "me", "-perms", "read"}, "POST", "/v1/tenants/t1/api-keys", `{"perms":"read","principal":"me"}`},
		{[]string{"keys", "revoke", "-id", "0123456789abcdef"}, "POST", "/v1/api-keys/0123456789abcdef/revoke", ""},
		{[]string{"limits", "set", "-queue", "resolve_load", "-n", "1"}, "POST", "/v1/limits/resolve_load", `{"limit":1}`},
	} {
		r := fakeControld(t, 200, `{"x": 1}`)
		code, out, errb := runEnv(context.Background(), env(nil), append([]string{"-addr", r.srv.URL, "-secrets", secrets}, tc.args...)...)
		if code != exitOK || out != `{"x": 1}` || errb != "" {
			t.Errorf("%v: exit %d out %q err %q", tc.args, code, out, errb)
		}
		got := r.requests()
		if len(got) != 1 {
			t.Errorf("%v: sent %v", tc.args, got)
			continue
		}
		s := got[0]
		path := s.path
		if s.method != tc.method || path != tc.path || s.body != tc.body || s.auth != "Bearer "+adminTok {
			t.Errorf("%v: sent %+v, want %s %s %s", tc.args, s, tc.method, tc.path, tc.body)
		}
	}
}

// An error answer is printed and exits 1; bad usage exits 2 and sends nothing.
func TestControldFailuresAndUsage(t *testing.T) {
	r := fakeControld(t, 409, `{"code":"conflict","message":"vm-b is not attested dead"}`)
	e := env(map[string]string{"ADMIN_TOKEN": adminTok, "MANSIOCTL_ADDR": r.srv.URL})
	if code, out, errb := runEnv(context.Background(), e, "vms", "restore", "-vm", "vm-b"); code != exitError ||
		out != `{"code":"conflict","message":"vm-b is not attested dead"}` || !strings.Contains(errb, "HTTP 409") {
		t.Errorf("error answer: exit %d out %q err %q", code, out, errb)
	}
	r.requests()
	for _, args := range [][]string{
		{"databases", "create", "-tenant", "t1"}, {"databases", "get"}, {"databases", "move", "-to", "vm-b"}, {"databases", "get", "t1:db"},
		{"databases", "get", "-db", "t1:db", "extra"}, {"vms", "attest-dead", "-vm", "vm-b"}, {"vms", "restore"}, {"vms", "list", "x"},
		{"keys", "create", "-tenant", "t1"}, {"keys", "revoke"}, {"limits", "set", "-queue", "q"}, {"limits", "set", "-queue", "q", "-n", "0"},
		{"limits", "set", "-n", "1"}, {"limits", "set", "-queue", "q", "-n", "x"}, {"databases", "nope"},
		{"-format", "table", "vms", "list"},
		{"-agent", "127.0.0.1:7072", "vms", "list"}, // -agent is the agent group's
	} {
		if code, _, errb := runEnv(context.Background(), e, args...); code != exitUsage || errb == "" {
			t.Errorf("%v: exit %d err %q, want %d", args, code, errb, exitUsage)
		}
	}
	if got := r.requests(); len(got) != 0 {
		t.Errorf("bad usage sent %v", got)
	}
}

// controld over HTTPS with a private CA: -ca works, the system roots do not.
func TestControldPrivateCA(t *testing.T) {
	ca := edgetest.NewCA(t)
	caFile, _, _ := ca.WriteFiles(t, t.TempDir(), "controld", "127.0.0.1")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `[]`) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.TLS(t, "127.0.0.1")}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	secrets := controldSecrets(t)
	if code, out, errb := runEnv(context.Background(), env(nil), "-addr", srv.URL, "-secrets", secrets, "-ca", caFile, "vms", "list"); code != exitOK || out != "[]" {
		t.Errorf("with -ca: exit %d out %q err %q", code, out, errb)
	}
	if code, _, errb := runEnv(context.Background(), env(nil), "-addr", srv.URL, "-secrets", secrets, "vms", "list"); code != exitError ||
		!strings.Contains(errb, "no answer") {
		t.Errorf("without -ca: exit %d err %q", code, errb)
	}
}
