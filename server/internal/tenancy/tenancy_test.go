package tenancy_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det/dettest"
	"github.com/nickstrad/mansio/server/internal/tenancy"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancytest"
)

var ctx = context.Background()

func TestFakeStoreContract(t *testing.T) {
	tenancytest.RunStoreSuite(t, func(t *testing.T) tenancytest.Subject {
		f := tenancytest.NewFake()
		return tenancytest.Subject{Store: f, AddLegacy: func(_ *testing.T, id, tenant string, hash [32]byte) { f.AddLegacyKey(id, tenant, hash) }}
	})
}

func newService(t *testing.T, mode tenancy.Mode, mod func(*tenancy.Config)) (*tenancy.Service, *tenancytest.Fake, *dettest.Clock) {
	t.Helper()
	f := tenancytest.NewFake()
	clock := dettest.NewClock(time.Unix(1_800_000_000, 0))
	cfg := tenancy.Config{Mode: mode, Clock: clock, CacheTTL: time.Second, Apps: []string{"a", "app", "any-app"}}
	if mod != nil {
		mod(&cfg)
	}
	s, err := tenancy.New(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, clock
}

func reason(t *testing.T, err error) string {
	t.Helper()
	var ae *tenancy.AuthError
	if !errors.As(err, &ae) || !errors.Is(err, tenancy.ErrUnauthenticated) {
		t.Fatalf("err = %v, want an AuthError", err)
	}
	return ae.Reason
}

func TestConfigRefusals(t *testing.T) {
	f := tenancytest.NewFake()
	for name, cfg := range map[string]tenancy.Config{
		"off without tokens":  {Mode: tenancy.Off},
		"short app token":     {Mode: tenancy.Off, AppTokens: map[string]string{"app": "short"}},
		"api-key, no store":   {Mode: tenancy.APIKeys},
		"unknown mode":        {Mode: 9},
		"session ttl too low": {Mode: tenancy.Sessions, SessionTTL: time.Second, Apps: []string{"a"}},
		"negative cache ttl":  {Mode: tenancy.APIKeys, CacheTTL: -1, Apps: []string{"a"}},
		"no apps":             {Mode: tenancy.APIKeys},
	} {
		var store tenancy.Store = f
		if name == "api-key, no store" {
			store = nil
		}
		if _, err := tenancy.New(store, cfg); !errors.Is(err, tenancy.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for in, want := range map[string]bool{"off": true, "api-key": true, "session": true, "better-auth": false, "": false} {
		if _, ok := tenancy.ParseMode(in); ok != want {
			t.Errorf("ParseMode(%q) ok = %v", in, ok)
		}
	}
}

func TestOffModeAppTokens(t *testing.T) {
	tokens := map[string]string{"one": "0123456789abcdef-one", "two": "0123456789abcdef-two"}
	s, f, _ := newService(t, tenancy.Off, func(c *tenancy.Config) { c.AppTokens = tokens })
	p, err := s.Authenticate(ctx, tokens["one"], "one")
	if err != nil || p.Tenant != "default" || p.Sub != "app:one" {
		t.Fatalf("own token: %+v %v", p, err)
	}
	for _, c := range []struct{ token, app, reason string }{
		{tokens["two"], "one", tenancy.ReasonMismatch}, // another app's token
		{tokens["one"], "three", tenancy.ReasonMismatch},
		{tokens["one"] + "x", "one", tenancy.ReasonMismatch},
		{"", "one", tenancy.ReasonMissing},
		{"has space", "one", tenancy.ReasonMalformed},
		{strings.Repeat("a", 513), "one", tenancy.ReasonMalformed},
	} {
		_, err := s.Authenticate(ctx, c.token, c.app)
		if got := reason(t, err); got != c.reason {
			t.Errorf("%q for %s: reason %s, want %s", c.token, c.app, got, c.reason)
		}
	}
	if f.Calls != 0 {
		t.Fatalf("off mode touched the store %d times", f.Calls)
	}
}

func TestAPIKeysNewAndLegacy(t *testing.T) {
	s, f, clock := newService(t, tenancy.APIKeys, nil)
	tn, key, err := s.CreateTenant(ctx, "Acme", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key.Plaintext, "mansio_"+key.ID+"_") || key.Tenant != tn.ID || key.Name != "default" {
		t.Fatalf("issued %+v", key)
	}
	p, err := s.Authenticate(ctx, key.Plaintext, "any-app")
	if err != nil || p.Tenant != tn.ID || p.Sub != "key:"+key.ID {
		t.Fatalf("new key: %+v %v", p, err)
	}
	// A wrong secret under a real id is refused (constant-time compare against the stored hash).
	forged := key.Plaintext[:len(key.Plaintext)-2] + "AA"
	if forged == key.Plaintext {
		forged = key.Plaintext[:len(key.Plaintext)-2] + "BA"
	}
	reason(t, func() error { _, err := s.Authenticate(ctx, forged, "a"); return err }())

	legacy := "mansio_" + strings.Repeat("A", 42) + "E" // 43 base64url chars = 32 bytes
	f.AddLegacyKey("00000000-0000-4000-8000-000000000001", tn.ID, sha256.Sum256([]byte(legacy)))
	if p, err := s.Authenticate(ctx, legacy, "a"); err != nil || p.Tenant != tn.ID {
		t.Fatalf("legacy key: %+v %v", p, err)
	}
	for _, bad := range []string{"mansio_unknown", "mansio_" + strings.Repeat("A", 43), "not-a-key-at-all", "mansio_0123456789abcdef_short"} {
		if got := reason(t, func() error { _, err := s.Authenticate(ctx, bad, "a"); return err }()); got != tenancy.ReasonMismatch {
			t.Errorf("%s: %s", bad, got)
		}
	}
	// A session-shaped token in api-key mode is refused without a store call.
	before := f.Calls
	reason(t, func() error { _, err := s.Authenticate(ctx, "abcdefghijklmnopqrstuvwxyzABCDEF", "a"); return err }())
	if f.Calls != before {
		t.Fatal("api-key mode looked up a session")
	}
	_ = clock
}

func TestCacheRevocationAndUnavailable(t *testing.T) {
	s, f, clock := newService(t, tenancy.APIKeys, nil)
	tn, key, err := s.CreateTenant(ctx, "Acme", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, key.Plaintext, "a"); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls
	if _, err := s.Authenticate(ctx, key.Plaintext, "a"); err != nil || f.Calls != calls {
		t.Fatalf("second call not cached: %v, %d store calls", err, f.Calls-calls)
	}
	// A negative answer is cached too.
	reason(t, func() error {
		_, err := s.Authenticate(ctx, "mansio_0123456789abcdef_"+strings.Repeat("A", 43), "a")
		return err
	}())
	calls = f.Calls
	reason(t, func() error {
		_, err := s.Authenticate(ctx, "mansio_0123456789abcdef_"+strings.Repeat("A", 43), "a")
		return err
	}())
	if f.Calls != calls {
		t.Fatal("refusal not cached")
	}
	// A local revocation applies at once, cache or not.
	if err := s.RevokeKey(ctx, tn.ID, key.ID); err != nil {
		t.Fatal(err)
	}
	reason(t, func() error { _, err := s.Authenticate(ctx, key.Plaintext, "a"); return err }())
	// A store outage is passed on as unavailable and never cached.
	k2, err := s.IssueKey(ctx, tn.ID, "second")
	if err != nil {
		t.Fatal(err)
	}
	f.Fail = tenancy.ErrUnavailable
	if _, err := s.Authenticate(ctx, k2.Plaintext, "a"); !errors.Is(err, tenancy.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	f.Fail = nil
	if _, err := s.Authenticate(ctx, k2.Plaintext, "a"); err != nil {
		t.Fatalf("after the outage: %v", err)
	}
	// Another process's revocation is seen after CacheTTL.
	if err := f.RevokeKey(ctx, tn.ID, k2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, k2.Plaintext, "a"); err != nil {
		t.Fatalf("within the TTL the cached answer stands: %v", err)
	}
	clock.Advance(time.Second)
	reason(t, func() error { _, err := s.Authenticate(ctx, k2.Plaintext, "a"); return err }())
}

func TestCacheIsBounded(t *testing.T) {
	s, _, _ := newService(t, tenancy.APIKeys, func(c *tenancy.Config) { c.CacheMax = 2 })
	for i := range 5 {
		reason(t, func() error {
			_, err := s.Authenticate(ctx, "mansio_"+strings.Repeat(string(rune('A'+i)), 43), "a")
			return err
		}())
	}
	if n := tenancy.CacheLen(s); n > 2 {
		t.Fatalf("cache holds %d entries, max 2", n)
	}
}

func TestDeactivationRefusesKeys(t *testing.T) {
	s, _, _ := newService(t, tenancy.APIKeys, nil)
	tn, key, _ := s.CreateTenant(ctx, "Acme", "acme")
	if _, err := s.Authenticate(ctx, key.Plaintext, "a"); err != nil {
		t.Fatal(err)
	}
	got, err := s.DeactivateTenant(ctx, tn.ID)
	if err != nil || got.Active {
		t.Fatalf("deactivate: %+v %v", got, err)
	}
	reason(t, func() error { _, err := s.Authenticate(ctx, key.Plaintext, "a"); return err }())
	if _, err := s.IssueKey(ctx, tn.ID, "late"); !errors.Is(err, tenancy.ErrDeactivated) {
		t.Fatalf("issue for a deactivated tenant: %v", err)
	}
}

func TestAdminValidation(t *testing.T) {
	s, _, _ := newService(t, tenancy.APIKeys, nil)
	tn, _, err := s.CreateTenant(ctx, "Acme", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateTenant(ctx, "Acme 2", "acme"); !errors.Is(err, tenancy.ErrConflict) {
		t.Fatalf("duplicate slug: %v", err)
	}
	checks := map[string]error{
		"empty name":      func() error { _, _, err := s.CreateTenant(ctx, " ", "x"); return err }(),
		"control in slug": func() error { _, _, err := s.CreateTenant(ctx, "n", "a\x01b"); return err }(),
		"long name":       func() error { _, _, err := s.CreateTenant(ctx, strings.Repeat("n", 201), "y"); return err }(),
		"bad tenant id":   func() error { _, err := s.GetTenant(ctx, "ACME"); return err }(),
		"bad key id":      s.RevokeKey(ctx, tn.ID, "not-an-id"),
		"bad link email":  s.Link(ctx, "nope", tn.ID),
		"empty key name":  func() error { _, err := s.IssueKey(ctx, tn.ID, ""); return err }(),
	}
	for name, err := range checks {
		if !errors.Is(err, tenancy.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.GetTenant(ctx, "00000000-0000-4000-8000-00000000ffff"); !errors.Is(err, tenancy.ErrNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
	if err := s.RevokeKey(ctx, "00000000-0000-4000-8000-00000000ffff", "0123456789abcdef"); !errors.Is(err, tenancy.ErrNotFound) {
		t.Fatalf("revoke under an unknown tenant: %v", err)
	}
	if list, err := s.ListTenants(ctx); err != nil || len(list) != 1 || list[0].ID != tn.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestSessions(t *testing.T) {
	s, _, clock := newService(t, tenancy.Sessions, func(c *tenancy.Config) { c.SessionTTL = time.Hour })
	a, akey, _ := s.CreateTenant(ctx, "A", "a")
	b, _, _ := s.CreateTenant(ctx, "B", "b")
	u, signup, err := s.SignUp(ctx, "Ann", "Ann@Example.test", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "ann@example.test" || len(signup) != 32 || len(u.ID) != 32 {
		t.Fatalf("user %+v token %q", u, signup)
	}
	if _, _, err := s.SignUp(ctx, "Ann", "ann@example.test", "correct horse"); !errors.Is(err, tenancy.ErrConflict) {
		t.Fatalf("second sign-up: %v", err)
	}
	for name, err := range map[string]error{
		"short password": func() error { _, _, err := s.SignUp(ctx, "B", "b@example.test", "short"); return err }(),
		"bad email":      func() error { _, _, err := s.SignUp(ctx, "B", "b@", "long enough"); return err }(),
	} {
		if !errors.Is(err, tenancy.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	reason(t, func() error { _, err := s.Authenticate(ctx, signup, "app"); return err }()) // no membership yet
	if err := s.Link(ctx, "ann@example.test", a.ID); err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(ctx, signup, "app") // the link cleared the cached refusal
	if err != nil || p.Tenant != a.ID || p.Sub != "session" {
		t.Fatalf("session: %+v %v", p, err)
	}
	_, signin, err := s.SignIn(ctx, "ANN@example.test", "correct horse")
	if err != nil || signin == signup {
		t.Fatalf("sign-in: %q %v", signin, err)
	}
	if p, err := s.Authenticate(ctx, signin, "app"); err != nil || p.Tenant != a.ID {
		t.Fatalf("sign-in session: %+v %v", p, err)
	}
	for _, c := range []struct{ email, password string }{
		{"ann@example.test", "correct horse!"}, {"nobody@example.test", "correct horse"}, {"bad", "x"}, {"ann@example.test", ""},
	} {
		reason(t, func() error { _, _, err := s.SignIn(ctx, c.email, c.password); return err }())
	}
	// API keys still work in session mode.
	if p, err := s.Authenticate(ctx, akey.Plaintext, "app"); err != nil || p.Tenant != a.ID {
		t.Fatalf("key in session mode: %+v %v", p, err)
	}
	// Two memberships: refused (exactly one decides the tenant).
	if err := s.Link(ctx, "ann@example.test", b.ID); err != nil {
		t.Fatal(err)
	}
	reason(t, func() error { _, err := s.Authenticate(ctx, signin, "app"); return err }())
	// Expired sessions are refused.
	s2, _, clock2 := newService(t, tenancy.Sessions, func(c *tenancy.Config) { c.SessionTTL = time.Hour })
	t2, _, _ := s2.CreateTenant(ctx, "T", "t")
	_, tok, _ := s2.SignUp(ctx, "Bo", "bo@example.test", "long enough")
	if err := s2.Link(ctx, "bo@example.test", t2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Authenticate(ctx, tok, "app"); err != nil {
		t.Fatal(err)
	}
	clock2.Advance(time.Hour + time.Second)
	reason(t, func() error { _, err := s2.Authenticate(ctx, tok, "app"); return err }())
	_ = clock
}

func TestHashSlotsBoundTheWait(t *testing.T) {
	s, _, _ := newService(t, tenancy.Sessions, func(c *tenancy.Config) { c.HashSlots = 1 })
	release := tenancy.HoldHashSlot(s)
	defer release()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, _, err := s.SignUp(cctx, "C", "c@example.test", "long enough"); !errors.Is(err, tenancy.ErrUnavailable) {
		t.Fatalf("err = %v, want unavailable once every slot is busy past the deadline", err)
	}
}

// A credential is refused for an app the agent does not serve, and a refusal decided by the credential's
// shape is neither looked up nor cached.
func TestAppsAllowlistAndShapeRefusals(t *testing.T) {
	s, f, _ := newService(t, tenancy.APIKeys, nil)
	_, key, err := s.CreateTenant(ctx, "Acme", "acme")
	if err != nil {
		t.Fatal(err)
	}
	before := f.Calls
	if got := reason(t, func() error { _, err := s.Authenticate(ctx, key.Plaintext, "not-served"); return err }()); got != tenancy.ReasonMismatch {
		t.Fatalf("unserved app: %s", got)
	}
	for range 3 {
		reason(t, func() error { _, err := s.Authenticate(ctx, "garbage-bearer-token", "a"); return err }())
	}
	if f.Calls != before || tenancy.CacheLen(s) != 0 {
		t.Fatalf("%d store calls, %d cached, want none", f.Calls-before, tenancy.CacheLen(s))
	}
}
