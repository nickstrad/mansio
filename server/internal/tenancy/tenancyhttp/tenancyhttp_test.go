package tenancyhttp_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/tenancy"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancyhttp"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancytest"
)

const admin = "admin-token-0123456789"

func rig(t *testing.T, mode tenancy.Mode, mod func(*tenancyhttp.Config)) (*httptest.Server, *tenancytest.Fake) {
	t.Helper()
	f := tenancytest.NewFake()
	cfg := tenancy.Config{Mode: mode, Apps: []string{"app"}}
	if mode == tenancy.Off {
		cfg.AppTokens = map[string]string{"app": "0123456789abcdef"}
	}
	svc, err := tenancy.New(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	hc := tenancyhttp.Config{AdminToken: admin, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mod != nil {
		mod(&hc)
	}
	if err := tenancyhttp.Mount(mux, svc, hc); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

type reply struct {
	status int
	header http.Header
	body   map[string]any
}

func call(t *testing.T, srv *httptest.Server, method, path, token string, body any) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		if s, ok := body.(string); ok {
			b = []byte(s)
		}
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return reply{res.StatusCode, res.Header, out}
}

func (r reply) tag() string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["tag"].(string)
	return s
}

func (r reply) result() map[string]any { m, _ := r.body["result"].(map[string]any); return m }

func TestAdminGate(t *testing.T) {
	srv, _ := rig(t, tenancy.APIKeys, nil)
	for _, tok := range []string{"", "wrong"} {
		for _, path := range []string{"/admin/tenants", "/admin/logs/ingest", "/admin/anything"} {
			if r := call(t, srv, "POST", path, tok, map[string]string{}); r.status != 401 || r.tag() != "AuthError" || r.header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s with %q: %d %v", path, tok, r.status, r.body)
			}
		}
	}
	if r := call(t, srv, "POST", "/admin/logs/ingest", admin, nil); r.status != 404 {
		t.Errorf("unknown admin path with the right token: %d", r.status)
	}
	if err := tenancyhttp.Mount(http.NewServeMux(), nil, tenancyhttp.Config{AdminToken: "short", Log: slog.Default()}); err == nil {
		t.Error("a short admin token was accepted")
	}
}

func TestTenantLifecycle(t *testing.T) {
	srv, f := rig(t, tenancy.APIKeys, nil)
	created := call(t, srv, "POST", "/admin/tenants", admin, map[string]string{"name": "Acme", "slug": "acme"})
	if created.status != 200 {
		t.Fatalf("create: %d %v", created.status, created.body)
	}
	tn, _ := created.result()["tenant"].(map[string]any)
	key, _ := created.result()["apiKey"].(map[string]any)
	id, _ := tn["id"].(string)
	if tn["status"] != "active" || tn["slug"] != "acme" || key["tenantId"] != id || !strings.HasPrefix(key["plaintext"].(string), "mansio_") {
		t.Fatalf("create answer %v", created.body)
	}
	if r := call(t, srv, "POST", "/admin/tenants", admin, map[string]string{"name": "Acme", "slug": "acme"}); r.status != 409 || r.tag() != "ConflictError" {
		t.Errorf("duplicate slug: %d %v", r.status, r.body)
	}
	for _, bad := range []any{map[string]string{"name": "x"}, map[string]string{"name": "x", "slug": "y", "extra": "z"}, "not json"} {
		if r := call(t, srv, "POST", "/admin/tenants", admin, bad); r.status != 400 || r.tag() != "SchemaError" {
			t.Errorf("bad body %v: %d %v", bad, r.status, r.body)
		}
	}
	if r := call(t, srv, "GET", "/admin/tenants", admin, nil); r.status != 200 || len(r.body["result"].([]any)) != 1 {
		t.Errorf("list: %d %v", r.status, r.body)
	}
	if r := call(t, srv, "GET", "/admin/tenants/"+id, admin, nil); r.status != 200 || r.result()["id"] != id {
		t.Errorf("show: %d %v", r.status, r.body)
	}
	if r := call(t, srv, "GET", "/admin/tenants/00000000-0000-4000-8000-000000000999", admin, nil); r.status != 404 || r.tag() != "UnknownTenant" {
		t.Errorf("unknown: %d %v", r.status, r.body)
	}
	issued := call(t, srv, "POST", "/admin/tenants/"+id+"/keys", admin, map[string]string{"name": "second"})
	if issued.status != 200 || issued.result()["name"] != "second" {
		t.Fatalf("issue: %d %v", issued.status, issued.body)
	}
	kid := issued.result()["id"].(string)
	if r := call(t, srv, "DELETE", "/admin/tenants/"+id+"/keys/"+kid, admin, nil); r.status != 200 || r.result()["revoked"] != true {
		t.Errorf("revoke: %d %v", r.status, r.body)
	}
	if r := call(t, srv, "DELETE", "/admin/tenants/"+id, admin, nil); r.status != 200 || r.result()["status"] != "deactivated" {
		t.Errorf("deactivate: %d %v", r.status, r.body)
	}
	if r := call(t, srv, "POST", "/admin/tenants/"+id+"/keys", admin, map[string]string{"name": "late"}); r.status != 403 || r.tag() != "TenantDeactivated" {
		t.Errorf("issue after deactivation: %d %v", r.status, r.body)
	}
	f.Fail = tenancy.ErrUnavailable
	if r := call(t, srv, "GET", "/admin/tenants", admin, nil); r.status != 503 || r.tag() != "unavailable" || r.header.Get("Retry-After") != "1" {
		t.Errorf("store down: %d %v", r.status, r.body)
	}
	f.Fail = io.ErrUnexpectedEOF // not a sentinel: a bug
	if r := call(t, srv, "GET", "/admin/tenants", admin, nil); r.status != 500 || r.tag() != "Defect" {
		t.Errorf("bug: %d %v", r.status, r.body)
	}
	// /api/auth and /admin/users/link exist only in session mode.
	if r := call(t, srv, "POST", "/api/auth/sign-in/email", "", map[string]string{}); r.status != 404 {
		t.Errorf("sign-in in api-key mode: %d", r.status)
	}
}

func TestOffModeHasNoTenantRoutes(t *testing.T) {
	srv, _ := rig(t, tenancy.Off, nil)
	if r := call(t, srv, "GET", "/admin/tenants", admin, nil); r.status != 404 {
		t.Errorf("off mode: %d", r.status)
	}
}

func TestSignUpAndSignIn(t *testing.T) {
	srv, _ := rig(t, tenancy.Sessions, func(c *tenancyhttp.Config) { c.SignInRate, c.SignInBurst = 0.001, 5 })
	tn := call(t, srv, "POST", "/admin/tenants", admin, map[string]string{"name": "A", "slug": "a"}).result()["tenant"].(map[string]any)
	up := call(t, srv, "POST", "/api/auth/sign-up/email", "", map[string]any{"name": "Ann", "email": "ann@example.test", "password": "long enough", "rememberMe": true})
	user, _ := up.body["user"].(map[string]any)
	if up.status != 200 || len(up.body["token"].(string)) != 32 || user["email"] != "ann@example.test" || user["image"] != nil {
		t.Fatalf("sign-up: %d %v", up.status, up.body)
	}
	if r := call(t, srv, "POST", "/api/auth/sign-up/email", "", map[string]string{"name": "Ann", "email": "ann@example.test", "password": "long enough"}); r.status != 422 || r.body["code"] != "USER_ALREADY_EXISTS" {
		t.Errorf("second sign-up: %d %v", r.status, r.body)
	}
	if r := call(t, srv, "POST", "/admin/users/link", admin, map[string]string{"email": "ann@example.test", "tenantId": tn["id"].(string)}); r.status != 200 {
		t.Errorf("link: %d %v", r.status, r.body)
	}
	if r := call(t, srv, "POST", "/admin/users/link", admin, map[string]string{"email": "nobody@example.test", "tenantId": tn["id"].(string)}); r.status != 404 || r.tag() != "NotFoundError" {
		t.Errorf("link unknown user: %d %v", r.status, r.body)
	}
	in := call(t, srv, "POST", "/api/auth/sign-in/email", "", map[string]string{"email": "ann@example.test", "password": "long enough"})
	if in.status != 200 || in.body["redirect"] != false || in.body["token"] == up.body["token"] {
		t.Fatalf("sign-in: %d %v", in.status, in.body)
	}
	if r := call(t, srv, "POST", "/api/auth/sign-in/email", "", map[string]string{"email": "ann@example.test", "password": "wrong password"}); r.status != 401 || r.body["code"] != "INVALID_EMAIL_OR_PASSWORD" {
		t.Errorf("wrong password: %d %v", r.status, r.body)
	}
	if r := call(t, srv, "POST", "/api/auth/sign-up/email", "", "{"); r.status != 400 {
		t.Errorf("bad JSON: %d %v", r.status, r.body)
	}
	if r := call(t, srv, "POST", "/api/auth/sign-in/email", "", map[string]string{"email": "x@example.test", "password": "y"}); r.status != 429 || r.header.Get("Retry-After") == "" {
		t.Errorf("past the burst: %d %v", r.status, r.body)
	}
}
