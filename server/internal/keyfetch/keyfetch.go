// Package keyfetch keeps a ticket key ring in step with the issuer (control-plane plan CP-5): every Every it
// GETs URL (controld's GET /v1/keys), bounded by Timeout, and replaces the ring when the set changed. Shared by
// agentd and the edge (CP-7), which verify the same tickets.
//
// The issuer publishes a key before it signs with it and keeps a retired key published for the ticket lifetime,
// so a fetch period well under ticket_keys.activate never refuses a valid ticket. A failed fetch keeps the
// current ring (tickets keep verifying while controld is down) and is retried next period; the last set is
// cached in Cache, so a restart without controld still verifies.
//
// An adapter (HTTP, files, logs). Error module "keyfetch"; callers wrap Open's error in their own module.
package keyfetch

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/fsx"
	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/tlsx"
)

const module = "keyfetch"

// MaxBody bounds the key set answer (Release It B6).
const MaxBody = 64 << 10

// Config is one fetcher.
type Config struct {
	URL            string        // the issuer's key set (required)
	Cache          string        // a ticket.PublicKeyFile kept up to date; "" = none
	Every, Timeout time.Duration // fetch period; one fetch's bound (< Every)
	Client         *http.Client  // default: a client trusting CAFile
	CAFile         string        // PEM CA bundle that signs the issuer's certificate (a private CA); "" = system roots
	Log            *slog.Logger
	Event          string    // log event prefix, e.g. AGENT → AGENT.KEYS_UPDATED
	Clock          det.Clock // fetch cadence and deadlines, key age; default det.System()
}

// Fetcher is a running fetch loop's state.
type Fetcher struct {
	cfg  Config
	ring *ticket.KeyRing

	lastOK   atomic.Int64 // unix nanos of the last successful fetch; 0 = never
	failures atomic.Int64
	failing  atomic.Bool
}

// Open builds the ring: the cached set if there is one, else one fetch before serving. A failed first fetch is
// not fatal (the caller serves, and every ticket is refused until a key set arrives); an unreadable cache is.
func Open(ctx context.Context, cfg Config) (*ticket.KeyRing, *Fetcher, error) {
	if cfg.Client == nil {
		c, err := tlsx.HTTPClient(cfg.CAFile)
		if err != nil {
			return nil, nil, errs.Wrap(module, err, "cannot load the CA file for the keys url").With("ca_file", cfg.CAFile)
		}
		cfg.Client = c
	}
	cfg.Clock = det.Or(cfg.Clock)
	f := &Fetcher{cfg: cfg, ring: ticket.NewKeyRing(nil)}
	if cfg.Cache != "" {
		ring, err := LoadFile(cfg.Cache)
		switch {
		case err == nil:
			f.ring = ring
		case !errors.Is(err, fs.ErrNotExist):
			return nil, nil, err
		}
	}
	if f.ring.Len() == 0 { // nothing cached: try once before serving; with a cache, Run fetches at once
		if err := f.Fetch(ctx); err != nil {
			f.fail(err)
		}
	}
	return f.ring, f, nil
}

// LoadFile reads a ticket.PublicKeyFile into a ring.
func LoadFile(path string) (*ticket.KeyRing, error) {
	b, err := os.Open(path)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot open the key file").With("path", path)
	}
	defer b.Close()
	keys, err := ticket.ParsePublicKeys(b)
	if err != nil {
		return nil, errs.Wrap(module, err, "invalid key file %s", path).With("path", path)
	}
	return ticket.NewKeyRing(keys), nil
}

// Run fetches now, then every period until ctx ends; a fetch never overlaps the next (Release It S1).
func (f *Fetcher) Run(ctx context.Context) {
	t := f.cfg.Clock.NewTicker(f.cfg.Every)
	defer t.Stop()
	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-t.C():
			}
		}
		if err := f.Fetch(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			f.fail(err)
		} else if f.failing.Swap(false) {
			f.cfg.Log.Info("ticket keys fetched again", "event", f.cfg.Event+".KEYS_FETCH_OK", "url", f.cfg.URL)
		}
	}
}

// fail counts a failed fetch and logs the transition to failing (once, not every period).
func (f *Fetcher) fail(err error) {
	f.failures.Add(1)
	if !f.failing.Swap(true) {
		f.cfg.Log.Warn("cannot fetch ticket keys; keeping the current set, retrying every period", "event", f.cfg.Event+".KEYS_FETCH_FAILED",
			"url", f.cfg.URL, "keys", f.ring.Len(), "error", err.Error())
	}
}

// Failures is the number of failed fetches; Failing reports whether the last one failed.
func (f *Fetcher) Failures() int64 { return f.failures.Load() }
func (f *Fetcher) Failing() bool   { return f.failing.Load() }

// Fetch GETs the key set once and installs it if it changed.
func (f *Fetcher) Fetch(ctx context.Context) error {
	ctx, cancel := det.WithTimeout(ctx, f.cfg.Clock, f.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.cfg.URL, nil)
	if err != nil {
		return errs.Wrap(module, err, "bad keys url")
	}
	res, err := f.cfg.Client.Do(req)
	if err != nil {
		return errs.Wrap(module, err, "cannot fetch ticket keys; the current set stays")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, MaxBody))
		return errs.Wrap(module, nil, "cannot fetch ticket keys: %s answered %d; the current set stays", f.cfg.URL, res.StatusCode)
	}
	keys, err := ticket.ParsePublicKeys(io.LimitReader(res.Body, MaxBody))
	if err != nil {
		return errs.Wrap(module, err, "invalid ticket key set from %s; the current set stays", f.cfg.URL)
	}
	f.lastOK.Store(f.cfg.Clock.Now().UnixNano())
	if f.ring.Equal(keys) {
		return nil
	}
	f.ring.Replace(keys)
	f.cfg.Log.Info("ticket keys updated", "event", f.cfg.Event+".KEYS_UPDATED", "kids", slices.Sorted(maps.Keys(keys)))
	if f.cfg.Cache != "" {
		if err := writeKeyFile(f.cfg.Cache, keys); err != nil {
			f.cfg.Log.Warn("cannot cache the ticket keys; a restart without controld will not have them", "event", f.cfg.Event+".KEYS_CACHE_FAILED",
				"path", f.cfg.Cache, "error", err.Error())
		}
	}
	return nil
}

// writeKeyFile writes keys as a ticket.PublicKeyFile atomically (fsx.WriteAtomic: temp file, fsync, rename,
// directory fsync).
func writeKeyFile(path string, keys map[string]ed25519.PublicKey) error {
	b, _ := json.MarshalIndent(ticket.NewPublicKeyFile(keys), "", "  ")
	if err := fsx.WriteAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Register adds the fetcher's metrics as <prefix>_ticket_keys, <prefix>_keys_fetch_failures_total and
// <prefix>_keys_age_seconds.
func (f *Fetcher) Register(reg *metrics.Registry, prefix string) {
	reg.Add(prefix+"_ticket_keys", "Ticket public keys held.", metrics.Gauge, func(e *metrics.Emit) { e.Value(nil, float64(f.ring.Len())) })
	reg.Add(prefix+"_keys_fetch_failures_total", "Failed fetches of the ticket key set.", metrics.Counter, func(e *metrics.Emit) {
		e.Value(nil, float64(f.failures.Load()))
	})
	reg.Add(prefix+"_keys_age_seconds", "Seconds since the last successful key fetch (-1: never).", metrics.Gauge, func(e *metrics.Emit) {
		v := -1.0
		if t := f.lastOK.Load(); t != 0 {
			v = det.Since(f.cfg.Clock, time.Unix(0, t)).Seconds()
		}
		e.Value(nil, v)
	})
}
