package front_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// creds is a fake Credentials: token → tenant, with fault switches: err fails every call; hold, if set before
// use, makes every call wait until it is closed or the caller gives up (a slow tenancy store).
type creds struct {
	tenants map[string]string
	err     error
	hold    chan struct{}

	mu   sync.Mutex
	apps []string // the apps it was asked about
}

func (c *creds) Authenticate(ctx context.Context, token, app string) (string, string, error) {
	c.mu.Lock()
	c.apps = append(c.apps, app)
	c.mu.Unlock()
	if c.hold != nil {
		select {
		case <-c.hold:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
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
	return newCredsRigWith(t, c, front.OpsConfig{}, dbs...)
}

// newCredsRigWith is newCredsRig with more of the ops config set (its schemas, ids and credentials are the rig's).
func newCredsRigWith(t *testing.T, c *creds, cfg front.OpsConfig, dbs ...string) *opsRig {
	t.Helper()
	r := newOpsRig(t, front.Config{}, dbs...)
	v := ticket.Verifier{Keys: ticket.NewKeyRing(r.iss.Keys()), VM: vm, Skew: 5 * time.Second, MaxTTL: time.Hour}
	r.s = front.NewServer(front.Config{}, v, r.adm, front.ActorExecutor{Host: r.host}, nil)
	cfg.Schemas, cfg.IDs, cfg.Credentials, cfg.CredentialEpoch = r.schemas, &seqIDs{}, c, 1
	r.s.EnableOps(cfg)
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
	if q := r.post(t, "key-b", request("acme", "notes", "query", "", notesFP, get), nil); q.status != 200 || q.raw != `{"result":[null],"seq":0}`+"\n" {
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
	for _, h := range []string{"bearer key-a", "Bearer  key-a ", "Bearer key-a\t"} {
		if got := r.post(t, "", request("acme", "notes", "query", "", notesFP, get), map[string]string{"Authorization": h}); got.status != 200 {
			t.Errorf("Authorization %q (scheme case, spaces around the token): %d %s", h, got.status, got.raw)
		}
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

// B6's overload scenario: a slow tenancy store under a burst of credential requests. Before, every request
// went into Credentials.Authenticate ahead of every limit in the pipeline: unbounded in number, in no
// admission gauge, each holding its handler for its whole deadline. With the pre-admission verify slots, at
// most VerifySlots requests are inside the checks; the rest are shed at once with Retry-After, the refusals
// are counted, and the ticket path (no remote step) is untouched.
func TestOpsCredentialChecksAreBounded(t *testing.T) {
	const slots, burst = 2, 12
	c := &creds{tenants: map[string]string{"key-a": "acme"}, hold: make(chan struct{})}
	r := newCredsRigWith(t, c, front.OpsConfig{VerifySlots: slots}, "acme:notes")
	r.schemas.add("notes", notesSchema)
	get := `[{"op":"get","table":"notes","id":"s1"}]`
	type answer struct {
		status int
		tag    string
		retry  string
		took   time.Duration
	}
	answers := make(chan answer, burst)
	for range burst {
		go func() {
			t0 := time.Now()
			w := r.post(t, "key-a", request("acme", "notes", "query", "", notesFP, get), nil)
			answers <- answer{w.status, w.tag(), w.header.Get("Retry-After"), time.Since(t0)}
		}()
	}
	for range burst - slots { // the refusals arrive while the store is still stuck
		select {
		case a := <-answers:
			if a.status != 503 || a.tag != "shed" || a.retry == "" || a.took > time.Second {
				t.Fatalf("a credential request beyond the verify slots: %+v, want shed 503 with Retry-After at once", a)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("requests beyond the verify slots were not refused while the store was stuck")
		}
	}
	if v := r.s.OpsVerify(); v.Limit() != slots || v.Shed() != burst-slots || v.InUse() != slots {
		t.Fatalf("verify slots %d, in use %d, shed %d; want %d, %d and %d", v.Limit(), v.InUse(), v.Shed(), slots, slots, burst-slots)
	}
	if got := r.s.InFlight(); got != slots {
		t.Fatalf("%d requests in flight with the store stuck, want the %d holding a verify slot", got, slots)
	}
	// A ticket needs no remote check: it is served while every verify slot is taken.
	if q := r.post(t, r.tok(t, "acme:notes", ticket.Read), request("acme", "notes", "query", "", notesFP, get), nil); q.status != 200 {
		t.Fatalf("ticket request with the verify slots full: %d %s", q.status, q.raw)
	}
	close(c.hold)
	for range slots {
		if a := <-answers; a.status != 200 {
			t.Fatalf("a request that held a verify slot: %+v", a)
		}
	}
	if q := r.post(t, "key-a", request("acme", "notes", "query", "", notesFP, get), nil); q.status != 200 {
		t.Fatalf("after the store recovered: %d %s", q.status, q.raw)
	}
}

// countedBody counts the bytes a handler read from a request body.
type countedBody struct {
	r    io.Reader
	read int
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += n
	return n, err
}

// B6: a draining agent refuses /v1/ops before it reads the body: the handler buffers none of it, and the
// refusal is still counted as a shed answer. (Over HTTP/1.1 net/http itself then discards up to 256 KiB of an
// unread body before it replies; that costs no memory and no slot.)
func TestOpsDrainingIsRefusedBeforeTheBody(t *testing.T) {
	r := newCredsRig(t, &creds{tenants: map[string]string{"key-a": "acme"}}, "acme:notes")
	r.schemas.add("notes", notesSchema)
	h := front.NewHandler(r.s, logx.New(r.log, slog.LevelDebug))
	send := func() (*httptest.ResponseRecorder, int) {
		body := &countedBody{r: bytes.NewReader(request("acme", "notes", "query", "", notesFP, `[{"op":"get","table":"notes","id":"s1"}]`))}
		req := httptest.NewRequest(http.MethodPost, front.OpsPath, body)
		req.Header.Set("Authorization", "Bearer key-a")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w, body.read
	}
	if w, read := send(); w.Code != 200 || read == 0 {
		t.Fatalf("before draining: %d %s, %d body bytes read", w.Code, w.Body, read)
	}
	r.s.Drain()
	w, read := send()
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"shed"`) || w.Header().Get("Retry-After") == "" || read != 0 {
		t.Fatalf("draining: %d %s, %d body bytes read; want shed 503 with Retry-After and 0", w.Code, w.Body, read)
	}
	if got := r.s.Stats().Outcomes[slices.Index(front.Codes[:], front.Shed)][0]; got != 1 {
		t.Fatalf("%d shed answers counted, want the refusal", got)
	}
}
