// Package retry implements client retry policies: backoff, jitter, which outcomes to retry, a total-time limit
// and a retry budget. Policy, Delay and Decide are pure; Budget is the only shared state.
//
// The zero Policy is safe to reuse in a production client (Release It R3): 3 attempts, exponential backoff from
// 50 ms capped at 5 s, full jitter, and the server's Retry-After honored. The "none" backoff and jitter modes
// exist for the benchmark's badly behaved profiles, which must ask for them explicitly.
//
// The benchmark runs several client profiles at once to compare well-behaved clients (capped exponential
// backoff with jitter, retry only retryable outcomes, a budget) with badly behaved ones (immediate retries, no
// jitter, retry everything), and to see how retries amplify load on a struggling database.
package retry

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
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

// Policy is one client profile's retry behavior. Call Normalize before use. Zero fields take safe defaults.
type Policy struct {
	MaxAttempts  int           // total attempts including the first; default 3; 1 = never retry
	Backoff      Backoff       // default exponential
	Base         time.Duration // first delay; default 50ms when backing off
	Max          time.Duration // cap on any jittered delay (a server hint may exceed it); default 5s
	Jitter       Jitter        // default full
	RetryOn      outcome.Set   // default {shed, unavailable, aborted}
	BudgetRatio  float64       // retries allowed per first attempt (see Budget); 0 = unlimited
	TotalTimeout time.Duration // no retry may start later than this after the first attempt; 0 = never
}

// Safe defaults (Release It R3, guide §P "Client").
const (
	DefaultMaxAttempts = 3
	DefaultBase        = 50 * time.Millisecond
	DefaultMax         = 5 * time.Second
)

// Normalize fills defaults and validates.
func (p *Policy) Normalize() error {
	if p.MaxAttempts < 0 {
		return fmt.Errorf("max_attempts must be >= 0, got %d", p.MaxAttempts)
	}
	if p.MaxAttempts == 0 {
		p.MaxAttempts = DefaultMaxAttempts
	}
	if p.MaxAttempts > MaxAttemptsCap {
		return fmt.Errorf("max_attempts must be <= %d, got %d", MaxAttemptsCap, p.MaxAttempts)
	}
	if p.Backoff == "" {
		p.Backoff = Exponential
	}
	if !slices.Contains([]Backoff{NoBackoff, Fixed, Exponential}, p.Backoff) {
		return fmt.Errorf("backoff %q: want none, fixed or exponential", p.Backoff)
	}
	if p.Jitter == "" {
		p.Jitter = Full
	}
	if !slices.Contains([]Jitter{NoJitter, Full, Equal, Decorrelated}, p.Jitter) {
		return fmt.Errorf("jitter %q: want none, full, equal or decorrelated", p.Jitter)
	}
	if p.Backoff != NoBackoff && p.Base <= 0 {
		p.Base = DefaultBase
	}
	if p.Max <= 0 {
		p.Max = DefaultMax
	}
	p.Max = max(p.Max, p.Base)
	if p.BudgetRatio < 0 {
		return fmt.Errorf("budget_ratio must be >= 0, got %g", p.BudgetRatio)
	}
	if p.RetryOn == 0 {
		p.RetryOn = outcome.SetOf(outcome.Shed, outcome.Unavailable, outcome.Aborted)
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
	N          int           // 1 = first attempt
	Outcome    outcome.Code  // what happened
	Elapsed    time.Duration // since the first attempt was scheduled
	Prev       time.Duration // delay before this attempt (0 for the first)
	ServerHint time.Duration // the server's Retry-After (0 = none); the next attempt waits at least this long
}

// HintOf returns the retry hint carried by err's chain: the RetryAfter of the first error that has one
// (front.RetryHint, a breaker's time to half-open), or 0. In-process callers pass it as Attempt.ServerHint;
// network clients take the response's Retry-After instead.
func HintOf(err error) time.Duration {
	if err == nil {
		return 0
	}
	var h interface{ RetryAfter() time.Duration }
	if errors.As(err, &h) {
		return max(h.RetryAfter(), 0)
	}
	return 0
}

// MaxAttemptsCap bounds attempts regardless of policy (records store the attempt number in a byte).
const MaxAttemptsCap = 255

// Decide says whether to retry after a, and after what delay: the jittered backoff, or the server's hint if that
// is longer (Release It R3: max(serverHint, jitteredDelay)). It ignores the budget, which is shared state.
//
//	post: retry ⇒ delay >= a.ServerHint, and a.Elapsed + delay <= TotalTimeout when TotalTimeout > 0
func (p *Policy) Decide(rng *rand.Rand, a Attempt) (delay time.Duration, retry bool) {
	next := a.N + 1
	if next > p.MaxAttempts || next > MaxAttemptsCap || !p.RetryOn.Has(a.Outcome) {
		return 0, false
	}
	delay = max(p.Delay(rng, next, a.Prev), a.ServerHint)
	if p.TotalTimeout > 0 && a.Elapsed+delay > p.TotalTimeout {
		return 0, false
	}
	return delay, true
}

// Budget caps retries at Ratio per first attempt with a token bucket: each first attempt adds Ratio tokens, each
// retry takes one, and the bucket holds at most Burst (default BudgetAllowance). Credit therefore expires: a
// quiet hour banks at most Burst retries for the storm that follows (Release It R4), so over any window
// retries <= Burst + Ratio × first attempts in that window. The zero bucket is full, so a cold start can retry.
// Safe for concurrent use.
type Budget struct {
	Ratio float64
	Burst int // bucket size in retries; 0 = BudgetAllowance

	mu      sync.Mutex
	spent   float64 // Burst minus tokens (0 = full), so the zero Budget starts full
	firsts  atomic.Int64
	retries atomic.Int64
}

// BudgetAllowance is the default bucket size: retries allowed before any first attempt, and the most credit a
// quiet period can bank.
const BudgetAllowance = 10

func (b *Budget) burst() float64 {
	if b.Burst > 0 {
		return float64(b.Burst)
	}
	return BudgetAllowance
}

// First records a first attempt and refills the bucket by Ratio.
func (b *Budget) First() {
	b.firsts.Add(1)
	if b.Ratio <= 0 {
		return
	}
	b.mu.Lock()
	b.spent = max(b.spent-b.Ratio, 0)
	b.mu.Unlock()
}

// TryRetry takes one retry from the budget, or reports that it is spent.
func (b *Budget) TryRetry() bool {
	if b.Ratio > 0 {
		b.mu.Lock()
		ok := b.spent+1 <= b.burst()+1e-9 // tolerate float error from summing Ratio
		if ok {
			b.spent++
		}
		b.mu.Unlock()
		if !ok {
			return false
		}
	}
	b.retries.Add(1)
	return true
}

// Counts returns first attempts and retries so far.
func (b *Budget) Counts() (firsts, retries int64) { return b.firsts.Load(), b.retries.Load() }
