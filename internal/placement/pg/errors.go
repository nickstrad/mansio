package pg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/owner"
)

var (
	// ErrUnavailable: the authority cannot answer now. Either the breaker is open (nothing was sent; the
	// error has a RetryAfter hint), or the call hit a system failure: a timeout, a lock timeout, a connection
	// error. For a CAS the outcome is then unknown: re-read before deciding. Retry later (Release It C3, F2).
	ErrUnavailable = errors.New("placement: authority unavailable")
	// ErrInvariant: the schema refused the change (the epoch-monotonic trigger, a check, a duplicate moves
	// row). Not a conflict: the expected row matched, and the caller asked for an illegal next row. A bug.
	ErrInvariant = errors.New("placement: change breaks an invariant of the authority")
	// ErrUnknownVM: the owner VM named by the change is not in the vms registry.
	ErrUnknownVM = errors.New("placement: unknown vm")
)

// classify maps a driver error to the package's error classes. It returns err itself (with its chain) when
// it is a system failure, marked unavailable.
func classify(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23503": // foreign_key_violation: owner_vm, from_vm or to_vm not in vms
			return marked{err, ErrUnknownVM}
		case "23514", "23502", "23505", "23P01": // check, not null, unique, exclusion
			return marked{err, ErrInvariant}
		}
	}
	return marked{err, ErrUnavailable}
}

// isAnswer reports whether err says something about the data or the request, not about the authority's
// health: it does not count against the breaker (Release It C2).
func isAnswer(err error) bool {
	for _, a := range []error{owner.ErrNotFound, owner.ErrConflict, owner.ErrInvalid, ErrInvariant, ErrUnknownVM} {
		if errors.Is(err, a) {
			return true
		}
	}
	return false
}

// marked makes err match class too, without changing its message or chain.
type marked struct {
	err   error
	class error
}

func (e marked) Error() string        { return e.err.Error() }
func (e marked) Unwrap() error        { return e.err }
func (e marked) Is(target error) bool { return target == e.class }

// openError is a call the breaker refused.
type openError struct{ wait time.Duration }

func (e *openError) Error() string { return "placement circuit breaker open" }
func (e *openError) Is(target error) bool {
	return target == ErrUnavailable || target == breaker.ErrOpen
}

// RetryAfter is the breaker's hint (front.RetryHint).
func (e *openError) RetryAfter() time.Duration { return e.wait }

// gaveUp reports whether the caller's own ctx ended (as opposed to the adapter's per-call deadline).
func gaveUp(parent context.Context) bool { return parent.Err() != nil }
