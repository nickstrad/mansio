package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/control"
	"github.com/nickstrad/mansio/server/internal/control/agenthttp"
	"github.com/nickstrad/mansio/server/internal/control/locator"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
)

// The control API (Authorization: Bearer <admin_token>). Every call runs under api_timeout, at most
// api_concurrency at once (more are refused 503 at once, never queued; Release It T1, B1). Answers are JSON:
// the database or VM report on success, {"code", "message"[, "log_id"]} otherwise:
//
// GET /v1/agents (Bearer <edge_token>, only when the secrets set one) answers {"code":"ok","agents":{vm: endpoint}}
// for every VM not attested dead (read from the locator's replica, if any: stale is safe).
//
//	code         | status | meaning
//	-------------+--------+---------------------------------------------------------------------------
//	bad_request  | 400    | malformed body, tenant, name or key
//	unauthorized | 401    | missing or wrong admin token; resolve: unknown, malformed or revoked api key
//	forbidden    | 403    | resolve: the api key belongs to another tenant
//	not_found    | 404    | no such database or vm
//	suspended    | 409    | resolve: the database is suspended
//	moving       | 503    | resolve: the database is moving or being created; resolve again after Retry-After
//	             |        | (the body's retry_after_ms is the precise hint: ~50 ms while a new owner's claim lands)
//	shed         | 503    | resolve: too many unknown api key ids from this source; retry after Retry-After
//	conflict     | 409    | not allowed in the database's state, a dropped name, a restore without attestation
//	no_capacity  | 503    | no live vm under the policy's caps
//	unavailable  | 503    | Postgres, S3 or an agent cannot do it now; the flow is recorded and resumes; Retry-After
//	timeout      | 504    | api_timeout passed; the flow is recorded and resumes (call again to follow it)
//	internal     | 500    | a bug; report the log_id
const maxBody = 4 << 10

// maxAgents bounds GET /v1/agents (Release It B6): more VMs than any deployment of this design.
const maxAgents = 10_000

func (d *daemon) routes(reg *metrics.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", reg.Handler())
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !d.ready.Load() {
			http.Error(w, "startup: migrating and registering agents", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	d.resolveRoutes(mux)
	d.schemaRoutes(mux)
	mux.HandleFunc("POST /v1/databases", d.handle("create", func(ctx context.Context, r *http.Request) (any, error) {
		var req struct{ Tenant, Name string }
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return d.ctl.Create(ctx, req.Tenant, req.Name)
	}))
	mux.HandleFunc("GET /v1/databases/{key}", d.handle("", func(ctx context.Context, r *http.Request) (any, error) {
		return d.ctl.Registry.GetDatabase(ctx, r.PathValue("key"))
	}))
	mux.HandleFunc("POST /v1/databases/{key}/move", d.handle("move", func(ctx context.Context, r *http.Request) (any, error) {
		var req struct{ To string }
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return d.ctl.Move(ctx, r.PathValue("key"), req.To)
	}))
	mux.HandleFunc("POST /v1/databases/{key}/suspend", d.handle("suspend", func(ctx context.Context, r *http.Request) (any, error) {
		return d.ctl.Suspend(ctx, r.PathValue("key"))
	}))
	mux.HandleFunc("POST /v1/databases/{key}/resume", d.handle("resume", func(ctx context.Context, r *http.Request) (any, error) {
		var req struct{ To string }
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return d.ctl.Resume(ctx, r.PathValue("key"), req.To)
	}))
	mux.HandleFunc("POST /v1/databases/{key}/drop", d.handle("drop", func(ctx context.Context, r *http.Request) (any, error) {
		return d.ctl.Drop(ctx, r.PathValue("key"))
	}))
	mux.HandleFunc("GET /v1/vms", d.handle("", func(ctx context.Context, r *http.Request) (any, error) {
		return d.ctl.Registry.Load(ctx, 1000)
	}))
	if d.edge != "" {
		// The edges' resolver (edge.RegistryRoutes): every VM not attested dead → its query endpoint.
		mux.HandleFunc("GET /v1/agents", d.handleAs([]string{d.edge}, "edge", "", func(ctx context.Context, r *http.Request) (any, error) {
			vms, err := d.replica.LiveVMs(ctx, maxAgents)
			if err != nil {
				return nil, err
			}
			agents := make(map[string]string, len(vms))
			for _, v := range vms {
				agents[v.VM] = v.Endpoint
			}
			return map[string]any{"code": "ok", "agents": agents}, nil
		}))
	}
	mux.HandleFunc("POST /v1/vms/{vm}/attest-dead", d.handle("attest-dead", func(ctx context.Context, r *http.Request) (any, error) {
		var req struct{ By string }
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		if req.By == "" {
			return nil, errBadRequest("by: who attests the vm is dead is required")
		}
		if err := d.ctl.AttestDead(ctx, r.PathValue("vm"), req.By); err != nil {
			return nil, err
		}
		return map[string]string{"code": "ok", "vm": r.PathValue("vm"), "attested_by": req.By}, nil
	}))
	mux.HandleFunc("POST /v1/vms/{vm}/restore", d.handle("restore", func(ctx context.Context, r *http.Request) (any, error) {
		rep, err := d.ctl.Restore(ctx, r.PathValue("vm"), d.cfg.PerKeyTimeout.Duration)
		if err != nil {
			return nil, err
		}
		failed := map[string]string{}
		for k, e := range rep.Failed {
			failed[k] = e.Error()
		}
		return map[string]any{"code": "ok", "moved": rep.Moved, "skipped": rep.Skipped, "failed": failed}, nil
	}))
	return mux
}

// apiError is an answer other than ok.
type apiError struct {
	code   string
	status int
	msg    string
	quiet  bool // an expected answer (moving during a move): not logged as a refusal
}

func (e *apiError) Error() string { return e.code + ": " + e.msg }

func errBadRequest(msg string) error {
	return &apiError{"bad_request", http.StatusBadRequest, msg, false}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errBadRequest("body must be a small JSON object: " + err.Error())
	}
	return nil
}

// handle runs fn under the admin identity, a slot and the API deadline, and writes its answer. A named
// action (not "") is logged once when it succeeds.
func (d *daemon) handle(action string, fn func(ctx context.Context, r *http.Request) (any, error)) http.HandlerFunc {
	return d.handleAs([]string{d.token}, "admin", action, fn)
}

// handleAs is handle for the holder of any of tokens (named who in the refusal). An empty token never matches.
func (d *daemon) handleAs(tokens []string, who, action string, fn func(ctx context.Context, r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !slices.ContainsFunc(tokens, func(t string) bool { return t != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(t)) == 1 }) {
			d.write(w, r, nil, &apiError{"unauthorized", http.StatusUnauthorized, who + " token required", false})
			return
		}
		d.call(w, r, d.sem, nil, d.cfg.APITimeout.Duration, func(ctx context.Context, r *http.Request) (any, error) {
			out, err := fn(ctx, r)
			if err == nil && action != "" {
				d.log.Info("control action", "event", "CONTROLD."+strings.ToUpper(strings.ReplaceAll(action, "-", "_")), "path", r.URL.Path)
			}
			return out, err
		})
	}
}

// call is every API call's pipeline after authentication: refused until startup is done, one slot of sem
// (refused at once when full, counted in shed if not nil; never queued, Release It B1), fn under timeout
// (T1), the answer written.
func (d *daemon) call(w http.ResponseWriter, r *http.Request, sem chan struct{}, shed *atomic.Int64, timeout time.Duration,
	fn func(ctx context.Context, r *http.Request) (any, error)) {
	if !d.ready.Load() {
		d.write(w, r, nil, &apiError{"unavailable", http.StatusServiceUnavailable, "starting: the schema is not migrated yet", false})
		return
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	default:
		if shed != nil {
			shed.Add(1)
		}
		d.write(w, r, nil, &apiError{"unavailable", http.StatusServiceUnavailable, "too many calls in flight; retry", false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	out, err := fn(ctx, r)
	d.write(w, r, out, err)
}

// classify maps an error to its answer; nil means a bug (internal).
func classify(err error) *apiError {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, locator.ErrUnauthorized):
		return &apiError{"unauthorized", http.StatusUnauthorized, "api key required: unknown, malformed or revoked", false}
	case errors.Is(err, locator.ErrForbidden):
		return &apiError{"forbidden", http.StatusForbidden, "the api key belongs to another tenant", false}
	case errors.Is(err, locator.ErrNotFound):
		return &apiError{"not_found", http.StatusNotFound, "no such database", false}
	case errors.Is(err, locator.ErrMoving):
		return &apiError{"moving", http.StatusServiceUnavailable, "the database is moving or being created; resolve again after Retry-After", true}
	case errors.Is(err, locator.ErrRateLimited): // counted (controld_locator_rate_limited_total), not logged: a flood
		return &apiError{"shed", http.StatusServiceUnavailable, "too many unknown api key ids from this source; retry after Retry-After", true}
	case errors.Is(err, locator.ErrSuspended):
		return &apiError{"suspended", http.StatusConflict, "the database is suspended", false}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &apiError{"timeout", http.StatusGatewayTimeout, "deadline passed; the flow is recorded and resumes; call again to follow it", false}
	case errors.Is(err, owner.ErrInvalid):
		return &apiError{"bad_request", http.StatusBadRequest, "invalid tenant, name, key or vm", false}
	case errors.Is(err, owner.ErrNotFound), errors.Is(err, pg.ErrUnknownVM):
		return &apiError{"not_found", http.StatusNotFound, "no such database or vm", false}
	case errors.Is(err, control.ErrState), errors.Is(err, control.ErrExists), errors.Is(err, control.ErrNotDead),
		errors.Is(err, owner.ErrConflict), errors.Is(err, owner.ErrMoving), errors.Is(err, owner.ErrFenced),
		errors.Is(err, owner.ErrWrongOwner), errors.Is(err, owner.ErrIllegal):
		return &apiError{"conflict", http.StatusConflict, err.Error(), false}
	case errors.Is(err, control.ErrNoCapacity):
		return &apiError{"no_capacity", http.StatusServiceUnavailable, "no live vm under the policy's caps", false}
	case errors.Is(err, pg.ErrUnavailable), errors.Is(err, agenthttp.ErrUnavailable), errors.Is(err, owner.ErrUnavailable),
		errors.Is(err, control.ErrNoAgent), objstore.Classify(err) == objstore.Transient:
		return &apiError{"unavailable", http.StatusServiceUnavailable, "a dependency cannot do it now; the flow is recorded and resumes: " + err.Error(), false}
	}
	return nil
}

func (d *daemon) write(w http.ResponseWriter, r *http.Request, out any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err == nil {
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var body struct {
		Code         string `json:"code"`
		Message      string `json:"message"`
		LogID        string `json:"log_id,omitempty"`
		RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
	}
	ae := classify(err)
	if ae == nil {
		id := logx.NewID()
		d.log.Error("control call failed (bug)", "event", "CONTROLD.FAILED", "path", r.URL.Path, "log_id", id, "error", err.Error(), "chain", errs.Chain(err))
		ae, body.LogID = &apiError{"internal", http.StatusInternalServerError, "internal error", false}, id
	} else if ae.status >= 500 && !ae.quiet {
		d.log.Warn("control call refused", "event", "CONTROLD.REFUSED", "path", r.URL.Path, "code", ae.code, "error", err.Error())
	}
	if ae.status == http.StatusServiceUnavailable {
		hint := retryHint(err)
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int((hint+time.Second-1)/time.Second))))
		body.RetryAfterMS = max(1, hint.Milliseconds())
	}
	body.Code, body.Message = ae.code, ae.msg
	w.WriteHeader(ae.status)
	_ = json.NewEncoder(w).Encode(body)
}

// retryHint is the hint in err's chain (front.RetryHint: a breaker's, the locator's moving), else 1 s, capped at
// front.MaxRetryAfter. The Retry-After header rounds it up to whole seconds (at least 1); the body's
// retry_after_ms keeps it precise, as agentd's answers do.
func retryHint(err error) time.Duration {
	var h front.RetryHint
	if !errors.As(err, &h) || h.RetryAfter() <= 0 {
		return time.Second
	}
	return min(h.RetryAfter(), front.MaxRetryAfter)
}
