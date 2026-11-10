// Package tenancyhttp is package tenancy's HTTP adapter (IR-4a, ADR 0012): the admin API scripts/tenants.ts
// speaks (/admin/tenants…, plus POST /admin/users/link for scripts/auth.ts users link) and Better Auth's
// email/password endpoints (/api/auth/sign-up/email, /api/auth/sign-in/email), with the Deno platform's
// shapes, so the CLIs and clients work against either backend.
//
// Every /admin/ path needs Authorization: Bearer <admin token> (constant-time), checked before routing: an
// unknown admin path answers 401 to a wrong token and 404 to the right one. Answers are {"result": …} or
// {"error": {"tag", "message"}} (/admin), and Better Auth's {token, user} or {code, message} (/api/auth).
// Each call runs under Timeout, at most Concurrency at once (more are refused 503 at once, never queued;
// Release It T1, B1); sign-in and sign-up also spend a per-source token (ratelimit.SourceLimiter: 429).
package tenancyhttp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/ratelimit"
	"github.com/nickstrad/mansio/server/internal/tenancy"
)

// Config sets up the routes. Zero fields take defaults.
type Config struct {
	AdminToken  string        // required, at least 16 bytes
	Timeout     time.Duration // one call; default 10 s (a sign-in waits for a hash slot)
	Concurrency int           // admin calls at once, and sign-in/up calls at once (separate slots); default 16 each
	// SignInRate and SignInBurst bound sign-ins and sign-ups per source (client IP, IPv6 /64); default 1/s,
	// burst 10.
	SignInRate, SignInBurst float64
	Log                     *slog.Logger // required
	Shed                    *Shed        // counts refusals for lack of a slot; nil: not counted
}

// DefaultConcurrency is Config.Concurrency's default.
const DefaultConcurrency = 16

// Shed counts the calls refused because every slot of their pool was taken.
type Shed struct{ Admin, Auth atomic.Int64 }

// Queues is the two slot pools for metrics.Queues; concurrency is Config.Concurrency (0: the default).
func (s *Shed) Queues(concurrency int) []metrics.QueueStat {
	if concurrency == 0 {
		concurrency = DefaultConcurrency
	}
	return []metrics.QueueStat{
		{Queue: "tenancy_admin", Capacity: concurrency, Rejected: map[string]int64{metrics.ReasonFull: s.Admin.Load()}},
		{Queue: "tenancy_auth", Capacity: concurrency, Rejected: map[string]int64{metrics.ReasonFull: s.Auth.Load()}},
	}
}

// maxBody caps every request body (Release It B6).
const maxBody = 4 << 10

// writeGrace is how long past a call's Timeout its answer may take to write.
const writeGrace = 2 * time.Second

type routes struct {
	svc      *tenancy.Service
	cfg      Config
	adminSem chan struct{} // admin calls; sign-in/up have their own slots (authSem), so neither starves the other
	authSem  chan struct{}
	limiter  *ratelimit.SourceLimiter
}

// Mount registers "/admin/" and, in session mode, "/api/auth/" on mux. Without a store (off mode) the admin
// gate still answers, with no routes behind it.
func Mount(mux *http.ServeMux, svc *tenancy.Service, cfg Config) error {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.Shed == nil {
		cfg.Shed = &Shed{}
	}
	if cfg.SignInRate == 0 {
		cfg.SignInRate, cfg.SignInBurst = 1, 10
	}
	switch {
	case len(cfg.AdminToken) < 16:
		return errs.Wrap("tenancy", tenancy.ErrInvalid, "the admin token must be at least 16 bytes")
	case cfg.Log == nil || cfg.Timeout < 0 || cfg.Concurrency < 1 || cfg.SignInRate < 0 || cfg.SignInBurst < 1:
		return errs.Wrap("tenancy", tenancy.ErrInvalid, "tenancyhttp: log required; timeout >= 0, concurrency >= 1, sign-in burst >= 1")
	}
	rt := &routes{svc: svc, cfg: cfg, adminSem: make(chan struct{}, cfg.Concurrency), authSem: make(chan struct{}, cfg.Concurrency),
		limiter: ratelimit.NewSourceLimiter(cfg.SignInRate, cfg.SignInBurst, 100_000)}
	admin := http.NewServeMux()
	if svc.Mode() != tenancy.Off {
		rt.adminRoutes(admin)
	}
	admin.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		adminError(w, http.StatusNotFound, "RouteNotFound", "no such admin route")
	})
	mux.Handle("/admin/", rt.gate(admin))
	if svc.Mode() == tenancy.Sessions {
		mux.HandleFunc("POST /api/auth/sign-up/email", rt.call(rt.signUp))
		mux.HandleFunc("POST /api/auth/sign-in/email", rt.call(rt.signIn))
	}
	return nil
}

// gate checks the admin token before anything else.
func (rt *routes) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, reason := bearer(r.Header.Get("Authorization"))
		if reason == "" && subtle.ConstantTimeCompare([]byte(tok), []byte(rt.cfg.AdminToken)) != 1 {
			reason = tenancy.ReasonMismatch
		}
		if reason != "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mansio admin"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"tag": "AuthError",
				"message": "AuthError: scope=\"admin\" reason=\"" + reason + "\""}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearer reads "Bearer <token>": the token, or why there is none.
func bearer(h string) (string, string) {
	const p = "Bearer " // the scheme is case-insensitive, as on /v1/ops (front.bearer)
	switch {
	case h == "":
		return "", tenancy.ReasonMissing
	case len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) || strings.TrimSpace(h[len(p):]) == "":
		return "", tenancy.ReasonMalformed
	default:
		return strings.TrimSpace(h[len(p):]), ""
	}
}

func (rt *routes) adminRoutes(mux *http.ServeMux) {
	svc := rt.svc
	mux.HandleFunc("GET /admin/tenants", rt.admin("", func(ctx context.Context, r *http.Request) (any, error) {
		ts, err := svc.ListTenants(ctx)
		out := make([]tenantJSON, len(ts))
		for i, t := range ts {
			out[i] = toTenant(t)
		}
		return out, err
	}))
	mux.HandleFunc("POST /admin/tenants", rt.admin("", func(ctx context.Context, r *http.Request) (any, error) {
		var body struct{ Name, Slug string }
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		t, k, err := svc.CreateTenant(ctx, body.Name, body.Slug)
		if err != nil {
			return nil, err
		}
		return map[string]any{"tenant": toTenant(t), "apiKey": toIssued(k)}, nil
	}))
	mux.HandleFunc("GET /admin/tenants/{id}", rt.admin("UnknownTenant", func(ctx context.Context, r *http.Request) (any, error) {
		t, err := svc.GetTenant(ctx, r.PathValue("id"))
		return toTenant(t), err
	}))
	mux.HandleFunc("DELETE /admin/tenants/{id}", rt.admin("UnknownTenant", func(ctx context.Context, r *http.Request) (any, error) {
		t, err := svc.DeactivateTenant(ctx, r.PathValue("id"))
		return toTenant(t), err
	}))
	mux.HandleFunc("POST /admin/tenants/{id}/keys", rt.admin("UnknownTenant", func(ctx context.Context, r *http.Request) (any, error) {
		var body struct{ Name string }
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		k, err := svc.IssueKey(ctx, r.PathValue("id"), body.Name)
		return toIssued(k), err
	}))
	mux.HandleFunc("DELETE /admin/tenants/{id}/keys/{key}", rt.admin("UnknownTenant", func(ctx context.Context, r *http.Request) (any, error) {
		return map[string]bool{"revoked": true}, svc.RevokeKey(ctx, r.PathValue("id"), r.PathValue("key"))
	}))
	if svc.Mode() == tenancy.Sessions {
		mux.HandleFunc("POST /admin/users/link", rt.admin("NotFoundError", func(ctx context.Context, r *http.Request) (any, error) {
			var body struct {
				Email    string `json:"email"`
				TenantID string `json:"tenantId"`
			}
			if err := decode(r, &body); err != nil {
				return nil, err
			}
			return map[string]bool{"linked": true}, svc.Link(ctx, body.Email, body.TenantID)
		}))
	}
}

type tenantJSON struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

func toTenant(t tenancy.Tenant) tenantJSON {
	status := "deactivated"
	if t.Active {
		status = "active"
	}
	return tenantJSON{ID: t.ID, Name: t.Name, Slug: t.Slug, Status: status, CreatedAt: t.Created.UTC()}
}

func toIssued(k tenancy.IssuedKey) map[string]any {
	return map[string]any{"id": k.ID, "tenantId": k.Tenant, "name": k.Name, "prefix": "mansio_",
		"createdAt": k.Created.UTC(), "plaintext": k.Plaintext}
}

// decode reads a JSON object body (at most maxBody, unknown fields refused) into v.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body) // bounded and already in memory: prepare read it
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errs.Wrap("tenancy", tenancy.ErrInvalid, "the body must be a JSON object with only the documented fields")
	}
	return nil
}

// prepare runs before a call takes a slot, so a client that sends slowly holds none: it reads the whole body
// (at most maxBody; the rest is ignored) into memory, under whatever read deadline the listener set, and
// allows the answer to be written for the call's Timeout plus writeGrace. It reports false if the body could
// not be read; the caller answers 400.
func (rt *routes) prepare(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(rt.cfg.Timeout + writeGrace)) // error: a test recorder
	return true
}

// enter takes a slot of sem, or reports false (all taken: refuse, never queue).
func enter(sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// admin wraps an admin handler: slot, deadline, the {"result"} / {"error"} answer. notFound is the tag of
// ErrNotFound.
func (rt *routes) admin(notFound string, fn func(ctx context.Context, r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !rt.prepare(w, r) {
			adminError(w, http.StatusBadRequest, "SchemaError", "the body could not be read in time")
			return
		}
		if !enter(rt.adminSem) {
			rt.cfg.Shed.Admin.Add(1)
			w.Header().Set("Retry-After", "1")
			adminError(w, http.StatusServiceUnavailable, "shed", "too many admin calls at once; retry")
			return
		}
		defer func() { <-rt.adminSem }()
		ctx, cancel := context.WithTimeout(r.Context(), rt.cfg.Timeout)
		defer cancel()
		out, err := fn(ctx, r)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"result": out})
			return
		}
		status, tag := rt.classify(r, err, notFound)
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "1")
		}
		adminError(w, status, tag, tag)
	}
}

// classify maps an error to a status and tag, logging what needs it (Release It F2: unavailable WARN, bug ERROR
// with a log ID in the answer's message).
func (rt *routes) classify(r *http.Request, err error, notFound string) (int, string) {
	switch {
	case errors.Is(err, tenancy.ErrUnauthenticated):
		return http.StatusUnauthorized, "AuthError"
	case errors.Is(err, tenancy.ErrInvalid):
		return http.StatusBadRequest, "SchemaError"
	case errors.Is(err, tenancy.ErrNotFound):
		if notFound == "" {
			notFound = "NotFoundError"
		}
		return http.StatusNotFound, notFound
	case errors.Is(err, tenancy.ErrConflict):
		return http.StatusConflict, "ConflictError"
	case errors.Is(err, tenancy.ErrDeactivated):
		return http.StatusForbidden, "TenantDeactivated"
	case errors.Is(err, tenancy.ErrUnavailable):
		rt.cfg.Log.Warn("tenancy store unavailable", "event", "TENANCY.UNAVAILABLE", "path", r.URL.Path, "error", err.Error())
		return http.StatusServiceUnavailable, "unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "expired"
	}
	id := logx.NewID()
	rt.cfg.Log.Error("tenancy call failed (internal)", "log_id", id, "path", r.URL.Path, "error", err.Error(), "chain", errs.Chain(err))
	return http.StatusInternalServerError, "Defect"
}

func adminError(w http.ResponseWriter, status int, tag, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"tag": tag, "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) // an error means the client left
}

// Better Auth's endpoints.

type userJSON struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	Image         *string   `json:"image"`
	EmailVerified bool      `json:"emailVerified"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func toUser(u tenancy.User) userJSON {
	return userJSON{ID: u.ID, Email: u.Email, Name: u.Name, EmailVerified: u.Verified, CreatedAt: u.Created.UTC(), UpdatedAt: u.Updated.UTC()}
}

// authBody is what sign-up and sign-in read; Better Auth's other fields (image, callbackURL, rememberMe) are
// accepted and ignored.
type authBody struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (rt *routes) signUp(ctx context.Context, b authBody) (any, error) {
	u, token, err := rt.svc.SignUp(ctx, b.Name, b.Email, b.Password)
	return map[string]any{"token": token, "user": toUser(u)}, err
}

func (rt *routes) signIn(ctx context.Context, b authBody) (any, error) {
	u, token, err := rt.svc.SignIn(ctx, b.Email, b.Password)
	return map[string]any{"redirect": false, "token": token, "user": toUser(u)}, err
}

// call wraps a Better Auth endpoint: per-source rate limit, slot, deadline, lenient JSON body, {code, message}
// errors.
func (rt *routes) call(fn func(ctx context.Context, b authBody) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := rt.limiter.Allow(ratelimit.SourceKey(r.RemoteAddr), time.Now()); !ok {
			secs := strconv.Itoa(int(wait/time.Second) + 1)
			w.Header().Set("Retry-After", secs)
			w.Header().Set("X-Retry-After", secs)
			authError(w, http.StatusTooManyRequests, "TOO_MANY_REQUESTS", "Too many requests. Please try again later.")
			return
		}
		if !rt.prepare(w, r) {
			authError(w, http.StatusBadRequest, "VALIDATION_ERROR", "The body could not be read in time.")
			return
		}
		if !enter(rt.authSem) {
			rt.cfg.Shed.Auth.Add(1)
			w.Header().Set("Retry-After", "1")
			authError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "Too many requests at once; retry.")
			return
		}
		defer func() { <-rt.authSem }()
		ctx, cancel := context.WithTimeout(r.Context(), rt.cfg.Timeout)
		defer cancel()
		var b authBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			authError(w, http.StatusBadRequest, "VALIDATION_ERROR", "The body must be a JSON object.")
			return
		}
		out, err := fn(ctx, b)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, out)
		case errors.Is(err, tenancy.ErrUnauthenticated):
			authError(w, http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD", "Invalid email or password")
		case errors.Is(err, tenancy.ErrInvalid):
			authError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid name, email or password (8 to 128 characters).")
		case errors.Is(err, tenancy.ErrConflict):
			authError(w, http.StatusUnprocessableEntity, "USER_ALREADY_EXISTS", "User already exists. Use another email.")
		default:
			status, tag := rt.classify(r, err, "")
			if status == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", "1")
			}
			authError(w, status, strings.ToUpper(tag), tag)
		}
	}
}

func authError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}
