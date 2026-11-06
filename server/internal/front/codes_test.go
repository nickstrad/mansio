package front

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/ticket"
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

// Retry-After is computed: the Spec floor, raised by a hint in the error chain (rounded up to whole seconds),
// capped at MaxRetryAfter; codes without Retry-After ignore hints.
func TestRetryAfterIsComputed(t *testing.T) {
	unavail := fmt.Errorf("%w: s3 breaker open", ErrUnavailable)
	tests := []struct {
		name string
		code Code
		err  error
		want int
	}{
		{"no hint: floor", Unavailable, unavail, 1},
		{"hint below the floor", Unavailable, WithRetryAfter(unavail, 200*time.Millisecond), 1},
		{"hint exactly 1 s (on point)", Unavailable, WithRetryAfter(unavail, time.Second), 1},
		{"hint just over 1 s rounds up (off point)", Unavailable, WithRetryAfter(unavail, time.Second+1), 2},
		{"hint deep in the chain", Unavailable, fmt.Errorf("agentd: %w", WithRetryAfter(unavail, 4500*time.Millisecond)), 5},
		{"hint at the cap", Shed, WithRetryAfter(ErrShed, MaxRetryAfter), 60},
		{"hint over the cap", Fenced, WithRetryAfter(ErrFenced, time.Hour), 60},
		{"no Retry-After for this code: hint ignored", BadRequest, WithRetryAfter(ErrStatement, 5*time.Second), 0},
		{"no Retry-After for internal", Internal, WithRetryAfter(errors.New("x"), 5*time.Second), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if c := Classify(tc.err); c != tc.code {
				t.Fatalf("Classify = %s, want %s (the hint must not hide the cause)", c, tc.code)
			}
			if got := retryAfter(tc.code, tc.err); got != tc.want {
				t.Fatalf("retryAfter = %d, want %d", got, tc.want)
			}
		})
	}
}
