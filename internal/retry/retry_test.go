package retry

import (
	"bytes"
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

// The zero policy is the safe one (RI-10): 3 attempts, exponential from 50 ms to 5 s, full jitter. "none" modes
// must be asked for, and then they stay.
func TestNormalizeDefaults(t *testing.T) {
	p := policy(t, Policy{})
	if p.MaxAttempts != 3 || p.Backoff != Exponential || p.Jitter != Full || p.Base != 50*ms || p.Max != 5*time.Second {
		t.Fatalf("%+v", p)
	}
	if !p.RetryOn.Has(outcome.Shed) || !p.RetryOn.Has(outcome.Aborted) || p.RetryOn.Has(outcome.StmtTimeout) {
		t.Fatalf("default retry_on = %b", p.RetryOn)
	}
	if b := policy(t, Policy{MaxAttempts: 10, Backoff: NoBackoff, Jitter: NoJitter}); b.Backoff != NoBackoff || b.Jitter != NoJitter ||
		b.Base != 0 || b.MaxAttempts != 10 {
		t.Fatalf("explicit bad-client profile changed: %+v", b)
	}
	if b := policy(t, Policy{MaxAttempts: 1}); b.MaxAttempts != 1 {
		t.Fatalf("max_attempts 1 (never retry) changed: %+v", b)
	}
	if b := policy(t, Policy{Backoff: Fixed}); b.Base != 50*ms {
		t.Fatalf("backoff without base: base %s", b.Base)
	}
	if b := policy(t, Policy{Backoff: Fixed, Base: 2 * time.Second, Max: time.Second}); b.Max != 2*time.Second {
		t.Fatalf("max below base not raised: %s", b.Max)
	}
}

func TestNormalizeRejects(t *testing.T) {
	for _, p := range []Policy{{Backoff: "linear"}, {Jitter: "some"}, {BudgetRatio: -0.1}, {MaxAttempts: -1}} {
		if err := p.Normalize(); err == nil {
			t.Errorf("%+v: want an error", p)
		}
	}
}

func TestDelayWithoutJitter(t *testing.T) {
	exp := policy(t, Policy{Backoff: Exponential, Base: 10 * ms, Max: 70 * ms, Jitter: NoJitter})
	fixed := policy(t, Policy{Backoff: Fixed, Base: 25 * ms, Jitter: NoJitter})
	none := policy(t, Policy{Backoff: NoBackoff, Jitter: Full})
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
	p := policy(t, Policy{MaxAttempts: 3, Backoff: Fixed, Base: 100 * ms, Jitter: NoJitter, TotalTimeout: time.Second,
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

// The budget is a token bucket: full (Burst) at start, Ratio per first attempt, capped at Burst.
func TestBudget(t *testing.T) {
	type step struct {
		firsts, tries, allowed int
	}
	tests := []struct {
		name  string
		ratio float64
		burst int
		steps []step
	}{
		{"unlimited", 0, 0, []step{{0, 1000, 1000}}},
		{"cold start gets the allowance (on point), then nothing (off point)", 0.1, 0, []step{{0, BudgetAllowance + 1, BudgetAllowance}}},
		{"10 firsts at 0.1 earn one retry (on point)", 0.1, 0, []step{{0, 100, BudgetAllowance}, {10, 2, 1}}},
		{"9 firsts at 0.1 earn none (off point)", 0.1, 0, []step{{0, 100, BudgetAllowance}, {9, 1, 0}}},
		{"credit expires: a quiet period banks at most the burst", 0.1, 0, []step{{100000, 1000, BudgetAllowance}}},
		{"custom burst", 0.5, 3, []step{{0, 10, 3}, {4, 10, 2}, {100, 10, 3}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &Budget{Ratio: tc.ratio, Burst: tc.burst}
			total := 0
			for i, s := range tc.steps {
				for range s.firsts {
					b.First()
				}
				got := 0
				for range s.tries {
					if b.TryRetry() {
						got++
					}
				}
				if got != s.allowed {
					t.Fatalf("step %d: allowed %d retries, want %d", i, got, s.allowed)
				}
				total += got
			}
			if _, r := b.Counts(); r != int64(total) {
				t.Fatalf("Counts retries %d, want %d", r, total)
			}
		})
	}
	// Steady state, every first attempt failing and retried at once: the ratio, plus the initial burst.
	b := &Budget{Ratio: 0.1}
	got := 0
	for range 1000 {
		b.First()
		if b.TryRetry() {
			got++
		}
	}
	if got < 100 || got > 100+BudgetAllowance { // 109: the first refill lands on a full bucket
		t.Fatalf("steady state allowed %d retries per 1000 firsts, want 100..%d", got, 100+BudgetAllowance)
	}
}

// Property (RI-10 "windowed budget"): for any interleaving of first attempts and retries, every window of the
// history spends at most Burst + Ratio × (first attempts in the window) retries. A cumulative budget fails this:
// a quiet stretch would pay for a later storm.
func FuzzBudgetWindow(f *testing.F) {
	f.Add(uint8(10), uint8(0), []byte{0, 0, 0, 1, 1, 1, 1, 1})
	f.Add(uint8(50), uint8(3), []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1})
	f.Add(uint8(10), uint8(0), append(make([]byte, 300), bytes.Repeat([]byte{1}, 40)...)) // a quiet stretch, then a storm
	f.Fuzz(func(t *testing.T, ratioPct, burst uint8, script []byte) {
		if ratioPct == 0 || len(script) > 400 {
			t.Skip()
		}
		b := &Budget{Ratio: float64(ratioPct) / 100, Burst: int(burst % 20)}
		cap := float64(b.capacity()) / micro
		// firsts[i], retries[i]: counts in the prefix of length i.
		firsts, retries := []int{0}, []int{0}
		for _, op := range script {
			f, r := firsts[len(firsts)-1], retries[len(retries)-1]
			if op%2 == 0 {
				b.First()
				f++
			} else if b.TryRetry() {
				r++
			}
			firsts, retries = append(firsts, f), append(retries, r)
		}
		for i := range firsts {
			for j := i + 1; j < len(firsts); j++ {
				rs, fs := retries[j]-retries[i], firsts[j]-firsts[i]
				if float64(rs) > cap+b.Ratio*float64(fs)+1e-6 {
					t.Fatalf("window [%d,%d): %d retries for %d firsts, ratio %g, burst %g", i, j, rs, fs, b.Ratio, cap)
				}
			}
		}
	})
}

// The server's Retry-After wins when it is longer than the backoff (Release It R3), and still respects
// TotalTimeout. Policy: fixed 100 ms, total 1 s.
func TestDecideHonorsServerHint(t *testing.T) {
	p := policy(t, Policy{MaxAttempts: 3, Backoff: Fixed, Base: 100 * ms, Jitter: NoJitter, Max: 100 * ms,
		TotalTimeout: time.Second, RetryOn: outcome.SetOf(outcome.Shed)})
	tests := []struct {
		name    string
		hint    time.Duration
		elapsed time.Duration
		delay   time.Duration
		retry   bool
	}{
		{"no hint: backoff", 0, 0, 100 * ms, true},
		{"hint below backoff (off point)", 100*ms - 1, 0, 100 * ms, true},
		{"hint equals backoff (on point)", 100 * ms, 0, 100 * ms, true},
		{"hint above backoff wins, even above Max", 700 * ms, 0, 700 * ms, true},
		{"hint lands exactly at total timeout", 700 * ms, 300 * ms, 700 * ms, true},
		{"hint would land past total timeout: give up", 700 * ms, 300*ms + 1, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := p.Decide(rng(), Attempt{N: 1, Outcome: outcome.Shed, Elapsed: tc.elapsed, ServerHint: tc.hint})
			if ok != tc.retry || d != tc.delay {
				t.Fatalf("Decide = %s, %v; want %s, %v", d, ok, tc.delay, tc.retry)
			}
		})
	}
	if _, ok := p.Decide(rng(), Attempt{N: 1, Outcome: outcome.OK, ServerHint: time.Second}); ok {
		t.Fatal("a hint does not make a success retryable")
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
		// Decide with a server hint (prevMs doubles as the hint): never earlier than the hint, never later
		// than max(Max, hint).
		p.MaxAttempts, p.RetryOn = MaxAttemptsCap, outcome.All
		hint := time.Duration(prevMs) * ms
		if dd, ok := p.Decide(rand.New(rand.NewPCG(seed, 0)), Attempt{N: int(next) - 1, Outcome: outcome.Shed, Prev: hint, ServerHint: hint}); ok &&
			(dd < hint || dd > max(p.Max, hint)) {
			t.Fatalf("Decide delay %s outside [hint %s, max(%s, hint)]", dd, hint, p.Max)
		}
	})
}
