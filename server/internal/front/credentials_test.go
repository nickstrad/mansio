package front_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// creds is a fake Credentials: token → tenant, with a fault switch.
type creds struct {
	tenants map[string]string
	err     error
	apps    []string // the apps it was asked about
}

func (c *creds) Authenticate(_ context.Context, token, app string) (string, string, error) {
	c.apps = append(c.apps, app)
	switch {
	case c.err != nil:
		return "", "", c.err
	case token == "":
		return "", "", &front.AuthError{Reason: "missing"}
	case c.tenants[token] == "":
		return "", "", &front.AuthError{Reason: "mismatch"}
	}
	return c.tenants[token], "key:test", nil
}

// newCredsRig is newOpsRig with Credentials at epoch 1.
func newCredsRig(t *testing.T, c *creds, dbs ...string) *opsRig {
	t.Helper()
	r := newOpsRig(t, front.Config{}, dbs...)
	v := ticket.Verifier{Keys: ticket.NewKeyRing(r.iss.Keys()), VM: vm, Skew: 5 * time.Second, MaxTTL: time.Hour}
	r.s = front.NewServer(front.Config{}, v, r.adm, front.ActorExecutor{Host: r.host}, nil)
	r.s.EnableOps(front.OpsConfig{Schemas: r.schemas, IDs: &seqIDs{}, Credentials: c, CredentialEpoch: 1})
	r.srv.Close()
	r.srv = httptest.NewServer(front.NewHandler(r.s, logx.New(r.log, slog.LevelDebug)))
	t.Cleanup(r.srv.Close)
	return r
}

// A credential decides the tenant: the body's db.tenant is ignored; tickets still work; failures are
// AuthError 401 (credentials) or ticket_invalid (a ticket that does not verify).
func TestOpsCredentials(t *testing.T) {
	c := &creds{tenants: map[string]string{"key-a": "acme", "key-b": "beta", "key-bad": "Not A Tenant"}}
	r := newCredsRig(t, c, "acme:notes", "beta:notes")
	r.schemas.add("notes", notesSchema)
	insert := `[{"op":"insert","table":"notes","value":{"title":"a","n":1}}]`
	get := `[{"op":"get","table":"notes","id":"s1"}]`
	if w := r.post(t, "key-a", request("forged", "notes", "mutation", "r1", notesFP, insert), nil); w.status != 200 {
		t.Fatalf("insert: %d %s", w.status, w.raw)
	}
	// acme's database has s1; beta's does not, whatever the body claims.
	if q := r.post(t, "key-a", request("beta", "notes", "query", "", notesFP, get), nil); q.status != 200 || !strings.Contains(q.raw, `"_id":"s1"`) {
		t.Fatalf("own database through a forged tenant: %d %s", q.status, q.raw)
	}
	if q := r.post(t, "key-b", request("acme", "notes", "query", "", notesFP, get), nil); q.status != 200 || q.raw != `{"result":[null]}`+"\n" {
		t.Fatalf("other tenant: %d %s", q.status, q.raw)
	}
	// A ticket for beta still reaches beta.
	if q := r.post(t, r.tok(t, "beta:notes", ticket.Read), request("beta", "notes", "query", "", notesFP, get), nil); q.status != 200 {
		t.Fatalf("ticket: %d %s", q.status, q.raw)
	}
	for _, tc := range []struct {
		name, auth, tag string
		status          int
		reason          any
	}{
		{"no credential", "", "AuthError", 401, "missing"},
		{"unknown", "nope", "AuthError", 401, "mismatch"},
		{"a ticket that does not verify", r.tok(t, "acme:other", ticket.Read), "ticket_invalid", 401, nil},
	} {
		got := r.post(t, tc.auth, request("acme", "notes", "query", "", notesFP, get), nil)
		if got.status != tc.status || got.tag() != tc.tag || (tc.reason != nil && got.field("reason") != tc.reason) {
			t.Errorf("%s: %d %s", tc.name, got.status, got.raw)
		}
		if tc.tag == "AuthError" && got.header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate", tc.name)
		}
	}
	for _, h := range []string{"Basic abc", "Bearer"} {
		got := r.post(t, "", request("acme", "notes", "query", "", notesFP, get), map[string]string{"Authorization": h})
		if got.status != 401 || got.field("reason") != "malformed" {
			t.Errorf("Authorization %q: %d %s", h, got.status, got.raw)
		}
	}
	if got := r.post(t, "", request("acme", "notes", "query", "", notesFP, get), map[string]string{"Authorization": "bearer key-a"}); got.status != 200 {
		t.Errorf("a lowercase scheme: %d %s", got.status, got.raw)
	}
	if got := r.post(t, "key-bad", request("acme", "notes", "query", "", notesFP, get), nil); got.status != 500 {
		t.Errorf("a tenant that makes no database id: %d %s (a broken port: internal)", got.status, got.raw)
	}
	c.err = fmt.Errorf("%w: store down", front.ErrUnavailable)
	if got := r.post(t, "key-a", request("acme", "notes", "query", "", notesFP, get), nil); got.status != 503 || got.tag() != "unavailable" || got.header.Get("Retry-After") == "" {
		t.Errorf("store down: %d %s", got.status, got.raw)
	}
	if !errors.Is(c.err, front.ErrUnavailable) || c.apps[0] != "notes" {
		t.Fatalf("asked about %v", c.apps)
	}
}

// quotaExec refuses every call with a creation quota.
type quotaExec struct{}

func (quotaExec) Exec(context.Context, front.Call) (front.Reply, error) {
	return front.Reply{}, &front.QuotaError{Scope: "tenant", Limit: 100}
}

// A credential request naming an unregistered schema is refused before admission (nothing is created); a
// creation cap is QuotaExceeded 429 with its scope and limit.
func TestOpsCredentialsSchemaAndQuota(t *testing.T) {
	c := &creds{tenants: map[string]string{"key-a": "acme"}}
	r := newCredsRig(t, c)
	r.schemas.add("notes", notesSchema)
	get := `[{"op":"get","table":"notes","id":"s1"}]`
	if got := r.post(t, "key-a", request("acme", "other", "query", "", notesFP, get), nil); got.status != 409 || got.tag() != "SchemaDriftError" {
		t.Fatalf("unregistered for the app: %d %s", got.status, got.raw)
	}
	if n, _, _ := r.adm.Counts(); n != 0 {
		t.Fatalf("%d admissions for a refused request", n)
	}
	v := ticket.Verifier{Keys: ticket.NewKeyRing(r.iss.Keys()), VM: vm, Skew: 5 * time.Second, MaxTTL: time.Hour}
	s := front.NewServer(front.Config{}, v, creatingAdmit{}, quotaExec{}, nil)
	s.EnableOps(front.OpsConfig{Schemas: r.schemas, Credentials: c, CredentialEpoch: 1})
	out := s.HandleOps(context.Background(), front.Input{Ticket: "key-a", Body: request("acme", "notes", "query", "", notesFP, get)})
	if out.Status != 429 || out.Tag != "QuotaExceeded" || !strings.Contains(string(out.Body), `"scope":"tenant"`) || !strings.Contains(string(out.Body), `"limit":100`) {
		t.Fatalf("quota: %d %s", out.Status, out.Body)
	}
}

// creatingAdmit admits everything.
type creatingAdmit struct{}

func (creatingAdmit) Admit(string, uint64) (func(), error) { return func() {}, nil }
