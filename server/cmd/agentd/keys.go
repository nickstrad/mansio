package main

import (
	"context"
	"log/slog"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/keyfetch"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// openKeys builds the ticket key ring: static (keys_file; nil fetcher) or fetched (keys_url, with keys_file as
// the cache; internal/keyfetch, CP-5). With keys_url a failed first fetch is not fatal: the agent serves, and
// every ticket is refused as ticket_invalid until a key set arrives.
func openKeys(ctx context.Context, cfg *Config, log *slog.Logger) (*ticket.KeyRing, *keyfetch.Fetcher, error) {
	if cfg.KeysURL == "" {
		ring, err := loadKeys(cfg.KeysFile)
		return ring, nil, err
	}
	ring, f, err := keyfetch.Open(ctx, keyfetch.Config{URL: cfg.KeysURL, Cache: cfg.KeysFile, Every: cfg.KeysRefresh.Duration,
		Timeout: cfg.KeysTimeout.Duration, CAFile: cfg.ControlCAFile, Log: log, Event: "AGENT"})
	if err != nil {
		return nil, nil, errs.Wrap(module, err, "cannot load the cached ticket keys (keys_file)").With("path", cfg.KeysFile)
	}
	return ring, f, nil
}
