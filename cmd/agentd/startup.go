package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/actor"
	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/retry"
)

// startup opens the databases while the agent already serves (Release It H1): each database is served the
// moment it is open; until then its requests are refused unavailable (503 + Retry-After). It is the static
// stand-in for the owner table's reconcile: at CP-1, owner.Table.ReconcileUntilDone takes its place (the
// table's Admit already refuses an unreconciled key with owner.ErrUnavailable), and startupAdmitter goes away.
type startup struct {
	open      sync.Map    // db → struct{}: open and added to the host
	firstDone atomic.Bool // every database was tried once
	pending   atomic.Int64
}

// startupAdmitter refuses databases that are not open yet, before admission (fail fast, Release It F1).
type startupAdmitter struct {
	front.Admitter
	st *startup
}

func (a startupAdmitter) Admit(db string, epoch uint64) (func(), error) {
	release, err := a.Admitter.Admit(db, epoch)
	if err != nil {
		return nil, err
	}
	if _, ok := a.st.open.Load(db); !ok {
		release()
		return nil, fmt.Errorf("%w: database %s is not open yet (agent starting)", front.ErrUnavailable, db)
	}
	return release, nil
}

// readyCheck passes once every database was tried at least once (some may still be retrying).
func (st *startup) readyCheck(context.Context) error {
	if !st.firstDone.Load() {
		return fmt.Errorf("startup: %d database(s) not tried yet", st.pending.Load())
	}
	return nil
}

// openAll opens dbs into host, at most workers at a time, and retries the failures with a growing jittered
// backoff until every one is open or ctx ends. One log line per round (AGENT.STARTUP_ROUND; WARN while some
// fail), and AGENT.ALL_OPEN at the end.
func (st *startup) openAll(ctx context.Context, cfg *Config, dbs []string, host *front.StaticHost, log *slog.Logger) {
	acfg := cfg.Actor.Actor()
	ids := make(map[string]int, len(dbs))
	for i, db := range dbs {
		ids[db] = i
	}
	backoff := retry.Policy{Backoff: retry.Exponential, Jitter: retry.Equal, Base: 100 * time.Millisecond, Max: 5 * time.Second}
	_ = backoff.Normalize() // a constant policy: cannot fail
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	todo := dbs
	st.pending.Store(int64(len(todo)))
	var delay time.Duration
	for round := 1; ; round++ {
		t0 := time.Now()
		failed, first := st.openRound(ctx, todo, cfg.StartupWorkers, func(db string) error {
			a := actor.New(ids[db], dbPath(cfg, db), acfg)
			if err := a.Start(true); err != nil {
				return errs.Wrap(module, err, "cannot open database %s; it is refused (unavailable) until a retry opens it", db).With("db", db)
			}
			if !host.Add(db, a) { // shutting down
				a.Close()
				return nil
			}
			st.open.Store(db, struct{}{})
			return nil
		})
		st.pending.Store(int64(len(failed)))
		st.firstDone.Store(true)
		attrs := []any{"event", "AGENT.STARTUP_ROUND", "round", round, "tried", len(todo), "failed", len(failed), "took_ms", time.Since(t0).Milliseconds()}
		if len(failed) == 0 {
			log.Info("databases opened", attrs...)
			log.Info("every database is open", "event", "AGENT.ALL_OPEN", "dbs", len(dbs))
			return
		}
		log.Warn("some databases did not open; retrying", append(attrs, "error", first.Error(), "chain", errs.Chain(first))...)
		todo = failed
		delay = backoff.Delay(rng, round+1, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// openRound runs open over dbs, at most n at a time, and returns the ones that failed and the first error.
func (st *startup) openRound(ctx context.Context, dbs []string, n int, open func(string) error) (failed []string, first error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, n)
	for _, db := range dbs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			mu.Lock()
			failed = append(failed, db)
			mu.Unlock()
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := open(db); err != nil {
				mu.Lock()
				failed = append(failed, db)
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if first == nil && len(failed) > 0 {
		first = errs.Wrap(module, ctx.Err(), "startup stopped before %d database(s) were tried", len(failed))
	}
	return failed, first
}

// openCanary creates the canary database (one marked row, nothing else) in a fresh temporary directory and
// starts its actor. The directory is removed by the returned cleanup.
func openCanary(cfg *Config) (*actor.Actor, func(), error) {
	dir, err := os.MkdirTemp("", "agentd-canary-*")
	if err != nil {
		return nil, nil, errs.Wrap(module, err, "cannot create the canary directory")
	}
	cleanup := func() { os.RemoveAll(dir) }
	path := filepath.Join(dir, "canary.db")
	err = actor.Seed(path, actor.Config{}, `CREATE TABLE canary (note TEXT NOT NULL)`, func(exec func(string, ...any) error) error {
		return exec(`INSERT INTO canary (note) VALUES ('synthetic readiness canary; not tenant data')`)
	})
	if err != nil {
		cleanup()
		return nil, nil, errs.Wrap(module, err, "cannot create the canary database")
	}
	a := actor.New(-1, path, cfg.Actor.Actor())
	if err := a.Start(true); err != nil {
		cleanup()
		return nil, nil, errs.Wrap(module, err, "cannot open the canary database")
	}
	return a, cleanup, nil
}

// breakerCheck fails readiness while b is open (Release It H2: ready means the breaker is not open). CP-1 adds
// one per remote dependency (s3, placement) to newAgent's readiness checks.
func breakerCheck(name string, b *breaker.Breaker) front.Check {
	return front.Check{Name: name + "_breaker", Run: func(context.Context) error {
		if s := b.State(time.Now()); s == breaker.Open {
			return fmt.Errorf("%s breaker is %s", name, s)
		}
		return nil
	}}
}
