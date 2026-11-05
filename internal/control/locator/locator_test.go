package locator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/ticket"
	"github.com/nickstrad/ts-db-go/internal/ticket/tickettest"
)

// fakeStore is a Store with fault switches: block holds every load until released; fail and panicky make
// loads fail. Mutex-guarded (knowledge/testing/README.md §3).
type fakeStore struct {
	mu      sync.Mutex
	rows    map[string]Placement
	keys    map[string]StoredKey
	block   chan struct{} // non-nil: loads wait for it to close
	fail    error
	panicky bool
	locates map[bool]int // by primary
	keyRead int
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]Placement{}, keys: map[string]StoredKey{}, locates: map[bool]int{}}
}

func (s *fakeStore) wait(ctx context.Context) error {
	s.mu.Lock()
	b, fail, p := s.block, s.fail, s.panicky
	s.mu.Unlock()
	if b != nil {
		select {
		case <-b:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if p {
		panic("store bug")
	}
	return fail
}

func (s *fakeStore) Locate(ctx context.Context, key string, primary bool) (Placement, error) {
	s.mu.Lock()
	s.locates[primary]++
	s.mu.Unlock()
	if err := s.wait(ctx); err != nil {
		return Placement{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[key], nil
}

func (s *fakeStore) APIKey(ctx context.Context, id string) (StoredKey, error) {
	s.mu.Lock()
	s.keyRead++
	k, ok := s.keys[id]
	s.mu.Unlock()
	if !ok {
		return StoredKey{}, ErrNotFound
	}
	return k, nil
}

func (s *fakeStore) set(key string, p Placement) {
	s.mu.Lock()
	s.rows[key] = p
	s.mu.Unlock()
}

func (s *fakeStore) counts() (replica, primary int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.locates[false], s.locates[true]
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

var iss = tickettest.NewIssuer("k1")

type fixture struct {
	st  *fakeStore
	l   *Locator
	clk *clock
	key string // an API key of tenant "acme", principal "app", read|write
}

func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()
	st := newFakeStore()
	key, id, hash, err := NewAPIKey(bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	st.keys[id] = StoredKey{Tenant: "acme", Principal: "app", Perms: ticket.Read | ticket.Write, Hash: hash}
	st.set("acme:db", Placement{Intent: "active", Found: true, VM: "vm-a", Endpoint: "10.0.0.1:7070", Epoch: 3, Gen: 9})
	clk := &clock{now: time.Unix(1_800_000_000, 0)}
	cfg.Now = clk.Now
	return &fixture{st: st, l: New(st, signer{}, cfg), clk: clk, key: key}
}

// signer mints with tickettest's issuer.
type signer struct{}

func (signer) Mint(c ticket.Claims) (string, error) { c.KID = iss.KID; return ticket.Mint(iss.Priv, c) }

func (f *fixture) resolve(t *testing.T, name string, minEpoch uint64) (Route, error) {
	t.Helper()
	return f.l.Resolve(context.Background(), Request{Tenant: "acme", Name: name, APIKey: f.key, MinEpoch: minEpoch})
}

func TestResolveMintsATicketForTheOwner(t *testing.T) {
	f := newFixture(t, Config{TicketTTL: 30 * time.Second})
	r, err := f.resolve(t, "db", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.DB != "acme:db" || r.VM != "vm-a" || r.Endpoint != "10.0.0.1:7070" || r.Epoch != 3 || r.Gen != 9 || r.TTL != 30*time.Second {
		t.Fatalf("%+v", r)
	}
	v := ticket.Verifier{Keys: ticket.NewKeyRing(iss.Keys()), VM: "vm-a"}
	c, err := v.Verify(r.Ticket, f.clk.Now(), ticket.Want{DB: "acme:db", Perm: ticket.Write})
	if err != nil {
		t.Fatal(err)
	}
	if c.TID != "acme" || c.Sub != "app" || c.Epoch != 3 || c.Gen != 9 || c.EXP-c.IAT != 30 || !r.Expires.Equal(time.Unix(c.EXP, 0)) {
		t.Fatalf("claims %+v", c)
	}
}

func TestResolveAnswers(t *testing.T) {
	f := newFixture(t, Config{MovingRetry: 3 * time.Second, ClaimRetry: 20 * time.Millisecond})
	_, id, _, _ := NewAPIKey(bytes.NewReader(bytes.Repeat([]byte{1}, 64)))
	revoked, rid, rhash, _ := NewAPIKey(bytes.NewReader(bytes.Repeat([]byte{2}, 64)))
	f.st.keys[rid] = StoredKey{Tenant: "acme", Principal: "old", Perms: ticket.Read, Hash: rhash, Revoked: true}
	other, oid, ohash, _ := NewAPIKey(bytes.NewReader(bytes.Repeat([]byte{3}, 64)))
	f.st.keys[oid] = StoredKey{Tenant: "other", Principal: "x", Perms: ticket.Read, Hash: ohash}
	wrongSecret := f.key[:len(f.key)-1] + "A"
	if wrongSecret == f.key {
		wrongSecret = f.key[:len(f.key)-1] + "B"
	}
	f.st.set("acme:moving", Placement{Intent: "moving", Found: true, VM: "vm-a", Epoch: 2, Moving: true})
	f.st.set("acme:creating", Placement{Intent: "creating"})
	f.st.set("acme:suspended", Placement{Intent: "suspended", Found: true, VM: "vm-a", Epoch: 2, Moving: true})
	f.st.set("acme:dropping", Placement{Intent: "dropping", Found: true, VM: "vm-a", Epoch: 2, Moving: true})
	f.st.set("acme:dropped", Placement{Intent: "dropped"})
	f.st.set("acme:bare", Placement{Found: true, VM: "vm-b", Epoch: 1}) // no registry row: still served
	f.st.set("acme:flipped", Placement{Intent: "moving", Target: "vm-b", Found: true, VM: "vm-b", Epoch: 4})
	f.st.set("acme:unfenced", Placement{Intent: "moving", Target: "vm-b", Found: true, VM: "vm-a", Epoch: 3})
	f.st.set("acme:placed", Placement{Intent: "creating", Found: true, VM: "vm-a", Epoch: 1})
	tests := []struct {
		name, db, key string
		err           error
		hint          time.Duration // ErrMoving's
	}{
		{"ok", "db", f.key, nil, 0},
		{"malformed key", "db", "nope", ErrUnauthorized, 0},
		{"unknown key id", "db", "tsdb_" + id + "_" + f.key[len("tsdb_")+17:], ErrUnauthorized, 0},
		{"wrong secret", "db", wrongSecret, ErrUnauthorized, 0},
		{"revoked", "db", revoked, ErrUnauthorized, 0},
		{"other tenant's key", "db", other, ErrForbidden, 0},
		{"no such database", "nothing", f.key, ErrNotFound, 0},
		{"moving", "moving", f.key, ErrMoving, 3 * time.Second},
		{"creating (no placement row yet)", "creating", f.key, ErrMoving, 3 * time.Second},
		{"suspended", "suspended", f.key, ErrSuspended, 0},
		{"dropping", "dropping", f.key, ErrNotFound, 0},
		{"dropped", "dropped", f.key, ErrNotFound, 0},
		{"placement without a registry row", "bare", f.key, nil, 0},
		{"flipped, registry not yet active: the claim is pending", "flipped", f.key, ErrMoving, 20 * time.Millisecond},
		{"placed, registry still creating: the claim is pending", "placed", f.key, ErrMoving, 20 * time.Millisecond},
		{"moving but not fenced yet: route to the source", "unfenced", f.key, nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.l.Resolve(context.Background(), Request{Tenant: "acme", Name: tc.db, APIKey: tc.key})
			if !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
				t.Fatalf("got %v, want %v", err, tc.err)
			}
			if errors.Is(err, ErrMoving) {
				var h front.RetryHint
				if !errors.As(err, &h) || h.RetryAfter() != tc.hint {
					t.Fatalf("moving without its retry hint: %v", err)
				}
			}
		})
	}
}

func TestStoreErrorsPassThrough(t *testing.T) {
	f := newFixture(t, Config{})
	boom := errors.New("authority down")
	f.st.fail = boom
	if _, err := f.resolve(t, "db", 0); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	f.st.fail = nil
	if _, err := f.resolve(t, "db", 0); err != nil {
		t.Fatalf("a failed load was cached: %v", err)
	}
}

func TestCacheIsOffUntilTheListenerConnects(t *testing.T) {
	f := newFixture(t, Config{})
	for range 3 {
		f.resolve(t, "db", 0)
	}
	if r, _ := f.st.counts(); r != 3 {
		t.Fatalf("untrusted: %d loads, want 3", r)
	}
	f.l.Reset()
	for range 3 {
		f.resolve(t, "db", 0)
	}
	if r, _ := f.st.counts(); r != 4 {
		t.Fatalf("trusted: %d loads, want 4", r)
	}
	f.l.Untrust()
	if s := f.l.Stats(); s.Entries != 0 || s.Trusted {
		t.Fatalf("untrust kept the cache: %+v", s)
	}
	f.resolve(t, "db", 0)
	if r, _ := f.st.counts(); r != 5 {
		t.Fatalf("after untrust: %d loads, want 5", r)
	}
}

func TestCacheTTL(t *testing.T) {
	f := newFixture(t, Config{CacheTTL: time.Minute})
	f.l.Reset()
	f.resolve(t, "db", 0)
	f.clk.add(time.Minute - time.Nanosecond) // off point: still fresh
	f.resolve(t, "db", 0)
	if r, _ := f.st.counts(); r != 1 {
		t.Fatalf("%d loads before the TTL, want 1", r)
	}
	f.clk.add(time.Nanosecond) // on point: expired
	f.resolve(t, "db", 0)
	if r, _ := f.st.counts(); r != 2 {
		t.Fatalf("%d loads at the TTL, want 2", r)
	}
}

func TestInvalidateSendsTheNextLoadToThePrimary(t *testing.T) {
	f := newFixture(t, Config{})
	f.l.Reset()
	f.resolve(t, "db", 0)
	f.st.set("acme:db", Placement{Intent: "active", Found: true, VM: "vm-b", Endpoint: "b", Epoch: 4})
	f.l.Invalidate("acme:db", 2) // late: the cache holds epoch 3
	if r, _ := f.resolve(t, "db", 0); r.Epoch != 3 {
		t.Fatalf("a late notification invalidated a newer row: %+v", r)
	}
	f.l.Invalidate("acme:db", 3) // ACTIVE → MOVING keeps the epoch: invalidates
	r, err := f.resolve(t, "db", 0)
	if err != nil || r.Epoch != 4 || r.VM != "vm-b" {
		t.Fatalf("%+v %v", r, err)
	}
	if rep, pri := f.st.counts(); rep != 1 || pri != 1 {
		t.Fatalf("loads replica %d primary %d, want 1 and 1", rep, pri)
	}
	f.resolve(t, "db", 0)
	if rep, pri := f.st.counts(); rep != 1 || pri != 1 {
		t.Fatalf("the primary's row was not cached: replica %d primary %d", rep, pri)
	}
	f.l.Invalidate("acme:unknown", 1) // uncached keys leave nothing behind
	if s := f.l.Stats(); s.Entries != 1 || s.Notifications != 3 {
		t.Fatalf("%+v", s)
	}
}

// A move: "moving" from the flip until the registry says active; its notification (InvalidateState) ends the
// "moving" answers at once, whatever the cached epoch.
func TestRegistryFlipEndsTheClaimWindow(t *testing.T) {
	f := newFixture(t, Config{})
	f.l.Reset()
	f.resolve(t, "db", 0)
	f.st.set("acme:db", Placement{Intent: "moving", Target: "vm-b", Found: true, VM: "vm-b", Endpoint: "b", Epoch: 4})
	f.l.Invalidate("acme:db", 4)
	if _, err := f.resolve(t, "db", 0); !errors.Is(err, ErrMoving) {
		t.Fatalf("claim pending: %v", err)
	}
	f.st.set("acme:db", Placement{Intent: "active", Found: true, VM: "vm-b", Endpoint: "b", Epoch: 4})
	if _, err := f.resolve(t, "db", 0); !errors.Is(err, ErrMoving) {
		t.Fatalf("the claim-pending row was not cached: %v", err)
	}
	f.l.InvalidateState("acme:db")
	if r, err := f.resolve(t, "db", 0); err != nil || r.VM != "vm-b" || r.Epoch != 4 {
		t.Fatalf("after the registry flip: %+v %v", r, err)
	}
	if rep, pri := f.st.counts(); rep != 1 || pri != 2 {
		t.Fatalf("loads replica %d primary %d, want 1 and 2 (each notified key reads the primary)", rep, pri)
	}
	f.l.InvalidateState("acme:unknown")
	if s := f.l.Stats(); s.Entries != 1 || s.Notifications != 3 {
		t.Fatalf("%+v", s)
	}
}

func TestMinEpoch(t *testing.T) {
	f := newFixture(t, Config{})
	f.l.Reset()
	f.resolve(t, "db", 0)
	if _, err := f.resolve(t, "db", 3); err != nil { // on point: the cached epoch is enough
		t.Fatal(err)
	}
	if rep, pri := f.st.counts(); rep != 1 || pri != 0 {
		t.Fatalf("min_epoch 3 with epoch 3 cached: replica %d primary %d", rep, pri)
	}
	f.st.set("acme:db", Placement{Intent: "active", Found: true, VM: "vm-b", Epoch: 4})
	r, err := f.resolve(t, "db", 4) // off point: above the cache
	if err != nil || r.Epoch != 4 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, pri := f.st.counts(); pri != 1 {
		t.Fatalf("min_epoch above the cache did not read the primary")
	}
	if r, _ := f.resolve(t, "db", 0); r.Epoch != 4 {
		t.Fatalf("the primary's newer row was not cached: %+v", r)
	}
}

func TestLoadsAreSingleFlight(t *testing.T) {
	f := newFixture(t, Config{})
	f.l.Reset()
	f.st.block = make(chan struct{})
	var wg sync.WaitGroup
	var ok atomic.Int64
	for range 64 {
		wg.Go(func() {
			if _, err := f.resolve(t, "db", 0); err == nil {
				ok.Add(1)
			}
		})
	}
	waitFor(t, func() bool { s := f.l.Stats(); return s.Misses+s.Coalesced == 64 })
	close(f.st.block)
	wg.Wait()
	if rep, _ := f.st.counts(); rep != 1 || ok.Load() != 64 {
		t.Fatalf("%d loads for 64 resolves (%d ok)", rep, ok.Load())
	}
	if f.st.keyRead != 1 {
		t.Fatalf("%d api key reads for 64 resolves", f.st.keyRead)
	}
}

func TestALoadOverlappingAChangeIsNotCached(t *testing.T) {
	for name, change := range map[string]func(l *Locator){
		"invalidate": func(l *Locator) { l.Invalidate("acme:db", 3) },
		"reset":      func(l *Locator) { l.Reset() },
		"untrust":    func(l *Locator) { l.Untrust(); l.Reset() },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, Config{})
			f.l.Reset()
			f.st.block = make(chan struct{})
			done := make(chan error, 1)
			go func() { _, err := f.resolve(t, "db", 0); done <- err }()
			waitFor(t, func() bool { return f.l.Stats().Misses == 1 })
			change(f.l)
			close(f.st.block)
			if err := <-done; err != nil { // the answer still serves its callers: stale is safe
				t.Fatal(err)
			}
			if s := f.l.Stats(); s.Entries != 0 {
				t.Fatalf("a load that overlapped a change was cached: %+v", s)
			}
		})
	}
}

func TestACallerGivingUpDoesNotCancelTheLoad(t *testing.T) {
	f := newFixture(t, Config{})
	f.l.Reset()
	f.st.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := f.l.Resolve(ctx, Request{Tenant: "acme", Name: "db", APIKey: f.key})
		first <- err
	}()
	waitFor(t, func() bool { return f.l.Stats().Misses == 1 })
	second := make(chan error, 1)
	go func() { _, err := f.resolve(t, "db", 0); second <- err }()
	waitFor(t, func() bool { return f.l.Stats().Coalesced == 1 })
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first: %v", err)
	}
	close(f.st.block)
	if err := <-second; err != nil {
		t.Fatalf("second caller failed with the first: %v", err)
	}
}

func TestLoadTimeoutBoundsALoadNobodyWaitsFor(t *testing.T) {
	f := newFixture(t, Config{LoadTimeout: 20 * time.Millisecond})
	f.st.block = make(chan struct{}) // never released
	_, err := f.resolve(t, "db", 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestAStorePanicReachesEveryWaiter(t *testing.T) {
	f := newFixture(t, Config{})
	f.st.panicky = true
	defer func() {
		if p := recover(); p == nil {
			t.Fatal("no panic")
		}
		f.st.panicky = false
		if _, err := f.resolve(t, "db", 0); err != nil {
			t.Fatalf("the panicked load stuck: %v", err)
		}
	}()
	f.resolve(t, "db", 0)
}

func TestCacheIsBounded(t *testing.T) {
	f := newFixture(t, Config{CacheMax: 3})
	f.l.Reset()
	for i := range 10 {
		f.st.set(fmt.Sprintf("acme:d%d", i), Placement{Intent: "active", Found: true, VM: "vm-a", Epoch: 1})
		if _, err := f.resolve(t, fmt.Sprintf("d%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	if s := f.l.Stats(); s.Entries != 3 {
		t.Fatalf("%d entries, bound 3", s.Entries)
	}
}

func TestRevocationTakesAuthTTL(t *testing.T) {
	f := newFixture(t, Config{AuthTTL: 10 * time.Second})
	f.resolve(t, "db", 0)
	id, _, _ := ParseAPIKey(f.key)
	f.st.mu.Lock()
	k := f.st.keys[id]
	k.Revoked = true
	f.st.keys[id] = k
	f.st.mu.Unlock()
	f.clk.add(10*time.Second - time.Nanosecond)
	if _, err := f.resolve(t, "db", 0); err != nil {
		t.Fatalf("inside the auth TTL: %v", err)
	}
	f.clk.add(time.Nanosecond)
	if _, err := f.resolve(t, "db", 0); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("after the auth TTL: %v", err)
	}
}

func TestAPIKeys(t *testing.T) {
	key, id, hash, err := NewAPIKey(bytes.NewReader(bytes.Repeat([]byte{9}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	gid, ghash, ok := ParseAPIKey(key)
	if !ok || gid != id || ghash != hash || !ValidKeyID(id) {
		t.Fatalf("round trip: %v %s %s", ok, gid, id)
	}
	if hash == sha256.Sum256([]byte(key)) {
		t.Fatal("the hash must cover the secret only")
	}
	for _, bad := range []string{"", "tsdb_", key[:len(key)-1], key + "A", "tsdb_" + id + "-" + key[22:], "TSDB" + key[4:],
		"tsdb_" + "ABCDEF0123456789" + key[21:], key[:len(key)-2] + "=="} {
		if _, _, ok := ParseAPIKey(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, _, _, err := NewAPIKey(bytes.NewReader(nil)); err == nil {
		t.Fatal("no randomness accepted")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
