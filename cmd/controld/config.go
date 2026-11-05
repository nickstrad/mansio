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
)

// Config is controld's JSON config. Defaults are applied before decoding; unknown fields are errors;
// relative paths are relative to the config file's directory.
type Config struct {
	Listen      string `json:"listen"`       // the control API and /livez, /readyz, /metrics; default 127.0.0.1:7080
	SecretsFile string `json:"secrets_file"` // 0600: pg_dsn, control_token, admin_token, s3 keys (required)

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
	} `json:"s3"`

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
	for _, p := range []*string{&c.SecretsFile, &c.LogFile} {
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

// Secrets is controld's secrets file (0600; object-storage guide S2, RI-16).
type Secrets struct {
	PGDSN        string `json:"pg_dsn"`
	ControlToken string `json:"control_token"` // the agents' lifecycle token
	AdminToken   string `json:"admin_token"`   // this API's token (operators, scripts)
	S3AccessKey  string `json:"s3_access_key"` // the gc identity
	S3SecretKey  string `json:"s3_secret_key"`
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
	case len(s.ControlToken) < minToken || len(s.AdminToken) < minToken:
		return nil, errs.Wrap(module, nil, "secrets_file %s: control_token and admin_token must be at least %d characters", path, minToken)
	}
	return &s, nil
}
