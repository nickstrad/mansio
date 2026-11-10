package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/errs"
)

// tool runs the commands against one store. Commands print to out; they return errors for realMain to report.
type tool struct {
	s   durable.GCStore
	out io.Writer
	now func() time.Time
	tmp string // scratch directory for verify downloads
	gc  durable.GCPolicy
}

func (t *tool) printf(format string, args ...any) { fmt.Fprintf(t.out, format, args...) }

// inspect shows key's manifest, every generation object with its metadata and role, and what GC would
// delete (guide O3).
func (t *tool) inspect(ctx context.Context, key string) error {
	l, err := t.s.ListKey(ctx, key)
	if err != nil {
		return errs.Wrap(module, err, "cannot inspect %s", key)
	}
	t.printf("key       %s\nprefix    %s\n", key, durable.KeyPrefix(key))
	m, v, merr := t.s.ReadManifest(ctx, key)
	switch {
	case l.Manifest == nil:
		t.printf("manifest  none\n")
	case merr != nil:
		t.printf("manifest  UNREADABLE version %s modified %s: %v\n", l.Manifest.Version, stamp(l.Manifest.Modified), merr)
	default:
		t.printf("manifest  generation %d epoch %d object %s size %d sha256 %s\n          version %s written %s modified %s\n",
			m.Generation, m.Epoch, base(m.Object), m.Size, m.SHA256, v, stamp(m.WrittenAt), stamp(l.Manifest.Modified))
	}
	deletable, _, _ := durable.PlanSweep(l, m, v, merr, t.now(), t.gc) // the sweep's own decision
	t.printf("generations (%d, %d bytes listed):\n", len(l.Generations), l.Bytes())
	for _, o := range l.Generations {
		role := "retained"
		switch {
		case merr == nil && o.Name == m.Object:
			role = "CURRENT"
		case slices.Contains(deletable, o.Name):
			role = "orphan (gc deletes)"
		}
		info, err := t.s.StatGeneration(ctx, o.Name)
		meta := fmt.Sprintf("meta: key=%s generation=%d epoch=%d format=%s sha256=%s", info.Key, info.Generation, info.Epoch, info.Format, info.SHA256)
		if err != nil {
			meta = "meta: " + err.Error()
		}
		t.printf("  %-48s %12d  %s  %-20s %s\n", base(o.Name), o.Size, stamp(o.Modified), role, meta)
	}
	return nil
}

// verify downloads the generation key's manifest names and checks its size and sha256 (guide V1), and that
// the object's recorded metadata agrees with the manifest.
func (t *tool) verify(ctx context.Context, key string) error {
	m, _, err := t.s.ReadManifest(ctx, key)
	if err != nil {
		return errs.Wrap(module, err, "cannot verify %s", key)
	}
	if m.Generation == 0 {
		t.printf("%s: generation 0 (empty database), nothing to verify\n", key)
		return nil
	}
	info, err := t.s.StatGeneration(ctx, m.Object)
	if err != nil {
		return errs.Wrap(module, err, "cannot verify %s: generation %d (%s) is missing or unreachable", key, m.Generation, m.Object)
	}
	if !info.Holds(m.Digest()) || info.Key != key || info.Generation != m.Generation {
		return errs.Wrap(module, durable.ErrCorrupt, "%s: the metadata of %s (%+v) does not match the manifest", key, m.Object, info)
	}
	path := filepath.Join(t.tmp, "verify.db")
	defer os.Remove(path)
	f, err := t.s.FetchGeneration(ctx, m, path)
	if err != nil {
		return errs.Wrap(module, err, "%s: generation %d failed verification", key, m.Generation)
	}
	t.printf("%s: generation %d epoch %d OK (%d bytes, sha256 %s)\n", key, m.Generation, m.Epoch, f.Bytes, m.SHA256)
	return nil
}

// rebuildArgs are rebuild-manifest's inputs (key already validated).
type rebuildArgs struct {
	key       string
	gen       uint64
	epoch     uint64 // the owner's epoch, from the placement row
	object    string // optional: which object of generation gen (when two epochs uploaded it)
	ifVersion string // the manifest version the operator saw (inspect); required when a manifest exists
}

// rebuildManifest publishes a manifest for an existing generation object, rebuilt from the object's own
// metadata (guide I4), after downloading and verifying it. It never overwrites blindly: with no manifest it
// publishes only if none appears meanwhile (If-None-Match); otherwise only on the version the operator saw
// (If-Match). A readable stored manifest also bounds it: the new (epoch, generation) must be greater (W3),
// and the epoch never lower (W2). A rollback to an older generation therefore needs a new epoch.
func (t *tool) rebuildManifest(ctx context.Context, a rebuildArgs) error {
	obj, err := t.pickObject(ctx, "rebuild", a)
	if err != nil {
		return err
	}
	info, err := t.s.StatGeneration(ctx, obj)
	if err != nil {
		return errs.Wrap(module, err, "cannot rebuild %s: cannot read %s", a.key, obj)
	}
	_, objEpoch, _ := durable.ParseGenObject(a.key, obj)
	switch {
	case info.Key != a.key, info.Generation != a.gen, info.Epoch != objEpoch, info.Format != durable.Format:
		return errs.Wrap(module, nil, "cannot rebuild %s: %s carries metadata %+v, not key %s generation %d epoch %d format %s; nothing published",
			a.key, obj, info, a.key, a.gen, objEpoch, durable.Format)
	case a.epoch < objEpoch:
		return errs.Wrap(module, nil, "cannot rebuild %s at epoch %d: %s was uploaded at the later epoch %d; nothing published", a.key, a.epoch, obj, objEpoch)
	}
	m := durable.NextManifest(a.key, a.gen, objEpoch, durable.Digest{Size: info.Size, SHA256: info.SHA256}, t.now())
	m.Epoch = a.epoch
	if err := m.Validate(); err != nil {
		return errs.Wrap(module, err, "cannot rebuild %s from %s's metadata; nothing published", a.key, obj)
	}
	path := filepath.Join(t.tmp, "rebuild.db")
	defer os.Remove(path)
	if _, err := t.s.FetchGeneration(ctx, m, path); err != nil {
		return errs.Wrap(module, err, "cannot rebuild %s: %s does not verify against its metadata; nothing published", a.key, obj)
	}

	prev := durable.NoVersion
	stored, v, err := t.s.ReadManifest(ctx, a.key)
	switch {
	case errors.Is(err, durable.ErrNoManifest):
		if a.ifVersion != "" {
			return errs.Wrap(module, nil, "cannot rebuild %s: -if-version %s given but no manifest exists (it changed since you looked?); nothing published", a.key, a.ifVersion)
		}
	case err != nil && !errors.Is(err, durable.ErrBadManifest):
		return errs.Wrap(module, err, "cannot rebuild %s: manifest read failed; nothing published, safe to retry", a.key)
	case a.ifVersion == "":
		return errs.Wrap(module, nil, "cannot rebuild %s: a manifest exists; run inspect and pass its version as -if-version; nothing published", a.key)
	case err != nil:
		prev = durable.Version(a.ifVersion) // unreadable: the version is the only condition
	case v != durable.Version(a.ifVersion):
		return errs.Wrap(module, durable.ErrStale, "cannot rebuild %s: the manifest is at version %s, not %s; nothing published", a.key, v, a.ifVersion)
	case stored.Epoch > m.Epoch || stored.Epoch == m.Epoch && stored.Generation >= m.Generation:
		return errs.Wrap(module, durable.ErrStale,
			"cannot rebuild %s at (epoch %d, generation %d): the stored manifest is at (%d, %d); a rollback needs a higher epoch; nothing published",
			a.key, m.Epoch, m.Generation, stored.Epoch, stored.Generation)
	default:
		// A rollback keeps the key's seal, so the next owner's stamp puts document versions above everything
		// issued so far (docexec.VersionFloor). An unreadable manifest's seal is lost: the count restarts.
		prev, m = v, m.Succeeding(&stored)
	}
	nv, err := t.s.PublishManifest(ctx, m, prev)
	if err != nil {
		return errs.Wrap(module, err, "cannot rebuild %s: publish failed; run inspect before retrying", a.key)
	}
	t.printf("%s: published generation %d epoch %d (%s), version %s\n", a.key, m.Generation, m.Epoch, base(obj), nv)
	return nil
}

// pickObject finds the object of generation a.gen: a.object if given, else the only one listed. verb names
// the command in errors.
func (t *tool) pickObject(ctx context.Context, verb string, a rebuildArgs) (string, error) {
	if a.object != "" {
		name := t.objectName(a.key, a.object)
		if g, _, ok := durable.ParseGenObject(a.key, name); !ok || g != a.gen {
			return "", errs.Wrap(module, nil, "cannot %s %s: %q is not a generation %d object of it", verb, a.key, a.object, a.gen)
		}
		return name, nil
	}
	l, err := t.s.ListKey(ctx, a.key)
	if err != nil {
		return "", errs.Wrap(module, err, "cannot %s %s", verb, a.key)
	}
	var found []string
	for _, o := range l.Generations {
		if g, _, ok := durable.ParseGenObject(a.key, o.Name); ok && g == a.gen {
			found = append(found, o.Name)
		}
	}
	switch len(found) {
	case 0:
		return "", errs.Wrap(module, durable.ErrNotFound, "cannot %s %s: no object of generation %d", verb, a.key, a.gen)
	case 1:
		return found[0], nil
	}
	return "", errs.Wrap(module, nil, "cannot %s %s: generation %d has %d objects (%s); pick one with -object", verb, a.key, a.gen, len(found), strings.Join(found, ", "))
}

// objectName is a full generation object name: object as given, or under key's generation prefix if it is a
// bare name.
func (t *tool) objectName(key, object string) string {
	if strings.Contains(object, "/") {
		return object
	}
	return durable.GenPrefix(key) + object
}

// gc sweeps keys (durable.Sweep): one key, one shard, or all 256. A failure on one key is reported and the
// sweep goes on; the joined failures are returned.
func (t *tool) sweep(ctx context.Context, keys func(func(string) error) error, dryRun bool) error {
	var (
		failed               []error
		nKeys, nDel          int
		listed, deletedBytes int64
	)
	verb := "deleted"
	if dryRun {
		verb = "would delete"
	}
	err := keys(func(key string) error {
		r, err := durable.Sweep(ctx, t.s, key, t.now(), t.gc, dryRun)
		nKeys++
		listed += r.Bytes
		if err != nil {
			failed = append(failed, err)
			t.printf("%s: FAILED: %v\n", key, err)
			return ctx.Err() // stop only when interrupted
		}
		nDel, deletedBytes = nDel+len(r.Deleted), deletedBytes+r.DeletedBytes
		switch {
		case r.Skipped != "":
			t.printf("%s: skipped (%s)\n", key, r.Skipped)
		case len(r.Deleted) > 0:
			t.printf("%s: %s %d of %d generations, %d bytes: %s\n", key, verb, len(r.Deleted), r.Objects, r.DeletedBytes, strings.Join(bases(r.Deleted), " "))
		}
		return nil
	})
	t.printf("gc: %d keys, %d bytes listed; %s %d generations, %d bytes\n", nKeys, listed, verb, nDel, deletedBytes)
	if err != nil {
		failed = append(failed, err)
	}
	if len(failed) > 0 {
		return errs.Wrap(module, errors.Join(failed...), "gc failed on %d keys; the rest were swept, safe to rerun", len(failed))
	}
	return nil
}

// base is an object name without its key prefix.
func base(name string) string { return name[strings.LastIndex(name, "/")+1:] }

func bases(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = base(n)
	}
	return out
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
