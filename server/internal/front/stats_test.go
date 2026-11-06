package front_test

import (
	"context"
	"testing"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/front/fronttest"
)

// Every answer is counted once, by code, with its latency; the high-water mark follows in-flight requests.
func TestServerStatsCountEveryAnswer(t *testing.T) {
	r := newRig(t, front.Config{})
	tok := r.tok(t, "db-1", nil)
	r.s.Handle(context.Background(), front.Input{Ticket: tok, Body: get(1)})
	r.s.Handle(context.Background(), front.Input{Ticket: tok, Body: get(2)})
	r.s.Handle(context.Background(), front.Input{Ticket: tok, Body: []byte("{")})
	r.s.Handle(context.Background(), front.Input{Ticket: "", Body: get(1)})
	r.ex.Set(func(e *fronttest.Executor) { e.Block = true })
	out := r.s.Handle(context.Background(), front.Input{Ticket: tok, Body: put(7), Timeout: 1})
	if !out.Resp.Ambiguous {
		t.Fatalf("blocked write: %+v", out.Resp)
	}
	st := r.s.Stats()
	want := map[front.Code]int64{front.OK: 2, front.BadRequest: 1, front.TicketInvalid: 1, front.Expired: 1}
	for i, c := range front.Codes {
		if n := st.Outcomes[i][0] + st.Outcomes[i][1]; n != want[c] {
			t.Errorf("%s: %d, want %d", c, n, want[c])
		}
	}
	var n uint64
	for _, c := range st.LatencyCounts {
		n += c
	}
	if st.Outcomes[3][1] != 1 || n != 5 || st.InFlight != 0 || st.InFlightHighWater != 1 || len(st.LatencyCounts) != len(st.LatencyBounds)+1 {
		t.Fatalf("stats %+v", st)
	}
}
