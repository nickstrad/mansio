// Package breaker is a circuit breaker for one remote dependency (Release It C1–C4): Closed → Open when the
// share of system failures in a window passes a threshold → HalfOpen after a cool-down, where one probe call
// decides Closed or Open again.
//
// Domain code: no I/O, no logging, no clock of its own. Every method takes now, so tests and fuzzers drive
// time; a transition is reported through Config.OnChange for the adapter that wires the breaker to log
// (message code S3.BREAKER_OPEN etc.) and to expose (RI-11). The caller decides what counts as a failure
// (Release It C2: only system failures; answers such as not-found are successes).
//
// Use: p, wait, ok := b.Allow(now); if !ok, fail fast with a retry hint of wait; else make the call and report
// b.Done(p, now, outcome). Outcomes of calls permitted before the last transition are ignored, so a slow
// call that started while Closed cannot reopen a breaker that has since closed again.
package breaker

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
)

// ErrOpen is what a caller wraps when Allow refuses: the breaker is open (or half-open with its probe in
// flight). Nothing was sent.
var ErrOpen = errors.New("breaker: open")

// State is the breaker's state.
type State int

const (
	Closed   State = iota // calls flow; failures are counted
	Open                  // calls fail fast until the cool-down ends
	HalfOpen              // one probe call is let through
)

func (s State) String() string { return [...]string{"closed", "open", "half_open"}[s] }

// Outcome is how a permitted call ended.
type Outcome int

const (
	Success Outcome = iota // the dependency answered (an application error such as not-found is an answer)
	Failure                // a system failure: timeout, connection error, 5xx
	Ignore                 // says nothing about the dependency (the caller gave up first)
)

// Config sets the thresholds. The zero value takes the S3 defaults of Release It §P.
type Config struct {
	Window      time.Duration // failures are counted per window of this length; default 10 s
	MinCalls    int           // the breaker opens only after at least this many calls in a window; default 20
	FailureRate float64       // ...and when failures/calls reaches this share, in (0, 1]; default 0.5
	CoolDown    time.Duration // how long it stays open before a probe; default 5 s
	// OnChange, if set, is called after every transition, outside the breaker's lock.
	OnChange func(t Transition)
}

// Transition is one state change.
type Transition struct {
	From, To State
	At       time.Time
	Calls    int // calls and failures in the window that opened it (From == Closed); else 0
	Failures int
}

// Normalize fills in defaults and checks the config (the edge: it may come from a file).
func (c *Config) Normalize() error {
	if c.Window == 0 {
		c.Window = 10 * time.Second
	}
	if c.MinCalls == 0 {
		c.MinCalls = 20
	}
	if c.FailureRate == 0 {
		c.FailureRate = 0.5
	}
	if c.CoolDown == 0 {
		c.CoolDown = 5 * time.Second
	}
	if c.Window < 0 || c.MinCalls < 1 || c.CoolDown < 0 || !(c.FailureRate > 0 && c.FailureRate <= 1) {
		return fmt.Errorf("breaker: invalid config: window %v, min calls %d, failure rate %v, cool-down %v",
			c.Window, c.MinCalls, c.FailureRate, c.CoolDown)
	}
	return nil
}

// Permit is a call Allow let through. Report its outcome with Done exactly once.
type Permit struct {
	gen   uint64
	probe bool
}

// Breaker is safe for concurrent use.
//
// Invariant: Closed ⇒ 0 <= fails <= calls and not probing; Open ⇒ not probing; HalfOpen ⇒ calls == fails == 0.
// gen changes on every transition, so permits from before it are recognized and ignored.
type Breaker struct {
	cfg Config

	mu          sync.Mutex
	state       State
	gen         uint64
	windowStart time.Time
	calls       int
	fails       int
	until       time.Time // Open: when the cool-down ends
	probing     bool      // HalfOpen: the probe is in flight
	transitions uint64
}

// New returns a closed breaker. It fails only on an invalid config.
func New(cfg Config) (*Breaker, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return &Breaker{cfg: cfg}, nil
}

// Allow asks to make one call at now.
//
//	post: ok ⇒ the caller must report Done(p, …) once
//	post: !ok ⇒ wait > 0 is how long until a call may be let through again (a retry hint)
//	post: HalfOpen ⇒ at most one permit is outstanding
func (b *Breaker) Allow(now time.Time) (p Permit, wait time.Duration, ok bool) {
	b.mu.Lock()
	var tr *Transition
	defer func() { b.mu.Unlock(); b.notify(tr) }()
	switch b.state {
	case Closed:
		b.roll(now)
		return Permit{gen: b.gen}, 0, true
	case Open:
		if now.Before(b.until) {
			return Permit{}, b.until.Sub(now), false
		}
		tr = b.move(HalfOpen, now)
	}
	if b.probing {
		return Permit{}, max(b.cfg.CoolDown, time.Millisecond), false
	}
	b.probing = true
	b.invariant()
	return Permit{gen: b.gen, probe: true}, 0, true
}

// Done reports how a permitted call ended.
func (b *Breaker) Done(p Permit, now time.Time, o Outcome) {
	b.mu.Lock()
	var tr *Transition
	defer func() { b.mu.Unlock(); b.notify(tr) }()
	if p.gen != b.gen {
		return // permitted before the last transition: says nothing about now
	}
	switch b.state {
	case Closed:
		assert.That(!p.probe, "breaker: a probe permit in state closed")
		b.roll(now)
		if o == Ignore {
			return
		}
		b.calls++
		if o == Failure {
			b.fails++
		}
		if b.calls >= b.cfg.MinCalls && float64(b.fails) >= b.cfg.FailureRate*float64(b.calls) {
			tr = b.move(Open, now)
		}
	case HalfOpen:
		assert.That(p.probe && b.probing, "breaker: a non-probe permit of the half-open generation")
		switch o {
		case Success:
			tr = b.move(Closed, now)
		case Failure:
			tr = b.move(Open, now)
		default:
			b.probing = false // the probe said nothing: the next Allow probes again
		}
	default:
		assert.That(false, "breaker: a permit of an open generation")
	}
	b.invariant()
}

// roll starts a new counting window when the current one has ended. pre: b.mu held, state Closed.
func (b *Breaker) roll(now time.Time) {
	if !now.Before(b.windowStart.Add(b.cfg.Window)) {
		b.windowStart, b.calls, b.fails = now, 0, 0
	}
}

// move changes state and returns the transition to report. pre: b.mu held.
func (b *Breaker) move(to State, now time.Time) *Transition {
	t := &Transition{From: b.state, To: to, At: now}
	if b.state == Closed {
		t.Calls, t.Failures = b.calls, b.fails
	}
	b.state, b.probing, b.calls, b.fails = to, false, 0, 0
	b.gen++
	b.transitions++
	switch to {
	case Open:
		b.until = now.Add(b.cfg.CoolDown)
	case Closed:
		b.windowStart = now
	}
	return t
}

func (b *Breaker) notify(t *Transition) {
	if t != nil && b.cfg.OnChange != nil {
		b.cfg.OnChange(*t)
	}
}

func (b *Breaker) invariant() {
	assert.That(b.fails >= 0 && b.fails <= b.calls, "breaker: %d failures of %d calls", b.fails, b.calls)
	assert.That(b.state == HalfOpen || !b.probing, "breaker: probing in state %s", b.state)
	assert.That(b.state == Closed || b.calls == 0, "breaker: %d calls counted in state %s", b.calls, b.state)
}

// State is the state at now: an open breaker whose cool-down has ended reports HalfOpen (the next Allow
// probes).
func (b *Breaker) State(now time.Time) State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && !now.Before(b.until) {
		return HalfOpen
	}
	return b.state
}

// Transitions counts state changes since New (RI-11 exposes it).
func (b *Breaker) Transitions() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.transitions
}
