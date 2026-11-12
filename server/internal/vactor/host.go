// Package vactor hosts virtual actors: one open database per key, created on first use from an object in
// storage and removed again on passivation.
//
// Two modes. Plain (Options.Store nil, the coldload benchmark): the Fetcher downloads object `key`, and
// passivation just drops the local copy. Durable (Options.Store set): activation reads the key's manifest
// and fetches its generation (internal/durable); warm writes are written back as new generations; passivation
// writes back before it deletes anything, and a failed write-back leaves the database warm. See writeback.go
// and maintain.go.
//
// Get(key) returns the key's open database. If it is not active, the first caller starts an activation: fetch the
// object to a local file, open it, run the first query, then optional warm-up queries. Callers that ask for the same
// key meanwhile wait for that one activation. A failed activation is forgotten, but the key keeps a short, jittered
// tombstone (Options.FailBackoff, doubling per failure in a row up to MaxFailBackoff): until it ends, Gets fail fast
// with the same cause instead of hitting storage again; the first Get after it tries again. Passivate closes the
// database and deletes the local copy; a Get that arrives while a key is passivating waits for it to finish, then
// activates afresh.
//
// Every activation has a deadline (Options.ActivationTimeout, grown by size/MinThroughput once the size is
// known) and goes through a fetch bulkhead (Options.MaxFetches, MaxFetchBytes, MaxFetchQueue): a full queue
// sheds with ErrBusy instead of waiting.
//
// The host reaches storage and SQLite only through two ports, Fetcher and Opener, so it can be tested with
// fakes (see vactortest). It owns its cache directory: NewHost empties it and Close removes it.
package vactor

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/retry"
)

const module = "vactor"

var (
	// ErrClosed is returned by Get after Close.
	ErrClosed = errors.New("vactor: host closed")
	// ErrNotFound is what a Fetcher returns (wrapped) when the object does not exist.
	ErrNotFound = errors.New("vactor: object not found")
	// ErrBusy: the host cannot take this activation now. Either the fetch queue is full (Options.MaxFetchQueue;
	// Get fails at once), or the activation did not finish within its deadline (Options.ActivationTimeout).
	// Nothing was cached; retry later. agentd answers it 503 unavailable.
	ErrBusy = errors.New("vactor: busy")
	// ErrInDoubt: a write-back sent nothing because maxPending publishes of ours are in doubt and may still
	// land (none is older than Options.GC.Grace yet). Safe to retry; the database stays warm.
	ErrInDoubt = errors.New("vactor: too many publishes in doubt")

	errShed              = fmt.Errorf("%w: the fetch queue is full", ErrBusy)    // shed: nothing attempted, no tombstone
	errActivationTimeout = fmt.Errorf("%w: activation deadline passed", ErrBusy) // the activation ctx's own cause
)

// Fetcher downloads the object key to the local file path.
//
//	post: err == nil ⇒ path holds the whole object and Fetched.Bytes is its size
//	post: err != nil ⇒ path does not exist (no partial file)
//	post: a missing object gives an error matching errors.Is(err, ErrNotFound)
type Fetcher interface {
	Fetch(ctx context.Context, key, path string) (Fetched, error)
}

// Sizer is an optional extension of a Fetcher: it says how large object key is before the fetch (one HEAD).
// With it, plain mode bounds fetches by bytes as well as by count (Options.MaxFetchBytes) and grows the
// activation deadline by size/MinThroughput, exactly as durable mode does with the manifest's size. Without
// it a plain fetch counts 0 bytes, and only MaxFetches bounds how many large transfers share the link.
//
//	post: a missing object gives an error matching errors.Is(err, ErrNotFound)
type Sizer interface {
	Size(ctx context.Context, key string) (int64, error)
}

// Fetched describes one download.
type Fetched struct {
	Bytes int64
	TTFB  time.Duration // request sent → response headers
	Total time.Duration // request sent → local file closed
}

// Opener opens the local database file at path. It must not create a missing file.
//
// incarnation tells one restore of the database from the next: in durable mode it is the seal of the manifest
// the file was activated under (durable.Manifest.Seal), which rises with every stamp, so a database restored
// after a crash never opens under the incarnation that wrote what the crash lost. 0 without a store, where a
// file is never rolled back. The opener hands it to whatever issues numbers that must not repeat.
type Opener interface {
	Open(path string, incarnation uint64) (DB, error)
}

// DB is an open database as the host needs it.
type DB interface {
	Query(ctx context.Context, sql string) error
	Close()
}

// DurableDB is what durable mode needs from an open database. actordb implements it over internal/actor.
type DurableDB interface {
	DB
	// Commits counts committed write transactions since open; it stays readable after Close.
	//
	//	post: monotonic; a write the caller saw acknowledged is already counted
	Commits() uint64
	// Snapshot writes a consistent copy to path (which does not exist).
	//
	//	post: err == nil ⇒ path holds at least the first `commits` commits
	Snapshot(ctx context.Context, path string) (commits uint64, err error)
}

// Defaults of the bounds NewHost applies (the capacity table reads them, docs/capacity.md).
const (
	DefaultWorkers       = 8       // Options.Workers
	DefaultMaxFetches    = 16      // Options.MaxFetches
	DefaultMaxFetchBytes = 1 << 30 // Options.MaxFetchBytes
	DefaultMaxFetchQueue = 256     // Options.MaxFetchQueue
)

// Options apply to every activation.
type Options struct {
	CacheDir string // local copies live here; emptied by NewHost, removed by Close

	// FirstQuery runs once the database is open; "ready" means it has answered. Default: count sqlite_master.
	FirstQuery string
	// WarmQueries run after FirstQuery, in order, before Get returns.
	WarmQueries []string
	// KeepLocal keeps the local file on passivation; the next activation then skips the fetch (local hit).
	KeepLocal bool
	// QueryTimeout bounds each first/warm query (within the activation's own deadline). Default 5 min.
	QueryTimeout time.Duration

	// ActivationTimeout bounds one activation as a whole: manifest read, the wait for a fetch slot, the fetch,
	// open, first and warm queries. Once the manifest gives the generation's size, the bound grows by
	// size/MinThroughput, so a large database is not cut off for being large. An activation that hits it fails
	// with ErrBusy. Default 30 s.
	ActivationTimeout time.Duration
	// MinThroughput (bytes/s) is the slowest fetch rate the activation deadline allows for. Default 32 MiB/s.
	MinThroughput int64
	// MaxFetches and MaxFetchBytes bound fetches in flight, by count and by bytes (the bytes are known in
	// durable mode, and in plain mode when the Fetcher is a Sizer; otherwise a plain Fetch counts 0 bytes).
	// One fetch larger than MaxFetchBytes runs alone. Defaults 16 and 1 GiB: the measured coldload knee.
	//
	// The byte bound is what keeps the bulkhead consistent with a store's per-transfer deadline (objstore:
	// OpTimeout + size/MinThroughput): the fetches in flight share the link, so each must still finish in
	// time when MaxFetchBytes moves at the link's rate. At ~260 MB/s, 1 GiB in flight takes ~4.1 s, inside
	// the 5 s OpTimeout alone; 16 × 128 MiB counted as 0 bytes did not
	// (server/docs/notes/coldload/fetch-bulkhead.md).
	MaxFetches    int
	MaxFetchBytes int64
	// MaxFetchQueue bounds activations waiting for a fetch slot. When it is full, Get fails at once with
	// ErrBusy instead of queueing. Default 256; negative: no queue.
	MaxFetchQueue int
	// FailBackoff is how long a key fails fast after a failed activation; it doubles with each failure in a
	// row up to MaxFailBackoff, jittered to [d/2, d]. Not after shedding (ErrBusy without a timeout), an open
	// breaker, a stale epoch or Close: those are not the key's failures. Defaults 100 ms and 5 s; a negative
	// FailBackoff turns tombstones off.
	FailBackoff    time.Duration
	MaxFailBackoff time.Duration

	// Store turns on durable mode (the Fetcher is then unused and may be nil). Not with KeepLocal: a kept
	// copy may be older than the manifest; use GenCacheBytes.
	Store durable.Store
	// GenCacheBytes turns on the local generation cache in durable mode: a clean passivation keeps the database
	// file as <CacheDir>/gen/<sha256>.db, and an activation whose manifest names that exact sha256 and size moves
	// it back instead of downloading (1 GET in all). The cache holds at most this many bytes, least recently
	// cached evicted first. 0: off.
	GenCacheBytes int64
	// CreateMissing: activating a key with no manifest starts an empty database (first write-back creates
	// the manifest). Otherwise such a Get fails with ErrNotFound.
	CreateMissing bool
	// Policy drives Tick / RunMaintenance: periodic write-back and eviction.
	Policy Policy
	// WriteBackTimeout bounds each write-back that has no caller context (PassivateAll, Close) and each job of
	// a maintenance tick. Default 5 min.
	WriteBackTimeout time.Duration
	// GC is owner-side retention: after each publish the host deletes the
	// generations it published more than GC.Keep publishes ago (default 2; negative: keep none but the
	// current). GC.Grace also bounds how long a publish whose outcome is unknown may still land: the host keeps it
	// pending that long (maxPending). It must not exceed the sweeper's (durable.Sweep, dbtool -grace); agentd
	// leaves it at the default (10 min, not configurable). Durable mode only.
	GC durable.GCPolicy
	// Workers bounds concurrent write-backs and passivations in Tick, PassivateAll and Close. Default 8.
	Workers int
	// Clock is the time for activation deadlines, failure backoff, idle and write-back ages, manifest stamps and
	// timings. Default det.System().
	Clock det.Clock
	// Rand jitters the failure backoff. Default det.SystemRand().
	Rand det.Rand

	// CheckActivation, if set, runs after the manifest is read and before anything is fetched; an error
	// fails the activation (fencing: owner.Table.CheckActivation). m is the zero manifest for a new key.
	CheckActivation func(key string, epoch uint64, m durable.Manifest) error
	// Guard, if set, wraps every write-back and passivation that Tick starts (fencing: owner.Table.Publish,
	// so a fence drains them). Direct calls (WriteBack, Passivate) are not guarded: the fence itself makes them.
	Guard func(ctx context.Context, key string, epoch uint64, do func(context.Context) error) error
}

// Activation says where one activation's time went.
type Activation struct {
	Bytes      int64
	LocalHit   bool          // the file was already on local disk; no fetch
	TTFB       time.Duration // fetch request sent → headers back
	Fetch      time.Duration // fetch request sent → local file closed
	Open       time.Duration
	FirstQuery time.Duration
	Warm       time.Duration
	Total      time.Duration // activation start → ready
}

// Result is what one Get returned.
type Result struct {
	DB DB
	// Act is set only for the caller whose Get started the activation; nil for callers that waited on it or
	// found the key already active.
	Act *Activation
	// Waited is true when this caller waited on an activation that another caller started.
	Waited bool
}

// Stats are running totals since NewHost.
type Stats struct {
	Activations int64 // finished, ok or not
	Failures    int64
	Fetches     int64
	LocalHits   int64
	// FetchShed counts activations refused because the fetch queue was full; TombstoneRefusals counts Gets
	// failed fast inside a key's failure backoff. Neither started anything.
	FetchShed         int64
	TombstoneRefusals int64

	WriteBacks        int64 // generations published
	WriteBackFailures int64
	BytesUploaded     int64
	// LastWriteBackOK is when a write-back last published (the gauge last_writeback_ok); zero if none yet.
	LastWriteBackOK time.Time

	// GenerationsDeleted counts generations deleted by owner-side retention;
	// RetentionFailures counts failed retention deletes (those objects are left to the orphan sweeper).
	GenerationsDeleted int64
	RetentionFailures  int64
	// PublishesInDoubt counts write-backs and stamps that sent nothing because maxPending publishes of the key
	// were in doubt (ErrInDoubt). Non-zero after an S3 brownout: those keys publish again within GC.Grace.
	PublishesInDoubt int64

	// Requests counts store requests by operation (durable.Op* names), retries included, when the Store
	// counts them (durable.RequestCounter); nil otherwise. A Store shared by several hosts counts them all.
	Requests map[string]int64
	// RequestCostUSD is Requests priced at durable.S3Standard list prices.
	RequestCostUSD float64
}

type entry struct {
	done  chan struct{} // closed when the activation finishes
	db    DB
	act   Activation
	err   error
	panic any // set if the activation panicked (a bug); re-raised in every caller

	gone chan struct{} // non-nil while passivating; closed when the local copy is gone

	durableState
}

// Host owns the active databases. Safe for concurrent use.
type Host struct {
	fetch  Fetcher
	size   Sizer // fetch's Sizer, if it is one (plain mode)
	open   Opener
	opts   Options
	ctx    context.Context // cancelled by Close: stops fetches and queries in flight
	cancel context.CancelFunc

	gate *fetchGate // fetch bulkhead
	gens *genCache  // local generation cache; nil when off

	mu      sync.Mutex
	entries map[string]*entry
	tombs   map[string]tomb // keys whose last activation failed: fail fast until tomb.until
	backoff retry.Policy    // the tombstones' backoff: FailBackoff doubling to MaxFailBackoff, equal jitter
	rng     *rand.Rand      // jitter for backoff; guarded by mu
	closed  bool

	activations, failures, fetches, localHits    atomic.Int64
	writeBacks, writeBackFailures, bytesUploaded atomic.Int64
	lastWriteBackOK                              atomic.Int64 // UnixNano by opts.Clock; 0 = never
	generationsDeleted, retentionFailures        atomic.Int64
	publishesInDoubt                             atomic.Int64 // ErrInDoubt answers
	fetchShed, tombRefusals                      atomic.Int64
	requests                                     durable.RequestCounter // opts.Store, if it counts; else nil
}

// NewHost creates a host with an empty cache directory.
func NewHost(f Fetcher, o Opener, opts Options) (*Host, error) {
	if opts.CacheDir == "" {
		return nil, errs.Wrap(module, nil, "cannot create host: no cache directory given")
	}
	switch {
	case f == nil && opts.Store == nil:
		return nil, errs.Wrap(module, nil, "cannot create host: neither a Fetcher nor a durable Store given")
	case opts.Store != nil && opts.KeepLocal:
		return nil, errs.Wrap(module, nil, "cannot create host: KeepLocal and a durable Store do not mix (a kept copy may be older than the manifest); use GenCacheBytes")
	}
	if err := opts.Policy.Validate(); err != nil {
		return nil, errs.Wrap(module, err, "cannot create host")
	}
	if opts.WriteBackTimeout <= 0 {
		opts.WriteBackTimeout = 5 * time.Minute
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	opts.GC = opts.GC.Normalize()
	opts.Clock, opts.Rand = det.Or(opts.Clock), det.RandOr(opts.Rand)
	if err := os.RemoveAll(opts.CacheDir); err != nil {
		return nil, errs.Wrap(module, err, "cannot clear cache directory %s", opts.CacheDir)
	}
	if err := os.MkdirAll(opts.CacheDir, 0o755); err != nil {
		return nil, errs.Wrap(module, err, "cannot create cache directory %s", opts.CacheDir)
	}
	if opts.FirstQuery == "" {
		opts.FirstQuery = "SELECT count(*) FROM sqlite_master"
	}
	if opts.QueryTimeout <= 0 {
		opts.QueryTimeout = 5 * time.Minute
	}
	if opts.ActivationTimeout <= 0 {
		opts.ActivationTimeout = 30 * time.Second
	}
	if opts.MinThroughput <= 0 {
		opts.MinThroughput = 32 << 20
	}
	if opts.MaxFetches <= 0 {
		opts.MaxFetches = DefaultMaxFetches
	}
	if opts.MaxFetchBytes <= 0 {
		opts.MaxFetchBytes = DefaultMaxFetchBytes
	}
	if opts.FailBackoff == 0 {
		opts.FailBackoff = 100 * time.Millisecond
	}
	if opts.MaxFailBackoff <= 0 {
		opts.MaxFailBackoff = 5 * time.Second
	}
	switch {
	case opts.MaxFetchQueue == 0:
		opts.MaxFetchQueue = DefaultMaxFetchQueue
	case opts.MaxFetchQueue < 0:
		opts.MaxFetchQueue = 0
	}
	size, _ := f.(Sizer)
	reqs, _ := opts.Store.(durable.RequestCounter)
	if assert.Enabled {
		if f != nil {
			f = checkedFetcher{f}
		}
		if opts.Store != nil {
			opts.Store = checkedStore{opts.Store}
		}
	}
	backoff := retry.Policy{Backoff: retry.Exponential, Base: max(opts.FailBackoff, 1), Max: opts.MaxFailBackoff, Jitter: retry.Equal}
	if err := backoff.Normalize(); err != nil {
		return nil, errs.Wrap(module, err, "cannot create host: invalid failure backoff")
	}
	var gens *genCache
	if opts.Store != nil && opts.GenCacheBytes > 0 {
		var err error
		if gens, err = newGenCache(filepath.Join(opts.CacheDir, "gen"), opts.GenCacheBytes); err != nil {
			return nil, errs.Wrap(module, err, "cannot create the generation cache in %s", opts.CacheDir)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Host{fetch: f, size: size, requests: reqs, gens: gens, open: o, opts: opts, ctx: ctx, cancel: cancel, entries: make(map[string]*entry),
		tombs: make(map[string]tomb), backoff: backoff, rng: rand.New(opts.Rand),
		gate: newFetchGate(opts.MaxFetches, opts.MaxFetchBytes, opts.MaxFetchQueue)}, nil
}

// LocalPath is where key's local copy lives: <CacheDir>/<escaped key>.db. The escaping is one-to-one and the
// name always ends in ".db", so no key's file is another key's side file (-wal, .snap) or the gen/ cache.
func (h *Host) LocalPath(key string) string {
	return filepath.Join(h.opts.CacheDir, url.PathEscape(key)+".db")
}

// Get is GetAt with epoch 0 (no fencing).
func (h *Host) Get(ctx context.Context, key string) (Result, error) { return h.GetAt(ctx, key, 0) }

// GetAt returns key's open database, activating it if needed. ctx bounds only this caller's wait: cancelling
// it does not stop an activation other callers may be waiting on.
//
// epoch is the caller's ownership epoch (internal/owner admits it; the host does not decide ownership). In
// durable mode write-backs publish at the highest epoch any caller of this activation passed, and an
// activation whose manifest holds a higher epoch fails with durable.ErrStale.
//
//	post: err == nil ⇒ res.DB != nil, and the key stays active until passivated
//	post: res.Act != nil ⇒ this caller started the activation, so res.Waited is false
//	post: a failed activation is not cached; until its tombstone ends, Gets for key fail fast with its cause
//	      (and a RetryAfter hint), and the first Get after that tries again
//	post: if the activation panicked, Get panics with the same value in every caller (a bug)
func (h *Host) GetAt(ctx context.Context, key string, epoch uint64) (Result, error) {
	for {
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return Result{}, errs.Wrap(module, ErrClosed, "cannot activate %s", key)
		}
		e, ok := h.entries[key]
		if ok && e.gone != nil { // passivating: wait until it is gone (or the passivation gave up), then start over
			gone := e.gone
			h.mu.Unlock()
			select {
			case <-gone:
				continue
			case <-ctx.Done():
				return Result{}, h.gaveUp(key, ctx)
			}
		}
		if !ok {
			if t, wait, ok := h.tombstoned(key); ok {
				h.mu.Unlock()
				h.tombRefusals.Add(1)
				return Result{}, errs.Wrap(module, &hinted{t.err, wait},
					"cannot activate %s: its last %d activation(s) failed; failing fast for %v, nothing started", key, t.n, wait)
			}
			if h.gate.shedding() { // fail fast before any storage call
				h.mu.Unlock()
				h.fetchShed.Add(1)
				return Result{}, errs.Wrap(module, errShed, "cannot activate %s: %d fetches are already queued; nothing started, retry later",
					key, h.opts.MaxFetchQueue)
			}
			e = &entry{done: make(chan struct{})}
			e.epoch.Store(epoch)
			e.touch(h.opts.Clock.Now())
			h.entries[key] = e
			h.mu.Unlock()
			go h.activate(key, e) // own goroutine: one caller's cancel must not fail the others
			select {
			case <-e.done:
				return e.result(&e.act, false)
			case <-ctx.Done():
				return Result{}, h.gaveUp(key, ctx)
			}
		}
		h.mu.Unlock()
		e.raiseEpoch(epoch)
		e.touch(h.opts.Clock.Now())
		select {
		case <-e.done:
			return e.result(nil, false)
		default:
		}
		select {
		case <-e.done:
			return e.result(nil, true)
		case <-ctx.Done():
			return Result{}, h.gaveUp(key, ctx)
		}
	}
}

func (e *entry) result(act *Activation, waited bool) (Result, error) {
	if e.panic != nil {
		panic(e.panic) // forward the activation goroutine's panic to the caller: panics do not cross goroutines
	}
	assert.That(e.err != nil || e.db != nil, "Get: nil DB without an error")
	assert.That(act == nil || !waited, "Get: both owner and waiter")
	return Result{DB: e.db, Act: act, Waited: waited}, e.err
}

func (h *Host) gaveUp(key string, ctx context.Context) error {
	return errs.Wrap(module, ctx.Err(), "stopped waiting for %s; its activation continues for other callers", key)
}

func (h *Host) activate(key string, e *entry) {
	defer close(e.done)
	defer func() {
		if p := recover(); p != nil {
			e.db, e.err, e.panic = nil, nil, fmt.Sprintf("vactor: activation of %s panicked: %v\n%s", key, p, debug.Stack())
			h.forget(key, e)
		}
	}()
	t0 := h.opts.Clock.Now()
	e.db, e.err = h.load(t0, key, e)
	e.act.Total = h.since(t0)
	h.activations.Add(1)
	// Invariant: an activation either fully succeeded (db open) or left nothing behind.
	assert.That((e.err == nil) == (e.db != nil), "activate(%s): err=%v but db=%v", key, e.err, e.db)
	if e.err != nil {
		h.failures.Add(1)
		if !h.opts.KeepLocal {
			RemoveDB(h.LocalPath(key)) // a fetched copy that failed to open must not leak disk
		}
		h.forget(key, e)
		return
	}
	h.mu.Lock()
	delete(h.tombs, key)
	h.mu.Unlock()
}

// forget removes a failed entry so a later Get retries, and leaves a tombstone if the failure was the key's.
func (h *Host) forget(key string, e *entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.entries[key] == e {
		delete(h.entries, key)
	}
	if e.err != nil && h.backsOff(e.err) {
		now := h.opts.Clock.Now()
		t := h.tombs[key]
		if t.stale(now, h.opts.MaxFailBackoff) {
			t.n = 0 // the last failure is long past: start the backoff over
		}
		t.n++
		t.until, t.err = now.Add(h.backoff.Delay(h.rng, t.n+1, 0)), e.err
		h.tombs[key] = t
	}
}

// load runs one activation from start under a ctx whose deadline is start + ActivationTimeout, grown by
// size/MinThroughput once the size is known (bodies are bounded by time as well as size). No step uses h.ctx alone:
// Close cancels them all through ctx. Hitting that deadline fails with ErrBusy; the context's cause, not the error
// chain, tells it apart from a query's own QueryTimeout.
func (h *Host) load(start time.Time, key string, e *entry) (_ DB, err error) {
	ctx, cancel := h.activationCtx(start, 0)
	defer func() {
		if err != nil && errors.Is(context.Cause(ctx), errActivationTimeout) {
			err = errs.Wrap(module, fmt.Errorf("%w: %w", errActivationTimeout, err),
				"cannot activate %s: no answer within its deadline (ActivationTimeout %v + size/MinThroughput); nothing cached, retry later",
				key, h.opts.ActivationTimeout)
		}
		cancel()
	}()
	act := &e.act
	path := h.LocalPath(key)
	if h.opts.Store != nil {
		m, err := h.readManifest(ctx, key, e)
		if err != nil {
			return nil, err
		}
		e.incarnation = m.Seal
		if m.Size > 0 {
			cancel()
			ctx, cancel = h.activationCtx(start, m.Size)
		}
		if m.Generation > 0 && h.gens != nil && h.gens.take(m.Digest(), path) {
			act.LocalHit, act.Bytes = true, m.Size
			h.localHits.Add(1)
		} else if err := h.fetchGeneration(ctx, key, e, m, path); err != nil {
			return nil, err
		}
	} else if size, ok := h.localCopy(path); ok {
		act.LocalHit, act.Bytes = true, size
		h.localHits.Add(1)
	} else {
		var n int64 // 0: unknown
		if h.size != nil {
			if n, err = h.size.Size(ctx, key); err != nil {
				return nil, errs.Wrap(module, err, "cannot activate %s: size lookup failed; nothing cached, safe to retry", key)
			}
			cancel()
			ctx, cancel = h.activationCtx(start, n)
		}
		if err := h.gatedFetch(ctx, key, n, e, func(ctx context.Context) (Fetched, error) {
			return h.fetch.Fetch(ctx, key, path)
		}); err != nil {
			return nil, errs.Wrap(module, err, "cannot activate %s: download failed; nothing cached, safe to retry", key)
		}
	}

	t := h.opts.Clock.Now()
	db, err := h.open.Open(path, e.incarnation)
	act.Open = h.since(t)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot activate %s: open failed; safe to retry", key)
	}
	if h.opts.Store != nil {
		ddb, ok := db.(DurableDB)
		if !ok {
			db.Close()
			return nil, errs.Wrap(module, nil, "cannot activate %s: the Opener's database (%T) cannot be written back; use a DurableDB opener", key, db)
		}
		e.pubCommits.Store(ddb.Commits())
		e.lastWB.Store(h.opts.Clock.Now().UnixNano())
	}

	t = h.opts.Clock.Now()
	err = h.query(ctx, db, h.opts.FirstQuery)
	act.FirstQuery = h.since(t)
	if err != nil {
		db.Close()
		return nil, errs.Wrap(module, err, "cannot activate %s: first query failed; safe to retry", key)
	}

	t = h.opts.Clock.Now()
	for _, q := range h.opts.WarmQueries {
		if err := h.query(ctx, db, q); err != nil {
			db.Close()
			return nil, errs.Wrap(module, err, "cannot activate %s: warm query %q failed; safe to retry", key, q)
		}
	}
	act.Warm = h.since(t)
	return db, nil
}

// localCopy reports the size of a kept local copy. Without KeepLocal there is none to reuse.
func (h *Host) localCopy(path string) (int64, bool) {
	if !h.opts.KeepLocal {
		return 0, false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return fi.Size(), true
}

// tomb is a key whose activation failed n times in a row; Gets fail fast with err until until.
type tomb struct {
	until time.Time
	n     int
	err   error
}

// stale reports whether t's backoff ended so long ago (more than the longest backoff) that a new failure
// starts over, and the reaper may drop it.
func (t tomb) stale(now time.Time, longest time.Duration) bool {
	return now.After(t.until.Add(longest))
}

// backsOff reports whether a failed activation's error is the key's own failure, which earns a tombstone.
// pre: h.mu held.
func (h *Host) backsOff(err error) bool {
	switch {
	case h.opts.FailBackoff < 0, h.closed:
		return false
	case errors.Is(err, errShed), errors.Is(err, breaker.ErrOpen), errors.Is(err, durable.ErrStale):
		return false // not attempted, or not this key's failure
	}
	return true
}

// tombstoned returns key's tombstone and the wait left if key is in its backoff. pre: h.mu held.
func (h *Host) tombstoned(key string) (tomb, time.Duration, bool) {
	t, ok := h.tombs[key]
	if !ok {
		return t, 0, false
	}
	wait := t.until.Sub(h.opts.Clock.Now())
	return t, wait, wait > 0
}

// pruneTombs drops tombstones whose backoff ended long enough ago that a new failure starts over (every
// map that grows has a reaper). Tick calls it.
func (h *Host) pruneTombs(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, t := range h.tombs {
		if t.stale(now, h.opts.MaxFailBackoff) {
			delete(h.tombs, k)
		}
	}
}

// hinted carries a retry hint (front.RetryHint) on err without changing its message or chain.
type hinted struct {
	err   error
	after time.Duration
}

func (e *hinted) Error() string             { return e.err.Error() }
func (e *hinted) Unwrap() error             { return e.err }
func (e *hinted) RetryAfter() time.Duration { return e.after }

func (h *Host) since(t time.Time) time.Duration { return det.Since(h.opts.Clock, t) }

// activationCtx is the activation's ctx for a generation of size bytes (0: unknown), from h.ctx.
func (h *Host) activationCtx(start time.Time, size int64) (context.Context, context.CancelFunc) {
	return h.opts.Clock.WithDeadlineCause(h.ctx, start.Add(h.opts.ActivationTimeout+transferTime(size, h.opts.MinThroughput)), errActivationTimeout)
}

// gatedFetch runs fetch through the fetch bulkhead and records it in e.act.
func (h *Host) gatedFetch(ctx context.Context, key string, size int64, e *entry, fetch func(context.Context) (Fetched, error)) error {
	release, err := h.gate.acquire(ctx, size)
	if err != nil {
		if errors.Is(err, errShed) {
			h.fetchShed.Add(1)
		}
		return errs.Wrap(module, err, "no fetch slot for %s (%d bytes); retry later", key, size)
	}
	defer release()
	h.fetches.Add(1)
	f, err := fetch(ctx)
	e.act.Bytes, e.act.TTFB, e.act.Fetch = f.Bytes, f.TTFB, f.Total
	return err
}

// transferTime is how long size bytes take at the minimum throughput.
func transferTime(size, bytesPerSec int64) time.Duration {
	return time.Duration(float64(size) / float64(bytesPerSec) * float64(time.Second))
}

func (h *Host) query(ctx context.Context, db DB, q string) error {
	ctx, cancel := det.WithTimeout(ctx, h.opts.Clock, h.opts.QueryTimeout)
	defer cancel()
	return db.Query(ctx, q)
}

// SetFetchLimit changes Options.MaxFetches at run time (an operator's control).
// Fetches already running finish; the next ones respect n: none starts while n or more run. Raising it lets
// queued activations start at once. The byte bound and the queue bound are unchanged. It returns the previous
// limit.
//
//	pre: n >= 1 (the caller validates it)
func (h *Host) SetFetchLimit(n int) (prev int) { return h.gate.setMaxN(n) }

// FetchLimit is the current limit on fetches in flight (Options.MaxFetches, or the last SetFetchLimit).
func (h *Host) FetchLimit() int { return h.gate.limit() }

// Active counts keys that are active or activating.
func (h *Host) Active() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.entries)
}

// OpenDBs returns the databases that are open now (activation finished), for a scrape-time scan (O(active)).
func (h *Host) OpenDBs() []DB {
	h.mu.Lock()
	defer h.mu.Unlock()
	dbs := make([]DB, 0, len(h.entries))
	for _, e := range h.entries {
		select {
		case <-e.done:
			if e.db != nil {
				dbs = append(dbs, e.db)
			}
		default:
		}
	}
	return dbs
}

func (h *Host) Stats() Stats {
	var reqs map[string]int64
	if h.requests != nil {
		reqs = h.requests.RequestCounts()
	}
	return Stats{
		Requests:          reqs,
		RequestCostUSD:    durable.RequestCost(reqs, durable.S3Standard),
		Activations:       h.activations.Load(),
		Failures:          h.failures.Load(),
		Fetches:           h.fetches.Load(),
		LocalHits:         h.localHits.Load(),
		FetchShed:         h.fetchShed.Load(),
		TombstoneRefusals: h.tombRefusals.Load(),
		WriteBacks:        h.writeBacks.Load(),
		WriteBackFailures: h.writeBackFailures.Load(),
		BytesUploaded:     h.bytesUploaded.Load(),
		LastWriteBackOK:   unixNano(h.lastWriteBackOK.Load()),

		GenerationsDeleted: h.generationsDeleted.Load(),
		RetentionFailures:  h.retentionFailures.Load(),
		PublishesInDoubt:   h.publishesInDoubt.Load(),
	}
}

// unixNano is the time n UnixNano stands for; 0 is the zero time.
func unixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// RemoveDB deletes a SQLite database file and its side files (-wal, -shm, -journal). Missing files are fine.
func RemoveDB(path string) {
	for _, suf := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(path + suf)
	}
}

// checkedFetcher asserts the Fetcher postconditions on every call (assertion builds only). A broken adapter
// would otherwise show up much later as a corrupt or empty database.
type checkedFetcher struct{ Fetcher }

func (c checkedFetcher) Fetch(ctx context.Context, key, path string) (Fetched, error) {
	f, err := c.Fetcher.Fetch(ctx, key, path)
	fi, serr := os.Stat(path)
	if err == nil {
		assert.That(serr == nil && fi.Size() == f.Bytes, "Fetch(%s) ok but file %v size != Bytes %d", key, serr, f.Bytes)
		assert.That(f.TTFB >= 0 && f.TTFB <= f.Total, "Fetch(%s): TTFB %v outside [0, Total %v]", key, f.TTFB, f.Total)
	} else {
		assert.That(errors.Is(serr, os.ErrNotExist), "Fetch(%s) failed (%v) but left %s", key, err, path)
	}
	_, perr := os.Stat(path + ".part")
	assert.That(errors.Is(perr, os.ErrNotExist), "Fetch(%s) left %s.part", key, path)
	return f, err
}
