package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/edge"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/keyfetch"
	"github.com/nickstrad/mansio/server/internal/load"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/tlsx"
)

// Config is the edge's JSON config. Defaults are applied before decoding; unknown fields are errors. Relative
// paths are relative to the config file's directory.
type Config struct {
	Listen       string `json:"listen"`        // client listener; default 127.0.0.1:7443
	HealthListen string `json:"health_listen"` // lifecycle listener (/livez, /readyz, /metrics); default 127.0.0.1:7444
	// TLS is the client-facing server certificate. Required unless -dev (then the listener speaks h1 + h2c).
	TLS *TLSFiles `json:"tls"`

	// Ticket public keys, as agentd: keys_file (static), or keys_url (controld's GET /v1/keys, fetched every
	// keys_refresh by internal/keyfetch; keys_file is then the cache). Plain-HTTP keys_url needs -dev.
	KeysFile string `json:"keys_file"`
	KeysURL  string `json:"keys_url"`
	// ControlCAFile is the PEM CA bundle that signs controld's certificate (an https keys_url or registry url
	// with a private CA); default the system roots.
	ControlCAFile string        `json:"control_ca_file"`
	KeysRefresh   load.Duration `json:"keys_refresh"` // default 30 s
	KeysTimeout   load.Duration `json:"keys_timeout"` // one fetch; default 3 s
	ClockSkew     load.Duration `json:"clock_skew"`
	MaxTicketTTL  load.Duration `json:"max_ticket_ttl"`
	TicketCache   int           `json:"ticket_cache"` // verified tickets remembered; 0 = off
	// RateLimit charges each source (client IP, IPv6 /64) one token per ticket the edge must verify.
	RateLimit   RateLimit `json:"rate_limit"`
	VerifySlots int       `json:"verify_slots"` // full verifies at once; 0 = 2 × GOMAXPROCS

	// Agents maps each VM id (the tickets' vm claim) to its agent's query listener, e.g. https://10.0.0.5:7070
	// (static routes), unless Registry is set: exactly one of the two.
	Agents map[string]string `json:"agents"`
	// Registry fetches the routes from controld's VM registry instead (edge.RegistryRoutes).
	Registry *Registry `json:"registry"`
	AgentTLS AgentTLS  `json:"agent_tls"`
	// AgentInFlight bounds requests in flight per agent (0 = 4096); more are shed.
	AgentInFlight int `json:"agent_in_flight"`
	// MaxInFlight bounds requests in the edge at once across agents (0 = 2 × agent_in_flight); more are shed
	// before their body is read. Memory ceiling for request bodies: max_in_flight × max_body.
	MaxInFlight  int           `json:"max_in_flight"`
	AgentBreaker BreakerConfig `json:"agent_breaker"`
	DialTimeout  load.Duration `json:"dial_timeout"`  // connect to an agent; default 2 s
	TLSHandshake load.Duration `json:"tls_handshake"` // handshake with an agent; default 3 s

	DefaultTimeout    load.Duration `json:"default_timeout"` // as the agents' (default 2 s / 30 s)
	MaxTimeout        load.Duration `json:"max_timeout"`
	ReplyGrace        load.Duration `json:"reply_grace"` // past the deadline, for the agent's own expired answer; default 1 s
	MaxBody           int           `json:"max_body"`    // request body bytes; default 1 MiB (the agents' limits.max_body)
	BodyReadTimeout   load.Duration `json:"body_read_timeout"`
	ReadHeaderTimeout load.Duration `json:"read_header_timeout"`
	IdleTimeout       load.Duration `json:"idle_timeout"`
	// DrainDelay is how long the edge keeps serving after /readyz starts failing at shutdown, before it stops
	// accepting: set it to at least the load balancer's readiness poll interval so the balancer stops routing
	// here before the listener closes. Default 0 (stop accepting at once). Part of shutdown_budget.
	DrainDelay     load.Duration `json:"drain_delay"`
	DrainTimeout   load.Duration `json:"drain_timeout"`   // in-flight requests finish within this at shutdown
	ShutdownBudget load.Duration `json:"shutdown_budget"` // the whole shutdown

	LogFile  string `json:"log_file"`
	LogLevel string `json:"log_level"` // debug | info | warn

	// Dev is the -dev flag: plain HTTP to clients (no tls section) and to agents (http:// URLs) allowed.
	Dev bool `json:"-"`
}

// TLSFiles is a server certificate and its key (PEM).
type TLSFiles = tlsx.Files

// Registry is controld's GET /v1/agents: VM → query endpoint (host:port), polled every refresh (and sooner when a
// verified ticket names an unknown VM), each fetch bounded by timeout and behind a breaker. A failed fetch keeps
// the last routes.
type Registry struct {
	URL         string        `json:"url"`
	TokenFile   string        `json:"token_file"`   // 0600; controld's edge_token
	Refresh     load.Duration `json:"refresh"`      // default 10 s
	Timeout     load.Duration `json:"timeout"`      // one fetch; default 2 s
	AgentScheme string        `json:"agent_scheme"` // the agents' scheme: https (default), http only with -dev
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
		KeysRefresh: d(30 * time.Second), KeysTimeout: d(3 * time.Second), ClockSkew: d(5 * time.Second), MaxTicketTTL: d(10 * time.Minute), TicketCache: 65536,
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
	if c.ControlCAFile != "" && !strings.HasPrefix(c.KeysURL, "https:") && (c.Registry == nil || !strings.HasPrefix(c.Registry.URL, "https:")) {
		bad("control_ca_file needs an https keys_url or registry.url (it would be unused)")
	}
	if r := c.Registry; r != nil { // a pointer section: its defaults apply after decoding
		if r.Refresh.Duration == 0 {
			r.Refresh.Duration = 10 * time.Second
		}
		if r.Timeout.Duration == 0 {
			r.Timeout.Duration = 2 * time.Second
		}
		if r.AgentScheme == "" {
			r.AgentScheme = "https"
		}
	}
	switch r := c.Registry; {
	case r == nil && len(c.Agents) == 0:
		bad("agents (at least one vm → URL) or registry is required")
	case r != nil && len(c.Agents) > 0:
		bad("agents and registry are exclusive: static routes or controld's registry")
	case r != nil:
		if u, err := url.Parse(r.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("registry.url %q must be an http(s) URL", r.URL)
		}
		if r.TokenFile == "" {
			bad("registry.token_file is required (controld's edge_token)")
		}
		if r.Refresh.Duration <= 0 || r.Timeout.Duration <= 0 || r.Timeout.Duration >= r.Refresh.Duration {
			bad("registry: refresh and timeout must be > 0, timeout shorter than refresh")
		}
		if r.AgentScheme != "http" && r.AgentScheme != "https" {
			bad("registry.agent_scheme %q: want https (or http with -dev)", r.AgentScheme)
		}
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
	if c.TicketCache < 0 || c.VerifySlots < 0 || c.AgentInFlight < 0 || c.MaxInFlight < 0 || c.MaxBody <= 0 {
		bad("ticket_cache, verify_slots, agent_in_flight and max_in_flight must be >= 0 (0 = default), max_body > 0")
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
	if c.DrainDelay.Duration < 0 {
		bad("drain_delay must be >= 0")
	}
	if c.DrainDelay.Duration+c.DrainTimeout.Duration > c.ShutdownBudget.Duration {
		bad("drain_delay %v + drain_timeout %v is over shutdown_budget %v (both are part of the shutdown)", c.DrainDelay, c.DrainTimeout, c.ShutdownBudget)
	}
	switch c.LogLevel {
	case "debug", "info", "warn":
	default:
		bad("log_level %q: want debug, info or warn", c.LogLevel)
	}
	paths := []*string{&c.ControlCAFile, &c.KeysFile, &c.LogFile, &c.AgentTLS.CAFile}
	if c.TLS != nil {
		paths = append(paths, &c.TLS.CertFile, &c.TLS.KeyFile)
	}
	if c.Registry != nil {
		paths = append(paths, &c.Registry.TokenFile)
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
	plain := 0 // agents reached over plain http
	for _, u := range c.Agents {
		if pu, _ := url.Parse(u); pu.Scheme == "http" {
			plain++
		}
	}
	if c.Registry != nil && c.Registry.AgentScheme == "http" {
		plain = 1
	}
	switch {
	case c.AgentTLS.CAFile != "" && c.Registry == nil && plain == len(c.Agents):
		return errs.Wrap(module, nil, "agent_tls.ca_file is set but every agent URL is plain http (the CA would be unused)")
	case c.AgentTLS.CAFile != "" && plain > 0 && c.Registry != nil:
		return errs.Wrap(module, nil, "agent_tls.ca_file is set but registry.agent_scheme is http (the CA would be unused)")
	case c.Dev:
		return nil
	case strings.HasPrefix(c.KeysURL, "http:"):
		return errs.Wrap(module, nil, "keys_url is plain HTTP: whoever can change the answer can mint tickets; use https, or pass -dev "+
			"for a local run").With("keys_url", c.KeysURL)
	case c.Registry != nil && strings.HasPrefix(c.Registry.URL, "http:"):
		return errs.Wrap(module, nil, "registry.url is plain HTTP: whoever can change the answer can redirect queries; use https, or pass "+
			"-dev for a local run").With("url", c.Registry.URL)
	case c.TLS == nil:
		return errs.Wrap(module, nil, "no tls section: plain HTTP to clients is for local development only; add \"tls\": "+
			"{\"cert_file\", \"key_file\"}, or pass -dev for a local run")
	case plain > 0:
		return errs.Wrap(module, nil, "%d agent URLs are plain http (or registry.agent_scheme): the edge → agent hop needs https (with agent_tls.ca_file for a "+
			"private CA) outside local development; pass -dev for a local run", plain)
	}
	return nil
}

// serverTLS loads the client-facing certificate: TLS 1.2 or later, h2 and http/1.1 by ALPN.
func (c *Config) serverTLS() (*tls.Config, error) {
	t, err := tlsx.ServerConfig(c.TLS.CertFile, c.TLS.KeyFile)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot load the tls certificate").With("cert_file", c.TLS.CertFile)
	}
	return t, nil
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
	pool, err := tlsx.LoadPool(c.AgentTLS.CAFile)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot load agent_tls.ca_file").With("ca_file", c.AgentTLS.CAFile)
	}
	t.TLSClientConfig.RootCAs = pool
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

// openKeys builds the ticket key ring: static (keys_file; nil fetcher) or fetched (keys_url, keys_file as the
// cache). A failed first fetch is not fatal: every ticket is refused until a key set arrives.
func openKeys(ctx context.Context, c *Config, hc *http.Client, log *slog.Logger) (*ticket.KeyRing, *keyfetch.Fetcher, error) {
	var ring *ticket.KeyRing
	var f *keyfetch.Fetcher
	var err error
	if c.KeysURL == "" {
		ring, err = keyfetch.LoadFile(c.KeysFile)
	} else {
		ring, f, err = keyfetch.Open(ctx, keyfetch.Config{URL: c.KeysURL, Cache: c.KeysFile, Every: c.KeysRefresh.Duration,
			Timeout: c.KeysTimeout.Duration, Client: hc, Log: log, Event: "EDGE"})
	}
	if err != nil {
		return nil, nil, errs.Wrap(module, err, "cannot load the ticket keys (keys_file)").With("path", c.KeysFile)
	}
	return ring, f, nil
}

// registry opens controld's VM registry (nil without a registry section) and fetches it once: a failed first
// fetch is not fatal (tickets for unknown VMs answer wrong_owner until a fetch succeeds). hc is the client to
// controld (control_ca_file).
func (c *Config) registry(ctx context.Context, hc *http.Client, log *slog.Logger) (*edge.RegistryRoutes, error) {
	r := c.Registry
	if r == nil {
		return nil, nil
	}
	tok, err := readToken(r.TokenFile)
	if err != nil {
		return nil, err
	}
	reg, err := edge.NewRegistryRoutes(edge.RegistryConfig{URL: r.URL, Token: tok, Scheme: r.AgentScheme, Every: r.Refresh.Duration,
		Timeout: r.Timeout.Duration, Client: hc, Log: log, Breaker: breaker.Config{OnChange: logx.BreakerLog(log, "registry")}})
	if err != nil {
		return nil, err
	}
	if err := reg.Fetch(ctx); err != nil {
		log.Warn("cannot fetch the agent registry before serving; retrying every refresh", "event", "EDGE.REGISTRY_FETCH_FAILED",
			"url", r.URL, "error", err.Error())
	}
	return reg, nil
}

// minToken is the shortest token accepted (controld's minToken).
const minToken = 16

// controlClient is the one HTTP client to controld (keys_url, registry.url), trusting control_ca_file.
func (c *Config) controlClient() (*http.Client, error) {
	hc, err := tlsx.HTTPClient(c.ControlCAFile)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot load control_ca_file").With("ca_file", c.ControlCAFile)
	}
	return hc, nil
}

// readToken reads a token file: 0600 (others cannot read it), one line, at least 16 characters.
func readToken(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", errs.Wrap(module, err, "cannot read registry.token_file").With("path", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", errs.Wrap(module, nil, "registry.token_file %s has mode %#o; make it 0600 (chmod 600)", path, perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errs.Wrap(module, err, "cannot read registry.token_file").With("path", path)
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) < minToken {
		return "", errs.Wrap(module, nil, "registry.token_file %s: the token must be at least %d characters", path, minToken)
	}
	return tok, nil
}
