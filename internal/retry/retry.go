// Package retry implements client retry policies: backoff, jitter, which outcomes to retry, a total-time limit
// and a retry budget. Policy, Delay and Decide are pure; Budget is the only shared state.
//
// The benchmark runs several client profiles at once to compare well-behaved clients (capped exponential
// backoff with jitter, retry only retryable outcomes, a budget) with badly behaved ones (immediate retries, no
// jitter, retry everything), and to see how retries amplify load on a struggling database.
package retry

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/outcome"
)

// Backoff is how the delay grows between attempts.
type Backoff string

const (
	NoBackoff   Backoff = "none"        // retry at once
	Fixed       Backoff = "fixed"       // Base every time
	Exponential Backoff = "exponential" // Base·2^(n-2) for attempt n, capped at Max
)

// Jitter randomizes a delay d.
type Jitter string

const (
	NoJitter     Jitter = "none"
	Full         Jitter = "full"         // uniform in [0, d]
	Equal        Jitter = "equal"        // uniform in [d/2, d]
	Decorrelated Jitter = "decorrelated" // min(Max, uniform in [Base, 3·previous])
)

// Policy is one client profile's retry behavior. Call Normalize before use.
type Policy struct {
	MaxAttempts  int           // total attempts including the first; 1 = never retry
	Backoff      Backoff       // default none
	Base         time.Duration // first delay; default 10ms when backing off
	Max          time.Duration // cap on any delay; default 10s
	Jitter       Jitter        // default none
	RetryOn      outcome.Set   // default {shed, aborted}
	BudgetRatio  float64       // retries allowed per first attempt; 0 = unlimited
	TotalTimeout time.Duration // no retry may start later than this after the first attempt; 0 = never
}

// Normalize fills defaults and validates.
func (p *Policy) Normalize() error {
	p.MaxAttempts = max(p.MaxAttempts, 1)
	if p.MaxAttempts > MaxAttemptsCap {
		return fmt.Errorf("max_attempts must be <= %d, got %d", MaxAttemptsCap, p.MaxAttempts)
	}
	if p.Backoff == "" {
		p.Backoff = NoBackoff
	}
	if !slices.Contains([]Backoff{NoBackoff, Fixed, Exponential}, p.Backoff) {
		return fmt.Errorf("backoff %q: want none, fixed or exponential", p.Backoff)
	}
	if p.Jitter == "" {
		p.Jitter = NoJitter
	}
	if !slices.Contains([]Jitter{NoJitter, Full, Equal, Decorrelated}, p.Jitter) {
		return fmt.Errorf("jitter %q: want none, full, equal or decorrelated", p.Jitter)
	}
	if p.Backoff != NoBackoff && p.Base <= 0 {
		p.Base = 10 * time.Millisecond
	}
	if p.Max <= 0 {
		p.Max = 10 * time.Second
	}
	p.Max = max(p.Max, p.Base)
	if p.BudgetRatio < 0 {
		return fmt.Errorf("budget_ratio must be >= 0, got %g", p.BudgetRatio)
	}
	if p.RetryOn == 0 {
		p.RetryOn = outcome.SetOf(outcome.Shed, outcome.Aborted)
	}
	return nil
}

// Delay is the wait before attempt next (2, 3, ...); prev is the previous delay (used by decorrelated jitter).
//
//	pre:  p normalized, next >= 2
//	post: 0 <= delay <= p.Max
func (p *Policy) Delay(rng *rand.Rand, next int, prev time.Duration) time.Duration {
	assert.That(next >= 2, "Delay: attempt %d is not a retry", next)
	var d time.Duration
	switch p.Backoff {
	case NoBackoff:
		return 0
	case Fixed:
		d = p.Base
	case Exponential:
		d = p.Base
		for i := 2; i < next && d < p.Max; i++ {
			d *= 2
		}
	}
	d = min(d, p.Max)
	switch p.Jitter {
	case Full:
		d = time.Duration(rng.Int64N(int64(d) + 1))
	case Equal:
		d = d/2 + time.Duration(rng.Int64N(int64(d/2)+1))
	case Decorrelated:
		hi := max(3*prev, p.Base)
		d = min(p.Base+time.Duration(rng.Int64N(int64(hi-p.Base)+1)), p.Max)
	}
	assert.That(d >= 0 && d <= p.Max, "Delay %s outside [0, %s]", d, p.Max)
	return d
}

// Attempt describes the attempt that just finished.
type Attempt struct {
	N       int           // 1 = first attempt
	Outcome outcome.Code  // what happened
	Elapsed time.Duration // since the first attempt was scheduled
	Prev    time.Duration // delay before this attempt (0 for the first)
}

// MaxAttemptsCap bounds attempts regardless of policy (records store the attempt number in a byte).
const MaxAttemptsCap = 255

// Decide says whether to retry after a, and after what delay. It ignores the budget, which is shared state.
func (p *Policy) Decide(rng *rand.Rand, a Attempt) (delay time.Duration, retry bool) {
	next := a.N + 1
	if next > p.MaxAttempts || next > MaxAttemptsCap || !p.RetryOn.Has(a.Outcome) {
		return 0, false
	}
	delay = p.Delay(rng, next, a.Prev)
	if p.TotalTimeout > 0 && a.Elapsed+delay > p.TotalTimeout {
		return 0, false
	}
	return delay, true
}

// Budget caps retries at Ratio per first attempt, plus a small allowance so a cold start can retry. Safe for
// concurrent use.
type Budget struct {
	Ratio   float64
	firsts  atomic.Int64
	retries atomic.Int64
}

// BudgetAllowance is the number of retries allowed before any ratio applies.
const BudgetAllowance = 10

// First records a first attempt.
func (b *Budget) First() { b.firsts.Add(1) }

// TryRetry takes one retry from the budget, or reports that it is spent.
func (b *Budget) TryRetry() bool {
	if b.Ratio <= 0 {
		b.retries.Add(1)
		return true
	}
	limit := int64(b.Ratio*float64(b.firsts.Load())) + BudgetAllowance
	for {
		r := b.retries.Load()
		if r >= limit {
			return false
		}
		if b.retries.CompareAndSwap(r, r+1) {
			return true
		}
	}
}

// Counts returns first attempts and retries so far.
func (b *Budget) Counts() (firsts, retries int64) { return b.firsts.Load(), b.retries.Load() }
