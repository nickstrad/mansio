package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/vactor"
)

// unavailableCauses are the dependency sentinels that mean "a dependency cannot serve this request now; nothing
// ran". The front answers them 503 unavailable + Retry-After and logs them at WARN, sampled — never internal
// (Release It F2, plan RI-04). Adding a dependency is one line here:
//
//   - owner.ErrUnavailable: the key is not reconciled since restart, or the authority/manifest is unreadable.
//   - vactor.ErrBusy: the fetch bulkhead's queue is full, or an activation hit its own deadline (RI-02).
//   - durable.ErrUnavailable (RI-03, not yet in durable): the S3 breaker is open. Its RetryAfter hint should
//     reach the wire through front.RetryHint (RI-10).
//
// A bare context.DeadlineExceeded is deliberately NOT listed, because it also means "the caller's deadline
// passed" (expired); vactor wraps its own activation deadline in ErrBusy.
var unavailableCauses = []error{owner.ErrUnavailable, vactor.ErrBusy}

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
