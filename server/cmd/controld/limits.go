package main

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/nickstrad/mansio/server/internal/slots"
)

// The runtime levers (Backpressure in Microservices ch. 4 "Incident Actions": they must exist before the
// incident). POST /v1/limits/{queue} {"limit": N} sets, from now on and until the process restarts, the limit
// of one of the slot pools that stand in front of Postgres and the agents:
//
//	resolve_load     the locator's store reads and the agents' schema reads (locator.load_concurrency)
//	resolve_create   creates on first use (locator.create_concurrency)
//	controld_api     the admin API (api_concurrency)
//	tenancy_admin, tenancy_auth   the tenant admin and sign-in pools (with a tenancy section)
//
// Lower one when its dependency struggles (refusals answer at once); raise it back after. The pool budget
// still holds: load + create + api_concurrency must stay below pg.max_conns.

// limitPools are the pools the lever may change, by their metrics queue name.
func (d *daemon) limitPools() map[string]*slots.Pool {
	pools := map[string]*slots.Pool{"resolve_load": d.loads, "resolve_create": d.creates, "controld_api": d.api}
	if t := d.tenancyPools; t.Admin != nil {
		pools["tenancy_admin"], pools["tenancy_auth"] = t.Admin, t.Auth
	}
	return pools
}

func (d *daemon) limitRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/limits/{queue}", d.handle("limit", func(_ context.Context, r *http.Request) (any, error) {
		var req struct{ Limit int }
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		pools := d.limitPools()
		queue := r.PathValue("queue")
		pool := pools[queue]
		if pool == nil {
			return nil, &apiError{"not_found", http.StatusNotFound,
				fmt.Sprintf("no slot pool %q; have %s", queue, strings.Join(slices.Sorted(maps.Keys(pools)), ", ")), false}
		}
		if req.Limit < 1 {
			return nil, errBadRequest("limit must be an integer >= 1")
		}
		budgeted := pool == d.loads || pool == d.creates || pool == d.api
		if used := d.loads.Limit() + d.creates.Limit() + d.api.Limit() - pool.Limit() + req.Limit; budgeted && used >= int(d.cfg.PG.MaxConns) {
			return nil, errBadRequest(fmt.Sprintf("with %s at %d the callers of the pool would total %d, not below pg.max_conns %d", queue, req.Limit,
				used, d.cfg.PG.MaxConns))
		}
		prev := pool.SetLimit(req.Limit)
		d.log.Warn("limit set", "event", "CONTROLD.LIMIT_SET", "queue", queue, "limit", req.Limit, "previous", prev, "remote", r.RemoteAddr)
		return map[string]any{"code": "ok", "queue": queue, "limit": req.Limit, "previous": prev}, nil
	}))
}
