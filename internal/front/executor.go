package front

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/actor"
	"github.com/nickstrad/ts-db-go/internal/errs"

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
	r := &actor.Request{SQL: c.SQL, Args: c.Args, Write: c.Kind == KindWrite, StmtTimeout: c.StmtTimeout,
		Deadline: c.Deadline}
	if c.Kind == KindRead {
		r.MaxRows = c.MaxRows
	}
	if err := a.Submit(r); err != nil {
		return Reply{}, actorError(err, false)
	}
	grace := e.Grace
	if grace <= 0 {
		grace = time.Second
	}
	wctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), c.Deadline.Add(grace))
	defer cancel()
	res := r.Wait(wctx)
	if res.Err != nil {
		if wctx.Err() != nil && errors.Is(res.Err, wctx.Err()) {
			return Reply{}, ErrNoReply
		}
		return Reply{}, actorError(res.Err, res.TimedOut)
	}
	if c.Kind == KindWrite {
		return Reply{Count: res.Rows}, nil
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

// StaticHost serves a fixed set of started actors. Safe for concurrent Lookup; Close closes every actor.
type StaticHost struct {
	mu     sync.RWMutex
	actors map[string]*actor.Actor
	closed bool
}

// NewStaticHost returns a host serving actors (db id → started actor). It takes ownership of them.
func NewStaticHost(actors map[string]*actor.Actor) *StaticHost { return &StaticHost{actors: actors} }

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
		return nil, ErrUnknownDB
	}
	return a, nil
}

// Len is the number of databases served.
func (h *StaticHost) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.actors)
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
