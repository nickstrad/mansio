package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/outcome"
	"github.com/nickstrad/ts-db-go/internal/retry"
	"github.com/nickstrad/ts-db-go/internal/tlsx"
)

const module = "client"

// Defaults (Release It §P "Client").
const (
	DefaultTimeout        = 5 * time.Second  // a call's deadline when its ctx has none
	DefaultResolveTimeout = 3 * time.Second  // one resolve (the locator's own deadline)
	DefaultDialTimeout    = 3 * time.Second  // TCP connect
	DefaultHeaderTimeout  = 60 * time.Second // backstop only: every call's ctx deadline ends it first
	DefaultMaxReroutes    = 5                // re-resolves in one call (a move costs ~2: fenced, then moving until the claim lands)
	DefaultMaxRoutes      = 10_000           // cached databases
	DefaultRefreshAhead   = 0.8              // refresh at 80% of the ticket TTL …
	DefaultRefreshJitter  = 0.1              // … ±10% of the TTL
	DefaultBudgetRatio    = 0.1              // retries per first attempt
	MaxResponse           = 32 << 20         // bytes of one answer body
)

// Config configures a Client. Locator, APIKey and Tenant are required; zero fields take the defaults above.
type Config struct {
	Locator string // controld's base URL, e.g. http://10.0.0.2:7080
	APIKey  string // tsdb_<id>_<secret> (CP-5)
	Tenant  string // every database this client names belongs to this tenant (an API key covers one tenant)

	// Scheme is used for endpoints the locator returns as host:port: "http" (default) or "https" (TLS agents
	// or an edge). Endpoints that already carry a scheme are used as they are.
	Scheme string
	// CAFile is a PEM CA to trust for https endpoints and the locator (a private CA, e.g. the edge's); empty =
	// system roots. Ignored when HTTP is set.
	CAFile string
	// HTTP overrides the transport (tests, custom TLS). It must have dial and response-header timeouts.
	HTTP *http.Client

	Retry  retry.Policy  // normalized by New; the zero Policy is the safe default
	Budget *retry.Budget // shared by every call (and may be shared across clients); nil = Ratio DefaultBudgetRatio

	Timeout        time.Duration // a call's deadline when its ctx has none
	ResolveTimeout time.Duration // one resolve, run under its own deadline (single-flight)
	MaxReroutes    int           // re-resolves per call before giving up
	MaxRoutes      int           // routes cached
	RefreshAhead   float64       // background refresh at this fraction of the ticket TTL …
	RefreshJitter  float64       // … ± this fraction, so clients do not refresh together

	Hooks Hooks
}

// Hooks observe the client (benchmarks, tests). They run on the calling goroutine and must be cheap and safe for
// concurrent use. Nil hooks are skipped.
type Hooks struct {
	Resolve func(ResolveEvent) // every route lookup
	Retry   func(RetryEvent)   // every wait before another attempt
}

// ResolveEvent is one route lookup in a call.
type ResolveEvent struct {
	DB   string
	Hit  bool          // served from the cache without waiting
	Wait time.Duration // time spent getting the route (a miss includes the resolve call)
	Err  error
}

// RetryEvent is one decision to try again.
type RetryEvent struct {
	DB      string
	Attempt int           // the attempt that just finished (1 = first)
	Code    string        // what it answered: a wire code, a locator code ("moving", …), or "transport"
	Reroute bool          // the route is re-resolved before the next attempt
	Hint    time.Duration // the server's Retry-After (0 = none)
	Delay   time.Duration // the wait before the next attempt (>= Hint)
}

// Request is one statement for one database.
type Request struct {
	DB   string // database name within Config.Tenant
	Kind front.Kind
	SQL  string
	Args []any // nil, int64, float64, string, []byte (sent as a blob), or front.Blob
	// RequestID makes a write apply at most once (CP-2). Empty: the client makes a unique one. Every attempt of
	// the call sends the same id, so a retried write that already applied answers ok + duplicate.
	RequestID string
}

// Result is a call's final answer.
type Result struct {
	front.Response        // the last answer from an agent (Code "" if none arrived)
	Attempts       int    // requests sent to agents
	Reroutes       int    // re-resolves after wrong_owner, fenced, ticket_invalid, no answer or moving
	VM             string // the VM of the last route used
	Epoch          uint64
}

// Error is a call that did not end ok. Op says where it ended; Code is the wire code (Op "query"), the locator's
// code (Op "resolve"), "transport" (no answer), or "deadline" (the call's deadline passed between attempts).
type Error struct {
	Op      string
	Code    string
	Message string
	// MayHaveApplied: a write whose outcome is unknown (no answer, internal, an ambiguous expiry). Retrying it
	// with the same RequestID is safe.
	MayHaveApplied bool
	Err            error // the transport or context error, if any
}

func (e *Error) Error() string {
	s := e.Op + " " + e.Code
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf is "ok" for nil, else the final code of a call's error ("resolve:<code>" when the resolve failed).
func CodeOf(err error) string {
	var e *Error
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &e) && e.Op == "resolve":
		return "resolve:" + e.Code
	case errors.As(err, &e):
		return e.Code
	}
	return "error"
}

// Client sends statements to the VM that owns each database, as the locator says. Safe for concurrent use.
type Client struct {
	cfg    Config
	http   *http.Client
	routes *routes
	budget *retry.Budget
	idBase string
	idSeq  atomic.Uint64
	stop   context.CancelFunc

	rng *mrand.Rand // over the runtime's lock-free generator: safe for concurrent use

	sleep func(ctx context.Context, d time.Duration) error // tests replace it
}

// New validates cfg and returns a client. It does no I/O.
func New(cfg Config) (*Client, error) {
	if cfg.Locator == "" || cfg.APIKey == "" || cfg.Tenant == "" {
		return nil, errs.Wrap(module, nil, "locator, api key and tenant are required")
	}
	if cfg.Scheme == "" {
		cfg.Scheme = "http"
	}
	if cfg.Scheme != "http" && cfg.Scheme != "https" {
		return nil, errs.Wrap(module, nil, "scheme %q: want http or https", cfg.Scheme)
	}
	if err := cfg.Retry.Normalize(); err != nil {
		return nil, errs.Wrap(module, err, "invalid retry policy")
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.Timeout, DefaultTimeout)
	def(&cfg.ResolveTimeout, DefaultResolveTimeout)
	if cfg.MaxReroutes <= 0 {
		cfg.MaxReroutes = DefaultMaxReroutes
	}
	if cfg.MaxRoutes <= 0 {
		cfg.MaxRoutes = DefaultMaxRoutes
	}
	if cfg.RefreshAhead <= 0 {
		cfg.RefreshAhead = DefaultRefreshAhead
	}
	if cfg.RefreshJitter <= 0 {
		cfg.RefreshJitter = DefaultRefreshJitter
	}
	if cfg.RefreshAhead-cfg.RefreshJitter <= 0 || cfg.RefreshAhead+cfg.RefreshJitter >= 1 {
		return nil, errs.Wrap(module, nil, "refresh ahead %g ± jitter %g must stay inside (0, 1) of the TTL", cfg.RefreshAhead, cfg.RefreshJitter)
	}
	hc := cfg.HTTP
	if hc == nil {
		var err error
		if hc, err = NewHTTPClient(cfg.CAFile); err != nil {
			return nil, err
		}
	}
	b := make([]byte, 6)
	rand.Read(b)
	c := &Client{cfg: cfg, http: hc, budget: cfg.Budget, idBase: "c" + hex.EncodeToString(b) + "-",
		rng: mrand.New(globalSource{}), sleep: sleepCtx}
	if c.budget == nil {
		c.budget = &retry.Budget{Ratio: DefaultBudgetRatio}
	}
	root, stop := context.WithCancel(context.Background())
	c.stop = stop
	c.routes = newRoutes(root, c.resolve, cfg.ResolveTimeout, cfg.MaxRoutes, time.Now, func(at time.Time, ttl time.Duration) time.Time {
		return refreshAt(at, ttl, cfg.RefreshAhead, cfg.RefreshJitter, c.rng.Float64())
	})
	return c, nil
}

// NewHTTPClient is the default transport: dial and TLS handshake timeouts, a response-header backstop, pooled
// connections (HTTP/2 over TLS when the server offers it), and caFile's CA if given.
func NewHTTPClient(caFile string) (*http.Client, error) {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: DefaultDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   DefaultDialTimeout,
		ResponseHeaderTimeout: DefaultHeaderTimeout,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if caFile != "" {
		cfg, err := tlsx.ClientConfig(caFile)
		if err != nil {
			return nil, errs.Wrap(module, err, "cannot load the CA file").With("path", caFile)
		}
		tr.TLSClientConfig = cfg
	}
	return &http.Client{Transport: tr}, nil
}

// Close stops background resolves and closes idle connections. Calls in progress end with their contexts.
func (c *Client) Close() {
	c.stop()
	c.http.CloseIdleConnections()
}

// Budget is the retry budget the client spends.
func (c *Client) Budget() *retry.Budget { return c.budget }

// globalSource draws from math/rand/v2's per-thread generator, so one *rand.Rand over it is safe to share.
type globalSource struct{}

func (globalSource) Uint64() uint64 { return mrand.Uint64() }

// Do runs req at the database's owner and returns the final answer. The loop (control-plane plan §2):
//
//	route (cached; resolved single-flight) → POST /v1/query with the route's ticket and X-Timeout-Ms
//	ok → done · wrong_owner / fenced → drop the route, re-resolve with min_epoch = epoch+1 (after Retry-After)
//	ticket_invalid / no answer → drop the route, re-resolve · shed / unavailable / aborted → the retry policy
//	(wait max(Retry-After, jittered backoff), spend the budget) · anything else → fail
//
// The call never waits past its deadline (ctx's, or Config.Timeout). Re-resolves are bounded by MaxReroutes and
// back off from the second one; they do not spend the retry budget (they redirect load, they do not repeat it).
// Errors are *Error (errors.As), wrapped once.
func (c *Client) Do(ctx context.Context, req Request) (Result, error) {
	var res Result
	if req.Kind != front.KindRead && req.Kind != front.KindWrite {
		return res, errs.Wrap(module, &Error{Op: "query", Code: "bad_request", Message: "kind must be read or write"}, "invalid request; nothing was sent")
	}
	if req.DB == "" {
		return res, errs.Wrap(module, &Error{Op: "query", Code: "bad_request", Message: "db is required"}, "invalid request; nothing was sent")
	}
	if req.Kind == front.KindWrite && req.RequestID == "" {
		req.RequestID = c.idBase + strconv.FormatUint(c.idSeq.Add(1), 36)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()
	first := time.Now()
	c.budget.First()

	var (
		body    []byte
		prev    time.Duration // the previous wait
		retries int           // same-route retries and resolve retries (policy attempts)
		last    *Error
	)
	fail := func(e *Error) (Result, error) {
		return res, errs.Wrap(module, e, "%s %s/%s ended %s after %d attempts and %d re-resolves", req.Kind, c.cfg.Tenant, req.DB,
			e.Code, res.Attempts, res.Reroutes).With("request_id", req.RequestID)
	}
	for {
		// 1. The route.
		t0 := time.Now()
		r, hit, err := c.routes.get(ctx, req.DB) // a reroute's min_epoch lives in the cache (drop)
		if h := c.cfg.Hooks.Resolve; h != nil {
			h(ResolveEvent{DB: req.DB, Hit: hit, Wait: time.Since(t0), Err: err})
		}
		var a answer
		var st step
		var oc outcome.Code
		if err != nil {
			var re *resolveError
			switch {
			case ctx.Err() != nil:
				return fail(c.deadlineError(ctx, last))
			case errors.As(err, &re):
				a, last = re.a, &Error{Op: "resolve", Code: re.a.Code, Message: re.msg, Err: re.err}
			default:
				return res, errs.Wrap(module, err, "resolve %s/%s failed unexpectedly", c.cfg.Tenant, req.DB) // a bug
			}
			st, oc = resolveStep(a)
			if st == stepFail {
				return fail(last)
			}
		} else {
			res.VM, res.Epoch = r.VM, r.Epoch
			if body == nil {
				if body, err = encode(r.DB, req); err != nil {
					return res, errs.Wrap(module, &Error{Op: "query", Code: "bad_request", Message: err.Error()}, "cannot encode the request; nothing was sent")
				}
			}
			// 2. The attempt.
			var resp front.Response
			res.Attempts++
			a, resp, err = c.send(ctx, r, body, deadline)
			if ctx.Err() != nil {
				return fail(c.deadlineError(ctx, last))
			}
			if a.Code != codeTransport {
				res.Response = resp
			}
			st, oc = decide(a)
			if st == stepDone {
				return res, nil
			}
			last = &Error{Op: "query", Code: a.Code, Message: resp.Message, Err: err,
				MayHaveApplied: req.Kind == front.KindWrite && (a.Code == codeTransport || a.Ambiguous || a.Code == string(front.Internal))}
			if st == stepFail {
				return fail(last)
			}
			if st == stepReroute {
				c.routes.drop(req.DB, r.Epoch, minEpochAfter(a, r.Epoch))
			}
		}

		// 3. The wait.
		var d time.Duration
		switch st {
		case stepReroute: // wrong_owner, fenced, ticket_invalid, no answer, the locator's moving
			if res.Reroutes >= c.cfg.MaxReroutes {
				return fail(last)
			}
			res.Reroutes++
			d = a.RetryAfter
			if res.Reroutes > 1 {
				d = max(d, c.cfg.Retry.Delay(c.rng, res.Reroutes, prev))
			}
		case stepRetry:
			var ok bool
			d, ok = c.cfg.Retry.Decide(c.rng, retry.Attempt{N: retries + 1, Outcome: oc, Elapsed: time.Since(first), Prev: prev, ServerHint: a.RetryAfter})
			if !ok || !c.budget.TryRetry() {
				return fail(last)
			}
			retries++
		}
		if !time.Now().Add(d).Before(deadline) {
			return fail(last) // the wait would end past the call's deadline (RI T4)
		}
		if h := c.cfg.Hooks.Retry; h != nil {
			h(RetryEvent{DB: req.DB, Attempt: res.Attempts, Code: last.Code, Reroute: st == stepReroute, Hint: a.RetryAfter, Delay: d})
		}
		if err := c.sleep(ctx, d); err != nil {
			return fail(c.deadlineError(ctx, last))
		}
		prev = d
	}
}

// deadlineError is the error for a call whose context ended; it keeps whether a write may have applied.
func (c *Client) deadlineError(ctx context.Context, last *Error) *Error {
	e := &Error{Op: "query", Code: "deadline", Message: "the call's deadline passed", Err: ctx.Err()}
	if last != nil {
		e.Message += "; last answer " + last.Code
		e.MayHaveApplied = last.MayHaveApplied
	}
	return e
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// encode is the query body for database key db. It is built once per call: every attempt sends the same bytes,
// so the same request_id.
func encode(db string, req Request) ([]byte, error) {
	args := front.WireArgs(slices.Clone(req.Args))
	return json.Marshal(struct {
		DB        string     `json:"db"`
		RequestID string     `json:"request_id,omitempty"`
		Kind      front.Kind `json:"kind"`
		SQL       string     `json:"sql"`
		Args      []any      `json:"args"`
	}{db, req.RequestID, req.Kind, req.SQL, args})
}

// timeoutHeader is the X-Timeout-Ms to send with remaining time left: a little less than remaining, so the
// agent answers expired before this client stops listening (RI T4: inner deadlines are shorter).
//
//	post: 1 <= result, and result ms <= remaining when remaining >= 1 ms
func timeoutHeader(remaining time.Duration) int64 {
	ms := (remaining - min(remaining/10, 100*time.Millisecond)).Milliseconds()
	return max(ms, 1)
}

// send is one attempt at route r. A transport failure (no answer) returns codeTransport and the error.
func (c *Client) send(ctx context.Context, r Route, body []byte, deadline time.Time) (answer, front.Response, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, r.queryURL, bytes.NewReader(body))
	if err != nil {
		return answer{Code: codeTransport}, front.Response{}, err
	}
	hr.Header.Set("Authorization", r.auth)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set(front.TimeoutHeader, strconv.FormatInt(timeoutHeader(time.Until(deadline)), 10))
	hres, err := c.http.Do(hr)
	if err != nil {
		return answer{Code: codeTransport}, front.Response{}, err
	}
	defer drain(hres.Body)
	var resp front.Response
	if err := json.NewDecoder(io.LimitReader(hres.Body, MaxResponse)).Decode(&resp); err != nil || resp.Code == "" {
		if err == nil {
			err = fmt.Errorf("HTTP %d answer without a code", hres.StatusCode)
		}
		return answer{Code: codeTransport}, front.Response{}, fmt.Errorf("unreadable answer: %w", err)
	}
	return answer{Code: string(resp.Code), Ambiguous: resp.Ambiguous, RetryAfter: retryAfter(hres.Header.Get("Retry-After"), resp.RetryAfterMS)}, resp, nil
}

// drain reads what a decoder left (agentd's trailing newline, a chunk terminator) so the connection is reused,
// then closes the body.
func drain(b io.ReadCloser) {
	io.Copy(io.Discard, io.LimitReader(b, 4<<10))
	b.Close()
}

func (c *Client) url(endpoint string) string {
	if strings.Contains(endpoint, "://") {
		return strings.TrimSuffix(endpoint, "/")
	}
	return c.cfg.Scheme + "://" + endpoint
}

// resolveError is a resolve that did not return a route: a locator answer other than ok, or no answer.
type resolveError struct {
	a   answer
	msg string
	err error
}

func (e *resolveError) Error() string {
	if e.err != nil {
		return "resolve: " + e.a.Code + ": " + e.err.Error()
	}
	return "resolve: " + e.a.Code + ": " + e.msg
}

func (e *resolveError) Unwrap() error { return e.err }

// resolve is POST /v1/resolve (CP-5). It runs in the route cache's goroutine under ctx's deadline.
func (c *Client) resolve(ctx context.Context, db string, minEpoch uint64) (Route, error) {
	b, _ := json.Marshal(struct {
		Tenant   string `json:"tenant"`
		DB       string `json:"db"`
		MinEpoch uint64 `json:"min_epoch,omitempty"`
	}{c.cfg.Tenant, db, minEpoch})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.cfg.Locator, "/")+"/v1/resolve", bytes.NewReader(b))
	if err != nil {
		return Route{}, &resolveError{a: answer{Code: "bad_request"}, msg: "bad locator URL", err: err}
	}
	hr.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	hr.Header.Set("Content-Type", "application/json")
	hres, err := c.http.Do(hr)
	if err != nil {
		return Route{}, &resolveError{a: answer{Code: codeTransport}, err: err}
	}
	defer drain(hres.Body)
	var out struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		RetryMS  int    `json:"retry_after_ms"`
		DB       string `json:"db"`
		VM       string `json:"vm"`
		Endpoint string `json:"endpoint"`
		Epoch    uint64 `json:"epoch"`
		Ticket   string `json:"ticket"`
		TTLMs    int64  `json:"ttl_ms"`
	}
	if err := json.NewDecoder(io.LimitReader(hres.Body, 1<<20)).Decode(&out); err != nil || out.Code == "" {
		if err == nil {
			err = fmt.Errorf("HTTP %d answer without a code", hres.StatusCode)
		}
		return Route{}, &resolveError{a: answer{Code: codeTransport}, err: fmt.Errorf("unreadable locator answer: %w", err)}
	}
	if out.Code != "ok" {
		return Route{}, &resolveError{a: answer{Code: out.Code, RetryAfter: retryAfter(hres.Header.Get("Retry-After"), out.RetryMS)}, msg: out.Message}
	}
	if out.Endpoint == "" || out.Ticket == "" || out.TTLMs <= 0 || out.DB == "" {
		return Route{}, &resolveError{a: answer{Code: codeTransport}, err: errors.New("locator answer ok without endpoint, ticket, db or ttl")}
	}
	return Route{DB: out.DB, VM: out.VM, Endpoint: out.Endpoint, Epoch: out.Epoch, Ticket: out.Ticket,
		TTL: time.Duration(out.TTLMs) * time.Millisecond, queryURL: c.url(out.Endpoint) + front.QueryPath, auth: "Bearer " + out.Ticket}, nil
}
