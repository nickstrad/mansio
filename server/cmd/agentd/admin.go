package main

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
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

// admin serves the admin endpoints over the agent's own handles (a.be.breakers, a.be.host: nil in static mode).
type admin struct{ *agent }

// adminHandler is the admin listener's handler.
func (a *agent) adminHandler() http.Handler {
	ad := admin{a}
	ep := &jsonEndpoints{log: a.log, event: "ADMIN", timeout: a.cfg.LifecycleTimeout.Duration, sem: make(chan struct{}, adminConcurrency),
		logRefusal: func(*controlError) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/breaker/{op}", ep.handle(ad.breaker))
	mux.HandleFunc("POST /admin/drain", ep.handle(ad.drain))
	mux.HandleFunc("POST /admin/loglevel", ep.handle(ad.loglevel))
	if a.be.host != nil {
		mux.HandleFunc("POST /admin/fetch:limit", ep.handle(ad.fetchLimit))
		mux.HandleFunc("POST /admin/passivate", ep.handle(ad.passivate))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ep.write(w, r, nil, notFound("no admin endpoint "+r.Method+" "+r.URL.Path+" (in this mode)"))
	})
	return mux
}

func notFound(msg string) error { return &controlError{"not_found", http.StatusNotFound, msg} }

// breaker trips or resets one named breaker: {op} is "<dep>:trip" or "<dep>:reset".
func (ad *admin) breaker(_ context.Context, r *http.Request) (any, error) {
	dep, action, _ := strings.Cut(r.PathValue("op"), ":")
	b, ok := ad.be.breakers[dep]
	if !ok {
		return nil, notFound(fmt.Sprintf("no breaker %q (have %s)", dep, strings.Join(slices.Sorted(maps.Keys(ad.be.breakers)), ", ")))
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
	return map[string]any{"code": "ok", "dep": dep, "state": b.State(now).String(), "forced": b.Forced(), "changed": changed}, nil
}

// fetchLimit sets the host's limit on S3 fetches in flight; the next fetches respect it.
func (ad *admin) fetchLimit(_ context.Context, r *http.Request) (any, error) {
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil || n < 1 {
		return nil, badRequest("n must be an integer >= 1")
	}
	prev := ad.be.host.SetFetchLimit(n)
	ad.log.Warn("admin: fetch limit set", "event", "ADMIN.FETCH_LIMIT", "fetch_limit", n, "previous", prev, "remote", r.RemoteAddr)
	return map[string]any{"code": "ok", "fetch_limit": n, "previous": prev}, nil
}

// drain stops the front taking queries (they are shed as draining; /readyz and /healthz say draining). It is
// one way: the process keeps running until it is stopped, and its shutdown drain then finds it done.
func (ad *admin) drain(_ context.Context, r *http.Request) (any, error) {
	ad.srv.Drain()
	n := ad.srv.InFlight()
	ad.log.Warn("admin: draining", "event", "ADMIN.DRAIN", "in_flight", n, "remote", r.RemoteAddr)
	return map[string]any{"code": "ok", "in_flight": n}, nil
}

// passivate writes one warm key back and drops its local copy (a no-op for a key that is not warm). The next
// request for it re-warms it.
func (ad *admin) passivate(ctx context.Context, r *http.Request) (any, error) {
	key := r.URL.Query().Get("key")
	if err := front.CheckDBID(key); err != nil {
		return nil, badRequest("key: " + err.Error())
	}
	if err := ad.be.host.Passivate(ctx, key); err != nil {
		return nil, errs.Wrap(module, err, "cannot passivate %s; it stays warm", key)
	}
	ad.log.Warn("admin: passivated", "event", "ADMIN.PASSIVATE", "key", key, "remote", r.RemoteAddr)
	return map[string]any{"code": "ok", "key": key}, nil
}

// loglevel sets the log level.
func (ad *admin) loglevel(_ context.Context, r *http.Request) (any, error) {
	name := r.URL.Query().Get("level")
	l, ok := logLevels[name]
	if !ok {
		return nil, badRequest(fmt.Sprintf("level %q: want debug, info or warn", name))
	}
	prev := strings.ToLower(ad.level.Level().String())
	ad.level.Set(l)
	ad.log.Warn("admin: log level set", "event", "ADMIN.LOGLEVEL", "level", name, "previous", prev, "remote", r.RemoteAddr)
	return map[string]any{"code": "ok", "level": name, "previous": prev}, nil
}
