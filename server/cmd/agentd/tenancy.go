package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/load"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/retry"
	"github.com/nickstrad/mansio/server/internal/tenancy"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancyhttp"
)

// Single-node tenancy (IR-4a, ADR 0012): with a "tenancy" section, /v1/ops also accepts credentials that are
// not tickets (the auth port, front.Credentials: off mode's app tokens, api-key mode's tenant keys, session
// mode's keys and sessions), the query listener serves the admin API (/admin/tenants…) and, in session mode,
// /api/auth sign-up and sign-in (tenancyhttp). Keys, users and sessions live in Postgres (the control plane's
// schema, migrated here at start in the background: the "tenancy" readiness check passes once it is done).
// Single node only. Static mode: a credential's database is <tenant>:<app> at default_epoch; create_dbs makes
// those files on first use. Durable mode with placement_dir (IR-5): the database runs at its owner-table epoch;
// one not held here is claimed first, and with create_dbs one with no placement row is created by the claim
// path (claimingPlace). With placement_pg the section is refused: controld authenticates and creates.

// TenancyConfig is the "tenancy" section.
type TenancyConfig struct {
	Mode        string `json:"mode"`         // off | api-key | session
	SecretsFile string `json:"secrets_file"` // 0600: admin_token, app_tokens (off mode), pg_dsn (api-key, session)
	PGSchema    string `json:"pg_schema"`    // search_path for the tenancy tables ("" = server default)
	// CacheTTL bounds how long a credential's answer is reused (another process sees a revocation within it);
	// default 1 s. SessionTTL is a new session's lifetime; default 7 days.
	CacheTTL   load.Duration `json:"cache_ttl"`
	SessionTTL load.Duration `json:"session_ttl"`
	// Apps are the apps credentials may reach (api-key and session modes); default the apps in schemas_dir.
	// Off mode: app_tokens' apps.
	Apps []string `json:"apps"`
}

// TenancySecrets is the tenancy secrets file.
type TenancySecrets struct {
	AdminToken string            `json:"admin_token"`
	AppTokens  map[string]string `json:"app_tokens"`
	PGDSN      string            `json:"pg_dsn"`
}

// checkTenancy validates the section (called from parseConfig).
func checkTenancy(c *Config, bad func(string, ...any)) {
	t := c.Tenancy
	if _, ok := tenancy.ParseMode(t.Mode); !ok {
		bad("tenancy.mode %q: want off, api-key or session", t.Mode)
	}
	if t.SecretsFile == "" {
		bad("tenancy.secrets_file is required (the admin token, app tokens and Postgres DSN)")
	}
	if c.Durable() && c.PlacementPG != nil {
		bad("tenancy with placement_pg: multi-node authenticates at controld's resolve (its tenancy section); agents verify tickets")
	}
	if t.CacheTTL.Duration < 0 || t.SessionTTL.Duration < 0 {
		bad("tenancy: cache_ttl and session_ttl must be >= 0")
	}
	if t.Mode != "off" && len(t.Apps) == 0 && c.SchemasDir == "" {
		bad("tenancy: give apps (or schemas_dir): the apps credentials may reach")
	}
	for _, a := range t.Apps {
		if !pg.ValidName(a) {
			bad("tenancy.apps: %q is not an app name", a)
		}
	}
}

// loadTenancySecrets reads the tenancy secrets file: mode 0600 or stricter, no unknown fields, errors without
// secrets.
func loadTenancySecrets(path string, mode tenancy.Mode) (*TenancySecrets, error) {
	b, err := readSecretFile(path)
	if err != nil {
		return nil, err
	}
	var s TenancySecrets
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, errs.Wrap(module, nil, "tenancy secrets_file %s is not valid: want a JSON object with admin_token, app_tokens and pg_dsn", path).With("path", path)
	}
	switch {
	case len(s.AdminToken) < minControlToken:
		return nil, errs.Wrap(module, nil, "tenancy secrets_file %s: admin_token must be at least %d characters", path, minControlToken).With("path", path)
	case mode == tenancy.Off && len(s.AppTokens) == 0:
		return nil, errs.Wrap(module, nil, "tenancy secrets_file %s: off mode needs app_tokens", path).With("path", path)
	case mode != tenancy.Off && s.PGDSN == "":
		return nil, errs.Wrap(module, nil, "tenancy secrets_file %s: api-key and session modes need pg_dsn", path).With("path", path)
	}
	return &s, nil
}

// readSecretFile reads a file that only its owner may read or write.
func readSecretFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot stat secrets file").With("path", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, errs.Wrap(module, nil, "secrets file %s has mode %#o; make it 0600 (chmod 600)", path, perm).With("path", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot read secrets file").With("path", path)
	}
	return b, nil
}

// tenancyPart is what the tenancy section adds to an agent.
type tenancyPart struct {
	creds      front.Credentials
	mount      func(mux *http.ServeMux) error
	ready      func(context.Context) error
	background []func(context.Context)
	breaker    *breaker.Breaker // the tenancy store's (nil in off mode)
	close      func()
}

// newTenancy opens the tenancy store (if the mode needs one) and the service.
func newTenancy(cfg *Config, log *slog.Logger, schemas front.SchemaSource) (*tenancyPart, error) {
	mode, _ := tenancy.ParseMode(cfg.Tenancy.Mode)
	apps := cfg.Tenancy.Apps
	if dir, ok := schemas.(dirSchemas); ok && len(apps) == 0 {
		for k := range dir {
			if !slices.Contains(apps, k[0]) {
				apps = append(apps, k[0])
			}
		}
	}
	if mode != tenancy.Off && len(apps) == 0 {
		return nil, errs.Wrap(module, nil, "tenancy: schemas_dir %s registers no app and tenancy.apps is empty: credentials could reach nothing", cfg.SchemasDir)
	}
	sec, err := loadTenancySecrets(cfg.Tenancy.SecretsFile, mode)
	if err != nil {
		return nil, err
	}
	part := &tenancyPart{ready: func(context.Context) error { return nil }, close: func() {}}
	var store tenancy.Store
	if mode != tenancy.Off {
		p, err := pg.Open(pg.Config{DSN: sec.PGDSN, AppName: "agentd/tenancy", Schema: cfg.Tenancy.PGSchema, MaxConns: 8,
			Breaker: breaker.Config{OnChange: logx.BreakerLog(log, "tenancy")}})
		if err != nil {
			return nil, err
		}
		store, part.breaker, part.close = p.Tenancy(), p.Breaker(), p.Close
		var migrated atomic.Bool
		part.ready = func(context.Context) error {
			if !migrated.Load() {
				return errors.New("tenancy: the Postgres schema is not migrated yet")
			}
			return nil
		}
		part.background = append(part.background, func(ctx context.Context) { migrate(ctx, p, &migrated, log) })
	}
	svc, err := tenancy.New(store, tenancy.Config{Mode: mode, Apps: apps, AppTokens: sec.AppTokens, CacheTTL: cfg.Tenancy.CacheTTL.Duration,
		SessionTTL: cfg.Tenancy.SessionTTL.Duration})
	if err != nil {
		part.close()
		return nil, err
	}
	part.creds = credentials{svc}
	part.mount = func(mux *http.ServeMux) error {
		return tenancyhttp.Mount(mux, svc, tenancyhttp.Config{AdminToken: sec.AdminToken, Log: log})
	}
	return part, nil
}

// migrate applies the schema, retrying with jittered backoff until it succeeds or ctx ends (Release It H1:
// the agent serves meanwhile; credentials are unavailable until then).
func migrate(ctx context.Context, p *pg.Placement, done *atomic.Bool, log *slog.Logger) {
	backoff := retry.Policy{Backoff: retry.Exponential, Jitter: retry.Equal, Base: 100 * time.Millisecond, Max: 5 * time.Second}
	_ = backoff.Normalize()
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	var delay time.Duration
	for attempt := 1; ; attempt++ {
		v, err := p.Migrate(ctx)
		if err == nil {
			done.Store(true)
			log.Info("tenancy schema ready", "event", "AGENT.TENANCY_READY", "version", v)
			return
		}
		log.Warn("tenancy schema migration failed; retrying", "event", "AGENT.TENANCY_MIGRATE_FAILED", "attempt", attempt, "error", err.Error())
		delay = backoff.Delay(rng, attempt+1, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// credentials adapts tenancy.Service to front.Credentials.
type credentials struct{ svc *tenancy.Service }

func (c credentials) Authenticate(ctx context.Context, token, app string) (string, string, error) {
	p, err := c.svc.Authenticate(ctx, token, app)
	var ae *tenancy.AuthError
	switch {
	case err == nil:
		return p.Tenant, p.Sub, nil
	case errors.As(err, &ae):
		return "", "", &front.AuthError{Reason: ae.Reason}
	case errors.Is(err, tenancy.ErrUnavailable):
		return "", "", fmt.Errorf("%w: %w", front.ErrUnavailable, err)
	}
	return "", "", err
}

// creatingHost is static mode with create_dbs: a database not served yet is created (an empty file in db_dir)
// and opened on its first admitted request; the first request then creates its tables (front initDB).
// Creations are serialized (one at a time per agent: they are rare and fast), never under the host's lock.
//
// Creation is capped (Release It S, B6): at most max_dbs databases on the agent and max_dbs_per_tenant per
// tenant (the part of the id before ':'), counting those found at start. Past a cap the request fails
// QuotaExceeded (429) and nothing is created.
type creatingHost struct {
	*front.StaticHost
	cfg       *Config
	mu        sync.Mutex
	next      int            // the next actor id
	total     int            // databases served (found at start + created)
	perTenant map[string]int // the same, by tenant
}

func newCreatingHost(host *front.StaticHost, cfg *Config, dbs []string) *creatingHost {
	h := &creatingHost{StaticHost: host, cfg: cfg, next: len(dbs), total: len(dbs), perTenant: map[string]int{}}
	for _, db := range dbs {
		h.perTenant[tenantOf(db)]++
	}
	return h
}

// tenantOf is a database id's tenant: the part before ':' (ADR 0011), or the whole id.
func tenantOf(db string) string {
	t, _, _ := strings.Cut(db, ":")
	return t
}

func (h *creatingHost) Lookup(ctx context.Context, db string) (*actor.Actor, error) {
	a, err := h.StaticHost.Lookup(ctx, db)
	if !errors.Is(err, front.ErrUnknownDB) {
		return a, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if a, err := h.StaticHost.Lookup(ctx, db); !errors.Is(err, front.ErrUnknownDB) {
		return a, err // created meanwhile
	}
	tenant := tenantOf(db)
	switch {
	case h.total >= h.cfg.MaxDBs:
		return nil, &front.QuotaError{Scope: "agent", Limit: h.cfg.MaxDBs}
	case h.perTenant[tenant] >= h.cfg.MaxDBsPerTenant:
		return nil, &front.QuotaError{Scope: "tenant", Limit: h.cfg.MaxDBsPerTenant}
	}
	path := dbPath(h.cfg, db)
	created := false // whether this call made the file (a leftover one is kept as found)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	switch {
	case err == nil:
		created = true
		f.Close()
	case !errors.Is(err, os.ErrExist):
		return nil, fmt.Errorf("%w: cannot create database %s: %w", front.ErrUnavailable, db, err)
	}
	a = actor.New(h.next+1, path, h.cfg.Actor.Actor())
	if err := a.Start(true); err != nil {
		a.Close()
		if created {
			os.Remove(path) // the empty file this call made; nothing else refers to it
		}
		return nil, fmt.Errorf("%w: cannot open new database %s: %w", front.ErrUnavailable, db, err)
	}
	h.next++
	if !h.StaticHost.Add(db, a) {
		a.Close()
		return nil, fmt.Errorf("%w: host closed", front.ErrShed)
	}
	h.total++
	h.perTenant[tenant]++
	return a, nil
}

// creatingAdmitter admits a database not in the static table at default_epoch (create_dbs).
type creatingAdmitter struct {
	front.Admitter                      // the static table, behind startup
	table          front.StaticAdmitter // the databases found at start (at their configured epochs)
	epoch          uint64
}

func (a creatingAdmitter) Admit(db string, epoch uint64) (func(), error) {
	if _, known := a.table[db]; !known && epoch == a.epoch {
		return func() {}, nil
	}
	return a.Admitter.Admit(db, epoch)
}

// claimingPlace is single-node durable tenancy's front.OpsConfig.CredentialPlace: a credential request runs at
// its database's owner-table epoch. A database not active here is claimed first (the single node's own control
// plane: its placement row names this vm, e.g. after dbtool import -placement-dir, or after a restart's
// reconcile left it unclaimed); with create_dbs, one that has no placement row is created first, as control.go's
// claim create (ACTIVE(self, 1, 0)), within max_dbs and max_dbs_per_tenant (the keys this agent's table knows).
// Claims and creates are serialized (rare, and each is a few authority calls); warm requests never lock.
type claimingPlace struct {
	d  *durableAgent
	mu sync.Mutex
}

func (p *claimingPlace) place(ctx context.Context, db string) (uint64, uint64, error) {
	if l := p.d.tab.State(db); l.State == owner.Active {
		return uint64(l.Epoch), uint64(l.Gen), nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if l := p.d.tab.State(db); l.State == owner.Active {
		return uint64(l.Epoch), uint64(l.Gen), nil
	}
	_, err := p.d.place.Get(ctx, db)
	switch {
	case errors.Is(err, owner.ErrNotFound) && !p.d.cfg.CreateDBs:
		return 0, 0, fmt.Errorf("%w: %s has no placement row and create_dbs is off", front.ErrUnknownDB, db)
	case errors.Is(err, owner.ErrNotFound):
		if err := p.create(ctx, db); err != nil {
			return 0, 0, err
		}
	case err != nil:
		return 0, 0, asUnavailable(err)
	}
	l, err := p.d.tab.Claim(ctx, db)
	switch {
	case err == nil:
		return uint64(l.Epoch), uint64(l.Gen), nil
	case errors.Is(err, owner.ErrWrongOwner):
		return 0, 0, fmt.Errorf("%w: %w", front.ErrWrongOwner, err)
	case errors.Is(err, owner.ErrFenced), errors.Is(err, owner.ErrMoving):
		return 0, 0, fmt.Errorf("%w: %w", front.ErrFenced, err)
	}
	return 0, 0, asUnavailable(err)
}

// create writes db's placement row ACTIVE(self, 1, 0) within the caps.
//
//	pre: p.mu held
func (p *claimingPlace) create(ctx context.Context, db string) error {
	cfg := p.d.cfg
	keys := p.d.tab.Keys()
	tenant := tenantOf(db)
	n := 0
	for _, k := range keys {
		if tenantOf(k) == tenant {
			n++
		}
	}
	switch {
	case len(keys) >= cfg.MaxDBs:
		return &front.QuotaError{Scope: "agent", Limit: cfg.MaxDBs}
	case n >= cfg.MaxDBsPerTenant:
		return &front.QuotaError{Scope: "tenant", Limit: cfg.MaxDBsPerTenant}
	}
	err := p.d.place.CAS(ctx, db, owner.Place{}, owner.Place{Owner: cfg.VM, Epoch: 1, State: owner.PlaceActive})
	if errors.Is(err, owner.ErrConflict) {
		return nil // created meanwhile: the claim reads whatever is there
	}
	return asUnavailable(err)
}
