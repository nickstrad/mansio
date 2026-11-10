package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/nickstrad/mansio/server/internal/control/locator"
	"github.com/nickstrad/mansio/server/internal/tenancy"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancyhttp"
)

// Multi-node tenancy (IR-5; ADR 0012's multi-node half): controld authenticates once per resolve and issues
// the ticket; agents only verify tickets. New API keys ("mansio_<id>_<secret>") go through the locator's own
// path (their rows carry a principal and permissions); with a "tenancy" section, every other bearer of an app
// resolve goes to the same internal/tenancy service agentd uses single-node: legacy Deno-platform keys and, in
// session mode, user sessions (read and write). The tenant always comes from the credential. The section also
// mounts tenancyhttp (the tenant admin API under the admin token, and /api/auth in session mode).

// newTenancy builds the tenancy service over controld's own schema (nil without a "tenancy" section).
func newTenancy(cfg *Config, store tenancy.Store) (*tenancy.Service, error) {
	t := cfg.Tenancy
	if t == nil {
		return nil, nil
	}
	mode, _ := tenancy.ParseMode(t.Mode) // checked in parseConfig
	return tenancy.New(store, tenancy.Config{Mode: mode, Apps: t.Apps, AnyApp: len(t.Apps) == 0, CacheTTL: t.CacheTTL.Duration,
		SessionTTL: t.SessionTTL.Duration})
}

// credentials adapts tenancy.Service to locator.Credentials.
type credentials struct{ svc *tenancy.Service }

var _ locator.Credentials = credentials{}

func (c credentials) Authenticate(ctx context.Context, token, app string) (string, string, error) {
	p, err := c.svc.Authenticate(ctx, token, app)
	switch {
	case err == nil:
		return p.Tenant, p.Sub, nil
	case errors.Is(err, tenancy.ErrUnauthenticated):
		return "", "", fmt.Errorf("%w (%w)", locator.ErrUnauthorized, err)
	}
	return "", "", err
}

func (c credentials) Cached(token string) bool { return c.svc.Cached(token) }

// tenancyRoutes mounts the tenant admin API (and sign-in in session mode) when tenancy is on.
func (d *daemon) tenancyRoutes(mux *http.ServeMux) error {
	if d.tenancy == nil {
		return nil
	}
	return tenancyhttp.Mount(mux, d.tenancy, tenancyhttp.Config{AdminToken: d.token, Log: d.log, Shed: &d.tenancyShed})
}

// appAllowed reports whether credentials may reach app: tenancy.apps when set, else every app (create on first
// use still needs a registered schema).
func (d *daemon) appAllowed(app string) bool {
	return d.cfg.Tenancy == nil || len(d.cfg.Tenancy.Apps) == 0 || slices.Contains(d.cfg.Tenancy.Apps, app)
}

// errQuota is create on first use past a cap: 429 quota_exceeded, nothing created.
func errQuota(msg string) error {
	return &apiError{"quota_exceeded", http.StatusTooManyRequests, msg, false}
}

// checkCaps refuses a first-use create past locator.max_dbs_per_tenant or locator.max_dbs (live databases).
func (d *daemon) checkCaps(ctx context.Context, tenant string) error {
	n, all, err := d.pg.CountDatabases(ctx, tenant)
	switch {
	case err != nil:
		return err
	case n >= int64(d.cfg.Locator.MaxDBsPerTenant):
		return errQuota(fmt.Sprintf("tenant %s has %d databases, the most created on first use (locator.max_dbs_per_tenant)", tenant, n))
	case all >= int64(d.cfg.Locator.MaxDBs):
		return errQuota(fmt.Sprintf("%d databases exist, the most created on first use (locator.max_dbs)", all))
	}
	return nil
}
