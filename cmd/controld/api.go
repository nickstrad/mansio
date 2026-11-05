package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/ts-db-go/internal/control"
	"github.com/nickstrad/ts-db-go/internal/control/agenthttp"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/metrics"
	"github.com/nickstrad/ts-db-go/internal/objstore"
	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/placement/pg"
)

// The control API (Authorization: Bearer <admin_token>). Every call runs under api_timeout, at most
// api_concurrency at once (more are refused 503 at once, never queued; Release It T1, B1). Answers are JSON:
// the database or VM report on success, {"code", "message"[, "log_id"]} otherwise:
//
//	code         | status | meaning
//	-------------+--------+---------------------------------------------------------------------------
//	bad_request  | 400    | malformed body, tenant, name or key
//	unauthorized | 401    | missing or wrong admin token
//	not_found    | 404    | no such database or vm
//	conflict     | 409    | not allowed in the database's state, a dropped name, a restore without attestation
//	no_capacity  | 503    | no live vm under the policy's caps
//	unavailable  | 503    | Postgres, S3 or an agent cannot do it now; the flow is recorded and resumes; Retry-After
//	timeout      | 504    | api_timeout passed; the flow is recorded and resumes (call again to follow it)
//	internal     | 500    | a bug; report the log_id
const maxBody = 4 << 10

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
}

func (e *apiError) Error() string { return e.code + ": " + e.msg }

func errBadRequest(msg string) error { return &apiError{"bad_request", http.StatusBadRequest, msg} }

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
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(d.token)) != 1 {
			d.write(w, r, nil, &apiError{"unauthorized", http.StatusUnauthorized, "admin token required"})
			return
		}
		if !d.ready.Load() {
			d.write(w, r, nil, &apiError{"unavailable", http.StatusServiceUnavailable, "starting: the schema is not migrated yet"})
			return
		}
		select {
		case d.sem <- struct{}{}:
			defer func() { <-d.sem }()
		default:
			d.write(w, r, nil, &apiError{"unavailable", http.StatusServiceUnavailable, "too many calls in flight; retry"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), d.cfg.APITimeout.Duration)
		defer cancel()
		out, err := fn(ctx, r)
		if err == nil && action != "" {
			d.log.Info("control action", "event", "CONTROLD."+strings.ToUpper(strings.ReplaceAll(action, "-", "_")), "path", r.URL.Path)
		}
		d.write(w, r, out, err)
	}
}

// classify maps an error to its answer; nil means a bug (internal).
func classify(err error) *apiError {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &apiError{"timeout", http.StatusGatewayTimeout, "deadline passed; the flow is recorded and resumes; call again to follow it"}
	case errors.Is(err, owner.ErrInvalid):
		return &apiError{"bad_request", http.StatusBadRequest, "invalid tenant, name, key or vm"}
	case errors.Is(err, owner.ErrNotFound), errors.Is(err, pg.ErrUnknownVM):
		return &apiError{"not_found", http.StatusNotFound, "no such database or vm"}
	case errors.Is(err, control.ErrState), errors.Is(err, control.ErrExists), errors.Is(err, control.ErrNotDead),
		errors.Is(err, owner.ErrConflict), errors.Is(err, owner.ErrMoving), errors.Is(err, owner.ErrFenced),
		errors.Is(err, owner.ErrWrongOwner), errors.Is(err, owner.ErrIllegal):
		return &apiError{"conflict", http.StatusConflict, err.Error()}
	case errors.Is(err, control.ErrNoCapacity):
		return &apiError{"no_capacity", http.StatusServiceUnavailable, "no live vm under the policy's caps"}
	case errors.Is(err, pg.ErrUnavailable), errors.Is(err, agenthttp.ErrUnavailable), errors.Is(err, owner.ErrUnavailable),
		errors.Is(err, control.ErrNoAgent), objstore.Classify(err) == objstore.Transient:
		return &apiError{"unavailable", http.StatusServiceUnavailable, "a dependency cannot do it now; the flow is recorded and resumes: " + err.Error()}
	}
	return nil
}

func (d *daemon) write(w http.ResponseWriter, r *http.Request, out any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err == nil {
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	body := map[string]string{}
	ae := classify(err)
	if ae == nil {
		id := logx.NewID()
		d.log.Error("control call failed (bug)", "event", "CONTROLD.FAILED", "path", r.URL.Path, "log_id", id, "error", err.Error(), "chain", errs.Chain(err))
		ae, body["log_id"] = &apiError{"internal", http.StatusInternalServerError, "internal error"}, id
	} else if ae.status >= 500 {
		d.log.Warn("control call refused", "event", "CONTROLD.REFUSED", "path", r.URL.Path, "code", ae.code, "error", err.Error())
	}
	if ae.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(err)))
	}
	body["code"], body["message"] = ae.code, ae.msg
	w.WriteHeader(ae.status)
	_ = json.NewEncoder(w).Encode(body)
}

// retrySeconds is 1 s, raised by a breaker's hint in the chain, capped at 30 s.
func retrySeconds(err error) int {
	var h interface{ RetryAfter() time.Duration }
	if !errors.As(err, &h) {
		return 1
	}
	return max(1, min(30, int((h.RetryAfter()+time.Second-1)/time.Second)))
}
