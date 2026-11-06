package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/load"
	"github.com/nickstrad/ts-db-go/internal/placement/pg"
	"github.com/nickstrad/ts-db-go/internal/tlsx"
)

// Config is controld's JSON config. Defaults are applied before decoding; unknown fields are errors;
// relative paths are relative to the config file's directory.
type Config struct {
	Listen string `json:"listen"` // the control API and /livez, /readyz, /metrics; default 127.0.0.1:7080
	// TLS is the listener's certificate: HTTPS (h2 + h1) for the admin API, POST /v1/resolve (API keys and
	// tickets cross it) and GET /v1/keys (whoever can change its answer can mint tickets). Required unless -dev.
	TLS         *tlsx.Files `json:"tls"`
	SecretsFile string      `json:"secrets_file"` // 0600: pg_dsn, control_token, admin_token, s3 keys (required)

	PG struct {
		Schema   string `json:"schema"`    // search_path ("" = server default); tests use one schema each
		MaxConns int32  `json:"max_conns"` // default 20
	} `json:"pg"`

	// S3 is where the databases live: controld deletes a dropped database's prefix (the gc identity: Read,
	// List, Write). Same endpoint, bucket and prefix as the agents.
	S3 struct {
		Endpoint  string        `json:"endpoint"`
		Bucket    string        `json:"bucket"`
		Prefix    string        `json:"prefix"`
		OpTimeout load.Duration `json:"op_timeout"`
		// Secure speaks HTTPS; false (plain HTTP) is refused without -dev (object-storage guide S2, OS-11).
		Secure bool   `json:"secure"`
		CAFile string `json:"ca_file"` // PEM bundle of a private CA; needs secure
	} `json:"s3"`

	// Dev is the -dev flag, not a file field: allows plain-HTTP S3 and a plain-HTTP listener for a local run.
	Dev bool `json:"-"`

	// Agents is the VM registry's source: every agent controld may place databases on. Registered at start
	// (pg.RegisterVM); heartbeats come from polling each one's health endpoint.
	Agents []AgentConfig `json:"agents"`

	Policy struct {
		MaxDatabases    int64         `json:"max_databases"`     // per VM; 0 = no cap
		MaxBytes        int64         `json:"max_bytes"`         // per VM (cache bytes reported); 0 = no cap
		MaxHeartbeatAge load.Duration `json:"max_heartbeat_age"` // a VM silent longer gets nothing new; default 3 × heartbeat_interval
	} `json:"policy"`

	AgentTimeout      load.Duration `json:"agent_timeout"`      // one lifecycle call; default 30 s (a fence may write back)
	HeartbeatInterval load.Duration `json:"heartbeat_interval"` // default 2 s
	HeartbeatTimeout  load.Duration `json:"heartbeat_timeout"`  // one health call; default 1 s
	ResumeInterval    load.Duration `json:"resume_interval"`    // resumer pass period; default 5 s
	ResumeAfter       load.Duration `json:"resume_after"`       // a flow untouched this long is resumed; default 30 s
	ResumeLimit       int           `json:"resume_limit"`       // keys per scan per pass; default 100
	PerKeyTimeout     load.Duration `json:"per_key_timeout"`    // one key's resume or restore; default 60 s
	APITimeout        load.Duration `json:"api_timeout"`        // one API call; default 2 min (a restore moves many keys)
	APIConcurrency    int           `json:"api_concurrency"`    // API calls at once; more are refused 503; default 16
	ShutdownBudget    load.Duration `json:"shutdown_budget"`    // default 10 s

	// Locator is the resolve API (CP-5): POST /v1/resolve with an API key, answered from a placement cache
	// that Postgres notifications invalidate (PG-10). Its own bulkhead: resolves never queue behind admin calls.
	Locator struct {
		TicketTTL   load.Duration `json:"ticket_ttl"`   // default 60 s; must not exceed the agents' max_ticket_ttl
		CacheTTL    load.Duration `json:"cache_ttl"`    // default 5 min (notifications invalidate sooner)
		CacheMax    int           `json:"cache_max"`    // default 100 000
		AuthTTL     load.Duration `json:"auth_ttl"`     // API key rows cached; default 10 s
		Timeout     load.Duration `json:"timeout"`      // one resolve; default 3 s
		Concurrency int           `json:"concurrency"`  // resolves at once; more are refused 503; default 256
		MovingRetry load.Duration `json:"moving_retry"` // Retry-After while a database moves; default 1 s
		ClaimRetry  load.Duration `json:"claim_retry"`  // retry_after_ms while a new owner's claim is pending; default 50 ms
		ListenPing  load.Duration `json:"listen_ping"`  // a silent listener connection is pinged after this; default 5 s
		// RateLimit charges each source (client IP, IPv6 /64) one token per API key id not in the key cache
		// (unknown ids are never cached and each costs a primary read). Empty: answer shed + Retry-After.
		RateLimit struct {
			PerSecond  float64 `json:"lookups_per_second"` // default 20
			Burst      float64 `json:"burst"`              // default 50
			MaxSources int     `json:"max_sources"`        // default 100 000
		} `json:"rate_limit"`
	} `json:"locator"`

	// TicketKeys is the issuer's Ed25519 key ring: one file per key in dir (0700, files 0600; never shared with
	// agents, which fetch the public keys from GET /v1/keys). A key is created every rotate, published at
	// once, signs after activate (so agents fetch it first), and is deleted ticket_ttl + 2 min after its
	// successor began signing.
	TicketKeys struct {
		Dir      string        `json:"dir"`      // required
		Rotate   load.Duration `json:"rotate"`   // default 24 h
		Activate load.Duration `json:"activate"` // default 5 min: at least twice the agents' keys_refresh
		Check    load.Duration `json:"check"`    // how often the ring is re-planned; default 1 min
	} `json:"ticket_keys"`

	AgentBreaker struct {
		MinCalls int           `json:"min_calls"`
		CoolDown load.Duration `json:"cool_down"`
	} `json:"agent_breaker"`

	LogFile  string `json:"log_file"`
	LogLevel string `json:"log_level"` // debug | info (default) | warn
}

// AgentConfig is one agent: its VM id, the query endpoint clients reach (host:port, recorded in the
// registry), and its lifecycle listener (a URL).
type AgentConfig struct {
	VM        string `json:"vm"`
	Endpoint  string `json:"endpoint"`
	Lifecycle string `json:"lifecycle"`
}

func defaults() Config {
	d := func(v time.Duration) load.Duration { return load.Duration{Duration: v} }
	c := Config{Listen: "127.0.0.1:7080", AgentTimeout: d(30 * time.Second), HeartbeatInterval: d(2 * time.Second),
		HeartbeatTimeout: d(time.Second), ResumeInterval: d(5 * time.Second), ResumeAfter: d(30 * time.Second), ResumeLimit: 100,
		PerKeyTimeout: d(time.Minute), APITimeout: d(2 * time.Minute), APIConcurrency: 16, ShutdownBudget: d(10 * time.Second),
		LogLevel: "info"}
	c.PG.MaxConns = 20
	c.Locator.TicketTTL, c.Locator.CacheTTL, c.Locator.AuthTTL = d(time.Minute), d(5*time.Minute), d(10*time.Second)
	c.Locator.CacheMax, c.Locator.Timeout, c.Locator.Concurrency = 100_000, d(3*time.Second), 256
	c.Locator.RateLimit.PerSecond, c.Locator.RateLimit.Burst, c.Locator.RateLimit.MaxSources = 20, 50, 100_000
	c.Locator.MovingRetry, c.Locator.ClaimRetry, c.Locator.ListenPing = d(time.Second), d(50*time.Millisecond), d(5*time.Second)
	c.TicketKeys.Rotate, c.TicketKeys.Activate, c.TicketKeys.Check = d(24*time.Hour), d(5*time.Minute), d(time.Minute)
	return c
}

// parseConfig decodes r over the defaults and validates, reporting every problem at once.
func parseConfig(r io.Reader, dir string) (*Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	c := defaults()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	var problems []error
	bad := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	if c.Listen == "" || c.SecretsFile == "" {
		bad("listen and secrets_file are required")
	}
	if c.S3.Endpoint == "" || c.S3.Bucket == "" {
		bad("s3: endpoint and bucket are required (drop deletes the database's objects)")
	}
	if c.TLS != nil && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		bad("tls: cert_file and key_file are both required")
	}
	if c.S3.CAFile != "" && !c.S3.Secure {
		bad("s3: ca_file needs secure: true")
	}
	if c.S3.Prefix != "" && !strings.HasSuffix(c.S3.Prefix, "/") {
		bad("s3: prefix %q must end in /", c.S3.Prefix)
	}
	if len(c.Agents) == 0 {
		bad("agents: at least one")
	}
	seen := map[string]bool{}
	for i, a := range c.Agents {
		if !pg.ValidVM(a.VM) {
			bad("agents[%d]: vm %q is not a valid vm id", i, a.VM)
		}
		if seen[a.VM] {
			bad("agents[%d]: vm %q twice", i, a.VM)
		}
		seen[a.VM] = true
		if u, err := url.Parse(a.Lifecycle); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("agents[%d]: lifecycle %q must be an http(s) URL", i, a.Lifecycle)
		}
		if a.Endpoint == "" {
			bad("agents[%d]: endpoint is required", i)
		}
	}
	if c.TicketKeys.Dir == "" {
		bad("ticket_keys.dir is required (the issuer's signing keys)")
	}
	lc, tk := c.Locator, c.TicketKeys
	for name, d := range map[string]time.Duration{"locator.ticket_ttl": lc.TicketTTL.Duration, "locator.cache_ttl": lc.CacheTTL.Duration,
		"locator.auth_ttl": lc.AuthTTL.Duration, "locator.timeout": lc.Timeout.Duration, "locator.moving_retry": lc.MovingRetry.Duration,
		"locator.claim_retry": lc.ClaimRetry.Duration,
		"locator.listen_ping": lc.ListenPing.Duration, "ticket_keys.rotate": tk.Rotate.Duration, "ticket_keys.check": tk.Check.Duration} {
		if d <= 0 {
			bad("%s must be > 0", name)
		}
	}
	if rl := lc.RateLimit; rl.PerSecond <= 0 || rl.Burst < 1 || rl.MaxSources < 1 {
		bad("locator.rate_limit: lookups_per_second > 0, burst >= 1 and max_sources >= 1 required")
	}
	if lc.CacheMax < 1 || lc.Concurrency < 1 || tk.Activate.Duration < 0 {
		bad("locator.cache_max and locator.concurrency must be >= 1, ticket_keys.activate >= 0")
	}
	if tk.Rotate.Duration <= tk.Activate.Duration {
		bad("ticket_keys.rotate %v must exceed ticket_keys.activate %v", tk.Rotate, tk.Activate)
	}
	if c.Policy.MaxDatabases < 0 || c.Policy.MaxBytes < 0 || c.Policy.MaxHeartbeatAge.Duration < 0 {
		bad("policy: caps must be >= 0")
	}
	for name, d := range map[string]time.Duration{"agent_timeout": c.AgentTimeout.Duration, "heartbeat_interval": c.HeartbeatInterval.Duration,
		"heartbeat_timeout": c.HeartbeatTimeout.Duration, "resume_interval": c.ResumeInterval.Duration, "per_key_timeout": c.PerKeyTimeout.Duration,
		"api_timeout": c.APITimeout.Duration, "shutdown_budget": c.ShutdownBudget.Duration} {
		if d <= 0 {
			bad("%s must be > 0", name)
		}
	}
	if c.ResumeAfter.Duration < 0 || c.ResumeLimit < 1 || c.APIConcurrency < 1 || c.PG.MaxConns < 1 {
		bad("resume_after must be >= 0; resume_limit, api_concurrency and pg.max_conns >= 1")
	}
	if c.HeartbeatTimeout.Duration >= c.HeartbeatInterval.Duration {
		bad("heartbeat_timeout %v must be shorter than heartbeat_interval %v", c.HeartbeatTimeout, c.HeartbeatInterval)
	}
	if c.PerKeyTimeout.Duration < c.AgentTimeout.Duration {
		bad("per_key_timeout %v must be at least agent_timeout %v (a key's flow makes several agent calls)", c.PerKeyTimeout, c.AgentTimeout)
	}
	if c.Policy.MaxHeartbeatAge.Duration == 0 {
		c.Policy.MaxHeartbeatAge.Duration = 3 * c.HeartbeatInterval.Duration
	}
	if bc := (breaker.Config{MinCalls: c.AgentBreaker.MinCalls, CoolDown: c.AgentBreaker.CoolDown.Duration}); bc.Normalize() != nil {
		bad("agent_breaker: min_calls and cool_down must be >= 0")
	}
	switch c.LogLevel {
	case "debug", "info", "warn":
	default:
		bad("log_level %q: want debug, info or warn", c.LogLevel)
	}
	paths := []*string{&c.SecretsFile, &c.LogFile, &c.S3.CAFile, &c.TicketKeys.Dir}
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

// checkDev refuses, unless -dev is given, plain-HTTP S3 (the gc identity's keys would cross the network in the
// clear; object-storage guide S1, S2; OS-11) and a plain-HTTP listener (admin tokens and API keys in the clear,
// and whoever answers GET /v1/keys in its place can mint tickets).
func (c *Config) checkDev() error {
	switch {
	case c.Dev:
	case !c.S3.Secure:
		return errs.Wrap(module, nil, "s3.secure is false: plain-HTTP S3 is for local development only; set \"secure\": true "+
			"(and \"ca_file\" for a private CA) in the s3 section, or pass -dev for a local run").With("endpoint", c.S3.Endpoint)
	case c.TLS == nil:
		return errs.Wrap(module, nil, "no tls section: the API (admin tokens, api keys, ticket keys) in plain HTTP is for local "+
			"development only; add \"tls\": {\"cert_file\", \"key_file\"}, or pass -dev for a local run").With("listen", c.Listen)
	}
	return nil
}

// Secrets is controld's secrets file (0600; object-storage guide S2, RI-16).
type Secrets struct {
	PGDSN        string `json:"pg_dsn"`
	ControlToken string `json:"control_token"` // the agents' lifecycle token
	AdminToken   string `json:"admin_token"`   // this API's token (operators, scripts)
	S3AccessKey  string `json:"s3_access_key"` // the gc identity
	S3SecretKey  string `json:"s3_secret_key"`
	// PGReplicaDSN, if set, is a read replica the locator reads placement from (stale is safe; min_epoch
	// and notified keys read the primary). Default: the primary.
	PGReplicaDSN string `json:"pg_replica_dsn"`
	// EdgeToken, if set (at least 16 characters), is the edges' token for GET /v1/agents (the VM registry's
	// query endpoints). Without it that endpoint is off.
	EdgeToken string `json:"edge_token"`
}

const minToken = 16

// loadSecrets reads the secrets file, refusing one others can read, unknown fields and missing values.
// Errors never include a secret.
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
		return nil, errs.Wrap(module, nil, "secrets_file %s has mode %#o; make it 0600 (chmod 600)", path, perm)
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var s Secrets
	if dec.Decode(&s) != nil {
		return nil, errs.Wrap(module, nil, "secrets_file %s is not valid: want a JSON object with pg_dsn, control_token, admin_token, s3_access_key, s3_secret_key", path)
	}
	switch {
	case s.PGDSN == "" || s.S3AccessKey == "" || s.S3SecretKey == "":
		return nil, errs.Wrap(module, nil, "secrets_file %s: pg_dsn, s3_access_key and s3_secret_key are required", path)
	case len(s.ControlToken) < minToken || len(s.AdminToken) < minToken || s.EdgeToken != "" && len(s.EdgeToken) < minToken:
		return nil, errs.Wrap(module, nil, "secrets_file %s: control_token, admin_token and edge_token (if set) must be at least %d characters",
			path, minToken)
	}
	return &s, nil
}
