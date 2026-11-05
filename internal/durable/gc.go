package durable

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
)

// Generation GC (object-storage guide GC1–GC6, plan OS-06). Two reapers keep a key's storage near
// (K+1) × its size:
//
//   - Owner-side retention (PlanRetention): right after a publish, under the same serialization, the owner
//     deletes the published generations more than K publishes old. Computed names, no LIST.
//   - The orphan sweeper (Sweep, PlanGC): lists one key's prefix, reads its manifest, and deletes what no
//     manifest can name any more: uploads whose publish failed, zombies' uploads, and old predecessors the
//     owner forgot (a crash, a move).
//
// Both are bounded by the same rules: never the current generation, never the K highest-numbered ones below
// it, never an object the manifest has not been rewritten since (plus a grace window), never the manifest
// itself, and never anything outside the one key's gen/ prefix.

// StoredObject is one object a listing found.
type StoredObject struct {
	Name     string    // full object name, as GenObject or ManifestObject builds it
	Size     int64     //
	Modified time.Time // the store's LastModified: the server's clock, not ours
	Version  Version   // the store's version of it (ETag); for the manifest, what ReadManifest returns
}

// Listing is what is stored under one key's prefix.
type Listing struct {
	Manifest    *StoredObject  // manifest.json; nil if the key has none
	Generations []StoredObject // every object under KeyPrefix(key)+"gen/", sorted by name
}

// Bytes is the size of everything listed.
func (l Listing) Bytes() int64 {
	var n int64
	if l.Manifest != nil {
		n = l.Manifest.Size
	}
	for _, o := range l.Generations {
		n += o.Size
	}
	return n
}

// Lister is the inventory port, for the sweeper and cmd/dbtool only: LIST never runs on a request path
// (guide K2), and an agent's credential need not allow it (guide S1). Implementations: durabletest.Store,
// objstore.Durable.
type Lister interface {
	// ListKey lists everything under KeyPrefix(key), page by page.
	//
	//	post: err == nil ⇒ l.Generations is sorted by name and every name has prefix GenPrefix(key)
	ListKey(ctx context.Context, key string) (l Listing, err error)

	// ListKeys calls fn with each database key stored in shard (two lower-case hex digits: the first byte of
	// sha256(key), see KeyPrefix), in name order, one listing page at a time: memory does not grow with the
	// shard. An error from fn stops the listing and is returned.
	ListKeys(ctx context.Context, shard string, fn func(key string) error) error
}

// GCStore is what the sweeper needs: the store plus its inventory.
type GCStore interface {
	Store
	Lister
}

// GenPrefix is the prefix of all of key's generation objects.
func GenPrefix(key string) string { return KeyPrefix(key) + "gen/" }

// Shards are the 256 values of a KeyPrefix shard, "00" … "ff".
func Shards() []string {
	out := make([]string, 256)
	for i := range out {
		out[i] = fmt.Sprintf("%02x", i)
	}
	return out
}

// ValidShard reports whether s names a shard ("00" … "ff").
func ValidShard(s string) bool { return lowerHex(s, 1) }

// ParseGenObject parses a generation object name of key into its generation and upload epoch.
func ParseGenObject(key, name string) (gen, epoch uint64, ok bool) {
	const digits = 20
	rest, found := strings.CutPrefix(name, GenPrefix(key))
	if !found || len(rest) != digits+2+digits+3 || rest[digits:digits+2] != "-e" || !strings.HasSuffix(rest, ".db") {
		return 0, 0, false
	}
	g, gerr := strconv.ParseUint(rest[:digits], 10, 64)
	e, eerr := strconv.ParseUint(rest[digits+2:digits+2+digits], 10, 64)
	if gerr != nil || eerr != nil || name != GenObject(key, g, e) { // canonical form only: no signs, no padding tricks
		return 0, 0, false
	}
	return g, e, true
}

// GCPolicy is how much the reapers keep.
type GCPolicy struct {
	// Keep is K: the generations kept below the current one (guide GC1). Default 2; negative means 0.
	Keep int
	// Grace bounds delayed requests (guide GC3): an object is deleted only once the manifest was rewritten
	// more than Grace after the object's upload, and the upload is more than Grace old. Default 10 min,
	// 2 × the default WriteBackTimeout; use 2 × the writers' WriteBackTimeout.
	Grace time.Duration
}

// DefaultGCPolicy is K = 2 and a 10-minute grace window.
var DefaultGCPolicy = GCPolicy{Keep: 2, Grace: 10 * time.Minute}

// Normalize fills the defaults: a zero Keep or Grace takes DefaultGCPolicy's value. It is idempotent: a
// negative Keep (K = 0) stays negative, and the planners read it as 0.
func (p GCPolicy) Normalize() GCPolicy {
	if p.Keep == 0 {
		p.Keep = DefaultGCPolicy.Keep
	}
	if p.Grace <= 0 {
		p.Grace = DefaultGCPolicy.Grace
	}
	return p
}

// keep is K as the planners use it.
func (p GCPolicy) keep() int { return max(p.Keep, 0) }

// PlanGC decides which of key's generation objects the sweeper may delete. Pure. m is the current manifest,
// mod its LastModified, objs the listed generation objects, now the time of the decision (on a clock
// comparable to the store's LastModified). An object is deleted only if all of these hold:
//
//   - it is a generation object of m.Key (anything else under gen/ is kept: GC4);
//   - it is not m.Object, and not among the p.Keep highest-numbered generations below m.Generation (GC1);
//   - o.Modified + p.Grace < mod: the manifest was rewritten after the upload, so every publish that could
//     name o was conditional on a version that is gone and can never return (GC3, W3);
//   - now − o.Modified > p.Grace (GC3: bounds a delayed request).
//
// Orphans numbered above m.Generation (an upload whose publish failed) are deleted by the same rules.
//
//	pre:  p is normalized; m is valid
//	post: no name returned is m.Object or outside GenPrefix(m.Key); the result is sorted
func PlanGC(m Manifest, mod time.Time, objs []StoredObject, now time.Time, p GCPolicy) []string {
	type gen struct {
		o          StoredObject
		num, epoch uint64
	}
	var below, rest []gen
	for _, o := range objs {
		g, e, ok := ParseGenObject(m.Key, o.Name)
		switch {
		case !ok, o.Name == m.Object, o.Modified.IsZero():
			continue // keep: not ours to judge, the current one, or no evidence of its age
		case g < m.Generation:
			below = append(below, gen{o, g, e})
		default:
			rest = append(rest, gen{o, g, e})
		}
	}
	slices.SortFunc(below, func(a, b gen) int { // highest first
		return cmp.Or(cmp.Compare(b.num, a.num), cmp.Compare(b.epoch, a.epoch))
	})
	below = below[min(p.keep(), len(below)):] // the K highest below the current one stay
	var del []string
	for _, g := range append(below, rest...) {
		if g.o.Modified.Add(p.Grace).Before(mod) && now.Sub(g.o.Modified) > p.Grace {
			del = append(del, g.o.Name)
		}
	}
	slices.Sort(del)
	return del
}

// PlanRetention is owner-side retention (guide GC2): given the objects this owner published, oldest first
// and distinct, it keeps the current one and the p.Keep before it, and returns the rest to delete. Pure.
//
//	post: keep is the last min(len, K+1) of published; drop + keep == published
func PlanRetention(published []string, p GCPolicy) (keep, drop []string) {
	n := max(len(published)-p.keep()-1, 0)
	return published[n:], published[:n]
}

// SweepResult is what one Sweep of one key found and did (guide GC5: report objects and bytes per run).
type SweepResult struct {
	Key          string
	Objects      int      // generation objects listed
	Bytes        int64    // bytes listed under the key's prefix, manifest included
	Deleted      []string // generation objects deleted (or, in a dry run, to delete)
	DeletedBytes int64
	// Skipped says why nothing was decided: "no manifest" (never written, or being dropped: GC5a), or
	// "manifest moved" (it changed between the listing and the read; the next pass decides).
	Skipped string
}

// Sweep is the orphan sweeper for one key: it lists the key's prefix, reads its manifest, plans with PlanGC
// and, unless dryRun, deletes in batches. It never writes the manifest and never reaches outside the key's
// prefix (GC4). A key with no manifest is left alone: dropping a database is a placement tombstone and a
// prefix delete, not GC (GC5a).
//
// The listing and the read must agree on the manifest's version; if it moved in between, Sweep skips the key
// (the listed LastModified would not be the read manifest's).
func Sweep(ctx context.Context, s GCStore, key string, now time.Time, p GCPolicy, dryRun bool) (SweepResult, error) {
	p = p.Normalize()
	r := SweepResult{Key: key}
	l, err := s.ListKey(ctx, key)
	if err != nil {
		return r, errs.Wrap(module, err, "cannot sweep %s: listing failed; nothing deleted, safe to retry", key)
	}
	r.Objects, r.Bytes = len(l.Generations), l.Bytes()
	var m Manifest
	var v Version
	if l.Manifest != nil { // no manifest: nothing to read
		m, v, err = s.ReadManifest(ctx, key)
	}
	if r.Deleted, r.Skipped, err = PlanSweep(l, m, v, err, now, p); err != nil {
		return r, errs.Wrap(module, err, "cannot sweep %s: manifest read failed; nothing deleted, safe to retry", key)
	}
	for _, o := range l.Generations {
		if _, found := slices.BinarySearch(r.Deleted, o.Name); found {
			r.DeletedBytes += o.Size
		}
	}
	if dryRun || len(r.Deleted) == 0 {
		return r, nil
	}
	if err := s.DeleteGenerations(ctx, key, r.Deleted); err != nil {
		return r, errs.Wrap(module, err, "cannot sweep %s: deleting %d orphans failed; some may remain, safe to retry", key, len(r.Deleted))
	}
	return r, nil
}

// PlanSweep is Sweep's decision for one key, from its listing and the manifest read that followed (m, v,
// readErr; ignored when the listing has no manifest). Pure. skipped is "no manifest" or "manifest moved"
// (see SweepResult); err is a failed read.
//
//	pre: p is normalized
func PlanSweep(l Listing, m Manifest, v Version, readErr error, now time.Time, p GCPolicy) (del []string, skipped string, err error) {
	switch {
	case l.Manifest == nil, errors.Is(readErr, ErrNoManifest):
		return nil, "no manifest", nil
	case readErr != nil:
		return nil, "", readErr
	case v != l.Manifest.Version: // the listed LastModified is not this manifest's
		return nil, "manifest moved", nil
	}
	return PlanGC(m, l.Manifest.Modified, l.Generations, now, p), "", nil
}

// CheckDeletable checks names before a DeleteGenerations of key: every one must be a generation object of
// key (GC4). Adapters call it at their edge, so a bug upstream cannot reach another key or the manifest.
func CheckDeletable(key string, names []string) error {
	for _, n := range names {
		if _, _, ok := ParseGenObject(key, n); !ok {
			return errs.Wrap(module, nil, "refusing to delete %q for %s: not one of its generation objects; nothing deleted", n, key)
		}
	}
	return nil
}
