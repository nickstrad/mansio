package load

import (
	"fmt"
	"math"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/workload"
)

// Shape is how a phase's rate moves from StartQPS to EndQPS.
type Shape string

const (
	Linear Shape = "linear" // start → end over the phase
	Step   Shape = "step"   // hold start
	Sine   Shape = "sine"   // oscillate start ↔ end with Period
)

// Phase is one segment of the run.
type Phase struct {
	Name     string   `json:"name"`
	DBs      int      `json:"dbs"`      // active databases; requests go only to databases 0..DBs-1
	Duration Duration `json:"duration"` // how long the phase lasts

	// Rate: give either queries per second or queries per minute. End defaults to Start.
	StartQPS float64 `json:"start_qps"`
	EndQPS   float64 `json:"end_qps"`
	StartQPM float64 `json:"start_qpm"`
	EndQPM   float64 `json:"end_qpm"`

	Shape  Shape    `json:"shape"`  // default linear
	Period Duration `json:"period"` // sine period; default = phase duration

	Mix          map[workload.Class]float64 `json:"mix"`           // overrides the top-level mix
	Distribution *Distribution              `json:"distribution"`  // overrides the top-level distribution
	ClientShares map[string]float64         `json:"client_shares"` // profile name → share; overrides the profiles' shares
}

// normalize converts qpm to qps, fills defaults and returns every problem. Inherited fields (Mix, ClientShares,
// Distribution) must already be set.
func (p *Phase) normalize(isClient func(string) bool) []error {
	var problems []error
	bad := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	if p.DBs <= 0 {
		bad("dbs must be > 0")
	}
	if p.Duration.Duration <= 0 {
		bad("duration must be > 0")
	}
	hasQPS := p.StartQPS != 0 || p.EndQPS != 0
	hasQPM := p.StartQPM != 0 || p.EndQPM != 0
	switch {
	case hasQPS && hasQPM:
		bad("give the rate in qps or qpm, not both")
	case hasQPM:
		p.StartQPS, p.EndQPS = p.StartQPM/60, p.EndQPM/60
	}
	if p.EndQPS == 0 && p.EndQPM == 0 {
		p.EndQPS = p.StartQPS
	}
	if p.StartQPS < 0 || p.EndQPS < 0 {
		bad("rates must be >= 0")
	}
	switch p.Shape {
	case "":
		p.Shape = Linear
	case Linear, Step, Sine:
	default:
		bad("shape %q: want linear, step or sine", p.Shape)
	}
	if p.Period.Duration <= 0 {
		p.Period = p.Duration
	}
	if err := validateWeights(p.Mix, isClass); err != nil {
		bad("mix: %w", err)
	}
	if err := validateWeights(p.ClientShares, isClient); err != nil {
		bad("client_shares: %w", err)
	}
	if err := p.Distribution.normalize(); err != nil {
		bad("distribution: %w", err)
	}
	return problems
}

// RateAt is the target rate in requests per second at offset t into the phase.
func (p *Phase) RateAt(t time.Duration) float64 {
	t = max(t, 0)
	switch p.Shape {
	case Step:
		return p.StartQPS
	case Sine: // starts at StartQPS and reaches EndQPS half a period later
		mid, amp := (p.StartQPS+p.EndQPS)/2, (p.EndQPS-p.StartQPS)/2
		return mid - amp*math.Cos(2*math.Pi*t.Seconds()/p.Period.Seconds())
	default:
		if t >= p.Duration.Duration {
			return p.EndQPS
		}
		return p.StartQPS + (p.EndQPS-p.StartQPS)*t.Seconds()/p.Duration.Seconds()
	}
}

// ExpectedRequests integrates the rate over the phase.
func (p *Phase) ExpectedRequests() float64 {
	const steps = 1000
	dt := p.Duration.Duration / steps
	total := 0.0
	for i := range steps {
		total += p.RateAt(dt*time.Duration(i)+dt/2) * dt.Seconds()
	}
	return total
}

// Pacer turns a phase's rate into a count of requests owed at each tick (open loop: what is owed does not depend
// on how fast earlier requests were answered). It is pure: the caller supplies the clock.
//
//	Invariant: 0 <= owed < 1 between calls.
type Pacer struct {
	p             *Phase
	t0, last, end time.Time
	owed          float64
}

// NewPacer starts pacing p at t0.
//
//	pre: p.Duration > 0 (true of every phase of a normalized config)
func NewPacer(p *Phase, t0 time.Time) *Pacer {
	assert.That(p.Duration.Duration > 0, "NewPacer: phase %q has duration %s", p.Name, p.Duration.Duration)
	return &Pacer{p: p, t0: t0, last: t0, end: t0.Add(p.Duration.Duration)}
}

// Due reports how many requests to send at now (the rate integrated since the previous call, trapezoid rule,
// with the fractional remainder carried), the rate at now, and whether the phase is over. Times past the end
// count as the end; times before the previous call count as the previous call.
func (pc *Pacer) Due(now time.Time) (n int, rate float64, done bool) {
	if now.Before(pc.last) {
		now = pc.last
	}
	if now.After(pc.end) {
		now = pc.end
	}
	prevRate := pc.p.RateAt(pc.last.Sub(pc.t0))
	rate = pc.p.RateAt(now.Sub(pc.t0))
	pc.owed += (prevRate + rate) / 2 * now.Sub(pc.last).Seconds()
	pc.last = now
	n = int(pc.owed)
	pc.owed -= float64(n)
	return n, rate, !now.Before(pc.end)
}
