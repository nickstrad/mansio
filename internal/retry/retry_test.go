package retry

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/outcome"
)

const ms = time.Millisecond

func policy(t testing.TB, p Policy) Policy {
	t.Helper()
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	return p
}

func rng() *rand.Rand { return rand.New(rand.NewPCG(1, 1)) }

func TestNormalizeDefaults(t *testing.T) {
	p := policy(t, Policy{})
	if p.MaxAttempts != 1 || p.Backoff != NoBackoff || p.Jitter != NoJitter || p.Base != 0 || p.Max != 10*time.Second {
		t.Fatalf("%+v", p)
	}
	if !p.RetryOn.Has(outcome.Shed) || !p.RetryOn.Has(outcome.Aborted) || p.RetryOn.Has(outcome.StmtTimeout) {
		t.Fatalf("default retry_on = %b", p.RetryOn)
	}
	if b := policy(t, Policy{Backoff: Exponential}); b.Base != 10*ms {
		t.Fatalf("backoff without base: base %s", b.Base)
	}
	if b := policy(t, Policy{Backoff: Fixed, Base: 2 * time.Second, Max: time.Second}); b.Max != 2*time.Second {
		t.Fatalf("max below base not raised: %s", b.Max)
	}
}

func TestNormalizeRejects(t *testing.T) {
	for _, p := range []Policy{{Backoff: "linear"}, {Jitter: "some"}, {BudgetRatio: -0.1}} {
		if err := p.Normalize(); err == nil {
			t.Errorf("%+v: want an error", p)
		}
	}
}

func TestDelayWithoutJitter(t *testing.T) {
	exp := policy(t, Policy{Backoff: Exponential, Base: 10 * ms, Max: 70 * ms})
	fixed := policy(t, Policy{Backoff: Fixed, Base: 25 * ms})
	none := policy(t, Policy{Jitter: Full})
	tests := []struct {
		name string
		p    Policy
		next int
		want time.Duration
	}{
		{"exponential first retry is base", exp, 2, 10 * ms},
		{"exponential doubles", exp, 3, 20 * ms},
		{"exponential below cap", exp, 4, 40 * ms},
		{"exponential capped (on point)", exp, 5, 70 * ms},
		{"exponential stays capped", exp, 30, 70 * ms},
		{"fixed", fixed, 9, 25 * ms},
		{"no backoff ignores jitter", none, 5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Delay(rng(), tc.next, 0); got != tc.want {
				t.Fatalf("Delay(next=%d) = %s, want %s", tc.next, got, tc.want)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	p := policy(t, Policy{MaxAttempts: 3, Backoff: Fixed, Base: 100 * ms, TotalTimeout: time.Second,
		RetryOn: outcome.SetOf(outcome.Shed)})
	tests := []struct {
		name  string
		a     Attempt
		retry bool
	}{
		{"retryable, attempts left", Attempt{N: 1, Outcome: outcome.Shed}, true},
		{"last allowed retry (off point)", Attempt{N: 2, Outcome: outcome.Shed}, true},
		{"max attempts reached (on point)", Attempt{N: 3, Outcome: outcome.Shed}, false},
		{"success is never retried", Attempt{N: 1, Outcome: outcome.OK}, false},
		{"outcome not in retry_on", Attempt{N: 1, Outcome: outcome.StmtTimeout}, false},
		{"retry lands exactly at total timeout", Attempt{N: 1, Outcome: outcome.Shed, Elapsed: 900 * ms}, true},
		{"retry would land past total timeout", Attempt{N: 1, Outcome: outcome.Shed, Elapsed: 900*ms + 1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := p.Decide(rng(), tc.a)
			if ok != tc.retry {
				t.Fatalf("Decide(%+v) retry = %v, want %v", tc.a, ok, tc.retry)
			}
			if ok && d != 100*ms {
				t.Fatalf("delay %s", d)
			}
		})
	}
}

func TestMaxAttemptsCap(t *testing.T) {
	if err := (&Policy{MaxAttempts: MaxAttemptsCap}).Normalize(); err != nil {
		t.Fatalf("max_attempts at the cap (off point) rejected: %v", err)
	}
	if err := (&Policy{MaxAttempts: MaxAttemptsCap + 1}).Normalize(); err == nil {
		t.Fatal("max_attempts above the cap (on point) accepted")
	}
	p := policy(t, Policy{MaxAttempts: MaxAttemptsCap, RetryOn: outcome.All})
	if _, ok := p.Decide(rng(), Attempt{N: MaxAttemptsCap - 1, Outcome: outcome.Shed}); !ok {
		t.Fatal("the last allowed attempt was refused")
	}
	if _, ok := p.Decide(rng(), Attempt{N: MaxAttemptsCap, Outcome: outcome.Shed}); ok {
		t.Fatal("an attempt past the cap was allowed")
	}
}

func TestBudget(t *testing.T) {
	tests := []struct {
		name    string
		ratio   float64
		firsts  int
		allowed int
	}{
		{"unlimited", 0, 0, 1000},
		{"cold start gets only the allowance", 0.1, 0, BudgetAllowance},
		{"ratio of firsts plus allowance", 0.1, 1000, 100 + BudgetAllowance},
		{"fractional limit truncates", 0.1, 19, 1 + BudgetAllowance},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &Budget{Ratio: tc.ratio}
			for range tc.firsts {
				b.First()
			}
			got := 0
			for range 1000 {
				if b.TryRetry() {
					got++
				}
			}
			if got != tc.allowed {
				t.Fatalf("allowed %d retries, want %d", got, tc.allowed)
			}
		})
	}
}

// Property: for any policy and attempt, a delay is within [0, Max], and jitter kinds keep their own bounds.
func FuzzDelayBounds(f *testing.F) {
	f.Add(uint8(2), uint8(1), int64(10), int64(1000), uint8(3), int64(5), uint64(1))
	f.Add(uint8(1), uint8(3), int64(1), int64(1), uint8(200), int64(0), uint64(9))
	f.Fuzz(func(t *testing.T, backoff, jitter uint8, baseMs, maxMs int64, next uint8, prevMs int64, seed uint64) {
		if baseMs < 0 || maxMs < 0 || prevMs < 0 || baseMs > 1e6 || maxMs > 1e6 || prevMs > 1e6 || next < 2 {
			t.Skip()
		}
		p := Policy{
			Backoff: []Backoff{NoBackoff, Fixed, Exponential}[backoff%3],
			Jitter:  []Jitter{NoJitter, Full, Equal, Decorrelated}[jitter%4],
			Base:    time.Duration(baseMs) * ms, Max: time.Duration(maxMs) * ms,
		}
		if err := p.Normalize(); err != nil {
			t.Fatal(err)
		}
		d := p.Delay(rand.New(rand.NewPCG(seed, 0)), int(next), time.Duration(prevMs)*ms)
		if d < 0 || d > p.Max {
			t.Fatalf("delay %s outside [0, %s] for %+v", d, p.Max, p)
		}
		if p.Backoff == NoBackoff && d != 0 {
			t.Fatalf("no backoff gave %s", d)
		}
		if p.Backoff != NoBackoff && p.Jitter == Decorrelated && d < min(p.Base, p.Max) {
			t.Fatalf("decorrelated delay %s below base %s", d, p.Base)
		}
	})
}
