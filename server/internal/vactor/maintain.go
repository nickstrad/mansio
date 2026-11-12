package vactor

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/errs"
)

// TickReport is what one maintenance tick planned and did. Errors holds the planned actions that failed, by
// key; those databases are still warm (see Passivate and WriteBack).
type TickReport struct {
	Plan      Plan
	WroteBack []WriteBackResult
	Errors    map[string]error
	// TimedOut lists the keys (sorted) whose job hit its own deadline (Options.WriteBackTimeout); they are in
	// Errors too.
	TimedOut []string
	// OldestDirtyAge is the steady-state gauge oldest_dirty_age: how long the dirty database
	// with the oldest last write-back (or activation) has gone unpublished, at the start of the tick. It
	// bounds the writes a crash would lose (RPO). 0 when nothing is dirty.
	OldestDirtyAge time.Duration
	// Dirty counts the warm databases with unpublished writes at the start of the tick.
	Dirty int
	// Took is the tick's duration; Missed counts the intervals RunMaintenance skipped while it ran (ticks
	// never overlap: a slow tick is reported, not stacked). Missed is 0 from a direct Tick.
	Took   time.Duration
	Missed int
}

// Tick runs one maintenance round: it plans with PlanTick over the warm databases, then carries the plan out,
// at most Options.Workers at a time. Passivations write back first. Each job runs under its own deadline,
// Options.WriteBackTimeout, derived from ctx: one hung write-back cannot hold the tick, only its worker for
// that long. Background loops cannot stall.
//
//	post: err != nil only if the host is closed
//	post: every planned key is either done or in r.Errors
func (h *Host) Tick(ctx context.Context) (TickReport, error) {
	now := h.opts.Clock.Now()
	h.pruneTombs(now)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return TickReport{}, errs.Wrap(module, ErrClosed, "cannot run maintenance")
	}
	var keys []string
	for k, e := range h.entries {
		if e.gone != nil {
			continue
		}
		select {
		case <-e.done:
			if e.db != nil || e.stranded {
				keys = append(keys, k)
			}
		default:
		}
	}
	slices.Sort(keys) // the plan must not depend on map order: a simulation replays a tick from its seed
	ready := make([]*entry, len(keys))
	for i, k := range keys {
		ready[i] = h.entries[k]
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
	r.OldestDirtyAge, r.Dirty = dirtyState(now, cands)
	t0 := h.opts.Clock.Now()

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
		ctx, cancel := det.WithTimeout(ctx, h.opts.Clock, h.opts.WriteBackTimeout)
		defer cancel()
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
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				r.TimedOut = append(r.TimedOut, k)
			}
			mu.Unlock()
		}
		return nil
	})
	slices.Sort(r.TimedOut)
	r.Took = h.since(t0)
	return r, nil
}

// dirtyState is the age of the oldest unpublished state among cands, and how many of them are dirty.
func dirtyState(now time.Time, cands []Candidate) (oldest time.Duration, dirty int) {
	for _, c := range cands {
		if c.Dirty {
			oldest = max(oldest, now.Sub(c.LastWriteBack))
			dirty++
		}
	}
	return oldest, dirty
}

// RunMaintenance calls Tick every interval until ctx ends or the host closes, passing each report to report
// (on this goroutine). Ticks never overlap: the next interval starts when a tick ends, and a tick that ran
// past its interval reports the intervals it missed (TickReport.Missed). The caller owns the goroutine it
// runs on; cancel ctx to stop it.
//
//	pre:  every > 0
//	post: returns a wrapped ctx.Err() or ErrClosed
func (h *Host) RunMaintenance(ctx context.Context, every time.Duration, report func(TickReport)) error {
	if every <= 0 {
		return errs.Wrap(module, nil, "cannot run maintenance: interval %v must be > 0", every)
	}
	t := h.opts.Clock.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return errs.Wrap(module, ctx.Err(), "maintenance stopped")
		case <-t.C():
		}
		r, err := h.Tick(ctx)
		if err != nil {
			return err
		}
		r.Missed = int(r.Took / every)
		t.Reset(every) // a full interval after a slow tick, not a burst of queued ones
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
