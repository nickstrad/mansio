package front

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// The wire contract, one row per code: HTTP status, Retry-After, and every error that maps to it.
func TestSpecTable(t *testing.T) {
	rows := []struct {
		code       Code
		status     int
		retryAfter int
		errs       []error
	}{
		{OK, 200, 0, []error{nil}},
		{Shed, 503, 1, []error{ErrShed, ErrDraining, fmt.Errorf("%w: actor closed", ErrShed)}},
		{Unavailable, 503, 1, []error{ErrUnavailable, fmt.Errorf("%w: %w", ErrUnavailable, errors.New("owner: unavailable")),
			errs.Wrap("agentd", fmt.Errorf("%w: s3 breaker open", ErrUnavailable), "x")}},
		{Expired, 504, 0, []error{ErrExpired, ErrNoReply}},
		{StmtTimeout, 422, 0, []error{ErrStmtTimeout}},
		{Aborted, 409, 0, []error{ErrAborted}},
		{WrongOwner, 421, 0, []error{ErrWrongOwner, ErrUnknownDB, fmt.Errorf("owner: %w", ErrWrongOwner)}},
		{Fenced, 421, 1, []error{ErrFenced}},
		{TicketInvalid, 401, 0, []error{ticket.ErrMalformed, ticket.ErrVersion, ticket.ErrUnknownKey, ticket.ErrSignature,
			ticket.ErrClaims, ticket.ErrExpired, ticket.ErrNotYetValid, ticket.ErrTTL, ticket.ErrAudience,
			ticket.ErrDatabase, ticket.ErrPermission}},
		{BadRequest, 400, 0, []error{&BadRequestError{Msg: "x"}, ErrStatement, ErrTooManyRows, fmt.Errorf("%w: UNIQUE", ErrStatement)}},
		{Internal, 500, 0, []error{errors.New("raw"), errs.Wrap("front", errors.New("io"), "x")}},
	}
	if len(rows) != len(Codes) || len(Spec) != len(Codes) {
		t.Fatalf("%d rows, %d codes, %d spec entries", len(rows), len(Codes), len(Spec))
	}
	for i, r := range rows {
		if Codes[i] != r.code {
			t.Errorf("row %d: code %s, Codes has %s", i, r.code, Codes[i])
		}
		w, ok := Spec[r.code]
		if !ok || w.Status != r.status || w.RetryAfter != r.retryAfter || w.Retry == "" {
			t.Errorf("%s: spec %+v, want status %d retry-after %d", r.code, w, r.status, r.retryAfter)
		}
		if r.code != OK && message[r.code] == "" {
			t.Errorf("%s: no client message", r.code)
		}
		for _, err := range r.errs {
			if got := Classify(err); got != r.code {
				t.Errorf("Classify(%v) = %s, want %s", err, got, r.code)
			}
		}
	}
}
