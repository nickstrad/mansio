package locator

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/control"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/panicx"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// Config tunes a Locator. Zero fields take the defaults.
type Config struct {
	TicketTTL   time.Duration // a ticket's lifetime; default 60 s (option 4 §3.1)
	CacheTTL    time.Duration // longest a placement stays cached; default 5 min (notifications invalidate sooner)
	CacheMax    int           // placements cached; default 100 000 (Release It B6: bounded)
	AuthTTL     time.Duration // an API key row is re-read after this; revocation takes AuthTTL + TicketTTL; default 10 s
	AuthMax     int           // API keys cached; default 10 000
	LoadTimeout time.Duration // one store read (placement or key); default 2.5 s
	MovingRetry time.Duration // the retry hint while a database is moving or being created; default 1 s
	// ClaimRetry is the retry hint while the placement already names the new owner but its Claim has not landed
	// (the registry still says creating/moving): a few milliseconds of work left. Default 50 ms.
	ClaimRetry time.Duration
	Now        func() time.Time
}

func (c *Config) normalize() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.TicketTTL, time.Minute)
	def(&c.CacheTTL, 5*time.Minute)
	def(&c.AuthTTL, 10*time.Second)
	def(&c.LoadTimeout, 2500*time.Millisecond)
	def(&c.MovingRetry, time.Second)
	def(&c.ClaimRetry, 50*time.Millisecond)
	if c.CacheMax <= 0 {
		c.CacheMax = 100_000
	}
	if c.AuthMax <= 0 {
		c.AuthMax = 10_000
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Request is one resolve.
type Request struct {
	Tenant string
	Name   string // the database's name inside the tenant; the key is Tenant ":" Name
	APIKey string
	// MinEpoch > 0 says the caller saw this epoch refused (last seen + 1): a cached row below it is not used
	// and the primary is read.
	MinEpoch uint64
}

// Route is a resolve's answer: where the database is and a ticket for it.
type Route struct {
	DB       string
	Tenant   string
	VM       string
	Endpoint string
	Epoch    uint64
	Gen      uint64
	Ticket   string
	Perms    string
	// Expires is the ticket's exp; refresh ahead of it (option 4 §2: at ~80 % of the TTL, jittered).
	Expires time.Time
	TTL     time.Duration
}

// Stats are the locator's counters since start.
type Stats struct {
	Hits, Misses, Coalesced int64 // placement reads: from the cache, started a load, joined a load
	Notifications, Resets   int64 // Invalidate calls; Reset calls (listener connects)
	Entries                 int   // placements cached now (tombstones included)
	Trusted                 bool  // the cache is in use (a listener is connected)
}

// Locator resolves databases to routes with tickets. Safe for concurrent use.
//
//	Invariant: len(routes) <= CacheMax; len(keys) <= AuthMax; !trusted ⇒ len(routes) == 0
type Locator struct {
	store  Store
	signer Signer
	cfg    Config

	mu       sync.Mutex
	trusted  bool
	routes   map[string]*entry
	keys     map[string]keyEntry
	loads    map[loadKey]*flight[Placement]
	keyLoads map[string]*flight[StoredKey]
	stats    Stats
}

type entry struct {
	pl    Placement
	at    time.Time
	dirty bool // invalidated: a tombstone whose next load reads the primary (a replica may lag the notification)
}

type keyEntry struct {
	k  StoredKey
	at time.Time
}

type loadKey struct {
	key     string
	primary bool
}

// New returns a locator over store and signer. Its cache is off until the first Reset (the listener's
// connect): without notifications a cached row could be stale for CacheTTL.
func New(store Store, signer Signer, cfg Config) *Locator {
	cfg.normalize()
	return &Locator{store: store, signer: signer, cfg: cfg, routes: map[string]*entry{}, keys: map[string]keyEntry{},
		loads: map[loadKey]*flight[Placement]{}, keyLoads: map[string]*flight[StoredKey]{}}
}

// Resolve authenticates r.APIKey, reads the database's placement and mints a ticket for its owner.
//
//	post: err == nil ⇒ the ticket names r.Tenant ":" r.Name, the row's VM, epoch and generation, and the
//	      key's principal and permissions, and expires TicketTTL from now
//	post: errors match ErrUnauthorized, ErrForbidden, ErrNotFound, ErrMoving (with a RetryAfter hint),
//	      ErrSuspended, the ctx's error, or the store's own error
func (l *Locator) Resolve(ctx context.Context, r Request) (Route, error) {
	key := r.Tenant + ":" + r.Name
	fail := func(err error) (Route, error) {
		return Route{}, errs.Wrap(module, err, "cannot resolve %q", key)
	}
	k, err := l.authenticate(ctx, r.APIKey)
	if err != nil {
		return fail(err)
	}
	if k.Tenant != r.Tenant {
		return fail(ErrForbidden)
	}
	pl, err := l.locate(ctx, key, r.MinEpoch)
	if err != nil {
		return fail(err)
	}
	switch {
	case pl.Intent == control.StateDropping || pl.Intent == control.StateDropped || !pl.Found && pl.Intent == "":
		return fail(ErrNotFound)
	case pl.Intent == control.StateSuspended:
		return fail(ErrSuspended)
	case !pl.Found || pl.Moving:
		return fail(moving{l.cfg.MovingRetry})
	case claimPending(pl):
		return fail(moving{l.cfg.ClaimRetry})
	}
	now := l.cfg.Now()
	c := ticket.Claims{TID: r.Tenant, DBID: key, VM: pl.VM, Epoch: pl.Epoch, Gen: pl.Gen, Sub: k.Principal, Perms: k.Perms,
		IAT: now.Unix(), EXP: now.Add(l.cfg.TicketTTL).Unix()}
	tok, err := l.signer.Mint(c)
	if err != nil {
		return fail(fmt.Errorf("mint: %w", err))
	}
	exp := time.Unix(c.EXP, 0)
	return Route{DB: key, Tenant: r.Tenant, VM: pl.VM, Endpoint: pl.Endpoint, Epoch: pl.Epoch, Gen: pl.Gen, Ticket: tok,
		Perms: k.Perms.String(), Expires: exp, TTL: exp.Sub(now)}, nil
}

// claimPending reports that pl's placement is ACTIVE on an owner whose Claim the registry has not confirmed: a
// create's first owner, or a move's destination, before the registry flips to active. That owner answers
// wrong_owner at the current epoch until its Claim lands, so a client sent there would learn nothing; it is told
// "moving" instead, and the registry's flip (StateChannel) invalidates the cache when the Claim is done. A move
// whose placement still names the source is routed there (the source serves until it is fenced).
//
//	pre: pl.Found && !pl.Moving
func claimPending(pl Placement) bool {
	return pl.Intent == control.StateCreating || pl.Intent == control.StateMoving && pl.VM == pl.Target
}

// authenticate checks an API key: its id names a row, the secret's hash matches in constant time, and it is
// not revoked. Rows are cached for AuthTTL; unknown ids are not (a flood of garbage keys is the edge's to
// rate-limit, CP-7).
func (l *Locator) authenticate(ctx context.Context, raw string) (StoredKey, error) {
	id, hash, ok := ParseAPIKey(raw)
	if !ok {
		return StoredKey{}, ErrUnauthorized
	}
	l.mu.Lock()
	now := l.cfg.Now()
	var k StoredKey
	var err error
	if e, ok := l.keys[id]; ok && now.Sub(e.at) < l.cfg.AuthTTL {
		l.mu.Unlock()
		k = e.k
	} else {
		f := l.keyLoads[id]
		if f == nil {
			f = start(l, func(ctx context.Context) (StoredKey, error) { return l.store.APIKey(ctx, id) }, func(f *flight[StoredKey]) {
				delete(l.keyLoads, id)
				if f.ok() {
					evictOne(l.keys, id, l.cfg.AuthMax)
					l.keys[id] = keyEntry{f.val, l.cfg.Now()}
				}
			})
			l.keyLoads[id] = f
		}
		l.mu.Unlock()
		k, err = await(ctx, f)
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return StoredKey{}, ErrUnauthorized
	case err != nil:
		return StoredKey{}, err
	case subtle.ConstantTimeCompare(k.Hash[:], hash[:]) != 1 || k.Revoked:
		return StoredKey{}, ErrUnauthorized
	}
	return k, nil
}

// locate returns key's placement: from the cache when it is trusted, fresh, clean and at least minEpoch;
// otherwise from a load shared with every concurrent caller (the primary for minEpoch > 0 or a tombstone).
func (l *Locator) locate(ctx context.Context, key string, minEpoch uint64) (Placement, error) {
	l.mu.Lock()
	now := l.cfg.Now()
	e := l.routes[key]
	if e != nil && now.Sub(e.at) >= l.cfg.CacheTTL {
		e = nil
	}
	if e != nil && !e.dirty && e.pl.Epoch >= minEpoch {
		l.stats.Hits++
		l.mu.Unlock()
		return e.pl, nil
	}
	lk := loadKey{key, minEpoch > 0 || e != nil && e.dirty}
	f := l.loads[lk]
	if f != nil {
		l.stats.Coalesced++
	} else {
		l.stats.Misses++
		f = start(l, func(ctx context.Context) (Placement, error) { return l.store.Locate(ctx, key, lk.primary) }, func(f *flight[Placement]) {
			delete(l.loads, lk)
			l.keep(key, f)
		})
		l.loads[lk] = f
	}
	l.mu.Unlock()
	return await(ctx, f)
}

// keep caches a finished load's row, unless the load is stale (a notification or a listener change came while
// it ran), failed, found nothing, or a newer row is already cached.
//
//	pre: l.mu held
func (l *Locator) keep(key string, f *flight[Placement]) {
	if !f.ok() || f.stale || !l.trusted {
		return
	}
	if !f.val.Found {
		delete(l.routes, key)
		return
	}
	if cur := l.routes[key]; cur != nil && !cur.dirty && cur.pl.Epoch > f.val.Epoch {
		return
	}
	evictOne(l.routes, key, l.cfg.CacheMax)
	l.routes[key] = &entry{pl: f.val, at: l.cfg.Now()}
	assert.That(len(l.routes) <= l.cfg.CacheMax, "route cache over its bound: %d > %d", len(l.routes), l.cfg.CacheMax)
}

// Invalidate applies one placement notification: key's row changed and is now at epoch. A cached row is
// replaced by a tombstone (its next load reads the primary) unless it is already newer than epoch (a late
// notification), and loads in flight for key are not cached.
func (l *Locator) Invalidate(key string, epoch uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stats.Notifications++
	if e := l.routes[key]; e != nil && !e.dirty && e.pl.Epoch > epoch {
		return
	}
	l.invalidate(key)
}

// InvalidateState applies one registry notification: key's lifecycle state changed. Its cached row (whatever its
// epoch) becomes a tombstone and loads in flight are not cached: the registry flip does not change the epoch, so
// Invalidate's late-notification rule cannot apply.
func (l *Locator) InvalidateState(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stats.Notifications++
	l.invalidate(key)
}

// invalidate marks key's loads in flight stale and turns its cached row into a tombstone.
//
//	pre: l.mu held
func (l *Locator) invalidate(key string) {
	if f := l.loads[loadKey{key, false}]; f != nil {
		f.stale = true
	}
	if f := l.loads[loadKey{key, true}]; f != nil {
		f.stale = true
	}
	if e := l.routes[key]; e != nil {
		*e = entry{dirty: true, at: l.cfg.Now()}
	}
}

// Reset is the listener's (re)connect: LISTEN has succeeded, so every change from now on will be notified.
// Everything cached before is dropped (it may have missed notifications), loads in flight are not cached,
// and the cache is in use from now.
func (l *Locator) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stats.Resets++
	l.drop()
	l.trusted = true
}

// Untrust is the listener's loss: notifications may be missed until the next Reset, so the cache is dropped
// and bypassed (every resolve loads; loads stay single-flight).
func (l *Locator) Untrust() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.drop()
	l.trusted = false
}

// drop empties the route cache and marks loads in flight stale.
//
//	pre: l.mu held
func (l *Locator) drop() {
	clear(l.routes)
	for _, f := range l.loads {
		f.stale = true
	}
}

// Stats returns the counters.
func (l *Locator) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.stats
	s.Entries, s.Trusted = len(l.routes), l.trusted
	return s
}

// flight is one load shared by every caller that asked while it ran.
type flight[T any] struct {
	done  chan struct{}
	val   T
	err   error
	panic *panicx.Panic
	stale bool // guarded by Locator.mu
}

func (f *flight[T]) ok() bool { return f.err == nil && f.panic == nil }

// start runs load in its own goroutine under LoadTimeout, so a caller that gives up does not cancel it for
// the others and it cannot run longer than that. finish runs under l.mu before the waiters wake. A panic in
// load is re-raised in every waiter (panicx).
func start[T any](l *Locator, load func(ctx context.Context) (T, error), finish func(f *flight[T])) *flight[T] {
	f := &flight[T]{done: make(chan struct{})}
	go func() {
		defer func() {
			f.panic = panicx.From(recover())
			l.mu.Lock()
			finish(f)
			l.mu.Unlock()
			close(f.done)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), l.cfg.LoadTimeout)
		defer cancel()
		f.val, f.err = load(ctx)
	}()
	return f
}

// await waits for f or ctx.
func await[T any](ctx context.Context, f *flight[T]) (T, error) {
	select {
	case <-f.done:
		if f.panic != nil {
			panic(f.panic)
		}
		return f.val, f.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// evictOne makes room for key in m (bounded by max) by deleting an arbitrary other entry.
func evictOne[V any](m map[string]V, key string, max int) {
	if _, ok := m[key]; ok || len(m) < max {
		return
	}
	for k := range m {
		delete(m, k)
		return
	}
}
