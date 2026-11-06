package client

import (
	"strconv"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/outcome"
)

// The decisions of the client loop, pure: what an answer means for the next step, and when a cached route
// should be refreshed.

// step is what the loop does after an answer.
type step uint8

const (
	stepDone    step = iota // ok: return it
	stepFail                // a final answer: return it as an error
	stepRetry               // ask the retry policy (and the budget): same route, after a backoff ≥ Retry-After
	stepReroute             // the route is wrong or gone: drop it, resolve again, then retry
)

// answer is one attempt's result as the loop sees it: a wire code, or codeTransport.
type answer struct {
	Code       string        // wire or locator code, or codeTransport
	Ambiguous  bool          // an expired write that may have committed
	RetryAfter time.Duration // the server's hint; 0 = none
}

// codeTransport is the code of an attempt that got no answer: connect failure, reset, unreadable body.
const codeTransport = "transport"

// decide maps an answer from an agent to the next step (contract 3, front.Spec's "client should" column):
//
//	ok                                   → done
//	wrong_owner, fenced, ticket_invalid  → reroute (fenced: after Retry-After)
//	no answer (connect failure, reset)   → reroute: the VM may be gone; a write retries with the same request_id
//	shed, unavailable, aborted, expired,
//	stmt_timeout, internal               → the retry policy decides (by outcome; stmt_timeout and internal are
//	                                       not in the default RetryOn)
//	bad_request, anything unknown        → fail
func decide(a answer) (step, outcome.Code) {
	if a.Code == codeTransport {
		return stepReroute, outcome.Error
	}
	switch front.Code(a.Code) {
	case front.OK:
		return stepDone, outcome.OK
	case front.WrongOwner, front.Fenced, front.TicketInvalid:
		return stepReroute, outcome.Error
	case front.Internal:
		return stepRetry, outcome.Error
	case front.Shed, front.Unavailable, front.Aborted, front.Expired, front.StmtTimeout:
		c, _ := outcome.Parse(a.Code) // the wire names are the outcome names
		return stepRetry, c
	}
	return stepFail, outcome.Error
}

// minEpochAfter is the min_epoch for the re-resolve after a reroute from a route at epoch: one past it when the
// agent said the route's epoch is over (wrong_owner, fenced), which makes the locator read the primary; 0
// otherwise (a fresh ticket for the same placement, or a VM that did not answer).
func minEpochAfter(a answer, epoch uint64) uint64 {
	if c := front.Code(a.Code); c == front.WrongOwner || c == front.Fenced {
		return epoch + 1
	}
	return 0
}

// resolveStep maps a locator answer (POST /v1/resolve, CP-5) to the next step:
//
//	moving, unavailable, shed, timeout,
//	no answer                                → retry the resolve after Retry-After (moving: not a failure,
//	                                           so it does not spend the retry budget)
//	not_found, forbidden, unauthorized,
//	suspended, bad_request, unknown          → fail
func resolveStep(a answer) (step, outcome.Code) {
	switch a.Code {
	case "moving":
		return stepReroute, outcome.Unavailable
	case "unavailable", "shed", "timeout", codeTransport:
		return stepRetry, outcome.Unavailable
	}
	return stepFail, outcome.Error
}

// retryAfter reads a server's hint: the body's retry_after_ms when set (agentd's answers carry both, the body
// with millisecond precision), else the Retry-After header in seconds; anything unparsable is no hint.
//
//	post: result >= 0 and <= front.MaxRetryAfter
func retryAfter(header string, bodyMS int) time.Duration {
	var d time.Duration
	if bodyMS > 0 {
		d = time.Duration(bodyMS) * time.Millisecond
	} else if s, err := strconv.Atoi(header); err == nil && s > 0 {
		d = time.Duration(s) * time.Second
	}
	return min(d, front.MaxRetryAfter)
}

// refreshAt is when a route fetched at fetched with ticket TTL ttl should be refreshed in the background: at
// ahead × ttl, moved by up to ±jitter × ttl (u in [0, 1) picks where), so clients that resolved together do not
// refresh together.
//
//	pre:  0 < ahead - jitter, ahead + jitter < 1
//	post: fetched + (ahead-jitter)·ttl <= result <= fetched + (ahead+jitter)·ttl
func refreshAt(fetched time.Time, ttl time.Duration, ahead, jitter, u float64) time.Time {
	f := ahead + jitter*(2*u-1)
	return fetched.Add(time.Duration(f * float64(ttl)))
}

// usableUntil is when a route stops being used at all: its ticket's TTL from the moment the resolve was sent,
// minus a margin for clock skew and the request's own flight (a tenth of the TTL, at most a second).
func usableUntil(sent time.Time, ttl time.Duration) time.Time {
	return sent.Add(ttl - min(ttl/10, time.Second))
}
