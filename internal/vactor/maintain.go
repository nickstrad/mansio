package vactor

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
)

// TickReport is what one maintenance tick planned and did. Errors holds the planned actions that failed, by
// key; those databases are still warm (see Passivate and WriteBack).
type TickReport struct {
	Plan      Plan
	WroteBack []WriteBackResult
	Errors    map[string]error
}

// Tick runs one maintenance round: it plans with PlanTick over the warm databases, then carries the plan out,
// at most Options.Workers at a time. Passivations write back first. ctx bounds every write-back.
//
//	post: err != nil only if the host is closed
//	post: every planned key is either done or in r.Errors
func (h *Host) Tick(ctx context.Context) (TickReport, error) {
	now := h.opts.Now()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return TickReport{}, errs.Wrap(module, ErrClosed, "cannot run maintenance")
	}
	var ready []*entry
	var keys []string
	for k, e := range h.entries {
		if e.gone != nil {
			continue
		}
		select {
		case <-e.done:
			if e.db != nil || e.stranded {
				ready, keys = append(ready, e), append(keys, k)
			}
		default:
		}
	}
	h.mu.Unlock()

	cands := make([]Candidate, len(ready))
	for i, e := range ready {
		c := Candidate{Key: keys[i], LastUsed: time.Unix(0, e.lastUsed.Load()), LastWriteBack: time.Unix(0, e.lastWB.Load()),
			Bytes: fileBytes(h.LocalPath(keys[i]))}
		if h.opts.Store != nil {
			c.Dirty, c.Stale = e.dirty(), e.stale.Load()
			if ddb, ok := e.db.(DurableDB); ok {
				if n, pub := ddb.Commits(), e.pubCommits.Load(); n > pub {
					c.Commits = n - pub
				}
			}
			if e.forceDirty.Load() {
				c.Commits++
			}
		}
		cands[i] = c
	}
	r := TickReport{Plan: PlanTick(now, cands, h.opts.Policy), Errors: map[string]error{}}

	var jobs []string
	passivate := map[string]bool{}
	for _, ev := range r.Plan.Passivate {
		jobs, passivate[ev.Key] = append(jobs, ev.Key), true
	}
	jobs = append(jobs, r.Plan.WriteBack...)
	var mu sync.Mutex
	epochs := make(map[string]uint64, len(ready))
	for i, e := range ready {
		epochs[keys[i]] = e.epoch.Load()
	}
	h.parallel(jobs, h.opts.Workers, func(k string) error {
		do := func(ctx context.Context) error {
			if passivate[k] {
				return h.Passivate(ctx, k)
			}
			res, err := h.WriteBack(ctx, k)
			if err == nil && res.Wrote {
				mu.Lock()
				r.WroteBack = append(r.WroteBack, res)
				mu.Unlock()
			}
			return err
		}
		var err error
		if g := h.opts.Guard; g != nil {
			err = g(ctx, k, epochs[k], do)
		} else {
			err = do(ctx)
		}
		if err != nil {
			mu.Lock()
			r.Errors[k] = err
			mu.Unlock()
		}
		return nil
	})
	return r, nil
}

// RunMaintenance calls Tick every interval until ctx ends or the host closes, passing each report to report
// (on this goroutine). The caller owns the goroutine it runs on; cancel ctx to stop it.
//
//	pre:  every > 0
//	post: returns a wrapped ctx.Err() or ErrClosed
func (h *Host) RunMaintenance(ctx context.Context, every time.Duration, report func(TickReport)) error {
	if every <= 0 {
		return errs.Wrap(module, nil, "cannot run maintenance: interval %v must be > 0", every)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return errs.Wrap(module, ctx.Err(), "maintenance stopped")
		case <-t.C:
		}
		r, err := h.Tick(ctx)
		if err != nil {
			return err
		}
		if report != nil {
			report(r)
		}
	}
}

// fileBytes is a database's local size including its WAL.
func fileBytes(path string) int64 {
	var n int64
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}
