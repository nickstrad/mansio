package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/file"
	"github.com/nickstrad/mansio/server/internal/sqlitedsn"
)

// ttSchema is an app schema in fingerprint form, as agentd's schemas_dir holds it.
const ttSchema = `{"games":{"fields":{"player":{"node":{"kind":"string"}}},"storage":"columns","indexes":[]}}`

// docFile writes a document database as the Deno platform leaves it: a _platform_schema record holding
// schema, and a table with rows rows. It returns the path.
func docFile(t *testing.T, schema string, rows int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.sqlite")
	db, err := sql.Open("sqlite", sqlitedsn.Build(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS "_platform_schema" (id INTEGER PRIMARY KEY CHECK (id = 1), definition TEXT NOT NULL) STRICT`,
		`CREATE TABLE "games" ("_id" TEXT PRIMARY KEY, "player" TEXT NOT NULL) STRICT`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if schema != "" {
		if _, err := db.Exec(`INSERT INTO "_platform_schema" VALUES (1, ?)`, schema); err != nil {
			t.Fatal(err)
		}
	}
	for i := range rows {
		if _, err := db.Exec(`INSERT INTO "games" VALUES (?, 'amy')`, "g"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// rowCount counts the games in a database file, read-only.
func rowCount(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", sqlitedsn.ReadOnly(path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM "games"`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// publish uploads the file at path as key's generation gen at epoch 1 and publishes it (prev: the version it
// replaces). It returns the manifest and its version.
func (r *rig) publish(key string, gen uint64, path string, prev durable.Version) (durable.Manifest, durable.Version) {
	r.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	m := durable.NextManifest(key, gen, 1, durabletest.DigestOf(data), time.Now())
	if err := r.d.PutGeneration(ctx, m, path); err != nil {
		r.t.Fatal(err)
	}
	v, err := r.d.PublishManifest(ctx, m, prev)
	if err != nil {
		r.t.Fatal(err)
	}
	return m, v
}

// bothBackends runs suite against the in-process S3 always and SeaweedFS when COLDLOAD_S3 is set.
func bothBackends(t *testing.T, suite func(*testing.T, *rig)) {
	t.Run("in-process S3", func(t *testing.T) { suite(t, fakeRig(t)) })
	t.Run("SeaweedFS", func(t *testing.T) {
		ep := os.Getenv("COLDLOAD_S3")
		if ep == "" {
			t.Skip("set COLDLOAD_S3=host:port to run against a real S3")
		}
		suite(t, newRig(t, ep, "coldload", "coldload", "coldload-secret"))
	})
}

// decodeLine decodes the JSON line a command printed.
func decodeLine(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), v); err != nil {
		t.Fatalf("not one JSON line: %q (%v)", out, err)
	}
}

// Restore writes the current generation, or an older one by number, to a new file; it never overwrites,
// leaves no file when the download or the database check fails, and refuses an empty database.
func TestRestore(t *testing.T) { bothBackends(t, restoreSuite) }

func restoreSuite(t *testing.T, r *rig) {
	key := "acme:ttt"
	m1, v := r.publish(key, 1, docFile(t, ttSchema, 1), durable.NoVersion)
	m2, _ := r.publish(key, 2, docFile(t, ttSchema, 2), v)
	dir := t.TempDir()

	code, out, errOut := r.run("restore", key, filepath.Join(dir, "cur.db"))
	var got restoredFile
	if code != exitOK {
		t.Fatalf("restore: %d %s", code, errOut)
	}
	decodeLine(t, out, &got)
	if got.Generation != 2 || got.SHA256 != m2.SHA256 || got.Object != m2.Object || got.Restored != filepath.Join(dir, "cur.db") ||
		rowCount(t, got.Restored) != 2 {
		t.Fatalf("restore of the current generation: %+v", got)
	}
	_, out, _ = r.run("restore", "-gen", "1", key, filepath.Join(dir, "old.db"))
	decodeLine(t, out, &got)
	if got.Generation != 1 || got.SHA256 != m1.SHA256 || rowCount(t, got.Restored) != 1 {
		t.Fatalf("restore -gen 1: %+v", got)
	}
	if d, _ := sha256File(got.Restored); d.SHA256 != m1.SHA256 {
		t.Fatalf("restored bytes hash to %s, the generation's are %s", d.SHA256, m1.SHA256)
	}
	r.mustRun(exitOK, "restore", "-gen", "2", "-object", base(m2.Object), key, filepath.Join(dir, "obj.db"))

	// Never overwrite: the file or one of its sidecars exists.
	os.WriteFile(filepath.Join(dir, "side.db-wal"), nil, 0o644)
	for _, target := range []string{"cur.db", "side.db"} {
		if out := r.mustRun(exitError, "restore", key, filepath.Join(dir, target)); !strings.Contains(out, "refusing to overwrite") {
			t.Errorf("%s: %s", target, out)
		}
	}
	if out := r.mustRun(exitError, "restore", "-gen", "7", key, filepath.Join(dir, "none.db")); !strings.Contains(out, "no object of generation 7") {
		t.Errorf("missing generation: %s", out)
	}
	r.mustRun(exitError, "restore", "-gen", "1", "-object", "nope", key, filepath.Join(dir, "none.db"))
	r.mustRun(exitError, "restore", "other:ttt", filepath.Join(dir, "none.db")) // no manifest

	// A corrupt object, and a generation that is not a database: nothing left behind.
	bad := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(bad, []byte("not a database"), 0o644)
	if _, err := r.st.PutFile(ctx, r.root+m1.Object, bad); err != nil {
		t.Fatal(err)
	}
	r.mustRun(exitError, "restore", "-gen", "1", key, filepath.Join(dir, "corrupt.db"))
	junkKey := "acme:junk"
	r.publish(junkKey, 1, bad, durable.NoVersion)
	if out := r.mustRun(exitError, "restore", junkKey, filepath.Join(dir, "junk.db")); !strings.Contains(out, "not a sound database") {
		t.Errorf("junk generation: %s", out)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "cur.db obj.db old.db side.db-wal" {
		t.Fatalf("files left in the target directory: %v", names)
	}

	// An empty database (generation 0) has nothing to restore.
	if _, _, err := durable.Stamp(ctx, r.d, "acme:empty", 1, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if out := r.mustRun(exitError, "restore", "acme:empty", filepath.Join(dir, "empty.db")); !strings.Contains(out, "empty database") {
		t.Errorf("empty: %s", out)
	}
}

// Import makes a document database the first generation of tenant:app and writes its placement row; a rerun
// with the same file resumes; a different file, a drifted or missing schema record, a non-database and a key
// that already has a row are refused before anything is uploaded.
func TestImport(t *testing.T) { bothBackends(t, importSuite) }

func importSuite(t *testing.T, r *rig) {
	schema := filepath.Join(t.TempDir(), "v1.json")
	os.WriteFile(schema, []byte(ttSchema+"\n"), 0o644) // an editor's newline is not part of it
	placement := filepath.Join(t.TempDir(), "placement")
	src := docFile(t, ttSchema, 3)
	before, _ := sha256File(src)
	args := []string{"import", src, "-tenant", "acme", "-app", "ttt", "-schema", schema, "-placement-dir", placement, "-vm", "vm-1"}

	code, out, errOut := r.run(args...)
	if code != exitOK {
		t.Fatalf("import: %d %s", code, errOut)
	}
	var got imported
	decodeLine(t, out, &got)
	m, _, err := r.d.ReadManifest(ctx, "acme:ttt")
	if err != nil || !m.Same(got.Manifest) || m.Generation != 1 || m.Epoch != 1 || got.Resumed ||
		got.Placement == nil || *got.Placement != (placementRow{Owner: "vm-1", Epoch: 1, Gen: 1}) || !strings.HasPrefix(got.Fingerprint, "sha256:") {
		t.Fatalf("import answer %+v, stored manifest %+v (%v)", got, m, err)
	}
	pl, _ := file.Open(placement)
	if row, err := pl.Get(ctx, "acme:ttt"); err != nil || row != (owner.Place{Owner: "vm-1", Epoch: 1, Gen: 1, State: owner.PlaceActive}) {
		t.Fatalf("placement row %+v, %v", row, err)
	}
	if after, _ := sha256File(src); after != before {
		t.Fatal("import changed the source file")
	}
	restored := filepath.Join(t.TempDir(), "back.db")
	r.mustRun(exitOK, "restore", "acme:ttt", restored)
	if n := rowCount(t, restored); n != 3 {
		t.Fatalf("restored import holds %d rows", n)
	}

	// Rerun: every step is found done.
	_, out, _ = r.run(args...)
	decodeLine(t, out, &got)
	if !got.Resumed || !m.Same(got.Manifest) {
		t.Fatalf("rerun: %+v", got)
	}
	// Interrupted after the upload (of the same snapshot: VACUUM INTO of an unchanged file is deterministic):
	// the rerun publishes and writes the row.
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := snapshotFile(ctx, restored, snap); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(snap)
	half := durable.NextManifest("acme:half", 1, 1, durabletest.DigestOf(data), time.Now())
	if err := r.d.PutGeneration(ctx, half, snap); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = r.run("import", restored, "-tenant", "acme", "-app", "half", "-schema", schema, "-placement-dir", placement, "-vm", "vm-1")
	if code != exitOK {
		t.Fatalf("resume after upload: %s", errOut)
	}
	decodeLine(t, out, &got)
	if !got.Resumed || got.Manifest.SHA256 != half.SHA256 {
		t.Fatalf("resume after upload: %+v", got)
	}

	objects := func() int {
		n := 0
		for _, k := range []string{"acme:ttt", "acme:new"} {
			l, _ := r.d.ListKey(ctx, k)
			n += len(l.Generations)
		}
		return n
	}
	n := objects()
	refused := func(file, app, why string, extra ...string) {
		t.Helper()
		a := append([]string{"import", file, "-tenant", "acme", "-app", app, "-schema", schema}, extra...)
		if out := r.mustRun(exitError, a...); !strings.Contains(out, why) {
			t.Errorf("%s: %s, want %q", app, out, why)
		}
	}
	refused(docFile(t, ttSchema, 1), "ttt", "already has a placement row", "-placement-dir", placement, "-vm", "vm-2")
	refused(docFile(t, ttSchema, 1), "ttt", "already has a durable copy") // other bytes, no -placement-dir
	refused(docFile(t, strings.Replace(ttSchema, "columns", "json", 1), 1), "new", "drift")
	refused(docFile(t, "", 1), "new", "no readable _platform_schema")
	junk := filepath.Join(t.TempDir(), "junk.sqlite")
	os.WriteFile(junk, []byte(strings.Repeat("not a database ", 100)), 0o644)
	refused(junk, "new", "cannot copy")
	refused(filepath.Join(t.TempDir(), "missing.sqlite"), "new", "cannot copy")
	if objects() != n {
		t.Fatal("a refused import uploaded something")
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "missing.sqlite")); err == nil {
		t.Fatal("import created the missing file")
	}
}

// Import reads a source left in WAL mode with frames not checkpointed (a Deno server that did not stop
// cleanly) without changing it.
func TestImportReadsTheWAL(t *testing.T) {
	r := fakeRig(t)
	schema := filepath.Join(t.TempDir(), "v1.json")
	os.WriteFile(schema, []byte(ttSchema), 0o644)
	src := docFile(t, ttSchema, 1)
	db, err := sql.Open("sqlite", sqlitedsn.Build(src, "journal_mode(WAL)", "wal_autocheckpoint(0)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO "games" VALUES ('w1', 'bo'), ('w2', 'cy')`); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(src + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("no WAL frames: %v", err)
	}
	main, _ := sha256File(src)
	r.mustRun(exitOK, "import", src, "-tenant", "acme", "-app", "wal", "-schema", schema)
	restored := filepath.Join(t.TempDir(), "back.db")
	r.mustRun(exitOK, "restore", "acme:wal", restored)
	if n := rowCount(t, restored); n != 3 {
		t.Fatalf("imported %d rows, want 3 (the WAL's too)", n)
	}
	if after, _ := sha256File(src); after != main {
		t.Fatal("import checkpointed or changed the source")
	}
}

func TestDurabilityUsage(t *testing.T) {
	r := fakeRig(t)
	for _, args := range [][]string{
		{"restore"}, {"restore", "k"}, {"restore", "/bad", "x"}, {"restore", "k", "x", "y"},
		{"import"}, {"import", "f"}, {"import", "f", "-tenant", "acme", "-app", "a"}, // no -schema
		{"import", "f", "-tenant", "Acme", "-app", "a", "-schema", "s"},
		{"import", "f", "-tenant", "acme", "-app", "a:b", "-schema", "s"},
		{"import", "f", "-tenant", "acme", "-app", "a", "-schema", "s", "-vm", "vm-1"}, // -vm without -placement-dir
		{"import", "f", "g", "-tenant", "acme", "-app", "a", "-schema", "s"},
		{"import", "-tenant", "acme", "-app", "a", "-schema", "s"},
	} {
		if code, _, _ := r.run(args...); code != exitUsage {
			t.Errorf("dbtool %v = %d, want %d", args, code, exitUsage)
		}
	}
	// The file may also come last.
	schema := filepath.Join(t.TempDir(), "v1.json")
	os.WriteFile(schema, []byte(ttSchema), 0o644)
	r.mustRun(exitOK, "import", "-tenant", "acme", "-app", "last", "-schema", schema, docFile(t, ttSchema, 1))
}
