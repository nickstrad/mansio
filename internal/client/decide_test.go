package client

import (
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/outcome"
)

// Every wire code (front.Codes) has a row: a new code fails here until the client knows what to do with it.
func TestDecideEveryWireCode(t *testing.T) {
	want := map[front.Code]struct {
		s  step
		oc outcome.Code
	}{
		front.OK:            {stepDone, outcome.OK},
		front.Shed:          {stepRetry, outcome.Shed},
		front.Unavailable:   {stepRetry, outcome.Unavailable},
		front.Expired:       {stepRetry, outcome.Expired},
		front.StmtTimeout:   {stepRetry, outcome.StmtTimeout},
		front.Aborted:       {stepRetry, outcome.Aborted},
		front.WrongOwner:    {stepReroute, outcome.Error},
		front.Fenced:        {stepReroute, outcome.Error},
		front.TicketInvalid: {stepReroute, outcome.Error},
		front.BadRequest:    {stepFail, outcome.Error},
		front.Internal:      {stepRetry, outcome.Error},
	}
	for _, c := range front.Codes {
		w, ok := want[c]
		if !ok {
			t.Fatalf("wire code %q has no row", c)
		}
		if s, oc := decide(answer{Code: string(c)}); s != w.s || oc != w.oc {
			t.Errorf("decide(%s) = %v, %v; want %v, %v", c, s, oc, w.s, w.oc)
		}
	}
	if s, _ := decide(answer{Code: "something_new"}); s != stepFail {
		t.Errorf("unknown code: %v, want fail", s)
	}
	if s, _ := decide(answer{Transport: true}); s != stepReroute {
		t.Errorf("no answer: %v, want reroute", s)
	}
}

func TestResolveStep(t *testing.T) {
	tests := []struct {
		a    answer
		want step
	}{
		{answer{Code: "moving"}, stepReroute},
		{answer{Code: "unavailable"}, stepRetry},
		{answer{Code: "timeout"}, stepRetry},
		{answer{Code: "transport", Transport: true}, stepRetry},
		{answer{Code: "not_found"}, stepFail},
		{answer{Code: "forbidden"}, stepFail},
		{answer{Code: "unauthorized"}, stepFail},
		{answer{Code: "suspended"}, stepFail},
		{answer{Code: "bad_request"}, stepFail},
	}
	for _, tc := range tests {
		if s, _ := resolveStep(tc.a); s != tc.want {
			t.Errorf("resolveStep(%+v) = %v, want %v", tc.a, s, tc.want)
		}
	}
}

func TestMinEpochAfter(t *testing.T) {
	for code, want := range map[string]uint64{"wrong_owner": 8, "fenced": 8, "ticket_invalid": 0, "": 0} {
		if got := minEpochAfter(answer{Code: code, Transport: code == ""}, 7); got != want {
			t.Errorf("%q: %d, want %d", code, got, want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		header string
		bodyMS int
		want   time.Duration
	}{
		{"none", "", 0, 0},
		{"header seconds", "2", 0, 2 * time.Second},
		{"body wins (ms precision)", "1", 1500, 1500 * time.Millisecond},
		{"zero header", "0", 0, 0},
		{"negative header", "-3", 0, 0},
		{"http-date is not parsed", "Wed, 21 Oct 2015 07:28:00 GMT", 0, 0},
		{"capped (on point)", "60", 0, front.MaxRetryAfter},
		{"capped (above)", "3600", 0, front.MaxRetryAfter},
	}
	for _, tc := range tests {
		if got := retryAfter(tc.header, tc.bodyMS); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestRefreshAtBounds(t *testing.T) {
	t0 := time.Unix(1000, 0)
	ttl := 10 * time.Second
	for _, tc := range []struct {
		u    float64
		want time.Duration
	}{{0, 7 * time.Second}, {0.5, 8 * time.Second}, {0.999999, 9 * time.Second}} {
		got := refreshAt(t0, ttl, 0.8, 0.1, tc.u).Sub(t0)
		if d := got - tc.want; d < -time.Millisecond || d > time.Millisecond {
			t.Errorf("u=%g: refresh after %s, want %s", tc.u, got, tc.want)
		}
	}
	if got := usableUntil(t0, ttl).Sub(t0); got != 9*time.Second {
		t.Errorf("usable for %s, want 9s (TTL minus 1 s margin)", got)
	}
	if got := usableUntil(t0, 2*time.Second).Sub(t0); got != 1800*time.Millisecond {
		t.Errorf("short TTL usable for %s, want 1.8s (a tenth margin)", got)
	}
}

func TestTimeoutHeader(t *testing.T) {
	tests := []struct {
		remaining time.Duration
		want      int64
	}{
		{5 * time.Second, 4900},       // margin capped at 100 ms
		{500 * time.Millisecond, 450}, // a tenth
		{time.Millisecond, 1},         // floor
		{0, 1},                        // never 0 (the agent refuses it)
		{-time.Second, 1},
	}
	for _, tc := range tests {
		if got := timeoutHeader(tc.remaining); got != tc.want {
			t.Errorf("timeoutHeader(%s) = %d, want %d", tc.remaining, got, tc.want)
		}
	}
}
