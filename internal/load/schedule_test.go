package load

import (
	"math"
	"testing"
	"time"
)

const sec = time.Second

func TestRateAt(t *testing.T) {
	d := Duration{10 * sec}
	lin := Phase{StartQPS: 0, EndQPS: 100, Shape: Linear, Duration: d}
	step := Phase{StartQPS: 30, EndQPS: 100, Shape: Step, Duration: d}
	sine := Phase{StartQPS: 10, EndQPS: 90, Shape: Sine, Duration: d, Period: Duration{4 * sec}}
	tests := []struct {
		name string
		p    Phase
		at   time.Duration
		want float64
	}{
		{"linear start", lin, 0, 0},
		{"linear before start counts as start", lin, -sec, 0},
		{"linear midpoint", lin, 5 * sec, 50},
		{"linear end (on point)", lin, 10 * sec, 100},
		{"linear past the end holds", lin, 20 * sec, 100},
		{"step ignores end", step, 9 * sec, 30},
		{"sine starts at start", sine, 0, 10},
		{"sine reaches end half a period in", sine, 2 * sec, 90},
		{"sine returns after a period", sine, 4 * sec, 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.RateAt(tc.at); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("RateAt(%s) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

func TestExpectedRequests(t *testing.T) {
	p := Phase{StartQPS: 10, EndQPS: 20, Shape: Linear, Duration: Duration{60 * sec}}
	if got := p.ExpectedRequests(); math.Abs(got-900) > 1 {
		t.Fatalf("expected requests %v, want 900", got)
	}
}

// Values are binary-exact (4/s, multiples of 125ms) so the carried fraction is exact.
func TestPacerSpec(t *testing.T) {
	t0 := time.Unix(1000, 0)
	ms := time.Millisecond
	p := &Phase{StartQPS: 4, EndQPS: 4, Shape: Step, Duration: Duration{sec}}
	pc := NewPacer(p, t0)
	steps := []struct {
		name string
		at   time.Duration
		n    int
		done bool
	}{
		{"nothing owed at the start", 0, 0, false},
		{"375ms at 4/s owes 1.5: send 1, carry 0.5", 375 * ms, 1, false},
		{"the carried half completes one at 500ms", 500 * ms, 1, false},
		{"time going backwards owes nothing", 100 * ms, 0, false},
		{"past the end counts as the end: the last 2", time.Hour, 2, true},
		{"after the end: nothing more", 2 * time.Hour, 0, true},
	}
	for _, st := range steps {
		if n, rate, done := pc.Due(t0.Add(st.at)); n != st.n || done != st.done || rate != 4 {
			t.Fatalf("%s: n=%d rate=%v done=%v, want n=%d done=%v", st.name, n, rate, done, st.n, st.done)
		}
	}
}

// Property: however the ticks fall, a phase sends its integrated rate: within one request plus the error of
// approximating the curve by straight lines between ticks.
func FuzzPacerSendsTheIntegratedRate(f *testing.F) {
	f.Add(uint8(0), uint16(100), uint16(900), uint16(2000), uint16(500), []byte{1, 1, 1, 200, 3})
	f.Add(uint8(2), uint16(5000), uint16(0), uint16(3000), uint16(1000), []byte{255, 0, 17})
	f.Fuzz(func(t *testing.T, shape uint8, start, end, durMs, periodMs uint16, ticks []byte) {
		if durMs == 0 || len(ticks) == 0 {
			t.Skip()
		}
		p := &Phase{
			StartQPS: float64(start), EndQPS: float64(end),
			Shape:    []Shape{Linear, Step, Sine}[shape%3],
			Duration: Duration{time.Duration(durMs) * time.Millisecond},
			Period:   Duration{time.Duration(max(periodMs, 200)) * time.Millisecond},
		}
		t0 := time.Unix(0, 0)
		pc := NewPacer(p, t0)
		now, sent, done := t0, 0, false
		for i := 0; !done; i++ {
			now = now.Add(time.Duration(1+int(ticks[i%len(ticks)])%50) * time.Millisecond) // 1–50ms ticks
			n, rate, d := pc.Due(now)
			if n < 0 || rate < 0 {
				t.Fatalf("n=%d rate=%v", n, rate)
			}
			sent, done = sent+n, d
		}
		want := p.ExpectedRequests()
		// Trapezoids between ticks ≤ 50ms apart are exact for linear and step. For sine the error over the phase
		// is at most duration·dt²·max|rate''|/12, and max|rate''| = amplitude·(2π/period)².
		slack := 1.0
		if p.Shape == Sine {
			amp, w := math.Abs(p.EndQPS-p.StartQPS)/2, 2*math.Pi/p.Period.Seconds()
			slack += p.Duration.Seconds() * 0.05 * 0.05 * amp * w * w / 12
		}
		if math.Abs(float64(sent)-want) > slack+want*1e-3 {
			t.Fatalf("sent %d, want %.1f (±%.1f) for %+v", sent, want, slack, p)
		}
	})
}
