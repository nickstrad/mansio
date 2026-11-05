package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/actor"
	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/parallel"
	"github.com/nickstrad/ts-db-go/internal/retry"
)

// startup opens the databases while the agent already serves (Release It H1): each database is served the
// moment it is open; until then the host refuses it unavailable (503 + Retry-After; StaticHost.Expect). It is
// the static stand-in for the owner table's reconcile: at CP-1, owner.Table.ReconcileUntilDone takes its place
// (the table's Admit already refuses an unreconciled key with owner.ErrUnavailable), and startupAdmitter goes
// away.
type startup struct {
	host      *front.StaticHost
	firstDone atomic.Bool // every database was tried once
}

// startupAdmitter refuses databases that are not open yet, before admission (fail fast, Release It F1).
type startupAdmitter struct {
	front.Admitter
	host *front.StaticHost
}

func (a startupAdmitter) Admit(db string, epoch uint64) (func(), error) {
	if err := a.host.Pending(db); err != nil {
		return nil, err
	}
	return a.Admitter.Admit(db, epoch)
}

// readyCheck passes once every database was tried at least once (some may still be retrying).
func (st *startup) readyCheck(context.Context) error {
	if !st.firstDone.Load() {
		return fmt.Errorf("startup: %d database(s) not open yet, first round running", st.host.PendingCount())
	}
	return nil
}

// openAll opens dbs into the host, cfg.StartupWorkers at a time, and retries the failures with a growing
// jittered backoff until every one is open or ctx ends. One log line per round (AGENT.STARTUP_ROUND; WARN while
// some fail), and AGENT.ALL_OPEN at the end.
func (st *startup) openAll(ctx context.Context, cfg *Config, dbs []string, log *slog.Logger) {
	acfg := cfg.Actor.Actor()
	ids := make(map[string]int, len(dbs))
	for i, db := range dbs {
		ids[db] = i
	}
	backoff := retry.Policy{Backoff: retry.Exponential, Jitter: retry.Equal, Base: 100 * time.Millisecond, Max: 5 * time.Second}
	_ = backoff.Normalize() // a constant policy: cannot fail
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	todo := dbs
	var delay time.Duration
	for round := 1; ; round++ {
		t0 := time.Now()
		var failed []string
		var first error
		for i, err := range parallel.ForEach(ctx, len(todo), cfg.StartupWorkers, func(_ context.Context, i int) error {
			db := todo[i]
			a := actor.New(ids[db], dbPath(cfg, db), acfg)
			if err := a.Start(true); err != nil {
				return errs.Wrap(module, err, "cannot open database %s; it is refused (unavailable) until a retry opens it", db).With("db", db)
			}
			if !st.host.Add(db, a) { // shutting down
				a.Close()
			}
			return nil
		}) {
			if err != nil {
				failed, first = append(failed, todo[i]), cmp.Or(first, err)
			}
		}
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
