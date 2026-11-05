package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/metrics"
	"github.com/nickstrad/ts-db-go/internal/panicx"
)

const module = "edge"

// Config wires an Edge. Durations follow the agent's inbound sheet (Release It §P) so the edge never cuts a
// request off before the agent would.
type Config struct {
	Resolver Resolver
	Gate     *Gate
	Limiter  *SourceLimiter
	// Transport carries requests to the agents: one per process (it pools connections), TLS to the agents'
	// private CA outside development. It must bound dialing and the TLS handshake; each request's ctx bounds
	// the rest. The edge never retries (Release It R1: the client is the one retry layer).
	Transport http.RoundTripper
	// VerifySlots bounds full ticket verifies running at once (default 2 × GOMAXPROCS). A miss that finds no
	// free slot is shed at once, so a many-source flood costs at most this much CPU (Release It B1, B2).
	VerifySlots int
	// AgentInFlight bounds requests in flight to one agent (default 4096; at least the agent's vm_in_flight +
	// vm_queue so the agent, not the edge, decides admission). A full bulkhead sheds.
	AgentInFlight int
	// Breaker is each agent's breaker (zero value: Release It §P). Only transport failures count: an agent
	// that answers, whatever the code, is reachable (an agent's own S3 outage must not stop its warm traffic,
	// Release It C5). OnBreaker, if set, gives each agent's breaker its OnChange (log transitions).
	Breaker   breaker.Config
	OnBreaker func(vm string) func(breaker.Transition)

	DefaultTimeout  time.Duration // deadline without X-Timeout-Ms (agent default_timeout)
	MaxTimeout      time.Duration // longest deadline (agent max_timeout)
	Grace           time.Duration // time past the deadline the agent's own expired answer gets to arrive
	BodyReadTimeout time.Duration // the whole request body
	MaxBody         int           // request body bytes (agent limits.max_body)

	Log *slog.Logger     // ERROR lines for bugs (with a log ID); default: discard
	Now func() time.Time // default time.Now
}

// Edge is the proxy: an http.Handler for POST /v1/query.
type Edge struct {
	cfg    Config
	verify chan struct{} // verify slots

	mu  sync.RWMutex
	ups map[string]*upstream // by VM

	draining atomic.Bool
	st       counters
	latency  *metrics.LatencyHistogram
}

type upstream struct {
	vm, base, url string
	b             *breaker.Breaker
	slots         chan struct{}
}

// counters are the edge's request stats; all atomics (the request path takes no lock for them).
type counters struct {
	answers                                      [len(front.Codes)]atomic.Int64 // answers the edge made, by code
	agent                                        [6]atomic.Int64                // agent answers by status/100
	hits, misses, invalid, limited, verifyBusy   atomic.Int64
	inFlight, inFlightHigh                       atomic.Int64
	transportErrs, unknownVM, bulkheadFull, open atomic.Int64
}

// New validates cfg and returns the edge. No I/O.
func New(cfg Config) (*Edge, error) {
	if cfg.Resolver == nil || cfg.Gate == nil || cfg.Limiter == nil || cfg.Transport == nil {
		return nil, errs.Wrap(module, nil, "edge: resolver, gate, limiter and transport are required")
	}
	if cfg.DefaultTimeout <= 0 || cfg.MaxTimeout < cfg.DefaultTimeout || cfg.Grace <= 0 || cfg.BodyReadTimeout <= 0 || cfg.MaxBody <= 0 {
		return nil, errs.Wrap(module, nil, "edge: need 0 < default_timeout <= max_timeout, grace, body_read_timeout and max_body > 0")
	}
	if cfg.VerifySlots <= 0 {
		cfg.VerifySlots = 2 * runtime.GOMAXPROCS(0)
	}
	if cfg.AgentInFlight <= 0 {
		cfg.AgentInFlight = 4096
	}
	if err := cfg.Breaker.Normalize(); err != nil {
		return nil, errs.Wrap(module, err, "edge: invalid agent breaker config")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Edge{cfg: cfg, verify: make(chan struct{}, cfg.VerifySlots), ups: map[string]*upstream{},
		latency: metrics.NewLatency(nil)}, nil
}

// Drain makes the edge shed new requests (and /readyz fail, see cmd/edge); in-flight requests finish.
func (e *Edge) Drain() { e.draining.Store(true) }

// Draining reports whether Drain was called.
func (e *Edge) Draining() bool { return e.draining.Load() }

// InFlight is the number of requests being handled.
func (e *Edge) InFlight() int64 { return e.st.inFlight.Load() }

// ServeHTTP handles one request: every path except POST /v1/query is answered here.
func (e *Edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := e.cfg.Now()
	n := e.st.inFlight.Add(1)
	for h := e.st.inFlightHigh.Load(); n > h && !e.st.inFlightHigh.CompareAndSwap(h, n); h = e.st.inFlightHigh.Load() {
	}
	defer func() {
		e.st.inFlight.Add(-1)
		e.latency.Observe(e.cfg.Now().Sub(start))
	}()
	defer func() { // a bug: answer internal with a log ID if we still can
		if p := panicx.From(recover()); p != nil {
			id := logx.NewID()
			e.cfg.Log.Error("request failed (internal)", "event", "EDGE.BUG", "log_id", id, "error", p.Error(), "stack", p.Stack)
			e.answer(w, answer{code: front.Internal, logID: id})
		}
	}()
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(start.Add(e.cfg.BodyReadTimeout)) // errors: the writer cannot (a test recorder)
	rc.SetWriteDeadline(start.Add(e.cfg.BodyReadTimeout + e.cfg.Grace))
	if a, ok := e.handle(w, r, rc, start); !ok {
		e.answer(w, a)
	}
}

// answer is a response the edge makes itself.
type answer struct {
	code      front.Code
	status    int           // 0 = front.Spec[code].Status
	hint      time.Duration // raises the Retry-After floor (front.RetryHint semantics)
	msg       string
	ambiguous bool
	logID     string
}

// handle runs the pipeline. ok means the agent's answer was relayed; otherwise a is the edge's own answer.
func (e *Edge) handle(w http.ResponseWriter, r *http.Request, rc *http.ResponseController, start time.Time) (a answer, ok bool) {
	switch {
	case r.URL.Path != front.QueryPath:
		return answer{code: front.BadRequest, status: http.StatusNotFound, msg: "unknown path"}, false
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", http.MethodPost)
		return answer{code: front.BadRequest, status: http.StatusMethodNotAllowed, msg: "use POST"}, false
	case e.draining.Load():
		return answer{code: front.Shed, msg: "edge draining; nothing ran"}, false
	}
	timeout := e.cfg.DefaultTimeout
	if v := r.Header.Get(front.TimeoutHeader); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ms <= 0 || ms > int64(time.Hour/time.Millisecond) {
			return answer{code: front.BadRequest, msg: front.TimeoutHeader + " must be a positive integer number of milliseconds"}, false
		}
		timeout = min(time.Duration(ms)*time.Millisecond, e.cfg.MaxTimeout)
	}
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		e.st.invalid.Add(1)
		return answer{code: front.TicketInvalid, msg: "no bearer ticket"}, false
	}

	// The ticket: a cached hit is free; a miss is charged to its source, then needs a free verify slot.
	c, hit, err := e.cfg.Gate.Lookup(token, start)
	if hit {
		e.st.hits.Add(1)
	} else {
		e.st.misses.Add(1)
		if ok, wait := e.cfg.Limiter.Allow(SourceKey(r.RemoteAddr), start); !ok {
			e.st.limited.Add(1)
			return answer{code: front.Shed, hint: wait, msg: "too many new tickets from this source; nothing ran"}, false
		}
		select {
		case e.verify <- struct{}{}:
		default:
			e.st.verifyBusy.Add(1)
			return answer{code: front.Shed, msg: "edge busy verifying tickets; nothing ran"}, false
		}
		c, err = e.cfg.Gate.Verify(token, start)
		<-e.verify
	}
	if err != nil {
		e.st.invalid.Add(1)
		return answer{code: front.TicketInvalid}, false
	}

	up, found := e.upstream(c.VM)
	if !found {
		e.st.unknownVM.Add(1)
		return answer{code: front.WrongOwner, msg: "the ticket's vm is not served by this edge; re-resolve"}, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(e.cfg.MaxBody)))
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			return answer{code: front.BadRequest, status: http.StatusRequestEntityTooLarge, msg: "body too large"}, false
		case errors.Is(err, os.ErrDeadlineExceeded):
			return answer{code: front.BadRequest, status: http.StatusRequestTimeout, msg: "body not received in time"}, false
		}
		return answer{code: front.BadRequest, msg: "cannot read body"}, false
	}
	return e.forward(w, r, rc, up, body, start.Add(timeout))
}

// forward sends the request to up under its bulkhead and breaker, and relays the answer.
func (e *Edge) forward(w http.ResponseWriter, r *http.Request, rc *http.ResponseController, up *upstream, body []byte,
	deadline time.Time) (answer, bool) {
	select {
	case up.slots <- struct{}{}:
		defer func() { <-up.slots }()
	default:
		e.st.bulkheadFull.Add(1)
		return answer{code: front.Shed, msg: "too many requests in flight to this agent; nothing ran"}, false
	}
	now := e.cfg.Now()
	p, wait, ok := up.b.Allow(now)
	if !ok {
		e.st.open.Add(1)
		return answer{code: front.Unavailable, hint: wait, msg: "agent unreachable (breaker open); nothing ran"}, false
	}
	left := deadline.Sub(now)
	if left <= 0 {
		up.b.Done(p, now, breaker.Ignore)
		return answer{code: front.Expired, msg: "deadline passed at the edge; nothing ran"}, false
	}
	// The agent answers expired at the deadline; Grace lets that answer arrive. Past it the agent is stuck.
	ctx, cancel := context.WithDeadline(r.Context(), deadline.Add(e.cfg.Grace))
	defer cancel()
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) }})
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, up.url, bytes.NewReader(body))
	if err != nil { // the URL was validated; checked before asserting so the request path does not box arguments
		assert.That(false, "edge: cannot build the agent request for %q: %v", up.url, err)
	}
	out.Header.Set("Authorization", r.Header.Get("Authorization"))
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set(front.TimeoutHeader, strconv.FormatInt(max(int64((left+time.Millisecond-1)/time.Millisecond), 1), 10))
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		out.Header.Set("X-Forwarded-For", host)
	}
	rc.SetWriteDeadline(deadline.Add(2 * e.cfg.Grace))

	res, err := e.cfg.Transport.RoundTrip(out)
	if err != nil {
		switch {
		case r.Context().Err() != nil: // the client left: says nothing about the agent
			up.b.Done(p, e.cfg.Now(), breaker.Ignore)
		default:
			up.b.Done(p, e.cfg.Now(), breaker.Failure)
		}
		e.st.transportErrs.Add(1)
		if wrote.Load() { // the agent may have run it
			return answer{code: front.Expired, ambiguous: true, msg: "no answer from the agent; a write may have committed: retry with the same request_id"}, false
		}
		return answer{code: front.Unavailable, msg: "agent unreachable; nothing ran"}, false
	}
	defer res.Body.Close()
	up.b.Done(p, e.cfg.Now(), breaker.Success)
	e.st.agent[min(res.StatusCode/100, 5)].Add(1)
	h := w.Header()
	for _, k := range relayHeaders {
		if v := res.Header.Values(k); len(v) > 0 {
			h[k] = v
		}
	}
	w.WriteHeader(res.StatusCode)
	io.Copy(w, res.Body) // an error means the client or the agent left mid-body; nothing more to say
	return answer{}, true
}

// relayHeaders are the agent response headers the edge passes on (an allowlist: nothing else of the agent's
// leaks to clients).
var relayHeaders = []string{"Content-Type", "Retry-After", "Www-Authenticate", "Cache-Control"}

// upstream returns vm's agent, creating its breaker and bulkhead on first use or when its address changed.
func (e *Edge) upstream(vm string) (*upstream, bool) {
	base, ok := e.cfg.Resolver.Agent(vm)
	e.mu.RLock()
	u := e.ups[vm]
	e.mu.RUnlock()
	if ok && u != nil && u.base == base {
		return u, true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !ok {
		delete(e.ups, vm) // the resolver forgot it: so do we (bounded by the resolver's VMs)
		return nil, false
	}
	if u = e.ups[vm]; u != nil && u.base == base {
		return u, true
	}
	bc := e.cfg.Breaker
	if e.cfg.OnBreaker != nil {
		bc.OnChange = e.cfg.OnBreaker(vm)
	}
	b, err := breaker.New(bc)
	assert.That(err == nil, "edge: breaker config was normalized in New: %v", err)
	u = &upstream{vm: vm, base: base, url: strings.TrimSuffix(base, "/") + front.QueryPath, b: b,
		slots: make(chan struct{}, e.cfg.AgentInFlight)}
	e.ups[vm] = u
	return u, true
}

// Breaker returns vm's breaker if the edge has sent it traffic (admin, tests).
func (e *Edge) Breaker(vm string) (*breaker.Breaker, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	u, ok := e.ups[vm]
	if !ok {
		return nil, false
	}
	return u.b, true
}

// answer writes an edge-made answer in the agent's wire format (front.Response), with Retry-After for the codes
// that carry one: max(the Spec floor, the hint), capped at front.MaxRetryAfter.
func (e *Edge) answer(w http.ResponseWriter, a answer) {
	idx := codeIndex(a.code)
	e.st.answers[idx].Add(1)
	spec := front.Spec[a.code]
	status := spec.Status
	if a.status != 0 {
		status = a.status
	}
	retry := spec.RetryAfter
	if retry > 0 && a.hint > 0 {
		retry = max(retry, int((min(a.hint, front.MaxRetryAfter)+time.Second-1)/time.Second))
	}
	msg := a.msg
	if msg == "" {
		msg = defaultMessage[a.code]
	}
	b, _ := json.Marshal(front.Response{Code: a.code, Message: msg, Ambiguous: a.ambiguous, RetryAfterMS: retry * 1000, LogID: a.logID})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if retry > 0 {
		h.Set("Retry-After", strconv.Itoa(retry))
	}
	if status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", `Bearer realm="ts-db edge"`)
	}
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

var defaultMessage = map[front.Code]string{
	front.TicketInvalid: "ticket invalid",
	front.Internal:      "internal error",
}

// codeIndex is c's position in front.Codes.
func codeIndex(c front.Code) int {
	for i, k := range front.Codes {
		if k == c {
			return i
		}
	}
	assert.That(false, "edge: unknown code %q", c)
	return len(front.Codes) - 1
}

// bearer extracts the token from "Bearer <token>"; anything else is "" (ticket_invalid). Same rule as the agent.
func bearer(v string) string {
	const p = "Bearer "
	if len(v) > len(p) && strings.EqualFold(v[:len(p)], p) {
		return strings.TrimSpace(v[len(p):])
	}
	return ""
}
