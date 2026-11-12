package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" driver: restore and import check the files they handle

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/fsx"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/sqlitedsn"
)

// restoreArgs are restore's inputs (key already validated).
type restoreArgs struct {
	key    string
	target string // the NEW database file; it and its -wal, -shm, -journal must not exist
	gen    uint64 // the generation to restore (a backup's answer); 0 = the current one
	object string // optional: which object of generation gen (when two epochs uploaded it)
}

// restoredFile is restore's answer, one JSON line on stdout.
type restoredFile struct {
	Restored   string    `json:"restored"`
	Key        string    `json:"key"`
	Generation uint64    `json:"generation"`
	Epoch      uint64    `json:"epoch"`
	Object     string    `json:"object"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	WrittenAt  time.Time `json:"written_at,omitzero"` // when its manifest was written (unknown for an older generation)
}

// restore writes one generation of key to a new local file (the Deno platform's scripts/restore.ts, over
// generations): the current one, or generation gen. The download is verified (size and sha256),
// then the file passes integrity_check, then a hard link publishes it complete; it never overwrites. Nothing
// in object storage changes. To serve it again, import it (a new key or a new bucket) or put it in a
// static-mode db_dir.
func (t *tool) restore(ctx context.Context, a restoreArgs) error {
	dest, err := filepath.Abs(a.target)
	if err != nil {
		return errs.Wrap(module, err, "cannot restore %s: bad target path %q", a.key, a.target)
	}
	for _, f := range []string{dest, dest + "-wal", dest + "-shm", dest + "-journal"} {
		if _, err := os.Lstat(f); !errors.Is(err, os.ErrNotExist) {
			return errs.Wrap(module, err, "cannot restore %s: refusing to overwrite %s; nothing written", a.key, f)
		}
	}
	m, err := t.restorePoint(ctx, a)
	if err != nil {
		return err
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrap(module, err, "cannot restore %s: cannot create %s; nothing written", a.key, dir)
	}
	// Same directory as the target: the hard link below needs one filesystem.
	tmp := filepath.Join(dir, "."+filepath.Base(dest)+"."+logx.NewID()+".restore")
	if _, err := t.s.FetchGeneration(ctx, m, tmp); err != nil {
		return errs.Wrap(module, err, "cannot restore %s: generation %d failed its download or verification; nothing written", a.key, m.Generation)
	}
	defer os.Remove(tmp)
	if err := checkSQLite(ctx, tmp); err != nil {
		return errs.Wrap(module, err, "cannot restore %s: generation %d is not a sound database; nothing written", a.key, m.Generation)
	}
	if err := syncFile(tmp); err != nil {
		return errs.Wrap(module, err, "cannot restore %s: cannot sync the download; nothing written", a.key)
	}
	if err := os.Link(tmp, dest); err != nil { // fails if the target appeared meanwhile
		return errs.Wrap(module, err, "cannot restore %s to %s; nothing written", a.key, dest)
	}
	if err := fsx.SyncDir(dir); err != nil {
		return errs.Wrap(module, err, "restored %s to %s, but the directory sync failed; check the file before using it", a.key, dest)
	}
	return t.printJSON(restoredFile{Restored: dest, Key: a.key, Generation: m.Generation, Epoch: m.Epoch, Object: m.Object,
		Size: m.Size, SHA256: m.SHA256, WrittenAt: m.WrittenAt})
}

// restorePoint is the manifest to download: the stored one (no gen, or gen is current), or one built from the
// metadata of generation gen's object, which FetchGeneration then verifies the bytes against.
func (t *tool) restorePoint(ctx context.Context, a restoreArgs) (durable.Manifest, error) {
	m, _, err := t.s.ReadManifest(ctx, a.key)
	if a.gen == 0 {
		switch {
		case err != nil:
			return m, errs.Wrap(module, err, "cannot restore %s: manifest read failed; nothing written", a.key)
		case m.Generation == 0:
			return m, errs.Wrap(module, durable.ErrNotFound, "cannot restore %s: it is an empty database (generation 0); nothing written", a.key)
		}
		return m, nil
	}
	if err == nil && m.Generation == a.gen && (a.object == "" || t.objectName(a.key, a.object) == m.Object) {
		return m, nil
	}
	obj, err := t.pickObject(ctx, "restore", rebuildArgs{key: a.key, gen: a.gen, object: a.object})
	if err != nil {
		return durable.Manifest{}, errs.Wrap(module, err, "cannot restore generation %d of %s; nothing written (gc keeps %d generations below the current one)",
			a.gen, a.key, max(t.gc.Keep, 0))
	}
	info, err := t.s.StatGeneration(ctx, obj)
	if err != nil {
		return durable.Manifest{}, errs.Wrap(module, err, "cannot restore %s: cannot read %s; nothing written", a.key, obj)
	}
	_, epoch, _ := durable.ParseGenObject(a.key, obj)
	if info.Key != a.key || info.Generation != a.gen || info.Epoch != epoch || info.Format != durable.Format || info.SHA256 == "" {
		return durable.Manifest{}, errs.Wrap(module, durable.ErrCorrupt, "cannot restore %s: %s carries metadata %+v, not key %s generation %d format %s; nothing written",
			a.key, obj, info, a.key, a.gen, durable.Format)
	}
	return durable.Manifest{Key: a.key, Generation: a.gen, Epoch: epoch, Object: obj, Size: info.Size, SHA256: info.SHA256}, nil
}

// checkSQLite runs integrity_check on a database file nothing else has open, without changing it.
func checkSQLite(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", sqlitedsn.ReadOnly(path, true))
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check(1)").Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New("integrity_check: " + res)
	}
	return nil
}

// sha256File is a file's size and sha256.
func sha256File(path string) (durable.Digest, error) {
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

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// printJSON writes v as one JSON line.
func (t *tool) printJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return errs.Wrap(module, err, "cannot encode the answer")
	}
	t.printf("%s\n", b)
	return nil
}
