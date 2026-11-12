package front

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/errs"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ActorExecutor runs a call as one statement on its database's actor. It is the integration adapter between
// the pipeline and internal/actor.
type ActorExecutor struct {
	Host Host
	// Grace is how long past the call's deadline to wait for the actor's answer before giving up with ErrNoReply.
	// The actor bounds queueing (Deadline) and execution (StmtTimeout), so an answer normally comes before the
	// deadline; a write in a group-commit batch can also wait for its batch-mates. Default 1 s.
	Grace time.Duration
}

// Exec implements Executor. ctx cancellation (client gone) does not abandon a submitted call early: the wait
// ends at Deadline + Grace, so a write's outcome is known whenever the actor answers in time.
func (e ActorExecutor) Exec(ctx context.Context, c Call) (Reply, error) {
	a, err := e.Host.Lookup(ctx, c.DB)
	if err != nil {
		return Reply{}, err
	}
	// The actor applies a write's RequestID at most once per database; reads ignore it.
	r := &actor.Request{SQL: c.SQL, Args: c.Args, Write: c.Kind == KindWrite, StmtTimeout: c.StmtTimeout,
		Deadline: c.Deadline, RequestID: c.RequestID, Fn: c.Fn, Check: c.Check}
	if c.Kind == KindRead && c.Fn == nil {
		r.MaxRows = c.MaxRows
	}
	if err := a.Submit(r); err != nil {
		return Reply{}, actorError(err, false)
	}
	grace := e.Grace
	if grace <= 0 {
		grace = time.Second
	}
	wctx, cancel := det.WithDeadline(context.WithoutCancel(ctx), a.Clock(), c.Deadline.Add(grace))
	defer cancel()
	res := r.Wait(wctx)
	if res.Err != nil {
		if wctx.Err() != nil && errors.Is(res.Err, wctx.Err()) {
			return Reply{}, ErrNoReply
		}
		if c.Fn != nil {
			return Reply{}, closureError(res.Err, res.TimedOut)
		}
		return Reply{}, actorError(res.Err, res.TimedOut)
	}
	if c.Fn != nil {
		return Reply{Body: res.Body, Value: res.Value, Duplicate: res.Duplicate}, nil
	}
	if c.Kind == KindWrite {
		return Reply{Count: res.Rows, Duplicate: res.Duplicate}, nil
	}
	return Reply{Columns: res.Columns, Rows: res.Values, Count: res.Rows}, nil
}

// actorError maps an actor or SQLite error to this package's sentinels; anything unexpected is wrapped as an
// internal error (its text goes to the log, never to the client).
func actorError(err error, timedOut bool) error {
	switch {
	case timedOut:
		return ErrStmtTimeout
	case errors.Is(err, actor.ErrMailboxFull):
		return ErrShed
	case errors.Is(err, actor.ErrClosed):
		return fmt.Errorf("%w: actor closed", ErrShed)
	case errors.Is(err, actor.ErrExpired):
		return ErrExpired
	case errors.Is(err, actor.ErrAborted):
		return ErrAborted
	case errors.Is(err, actor.ErrTooManyRows):
		return ErrTooManyRows
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff { // primary result code
		case sqlite3.SQLITE_ERROR, sqlite3.SQLITE_CONSTRAINT, sqlite3.SQLITE_MISMATCH, sqlite3.SQLITE_RANGE,
			sqlite3.SQLITE_TOOBIG, sqlite3.SQLITE_READONLY:
			return fmt.Errorf("%w: %w", ErrStatement, err)
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return fmt.Errorf("%w: %w", ErrShed, err)
		}
	}
	return errs.Wrap("front", err, "statement failed unexpectedly; a write may or may not have committed")
}

// closureError maps the actor's own outcomes of a closure call to this package's sentinels and leaves the
// closure's errors (and the driver's, which only it can interpret) unwrapped for the caller.
func closureError(err error, timedOut bool) error {
	switch {
	case timedOut:
		return fmt.Errorf("%w: %w", ErrStmtTimeout, err)
	case errors.Is(err, actor.ErrMailboxFull), errors.Is(err, actor.ErrClosed), errors.Is(err, actor.ErrExpired),
		errors.Is(err, actor.ErrAborted):
		return actorError(err, false)
	}
	return err
}

// StaticHost serves a fixed set of started actors. Safe for concurrent Lookup; Close closes every actor.
type StaticHost struct {
	mu      sync.RWMutex
	actors  map[string]*actor.Actor
	pending map[string]bool // expected (Expect) but not served yet
	npend   atomic.Int64    // len(pending): once 0, Pending takes no lock (the request path asks it)
	closed  bool
}

// NewStaticHost returns a host serving actors (db id → started actor; may be nil). It takes ownership of them.
func NewStaticHost(actors map[string]*actor.Actor) *StaticHost {
	if actors == nil {
		actors = map[string]*actor.Actor{}
	}
	return &StaticHost{actors: actors, pending: map[string]bool{}}
}

// Expect marks dbs as served here but not open yet (startup opens them while the agent already serves): until
// Add serves one, Lookup and Pending refuse it as unavailable, not unknown.
func (h *StaticHost) Expect(dbs ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, db := range dbs {
		if h.actors[db] == nil {
			h.pending[db] = true
		}
	}
	h.npend.Store(int64(len(h.pending)))
}

// Pending returns an error wrapping ErrUnavailable while db is expected but not open yet; nil otherwise.
func (h *StaticHost) Pending(db string) error {
	if h.npend.Load() == 0 {
		return nil
	}
	h.mu.RLock()
	p := h.pending[db]
	h.mu.RUnlock()
	if p {
		return fmt.Errorf("%w: database %s is not open yet (agent starting)", ErrUnavailable, db)
	}
	return nil
}

// PendingCount is how many expected databases are not open yet.
func (h *StaticHost) PendingCount() int { return int(h.npend.Load()) }

// Add serves a started actor for db from now on (startup opens databases while the agent already serves). It
// reports false, taking nothing, once the host is closed: the caller closes a.
//
//	pre: db is not served yet
func (h *StaticHost) Add(db string, a *actor.Actor) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	assert.That(h.actors[db] == nil, "StaticHost.Add(%s): already served", db)
	h.actors[db] = a
	delete(h.pending, db)
	h.npend.Store(int64(len(h.pending)))
	return true
}

// Lookup implements Host.
func (h *StaticHost) Lookup(_ context.Context, db string) (*actor.Actor, error) {
	h.mu.RLock()
	a, ok := h.actors[db]
	closed := h.closed
	h.mu.RUnlock()
	switch {
	case closed:
		return nil, fmt.Errorf("%w: host closed", ErrShed)
	case !ok:
		if err := h.Pending(db); err != nil {
			return nil, err
		}
		return nil, ErrUnknownDB
	}
	return a, nil
}

// QueueStats are the actors' queue depths at one instant.
type QueueStats struct {
	Actors   int
	Total    int // requests queued across actors
	Max, P99 int // deepest queue, and the 99th percentile across actors
}

// QueueDepths scans every actor's queue length (O(actors); for a scrape, not a hot path).
func (h *StaticHost) QueueDepths() []int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	depths := make([]int, 0, len(h.actors))
	for _, a := range h.actors {
		depths = append(depths, a.QueueLen())
	}
	return depths
}

// SummarizeQueues is the QueueStats of one depth per actor (it sorts depths).
func SummarizeQueues(depths []int) QueueStats {
	st := QueueStats{Actors: len(depths)}
	if len(depths) == 0 {
		return st
	}
	slices.Sort(depths)
	for _, d := range depths {
		st.Total += d
	}
	st.Max, st.P99 = depths[len(depths)-1], depths[(len(depths)-1)*99/100]
	return st
}

// Close closes every actor (queued requests get actor.ErrClosed → shed). Later Lookups are shed.
func (h *StaticHost) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	actors := h.actors
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, a := range actors {
		wg.Go(a.Close)
	}
	wg.Wait()
}

// Shutdown is Close bounded by ctx, so shutdown has one budget. If ctx ends first it returns an error matching
// ctx.Err(); the actors finish closing in the background (each statement is bounded by its own timeout).
func (h *StaticHost) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() { h.Close(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errs.Wrap("front", ctx.Err(), "not every database closed in time; the rest finish closing in the background")
	}
}
