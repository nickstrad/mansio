package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/coldload"
)

func TestBuildTemplate(t *testing.T) {
	dir := t.TempDir()
	s := coldload.SizeClass{Name: "small", Bytes: 64 << 10}

	path, size, err := buildTemplate(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	if size < s.Bytes {
		t.Fatalf("size %d < target %d", size, s.Bytes)
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Fatal("template left a -wal file (not checkpointed)")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM items").Scan(&rows); err != nil || rows == 0 {
		t.Fatalf("items rows = %d, %v", rows, err)
	}
	db.Close()

	// Reuse: a file already ≥ 90% of the target is kept as is.
	before, _ := os.Stat(path)
	if _, _, err := buildTemplate(dir, s); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("template rebuilt although it was big enough")
	}
	// Rebuild: a bigger target replaces it.
	if _, size2, err := buildTemplate(dir, coldload.SizeClass{Name: "small", Bytes: 256 << 10}); err != nil || size2 < 256<<10 {
		t.Fatalf("rebuild: size %d, %v", size2, err)
	}
}

func TestActorOpenerNeverCreatesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, err := (actorOpener{}).Open(path); err == nil {
		t.Fatal("opened a missing file")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("Open created the file")
	}
}

func TestActorOpenerServesQueries(t *testing.T) {
	path, _, err := buildTemplate(t.TempDir(), coldload.SizeClass{Name: "x", Bytes: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	db, err := (actorOpener{}).Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Query(t.Context(), "SELECT k, name FROM items WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Query(t.Context(), "SELECT nope FROM nowhere"); err == nil {
		t.Fatal("bad query succeeded")
	}
}

// PG-07: the template is built at its own path even when the directory name holds URI characters.
func TestBuildTemplateInADirectoryWithURICharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a#b?c")
	path, _, err := buildTemplate(dir, coldload.SizeClass{Name: "small", Bytes: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "small.sqlite") {
		t.Fatalf("path = %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("template not at its own path: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 1 {
		t.Fatalf("stray files next to the directory: %v", entries)
	}
}
