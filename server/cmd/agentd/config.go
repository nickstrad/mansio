package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/load"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// Config is agentd's JSON config. Defaults are applied before decoding (a file overrides them); unknown fields
// are errors. Relative paths are relative to the config file's directory.
type Config struct {
	Listen       string   `json:"listen"`        // query listener; default 127.0.0.1:7070 (":0" picks a port)
	HealthListen string   `json:"health_listen"` // optional lifecycle listener: /healthz, /livez, /readyz, /metrics
	Protocols    []string `json:"protocols"`     // "h1", "h2c"; default both (plain HTTP only)
	// TLS makes the query listener speak HTTPS (tls.go); required for a non-loopback listen without -dev.
	TLS *TLSFiles `json:"tls"`
	VM  string    `json:"vm"` // this VM's id: the ticket audience (required)

	// AdminListen is the optional admin listener (runtime controls, admin.go): a loopback address only, since
	// binding is its only access control.
	AdminListen string `json:"admin_listen"`

	// Static mode (no s3 section; benchmarks and local runs): serve the <db>.db files in db_dir, owned at
	// default_epoch (or epochs[db]). No ownership table, no write-back.
	DBDir string   `json:"db_dir"` // directory of <db>.db files (required in static mode)
	DBs   []string `json:"dbs"`    // serve only these; default every *.db in db_dir
	// CreateDBs creates a database not found at start (an empty <db>.db) on its first request admitted at
	// default_epoch; db_dir may then start empty (tenancy.go).
	CreateDBs bool `json:"create_dbs"`
	// The creation caps (create_dbs): databases on this agent and per tenant, counting those found at start;
	// default 10 000 and 100. Past one a request is QuotaExceeded (429).
	MaxDBs          int `json:"max_dbs"`
	MaxDBsPerTenant int `json:"max_dbs_per_tenant"`
	// Tenancy accepts credentials besides tickets and serves the tenancy admin and sign-in API (tenancy.go).
	Tenancy *TenancyConfig `json:"tenancy"`

	// Durable mode (an s3 section): databases live in S3 (internal/durable layout v2) behind a virtual-actor
	// host, ownership is the owner table, and the control plane calls the lifecycle endpoints (control.go).
	S3 *S3Config `json:"s3"`
	// SecretsFile holds the S3 keys and the control-plane token: a JSON file outside the config, mode 0600 or
	// stricter (object-storage guide S2). Required in durable mode.
	SecretsFile string `json:"secrets_file"`
	CacheDir    string `json:"cache_dir"` // local database copies; emptied at start (required)
	OwnerDir    string `json:"owner_dir"` // the owner table's durable records (required)
	// The placement authority: exactly one of placement_dir (a file-backed single-node authority shared by the
	// agents of one host; CP-1) and placement_pg (Postgres, the control plane's authority; CP-4). With
	// placement_pg the DSN comes from the secrets file (pg_dsn).
	PlacementDir string             `json:"placement_dir"`
	PlacementPG  *PlacementPGConfig `json:"placement_pg"`
	// CacheBytes is the cache directory's budget (0: none). It splits into the generation cache
	// (gen_cache_bytes, default a quarter of it) and the warm databases (maintenance.max_bytes: the rest).
	CacheBytes    int64       `json:"cache_bytes"`
	GenCacheBytes int64       `json:"gen_cache_bytes"`
	Maintenance   Maintenance `json:"maintenance"`
	// Lifecycle endpoints (Claim, Fence, Forget, Health): each bounded by lifecycle_timeout (a caller's
	// X-Timeout-Ms may shorten it), at most lifecycle_concurrency at once; more are refused, never queued.
	LifecycleTimeout     load.Duration `json:"lifecycle_timeout"`
	LifecycleConcurrency int           `json:"lifecycle_concurrency"`
	ReconcilePerKey      load.Duration `json:"reconcile_per_key"` // one key's reconcile deadline; workers = startup_workers

	// Ticket public keys: keys_file (a ticket.PublicKeyFile; static, for dev and benches), or keys_url (the
	// issuer's GET /v1/keys on controld, fetched every keys_refresh; CP-5). With keys_url, keys_file is
	// optional and caches the last fetched set, so a restart while controld is down still verifies tickets.
	KeysFile string `json:"keys_file"`
	KeysURL  string `json:"keys_url"`
	// ControlCAFile is the PEM CA bundle that signs controld's certificate (an https keys_url with a private
	// CA); default the system roots.
	ControlCAFile string        `json:"control_ca_file"`
	KeysRefresh   load.Duration `json:"keys_refresh"` // default 30 s; controld's ticket_keys.activate must be at least twice this
	// SchemasURL is controld's API base URL for the app schema registry (GET /v1/apps/{app}/schemas/{fingerprint}
	// with the control token; internal/schemareg, IR-2). Durable mode only; "" = no registry client. An https URL
	// with a private CA uses control_ca_file.
	SchemasURL string `json:"schemas_url"`
	// SchemasDir is a local schema registry (single-node dev and tests, no controld): <app>/<name>.json files,
	// each one schema in fingerprint form (JSON.stringify of the defineSchema value), read at start. Static mode,
	// or durable mode without schemas_url (IR-4b).
	SchemasDir     string        `json:"schemas_dir"`
	SchemasTimeout load.Duration `json:"schemas_timeout"` // one fetch; default 3 s
	SchemaCache    int           `json:"schema_cache"`    // schemas cached (by app and fingerprint); default 1024
	// VerifySlots bounds /v1/ops credential requests inside their pre-admission checks at once (the tenancy
	// store, the schema registry, a claim or create); more are shed at once. 0 = 64.
	VerifySlots  int           `json:"verify_slots"`
	KeysTimeout  load.Duration `json:"keys_timeout"` // one fetch; default 3 s
	ClockSkew    load.Duration `json:"clock_skew"`
	MaxTicketTTL load.Duration `json:"max_ticket_ttl"`
	TicketCache  int           `json:"ticket_cache"` // verified tickets remembered; 0 = off

	// Static mode's admitter: every served database is owned at default_epoch unless epochs overrides it.
	DefaultEpoch uint64            `json:"default_epoch"`
	Epochs       map[string]uint64 `json:"epochs"`

	Admission         front.AdmissionLimits `json:"admission"`
	Limits            front.Limits          `json:"limits"`
	DefaultTimeout    load.Duration         `json:"default_timeout"`
	MaxTimeout        load.Duration         `json:"max_timeout"`
	StatementTimeout  load.Duration         `json:"statement_timeout"`
	ReplyGrace        load.Duration         `json:"reply_grace"`
	DrainTimeout      load.Duration         `json:"drain_timeout"`   // front drain, the first part of shutdown_budget
	ShutdownBudget    load.Duration         `json:"shutdown_budget"` // whole shutdown: drain, close databases; default 60 s
	ReadHeaderTimeout load.Duration         `json:"read_header_timeout"`
	BodyReadTimeout   load.Duration         `json:"body_read_timeout"` // whole request body; default 5 s
	IdleTimeout       load.Duration         `json:"idle_timeout"`
	Actor             load.ActorConfig      `json:"actor"`
	// actorQueues counts every actor's refusals and mailbox waits (set by newAgent; nil counts nothing).
	actorQueues *actor.Queues

	// Startup and readiness (Release It H1, H2). Databases open in the background, startup_workers at a time,
	// while the agent already serves. /readyz reflects a probe every ready_interval, each bounded by
	// ready_timeout (which must be shorter).
	StartupWorkers int           `json:"startup_workers"` // default 16
	ReadyInterval  load.Duration `json:"ready_interval"`  // default 1 s
	ReadyTimeout   load.Duration `json:"ready_timeout"`   // default 500 ms

	// EventLog writes mansio's event log (events.go, internal/eventlog) for the offline TS ingest and analysis
	// catalog; nil = none.
	EventLog *EventLogConfig `json:"event_log"`

	LogFile  string `json:"log_file"`  // JSON log; default stderr
	LogLevel string `json:"log_level"` // debug | info (default) | warn; POST /admin/loglevel changes it at run time

	// Dev is the -dev flag, not a file field: local development, where plain-HTTP S3 and the claim's create
	// stand-in are allowed (object-storage guide S2; control.go).
	Dev bool `json:"-"`
}

// EventLogConfig is the event log: <dir>/server/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl, read by
// `deno task logs:ingest` with LOG_DIR=<dir> (docs/observability.md).
type EventLogConfig struct {
	Dir          string `json:"dir"`            // LOG_DIR (required)
	Level        string `json:"level"`          // debug | info (default) | warn | error
	TraceSQL     bool   `json:"trace_sql"`      // a sql record per statement (TRACE_SQL); failed statements always
	Buffer       int    `json:"buffer"`         // queued records before drops; default 8192
	MaxFileBytes int64  `json:"max_file_bytes"` // roll size; default 64 MiB (LOG_MAX_FILE_BYTES)
}

// S3Config is where durable mode keeps databases. The keys come from the secrets file.
type S3Config struct {
	Endpoint     string        `json:"endpoint"` // host:port (required)
	Bucket       string        `json:"bucket"`   // required; created if missing
	Prefix       string        `json:"prefix"`   // root prefix inside the bucket ("" = none; must end in "/")
	OpTimeout    load.Duration `json:"op_timeout"`
	StallTimeout load.Duration `json:"stall_timeout"`
	MaxConns     int           `json:"max_conns"`
	// Breaker overrides the S3 breaker's thresholds; zero fields take Release It §P (50 % of >= 20 calls in
	// 10 s, cool-down 5 s).
	Breaker BreakerConfig `json:"breaker"`

	// Secure speaks HTTPS to the endpoint; false (plain HTTP) is refused without -dev (object-storage guide S2).
	Secure bool `json:"secure"`
	// CAFile is a PEM bundle of the CAs that sign the endpoint's certificate (a private CA); default the
	// system roots. Requires secure.
	CAFile string `json:"ca_file"`
}

// PlacementPGConfig selects the Postgres placement authority (internal/placement/pg). Zero fields take Release
// It §P (connect 3 s, statement 2 s, lock 1 s, pool = startup_workers + lifecycle_concurrency).
type PlacementPGConfig struct {
	Schema   string `json:"schema"`    // search_path ("" = the server default); tests use one schema each
	MaxConns int32  `json:"max_conns"` // default startup_workers + lifecycle_concurrency (RI §P)
	// Breaker overrides the placement breaker's thresholds (zero fields: RI §P).
	Breaker BreakerConfig `json:"breaker"`
}

// BreakerConfig is a breaker's thresholds in a config file; zero fields take Release It §P.
type BreakerConfig struct {
	Window      load.Duration `json:"window"`
	MinCalls    int           `json:"min_calls"`
	FailureRate float64       `json:"failure_rate"`
	CoolDown    load.Duration `json:"cool_down"`
}

// config is the breaker.Config (Normalize fills the defaults and checks it).
func (b BreakerConfig) config() breaker.Config {
	return breaker.Config{Window: b.Window.Duration, MinCalls: b.MinCalls, FailureRate: b.FailureRate, CoolDown: b.CoolDown.Duration}
}

// Maintenance drives the host's periodic write-back and eviction (vactor.Policy, Release It S2).
type Maintenance struct {
	Interval         load.Duration `json:"interval"`           // tick period; default 1 s
	WriteBackEvery   load.Duration `json:"write_back_every"`   // RPO in time; default 10 s
	WriteBackCommits uint64        `json:"write_back_commits"` // RPO in commits; 0 = off
	IdleAfter        load.Duration `json:"idle_after"`         // passivate after this long unused; default 5 min
	MaxActive        int           `json:"max_active"`         // warm budget by count; 0 = none
	MaxBytes         int64         `json:"max_bytes"`          // warm budget by bytes; default from cache_bytes
	WriteBackTimeout load.Duration `json:"write_back_timeout"` // one write-back or passivation; default 60 s
}

// actorConfig is the config every actor of this agent runs with.
func (c *Config) actorConfig() actor.Config {
	a := c.Actor.Actor()
	a.Queues = c.actorQueues
	return a
}

// Durable reports whether the config runs durable mode.
func (c *Config) Durable() bool { return c.S3 != nil }

func defaults() Config {
	d := func(v time.Duration) load.Duration { return load.Duration{Duration: v} }
	return Config{
		Listen: "127.0.0.1:7070", Protocols: []string{"h1", "h2c"},
		MaxDBs: 10_000, MaxDBsPerTenant: 100,
		ClockSkew: d(5 * time.Second), MaxTicketTTL: d(10 * time.Minute), TicketCache: 65536, DefaultEpoch: 1,
		KeysRefresh: d(30 * time.Second), KeysTimeout: d(3 * time.Second), SchemasTimeout: d(3 * time.Second), SchemaCache: 1024,
		Limits:         front.Limits{}.WithDefaults(),
		DefaultTimeout: d(2 * time.Second), MaxTimeout: d(30 * time.Second), StatementTimeout: d(time.Second),
		ReplyGrace: d(time.Second), DrainTimeout: d(10 * time.Second), ShutdownBudget: d(time.Minute), ReadHeaderTimeout: d(5 * time.Second), BodyReadTimeout: d(5 * time.Second),
		IdleTimeout: d(2 * time.Minute), LogLevel: "info",
		StartupWorkers: 16, ReadyInterval: d(time.Second), ReadyTimeout: d(500 * time.Millisecond),
		Maintenance: Maintenance{Interval: d(time.Second), WriteBackEvery: d(10 * time.Second), IdleAfter: d(5 * time.Minute),
			WriteBackTimeout: d(time.Minute)},
		LifecycleTimeout: d(30 * time.Second), LifecycleConcurrency: 16, ReconcilePerKey: d(5 * time.Second),
	}
}

// parseConfig decodes r over the defaults and validates, reporting every problem at once. dir resolves
// relative paths.
func parseConfig(r io.Reader, dir string) (*Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	c := defaults()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	var problems []error
	bad := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	if c.VM == "" {
		bad("vm is required")
	}
	if c.Durable() {
		checkDurable(&c, bad)
	} else if c.DBDir == "" {
		bad("db_dir is required (or an s3 section for durable mode)")
	}
	if c.Tenancy != nil {
		checkTenancy(&c, bad)
	}
	if c.CreateDBs && c.Durable() && c.PlacementPG != nil {
		bad("create_dbs with placement_pg: creating databases is controld's job (resolve with create on first use)")
	}
	if c.CreateDBs && c.SchemasDir == "" {
		bad("create_dbs needs schemas_dir: a database is only created for a registered schema")
	}
	if c.MaxDBs < 1 || c.MaxDBsPerTenant < 1 || c.MaxDBsPerTenant > c.MaxDBs {
		bad("max_dbs and max_dbs_per_tenant must be >= 1, max_dbs_per_tenant <= max_dbs")
	}
	if c.KeysFile == "" && c.KeysURL == "" {
		bad("keys_file or keys_url is required (the ticket public keys)")
	}
	if c.KeysURL != "" {
		if u, err := url.Parse(c.KeysURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("keys_url %q must be an http(s) URL", c.KeysURL)
		}
		if c.KeysRefresh.Duration <= 0 || c.KeysTimeout.Duration <= 0 || c.KeysTimeout.Duration >= c.KeysRefresh.Duration {
			bad("keys_refresh and keys_timeout must be > 0, keys_timeout shorter than keys_refresh")
		}
	}
	if c.ControlCAFile != "" && !strings.HasPrefix(c.KeysURL, "https:") && !strings.HasPrefix(c.SchemasURL, "https:") {
		bad("control_ca_file needs an https keys_url or schemas_url (it would be unused)")
	}
	if c.SchemasURL != "" {
		if u, err := url.Parse(c.SchemasURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("schemas_url %q must be an http(s) URL", c.SchemasURL)
		}
		if !c.Durable() {
			bad("schemas_url needs durable mode (the control token comes from the secrets file)")
		}
		if c.SchemasTimeout.Duration <= 0 || c.SchemaCache <= 0 {
			bad("schemas_timeout and schema_cache must be > 0")
		}
	}
	if c.SchemasDir != "" && c.SchemasURL != "" {
		bad("give at most one schema registry: schemas_dir (local files) or schemas_url (controld)")
	}
	if c.Listen == "" {
		bad("listen is required")
	}
	if c.TLS != nil && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		bad("tls: cert_file and key_file are both required")
	}
	if c.AdminListen != "" && !loopback(c.AdminListen) {
		bad("admin_listen %q must be a loopback address such as 127.0.0.1:7072 (binding is the admin endpoints' only access control)", c.AdminListen)
	}
	if len(c.Protocols) == 0 {
		bad("protocols: at least one of h1, h2c")
	}
	for _, p := range c.Protocols {
		if p != "h1" && p != "h2c" {
			bad("protocols: %q is not h1 or h2c", p)
		}
	}
	for _, db := range c.DBs {
		if err := front.CheckDBID(db); err != nil {
			bad("dbs: %q: %v", db, err)
		}
	}
	for db, e := range c.Epochs {
		if e == 0 {
			bad("epochs[%s]: epoch 0 means not owned; leave the database out of dbs instead", db)
		}
	}
	if c.DefaultEpoch == 0 {
		bad("default_epoch must be >= 1")
	}
	for name, d := range map[string]time.Duration{"clock_skew": c.ClockSkew.Duration, "max_ticket_ttl": c.MaxTicketTTL.Duration} {
		if d < 0 {
			bad("%s must be >= 0", name)
		}
	}
	for name, d := range map[string]time.Duration{"default_timeout": c.DefaultTimeout.Duration, "max_timeout": c.MaxTimeout.Duration,
		"statement_timeout": c.StatementTimeout.Duration, "reply_grace": c.ReplyGrace.Duration, "drain_timeout": c.DrainTimeout.Duration,
		"shutdown_budget":     c.ShutdownBudget.Duration,
		"read_header_timeout": c.ReadHeaderTimeout.Duration, "body_read_timeout": c.BodyReadTimeout.Duration, "idle_timeout": c.IdleTimeout.Duration} {
		if d <= 0 {
			bad("%s must be > 0", name)
		}
	}
	if c.DefaultTimeout.Duration > c.MaxTimeout.Duration {
		bad("default_timeout %v is over max_timeout %v", c.DefaultTimeout, c.MaxTimeout)
	}
	if c.StartupWorkers < 1 {
		bad("startup_workers must be >= 1")
	}
	if c.ReadyTimeout.Duration >= c.ReadyInterval.Duration {
		bad("ready_timeout %v must be shorter than ready_interval %v", c.ReadyTimeout, c.ReadyInterval)
	}
	if c.DrainTimeout.Duration > c.ShutdownBudget.Duration {
		bad("drain_timeout %v is over shutdown_budget %v (the drain is part of the shutdown)", c.DrainTimeout, c.ShutdownBudget)
	}
	if c.VerifySlots < 0 {
		bad("verify_slots must be >= 0 (0 = 64)")
	}
	if c.Limits.MaxBody <= 0 || c.Limits.MaxSQL <= 0 || c.Limits.MaxArgs <= 0 || c.Limits.MaxRows <= 0 {
		bad("limits must all be > 0")
	}
	if a := c.Admission; min(a.TenantInFlight, a.TenantQueue, a.DBInFlight, a.DBQueue, a.VMInFlight, a.VMQueue) < 0 {
		bad("admission: caps must be >= 0 (0 = default)")
	}
	c.Admission = c.Admission.WithDefaults() // after decoding: the VM defaults derive from the file's db_in_flight
	if a := c.Admission; a.DBInFlight > a.TenantInFlight || a.TenantInFlight > a.VMInFlight {
		bad("admission: caps must nest: db_in_flight %d <= tenant_in_flight %d <= vm_in_flight %d", a.DBInFlight, a.TenantInFlight, a.VMInFlight)
	}
	if err := c.Actor.Actor().Validate(); err != nil {
		bad("actor: %w", err)
	}
	switch c.LogLevel {
	case "debug", "info", "warn":
	default:
		bad("log_level %q: want debug, info or warn", c.LogLevel)
	}
	if c.Tenancy != nil && c.Tenancy.SecretsFile != "" && !filepath.IsAbs(c.Tenancy.SecretsFile) {
		c.Tenancy.SecretsFile = filepath.Join(dir, c.Tenancy.SecretsFile)
	}
	paths := []*string{&c.DBDir, &c.KeysFile, &c.LogFile, &c.SecretsFile, &c.CacheDir, &c.OwnerDir, &c.PlacementDir, &c.SchemasDir}
	if e := c.EventLog; e != nil {
		checkEventLog(e, bad)
		paths = append(paths, &e.Dir)
	}
	if c.S3 != nil {
		paths = append(paths, &c.S3.CAFile)
	}
	if c.TLS != nil {
		paths = append(paths, &c.TLS.CertFile, &c.TLS.KeyFile)
	}
	for _, p := range paths {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	return &c, errors.Join(problems...)
}

// checkDurable validates the durable-mode fields and derives the cache split.
func checkDurable(c *Config, bad func(string, ...any)) {
	s := c.S3
	if s.Endpoint == "" || s.Bucket == "" {
		bad("s3: endpoint and bucket are required")
	}
	if bc := s.Breaker.config(); bc.Normalize() != nil {
		bad("s3.breaker: invalid thresholds (window, min_calls >= 1, failure_rate in (0, 1], cool_down)")
	}
	if s.Prefix != "" && !strings.HasSuffix(s.Prefix, "/") {
		bad("s3: prefix %q must end in /", s.Prefix)
	}
	if s.CAFile != "" && !s.Secure {
		bad("s3: ca_file needs secure: true (plain HTTP would ignore it)")
	}
	if s.OpTimeout.Duration < 0 || s.StallTimeout.Duration < 0 || s.MaxConns < 0 {
		bad("s3: op_timeout, stall_timeout and max_conns must be >= 0 (0 = default)")
	}
	for name, v := range map[string]string{"secrets_file": c.SecretsFile, "cache_dir": c.CacheDir, "owner_dir": c.OwnerDir} {
		if v == "" {
			bad("%s is required in durable mode", name)
		}
	}
	switch pg := c.PlacementPG; {
	case (c.PlacementDir == "") == (pg == nil):
		bad("give exactly one placement authority: placement_dir (single node) or placement_pg (Postgres)")
	case pg != nil:
		if pg.MaxConns < 0 {
			bad("placement_pg.max_conns must be >= 0 (0 = default)")
		}
		if pg.MaxConns == 0 {
			pg.MaxConns = int32(c.StartupWorkers + c.LifecycleConcurrency)
		}
		if bc := pg.Breaker.config(); bc.Normalize() != nil {
			bad("placement_pg.breaker: invalid thresholds (window, min_calls >= 1, failure_rate in (0, 1], cool_down)")
		}
	}
	if c.DBDir != "" || len(c.DBs) > 0 || len(c.Epochs) > 0 {
		bad("db_dir, dbs and epochs are static mode only; durable mode serves what the control plane claims")
	}
	if c.CacheBytes < 0 || c.GenCacheBytes < 0 {
		bad("cache_bytes and gen_cache_bytes must be >= 0")
	}
	if c.CacheBytes > 0 {
		if c.GenCacheBytes == 0 {
			c.GenCacheBytes = c.CacheBytes / 4
		}
		if c.GenCacheBytes >= c.CacheBytes {
			bad("gen_cache_bytes %d must be below cache_bytes %d (the rest holds the warm databases)", c.GenCacheBytes, c.CacheBytes)
		}
		if c.Maintenance.MaxBytes == 0 {
			c.Maintenance.MaxBytes = c.CacheBytes - c.GenCacheBytes
		}
	}
	m := c.Maintenance
	if m.Interval.Duration <= 0 || m.WriteBackTimeout.Duration <= 0 {
		bad("maintenance: interval and write_back_timeout must be > 0")
	}
	if m.WriteBackEvery.Duration < 0 || m.IdleAfter.Duration < 0 || m.MaxActive < 0 || m.MaxBytes < 0 {
		bad("maintenance: write_back_every, idle_after, max_active and max_bytes must be >= 0")
	}
	if c.LifecycleTimeout.Duration <= 0 || c.LifecycleConcurrency < 1 || c.ReconcilePerKey.Duration <= 0 {
		bad("lifecycle_timeout and reconcile_per_key must be > 0, lifecycle_concurrency >= 1")
	}
}

// loopback reports whether addr (host:port) names a loopback host: localhost or a loopback IP. An empty host
// (":7072") binds every interface, so it is not.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkDev refuses what only local development may do unless -dev is given: durable mode over plain-HTTP S3
// (object-storage guide S1, S2; plan OS-11) and a plain-HTTP query listener off loopback (CP-7, tls.go).
func (c *Config) checkDev() error {
	if err := c.checkTLS(); err != nil {
		return err
	}
	if strings.HasPrefix(c.KeysURL, "http:") && !c.Dev {
		return errs.Wrap(module, nil, "keys_url is plain HTTP: whoever can change the answer can mint tickets; use https, or pass -dev "+
			"for a local run").With("keys_url", c.KeysURL)
	}
	if strings.HasPrefix(c.SchemasURL, "http:") && !c.Dev {
		return errs.Wrap(module, nil, "schemas_url is plain HTTP: the control token and the schemas would travel in the clear; use https, "+
			"or pass -dev for a local run").With("schemas_url", c.SchemasURL)
	}
	if c.Durable() && !c.S3.Secure && !c.Dev {
		return errs.Wrap(module, nil, "s3.secure is false: plain-HTTP S3 is for local development only; set \"secure\": true "+
			"(and \"ca_file\" for a private CA) in the s3 section, or pass -dev for a local run").With("endpoint", c.S3.Endpoint)
	}
	return nil
}

// Secrets is the secrets file (0600, outside the config; object-storage guide S2, Release It RI-16).
type Secrets struct {
	S3AccessKey  string `json:"s3_access_key"`
	S3SecretKey  string `json:"s3_secret_key"`
	ControlToken string `json:"control_token"` // the control plane's bearer token for the lifecycle endpoints
	PGDSN        string `json:"pg_dsn"`        // the placement authority's DSN (required with placement_pg)
}

// minControlToken is the shortest control token accepted (a guessable token would open Fence to anyone).
const minControlToken = 16

// loadSecrets reads the secrets file. It refuses a file that group or others can read or write, unknown
// fields, and missing values. Error messages never include a secret.
func loadSecrets(path string) (*Secrets, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot open secrets_file").With("path", path)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot stat secrets_file").With("path", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, errs.Wrap(module, nil, "secrets_file %s has mode %#o; make it 0600 (chmod 600)", path, perm).With("path", path)
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var s Secrets
	if err := dec.Decode(&s); err != nil {
		return nil, errs.Wrap(module, nil, "secrets_file %s is not valid: want a JSON object with s3_access_key, s3_secret_key and control_token", path).With("path", path)
	}
	switch {
	case s.S3AccessKey == "" || s.S3SecretKey == "":
		return nil, errs.Wrap(module, nil, "secrets_file %s: s3_access_key and s3_secret_key are required", path).With("path", path)
	case len(s.ControlToken) < minControlToken:
		return nil, errs.Wrap(module, nil, "secrets_file %s: control_token must be at least %d characters", path, minControlToken).With("path", path)
	}
	return &s, nil
}

// loadConfig reads and validates the config file.
func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot open config").With("path", path)
	}
	defer f.Close()
	c, err := parseConfig(f, filepath.Dir(path))
	if err != nil {
		return nil, errs.Wrap(module, err, "invalid config %s", path).With("path", path)
	}
	return c, nil
}

// loadKeys reads the ticket public key set.
func loadKeys(path string) (*ticket.KeyRing, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot open keys_file").With("path", path)
	}
	defer f.Close()
	keys, err := ticket.ParsePublicKeys(f)
	if err != nil {
		return nil, errs.Wrap(module, err, "invalid keys_file %s", path).With("path", path)
	}
	return ticket.NewKeyRing(keys), nil
}
