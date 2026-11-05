package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/load"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// Config is the edge's JSON config. Defaults are applied before decoding; unknown fields are errors. Relative
// paths are relative to the config file's directory.
type Config struct {
	Listen       string `json:"listen"`        // client listener; default 127.0.0.1:7443
	HealthListen string `json:"health_listen"` // lifecycle listener (/livez, /readyz, /metrics); default 127.0.0.1:7444
	// TLS is the client-facing server certificate. Required unless -dev (then the listener speaks h1 + h2c).
	TLS *TLSFiles `json:"tls"`

	// KeysFile is the issuer's public keys (ticket.PublicKeyFile). The seam for CP-5's key endpoint: the edge
	// holds them in a ticket.KeyRing, which a fetcher can Replace while serving.
	KeysFile     string        `json:"keys_file"`
	ClockSkew    load.Duration `json:"clock_skew"`
	MaxTicketTTL load.Duration `json:"max_ticket_ttl"`
	TicketCache  int           `json:"ticket_cache"` // verified tickets remembered; 0 = off
	// RateLimit charges each source (client IP, IPv6 /64) one token per ticket the edge must verify.
	RateLimit   RateLimit `json:"rate_limit"`
	VerifySlots int       `json:"verify_slots"` // full verifies at once; 0 = 2 × GOMAXPROCS

	// Agents maps each VM id (the tickets' vm claim) to its agent's query listener, e.g. https://10.0.0.5:7070.
	// The seam for controld's VM registry: edge.Resolver.
	Agents   map[string]string `json:"agents"`
	AgentTLS AgentTLS          `json:"agent_tls"`
	// AgentInFlight bounds requests in flight per agent (0 = 4096); more are shed.
	AgentInFlight int           `json:"agent_in_flight"`
	AgentBreaker  BreakerConfig `json:"agent_breaker"`
	DialTimeout   load.Duration `json:"dial_timeout"`  // connect to an agent; default 2 s
	TLSHandshake  load.Duration `json:"tls_handshake"` // handshake with an agent; default 3 s

	DefaultTimeout    load.Duration `json:"default_timeout"` // as the agents' (default 2 s / 30 s)
	MaxTimeout        load.Duration `json:"max_timeout"`
	ReplyGrace        load.Duration `json:"reply_grace"` // past the deadline, for the agent's own expired answer; default 1 s
	MaxBody           int           `json:"max_body"`    // request body bytes; default 1 MiB (the agents' limits.max_body)
	BodyReadTimeout   load.Duration `json:"body_read_timeout"`
	ReadHeaderTimeout load.Duration `json:"read_header_timeout"`
	IdleTimeout       load.Duration `json:"idle_timeout"`
	DrainTimeout      load.Duration `json:"drain_timeout"`   // in-flight requests finish within this at shutdown
	ShutdownBudget    load.Duration `json:"shutdown_budget"` // the whole shutdown

	LogFile  string `json:"log_file"`
	LogLevel string `json:"log_level"` // debug | info | warn

	// Dev is the -dev flag: plain HTTP to clients (no tls section) and to agents (http:// URLs) allowed.
	Dev bool `json:"-"`
}

// TLSFiles is a server certificate and its key (PEM).
type TLSFiles struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

// AgentTLS is how the edge checks agents' certificates.
type AgentTLS struct {
	CAFile string `json:"ca_file"` // PEM bundle of the private CA that signs agent certificates; default system roots
	// ServerName, if set, is the name every agent certificate must carry (one shared agent name), instead of
	// each agent URL's host.
	ServerName string `json:"server_name"`
}

// RateLimit is the per-source token bucket.
type RateLimit struct {
	PerSecond  float64 `json:"verifies_per_second"` // default 20
	Burst      float64 `json:"burst"`               // default 50
	MaxSources int     `json:"max_sources"`         // default 100,000
}

// BreakerConfig is a breaker's thresholds in a config file; zero fields take Release It §P.
type BreakerConfig struct {
	Window      load.Duration `json:"window"`
	MinCalls    int           `json:"min_calls"`
	FailureRate float64       `json:"failure_rate"`
	CoolDown    load.Duration `json:"cool_down"`
}

func (b BreakerConfig) config() breaker.Config {
	return breaker.Config{Window: b.Window.Duration, MinCalls: b.MinCalls, FailureRate: b.FailureRate, CoolDown: b.CoolDown.Duration}
}

func defaults() Config {
	d := func(v time.Duration) load.Duration { return load.Duration{Duration: v} }
	return Config{
		Listen: "127.0.0.1:7443", HealthListen: "127.0.0.1:7444",
		ClockSkew: d(5 * time.Second), MaxTicketTTL: d(10 * time.Minute), TicketCache: 65536,
		RateLimit:   RateLimit{PerSecond: 20, Burst: 50, MaxSources: 100_000},
		DialTimeout: d(2 * time.Second), TLSHandshake: d(3 * time.Second),
		DefaultTimeout: d(2 * time.Second), MaxTimeout: d(30 * time.Second), ReplyGrace: d(time.Second), MaxBody: 1 << 20,
		BodyReadTimeout: d(5 * time.Second), ReadHeaderTimeout: d(5 * time.Second), IdleTimeout: d(2 * time.Minute),
		DrainTimeout: d(10 * time.Second), ShutdownBudget: d(30 * time.Second), LogLevel: "info",
	}
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
	if c.Listen == "" || c.HealthListen == "" {
		bad("listen and health_listen are required (health never shares the client listener: Release It B4)")
	}
	if c.TLS != nil && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		bad("tls: cert_file and key_file are both required")
	}
	if c.KeysFile == "" {
		bad("keys_file is required")
	}
	if len(c.Agents) == 0 {
		bad("agents: at least one vm → URL")
	}
	for vm, u := range c.Agents {
		if vm == "" || len(vm) > ticket.MaxVM {
			bad("agents: vm id %q must be 1..%d bytes", vm, ticket.MaxVM)
		}
		if err := checkAgentURL(u); err != nil {
			bad("agents[%s]: %v", vm, err)
		}
	}
	if rl := c.RateLimit; rl.PerSecond <= 0 || rl.Burst < 1 || rl.MaxSources < 1 {
		bad("rate_limit: verifies_per_second > 0, burst >= 1 and max_sources >= 1 required")
	}
	if c.TicketCache < 0 || c.VerifySlots < 0 || c.AgentInFlight < 0 || c.MaxBody <= 0 {
		bad("ticket_cache, verify_slots and agent_in_flight must be >= 0 (0 = default), max_body > 0")
	}
	if bc := c.AgentBreaker.config(); bc.Normalize() != nil {
		bad("agent_breaker: invalid thresholds (window, min_calls >= 1, failure_rate in (0, 1], cool_down)")
	}
	for name, d := range map[string]time.Duration{"clock_skew": c.ClockSkew.Duration, "max_ticket_ttl": c.MaxTicketTTL.Duration} {
		if d < 0 {
			bad("%s must be >= 0", name)
		}
	}
	for name, d := range map[string]time.Duration{"dial_timeout": c.DialTimeout.Duration, "tls_handshake": c.TLSHandshake.Duration,
		"default_timeout": c.DefaultTimeout.Duration, "max_timeout": c.MaxTimeout.Duration, "reply_grace": c.ReplyGrace.Duration,
		"body_read_timeout": c.BodyReadTimeout.Duration, "read_header_timeout": c.ReadHeaderTimeout.Duration,
		"idle_timeout": c.IdleTimeout.Duration, "drain_timeout": c.DrainTimeout.Duration, "shutdown_budget": c.ShutdownBudget.Duration} {
		if d <= 0 {
			bad("%s must be > 0", name)
		}
	}
	if c.DefaultTimeout.Duration > c.MaxTimeout.Duration {
		bad("default_timeout %v is over max_timeout %v", c.DefaultTimeout, c.MaxTimeout)
	}
	if c.DrainTimeout.Duration > c.ShutdownBudget.Duration {
		bad("drain_timeout %v is over shutdown_budget %v (the drain is part of the shutdown)", c.DrainTimeout, c.ShutdownBudget)
	}
	switch c.LogLevel {
	case "debug", "info", "warn":
	default:
		bad("log_level %q: want debug, info or warn", c.LogLevel)
	}
	paths := []*string{&c.KeysFile, &c.LogFile, &c.AgentTLS.CAFile}
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

// checkAgentURL accepts scheme://host:port with an http or https scheme and nothing else.
func checkAgentURL(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return fmt.Errorf("%q is not a URL", s)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("%q: scheme must be https (http only with -dev)", s)
	case u.Host == "" || u.Port() == "":
		return fmt.Errorf("%q: want scheme://host:port", s)
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		return fmt.Errorf("%q: no path, query or credentials (the edge appends /v1/query)", s)
	}
	return nil
}

// checkDev refuses what only local development may do unless -dev is given: plain HTTP to clients or to agents
// (object-storage guide S2's rule for S3, applied to the edge's two hops), and a CA file nothing would use.
func (c *Config) checkDev() error {
	plain := 0
	for _, u := range c.Agents {
		if pu, _ := url.Parse(u); pu.Scheme == "http" {
			plain++
		}
	}
	switch {
	case c.AgentTLS.CAFile != "" && plain == len(c.Agents):
		return errs.Wrap(module, nil, "agent_tls.ca_file is set but every agent URL is plain http (the CA would be unused)")
	case c.Dev:
		return nil
	case c.TLS == nil:
		return errs.Wrap(module, nil, "no tls section: plain HTTP to clients is for local development only; add \"tls\": "+
			"{\"cert_file\", \"key_file\"}, or pass -dev for a local run")
	case plain > 0:
		return errs.Wrap(module, nil, "%d agent URLs are plain http: the edge → agent hop needs https (with agent_tls.ca_file for a "+
			"private CA) outside local development; pass -dev for a local run", plain)
	}
	return nil
}

// serverTLS loads the client-facing certificate: TLS 1.2 or later, h2 and http/1.1 by ALPN.
func (c *Config) serverTLS() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.TLS.CertFile, c.TLS.KeyFile)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot load the tls certificate").With("cert_file", c.TLS.CertFile)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}, nil
}

// agentTransport is the one transport to every agent (it pools connections): dial and handshake bounded,
// each request's ctx bounds the rest; h2 over TLS, h1 over plain HTTP (-dev); TLS 1.2 or later trusting
// agent_tls.ca_file if set.
func (c *Config) agentTransport() (*http.Transport, error) {
	t := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: c.DialTimeout.Duration, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: c.TLSHandshake.Duration,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.AgentTLS.ServerName},
	}
	if c.AgentTLS.CAFile == "" {
		return t, nil
	}
	pem, err := os.ReadFile(c.AgentTLS.CAFile)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot read agent_tls.ca_file").With("ca_file", c.AgentTLS.CAFile)
	}
	t.TLSClientConfig.RootCAs = x509.NewCertPool()
	if !t.TLSClientConfig.RootCAs.AppendCertsFromPEM(pem) {
		return nil, errs.Wrap(module, nil, "agent_tls.ca_file %s holds no PEM certificate", c.AgentTLS.CAFile).With("ca_file", c.AgentTLS.CAFile)
	}
	return t, nil
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
