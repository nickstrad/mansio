package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/load"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// Config is agentd's JSON config. Defaults are applied before decoding (a file overrides them); unknown fields
// are errors. Relative paths are relative to the config file's directory.
type Config struct {
	Listen       string   `json:"listen"`        // query listener; default 127.0.0.1:7070 (":0" picks a port)
	HealthListen string   `json:"health_listen"` // optional second listener for /healthz only
	Protocols    []string `json:"protocols"`     // "h1", "h2c"; default both
	VM           string   `json:"vm"`            // this VM's id: the ticket audience (required)

	DBDir string   `json:"db_dir"` // directory of <db>.db files (required)
	DBs   []string `json:"dbs"`    // serve only these; default every *.db in db_dir

	KeysFile     string        `json:"keys_file"` // ticket.PublicKeyFile JSON (required)
	ClockSkew    load.Duration `json:"clock_skew"`
	MaxTicketTTL load.Duration `json:"max_ticket_ttl"`
	TicketCache  int           `json:"ticket_cache"` // verified tickets remembered; 0 = off

	// Until fencing's owner store lands, the admitter is this static table: every served database is owned at
	// default_epoch unless epochs overrides it.
	DefaultEpoch uint64            `json:"default_epoch"`
	Epochs       map[string]uint64 `json:"epochs"`

	Admission         front.AdmissionLimits `json:"admission"`
	Limits            front.Limits          `json:"limits"`
	DefaultTimeout    load.Duration         `json:"default_timeout"`
	MaxTimeout        load.Duration         `json:"max_timeout"`
	StatementTimeout  load.Duration         `json:"statement_timeout"`
	ReplyGrace        load.Duration         `json:"reply_grace"`
	DrainTimeout      load.Duration         `json:"drain_timeout"`
	ReadHeaderTimeout load.Duration         `json:"read_header_timeout"`
	IdleTimeout       load.Duration         `json:"idle_timeout"`
	Actor             load.ActorConfig      `json:"actor"`

	LogFile  string `json:"log_file"`  // JSON log; default stderr
	LogLevel string `json:"log_level"` // debug | info (default) | warn
}

func defaults() Config {
	d := func(v time.Duration) load.Duration { return load.Duration{Duration: v} }
	return Config{
		Listen: "127.0.0.1:7070", Protocols: []string{"h1", "h2c"},
		ClockSkew: d(5 * time.Second), MaxTicketTTL: d(10 * time.Minute), TicketCache: 65536, DefaultEpoch: 1,
		Admission: front.AdmissionLimits{}.WithDefaults(), Limits: front.Limits{}.WithDefaults(),
		DefaultTimeout: d(2 * time.Second), MaxTimeout: d(30 * time.Second), StatementTimeout: d(time.Second),
		ReplyGrace: d(time.Second), DrainTimeout: d(10 * time.Second), ReadHeaderTimeout: d(5 * time.Second),
		IdleTimeout: d(2 * time.Minute), LogLevel: "info",
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
	if c.DBDir == "" {
		bad("db_dir is required")
	}
	if c.KeysFile == "" {
		bad("keys_file is required")
	}
	if c.Listen == "" {
		bad("listen is required")
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
		"read_header_timeout": c.ReadHeaderTimeout.Duration, "idle_timeout": c.IdleTimeout.Duration} {
		if d <= 0 {
			bad("%s must be > 0", name)
		}
	}
	if c.DefaultTimeout.Duration > c.MaxTimeout.Duration {
		bad("default_timeout %v is over max_timeout %v", c.DefaultTimeout, c.MaxTimeout)
	}
	if c.Limits.MaxBody <= 0 || c.Limits.MaxSQL <= 0 || c.Limits.MaxArgs <= 0 || c.Limits.MaxRows <= 0 {
		bad("limits must all be > 0")
	}
	if c.Admission.TenantInFlight <= 0 || c.Admission.TenantQueue < 0 || c.Admission.DBInFlight <= 0 || c.Admission.DBQueue < 0 {
		bad("admission: in-flight caps must be > 0 and queues >= 0")
	}
	if err := c.Actor.Actor().Validate(); err != nil {
		bad("actor: %w", err)
	}
	switch c.LogLevel {
	case "debug", "info", "warn":
	default:
		bad("log_level %q: want debug, info or warn", c.LogLevel)
	}
	for _, p := range []*string{&c.DBDir, &c.KeysFile, &c.LogFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	return &c, errors.Join(problems...)
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
