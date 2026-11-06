// Package load holds the benchmark configuration, the rate schedule and the request picker.
//
// A run is a list of phases. Each phase sets how many SQLite databases are active and how the request rate
// moves from start to end over the phase's duration (linear ramp, constant step, or sine wave). Ramping up,
// holding and ramping down are just consecutive phases. Requests are plain SQL strings, generated per database
// from its random schema (see packages schema and workload).
//
// Everything here is pure except LoadFile, which reads the file.
package load

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/outcome"
	"github.com/nickstrad/mansio/server/internal/retry"
	"github.com/nickstrad/mansio/server/internal/schema"
	"github.com/nickstrad/mansio/server/internal/workload"
)

// Duration is a time.Duration that reads "250ms", "10s", "2m" from JSON.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// DistKind names how requests spread over databases.
type DistKind string

const (
	Uniform DistKind = "uniform"
	Zipf    DistKind = "zipf" // low database ids are hot
)

// Distribution chooses which database a request goes to.
type Distribution struct {
	Kind DistKind `json:"kind"` // default uniform
	S    float64  `json:"s"`    // zipf skew, > 1; default 1.1
}

func (d *Distribution) normalize() error {
	switch d.Kind {
	case "", Uniform:
		d.Kind = Uniform
	case Zipf:
		if d.S == 0 {
			d.S = 1.1
		}
		if d.S <= 1 {
			return fmt.Errorf("zipf s must be > 1, got %g", d.S)
		}
	default:
		return fmt.Errorf("kind %q: want uniform or zipf", d.Kind)
	}
	return nil
}

// ActorConfig is the config file's "actor" section; Actor converts it. Zero fields take actor's defaults.
type ActorConfig struct {
	MailboxSize int      `json:"mailbox_size"`
	MaxBatch    int      `json:"max_batch"`
	ReadConns   int      `json:"read_conns"`   // read connections per database; 0 = reads share the write connection
	JournalMode string   `json:"journal_mode"` // WAL (default), DELETE, TRUNCATE, PERSIST, MEMORY, OFF
	IdleClose   Duration `json:"idle_close"`
	Synchronous string   `json:"synchronous"`
	CacheKiB    int      `json:"cache_kib"`
	BusyTimeout Duration `json:"busy_timeout"`
	StmtCache   int      `json:"stmt_cache"`
}

// Actor is the actor.Config this section describes, with defaults applied.
func (a ActorConfig) Actor() actor.Config {
	return actor.Config{
		MailboxSize: a.MailboxSize, MaxBatch: a.MaxBatch, ReadConns: a.ReadConns, IdleClose: a.IdleClose.Duration,
		JournalMode: a.JournalMode, Synchronous: a.Synchronous, CacheKiB: a.CacheKiB,
		BusyTimeout: a.BusyTimeout.Duration, StmtCache: a.StmtCache,
	}.WithDefaults()
}

// RetryConfig is a client profile's retry policy as written in the config (see retry.Policy). A profile without
// max_attempts never retries; one that retries gets retry.Policy's safe defaults for anything it leaves out, so
// a badly behaved bench profile must ask for "none" backoff and jitter explicitly.
type RetryConfig struct {
	MaxAttempts  int           `json:"max_attempts"` // including the first; default 1 (no retries)
	Backoff      retry.Backoff `json:"backoff"`      // none | fixed | exponential (default)
	Base         Duration      `json:"base"`         // first delay; default 50ms
	Max          Duration      `json:"max"`          // cap; default 5s
	Jitter       retry.Jitter  `json:"jitter"`       // none | full (default) | equal | decorrelated
	RetryOn      []string      `json:"retry_on"`     // outcome names (see package outcome) or "any"; default shed, aborted
	BudgetRatio  float64       `json:"budget_ratio"` // max retries per first attempt; 0 = unlimited
	TotalTimeout Duration      `json:"total_timeout"`
}

func (r RetryConfig) policy() (retry.Policy, error) {
	on, err := outcome.ParseSet(r.RetryOn)
	if err != nil {
		return retry.Policy{}, fmt.Errorf("retry_on: %w", err)
	}
	p := retry.Policy{MaxAttempts: max(r.MaxAttempts, 1), Backoff: r.Backoff, Base: r.Base.Duration, Max: r.Max.Duration,
		Jitter: r.Jitter, RetryOn: on, BudgetRatio: r.BudgetRatio, TotalTimeout: r.TotalTimeout.Duration}
	return p, p.Normalize()
}

// ClientProfile is one kind of client. Each request is issued by a profile chosen by share.
type ClientProfile struct {
	Name  string      `json:"name"`
	Share float64     `json:"share"` // relative weight among profiles
	Retry RetryConfig `json:"retry"`

	policy retry.Policy // built by Normalize
}

// Policy is the normalized retry policy.
//
//	pre: the config was normalized
func (c *ClientProfile) Policy() retry.Policy { return c.policy }

// MaxClients bounds the profile count (a record stores the profile index in a byte).
const MaxClients = 255

// Config is the whole benchmark.
type Config struct {
	Name    string `json:"name"`
	OutDir  string `json:"out_dir"`  // default results/<name>-<timestamp>
	DBDir   string `json:"db_dir"`   // default <tmp>/mansio-server/<schema fingerprint>
	KeepDBs bool   `json:"keep_dbs"` // keep databases after the run (a rerun with the same seed/schema reuses them)

	SetupWorkers     int      `json:"setup_workers"`     // parallel seeding/opening; default NumCPU
	Workers          int      `json:"workers"`           // max requests in flight; default 1024
	StatementTimeout Duration `json:"statement_timeout"` // per statement, interrupts SQLite; default 200ms
	RequestTimeout   Duration `json:"request_timeout"`   // per request incl. queueing; default 2s
	SampleEvery      Duration `json:"sample_every"`      // resource sampling interval; default 250ms
	RecordFraction   float64  `json:"record_fraction"`   // fraction of requests written to requests.csv, in (0, 1]; default 1
	Seed             uint64   `json:"seed"`              // RNG seed for schemas, data and load; default 1

	Actor        ActorConfig                `json:"actor"`
	Schema       schema.Options             `json:"schema"`
	Workload     workload.Options           `json:"workload"`
	Mix          map[workload.Class]float64 `json:"mix"`          // class → weight; default workload.DefaultMix()
	Distribution Distribution               `json:"distribution"` // default uniform
	Clients      []ClientProfile            `json:"clients"`      // default: one profile that never retries
	Phases       []Phase                    `json:"phases"`
}

// Defaults is the config before the file is applied. Maps and lists stay nil so a file replaces them rather than
// merging into them; Normalize fills those.
func Defaults() Config {
	return Config{
		Name:             "run",
		SetupWorkers:     runtime.NumCPU(),
		Workers:          1024,
		StatementTimeout: Duration{200 * time.Millisecond},
		RequestTimeout:   Duration{2 * time.Second},
		SampleEvery:      Duration{250 * time.Millisecond},
		RecordFraction:   1,
		Seed:             1,
		Schema:           schema.DefaultOptions(),
		Workload:         workload.DefaultOptions(),
	}
}

// Parse reads a config from r over Defaults, then normalizes it. Unknown fields are an error, so typos fail.
func Parse(r io.Reader) (*Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	c := Defaults()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadFile reads, defaults and validates a config file.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errs.Wrap("load", err, "cannot open config").With("path", path)
	}
	defer f.Close()
	c, err := Parse(f)
	if err != nil {
		return nil, errs.Wrap("load", err, "invalid config %s", path).With("path", path)
	}
	return c, nil
}

// Normalize fills the derived defaults (mix, clients, per-phase inheritance, qpm → qps) and validates
// everything, reporting every problem at once.
//
//	post: err == nil ⇒ every phase has DBs > 0, Duration > 0, a Shape, Mix, ClientShares and Distribution, and
//	      every client has a normalized policy.
func (c *Config) Normalize() error {
	var problems []error
	bad := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }

	if c.Workers < 1 || c.SetupWorkers < 1 {
		bad("workers and setup_workers must be >= 1")
	}
	if c.StatementTimeout.Duration <= 0 || c.RequestTimeout.Duration <= 0 || c.SampleEvery.Duration <= 0 {
		bad("statement_timeout, request_timeout and sample_every must be > 0")
	}
	if c.RecordFraction <= 0 || c.RecordFraction > 1 {
		bad("record_fraction must be in (0, 1], got %g", c.RecordFraction)
	}
	if err := c.Actor.Actor().Validate(); err != nil {
		bad("actor: %w", err)
	}
	if err := c.Schema.Validate(); err != nil {
		bad("schema: %w", err)
	}
	if err := c.Workload.Validate(); err != nil {
		bad("workload: %w", err)
	}
	if c.Mix == nil {
		c.Mix = workload.DefaultMix()
	}
	if err := validateWeights(c.Mix, isClass); err != nil {
		bad("mix: %w", err)
	}
	if err := c.Distribution.normalize(); err != nil {
		bad("distribution: %w", err)
	}

	if len(c.Clients) == 0 {
		c.Clients = []ClientProfile{{Name: "default", Share: 1}}
	}
	if len(c.Clients) > MaxClients {
		bad("clients: at most %d profiles", MaxClients)
	}
	shares := map[string]float64{}
	for i := range c.Clients {
		cp := &c.Clients[i]
		if cp.Name == "" {
			cp.Name = fmt.Sprintf("client-%d", i)
		}
		if _, dup := shares[cp.Name]; dup {
			bad("clients[%d]: duplicate name %q", i, cp.Name)
		}
		if strings.ContainsAny(cp.Name, ",\"\r\n") {
			bad("clients[%d]: name %q may not contain commas, quotes or newlines (it is a CSV field)", i, cp.Name)
		}
		shares[cp.Name] = cp.Share
		var err error
		if cp.policy, err = cp.Retry.policy(); err != nil {
			bad("clients[%d].retry: %w", i, err)
		}
	}
	isClient := func(n string) bool { _, ok := shares[n]; return ok }
	if err := validateWeights(shares, isClient); err != nil {
		bad("clients: shares: %w", err)
	}

	if len(c.Phases) == 0 {
		bad("phases: at least one phase is required")
	}
	for i := range c.Phases {
		p := &c.Phases[i]
		if p.Name == "" {
			p.Name = fmt.Sprintf("phase-%d", i)
		}
		if p.Mix == nil {
			p.Mix = c.Mix
		}
		if p.ClientShares == nil {
			p.ClientShares = shares
		}
		if p.Distribution == nil {
			d := c.Distribution
			p.Distribution = &d
		}
		for _, err := range p.normalize(isClient) {
			bad("phases[%d] %q: %w", i, p.Name, err)
		}
	}
	return errors.Join(problems...)
}

// MaxDBs is the largest database count any phase uses.
func (c *Config) MaxDBs() int {
	m := 0
	for _, p := range c.Phases {
		m = max(m, p.DBs)
	}
	return m
}

// TotalDuration is the sum of phase durations (setup time excluded).
func (c *Config) TotalDuration() time.Duration {
	var d time.Duration
	for _, p := range c.Phases {
		d += p.Duration.Duration
	}
	return d
}

// ClientNames lists the profiles in index order.
func (c *Config) ClientNames() []string {
	out := make([]string, len(c.Clients))
	for i, cp := range c.Clients {
		out[i] = cp.Name
	}
	return out
}

// Warnings lists legal but probably unintended settings.
func (c *Config) Warnings() []string { return c.Actor.Actor().Warnings() }

// FormatWeights renders weights stably ("a=1 b=2"), for logs and CSV.
func FormatWeights[K ~string](m map[K]float64) string {
	parts := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s=%g", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func isClass(c workload.Class) bool { _, ok := workload.Lookup(c); return ok }

// validateWeights checks every key is known, no weight is negative, and some weight is positive.
func validateWeights[K ~string](m map[K]float64, known func(K) bool) error {
	total := 0.0
	for _, k := range slices.Sorted(maps.Keys(m)) {
		switch w := m[k]; {
		case !known(k):
			return fmt.Errorf("unknown name %q", k)
		case w < 0:
			return fmt.Errorf("weight for %q is negative", k)
		default:
			total += w
		}
	}
	if total <= 0 {
		return errors.New("weights must sum to > 0")
	}
	return nil
}
