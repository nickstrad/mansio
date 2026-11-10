package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/agentglue"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// The control-plane endpoints (control-plane plan CP-1; option 4 §2), durable mode only, on the lifecycle
// listener so they never queue behind queries (Release It B4):
//
//	POST /v1/lifecycle/claim   {"db": "..."[, "create": true]} → {"code":"ok","db","state","epoch","gen"}
//	POST /v1/lifecycle/fence   {"db": "..."}                   → {"code":"ok","db","epoch","gen"} (the fence ack)
//	POST /v1/lifecycle/forget  {"db": "..."}                   → {"code":"ok","db"}
//	GET  /v1/lifecycle/health                                  → active databases, bytes, load
//
// Every call needs "Authorization: Bearer <control_token>" (the secrets file): control-plane identity only.
// Each runs under lifecycle_timeout, shortened by an X-Timeout-Ms header, and at most lifecycle_concurrency
// run at once; more are refused 503 at once, never queued (Release It T1, B1). Fence is idempotent; a Claim of
// the epoch already held only re-reads the authority. Errors answer {"code", "message"[, "log_id"]}:
//
//	code         | status | meaning
//	-------------+--------+-------------------------------------------------------------------------------
//	bad_request  | 400    | malformed body or database id
//	unauthorized | 401    | missing or wrong control token
//	wrong_owner  | 409    | the authority names another owner (claim), or the key is not held here (fence)
//	moving       | 409    | a move is in progress; retry after it
//	fenced       | 409    | fenced here at that epoch (claim), or superseded by a higher epoch
//	illegal      | 409    | the transition is not allowed from the key's state (e.g. forget while active)
//	unavailable  | 503    | S3, the authority or this agent cannot do it now; nothing acknowledged; Retry-After
//	timeout      | 504    | the deadline passed; for fence: admission stays stopped, call it again
//	internal     | 500    | a bug; report the log_id
//
// "create" is the single-node stand-in for the control plane's create (flow B): with no placement row, the
// agent writes ACTIVE(self, epoch 1, gen 0) first. Only with -dev (tests and single-node local runs); without
// it a claim with create answers bad_request: creating databases is the control plane's job (controld, CP-4),
// which writes the placement row itself and then calls a plain claim.
const (
	claimPath  = "/v1/lifecycle/claim"
	fencePath  = "/v1/lifecycle/fence"
	forgetPath = "/v1/lifecycle/forget"
	healthPath = "/v1/lifecycle/health"

	maxControlBody = 4 << 10
)

// control serves the lifecycle endpoints.
type control struct {
	d     *durableAgent
	srv   *front.Server
	token string
}

func (c *control) routes(mux *http.ServeMux) {
	ep := &jsonEndpoints{log: c.d.log, event: "LIFECYCLE", timeout: c.d.cfg.LifecycleTimeout.Duration, slots: c.d.lifecycle, authorize: c.authorize,
		logRefusal: func(ce *controlError) bool { return ce.status >= 500 || ce.code == "fenced" }}
	mux.HandleFunc("POST "+claimPath, ep.handle(c.claim))
	mux.HandleFunc("POST "+fencePath, ep.handle(c.fence))
	mux.HandleFunc("POST "+forgetPath, ep.handle(c.forget))
	mux.HandleFunc("GET "+healthPath, ep.handle(c.health))
}

// controlRequest is the body of claim, fence and forget.
type controlRequest struct {
	DB     string `json:"db"`
	Create bool   `json:"create"`
}

// controlError is an answer other than ok: a code, a status and a message safe to send.
type controlError struct {
	code   string
	status int
	msg    string
}

func (e *controlError) Error() string { return e.code + ": " + e.msg }

func badRequest(msg string) error { return &controlError{"bad_request", http.StatusBadRequest, msg} }

// authorize is the control-plane identity check: the bearer token from the secrets file.
func (c *control) authorize(r *http.Request) error {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if ok && subtle.ConstantTimeCompare([]byte(tok), []byte(c.token)) == 1 {
		return nil
	}
	return &controlError{"unauthorized", http.StatusUnauthorized, "control-plane token required"}
}

// decode reads the request body: a small JSON object with a valid database id.
func decode(r *http.Request) (controlRequest, error) {
	var req controlRequest
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxControlBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, badRequest("body must be a JSON object {\"db\": ...}")
	}
	if err := front.CheckDBID(req.DB); err != nil {
		return req, badRequest("db: " + err.Error())
	}
	return req, nil
}

// lifecycleOK is every ok answer of claim, fence and forget.
type lifecycleOK struct {
	Code  string `json:"code"`
	DB    string `json:"db"`
	State string `json:"state,omitempty"` // claim
	Epoch uint64 `json:"epoch,omitempty"` // claim, fence
	Gen   uint64 `json:"gen,omitempty"`   // claim, fence (0 is left out: a new database)
}

func (c *control) claim(ctx context.Context, r *http.Request) (any, error) {
	req, err := decode(r)
	if err != nil {
		return nil, err
	}
	if req.Create {
		if !c.d.cfg.Dev {
			return nil, badRequest("create is the control plane's job (controld); -dev only")
		}
		if err := c.create(ctx, req.DB); err != nil {
			return nil, err
		}
	}
	l, err := c.d.tab.Claim(ctx, req.DB)
	if err != nil {
		return nil, err
	}
	return lifecycleOK{"ok", req.DB, l.State.String(), uint64(l.Epoch), uint64(l.Gen)}, nil
}

// create writes ACTIVE(self, 1, 0) if the key has no placement row (the single-node stand-in for flow B).
func (c *control) create(ctx context.Context, key string) error {
	_, err := c.d.place.Get(ctx, key)
	if !errors.Is(err, owner.ErrNotFound) {
		return err // a row exists (nil), or the authority is unreadable
	}
	err = c.d.place.CAS(ctx, key, owner.Place{}, owner.Place{Owner: c.d.cfg.VM, Epoch: 1, State: owner.PlaceActive})
	if errors.Is(err, owner.ErrConflict) {
		return nil // created meanwhile; Claim reads whatever is there
	}
	return err
}

// fence hands the key off (owner.Table.Fence) and then drops the warm copy: the fence published every write
// it admitted, and admits nothing more. A superseded key is dropped too: its writes must not survive.
func (c *control) fence(ctx context.Context, r *http.Request) (any, error) {
	req, err := decode(r)
	if err != nil {
		return nil, err
	}
	ack, err := agentglue.Lifecycle{Tab: c.d.tab, Host: c.d.host}.Fence(ctx, req.DB)
	if err != nil {
		return nil, err
	}
	return lifecycleOK{Code: "ok", DB: ack.Key, Epoch: uint64(ack.Epoch), Gen: uint64(ack.Gen)}, nil
}

func (c *control) forget(ctx context.Context, r *http.Request) (any, error) {
	req, err := decode(r)
	if err != nil {
		return nil, err
	}
	if err := c.d.tab.Forget(ctx, req.DB); err != nil {
		return nil, err
	}
	return lifecycleOK{Code: "ok", DB: req.DB}, nil
}

// healthReport is what the control plane's placement policy reads: databases held, bytes, load.
type healthReport struct {
	Code   string         `json:"code"`
	VM     string         `json:"vm"`
	Owned  map[string]int `json:"owned"`  // keys by local ownership state
	Active int            `json:"active"` // databases warm (or activating) in the host
	Bytes  int64          `json:"bytes"`  // bytes in the cache directory (warm copies and the generation cache)
	Load   struct {
		InFlight       int     `json:"in_flight"`
		AdmissionQueue int     `json:"admission_queued"`
		FetchQueued    int     `json:"fetch_queued"`
		OldestDirtyAge float64 `json:"oldest_dirty_age_seconds"`
	} `json:"load"`
	S3Breaker string `json:"s3_breaker"`
}

func (c *control) health(ctx context.Context, _ *http.Request) (any, error) {
	d := c.d
	g := d.host.Gauges()
	h := healthReport{Code: "ok", VM: d.cfg.VM, Owned: map[string]int{}, Active: g.Active, S3Breaker: d.s3b.State(time.Now()).String()}
	for _, k := range d.tab.Keys() {
		h.Owned[d.tab.State(k).State.String()]++
	}
	n, err := dirBytes(ctx, d.cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	h.Bytes = n
	h.Load.InFlight = int(c.srv.InFlight())
	h.Load.AdmissionQueue = c.srv.Admission().Stats().Queued
	h.Load.FetchQueued = g.FetchQueued
	h.Load.OldestDirtyAge = d.ticks.lastTick().OldestDirtyAge.Seconds()
	return h, nil
}

// dirBytes sums the sizes of the files under dir, stopping when ctx ends.
func dirBytes(ctx context.Context, dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil // a file passivation removed meanwhile
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if fi, ierr := e.Info(); ierr == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	if err != nil {
		return 0, errs.Wrap(module, err, "cannot size the cache directory in time")
	}
	return n, nil
}

// classifyControl maps a lifecycle error to its answer. nil means a bug (internal).
func classifyControl(err error) *controlError {
	var ce *controlError
	switch {
	case errors.As(err, &ce):
		return ce
	case errors.Is(err, owner.ErrInvalid):
		return &controlError{"bad_request", http.StatusBadRequest, "invalid database key"}
	case errors.Is(err, owner.ErrSuperseded), errors.Is(err, owner.ErrFenced):
		return &controlError{"fenced", http.StatusConflict, "fenced here; a higher epoch or a completed fence holds it"}
	case errors.Is(err, owner.ErrMoving):
		return &controlError{"moving", http.StatusConflict, "a move is in progress; retry after it"}
	case errors.Is(err, owner.ErrWrongOwner):
		return &controlError{"wrong_owner", http.StatusConflict, "not owned by this agent at that epoch"}
	case errors.Is(err, owner.ErrIllegal):
		return &controlError{"illegal", http.StatusConflict, "not allowed in the key's current state"}
	case errors.Is(err, owner.ErrUnavailable), errors.Is(err, durable.ErrUnavailable), errors.Is(err, vactor.ErrBusy),
		errors.Is(err, vactor.ErrInDoubt), errors.Is(err, vactor.ErrClosed), errors.Is(err, pg.ErrUnavailable):
		return &controlError{"unavailable", http.StatusServiceUnavailable, "a dependency cannot do it now; nothing acknowledged, retry later"}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &controlError{"timeout", http.StatusGatewayTimeout, "deadline passed; nothing acknowledged, safe to call again"}
	}
	return nil
}

// retrySeconds is the Retry-After for an unavailable answer: 1 s, raised by a hint in the chain (an open
// breaker's time to half-open), capped at front.MaxRetryAfter.
func retrySeconds(err error) int {
	var h front.RetryHint
	if !errors.As(err, &h) {
		return 1
	}
	d := min(h.RetryAfter(), front.MaxRetryAfter)
	return max(1, int((d+time.Second-1)/time.Second))
}
