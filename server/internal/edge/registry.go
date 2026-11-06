package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// RegistryConfig is a Resolver fed by controld's VM registry (GET /v1/agents).
type RegistryConfig struct {
	URL    string // controld's GET /v1/agents (required)
	Token  string // its Bearer token (controld's edge_token)
	Scheme string // agents' endpoints are host:port; their base URL is Scheme://host:port; default "https"
	// Every is the poll period; Timeout bounds one fetch (< Every); MinGap is the least time between two fetches,
	// however often an unknown VM asks for an early one (default 1 s).
	Every, Timeout, MinGap time.Duration
	Client                 *http.Client // default: a fresh client (the system roots)
	Breaker                breaker.Config
	Log                    *slog.Logger
	Clock                  det.Clock // poll cadence, fetch deadlines, breaker time; default det.System()
}

// MaxRegistryBody bounds GET /v1/agents' answer (Release It B6).
const MaxRegistryBody = 4 << 20

// RegistryRoutes is a Resolver that answers from the last VM → endpoint map fetched from controld. Run polls it
// every Every, at once (after MinGap) when a verified ticket names a VM it does not know (a VM registered since),
// each fetch bounded by Timeout and behind a breaker. A failed fetch keeps the last map: endpoints rarely change,
// and a stale one costs an unavailable or wrong_owner answer, never a wrong database (the agent checks its
// ticket and epoch). Safe for concurrent use; Agent never blocks.
type RegistryRoutes struct {
	cfg RegistryConfig
	br  *breaker.Breaker

	routes   atomic.Pointer[map[string]string] // vm → base URL; nil until the first fetch
	kick     chan struct{}                     // capacity 1: an early fetch is wanted
	lastOK   atomic.Int64                      // unix nanos of the last successful fetch; 0 = never
	failures atomic.Int64
	failing  atomic.Bool
}

// NewRegistryRoutes validates cfg and returns a resolver with no routes yet: call Fetch (before serving) and Run.
func NewRegistryRoutes(cfg RegistryConfig) (*RegistryRoutes, error) {
	if cfg.Scheme == "" {
		cfg.Scheme = "https"
	}
	if cfg.MinGap <= 0 {
		cfg.MinGap = time.Second
	}
	cfg.Clock = det.Or(cfg.Clock)
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.URL == "" || cfg.Every <= 0 || cfg.Timeout <= 0 || cfg.Timeout >= cfg.Every || (cfg.Scheme != "http" && cfg.Scheme != "https") {
		return nil, errs.Wrap(module, nil, "edge: registry needs a url, 0 < timeout < every, and scheme http or https")
	}
	br, err := breaker.New(cfg.Breaker)
	if err != nil {
		return nil, errs.Wrap(module, err, "edge: invalid registry breaker config")
	}
	return &RegistryRoutes{cfg: cfg, br: br, kick: make(chan struct{}, 1)}, nil
}

// Agent returns vm's base URL from the last fetched map. An unknown VM asks Run for an early fetch.
func (r *RegistryRoutes) Agent(vm string) (string, bool) {
	if m := r.routes.Load(); m != nil {
		if u, ok := (*m)[vm]; ok {
			return u, true
		}
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
	return "", false
}

// Len is the number of VMs known (metrics, -check).
func (r *RegistryRoutes) Len() int {
	if m := r.routes.Load(); m != nil {
		return len(*m)
	}
	return 0
}

// Run fetches every Every, or sooner when Agent missed (but never within MinGap of the last fetch), until ctx
// ends; a fetch never overlaps the next (Release It S1). Failures are counted and logged on the transition only.
func (r *RegistryRoutes) Run(ctx context.Context) {
	clk := r.cfg.Clock
	t := clk.NewTicker(r.cfg.Every)
	defer t.Stop()
	last := clk.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
		case <-r.kick:
			if det.Sleep(ctx, clk, r.cfg.MinGap-det.Since(clk, last)) != nil {
				return
			}
		}
		last = clk.Now()
		t.Reset(r.cfg.Every) // a tick that queued during an early fetch does not fetch again at once
		if err := r.Fetch(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.fail(err)
		} else if r.failing.Swap(false) {
			r.cfg.Log.Info("agent registry fetched again", "event", "EDGE.REGISTRY_FETCH_OK", "url", r.cfg.URL, "agents", r.Len())
		}
	}
}

func (r *RegistryRoutes) fail(err error) {
	r.failures.Add(1)
	if !r.failing.Swap(true) {
		r.cfg.Log.Warn("cannot fetch the agent registry; keeping the last routes, retrying every period", "event", "EDGE.REGISTRY_FETCH_FAILED",
			"url", r.cfg.URL, "agents", r.Len(), "error", err.Error())
	}
}

// Fetch GETs the registry once (under Timeout and the breaker) and installs the map if it parses.
func (r *RegistryRoutes) Fetch(ctx context.Context) error {
	p, wait, ok := r.br.Allow(r.cfg.Clock.Now())
	if !ok {
		return errs.Wrap(module, breaker.ErrOpen, "agent registry breaker open; next try in %v; the last routes stay", wait)
	}
	m, err := r.fetch(ctx)
	outcome := breaker.Success
	switch {
	case ctx.Err() != nil:
		outcome = breaker.Ignore
	case err != nil:
		outcome = breaker.Failure
	}
	r.br.Done(p, r.cfg.Clock.Now(), outcome)
	if err != nil {
		return err
	}
	r.lastOK.Store(r.cfg.Clock.Now().UnixNano())
	if old := r.routes.Load(); old == nil || !maps.Equal(*old, m) {
		r.routes.Store(&m)
		r.cfg.Log.Info("agent routes updated", "event", "EDGE.REGISTRY_UPDATED", "agents", len(m))
	}
	return nil
}

func (r *RegistryRoutes) fetch(ctx context.Context) (map[string]string, error) {
	ctx, cancel := det.WithTimeout(ctx, r.cfg.Clock, r.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.URL, nil)
	if err != nil {
		return nil, errs.Wrap(module, err, "bad registry url")
	}
	if r.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.Token)
	}
	res, err := r.cfg.Client.Do(req)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot fetch the agent registry; the last routes stay")
	}
	defer res.Body.Close()
	body := io.LimitReader(res.Body, MaxRegistryBody)
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, body)
		return nil, errs.Wrap(module, nil, "cannot fetch the agent registry: %s answered %d; the last routes stay", r.cfg.URL, res.StatusCode)
	}
	var out struct {
		Agents map[string]string `json:"agents"`
	}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return nil, errs.Wrap(module, err, "invalid agent registry answer from %s; the last routes stay", r.cfg.URL)
	}
	m := make(map[string]string, len(out.Agents))
	for vm, ep := range out.Agents {
		if vm == "" || len(vm) > ticket.MaxVM || ep == "" {
			return nil, errs.Wrap(module, nil, "invalid agent registry answer from %s: vm %q endpoint %q; the last routes stay", r.cfg.URL, vm, ep)
		}
		m[vm] = fmt.Sprintf("%s://%s", r.cfg.Scheme, ep)
	}
	return m, nil
}

// Register adds the resolver's metrics: edge_registry_agents, edge_registry_fetch_failures_total and
// edge_registry_age_seconds (-1: never fetched).
func (r *RegistryRoutes) Register(reg *metrics.Registry) {
	reg.Add("edge_registry_agents", "Agents known from controld's VM registry.", metrics.Gauge, func(e *metrics.Emit) { e.Value(nil, float64(r.Len())) })
	reg.Add("edge_registry_fetch_failures_total", "Failed fetches of the agent registry.", metrics.Counter, func(e *metrics.Emit) {
		e.Value(nil, float64(r.failures.Load()))
	})
	reg.Add("edge_registry_age_seconds", "Seconds since the last successful registry fetch (-1: never).", metrics.Gauge, func(e *metrics.Emit) {
		v := -1.0
		if t := r.lastOK.Load(); t != 0 {
			v = det.Since(r.cfg.Clock, time.Unix(0, t)).Seconds()
		}
		e.Value(nil, v)
	})
}
