// Package front is the agent's client-facing request pipeline (scale-out option 4 §2 "actor-front"):
//
//	validate the request → verify the ticket → Admitter.Admit(db, epoch) → per-tenant and per-database
//	admission (caps, shedding, deadlines) → Executor → wire code
//
// INTERNAL, TRUSTED-CALLER PROTOCOL. A request carries parameterized SQL plus arguments. In production clients
// never send SQL: the trusted front compiles document requests into operations, and only that compiled form
// reaches an Executor. The SQL field exists so the pipeline (framing, tickets, admission, wire codes) can be
// built and measured before the compiler and the handler runtime exist. The "op" field is reserved for the
// operation payload that will replace "sql" without changing framing, admission or error codes. A light guard
// (one statement, allowed leading keywords, reads must look read-only) is defense in depth, not a boundary.
//
// The decisions are pure (codes.go, request.go, limiter.go); the pipeline (server.go) reaches the world only
// through ports (ports.go); http.go and executor.go are thin adapters.
package front

import (
	"errors"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// Code is a wire outcome (contract 3 in shared_state.md). The set is closed: clients switch on it.
type Code string

const (
	OK            Code = "ok"
	Shed          Code = "shed"           // not accepted (caps, full mailbox, draining); nothing ran
	Unavailable   Code = "unavailable"    // a dependency (storage, placement, fetch bulkhead) is unavailable; nothing ran
	Expired       Code = "expired"        // the request's deadline passed before an answer
	StmtTimeout   Code = "stmt_timeout"   // the statement timeout interrupted SQLite; nothing changed
	Aborted       Code = "aborted"        // rolled back because another write in its batch timed out
	WrongOwner    Code = "wrong_owner"    // this VM does not own the database at the ticket's epoch
	Fenced        Code = "fenced"         // the database is fenced here (moving away, suspended)
	TicketInvalid Code = "ticket_invalid" // missing, malformed, unsigned, expired, wrong audience/db/perms
	BadRequest    Code = "bad_request"    // the request itself is wrong; retrying it unchanged will fail again
	Internal      Code = "internal"       // a bug or an unexpected failure; the response carries a log ID
)

// Codes lists every wire code in spec order.
var Codes = [...]Code{OK, Shed, Unavailable, Expired, StmtTimeout, Aborted, WrongOwner, Fenced, TicketInvalid, BadRequest, Internal}

// index is c's position in Codes (a switch: no hashing on the request path).
func (c Code) index() int {
	switch c {
	case OK:
		return 0
	case Shed:
		return 1
	case Unavailable:
		return 2
	case Expired:
		return 3
	case StmtTimeout:
		return 4
	case Aborted:
		return 5
	case WrongOwner:
		return 6
	case Fenced:
		return 7
	case TicketInvalid:
		return 8
	case BadRequest:
		return 9
	}
	assert.That(c == Internal, "front: unknown code %q", c)
	return 10
}

// Outcome classes per integration point (Release It guide O1): the same six for every dependency, so
// dashboards and alerts compare them.
const (
	ClassOK        = "ok"
	ClassAppError  = "app_error" // the dependency answered no: bad input, not owned, statement rejected or too slow
	ClassTimeout   = "timeout"   // no answer before the deadline; nothing changed
	ClassRefused   = "refused"   // not accepted (shed, unavailable); nothing ran
	Class5xx       = "5xx"       // a bug or an unexpected failure
	ClassAmbiguous = "ambiguous" // no answer before the deadline, and a write may have committed
)

// OutcomeClass maps a wire code to its outcome class. ambiguous marks an expired write that may have
// committed.
func OutcomeClass(c Code, ambiguous bool) string {
	switch c {
	case OK:
		return ClassOK
	case Shed, Unavailable:
		return ClassRefused
	case Expired:
		if ambiguous {
			return ClassAmbiguous
		}
		return ClassTimeout
	case Internal:
		return Class5xx
	}
	return ClassAppError
}

// Wire is how a code goes on the wire.
type Wire struct {
	Status     int    // HTTP status
	RetryAfter int    // seconds for the Retry-After header, a floor raised by a RetryHint; 0 = no header
	Retry      string // what a client should do
}

// Spec is the wire contract and its single source in code; TestSpecTable checks every row, and
// knowledge/front/2026-11-05-wire-contract.md documents it.
var Spec = map[Code]Wire{
	OK:            {200, 0, "done (duplicate=true: this request_id was applied before; count is the original's)"},
	Shed:          {503, 1, "nothing ran; retry after Retry-After with backoff and jitter"},
	Unavailable:   {503, 1, "nothing ran; a dependency is down or recovering: retry after Retry-After with backoff and jitter"},
	Expired:       {504, 0, "reads and writes without ambiguous=true never ran: retry with a fresh deadline; ambiguous=true: a write may have committed, retry with the same request_id"},
	StmtTimeout:   {422, 0, "nothing changed; do not retry unchanged (the statement is too slow for the limit)"},
	Aborted:       {409, 0, "nothing changed; safe to retry at once"},
	WrongOwner:    {421, 0, "re-resolve the database (locator), then retry with the new ticket"},
	Fenced:        {421, 1, "re-resolve after Retry-After; the database is moving or suspended"},
	TicketInvalid: {401, 0, "get a new ticket, then retry"},
	BadRequest:    {400, 0, "fix the request; retrying it unchanged fails again"},
	Internal:      {500, 0, "report the log_id; a write may or may not have committed: retry with the same request_id"},
}

// Sentinels for outcomes produced by the ports and the pipeline. Adapters translate their own errors into these
// (e.g. the owner adapter maps owner.ErrFenced to ErrFenced) so this package imports no other agent's code.
//
// ErrUnavailable: a dependency the request needs (S3, the placement authority, the fetch bulkhead) cannot serve
// it now; nothing ran. It is a system failure, not a bug: 503 + Retry-After, logged at WARN, sampled (Release It
// F2). Adapters wrap their dependency's sentinel with it (cmd/agentd lists which ones).
var (
	ErrShed        = errors.New("front: shed")
	ErrUnavailable = errors.New("front: dependency unavailable")
	ErrExpired     = errors.New("front: deadline passed")
	ErrNoReply     = errors.New("front: no reply before the deadline; a write may have committed")
	ErrStmtTimeout = errors.New("front: statement timeout")
	ErrAborted     = errors.New("front: batch aborted")
	ErrWrongOwner  = errors.New("front: wrong owner")
	ErrFenced      = errors.New("front: fenced")
	ErrUnknownDB   = errors.New("front: database not hosted here")
	ErrStatement   = errors.New("front: statement rejected by SQLite")
	ErrTooManyRows = errors.New("front: result has more rows than allowed")
	ErrDraining    = errors.New("front: draining")
)

// RetryHint is implemented by errors that know when a retry can succeed, e.g. a breaker's time to half-open.
// For codes that carry Retry-After, the answer's Retry-After is max(the Spec floor, the hint), capped at
// MaxRetryAfter (Release It R3: the server tells clients when to come back).
type RetryHint interface{ RetryAfter() time.Duration }

// MaxRetryAfter caps a hinted Retry-After, so a wrong hint cannot park clients for long.
const MaxRetryAfter = time.Minute

// WithRetryAfter attaches a retry hint to err; errors.Is/As still see err.
func WithRetryAfter(err error, after time.Duration) error { return &hinted{err, after} }

type hinted struct {
	err   error
	after time.Duration
}

func (e *hinted) Error() string             { return e.err.Error() }
func (e *hinted) Unwrap() error             { return e.err }
func (e *hinted) RetryAfter() time.Duration { return e.after }

// retryAfter is the Retry-After in seconds for an answer of code caused by err.
//
//	post: Spec[code].RetryAfter == 0 ⇒ 0; otherwise Spec[code].RetryAfter <= result <= MaxRetryAfter in seconds
func retryAfter(code Code, err error) int {
	floor := Spec[code].RetryAfter
	var h RetryHint
	if floor == 0 || !errors.As(err, &h) {
		return floor
	}
	d := min(h.RetryAfter(), MaxRetryAfter)
	return max(floor, int((d+time.Second-1)/time.Second)) // whole seconds, rounded up
}

// BadRequestError is a validation failure. Msg is safe to send to the client: it describes their input only.
type BadRequestError struct {
	Msg    string
	Status int // 0 = 400; 413 for oversized bodies, 404/405 for path and method
}

func (e *BadRequestError) Error() string { return "bad request: " + e.Msg }

// Classify maps an error from the pipeline or a port to its wire code. Unknown errors are Internal.
func Classify(err error) Code {
	var bad *BadRequestError
	switch {
	case err == nil:
		return OK
	case errors.As(err, &bad), errors.Is(err, ErrStatement), errors.Is(err, ErrTooManyRows):
		return BadRequest
	case ticket.Invalid(err):
		return TicketInvalid
	case errors.Is(err, ErrShed), errors.Is(err, ErrDraining):
		return Shed
	case errors.Is(err, ErrUnavailable):
		return Unavailable
	case errors.Is(err, ErrExpired), errors.Is(err, ErrNoReply):
		return Expired
	case errors.Is(err, ErrStmtTimeout):
		return StmtTimeout
	case errors.Is(err, ErrAborted):
		return Aborted
	case errors.Is(err, ErrWrongOwner), errors.Is(err, ErrUnknownDB):
		return WrongOwner
	case errors.Is(err, ErrFenced):
		return Fenced
	}
	return Internal
}

// message is the fixed client-facing text per code; only bad_request adds the validator's own message.
var message = map[Code]string{
	Shed:          "not accepted now; nothing ran",
	Unavailable:   "a dependency is unavailable; nothing ran",
	Expired:       "deadline passed before an answer",
	StmtTimeout:   "statement timeout; nothing changed",
	Aborted:       "batch rolled back by another write's timeout; nothing changed",
	WrongOwner:    "this VM does not serve the database at this epoch",
	Fenced:        "database is fenced on this VM",
	TicketInvalid: "ticket invalid",
	BadRequest:    "bad request",
	Internal:      "internal error",
}
