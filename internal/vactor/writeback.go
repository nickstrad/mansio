package vactor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/panicx"
)

// durableState is an entry's durable-mode bookkeeping.
//
// Invariant: (man, ver) is the manifest this entry last read or published, and pubCommits is the db's commit
// count that manifest's generation holds. The entry is dirty iff forceDirty or db.Commits() != pubCommits.
// The fields without atomics are guarded by wb, which also serializes every write-back of the entry.
type durableState struct {
	wb       sync.Mutex
	man      durable.Manifest
	ver      durable.Version
	lastGen  uint64            // highest generation this host tried to upload for the key
	pending  *durable.Manifest // a publish or stamp of ours whose outcome we never learned (ambiguous error)
	stranded bool              // passivation's write-back failed and the file could not be reopened

	epoch      atomic.Uint64
	pubCommits atomic.Uint64
	forceDirty atomic.Bool // the db was reopened after a failed passivation: its commit counter restarted
	stale      atomic.Bool // a re-read after a failed publish or stamp showed another writer (guide R4)
	lastUsed   atomic.Int64
	lastWB     atomic.Int64
}

func (d *durableState) touch(now time.Time) { d.lastUsed.Store(now.UnixNano()) }

func (d *durableState) raiseEpoch(epoch uint64) {
	for {
		old := d.epoch.Load()
		if epoch <= old || d.epoch.CompareAndSwap(old, epoch) {
			return
		}
	}
}

// WriteBackResult says what one write-back did and where its time went.
type WriteBackResult struct {
	Key        string
	Wrote      bool   // false: the database was clean (or not active); nothing uploaded
	Generation uint64 // the generation now published (also when clean and active; 0 if inactive)
	Bytes      int64
	Snapshot   time.Duration
	Hash       time.Duration
	Upload     time.Duration
	Publish    time.Duration
}

// fetchDurable puts key's current generation at path (or an empty file for a new database) and records the
// manifest in e.
func (h *Host) fetchDurable(key string, e *entry, path string) error {
	if err := durable.ValidateKey(key); err != nil {
		return errs.Wrap(module, err, "cannot activate %s", key)
	}
	m, v, err := h.opts.Store.ReadManifest(h.ctx, key)
	switch {
	case errors.Is(err, durable.ErrNoManifest):
		if !h.opts.CreateMissing {
			return errs.Wrap(module, errors.Join(ErrNotFound, err), "cannot activate %s: it has no manifest; nothing cached", key)
		}
		m, v = durable.Manifest{Key: key}, durable.NoVersion
	case err != nil:
		return errs.Wrap(module, err, "cannot activate %s: manifest read failed; nothing cached, safe to retry", key)
	case m.Epoch > e.epoch.Load():
		return errs.Wrap(module, durable.ErrStale, "cannot activate %s at epoch %d: the manifest is at epoch %d (a newer owner); nothing cached",
			key, e.epoch.Load(), m.Epoch)
	}
	if c := h.opts.CheckActivation; c != nil {
		if err := c(key, e.epoch.Load(), m); err != nil {
			return errs.Wrap(module, err, "cannot activate %s: refused by the ownership check; nothing cached", key)
		}
	}
	if m.Generation == 0 {
		if err := os.WriteFile(path, nil, 0o644); err != nil { // an empty file is an empty SQLite database
			return errs.Wrap(module, err, "cannot activate %s: cannot create an empty database file; safe to retry", key)
		}
	} else {
		h.fetches.Add(1)
		f, err := h.opts.Store.FetchGeneration(h.ctx, m, path)
		e.act.Bytes, e.act.TTFB, e.act.Fetch = f.Bytes, f.TTFB, f.Total
		if err != nil {
			if errors.Is(err, durable.ErrNotFound) {
				err = errors.Join(ErrNotFound, err)
			}
			return errs.Wrap(module, err, "cannot activate %s: download of generation %d failed; nothing cached, safe to retry", key, m.Generation)
		}
	}
	e.man, e.ver, e.lastGen = m, v, m.Generation
	return nil
}

// dirty reports whether e has writes its published generation does not hold. pre: e.done is closed.
func (e *entry) dirty() bool {
	if e.forceDirty.Load() {
		return true
	}
	ddb, ok := e.db.(DurableDB)
	return ok && ddb.Commits() != e.pubCommits.Load()
}

// active returns key's entry if it is ready (activated, not passivating), else nil.
func (h *Host) active(key string) *entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[key]
	if !ok || e.gone != nil {
		return nil
	}
	select {
	case <-e.done:
		if e.db == nil && !e.stranded {
			return nil // a failed activation about to be forgotten
		}
		return e
	default:
		return nil // still activating: nothing written yet
	}
}

// WriteBack publishes key's warm database as a new generation if it is dirty, while it keeps serving: the
// snapshot runs on the actor's goroutine (DurableDB.Snapshot), so writes queued meanwhile are not lost —
// they stay dirty for the next write-back. Inactive, activating or clean keys: nothing to do.
//
//	post: err == nil && res.Wrote ⇒ the manifest names a generation holding every write acknowledged
//	      before WriteBack was called
//	post: err != nil ⇒ the database is still warm and its local file untouched; the message says whether a
//	      retry is safe (errors.Is(err, durable.ErrStale): it is not; a higher epoch owns the key)
func (h *Host) WriteBack(ctx context.Context, key string) (WriteBackResult, error) {
	if h.opts.Store == nil {
		return WriteBackResult{Key: key}, nil
	}
	e := h.active(key)
	if e == nil {
		return WriteBackResult{Key: key}, nil
	}
	e.wb.Lock()
	defer e.wb.Unlock()
	if !e.dirty() {
		return WriteBackResult{Key: key, Generation: e.man.Generation}, nil
	}
	if e.stale.Load() {
		h.writeBackFailures.Add(1)
		return WriteBackResult{Key: key}, errs.Wrap(module, durable.ErrStale,
			"cannot write back %s: an earlier publish found a higher epoch or another writer; nothing uploaded, database still warm; not safe to retry, re-check ownership", key)
	}
	if e.stranded { // db closed: the local file is stable and complete
		return h.upload(ctx, key, e, h.LocalPath(key), 0)
	}
	snap := h.LocalPath(key) + ".snap"
	RemoveDB(snap)
	defer RemoveDB(snap)
	t := time.Now()
	commits, err := e.db.(DurableDB).Snapshot(ctx, snap)
	if err != nil {
		h.writeBackFailures.Add(1)
		return WriteBackResult{Key: key}, errs.Wrap(module, err,
			"cannot write back %s: snapshot failed; nothing uploaded, database still warm, safe to retry", key)
	}
	res, err := h.upload(ctx, key, e, snap, commits)
	res.Snapshot = time.Since(t) - res.Hash - res.Upload - res.Publish
	return res, err
}

// WriteBackAt is WriteBack at epoch: it first raises the epoch the key's publishes carry (never lowers
// it). fencing's port: Publish(ctx, key, epoch).
func (h *Host) WriteBackAt(ctx context.Context, key string, epoch uint64) (WriteBackResult, error) {
	if e := h.active(key); e != nil {
		e.raiseEpoch(epoch)
	}
	return h.WriteBack(ctx, key)
}

// upload publishes the file at path as key's next generation. commits is the db commit count the file holds.
//
//	pre:  e.wb is held; path is a complete, stable database file
//	post: err == nil ⇒ e.man is the published manifest and e is clean as of commits
func (h *Host) upload(ctx context.Context, key string, e *entry, path string, commits uint64) (WriteBackResult, error) {
	res := WriteBackResult{Key: key}
	fail := func(err error, format string, args ...any) (WriteBackResult, error) {
		h.writeBackFailures.Add(1)
		return res, errs.Wrap(module, err, format, args...).With("key", key).With("local", path)
	}
	t := time.Now()
	d, err := hashFile(path)
	res.Hash = time.Since(t)
	if err != nil {
		return fail(err, "cannot write back %s: reading the snapshot failed; nothing uploaded, database still warm, safe to retry", key)
	}
	res.Bytes = d.Size

	t = time.Now()
	var gen uint64
	for try := 0; ; try++ {
		gen = max(e.man.Generation, e.lastGen) + 1
		e.lastGen = gen // never reuse a number we tried: its object may exist (orphan)
		err = h.opts.Store.PutGeneration(ctx, key, gen, path, d)
		if err != nil && h.holds(ctx, durable.GenObject(key, gen), d) {
			err = nil // the PUT applied and its reply was lost (or a re-send of it found it): it is ours
		}
		if !errors.Is(err, durable.ErrExists) || try == 7 {
			break
		}
	}
	res.Upload = time.Since(t)
	if err != nil {
		return fail(err, "cannot write back %s: upload of generation %d failed; manifest unchanged (generation %d), database still warm, safe to retry",
			key, gen, e.man.Generation)
	}

	t = time.Now()
	next := durable.NextManifest(key, gen, e.epoch.Load(), d, h.opts.Now())
	v, err := h.publish(ctx, e, &next)
	res.Publish = time.Since(t)
	if err != nil {
		if e.stale.Load() {
			return fail(err, "cannot write back %s: the manifest moved on or a higher epoch owns it; generation %d uploaded but not published, database still warm; not safe to retry, re-check ownership",
				key, gen)
		}
		return fail(err, "cannot write back %s: publishing generation %d failed or its reply was lost; database still warm, safe to retry", key, gen)
	}
	e.man, e.ver, e.pending = next, v, nil
	e.pubCommits.Store(commits)
	e.forceDirty.Store(false)
	e.lastWB.Store(h.opts.Now().UnixNano())
	h.writeBacks.Add(1)
	h.bytesUploaded.Add(d.Size)
	res.Wrote, res.Generation = true, gen
	return res, nil
}

// holds reports whether the stored object name holds exactly the snapshot d: the re-read that settles an
// upload whose outcome is unclear (object-storage guide R3). A failed HEAD says nothing: false.
func (h *Host) holds(ctx context.Context, name string, d durable.Digest) bool {
	info, err := h.opts.Store.StatGeneration(ctx, name)
	return err == nil && info.Holds(d)
}

// publish publishes next conditionally on e's version. When the outcome is unclear (an error, or ErrStale
// that our own earlier publish may have caused), it re-reads the manifest (object-storage guide R3, R4):
//
//   - the stored manifest is next: the publish did apply;
//   - it is e.pending (an earlier publish of ours whose reply was lost): adopt that, try once more on top;
//   - it is the version we published on, or the re-read failed: unknown, so next becomes e.pending;
//   - anything else is another writer: e.stale is set and the error is ErrStale.
//
// Adopting a pending stamp of ours raises e's epoch and next's to the stamped one.
//
//	pre: e.wb is held
func (h *Host) publish(ctx context.Context, e *entry, next *durable.Manifest) (durable.Version, error) {
	for try := 0; ; try++ {
		var prev *durable.Manifest
		if e.ver != durable.NoVersion {
			prev = &e.man
		}
		if err := durable.CheckPublish(prev, *next); err != nil {
			e.stale.Store(true)
			return durable.NoVersion, err
		}
		v, err := h.opts.Store.PublishManifest(ctx, *next, e.ver)
		if err == nil {
			return v, nil
		}
		stored, sv, rerr := h.opts.Store.ReadManifest(ctx, next.Key)
		switch other := e.otherWriter(stored, sv, rerr); {
		case rerr == nil && stored.Same(*next):
			return sv, nil
		case rerr == nil && try == 0 && e.pending != nil && stored.Same(*e.pending):
			e.man, e.ver, e.pending = stored, sv, nil
			e.raiseEpoch(stored.Epoch) // ours: a pending stamp may have raised it
			next.Epoch = max(next.Epoch, stored.Epoch)
			continue
		case other:
			e.stale.Store(true)
			return durable.NoVersion, errs.Wrap(module, errors.Join(durable.ErrStale, err),
				"the re-read manifest of %s (generation %d, epoch %d) was written by another writer", next.Key, stored.Generation, stored.Epoch)
		}
		pending := *next
		e.pending = &pending
		return durable.NoVersion, err
	}
}

// otherWriter reports whether a re-read (stored, sv, rerr) shows a manifest someone else wrote: it exists
// and is neither the version e last saw nor e.pending. The caller checks for its own write first.
func (e *durableState) otherWriter(stored durable.Manifest, sv durable.Version, rerr error) bool {
	switch {
	case errors.Is(rerr, durable.ErrNoManifest):
		return e.ver != durable.NoVersion // ours vanished: not ours to continue
	case rerr != nil:
		return false // unknown
	}
	return sv != e.ver && (e.pending == nil || !stored.Same(*e.pending))
}

// Passivate drops key's database. In durable mode it first closes the database (so nothing more can be
// written, even through handles callers still hold), and if there are writes the published generation does
// not have, uploads the closed file as a new generation; only then does it delete the local copy. A Get that
// arrives meanwhile waits. Passivating an inactive or already-passivating key does nothing.
//
//	post: err == nil ⇒ the key is inactive and every acknowledged write is in the published generation
//	post: err != nil ⇒ the key is warm again (the file reopened) with its local copy intact, or, if even the
//	      reopen failed, "stranded": Gets fail, the file is kept, and a later Passivate retries the upload
func (h *Host) Passivate(ctx context.Context, key string) error {
	h.mu.Lock()
	e, ok := h.entries[key]
	if !ok || e.gone != nil {
		h.mu.Unlock()
		return nil
	}
	e.gone = make(chan struct{})
	h.mu.Unlock()
	return h.release(ctx, key, e)
}

// release finishes a passivation that the caller marked (e.gone set) under the lock.
func (h *Host) release(ctx context.Context, key string, e *entry) error {
	<-e.done
	if h.opts.Store != nil && (e.db != nil || e.stranded) {
		if err := h.writeBackClosed(ctx, key, e); err != nil {
			return err
		}
	} else if e.db != nil {
		e.db.Close()
	}
	if !h.opts.KeepLocal {
		RemoveDB(h.LocalPath(key))
	}
	h.drop(key, e)
	return nil
}

// drop removes e from the map and wakes Gets waiting on its passivation.
func (h *Host) drop(key string, e *entry) {
	h.mu.Lock()
	if h.entries[key] == e {
		delete(h.entries, key)
	}
	h.mu.Unlock()
	close(e.gone)
}

// writeBackClosed is passivation's write-back: close, upload if dirty, and on failure put the key back warm.
func (h *Host) writeBackClosed(ctx context.Context, key string, e *entry) error {
	e.wb.Lock()
	defer e.wb.Unlock()
	if e.db != nil {
		e.db.Close()
	}
	if !e.dirty() {
		return nil
	}
	var err error
	if e.stale.Load() {
		err = errs.Wrap(module, durable.ErrStale, "cannot passivate %s: it has unpublished writes but a higher epoch owns the key; kept warm, use Discard to drop it", key)
	} else {
		var commits uint64
		if ddb, ok := e.db.(DurableDB); ok {
			commits = ddb.Commits()
		}
		path, cleanup, perr := h.closedCopy(key)
		if perr != nil {
			err = perr
		} else {
			_, err = h.upload(ctx, key, e, path, commits)
			cleanup()
		}
	}
	if err == nil {
		return nil
	}
	h.reinstate(key, e)
	return errs.Wrap(module, err, "passivation of %s cancelled: nothing deleted, the local copy is kept", key).With("local", h.LocalPath(key))
}

// closedCopy returns a single-file copy of key's closed database. Normally that is the file itself (SQLite
// checkpoints and removes the WAL on the last close); if a WAL is left, it reopens the file and snapshots it.
func (h *Host) closedCopy(key string) (string, func(), error) {
	path := h.LocalPath(key)
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() == 0 {
		return path, func() {}, nil
	}
	db, err := h.open.Open(path)
	if err != nil {
		return "", nil, errs.Wrap(module, err, "cannot write back %s: WAL left after close and reopen failed; safe to retry", key)
	}
	defer db.Close()
	ddb, ok := db.(DurableDB)
	if !ok {
		return "", nil, errs.Wrap(module, nil, "cannot write back %s: WAL left after close and %T cannot snapshot", key, db)
	}
	snap := path + ".snap"
	RemoveDB(snap)
	ctx, cancel := context.WithTimeout(context.Background(), h.opts.WriteBackTimeout)
	defer cancel()
	if _, err := ddb.Snapshot(ctx, snap); err != nil {
		RemoveDB(snap)
		return "", nil, errs.Wrap(module, err, "cannot write back %s: WAL left after close and the snapshot failed; safe to retry", key)
	}
	return snap, func() { RemoveDB(snap) }, nil
}

// reinstate puts key back warm after a failed passivation: a fresh entry over the reopened file (entries are
// immutable once done, so waiting Gets never see one change under them), then wakes the waiters.
//
//	pre: e.wb is held, e.db is closed
func (h *Host) reinstate(key string, e *entry) {
	n := &entry{done: make(chan struct{})}
	close(n.done)
	n.man, n.ver, n.lastGen, n.pending = e.man, e.ver, e.lastGen, e.pending
	n.epoch.Store(e.epoch.Load())
	n.stale.Store(e.stale.Load())
	n.lastUsed.Store(e.lastUsed.Load())
	n.lastWB.Store(e.lastWB.Load())
	n.forceDirty.Store(true) // the new db's commit counter starts again; what it holds is not published
	db, err := h.open.Open(h.LocalPath(key))
	switch {
	case err != nil:
		n.stranded = true
		n.err = errs.Wrap(module, err, "cannot serve %s: its write-back failed and the local copy could not be reopened; the copy is kept, passivate to retry the upload", key)
	default:
		if _, ok := db.(DurableDB); ok {
			n.db = db
			break
		}
		db.Close()
		n.stranded = true
		n.err = errs.Wrap(module, nil, "cannot serve %s: reopened database %T cannot be written back", key, db)
	}
	assert.That((n.db != nil) != n.stranded, "reinstate(%s): db %v stranded %v", key, n.db, n.stranded)
	h.mu.Lock()
	if h.entries[key] == e {
		h.entries[key] = n
	}
	h.mu.Unlock()
	close(e.gone)
}

// Discard drops key's database WITHOUT writing it back: unpublished writes are lost. For an owner that was
// fenced out (ErrStale): its writes must not survive. Reports whether it dropped unpublished writes.
func (h *Host) Discard(key string) (lostWrites bool) {
	h.mu.Lock()
	e, ok := h.entries[key]
	if !ok || e.gone != nil {
		h.mu.Unlock()
		return false
	}
	e.gone = make(chan struct{})
	h.mu.Unlock()
	<-e.done
	e.wb.Lock()
	if e.db != nil {
		e.db.Close()
	}
	lostWrites = h.opts.Store != nil && (e.db != nil || e.stranded) && e.dirty()
	e.wb.Unlock()
	RemoveDB(h.LocalPath(key))
	h.drop(key, e)
	return lostWrites
}

// Stamp re-publishes key's manifest at epoch, same generation gen (durable.CheckStamp), so that from then on
// every lower-epoch publish fails. fencing's new owner calls it before admitting work. If the key is warm
// here the stamp is conditional on the manifest this host last saw, and the entry adopts the new epoch.
//
// A failed warm stamp re-reads the manifest before deciding (object-storage guide R3, R4): if the stamp
// applied (its reply was lost, or a re-send of it got 412) it succeeds; if the stored manifest is an earlier
// publish of ours whose reply was lost, it adopts that and tries once more; the key is marked stale only if
// the re-read shows another writer.
func (h *Host) Stamp(ctx context.Context, key string, epoch, gen uint64) (durable.Manifest, error) {
	if h.opts.Store == nil {
		return durable.Manifest{}, errs.Wrap(module, nil, "cannot stamp %s: the host has no durable store", key)
	}
	e := h.active(key)
	if e == nil {
		m, _, err := durable.Stamp(ctx, h.opts.Store, key, epoch, gen, h.opts.Now())
		if err != nil {
			return durable.Manifest{}, errs.Wrap(module, err, "cannot stamp %s", key)
		}
		return m, nil
	}
	e.wb.Lock()
	defer e.wb.Unlock()
	for try := 0; ; try++ {
		var cur *durable.Manifest
		if e.ver != durable.NoVersion {
			cur = &e.man
		}
		var next durable.Manifest
		err := durable.CheckStamp(cur, key, epoch, gen)
		if err == nil {
			next = durable.Stamped(cur, key, epoch, h.opts.Now())
			var v durable.Version
			if v, err = h.opts.Store.PublishManifest(ctx, next, e.ver); err == nil {
				e.man, e.ver = next, v
				e.raiseEpoch(epoch)
				return next, nil
			}
		}
		stored, sv, rerr := h.opts.Store.ReadManifest(ctx, key)
		switch other := e.otherWriter(stored, sv, rerr); {
		case rerr == nil && next.Key != "" && stored.Same(next): // the stamp applied
			e.man, e.ver = stored, sv
			e.raiseEpoch(epoch)
			return stored, nil
		case rerr == nil && try == 0 && e.pending != nil && stored.Same(*e.pending):
			e.man, e.ver, e.pending = stored, sv, nil
			continue
		case other && errors.Is(err, durable.ErrStale):
			e.stale.Store(true)
		case rerr != nil && next.Key != "":
			e.pending = &next // it may have applied: a later publish that finds it adopts it
		}
		return durable.Manifest{}, errs.Wrap(module, err, "cannot stamp warm %s at epoch %d; re-read the manifest before retrying", key, epoch)
	}
}

// PassivateAll passivates every active key, at most workers at a time, each write-back bounded by
// Options.WriteBackTimeout. It returns the failures joined; those keys stay warm.
func (h *Host) PassivateAll(workers int) error {
	h.mu.Lock()
	marked := make(map[string]*entry, len(h.entries))
	for k, e := range h.entries {
		if e.gone == nil {
			e.gone = make(chan struct{})
			marked[k] = e
		}
	}
	h.mu.Unlock()
	keys := make([]string, 0, len(marked))
	for k := range marked {
		keys = append(keys, k)
	}
	errsOut := h.parallel(keys, workers, func(k string) error {
		ctx, cancel := context.WithTimeout(context.Background(), h.opts.WriteBackTimeout)
		defer cancel()
		return h.release(ctx, k, marked[k])
	})
	return errors.Join(errsOut...)
}

// parallel runs fn over keys, at most workers at a time, and returns the non-nil errors. A panic in fn is
// re-raised in the caller once every worker has stopped (Cox-Buday: panics do not cross goroutines).
func (h *Host) parallel(keys []string, workers int, fn func(string) error) []error {
	sem := make(chan struct{}, max(1, workers))
	var (
		mu    sync.Mutex
		out   []error
		first *panicx.Panic
		wg    sync.WaitGroup
	)
	for _, k := range keys {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			defer func() {
				if p := panicx.From(recover()); p != nil {
					mu.Lock()
					if first == nil {
						first = p
					}
					mu.Unlock()
				}
			}()
			if err := fn(k); err != nil {
				mu.Lock()
				out = append(out, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if first != nil {
		panic(first)
	}
	return out
}

// Close cancels activations in flight, passivates everything (writing back first, in durable mode),
// removes the cache directory, and makes later Gets return ErrClosed. If some write-backs fail, Close closes
// those databases but keeps their local files and the cache directory (the only up-to-date copies) and
// returns the failures.
func (h *Host) Close() error {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.cancel()
	err := h.PassivateAll(h.opts.Workers)
	kept := h.closeRemaining()
	if err != nil {
		return errs.Wrap(module, err, "host closed, but %d databases were not written back; their local copies are kept in %s (the only up-to-date copies)",
			kept, h.opts.CacheDir)
	}
	return os.RemoveAll(h.opts.CacheDir)
}

// closeRemaining waits out passivations still running (e.g. a Tick's) and closes databases left warm by a
// failed write-back, keeping their files. Returns how many it kept.
func (h *Host) closeRemaining() int {
	kept := 0
	for {
		h.mu.Lock()
		var e *entry
		var key string
		for k, x := range h.entries {
			key, e = k, x
			break
		}
		if e == nil {
			h.mu.Unlock()
			return kept
		}
		if g := e.gone; g != nil {
			h.mu.Unlock()
			<-g
			continue
		}
		delete(h.entries, key) // closed: no Get can find it again
		h.mu.Unlock()
		<-e.done
		if e.db != nil {
			e.db.Close()
		}
		kept++
	}
}

// hashFile returns a file's size and sha256.
func hashFile(path string) (durable.Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return durable.Digest{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return durable.Digest{}, err
	}
	return durable.Digest{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// checkedStore asserts the durable.Store postconditions the host relies on (assertion builds only). A broken
// adapter would otherwise show up much later as a lost or corrupt database.
type checkedStore struct{ durable.Store }

func (c checkedStore) ReadManifest(ctx context.Context, key string) (durable.Manifest, durable.Version, error) {
	m, v, err := c.Store.ReadManifest(ctx, key)
	assert.That(err != nil || (v != durable.NoVersion && m.Key == key), "ReadManifest(%s) ok but version %q key %q", key, v, m.Key)
	return m, v, err
}

func (c checkedStore) FetchGeneration(ctx context.Context, m durable.Manifest, path string) (durable.Fetched, error) {
	f, err := c.Store.FetchGeneration(ctx, m, path)
	fi, serr := os.Stat(path)
	if err == nil {
		assert.That(serr == nil && fi.Size() == m.Size && f.Bytes == m.Size, "FetchGeneration(%s) ok but file %v size != manifest %d", m.Object, serr, m.Size)
	} else {
		assert.That(errors.Is(serr, os.ErrNotExist), "FetchGeneration(%s) failed (%v) but left %s", m.Object, err, path)
	}
	return f, err
}

func (c checkedStore) PublishManifest(ctx context.Context, m durable.Manifest, prev durable.Version) (durable.Version, error) {
	v, err := c.Store.PublishManifest(ctx, m, prev)
	assert.That(err != nil || (v != durable.NoVersion && v != prev), "PublishManifest(%s) ok but version %q (prev %q)", m.Key, v, prev)
	return v, err
}
