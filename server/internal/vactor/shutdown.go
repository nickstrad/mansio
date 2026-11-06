package vactor

import (
	"cmp"
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/errs"
)

// ShutdownReport says what a bounded shutdown did (Release It H3).
type ShutdownReport struct {
	WrittenBack int // dirty databases published before the budget ended
	Dropped     int // databases closed and deleted: clean ones, and the written-back ones
	// Kept lists (sorted) the keys closed with their local copy kept because they were not written back: the
	// budget ended first, or their write-back failed. Their copies in CacheDir are the only up-to-date ones.
	Kept []string
	// Busy counts databases another caller (a maintenance tick) was still passivating when the budget ended.
	Busy     int
	CacheDir string // removed when Kept is empty and Busy is 0
	Took     time.Duration
}

// Close is Shutdown with a total budget of Options.WriteBackTimeout (not a budget per database).
func (h *Host) Close() error {
	ctx, cancel := det.WithTimeout(context.Background(), h.opts.Clock, h.opts.WriteBackTimeout)
	defer cancel()
	_, err := h.Shutdown(ctx)
	return err
}

// Shutdown closes the host within ctx's deadline. It cancels activations in flight, makes later Gets fail
// with ErrClosed, then passivates every database, at most Options.Workers at a time: dirty databases first,
// the one with the oldest unpublished writes (earliest last write-back) first, so the budget goes where a crash
// would lose the most. Each write-back is bounded by the rest of the budget and by Options.WriteBackTimeout.
// Once ctx ends it starts no more write-backs: it closes the remaining databases and keeps their local files.
//
//	post: returns soon after ctx ends (a write-back in flight stops at the deadline; the store honors ctx)
//	post: err == nil ⇔ every database was written back or clean, and the cache directory is removed
//	post: err != nil ⇒ rep.Kept are closed with their local files in rep.CacheDir; the error names how many,
//	      and matches context.DeadlineExceeded when the budget ended first
func (h *Host) Shutdown(ctx context.Context) (ShutdownReport, error) {
	start := h.opts.Clock.Now()
	h.mu.Lock()
	h.closed = true
	marked := make(map[string]*entry, len(h.entries))
	for k, e := range h.entries {
		if e.gone == nil {
			e.gone = make(chan struct{})
			marked[k] = e
		}
	}
	h.mu.Unlock()
	h.cancel()

	rep := ShutdownReport{CacheDir: h.opts.CacheDir}
	var (
		mu       sync.Mutex
		failures []error
	)
	h.parallel(h.shutdownOrder(marked), h.opts.Workers, func(k string) error {
		e := marked[k]
		<-e.done // activations were cancelled above; each also has its own deadline
		dirty := h.unpublished(e)
		if dirty && ctx.Err() != nil { // the budget is spent: keep the copy, start nothing
			h.keep(k, e)
			mu.Lock()
			rep.Kept = append(rep.Kept, k)
			mu.Unlock()
			return nil
		}
		kctx, cancel := det.WithTimeout(ctx, h.opts.Clock, h.opts.WriteBackTimeout)
		defer cancel()
		err := h.release(kctx, k, e) // on failure the key is warm again; closeRemaining keeps it below
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err != nil:
			failures = append(failures, err)
		case dirty:
			rep.WrittenBack++
			rep.Dropped++
		default:
			rep.Dropped++
		}
		return nil
	})
	kept, busy := h.closeRemaining(ctx)
	rep.Kept, rep.Busy = append(rep.Kept, kept...), busy
	slices.Sort(rep.Kept)
	rep.Took = h.since(start)
	if len(rep.Kept) == 0 && rep.Busy == 0 {
		return rep, os.RemoveAll(h.opts.CacheDir)
	}
	if ctx.Err() != nil {
		failures = append(failures, ctx.Err())
	}
	return rep, errs.Wrap(module, errors.Join(failures...),
		"host closed, but %d databases were not written back (%d still passivating); their local copies are kept in %s (the only up-to-date copies)",
		len(rep.Kept), rep.Busy, h.opts.CacheDir).With("cache_dir", h.opts.CacheDir)
}

// shutdownOrder lists the marked keys dirty first, oldest last write-back first; then the rest by key. A key
// still activating counts as clean (it has no writes yet).
func (h *Host) shutdownOrder(marked map[string]*entry) []string {
	type item struct {
		key   string
		dirty bool
		since int64
	}
	items := make([]item, 0, len(marked))
	for k, e := range marked {
		it := item{key: k}
		select {
		case <-e.done:
			it.dirty, it.since = h.unpublished(e), e.lastWB.Load()
		default:
		}
		items = append(items, it)
	}
	slices.SortFunc(items, func(a, b item) int {
		switch {
		case a.dirty != b.dirty:
			if a.dirty {
				return -1
			}
			return 1
		case a.dirty && a.since != b.since:
			return cmp.Compare(a.since, b.since)
		}
		return cmp.Compare(a.key, b.key)
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.key
	}
	return out
}

// unpublished reports whether e (activated) holds writes its published generation lacks (durable mode only).
func (h *Host) unpublished(e *entry) bool {
	return h.opts.Store != nil && (e.db != nil || e.stranded) && e.dirty()
}

// keep closes a marked entry's database without writing it back and drops it, keeping its local file.
//
//	pre: e.gone was set by the caller under h.mu, and e.done is closed
func (h *Host) keep(key string, e *entry) {
	e.closeDB()
	h.drop(key, e)
}

// closeDB closes e's database, if any, keeping its file. pre: e.done is closed.
func (e *entry) closeDB() {
	e.wb.lock()
	defer e.wb.unlock()
	if e.db != nil {
		e.db.Close()
	}
}

// closeRemaining closes the databases still in the host after Shutdown's passivations: those left warm by a
// failed write-back (their files are kept), after waiting, until ctx ends, for passivations that another
// caller is still running. It returns the keys it kept and how many passivations were still running at ctx's
// end.
func (h *Host) closeRemaining(ctx context.Context) (kept []string, busy int) {
	waited := map[*entry]bool{}
	for {
		h.mu.Lock()
		var e *entry
		var key string
		for k, x := range h.entries {
			if !waited[x] {
				key, e = k, x
				break
			}
		}
		if e == nil {
			h.mu.Unlock()
			return kept, busy
		}
		if g := e.gone; g != nil {
			h.mu.Unlock()
			select {
			case <-g:
			case <-ctx.Done():
				waited[e] = true
				busy++
			}
			continue
		}
		delete(h.entries, key) // closed: no Get can find it again
		h.mu.Unlock()
		<-e.done
		e.closeDB()
		kept = append(kept, key)
	}
}
