// Package vactor hosts virtual actors: one open database per key, created on first use from an object in
// storage and removed again on passivation.
//
// Two modes. Plain (Options.Store nil, the coldload benchmark): the Fetcher downloads object `key`, and
// passivation just drops the local copy. Durable (Options.Store set): activation reads the key's manifest
// and fetches its generation (internal/durable); warm writes are written back as new generations; passivation
// writes back before it deletes anything, and a failed write-back leaves the database warm. See writeback.go
// and maintain.go.
//
// Get(key) returns the key's open database. If it is not active, the first caller starts an activation:
// fetch the object to a local file, open it, run the first query, then optional warm-up queries. Callers
// that ask for the same key meanwhile wait for that one activation. A failed activation is forgotten, so a
// later Get tries again. Passivate closes the database and deletes the local copy; a Get that arrives while a
// key is passivating waits for it to finish, then activates afresh.
//
// The host reaches storage and SQLite only through two ports, Fetcher and Opener, so it can be tested with
// fakes (see vactortest). It owns its cache directory: NewHost empties it and Close removes it.
package vactor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/errs"
)

const module = "vactor"

var (
	// ErrClosed is returned by Get after Close.
	ErrClosed = errors.New("vactor: host closed")
	// ErrNotFound is what a Fetcher returns (wrapped) when the object does not exist.
	ErrNotFound = errors.New("vactor: object not found")
)

// Fetcher downloads the object key to the local file path.
//
//	post: err == nil ⇒ path holds the whole object and Fetched.Bytes is its size
//	post: err != nil ⇒ path does not exist (no partial file)
//	post: a missing object gives an error matching errors.Is(err, ErrNotFound)
type Fetcher interface {
	Fetch(ctx context.Context, key, path string) (Fetched, error)
}

// Fetched describes one download.
type Fetched struct {
	Bytes int64
	TTFB  time.Duration // request sent → response headers
	Total time.Duration // request sent → local file closed
}

// Opener opens the local database file at path. It must not create a missing file.
type Opener interface {
	Open(path string) (DB, error)
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

// Options apply to every activation.
type Options struct {
	CacheDir string // local copies live here; emptied by NewHost, removed by Close

	// FirstQuery runs once the database is open; "ready" means it has answered. Default: count sqlite_master.
	FirstQuery string
	// WarmQueries run after FirstQuery, in order, before Get returns.
	WarmQueries []string
	// KeepLocal keeps the local file on passivation; the next activation then skips the fetch (local hit).
	KeepLocal bool
	// QueryTimeout bounds each first/warm query. Default 5 min.
	QueryTimeout time.Duration

	// Store turns on durable mode (the Fetcher is then unused and may be nil). Not with KeepLocal.
	Store durable.Store
	// CreateMissing: activating a key with no manifest starts an empty database (first write-back creates
	// the manifest). Otherwise such a Get fails with ErrNotFound.
	CreateMissing bool
	// Policy drives Tick / RunMaintenance: periodic write-back and eviction.
	Policy Policy
	// WriteBackTimeout bounds each write-back that has no caller context: PassivateAll, Close. Default 5 min.
	WriteBackTimeout time.Duration
	// Workers bounds concurrent write-backs and passivations in Tick, PassivateAll and Close. Default 8.
	Workers int
	// Now is the clock (tests). Default time.Now.
	Now func() time.Time
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

	WriteBacks        int64 // generations published
	WriteBackFailures int64
	BytesUploaded     int64
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
	open   Opener
	opts   Options
	ctx    context.Context // cancelled by Close: stops fetches and queries in flight
	cancel context.CancelFunc

	mu      sync.Mutex
	entries map[string]*entry
	closed  bool

	activations, failures, fetches, localHits    atomic.Int64
	writeBacks, writeBackFailures, bytesUploaded atomic.Int64
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
		return nil, errs.Wrap(module, nil, "cannot create host: KeepLocal and a durable Store do not mix (a kept copy may be older than the manifest)")
	}
	if err := opts.Policy.Validate(); err != nil {
		return nil, errs.Wrap(module, err, "cannot create host")
	}
	if opts.WriteBackTimeout <= 0 {
		opts.WriteBackTimeout = 5 * time.Minute
	}
	if opts.Workers <= 0 {
		opts.Workers = 8
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
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
	if assert.Enabled {
		if f != nil {
			f = checkedFetcher{f}
		}
		if opts.Store != nil {
			opts.Store = checkedStore{opts.Store}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Host{fetch: f, open: o, opts: opts, ctx: ctx, cancel: cancel, entries: make(map[string]*entry)}, nil
}

var keyToFile = strings.NewReplacer("/", "__", "\\", "__")

// LocalPath is where key's local copy lives.
func (h *Host) LocalPath(key string) string {
	return filepath.Join(h.opts.CacheDir, keyToFile.Replace(key))
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
//	post: a failed activation is not cached: the next Get tries again
//	post: if the activation panicked, Get panics with the same value in every caller (a bug)
func (h *Host) GetAt(ctx context.Context, key string, epoch uint64) (Result, error) {
	for {
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return Result{}, errs.Wrap(module, ErrClosed, "cannot activate %s", key)
		}
		e, ok := h.entries[key]
		if ok && e.gone != nil { // passivating: wait until it is gone, then start over
			h.mu.Unlock()
			select {
			case <-e.gone:
				continue
			case <-ctx.Done():
				return Result{}, h.gaveUp(key, ctx)
			}
		}
		if !ok {
			e = &entry{done: make(chan struct{})}
			e.epoch.Store(epoch)
			e.touch(h.opts.Now())
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
		e.touch(h.opts.Now())
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
		panic(e.panic) // forward the activation goroutine's panic to the caller (Cox-Buday ch. 4)
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
	t0 := time.Now()
	e.db, e.err = h.load(key, e)
	e.act.Total = time.Since(t0)
	h.activations.Add(1)
	// Invariant: an activation either fully succeeded (db open) or left nothing behind.
	assert.That((e.err == nil) == (e.db != nil), "activate(%s): err=%v but db=%v", key, e.err, e.db)
	if e.err != nil {
		h.failures.Add(1)
		if !h.opts.KeepLocal {
			RemoveDB(h.LocalPath(key)) // a fetched copy that failed to open must not leak disk
		}
		h.forget(key, e)
	}
}

// forget removes a failed entry so the next Get retries.
func (h *Host) forget(key string, e *entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.entries[key] == e {
		delete(h.entries, key)
	}
}

func (h *Host) load(key string, e *entry) (DB, error) {
	act := &e.act
	path := h.LocalPath(key)
	if h.opts.Store != nil {
		if err := h.fetchDurable(key, e, path); err != nil {
			return nil, err
		}
	} else if size, ok := h.localCopy(path); ok {
		act.LocalHit, act.Bytes = true, size
		h.localHits.Add(1)
	} else {
		h.fetches.Add(1)
		f, err := h.fetch.Fetch(h.ctx, key, path)
		act.Bytes, act.TTFB, act.Fetch = f.Bytes, f.TTFB, f.Total
		if err != nil {
			return nil, errs.Wrap(module, err, "cannot activate %s: download failed; nothing cached, safe to retry", key)
		}
	}

	t := time.Now()
	db, err := h.open.Open(path)
	act.Open = time.Since(t)
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
		e.lastWB.Store(h.opts.Now().UnixNano())
	}

	t = time.Now()
	err = h.query(db, h.opts.FirstQuery)
	act.FirstQuery = time.Since(t)
	if err != nil {
		db.Close()
		return nil, errs.Wrap(module, err, "cannot activate %s: first query failed; safe to retry", key)
	}

	t = time.Now()
	for _, q := range h.opts.WarmQueries {
		if err := h.query(db, q); err != nil {
			db.Close()
			return nil, errs.Wrap(module, err, "cannot activate %s: warm query %q failed; safe to retry", key, q)
		}
	}
	act.Warm = time.Since(t)
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

func (h *Host) query(db DB, q string) error {
	ctx, cancel := context.WithTimeout(h.ctx, h.opts.QueryTimeout)
	defer cancel()
	return db.Query(ctx, q)
}

// Active counts keys that are active or activating.
func (h *Host) Active() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.entries)
}

func (h *Host) Stats() Stats {
	return Stats{
		Activations:       h.activations.Load(),
		Failures:          h.failures.Load(),
		Fetches:           h.fetches.Load(),
		LocalHits:         h.localHits.Load(),
		WriteBacks:        h.writeBacks.Load(),
		WriteBackFailures: h.writeBackFailures.Load(),
		BytesUploaded:     h.bytesUploaded.Load(),
	}
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
