package breaker

import (
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
)

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func newB(t testing.TB, c Config) *Breaker {
	t.Helper()
	b, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// calls makes n calls at now with outcome o.
func calls(b *Breaker, now time.Time, n int, o Outcome) {
	for range n {
		if p, _, ok := b.Allow(now); ok {
			b.Done(p, now, o)
		}
	}
}

func TestConfigNormalize(t *testing.T) {
	var c Config
	if err := c.Normalize(); err != nil || c.Window != 10*time.Second || c.MinCalls != 20 || c.FailureRate != 0.5 || c.CoolDown != 5*time.Second {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	for name, bad := range map[string]Config{
		"negative window":    {Window: -1},
		"negative min calls": {MinCalls: -1},
		"rate over 1":        {FailureRate: 1.01},
		"negative rate":      {FailureRate: -0.1},
		"negative cool-down": {CoolDown: -1},
	} {
		if _, err := New(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(Config{FailureRate: 1}); err != nil {
		t.Errorf("rate exactly 1 (on point) refused: %v", err)
	}
}

// Spec table: when the breaker opens, at the on and off points of MinCalls and FailureRate.
func TestOpensAtThreshold(t *testing.T) {
	tests := []struct {
		name       string
		fail, succ int
		want       State
	}{
		{"no calls", 0, 0, Closed},
		{"MinCalls-1 failures (off point)", 19, 0, Closed},
		{"MinCalls failures (on point)", 20, 0, Open},
		{"rate exactly 0.5 at MinCalls (on point)", 10, 10, Open},
		{"rate just under 0.5 (off point)", 9, 11, Closed},
		{"successes only", 0, 100, Closed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newB(t, Config{})
			calls(b, t0, tc.succ, Success) // successes first, so the last call decides
			calls(b, t0, tc.fail, Failure)
			if got := b.State(t0); got != tc.want {
				t.Fatalf("state %s, want %s", got, tc.want)
			}
		})
	}
}

// Ignored outcomes (the caller gave up) do not count as calls.
func TestIgnoreDoesNotCount(t *testing.T) {
	b := newB(t, Config{})
	calls(b, t0, 19, Failure)
	calls(b, t0, 100, Ignore)
	if b.State(t0) != Closed {
		t.Fatal("ignored outcomes opened the breaker")
	}
	calls(b, t0, 1, Failure)
	if b.State(t0) != Open {
		t.Fatal("the 20th counted failure did not open it")
	}
}

// Failures in an ended window do not count in the next one (window boundary on/off points).
func TestWindowRolls(t *testing.T) {
	b := newB(t, Config{})
	calls(b, t0, 19, Failure)
	calls(b, t0.Add(10*time.Second-1), 0, Failure) // still the first window: nothing happens
	calls(b, t0.Add(10*time.Second), 1, Failure)   // on point: a new window starts, with 1 failure
	if b.State(t0.Add(10*time.Second)) != Closed {
		t.Fatal("failures from the previous window counted")
	}
	b2 := newB(t, Config{})
	calls(b2, t0, 19, Failure)
	calls(b2, t0.Add(10*time.Second-1), 1, Failure) // off point: same window
	if b2.State(t0.Add(10*time.Second-1)) != Open {
		t.Fatal("the 20th failure just inside the window did not open it")
	}
}

// Open: fail fast with a hint until the cool-down ends (on point: a probe), then one probe decides.
func TestOpenHalfOpenClosed(t *testing.T) {
	var log []Transition
	b := newB(t, Config{OnChange: func(tr Transition) { log = append(log, tr) }})
	calls(b, t0, 20, Failure)
	until := t0.Add(5 * time.Second)

	if _, wait, ok := b.Allow(until.Add(-1)); ok || wait != 1 {
		t.Fatalf("Allow 1ns before the cool-down ends = ok %v wait %v, want refused with wait 1ns", ok, wait)
	}
	p, _, ok := b.Allow(until)
	if !ok {
		t.Fatal("no probe when the cool-down ended")
	}
	if _, wait, ok := b.Allow(until); ok || wait <= 0 {
		t.Fatalf("a second call while the probe is in flight = ok %v wait %v", ok, wait)
	}
	b.Done(p, until, Failure)
	if b.State(until) != Open {
		t.Fatal("a failed probe did not reopen")
	}
	again := until.Add(5 * time.Second)
	p, _, _ = b.Allow(again)
	b.Done(p, again, Success)
	if b.State(again) != Closed {
		t.Fatal("a good probe did not close")
	}
	want := []State{Open, HalfOpen, Open, HalfOpen, Closed}
	if len(log) != len(want) || b.Transitions() != uint64(len(want)) {
		t.Fatalf("transitions %+v (count %d), want %v", log, b.Transitions(), want)
	}
	for i, tr := range log {
		if tr.To != want[i] {
			t.Fatalf("transition %d to %s, want %s", i, tr.To, want[i])
		}
	}
	if log[0].From != Closed || log[0].Calls != 20 || log[0].Failures != 20 {
		t.Fatalf("opening transition %+v", log[0])
	}
}

// A probe whose caller gave up says nothing: the next Allow probes again.
func TestIgnoredProbeFreesTheSlot(t *testing.T) {
	b := newB(t, Config{})
	calls(b, t0, 20, Failure)
	at := t0.Add(5 * time.Second)
	p, _, _ := b.Allow(at)
	b.Done(p, at, Ignore)
	if _, _, ok := b.Allow(at); !ok {
		t.Fatal("no new probe after an ignored one")
	}
}

// A slow call permitted while closed and finishing after the breaker opened and closed again does not count.
func TestStalePermitIgnored(t *testing.T) {
	b := newB(t, Config{MinCalls: 1, FailureRate: 1})
	old, _, _ := b.Allow(t0)
	calls(b, t0, 1, Failure) // opens
	at := t0.Add(5 * time.Second)
	p, _, _ := b.Allow(at)
	b.Done(p, at, Success) // closes
	b.Done(old, at, Failure)
	if b.State(at) != Closed {
		t.Fatal("a permit from before two transitions reopened the breaker")
	}
}

// Trip holds the breaker open past the cool-down (no probe) until Reset; both are manual transitions; a repeat
// changes nothing; permits from before either are ignored.
func TestTripHoldsUntilReset(t *testing.T) {
	var log []Transition
	b := newB(t, Config{CoolDown: time.Second, OnChange: func(tr Transition) { log = append(log, tr) }})
	if b.Reset(t0) {
		t.Fatal("Reset of a closed breaker changed something")
	}
	old, _, _ := b.Allow(t0)
	if !b.Trip(t0) || b.Trip(t0) {
		t.Fatal("Trip: want true, then false (already forced)")
	}
	b.Done(old, t0, Success) // permitted before the trip: ignored, does not close it
	for _, at := range []time.Time{t0, t0.Add(time.Second), t0.Add(time.Hour)} {
		if _, wait, ok := b.Allow(at); ok || wait != time.Second || b.State(at) != Open || !b.Forced() {
			t.Fatalf("at %v: Allow ok %v wait %v state %s forced %v, want refused with the cool-down as hint, open",
				at.Sub(t0), ok, wait, b.State(at), b.Forced())
		}
	}
	at := t0.Add(time.Hour)
	if !b.Reset(at) || b.State(at) != Closed || b.Forced() {
		t.Fatalf("after Reset: state %s forced %v", b.State(at), b.Forced())
	}
	if _, _, ok := b.Allow(at); !ok {
		t.Fatal("refused after Reset")
	}
	if len(log) != 2 || log[0] != (Transition{From: Closed, To: Open, At: t0, Manual: true}) ||
		log[1] != (Transition{From: Open, To: Closed, At: at, Manual: true}) {
		t.Fatalf("transitions %+v", log)
	}

	// Trip of a breaker the calls opened: forced from then on (past the cool-down); Reset of a half-open one
	// voids its probe.
	b = newB(t, Config{MinCalls: 1, FailureRate: 1, CoolDown: time.Second})
	calls(b, t0, 1, Failure)
	if !b.Trip(t0) || b.State(t0.Add(time.Minute)) != Open {
		t.Fatal("Trip of an open breaker did not force it")
	}
	b.Reset(t0)
	calls(b, t0, 1, Failure)
	probe, _, _ := b.Allow(t0.Add(time.Second))
	b.Reset(t0.Add(time.Second))
	b.Done(probe, t0.Add(time.Second), Failure)
	if b.State(t0.Add(time.Second)) != Closed {
		t.Fatal("a probe from before Reset reopened the breaker")
	}
}

// Every assertion in Done and invariant can fire.
func TestAssertionsFire(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out")
	}
	for name, f := range map[string]func(){
		"probe permit while closed":  func() { b := newB(t, Config{}); b.Done(Permit{probe: true}, t0, Success) },
		"permit of an open gen":      func() { b := newB(t, Config{}); b.state = Open; b.Done(Permit{}, t0, Success) },
		"non-probe in half-open":     func() { b := newB(t, Config{}); b.state = HalfOpen; b.Done(Permit{}, t0, Success) },
		"invariant: fails > calls":   func() { (&Breaker{fails: 1}).invariant() },
		"invariant: probing closed":  func() { (&Breaker{probing: true}).invariant() },
		"invariant: calls when open": func() { (&Breaker{state: Open, calls: 1, fails: 1}).invariant() },
		"invariant: forced closed":   func() { (&Breaker{forced: true}).invariant() },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("did not fire")
				}
			}()
			f()
		})
	}
}

// --- model-based fuzzing ---

// model is the breaker written the slow, obvious way: it keeps every counted outcome of the current window
// and the era (number of transitions) each permit was given in.
type model struct {
	c           Config
	state       State
	windowStart time.Time
	outcomes    []bool // counted outcomes in the window; true = failure
	openedAt    time.Time
	probeOut    bool
	forced      bool
	era         int
}

type mpermit struct {
	era   int
	probe bool
	real  Permit
}

func (m *model) allow(now time.Time) (mpermit, time.Duration, bool) {
	if m.forced {
		return mpermit{}, m.c.CoolDown, false
	}
	if m.state == Open {
		if now.Before(m.openedAt.Add(m.c.CoolDown)) {
			return mpermit{}, m.openedAt.Add(m.c.CoolDown).Sub(now), false
		}
		m.state, m.probeOut, m.era = HalfOpen, false, m.era+1
	}
	switch m.state {
	case Closed:
		if !now.Before(m.windowStart.Add(m.c.Window)) {
			m.windowStart, m.outcomes = now, nil
		}
		return mpermit{era: m.era}, 0, true
	default: // HalfOpen
		if m.probeOut {
			return mpermit{}, m.c.CoolDown, false
		}
		m.probeOut = true
		return mpermit{era: m.era, probe: true}, 0, true
	}
}

func (m *model) done(p mpermit, now time.Time, o Outcome) {
	if p.era != m.era {
		return
	}
	open := func() { m.state, m.openedAt, m.outcomes, m.probeOut, m.era = Open, now, nil, false, m.era+1 }
	switch m.state {
	case Closed:
		if !now.Before(m.windowStart.Add(m.c.Window)) {
			m.windowStart, m.outcomes = now, nil
		}
		if o == Ignore {
			return
		}
		m.outcomes = append(m.outcomes, o == Failure)
		fails := 0
		for _, f := range m.outcomes {
			if f {
				fails++
			}
		}
		if len(m.outcomes) >= m.c.MinCalls && float64(fails)/float64(len(m.outcomes)) >= m.c.FailureRate {
			open()
		}
	case HalfOpen:
		switch o {
		case Success:
			m.state, m.windowStart, m.outcomes, m.probeOut, m.era = Closed, now, nil, false, m.era+1
		case Failure:
			open()
		default:
			m.probeOut = false
		}
	}
}

// trip and reset: the operator's controls. Each is a transition unless it changes nothing.
func (m *model) trip(now time.Time) bool {
	if m.forced {
		return false
	}
	m.state, m.forced, m.openedAt, m.outcomes, m.probeOut, m.era = Open, true, now, nil, false, m.era+1
	return true
}

func (m *model) reset(now time.Time) bool {
	if m.state == Closed {
		return false
	}
	m.state, m.forced, m.windowStart, m.outcomes, m.probeOut, m.era = Closed, false, now, nil, false, m.era+1
	return true
}

func (m *model) stateAt(now time.Time) State {
	if m.state == Open && !m.forced && !now.Before(m.openedAt.Add(m.c.CoolDown)) {
		return HalfOpen
	}
	return m.state
}

// FuzzBreakerModel runs a byte script of allow / done(success, failure, ignore) / tick / trip / reset against the
// breaker and the model, comparing every answer and the state after every step.
func FuzzBreakerModel(f *testing.F) {
	f.Add([]byte{0, 0, 0, 2, 2, 2, 4, 50, 0, 1})
	f.Add([]byte{0, 2, 0, 2, 0, 2, 0, 2, 4, 60, 0, 0, 3, 0, 1, 0, 1})
	f.Add([]byte{0, 0, 0, 0, 2, 2, 2, 2, 4, 200, 0, 1, 4, 10, 0, 2})
	f.Add([]byte{0, 0, 5, 1, 0, 4, 200, 0, 5, 6, 0, 1, 0, 2, 2, 2, 6, 0})
	f.Fuzz(func(t *testing.T, script []byte) {
		cfg := Config{Window: 2 * time.Second, MinCalls: 3, FailureRate: 0.5, CoolDown: time.Second}
		b := newB(t, cfg)
		m := &model{c: cfg}
		now := t0
		var out []mpermit // outstanding permits, oldest first
		for i := 0; i < len(script); i++ {
			switch op := script[i] % 7; op {
			case 0:
				rp, rwait, rok := b.Allow(now)
				mp, mwait, mok := m.allow(now)
				if rok != mok || rwait != mwait {
					t.Fatalf("step %d Allow: breaker (%v, %v), model (%v, %v)", i, rok, rwait, mok, mwait)
				}
				if rok {
					if rp.probe != mp.probe {
						t.Fatalf("step %d: probe %v vs model %v", i, rp.probe, mp.probe)
					}
					mp.real = rp
					out = append(out, mp)
				}
			case 1, 2, 3:
				if len(out) == 0 {
					continue
				}
				k := 0
				if i+1 < len(script) {
					i++
					k = int(script[i]) % len(out)
				}
				p := out[k]
				out = append(out[:k], out[k+1:]...)
				o := Outcome(op - 1)
				b.Done(p.real, now, o)
				m.done(p, now, o)
			case 4:
				d := 100 * time.Millisecond
				if i+1 < len(script) {
					i++
					d = time.Duration(script[i]) * 20 * time.Millisecond
				}
				now = now.Add(d)
			case 5:
				if got, want := b.Trip(now), m.trip(now); got != want {
					t.Fatalf("step %d Trip: %v, model %v", i, got, want)
				}
			case 6:
				if got, want := b.Reset(now), m.reset(now); got != want {
					t.Fatalf("step %d Reset: %v, model %v", i, got, want)
				}
			}
			if got, want := b.State(now), m.stateAt(now); got != want {
				t.Fatalf("step %d: state %s, model %s", i, got, want)
			}
			probes := 0
			for _, p := range out {
				if p.probe && p.era == m.era {
					probes++
				}
			}
			if probes > 1 {
				t.Fatalf("step %d: %d probes outstanding", i, probes)
			}
		}
		if b.Transitions() != uint64(m.era) {
			t.Fatalf("transitions %d, model %d", b.Transitions(), m.era)
		}
	})
}
