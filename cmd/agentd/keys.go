package main

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
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/metrics"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// keyFetcher keeps the ticket key ring in step with the issuer (control-plane plan CP-5): every keys_refresh
// it GETs keys_url (controld's /v1/keys), bounded by keys_timeout, and replaces the ring when the set changed.
// The issuer publishes a key before it signs with it and keeps a retired key published for the ticket
// lifetime, so a fetch period well under ticket_keys.activate never refuses a valid ticket. A failed fetch
// keeps the current ring (tickets keep verifying while controld is down) and is retried next period; the
// last set is cached in keys_file, so a restart without controld still verifies.
type keyFetcher struct {
	url, cache     string
	every, timeout time.Duration
	ring           *ticket.KeyRing
	client         *http.Client
	log            *slog.Logger

	lastOK   atomic.Int64 // unix nanos of the last successful fetch; 0 = never
	failures atomic.Int64
	failing  atomic.Bool
}

// maxKeysBody bounds the key set answer (Release It B6).
const maxKeysBody = 64 << 10

// openKeys builds the ring: static (keys_file) or fetched (keys_url, nil fetcher otherwise). With keys_url
// it loads the cached set if there is one, then tries one fetch; a failed first fetch is not fatal (the
// agent serves, and every ticket is refused as ticket_invalid until a key set arrives).
func openKeys(ctx context.Context, cfg *Config, log *slog.Logger) (*ticket.KeyRing, *keyFetcher, error) {
	if cfg.KeysURL == "" {
		ring, err := loadKeys(cfg.KeysFile)
		return ring, nil, err
	}
	kf := &keyFetcher{url: cfg.KeysURL, cache: cfg.KeysFile, every: cfg.KeysRefresh.Duration, timeout: cfg.KeysTimeout.Duration,
		ring: ticket.NewKeyRing(nil), client: &http.Client{}, log: log}
	if kf.cache != "" {
		ring, err := loadKeys(kf.cache)
		switch {
		case err == nil:
			kf.ring = ring
		case !errors.Is(err, fs.ErrNotExist):
			return nil, nil, err
		}
	}
	if err := kf.fetch(ctx); err != nil {
		kf.fail(err)
	}
	return kf.ring, kf, nil
}

// run fetches every period until ctx ends; a fetch never overlaps the next (Release It S1).
func (k *keyFetcher) run(ctx context.Context) {
	t := time.NewTicker(k.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := k.fetch(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			k.fail(err)
		} else if k.failing.Swap(false) {
			k.log.Info("ticket keys fetched again", "event", "AGENT.KEYS_FETCH_OK", "url", k.url)
		}
	}
}

// fail counts a failed fetch and logs the transition to failing (once, not every period).
func (k *keyFetcher) fail(err error) {
	k.failures.Add(1)
	if !k.failing.Swap(true) {
		k.log.Warn("cannot fetch ticket keys; keeping the current set, retrying every keys_refresh", "event", "AGENT.KEYS_FETCH_FAILED",
			"url", k.url, "keys", len(k.ring.KIDs()), "error", err.Error())
	}
}

// fetch GETs the key set once and installs it if it changed.
func (k *keyFetcher) fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, k.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return errs.Wrap(module, err, "bad keys_url")
	}
	res, err := k.client.Do(req)
	if err != nil {
		return errs.Wrap(module, err, "cannot fetch ticket keys; the current set stays")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, maxKeysBody))
		return errs.Wrap(module, nil, "cannot fetch ticket keys: %s answered %d; the current set stays", k.url, res.StatusCode)
	}
	keys, err := ticket.ParsePublicKeys(io.LimitReader(res.Body, maxKeysBody))
	if err != nil {
		return errs.Wrap(module, err, "invalid ticket key set from %s; the current set stays", k.url)
	}
	k.lastOK.Store(time.Now().UnixNano())
	if sameKeys(keys, k.ring) {
		return nil
	}
	k.ring.Replace(keys)
	k.log.Info("ticket keys updated", "event", "AGENT.KEYS_UPDATED", "kids", slices.Sorted(maps.Keys(keys)))
	if k.cache != "" {
		if err := writeKeyFile(k.cache, keys); err != nil {
			k.log.Warn("cannot cache the ticket keys; a restart without controld will not have them", "event", "AGENT.KEYS_CACHE_FAILED",
				"path", k.cache, "error", err.Error())
		}
	}
	return nil
}

func sameKeys(keys map[string]ed25519.PublicKey, ring *ticket.KeyRing) bool {
	kids := ring.KIDs()
	if len(kids) != len(keys) {
		return false
	}
	for _, kid := range kids {
		cur, _ := ring.Key(kid)
		if k, ok := keys[kid]; !ok || !k.Equal(cur) {
			return false
		}
	}
	return true
}

// writeKeyFile writes keys as a ticket.PublicKeyFile atomically (temp file, fsync, rename).
func writeKeyFile(path string, keys map[string]ed25519.PublicKey) error {
	f := ticket.PublicKeyFile{}
	for _, kid := range slices.Sorted(maps.Keys(keys)) {
		f.Keys = append(f.Keys, ticket.PublicKeyEntry{KID: kid, PublicKey: ticket.EncodeKey(keys[kid])})
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keys-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// register adds the key fetcher's metrics.
func (k *keyFetcher) register(reg *metrics.Registry) {
	reg.Add("agentd_ticket_keys", "Ticket public keys held.", metrics.Gauge, func(e *metrics.Emit) { e.Value(nil, float64(len(k.ring.KIDs()))) })
	reg.Add("agentd_keys_fetch_failures_total", "Failed fetches of the ticket key set.", metrics.Counter, func(e *metrics.Emit) {
		e.Value(nil, float64(k.failures.Load()))
	})
	reg.Add("agentd_keys_age_seconds", "Seconds since the last successful key fetch (-1: never).", metrics.Gauge, func(e *metrics.Emit) {
		v := -1.0
		if t := k.lastOK.Load(); t != 0 {
			v = time.Since(time.Unix(0, t)).Seconds()
		}
		e.Value(nil, v)
	})
}
