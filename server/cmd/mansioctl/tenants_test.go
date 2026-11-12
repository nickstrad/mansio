package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/edge/edgetest"
	"github.com/nickstrad/mansio/server/internal/tenancy"
)

var keyRE = regexp.MustCompile(`mansio_[A-Za-z0-9_-]+`)

// Every command against the real admin API: what it sends, and its answer in JSON and as a table. A key's
// plaintext appears in create's and issue-key's output only.
func TestTenantsCommands(t *testing.T) {
	r := newRig(t)

	// create, JSON: one document on stdout, the save note on stderr.
	code, out, errb := r.run(t, "tenants", "create", "Acme Corp")
	if code != exitOK {
		t.Fatalf("create: exit %d: %s", code, errb)
	}
	var created struct {
		Tenant tenantJSON `json:"tenant"`
		APIKey issuedJSON `json:"apiKey"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || strings.Count(out, "\n") != 1 {
		t.Fatalf("create: not one JSON document: %q (%v)", out, err)
	}
	tn, key := created.Tenant, created.APIKey
	if tn.Name != "Acme Corp" || tn.Slug != "acme-corp" || tn.Status != "active" || !uuidRE.MatchString(tn.ID) || tn.CreatedAt == "" {
		t.Errorf("create: tenant %+v", tn)
	}
	if key.TenantID != tn.ID || key.Name != "default" || !plaintextRE.MatchString(key.Plaintext) || len(key.ID) != 16 {
		t.Errorf("create: key %+v", key)
	}
	if !strings.Contains(errb, keyNote) || keyRE.MatchString(errb) {
		t.Errorf("create: stderr %q", errb)
	}
	wantSent(t, r, sent{"POST", "/admin/tenants", "Bearer " + adminTok, "application/json", `{"name":"Acme Corp","slug":"acme-corp"}`})

	// create, table, with -slug before the name.
	code, out, _ = r.run(t, "-format", "table", "tenants", "create", "-slug", "globex", "Globex")
	if code != exitOK || !regexp.MustCompile(`(?m)^id\s+name\s+slug\s+status\s+createdAt\n-+`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^[0-9a-f-]{36}  Globex  globex  active  \S+$`).MatchString(out) ||
		len(keyRE.FindAllString(out, -1)) != 1 || !strings.Contains(out, "\nAPI key: mansio_") || !strings.HasSuffix(out, keyNote+"\n") {
		t.Errorf("create table: exit %d\n%s", code, out)
	}
	wantSent(t, r, sent{"POST", "/admin/tenants", "Bearer " + adminTok, "application/json", `{"name":"Globex","slug":"globex"}`})

	// list and show: metadata only.
	code, out, errb = r.run(t, "tenants", "list")
	var list []tenantJSON
	if code != exitOK || json.Unmarshal([]byte(out), &list) != nil || len(list) != 2 || list[0] != tn || errb != "" || keyRE.MatchString(out) {
		t.Errorf("list: exit %d out %q err %q", code, out, errb)
	}
	wantSent(t, r, sent{"GET", "/admin/tenants", "Bearer " + adminTok, "", ""})
	code, out, _ = r.run(t, "-format", "table", "tenants", "list")
	if code != exitOK || !strings.HasSuffix(out, "(2 rows)\n") || !strings.Contains(out, "acme-corp") || keyRE.MatchString(out) {
		t.Errorf("list table: exit %d\n%s", code, out)
	}
	r.requests()
	code, out, _ = r.run(t, "tenants", "show", tn.ID)
	var shown tenantJSON
	if code != exitOK || json.Unmarshal([]byte(out), &shown) != nil || shown != tn {
		t.Errorf("show: exit %d out %q", code, out)
	}
	wantSent(t, r, sent{"GET", "/admin/tenants/" + tn.ID, "Bearer " + adminTok, "", ""})

	// issue-key, default name and -name after the id.
	code, out, errb = r.run(t, "tenants", "issue-key", tn.ID)
	var issued issuedJSON
	if code != exitOK || json.Unmarshal([]byte(out), &issued) != nil || issued.Name != "default" || issued.TenantID != tn.ID ||
		!plaintextRE.MatchString(issued.Plaintext) || issued.Plaintext == key.Plaintext || !strings.Contains(errb, keyNote) {
		t.Errorf("issue-key: exit %d out %q err %q", code, out, errb)
	}
	wantSent(t, r, sent{"POST", "/admin/tenants/" + tn.ID + "/keys", "Bearer " + adminTok, "application/json", `{"name":"default"}`})
	code, out, _ = r.run(t, "-format", "table", "tenants", "issue-key", tn.ID, "-name", "ci")
	if code != exitOK || !regexp.MustCompile(`(?m)^[0-9a-f]{16}  `+tn.ID+`  ci  `).MatchString(out) || len(keyRE.FindAllString(out, -1)) != 1 {
		t.Errorf("issue-key table: exit %d\n%s", code, out)
	}
	wantSent(t, r, sent{"POST", "/admin/tenants/" + tn.ID + "/keys", "Bearer " + adminTok, "application/json", `{"name":"ci"}`})

	// revoke-key asks every tenant: the key stops authenticating, the other key still does.
	svc, _ := tenancy.New(r.store, tenancy.Config{Mode: tenancy.APIKeys, Apps: []string{"app"}})
	if _, err := svc.Authenticate(context.Background(), key.Plaintext, "app"); err != nil {
		t.Fatalf("the first key does not authenticate: %v", err)
	}
	code, out, _ = r.run(t, "tenants", "revoke-key", key.ID)
	if code != exitOK || out != `{"id":"`+key.ID+`","revoked":true}`+"\n" {
		t.Errorf("revoke-key: exit %d out %q", code, out)
	}
	got := r.requests()
	if len(got) != 3 || got[0].method != "GET" || got[0].path != "/admin/tenants" {
		t.Errorf("revoke-key sent %v, want the list then one DELETE per tenant", got)
	}
	for _, s := range got[1:] {
		if s.method != "DELETE" || !strings.HasSuffix(s.path, "/keys/"+key.ID) {
			t.Errorf("revoke-key sent %v", s)
		}
	}
	svc, _ = tenancy.New(r.store, tenancy.Config{Mode: tenancy.APIKeys, Apps: []string{"app"}}) // no cached answers
	if _, err := svc.Authenticate(context.Background(), key.Plaintext, "app"); err == nil {
		t.Error("the revoked key still authenticates")
	}
	if _, err := svc.Authenticate(context.Background(), issued.Plaintext, "app"); err != nil {
		t.Errorf("the other key stopped authenticating: %v", err)
	}
	if code, out, _ := r.run(t, "-format", "table", "tenants", "revoke-key", key.ID); code != exitOK || out != "id                revoked\n----------------  -------\n"+key.ID+"  true\n(1 row)\n" {
		t.Errorf("revoke-key table (idempotent): exit %d\n%s", code, out)
	}

	// deactivate, then show says so.
	r.requests()
	code, out, _ = r.run(t, "-format", "table", "tenants", "deactivate", tn.ID)
	if code != exitOK || !strings.Contains(out, "deactivated") {
		t.Errorf("deactivate: exit %d\n%s", code, out)
	}
	wantSent(t, r, sent{"DELETE", "/admin/tenants/" + tn.ID, "Bearer " + adminTok, "", ""})
	if code, out, _ := r.run(t, "tenants", "show", tn.ID); code != exitOK || !strings.Contains(out, `"status":"deactivated"`) {
		t.Errorf("show deactivated: exit %d out %q", code, out)
	}
}

func wantSent(t *testing.T, r *rig, want sent) {
	t.Helper()
	got := r.requests()
	if len(got) != 1 || got[0] != want {
		t.Errorf("sent %+v, want %+v", got, want)
	}
}

// Error answers exit 1 with the status, the tag and what it means; nothing of the body beyond the tag.
func TestErrorAnswers(t *testing.T) {
	r := newRig(t)
	const missing = "11111111-1111-4111-8111-111111111111"
	_, out, _ := r.run(t, "tenants", "create", "Acme")
	var created struct {
		Tenant tenantJSON `json:"tenant"`
	}
	json.Unmarshal([]byte(out), &created)
	if code, _, _ := r.run(t, "tenants", "deactivate", created.Tenant.ID); code != exitOK {
		t.Fatal("deactivate failed")
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"wrong token", map[string]string{"ADMIN_TOKEN": "wrong-token-0123456789"}, []string{"tenants", "list"},
			"GET /admin/tenants: HTTP 401 AuthError: the admin token was refused"},
		{"unknown tenant", nil, []string{"tenants", "show", missing}, "HTTP 404 UnknownTenant: not found"},
		{"taken slug", nil, []string{"tenants", "create", "Acme"}, "HTTP 409 ConflictError: conflict"},
		{"deactivated tenant", nil, []string{"tenants", "issue-key", created.Tenant.ID}, "HTTP 403 TenantDeactivated: the tenant is deactivated"},
		{"too long for the server", nil, []string{"tenants", "create", strings.Repeat("a", 201)}, "HTTP 400 SchemaError: the server refused the input as invalid"},
	} {
		e := map[string]string{"ADMIN_TOKEN": adminTok, "MANSIOCTL_ADDR": r.srv.URL}
		for k, v := range tc.env {
			e[k] = v
		}
		code, out, errb := runEnv(context.Background(), env(e), tc.args...)
		if code != exitError || out != "" || !strings.Contains(errb, tc.want) {
			t.Errorf("%s: exit %d out %q err %q, want 1 and %q", tc.name, code, out, errb, tc.want)
		}
	}
	r.store.Fail = tenancy.ErrUnavailable
	if code, _, errb := r.run(t, "tenants", "list"); code != exitError || !strings.Contains(errb, "HTTP 503 unavailable: the server or its store is unavailable") {
		t.Errorf("store down: exit %d err %q", code, errb)
	}
}

// Answers that are not the admin API's, no answer, a timeout and an interrupt.
func TestTransportFailures(t *testing.T) {
	serve := func(h http.HandlerFunc) string {
		s := httptest.NewServer(h)
		t.Cleanup(s.Close)
		return s.URL
	}
	html := serve(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		fmt.Fprint(w, "<html>secret body</html>")
	})
	notAPI := serve(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"code":"ok"}`) })
	redirect := serve(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/x", http.StatusFound)
	})
	slow := serve(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	for _, tc := range []struct {
		addr, timeout, want string
	}{
		{html, "5s", "HTTP 502: the server failed"},
		{notAPI, "5s", "without an admin API answer"},
		{redirect, "5s", "HTTP 302: a redirect, not followed"},
		{"http://127.0.0.1:1", "5s", "no answer from 127.0.0.1:1"},
		{slow, "100ms", "no answer within -timeout 100ms"},
	} {
		code, out, errb := runEnv(context.Background(), env(map[string]string{"ADMIN_TOKEN": adminTok}),
			"-addr", tc.addr, "-timeout", tc.timeout, "tenants", "list")
		if code != exitError || out != "" || !strings.Contains(errb, tc.want) || strings.Contains(errb, "secret body") {
			t.Errorf("%s: exit %d out %q err %q, want 1 and %q", tc.addr, code, out, errb, tc.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if code, _, _ := runEnv(ctx, env(map[string]string{"ADMIN_TOKEN": adminTok}), "-addr", slow, "tenants", "list"); code != exitInterrupted {
		t.Errorf("interrupted: exit %d", code)
	}
}

// An https -addr signed by a private CA works with -ca and fails without it.
func TestPrivateCA(t *testing.T) {
	r, h := adminAPI(t)
	ca := edgetest.NewCA(t)
	caFile, _, _ := ca.WriteFiles(t, t.TempDir(), "server", "127.0.0.1")
	r.srv = httptest.NewUnstartedServer(h)
	r.srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.TLS(t, "127.0.0.1")}}
	r.srv.StartTLS()
	t.Cleanup(r.srv.Close)
	if code, out, errb := r.run(t, "-ca", caFile, "tenants", "list"); code != exitOK || out != "[]\n" {
		t.Errorf("with -ca: exit %d out %q err %q", code, out, errb)
	}
	if code, _, errb := r.run(t, "tenants", "list"); code != exitError || !strings.Contains(errb, "certificate") {
		t.Errorf("without -ca: exit %d err %q", code, errb)
	}
	if code, _, _ := r.run(t, "-ca", "/nonexistent/ca.pem", "tenants", "list"); code != exitError {
		t.Errorf("unreadable -ca: exit %d", code)
	}
}
