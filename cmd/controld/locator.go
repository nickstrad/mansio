package main

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nickstrad/ts-db-go/internal/control/locator"
	"github.com/nickstrad/ts-db-go/internal/metrics"
	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/placement/pg"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// The locator and ticket issuer inside controld (CP-5): POST /v1/resolve, GET /v1/keys, the API-key admin
// calls, the placement listener (PG-10) and the key rotation job.

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
	return locator.Placement{Intent: l.Intent, Found: l.Found, VM: l.Place.Owner, Endpoint: l.Endpoint, Epoch: uint64(l.Place.Epoch),
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
// notification invalidates one key, a lost connection turns the cache off until the next connect.
func (d *daemon) listen(ctx context.Context) {
	d.pg.Listen(ctx, pg.ListenConfig{Ping: d.cfg.Locator.ListenPing.Duration}, pg.ListenHooks{
		Ready: func() {
			d.loc.Reset()
			d.listening.Store(true)
			d.log.Info("placement listener connected; locator cache reset", "event", "LOCATOR.LISTENING")
		},
		Notify: d.loc.Invalidate,
		Lost: func(err error) {
			d.loc.Untrust()
			d.listening.Store(false)
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
			d.write(w, r, nil, &apiError{"unavailable", http.StatusServiceUnavailable, "no ticket keys yet"})
			return
		}
		d.write(w, r, f, nil)
	})
	mux.HandleFunc("POST /v1/tenants/{tenant}/api-keys", d.handle("create-api-key", func(ctx context.Context, r *http.Request) (any, error) {
		var req struct{ Principal, Perms string }
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		perms, ok := parsePerms(req.Perms)
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

func parsePerms(s string) (ticket.Perm, bool) {
	switch s {
	case "read":
		return ticket.Read, true
	case "write":
		return ticket.Write, true
	case "read|write", "":
		return ticket.Read | ticket.Write, true
	}
	return 0, false
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

// resolve is POST /v1/resolve (Authorization: Bearer <api key>; body {"tenant", "db"[, "min_epoch"]}). It runs
// under locator.timeout with its own bulkhead (locator.concurrency; more are refused at once).
func (d *daemon) resolve(w http.ResponseWriter, r *http.Request) {
	key, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !d.ready.Load() {
		d.write(w, r, nil, &apiError{"unavailable", http.StatusServiceUnavailable, "starting: the schema is not migrated yet"})
		return
	}
	select {
	case d.resolveSem <- struct{}{}:
		defer func() { <-d.resolveSem }()
	default:
		d.resolveShed.Add(1)
		d.write(w, r, nil, &apiError{"unavailable", http.StatusServiceUnavailable, "too many resolves in flight; retry"})
		return
	}
	var req struct {
		Tenant   string `json:"tenant"`
		DB       string `json:"db"`
		MinEpoch uint64 `json:"min_epoch"`
	}
	if err := decode(r, &req); err != nil {
		d.write(w, r, nil, err)
		return
	}
	if !pg.ValidTenant(req.Tenant) || !pg.ValidName(req.DB) {
		d.write(w, r, nil, errBadRequest("tenant and db must be a valid tenant id and database name"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), d.cfg.Locator.Timeout.Duration)
	defer cancel()
	rt, err := d.loc.Resolve(ctx, locator.Request{Tenant: req.Tenant, Name: req.DB, APIKey: key, MinEpoch: req.MinEpoch})
	if err != nil {
		d.write(w, r, nil, err)
		return
	}
	d.write(w, r, resolveAnswer{Code: "ok", DB: rt.DB, Tenant: rt.Tenant, VM: rt.VM, Endpoint: rt.Endpoint, Epoch: rt.Epoch, Gen: rt.Gen,
		Ticket: rt.Ticket, Perms: rt.Perms, TTLMs: rt.TTL.Milliseconds(), ExpiresAt: rt.Expires.UTC()}, nil)
}

// registerLocator adds the locator's and issuer's metrics.
func (d *daemon) registerLocator(reg *metrics.Registry) {
	counter := func(name, help string, v func(s locator.Stats) int64) {
		reg.Add(name, help, metrics.Counter, func(e *metrics.Emit) { e.Value(nil, float64(v(d.loc.Stats()))) })
	}
	counter("controld_locator_hits_total", "Resolves answered from the placement cache.", func(s locator.Stats) int64 { return s.Hits })
	counter("controld_locator_loads_total", "Placement loads started (cache misses).", func(s locator.Stats) int64 { return s.Misses })
	counter("controld_locator_coalesced_total", "Resolves that joined a load in flight (single-flight).", func(s locator.Stats) int64 { return s.Coalesced })
	counter("controld_locator_notifications_total", "Placement notifications applied.", func(s locator.Stats) int64 { return s.Notifications })
	counter("controld_locator_resets_total", "Listener connects (each resets the cache).", func(s locator.Stats) int64 { return s.Resets })
	reg.Add("controld_locator_cached", "Placements cached now.", metrics.Gauge, func(e *metrics.Emit) { e.Value(nil, float64(d.loc.Stats().Entries)) })
	reg.Add("controld_locator_listening", "1 while the placement listener is connected (the cache is in use).", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(nil, b2f(d.listening.Load()))
	})
	reg.Add("controld_locator_shed_total", "Resolves refused because locator.concurrency were in flight.", metrics.Counter, func(e *metrics.Emit) {
		e.Value(nil, float64(d.resolveShed.Load()))
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
