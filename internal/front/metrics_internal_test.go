package front

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOutcomeClassTable(t *testing.T) {
	for i, c := range Codes {
		if c.index() != i {
			t.Errorf("%s: index %d, want %d", c, c.index(), i)
		}
	}
	want := map[Code]string{OK: ClassOK, Shed: ClassRefused, Unavailable: ClassRefused, Expired: ClassTimeout,
		StmtTimeout: ClassAppError, Aborted: ClassAppError, WrongOwner: ClassAppError, Fenced: ClassAppError,
		TicketInvalid: ClassAppError, BadRequest: ClassAppError, Internal: Class5xx}
	for _, c := range Codes {
		if got := OutcomeClass(c, false); got != want[c] {
			t.Errorf("%s: %s, want %s", c, got, want[c])
		}
		if c != Expired && OutcomeClass(c, true) != want[c] {
			t.Errorf("%s ambiguous changed its class", c)
		}
	}
	if OutcomeClass(Expired, true) != ClassAmbiguous {
		t.Error("an ambiguous expired write is not ambiguous")
	}
}

func TestQueueDrain(t *testing.T) {
	tests := []struct {
		queued int
		rate   float64
		want   time.Duration
	}{
		{10, 0, 0},                        // no completions measured: no estimate
		{0, 1, time.Second},               // only itself ahead of nothing
		{9, 10, time.Second},              // on point: 10 at 10/s
		{99, 10, 10 * time.Second},        // 100 at 10/s
		{0, 1000, time.Millisecond},       // fast: below the 1 s floor (the floor applies on the wire)
		{5999, 100, 60 * time.Second},     // = MaxRetryAfter
		{100000, 1, 100001 * time.Second}, // past the cap: retryAfter caps it at MaxRetryAfter
	}
	for _, tc := range tests {
		if got := queueDrain(tc.queued, tc.rate); got != tc.want {
			t.Errorf("queueDrain(%d, %v) = %v, want %v", tc.queued, tc.rate, got, tc.want)
		}
	}
	if got := retryAfter(Shed, WithRetryAfter(ErrShed, queueDrain(100000, 1))); got != int(MaxRetryAfter/time.Second) {
		t.Errorf("capped Retry-After %d", got)
	}
}

func TestDrainRateWindows(t *testing.T) {
	var d drainRate
	t0 := time.Unix(1000, 0)
	for range 5 {
		d.done(t0.Add(100 * time.Millisecond))
	}
	if r := d.perSec(t0.Add(900 * time.Millisecond)); r != 0 {
		t.Fatalf("within the first second: %v, want 0 (no whole second yet)", r)
	}
	if r := d.perSec(t0.Add(time.Second)); r != 5 {
		t.Fatalf("next second: %v, want 5", r)
	}
	if r := d.perSec(t0.Add(2 * time.Second)); r != 0 {
		t.Fatalf("a quiet second later: %v, want 0", r)
	}
}

// RI-10's deferred estimate: a shed answer's Retry-After is the VM queue's drain time at the measured rate.
func TestShedRetryAfterIsQueueDrain(t *testing.T) {
	now := time.Unix(2000, 0)
	a := NewAdmission(AdmissionLimits{TenantInFlight: 1, TenantQueue: 1, DBInFlight: 1, DBQueue: 1, VMInFlight: 1, VMQueue: 1})
	a.now = func() time.Time { return now }
	for range 2 { // 2 completions in second 2000
		rel, err := a.Acquire(context.Background(), "t", "db")
		if err != nil {
			t.Fatal(err)
		}
		rel()
	}
	now = now.Add(time.Second)
	hold, _ := a.Acquire(context.Background(), "t", "db")
	defer hold()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Acquire(ctx, "t", "db") // fills the one-slot queue
	for a.Stats().Queued == 0 {
		time.Sleep(time.Millisecond)
	}
	_, err := a.Acquire(context.Background(), "t", "db")
	var h RetryHint
	if !errors.Is(err, ErrShed) || !errors.As(err, &h) || h.RetryAfter() != time.Second { // (1 queued + 1) / 2 per s
		t.Fatalf("err %v", err)
	}
	if st := a.Stats(); st.Shed != 1 || st.DrainPerSec != 2 || st.InFlightHighWater != 1 || st.QueuedHighWater != 1 {
		t.Fatalf("stats %+v", st)
	}
}
