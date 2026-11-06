package agentglue

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// UnavailableCauses are the dependency sentinels that mean "a dependency cannot serve this request now; nothing
// ran". The front answers them 503 unavailable + Retry-After and logs them at WARN, sampled — never internal
// (Release It F2, plan RI-04). Adding a dependency is one line here:
//
//   - owner.ErrUnavailable: the key is not reconciled since restart, or the authority/manifest is unreadable.
//   - vactor.ErrBusy: the fetch bulkhead's queue is full, or an activation hit its own deadline (RI-02).
//   - durable.ErrUnavailable: the S3 breaker is open, or a store call hit a system failure (RI-03). An open
//     breaker's time to half-open, and a key's failure backoff in vactor, ride in the chain as a
//     front.RetryHint, so the wire's Retry-After says when to come back.
//   - vactor.ErrInDoubt: a write-back sent nothing because too many publishes of the key are in doubt (after
//     an S3 brownout); its RetryHint is when the oldest is past the grace window.
//   - pg.ErrUnavailable: the Postgres placement authority's breaker is open (with a RetryHint), or a call to
//     it timed out or lost its connection (CP-4). owner.Table already wraps authority errors in
//     owner.ErrUnavailable; this line keeps any direct caller right too.
//
// A bare context.DeadlineExceeded is deliberately NOT listed, because it also means "the caller's deadline
// passed" (expired); vactor wraps its own activation deadline in ErrBusy.
var UnavailableCauses = []error{owner.ErrUnavailable, vactor.ErrBusy, vactor.ErrInDoubt, durable.ErrUnavailable, pg.ErrUnavailable}

// AsUnavailable wraps err with front.ErrUnavailable when it is one of UnavailableCauses, keeping the chain (and
// any retry hint) for the log; any other error is returned unchanged.
func AsUnavailable(err error) error {
	if err == nil || errors.Is(err, front.ErrUnavailable) {
		return err
	}
	for _, c := range UnavailableCauses {
		if errors.Is(err, c) {
			return fmt.Errorf("%w: %w", front.ErrUnavailable, err)
		}
	}
	return err
}

// DepAdmitter maps an Admitter's dependency failures to front.ErrUnavailable.
type DepAdmitter struct{ front.Admitter }

// Admit admits through the wrapped Admitter.
func (a DepAdmitter) Admit(db string, epoch uint64) (func(), error) {
	release, err := a.Admitter.Admit(db, epoch)
	return release, AsUnavailable(err)
}

// DepExecutor maps an Executor's dependency failures to front.ErrUnavailable.
type DepExecutor struct{ front.Executor }

// Exec runs through the wrapped Executor.
func (e DepExecutor) Exec(ctx context.Context, c front.Call) (front.Reply, error) {
	r, err := e.Executor.Exec(ctx, c)
	return r, AsUnavailable(err)
}
