package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/file"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/sqlitedsn"
)

// importArgs are import's inputs (validated by parseCommand).
type importArgs struct {
	file         string // the database file (Deno-written <DATA_DIR>/<app>.sqlite, <TENANTS_DIR>/<tenant>.db, or a restore)
	tenant, app  string // the key is tenant:app (ADR 0011)
	schema       string // the app's schema in fingerprint form (as schemas_dir holds it); the file's must equal it
	placementDir string // the single-node authority (agentd's placement_dir); "" = the control plane creates the row
	vm           string // the owner the placement row names (agentd's vm); required with placementDir
}

// check reports what is wrong with the arguments ("" = nothing).
func (a importArgs) check() string {
	switch {
	case !pg.ValidTenant(a.tenant):
		return fmt.Sprintf("-tenant %q must match %s", a.tenant, `^[a-z0-9][a-z0-9-]{0,62}$`)
	case !pg.ValidName(a.app):
		return fmt.Sprintf("-app %q must match %s", a.app, pg.NamePattern)
	case a.schema == "":
		return "-schema is required: the app's schema in fingerprint form (as agentd's schemas_dir holds it)"
	case (a.placementDir == "") != (a.vm == ""):
		return "-placement-dir and -vm go together"
	case a.vm != "" && !pg.ValidVM(a.vm):
		return fmt.Sprintf("-vm %q is not a valid VM id", a.vm)
	}
	if err := durable.ValidateKey(a.tenant + ":" + a.app); err != nil {
		return err.Error()
	}
	return ""
}

// importEpoch is the epoch an imported database starts at: the first owner's (flow B creates at 1).
const importEpoch = 1

// imported is import's answer, one JSON line on stdout.
type imported struct {
	Imported    string           `json:"imported"`
	Fingerprint string           `json:"fingerprint"`
	Manifest    durable.Manifest `json:"manifest"`
	Placement   *placementRow    `json:"placement"` // null without -placement-dir
	Resumed     bool             `json:"resumed"`   // an earlier import of the same bytes had already done some steps
}

type placementRow struct {
	Owner string `json:"owner"`
	Epoch uint64 `json:"epoch"`
	Gen   uint64 `json:"gen"`
}

// importFile makes an existing SQLite file the first generation of a new key (replace-platform plan §4.3):
//
//  1. a consistent standalone copy (VACUUM INTO from a read-only open: the source is not changed, and a -wal
//     it has is read), which must pass integrity_check;
//  2. the copy's _platform_schema fingerprint must be the app's schema (-schema), so the agent opens it with
//     no drift;
//  3. the key must be new: no manifest, and no placement row other than the one this import writes;
//  4. upload generation 1 at epoch 1 (create-only, guide I1), then publish the manifest only if none exists
//     (If-None-Match, W1), then write the placement row ACTIVE(vm, 1, 1) if the authority has none (a CAS).
//
// Data first, pointer second (I3). Every ambiguous step is resolved by re-reading (R3), and a rerun with the
// same file resumes where an interrupted one stopped. Once the row exists, the control plane's claim serves
// the key. Without -placement-dir only the generation and manifest are written: controld's create writes a
// row at generation 0, and the owner's claim adopts the manifest (its generation is not behind the row's).
func (t *tool) importFile(ctx context.Context, a importArgs) error {
	key := a.tenant + ":" + a.app
	want, err := os.ReadFile(a.schema)
	if err != nil {
		return errs.Wrap(module, err, "cannot import %s: cannot read the schema file; nothing written", key)
	}
	want = bytes.TrimRight(want, "\r\n")
	if err := docexec.CanonicalText(string(want)); err != nil {
		return errs.Wrap(module, err, "cannot import %s: %s is not a schema in fingerprint form (JSON.stringify of the schema); nothing written", key, a.schema)
	}
	snap := filepath.Join(t.tmp, "import.db")
	defer os.Remove(snap)
	if err := snapshotFile(ctx, a.file, snap); err != nil {
		return errs.Wrap(module, err, "cannot import %s: cannot copy %s (is it a SQLite database?); nothing written", key, a.file)
	}
	if err := checkSQLite(ctx, snap); err != nil {
		return errs.Wrap(module, err, "cannot import %s: %s is not a sound database; nothing written", key, a.file)
	}
	have, err := storedSchema(ctx, snap)
	if err != nil {
		return errs.Wrap(module, err, "cannot import %s: %s has no readable _platform_schema record; nothing written", key, a.file)
	}
	if have != string(want) {
		return errs.Wrap(module, nil, "cannot import %s: %s holds schema %s, the app's is %s (drift: migrate it or pick the matching schema); nothing written",
			key, a.file, docschema.Fingerprint(have), docschema.Fingerprint(string(want)))
	}
	d, err := sha256File(snap)
	if err != nil {
		return errs.Wrap(module, err, "cannot import %s: cannot hash the copy; nothing written", key)
	}

	var pl *file.Placement
	row := owner.Place{Owner: a.vm, Epoch: importEpoch, Gen: 1, State: owner.PlaceActive}
	if a.placementDir != "" {
		if pl, err = file.Open(a.placementDir); err != nil {
			return errs.Wrap(module, err, "cannot import %s; nothing written", key)
		}
		switch cur, err := pl.Get(ctx, key); {
		case errors.Is(err, owner.ErrNotFound), err == nil && cur == row:
		case err != nil:
			return errs.Wrap(module, err, "cannot import %s: placement read failed; nothing written, safe to retry", key)
		default:
			return errs.Wrap(module, nil, "cannot import %s: it already has a placement row %+v; nothing written", key, cur)
		}
	}

	out := imported{Imported: a.file, Fingerprint: docschema.Fingerprint(have)}
	if out.Manifest, out.Resumed, err = t.publishImport(ctx, key, snap, d); err != nil {
		return err
	}
	if pl != nil {
		resumed, err := createRow(ctx, pl, key, row)
		if err != nil {
			return errs.Wrap(module, err, "imported %s as generation 1, but the placement row is not written; rerun the same import to finish", key)
		}
		out.Resumed = out.Resumed || resumed
		out.Placement = &placementRow{Owner: row.Owner, Epoch: uint64(row.Epoch), Gen: uint64(row.Gen)}
	}
	return t.printJSON(out)
}

// publishImport uploads snap as key's generation 1 and publishes its manifest, or finds that an earlier
// import of the same bytes already did (resumed).
func (t *tool) publishImport(ctx context.Context, key, snap string, d durable.Digest) (durable.Manifest, bool, error) {
	switch cur, _, err := t.s.ReadManifest(ctx, key); {
	case err == nil && cur.Generation == 1 && cur.Epoch == importEpoch && cur.SHA256 == d.SHA256 && cur.Size == d.Size:
		return cur, true, nil // published by an earlier run
	case err == nil:
		return cur, false, errs.Wrap(module, durable.ErrExists, "cannot import %s: it already has a durable copy (generation %d, epoch %d); nothing written",
			key, cur.Generation, cur.Epoch)
	case !errors.Is(err, durable.ErrNoManifest):
		return cur, false, errs.Wrap(module, err, "cannot import %s: manifest read failed; nothing written, safe to retry", key)
	}
	m := durable.NextManifest(key, 1, importEpoch, d, t.now())
	resumed := false
	if err := t.s.PutGeneration(ctx, m, snap); err != nil {
		// Ambiguous or already there: the object's recorded digest says whether it holds these bytes (R3).
		info, serr := t.s.StatGeneration(ctx, m.Object)
		if serr != nil || !info.Holds(d) {
			return m, false, errs.Wrap(module, err, "cannot import %s: upload of %s failed; no manifest names it, safe to retry", key, m.Object)
		}
		resumed = errors.Is(err, durable.ErrExists)
	}
	if _, err := t.s.PublishManifest(ctx, m, durable.NoVersion); err != nil {
		stored, _, rerr := t.s.ReadManifest(ctx, key)
		if rerr != nil || !stored.Same(m) {
			return m, false, errs.Wrap(module, err, "cannot import %s: the manifest publish failed or another writer published first; "+
				"rerun the same import to finish (it refuses if another writer published)", key)
		}
		// It applied; the reply was lost.
	}
	return m, resumed, nil
}

// createRow writes key's placement row if there is none (a CAS from "no row"); resumed means it was already
// the row.
func createRow(ctx context.Context, pl *file.Placement, key string, row owner.Place) (resumed bool, err error) {
	err = pl.CAS(ctx, key, owner.Place{}, row)
	if errors.Is(err, owner.ErrConflict) {
		if cur, gerr := pl.Get(ctx, key); gerr == nil && cur == row {
			return true, nil
		}
	}
	return false, err
}

// snapshotFile writes a consistent standalone copy of the database at src to dst (VACUUM INTO, from a
// read-only connection: src is not changed).
func snapshotFile(ctx context.Context, src, dst string) error {
	db, err := sql.Open("sqlite", sqlitedsn.ReadOnly(src, false))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", dst)
	return err
}

// storedSchema is the database's _platform_schema definition (docexec's record, written byte for byte as
// platform/sqlite.ts does).
func storedSchema(ctx context.Context, path string) (string, error) {
	db, err := sql.Open("sqlite", sqlitedsn.ReadOnly(path, true))
	if err != nil {
		return "", err
	}
	defer db.Close()
	var def string
	err = db.QueryRowContext(ctx, `SELECT definition FROM "_platform_schema" WHERE id = 1`).Scan(&def)
	return def, err
}
