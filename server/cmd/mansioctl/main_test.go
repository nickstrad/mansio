package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickstrad/mansio/server/internal/tenancy"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancyhttp"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancytest"
)

const adminTok = "admin-token-0123456789"

// sent is one request the test server got.
type sent struct {
	method, path, auth, contentType, body string
}

// rig is the real admin API (tenancyhttp over the in-memory store) behind a recorder.
type rig struct {
	srv   *httptest.Server
	store *tenancytest.Fake
	mu    sync.Mutex
	got   []sent
}

func (r *rig) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(b))
		r.mu.Lock()
		r.got = append(r.got, sent{req.Method, req.URL.Path, req.Header.Get("Authorization"), req.Header.Get("Content-Type"), string(b)})
		r.mu.Unlock()
		next.ServeHTTP(w, req)
	})
}

func (r *rig) requests() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.got
	r.got = nil
	return out
}

// adminAPI mounts tenancyhttp in api-key mode, as agentd and controld do.
func adminAPI(t *testing.T) (*rig, http.Handler) {
	t.Helper()
	f := tenancytest.NewFake()
	svc, err := tenancy.New(f, tenancy.Config{Mode: tenancy.APIKeys, Apps: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := tenancyhttp.Mount(mux, svc, tenancyhttp.Config{AdminToken: adminTok, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatal(err)
	}
	r := &rig{store: f}
	return r, r.handler(mux)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r, h := adminAPI(t)
	r.srv = httptest.NewServer(h)
	t.Cleanup(r.srv.Close)
	return r
}

// env is a getenv over m.
func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// run runs mansioctl against r with the admin token in the environment.
func (r *rig) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runEnv(context.Background(), env(map[string]string{"ADMIN_TOKEN": adminTok, "MANSIOCTL_ADDR": r.srv.URL}), args...)
}

func runEnv(ctx context.Context, getenv func(string) string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := realMain(ctx, args, getenv, &out, &errb)
	return code, out.String(), errb.String()
}

// Bad usage exits 2 with a message and sends nothing.
func TestUsage(t *testing.T) {
	r := newRig(t)
	const id = "11111111-1111-4111-8111-111111111111"
	for _, args := range [][]string{
		{},
		{"nope"},
		{"tenants"},
		{"tenants", "nope"},
		{"-bogus", "tenants", "list"},
		{"-format", "yaml", "tenants", "list"},
		{"-timeout", "0s", "tenants", "list"},
		{"-addr", "ftp://x", "tenants", "list"},
		{"-addr", "http://u:p@127.0.0.1:1", "tenants", "list"},
		{"-addr", "127.0.0.1:7070", "tenants", "list"},
		{"tenants", "list", "extra"},
		{"tenants", "create"},
		{"tenants", "create", "a", "b"},
		{"tenants", "create", "   "},
		{"tenants", "create", "bad\x1bname"},
		{"tenants", "create", "ÄÖÜ"}, // no ASCII letters or digits: no default slug
		{"tenants", "create", "Acme", "-slug", "bad\nslug"},
		{"tenants", "create", "Acme", "-slug", "--name"},
		{"tenants", "create", "Acme", "-slug"},
		{"tenants", "create", "Acme", "-name", "x"}, // create has no -name
		{"tenants", "show"},
		{"tenants", "show", "ACME"},
		{"tenants", "show", strings.ToUpper("aaaaaaaa-1111-4111-8111-111111111111")},
		{"tenants", "show", id, id},
		{"tenants", "deactivate", "123"},
		{"tenants", "issue-key", id, "-name", "\x7f"},
		{"tenants", "issue-key", id, "-name", " "},
		{"tenants", "issue-key", "0123456789abcdef"},
		{"tenants", "revoke-key"},
		{"tenants", "revoke-key", "0123456789abcde"},   // 15 hex: off point
		{"tenants", "revoke-key", "0123456789abcdef0"}, // 17 hex: off point
		{"tenants", "revoke-key", "0123456789ABCDEF"},
	} {
		code, out, errb := r.run(t, args...)
		if code != exitUsage || out != "" || errb == "" {
			t.Errorf("%q: exit %d out %q err %q, want %d with a message", args, code, out, errb, exitUsage)
		}
	}
	if got := r.requests(); len(got) != 0 {
		t.Errorf("bad usage sent %v", got)
	}
	// The token is required, from a file or the environment.
	if code, _, errb := runEnv(context.Background(), env(map[string]string{"MANSIOCTL_ADDR": r.srv.URL}), "tenants", "list"); code != exitUsage ||
		!strings.Contains(errb, "ADMIN_TOKEN") {
		t.Errorf("no token: exit %d err %q", code, errb)
	}
	// -h prints the usage, with every group and command and why the token is not a flag.
	code, _, errb := r.run(t, "-h")
	if code != exitUsage || !strings.Contains(errb, "tenants revoke-key <key-id>") || !strings.Contains(errb, "shell history") {
		t.Errorf("-h: exit %d err %q", code, errb)
	}
}

func TestDeriveSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Acme":            "acme",
		"Acme Corp, Inc.": "acme-corp-inc",
		"--x--":           "x",
		"a__b":            "a-b",
		"Zürich 2":        "z-rich-2",
		"ÄÖÜ":             "",
		"9":               "9",
	} {
		if got := deriveSlug(in); got != want {
			t.Errorf("deriveSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTable(t *testing.T) {
	got := table([]string{"id", "name"}, [][]string{{"1", "a\x1bb"}, {"22", "é"}})
	want := "id  name\n--  ----\n1   a b\n22  é\n(2 rows)"
	if got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}
	if got := table([]string{"id"}, nil); got != "id\n--\n(0 rows)" {
		t.Errorf("empty table: %q", got)
	}
}

// The token comes from -token-file (the token alone or a JSON secrets file, mode 0600) over $ADMIN_TOKEN.
func TestTokenSources(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		return p
	}
	plain := write("token", adminTok+"\n", 0o600)
	secrets := write("secrets.json", `{"pg_dsn":"postgres://x","admin_token":"`+adminTok+`"}`, 0o600)
	open := write("open", adminTok, 0o644)
	noToken := write("empty.json", `{"pg_dsn":"x"}`, 0o600)
	wrongEnv := env(map[string]string{"ADMIN_TOKEN": "wrong-token-0123456789", "MANSIOCTL_ADDR": r.srv.URL})
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"-token-file", plain}, exitOK},
		{[]string{"-token-file", secrets}, exitOK},
		{[]string{"-token-file", open}, exitUsage},
		{[]string{"-token-file", noToken}, exitUsage},
		{[]string{"-token-file", filepath.Join(dir, "missing")}, exitUsage},
		{nil, exitError}, // the environment's wrong token: 401
	} {
		code, _, errb := runEnv(context.Background(), wrongEnv, append(tc.args, "tenants", "list")...)
		if code != tc.code {
			t.Errorf("%v: exit %d, want %d (%s)", tc.args, code, tc.code, errb)
		}
	}
	for _, s := range r.requests() {
		if s.auth != "Bearer "+adminTok && s.auth != "Bearer wrong-token-0123456789" {
			t.Errorf("sent Authorization %q", s.auth)
		}
	}
}
