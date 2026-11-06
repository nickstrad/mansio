package main

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

// unavailableCauses are the dependency sentinels that mean "a dependency cannot serve this request now; nothing
// ran". The front answers them 503 unavailable + Retry-After and logs them at WARN, sampled — never internal
// (Release It F2, plan RI-04). Adding a dependency is one line here:
//
//   - owner.ErrUnavailable: the key is not reconciled since restart, or the authority/manifest is unreadable.
//   - vactor.ErrBusy: the fetch bulkhead's queue is full, or an activation hit its own deadline (RI-02).
//   - durable.ErrUnavailable: the S3 breaker is open, or a store call hit a system failure (RI-03). An open
//     breaker's time to half-open, and a key's failure backoff in vactor, ride in the chain as a
//     front.RetryHint, so the wire's Retry-After says when to come back.
//   - pg.ErrUnavailable: the Postgres placement authority's breaker is open (with a RetryHint), or a call to
//     it timed out or lost its connection (CP-4). owner.Table already wraps authority errors in
//     owner.ErrUnavailable; this line keeps any direct caller right too.
//
// A bare context.DeadlineExceeded is deliberately NOT listed, because it also means "the caller's deadline
// passed" (expired); vactor wraps its own activation deadline in ErrBusy.
var unavailableCauses = []error{owner.ErrUnavailable, vactor.ErrBusy, durable.ErrUnavailable, pg.ErrUnavailable}

// asUnavailable wraps err with front.ErrUnavailable when it is one of unavailableCauses, keeping the chain (and
// any retry hint) for the log; any other error is returned unchanged.
func asUnavailable(err error) error {
	if err == nil || errors.Is(err, front.ErrUnavailable) {
		return err
	}
	for _, c := range unavailableCauses {
		if errors.Is(err, c) {
			return fmt.Errorf("%w: %w", front.ErrUnavailable, err)
		}
	}
	return err
}

// depAdmitter maps an Admitter's dependency failures to front.ErrUnavailable.
type depAdmitter struct{ front.Admitter }

func (a depAdmitter) Admit(db string, epoch uint64) (func(), error) {
	release, err := a.Admitter.Admit(db, epoch)
	return release, asUnavailable(err)
}

// depExecutor maps an Executor's dependency failures to front.ErrUnavailable.
type depExecutor struct{ front.Executor }

func (e depExecutor) Exec(ctx context.Context, c front.Call) (front.Reply, error) {
	r, err := e.Executor.Exec(ctx, c)
	return r, asUnavailable(err)
}
