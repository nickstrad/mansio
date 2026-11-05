package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/vactor"
)

// The admin endpoints: runtime controls without a deploy (Release It C4, ch. 16; plan RI-16). They are served
// only on admin_listen, a third listener separate from the query and lifecycle listeners, with the lifecycle
// listener's server-wide timeouts. admin_listen must be a loopback address: binding is their only access
// control (whoever can reach a loopback port already has a shell on the VM and could signal the process).
// cmd/agentctl is the scriptable client.
//
//	POST /admin/breaker/{dep}:trip     force breaker dep open until reset   → {"code":"ok","dep","state","forced","changed"}
//	POST /admin/breaker/{dep}:reset    close breaker dep                    → same
//	POST /admin/fetch:limit?n=N        S3 fetches in flight, N >= 1          → {"code":"ok","fetch_limit","previous"}
//	POST /admin/drain                  front drain: shed new queries, /readyz and /healthz say draining (one way) → {"code":"ok","in_flight"}
//	POST /admin/passivate?key=K        write K back and drop its warm copy  → {"code":"ok","key"}
//	POST /admin/loglevel?level=L       debug | info | warn                  → {"code":"ok","level","previous"}
//
// Breakers ("s3"; "placement" once it has one) and fetch:limit and passivate exist in durable mode only; in
// static mode they answer not_found, like any unknown path. Each call runs under lifecycle_timeout (an
// X-Timeout-Ms header may shorten it), at most adminConcurrency at once (more are refused, never queued).
// Errors answer {"code","message"[,"log_id"]}: bad_request 400, not_found 404, unavailable 503 + Retry-After,
// timeout 504, internal 500. Every action is logged once at WARN (event ADMIN.*), so it survives log_level warn.

// adminConcurrency bounds admin calls in flight; a passivate may upload for a while, so a trip must not wait
// behind one.
const adminConcurrency = 4

// admin serves the admin endpoints.
type admin struct {
	log      *slog.Logger
	level    *slog.LevelVar
	srv      *front.Server
	breakers map[string]*breaker.Breaker
	host     *vactor.Host // nil in static mode
	timeout  time.Duration
	sem      chan struct{}
}

// adminHandler is the admin listener's handler.
func (a *agent) adminHandler() http.Handler {
	ad := &admin{log: a.log, level: a.level, srv: a.srv, breakers: a.be.breakers, host: a.be.host,
		timeout: a.cfg.LifecycleTimeout.Duration, sem: make(chan struct{}, adminConcurrency)}
	mux := http.NewServeMux()
	ad.routes(mux)
	return mux
}

func (ad *admin) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/breaker/{op}", ad.handle(ad.breaker))
	mux.HandleFunc("POST /admin/drain", ad.handle(ad.drain))
	mux.HandleFunc("POST /admin/loglevel", ad.handle(ad.loglevel))
	if ad.host != nil {
		mux.HandleFunc("POST /admin/fetch:limit", ad.handle(ad.fetchLimit))
		mux.HandleFunc("POST /admin/passivate", ad.handle(ad.passivate))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ad.write(w, r, nil, notFound("no admin endpoint "+r.Method+" "+r.URL.Path+" (in this mode)"))
	})
}

func notFound(msg string) error { return &controlError{"not_found", http.StatusNotFound, msg} }

// handle runs fn under a slot and the deadline, and writes its answer.
func (ad *admin) handle(fn func(ctx context.Context, r *http.Request) (map[string]any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case ad.sem <- struct{}{}:
			defer func() { <-ad.sem }()
		default:
			ad.write(w, r, nil, &controlError{"unavailable", http.StatusServiceUnavailable, "too many admin calls in flight; retry"})
			return
		}
		timeout := ad.timeout
		if ms, err := strconv.Atoi(r.Header.Get(front.TimeoutHeader)); err == nil && ms > 0 {
			timeout = min(timeout, time.Duration(ms)*time.Millisecond)
		}
		// The listener's write timeout is short (healthServer); a passivate may upload for longer.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + healthWriteTimeout))
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		out, err := fn(ctx, r)
		ad.write(w, r, out, err)
	}
}

// breaker trips or resets one named breaker: {op} is "<dep>:trip" or "<dep>:reset".
func (ad *admin) breaker(_ context.Context, r *http.Request) (map[string]any, error) {
	dep, action, _ := strings.Cut(r.PathValue("op"), ":")
	b, ok := ad.breakers[dep]
	if !ok {
		return nil, notFound(fmt.Sprintf("no breaker %q (have %s)", dep, strings.Join(slices.Sorted(maps.Keys(ad.breakers)), ", ")))
	}
	now := time.Now()
	var changed bool
	switch action {
	case "trip":
		changed = b.Trip(now)
	case "reset":
		changed = b.Reset(now)
	default:
		return nil, badRequest(fmt.Sprintf("breaker action %q: want %s:trip or %s:reset", action, dep, dep))
	}
	ad.log.Warn("admin: breaker "+action, "event", "ADMIN.BREAKER_"+strings.ToUpper(action), "dep", dep, "changed", changed,
		"remote", r.RemoteAddr)
	return map[string]any{"dep": dep, "state": b.State(now).String(), "forced": b.Forced(), "changed": changed}, nil
}

// fetchLimit sets the host's limit on S3 fetches in flight; the next fetches respect it.
func (ad *admin) fetchLimit(_ context.Context, r *http.Request) (map[string]any, error) {
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil || n < 1 {
		return nil, badRequest("n must be an integer >= 1")
	}
	prev := ad.host.FetchLimit()
	ad.host.SetFetchLimit(n)
	ad.log.Warn("admin: fetch limit set", "event", "ADMIN.FETCH_LIMIT", "fetch_limit", n, "previous", prev, "remote", r.RemoteAddr)
	return map[string]any{"fetch_limit": n, "previous": prev}, nil
}

// drain stops the front taking queries (they are shed as draining; /readyz and /healthz say draining). It is
// one way: the process keeps running until it is stopped, and its shutdown drain then finds it done.
func (ad *admin) drain(_ context.Context, r *http.Request) (map[string]any, error) {
	ad.srv.Drain()
	n := ad.srv.InFlight()
	ad.log.Warn("admin: draining", "event", "ADMIN.DRAIN", "in_flight", n, "remote", r.RemoteAddr)
	return map[string]any{"in_flight": n}, nil
}

// passivate writes one warm key back and drops its local copy (a no-op for a key that is not warm). The next
// request for it re-warms it.
func (ad *admin) passivate(ctx context.Context, r *http.Request) (map[string]any, error) {
	key := r.URL.Query().Get("key")
	if err := front.CheckDBID(key); err != nil {
		return nil, badRequest("key: " + err.Error())
	}
	if err := ad.host.Passivate(ctx, key); err != nil {
		return nil, errs.Wrap(module, err, "cannot passivate %s; it stays warm", key)
	}
	ad.log.Warn("admin: passivated", "event", "ADMIN.PASSIVATE", "key", key, "remote", r.RemoteAddr)
	return map[string]any{"key": key}, nil
}

// loglevel sets the log level.
func (ad *admin) loglevel(_ context.Context, r *http.Request) (map[string]any, error) {
	name := r.URL.Query().Get("level")
	l, ok := logLevels[name]
	if !ok {
		return nil, badRequest(fmt.Sprintf("level %q: want debug, info or warn", name))
	}
	prev := strings.ToLower(ad.level.Level().String())
	ad.level.Set(l)
	ad.log.Warn("admin: log level set", "event", "ADMIN.LOGLEVEL", "level", name, "previous", prev, "remote", r.RemoteAddr)
	return map[string]any{"level": name, "previous": prev}, nil
}

// write answers one admin call; failures are logged once (bugs at ERROR with a log ID, the rest at WARN).
func (ad *admin) write(w http.ResponseWriter, r *http.Request, out map[string]any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err == nil {
		out["code"] = "ok"
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	body := map[string]string{}
	ce := classifyControl(err)
	if ce == nil {
		id := logx.NewID()
		ad.log.Error("admin call failed (bug)", "event", "ADMIN.FAILED", "path", r.URL.Path, "log_id", id, "error", err.Error(),
			"chain", errs.Chain(err))
		ce, body["log_id"] = &controlError{"internal", http.StatusInternalServerError, "internal error"}, id
	} else {
		ad.log.Warn("admin call refused", "event", "ADMIN.REFUSED", "path", r.URL.Path, "code", ce.code, "error", err.Error())
	}
	if ce.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(err)))
	}
	body["code"], body["message"] = ce.code, ce.msg
	w.WriteHeader(ce.status)
	_ = json.NewEncoder(w).Encode(body)
}
