package front_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/front/fronttest"
)

func canaryServer(ex front.Executor, adm front.AdmissionLimits) *front.Server {
	return front.NewServer(front.Config{Admission: adm}, nil, nil, ex, nil)
}

func getJSON(t *testing.T, h http.Handler) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, out
}

func TestCanaryOutcomes(t *testing.T) {
	tests := []struct {
		name string
		set  func(*fronttest.Executor)
		want error // nil = passes
	}{
		{"answers", func(*fronttest.Executor) {}, nil},
		{"wedged: no reply by the deadline", func(e *fronttest.Executor) { e.Block = true }, front.ErrNoReply},
		{"shed by its own actor: closed or stuck", func(e *fronttest.Executor) { e.Err = front.ErrShed }, front.ErrShed},
		{"dependency down", func(e *fronttest.Executor) { e.Err = front.ErrUnavailable }, front.ErrUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ex := fronttest.NewExecutor(front.CanaryDB)
			ex.Set(tc.set)
			s := canaryServer(ex, front.AdmissionLimits{})
			err := s.Canary(context.Background(), front.CanaryDB, 30*time.Millisecond)
			if (tc.want == nil) != (err == nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
			if c := ex.Last(); c.Tenant != front.CanaryTenant || c.RequestID != "canary" || c.Kind != front.KindRead || c.MaxRows != 1 {
				t.Fatalf("canary call not marked: %+v", c)
			}
		})
	}
	// Admission is part of the path: a full VM sheds the canary, which passes (overload is not a reason to
	// leave rotation); nothing reaches the executor.
	ex := fronttest.NewExecutor(front.CanaryDB)
	ex.Set(func(e *fronttest.Executor) { e.Block = true })
	s := canaryServer(ex, front.AdmissionLimits{DBInFlight: 1, TenantInFlight: 1, VMInFlight: 1, VMQueue: 1, TenantQueue: 1, DBQueue: 1})
	go s.Canary(context.Background(), front.CanaryDB, time.Second) // holds the only slot
	time.Sleep(20 * time.Millisecond)
	go s.Canary(context.Background(), front.CanaryDB, time.Second) // queued
	time.Sleep(20 * time.Millisecond)
	if err := s.Canary(context.Background(), front.CanaryDB, time.Second); err != nil {
		t.Fatalf("shed canary: %v", err)
	}
}

// RI-07 acceptance: once the canary's actor wedges, /readyz turns 503 within one probe interval (plus the
// probe's timeout); /livez stays 200.
func TestReadyzFalseWhenActorsWedged(t *testing.T) {
	ex := fronttest.NewExecutor(front.CanaryDB)
	s := canaryServer(ex, front.AdmissionLimits{})
	const every, timeout = 50 * time.Millisecond, 30 * time.Millisecond
	r := front.NewReadiness(timeout, 3*every, nil, front.Check{Name: "canary", Run: func(ctx context.Context) error {
		return s.Canary(ctx, front.CanaryDB, timeout*2/3) // inner deadline shorter than the probe's (T4)
	}})
	ready, live := front.ReadyHandler(s, r), front.LiveHandler(s)
	if code, body := getJSON(t, ready); code != http.StatusServiceUnavailable || body["status"] != "not_ready" {
		t.Fatalf("before the first probe: %d %v", code, body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx, every, nil)
	waitStatus(t, ready, http.StatusOK, every+timeout+50*time.Millisecond)

	ex.Set(func(e *fronttest.Executor) { e.Block = true })
	t0 := time.Now()
	waitStatus(t, ready, http.StatusServiceUnavailable, every+timeout+50*time.Millisecond)
	if took := time.Since(t0); took > every+timeout+20*time.Millisecond {
		t.Logf("readyz turned in %v", took)
	}
	_, body := getJSON(t, ready)
	if f, _ := body["failed"].(map[string]any); f["canary"] == nil {
		t.Fatalf("failed checks %v", body)
	}
	if code, _ := getJSON(t, live); code != http.StatusOK {
		t.Fatalf("livez %d while wedged", code)
	}
	ex.Set(func(e *fronttest.Executor) { e.Block = false })
	waitStatus(t, ready, http.StatusOK, 3*every)

	s.Drain()
	if code, body := getJSON(t, ready); code != http.StatusServiceUnavailable || body["status"] != "draining" {
		t.Fatalf("draining: %d %v", code, body)
	}
	if code, body := getJSON(t, live); code != http.StatusOK || body["draining"] != true {
		t.Fatalf("livez while draining: %d %v", code, body)
	}
}

func waitStatus(t *testing.T, h http.Handler, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		code, body := getJSON(t, h)
		if code == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %d (%v), want %d within %v", code, body, want, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReadinessProbe(t *testing.T) {
	clk := fronttest.NewClock(time.Now())
	ok := front.Check{Name: "ok", Run: func(context.Context) error { return nil }}
	tests := []struct {
		name   string
		check  front.Check
		failed string
	}{
		{"all pass", ok, ""},
		{"a check fails", front.Check{Name: "s3_breaker", Run: func(context.Context) error { return errors.New("open") }}, "s3_breaker"},
		{"a check panics", front.Check{Name: "bug", Run: func(context.Context) error { panic("boom") }}, "bug"},
		{"a check ignores ctx", front.Check{Name: "stuck", Run: func(context.Context) error { time.Sleep(time.Second); return nil }}, "probe"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := front.NewReadiness(20*time.Millisecond, time.Minute, clk.Now, ok, tc.check)
			st := r.Probe(context.Background())
			if st.Ready != (tc.failed == "") || (tc.failed != "" && st.Failed[tc.failed] == "") {
				t.Fatalf("state %+v", st)
			}
		})
	}
	// A result older than maxAge is not trusted: on point (= maxAge) still ready, off point not.
	r := front.NewReadiness(time.Second, time.Minute, clk.Now, ok)
	r.Probe(context.Background())
	clk.Advance(time.Minute)
	if !r.State().Ready {
		t.Fatal("result exactly maxAge old: not ready")
	}
	clk.Advance(time.Nanosecond)
	if st := r.State(); st.Ready || st.Failed["probe"] == "" {
		t.Fatalf("stale result: %+v", st)
	}
}

// A database expected but not open yet is unavailable (not unknown) until Add serves it.
func TestStaticHostPending(t *testing.T) {
	h := front.NewStaticHost(nil)
	h.Expect("a", "b")
	if _, err := h.Lookup(context.Background(), "a"); !errors.Is(err, front.ErrUnavailable) || h.PendingCount() != 2 {
		t.Fatalf("pending lookup: %v, count %d", err, h.PendingCount())
	}
	if _, err := h.Lookup(context.Background(), "zz"); !errors.Is(err, front.ErrUnknownDB) {
		t.Fatalf("unknown: %v", err)
	}
	if !h.Add("a", nil) || h.Pending("a") != nil || h.Pending("b") == nil || h.PendingCount() != 1 {
		t.Fatal("Add did not clear pending")
	}
}
