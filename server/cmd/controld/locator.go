package main

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/control/locator"
	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// The locator and ticket issuer inside controld: POST /v1/resolve, GET /v1/keys, the API-key admin
// calls, the placement listener (Postgres LISTEN/NOTIFY invalidates the cache) and the key rotation job.

// locStore adapts the Postgres adapter to locator.Store. Placement reads go to the replica unless the
// locator asks for the primary; API keys always read the primary (a key created a moment ago must work).
type locStore struct{ primary, replica *pg.Placement }

var _ locator.Store = locStore{}

func (s locStore) Locate(ctx context.Context, key string, primary bool) (locator.Placement, error) {
	p := s.replica
	if primary {
		p = s.primary
	}
	l, err := p.Locate(ctx, key)
	return locator.Placement{Intent: l.Intent, Target: l.Target, Found: l.Found, VM: l.Place.Owner, Endpoint: l.Endpoint, Epoch: uint64(l.Place.Epoch),
		Gen: uint64(l.Place.Gen), Moving: l.Place.State == owner.PlaceMoving}, err
}

func (s locStore) APIKey(ctx context.Context, id string) (locator.StoredKey, error) {
	k, err := s.primary.GetAPIKey(ctx, id)
	if errors.Is(err, owner.ErrNotFound) {
		return locator.StoredKey{}, locator.ErrNotFound
	}
	return locator.StoredKey{Tenant: k.Tenant, Principal: k.Principal, Perms: ticket.Perm(k.Perms), Hash: k.Hash, Revoked: k.Revoked}, err
}

// listen runs the placement listener until ctx ends: each (re)connect resets the locator's cache, each
// notification (a placement change, or a registry state flip that ends a claim window) invalidates one key, a
// lost connection turns the cache off until the next connect.
func (d *daemon) listen(ctx context.Context) {
	d.pg.Listen(ctx, pg.ListenConfig{Ping: d.cfg.Locator.ListenPing.Duration}, pg.ListenHooks{
		Ready: func() {
			d.loc.Reset()
			d.log.Info("placement listener connected; locator cache reset", "event", "LOCATOR.LISTENING")
		},
		Notify: d.loc.Invalidate,
		State:  d.loc.InvalidateState,
		Lost: func(err error) {
			d.loc.Untrust()
			d.log.Warn("placement listener lost; locator cache off until it reconnects", "event", "LOCATOR.LISTEN_LOST", "error", err.Error())
		},
	})
}

// rotateKeys re-plans the ticket key ring; failures keep the current ring and are retried next check.
func (d *daemon) rotateKeys(ctx context.Context) {
	created, removed, err := d.keys.rotate(time.Now())
	switch {
	case err != nil:
		d.log.Warn("ticket key rotation failed; the current ring stays", "event", "ISSUER.ROTATE_FAILED", "error", err.Error())
	case created != "" || len(removed) > 0:
		d.log.Info("ticket key ring rotated", "event", "ISSUER.ROTATED", "created", created, "removed", removed, "signing", d.signer.KID())
	}
}

// resolveRoutes adds the locator's endpoints to mux.
func (d *daemon) resolveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/resolve", d.resolve)
	mux.HandleFunc("GET /v1/keys", func(w http.ResponseWriter, r *http.Request) {
		f := d.signer.Public()
		if len(f.Keys) == 0 {
			d.write(w, r, nil, &apiError{"unavailable", http.StatusServiceUnavailable, "no ticket keys yet", false})
			return
		}
		d.write(w, r, f, nil)
	})
	mux.HandleFunc("POST /v1/tenants/{tenant}/api-keys", d.handle("create-api-key", func(ctx context.Context, r *http.Request) (any, error) {
		var req struct{ Principal, Perms string }
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		if req.Perms == "" {
			req.Perms = (ticket.Read | ticket.Write).String()
		}
		perms, ok := ticket.ParsePerm(req.Perms)
		if !ok {
			return nil, errBadRequest(`perms must be "read", "write" or "read|write"`)
		}
		key, id, hash, err := locator.NewAPIKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		tenant := r.PathValue("tenant")
		if err := d.pg.CreateAPIKey(ctx, pg.APIKey{ID: id, Tenant: tenant, Principal: req.Principal, Perms: uint8(perms), Hash: hash}); err != nil {
			return nil, err
		}
		return map[string]string{"code": "ok", "id": id, "key": key, "tenant": tenant, "principal": req.Principal, "perms": perms.String()}, nil
	}))
	mux.HandleFunc("POST /v1/api-keys/{id}/revoke", d.handle("revoke-api-key", func(ctx context.Context, r *http.Request) (any, error) {
		if err := d.pg.RevokeAPIKey(ctx, r.PathValue("id")); err != nil {
			return nil, err
		}
		return map[string]string{"code": "ok", "id": r.PathValue("id")}, nil
	}))
}

// resolveAnswer is the resolve API's success body.
type resolveAnswer struct {
	Code      string    `json:"code"`
	DB        string    `json:"db"`
	Tenant    string    `json:"tenant"`
	VM        string    `json:"vm"`
	Endpoint  string    `json:"endpoint"`
	Epoch     uint64    `json:"epoch"`
	Gen       uint64    `json:"gen"`
	Ticket    string    `json:"ticket"`
	Perms     string    `json:"perms"`
	TTLMs     int64     `json:"ttl_ms"`
	ExpiresAt time.Time `json:"expires_at"`
}

// resolve is POST /v1/resolve (Authorization: Bearer <api key>; body {"tenant", "db"[, "min_epoch"]}, or
// {["tenant",] "app"[, "create"][, "min_epoch"]} for an app's database tenant:app, ADR 0011). It runs under
// locator.timeout with its own bulkhead (locator.concurrency): resolves never queue behind admin calls. With
// "create": true a missing app database is created on first use when the key may write, the app has a
// registered schema (schemas.go) and the caps allow (tenancy.go); the answer may then be moving + Retry-After
// while its owner claims it. An app resolve may omit "tenant" (the credential's), and with a "tenancy"
// section may carry any credential tenancy accepts (tenancy.go); a "tenant" other than the credential's is
// forbidden.
func (d *daemon) resolve(w http.ResponseWriter, r *http.Request) {
	key, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	d.call(w, r, d.resolves, d.cfg.Locator.Timeout.Duration, func(ctx context.Context, r *http.Request) (any, error) {
		var req struct {
			Tenant   string `json:"tenant"`
			DB       string `json:"db"`
			App      string `json:"app"`
			Create   bool   `json:"create"`
			MinEpoch uint64 `json:"min_epoch"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		if (req.DB == "") == (req.App == "") || req.Create && req.App == "" {
			return nil, errBadRequest(`give exactly one of "db" and "app"; "create" goes with "app"`)
		}
		name, app := req.DB+req.App, req.App != ""
		if !pg.ValidTenant(req.Tenant) && !(app && req.Tenant == "") || !pg.ValidName(name) {
			return nil, errBadRequest("tenant (optional with app) and db (or app) must be a valid tenant id and database name")
		}
		if app && !d.appAllowed(name) {
			return nil, &apiError{"not_found", http.StatusNotFound, "no such database: app " + name + " is not in tenancy.apps", false}
		}
		rt, err := d.resolveApp(ctx, locator.Request{Tenant: req.Tenant, Name: name, App: app, APIKey: key, MinEpoch: req.MinEpoch,
			Source: r.RemoteAddr, Create: req.Create})
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &apiError{"timeout", http.StatusGatewayTimeout, "resolve deadline passed; retry with backoff", false}
		}
		if err != nil {
			return nil, err
		}
		return resolveAnswer{Code: "ok", DB: rt.DB, Tenant: rt.Tenant, VM: rt.VM, Endpoint: rt.Endpoint, Epoch: rt.Epoch, Gen: rt.Gen,
			Ticket: rt.Ticket, Perms: rt.Perms, TTLMs: rt.TTL.Milliseconds(), ExpiresAt: rt.Expires.UTC()}, nil
	})
}

// registerLocator adds the locator's and issuer's metrics.
func (d *daemon) registerLocator(reg *metrics.Registry) {
	var st locator.Stats
	reg.OnScrape(func() { st = d.loc.Stats() }) // one lock per scrape: the resolve path takes it too
	for _, m := range []struct {
		name, help string
		v          func() int64
	}{
		{"controld_locator_hits_total", "Resolves answered from the placement cache.", func() int64 { return st.Hits }},
		{"controld_locator_loads_total", "Placement loads started (cache misses).", func() int64 { return st.Misses }},
		{"controld_locator_coalesced_total", "Resolves that joined a load in flight (single-flight).", func() int64 { return st.Coalesced }},
		{"controld_locator_notifications_total", "Placement notifications applied.", func() int64 { return st.Notifications }},
		{"controld_locator_resets_total", "Listener connects (each resets the cache).", func() int64 { return st.Resets }},
		{"controld_locator_shed_total", "Resolves refused because locator.concurrency were in flight.", d.resolves.Shed},
		{"controld_locator_rate_limited_total", "Resolves refused: an uncached api key id from a source out of tokens.", func() int64 { return st.RateLimited }},
	} {
		reg.Add(m.name, m.help, metrics.Counter, func(e *metrics.Emit) { e.Value(nil, float64(m.v())) })
	}
	reg.Add("controld_locator_cached", "Placements cached now.", metrics.Gauge, func(e *metrics.Emit) { e.Value(nil, float64(st.Entries)) })
	reg.Add("controld_locator_listening", "1 while the placement listener is connected (the cache is in use).", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(nil, b2f(st.Trusted))
	})
	reg.Add("controld_issuer_published_keys", "Ticket public keys published to agents.", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(nil, float64(len(d.signer.Public().Keys)))
	})
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
