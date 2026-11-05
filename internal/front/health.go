package front

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/panicx"
)

// Lifecycle paths (Release It H2: liveness ≠ readiness). Serve them on the lifecycle listener, so they never
// queue behind queries (B4).
const (
	LivePath  = "/livez"  // the process is up and its lifecycle listener answers
	ReadyPath = "/readyz" // the agent can serve: startup finished, dependencies usable, the canary passes
)

// Canary marks the synthetic transaction readiness runs (Release It ch13). CanaryDB is not a valid client
// database id (CheckDBID rejects '~'), so no ticket or request can reach the canary database, and the canary
// never touches tenant data.
const (
	CanaryTenant = "~canary"
	CanaryDB     = "~canary"
	CanarySQL    = "SELECT 1"
)

// Canary runs one marked read on db through the real path behind ticket checks: admission → executor
// (→ actor). A canary that admission sheds passes: the VM is busy, not wedged, and readiness must not take an
// overloaded agent out of rotation (that only moves the load). A wedged actor shows as expired or no reply.
func (s *Server) Canary(ctx context.Context, db string, timeout time.Duration) error {
	deadline := s.clock().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	slot, err := s.adm.Acquire(ctx, CanaryTenant, db)
	if errors.Is(err, ErrShed) {
		return nil
	}
	if err != nil {
		return err
	}
	defer slot()
	_, err = s.exec.Exec(ctx, Call{Tenant: CanaryTenant, DB: db, RequestID: "canary", Kind: KindRead, SQL: CanarySQL,
		Deadline: deadline, StmtTimeout: min(s.cfg.StmtTimeout, timeout), MaxRows: 1})
	if errors.Is(err, ErrShed) {
		return nil
	}
	return err
}

// Check is one readiness condition: Run returns nil while it holds. Run must honor ctx.
type Check struct {
	Name string
	Run  func(ctx context.Context) error
}

// ReadyState is the result of the last probe.
type ReadyState struct {
	Ready  bool
	At     time.Time         // when the probe finished; zero before the first
	Failed map[string]string `json:",omitempty"` // check name → why it failed
}

// Readiness probes its checks every interval and keeps the last result, so /readyz answers from memory: a
// probe never runs per request, and a wedged check shows as a failure at its timeout, not as a hung /readyz.
type Readiness struct {
	checks  []Check
	timeout time.Duration // per probe
	maxAge  time.Duration // a result older than this is not trusted (the prober itself is stuck)
	now     Clock

	mu   sync.Mutex
	last ReadyState
}

// NewReadiness returns a prober whose result is "not ready" until its first probe. timeout bounds each probe;
// a result older than maxAge reads as not ready. now may be nil (time.Now).
func NewReadiness(timeout, maxAge time.Duration, now Clock, checks ...Check) *Readiness {
	if now == nil {
		now = time.Now
	}
	return &Readiness{checks: checks, timeout: timeout, maxAge: maxAge, now: now,
		last: ReadyState{Failed: map[string]string{"probe": "not probed yet"}}}
}

// Probe runs every check concurrently under one timeout, records the result and returns it. A check that
// panics fails (the panic is a bug in the check; it is reported as its error, not re-raised, so readiness
// keeps running).
func (r *Readiness) Probe(ctx context.Context) ReadyState {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	type result struct {
		name string
		err  error
	}
	out := make(chan result, len(r.checks))
	for _, c := range r.checks {
		go func() {
			defer func() {
				if p := panicx.From(recover()); p != nil {
					out <- result{c.Name, p}
				}
			}()
			out <- result{c.Name, c.Run(ctx)}
		}()
	}
	st := ReadyState{Ready: true}
	fail := func(name string, err error) {
		if st.Failed == nil {
			st.Failed = map[string]string{}
		}
		st.Ready, st.Failed[name] = false, err.Error()
	}
collect:
	for left := len(r.checks); left > 0; left-- {
		select {
		case res := <-out:
			if res.err != nil {
				fail(res.name, res.err)
			}
		case <-ctx.Done(): // checks ignoring ctx: fail them all; their goroutines have buffered slots
			fail("probe", fmt.Errorf("%d check(s) did not answer in time: %w", left, ctx.Err()))
			break collect
		}
	}
	st.At = r.now()
	r.mu.Lock()
	r.last = st
	r.mu.Unlock()
	return st
}

// Run probes at once and then every interval until ctx ends. Probes never overlap.
func (r *Readiness) Run(ctx context.Context, every time.Duration, report func(prev, cur ReadyState)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		prev := r.State()
		cur := r.Probe(ctx)
		if report != nil {
			report(prev, cur)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// State is the last result, marked not ready if it is older than maxAge.
func (r *Readiness) State() ReadyState {
	r.mu.Lock()
	st := r.last
	r.mu.Unlock()
	if st.Ready && r.maxAge > 0 && r.now().Sub(st.At) > r.maxAge {
		st.Ready, st.Failed = false, map[string]string{"probe": "last probe is too old; the prober is stuck"}
	}
	return st
}

// LiveHandler answers GET /livez: 200 whenever the process can answer at all, also while draining (a draining
// agent is alive; restarting it would cut the drain short).
func LiveHandler(s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, http.StatusOK, map[string]any{"status": "live", "draining": s.Draining(), "in_flight": s.InFlight()})
	})
}

// ReadyHandler answers GET /readyz: 200 when not draining and the last probe passed; otherwise 503 with the
// reason, so load balancers stop routing here.
func ReadyHandler(s *Server, r *Readiness) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		st := r.State()
		switch {
		case s.Draining():
			writeHealth(w, http.StatusServiceUnavailable, map[string]any{"status": "draining", "in_flight": s.InFlight()})
		case !st.Ready:
			writeHealth(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "failed": st.Failed, "at": st.At})
		default:
			writeHealth(w, http.StatusOK, map[string]any{"status": "ready", "at": st.At, "in_flight": s.InFlight()})
		}
	})
}

func writeHealth(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
