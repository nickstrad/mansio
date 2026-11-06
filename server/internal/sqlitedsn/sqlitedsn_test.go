package sqlitedsn_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/nickstrad/mansio/server/internal/sqlitedsn"
)

func TestBuild(t *testing.T) {
	tests := []struct {
		path    string
		pragmas []string
		want    string
	}{
		{"/tmp/a.db", nil, "file:/tmp/a.db?_pragma=foreign_keys(1)"},
		{"/tmp/a.db", []string{"journal_mode(WAL)", "synchronous(OFF)"}, "file:/tmp/a.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)"},
		{"/t/a#b?c.db", nil, "file:/t/a%23b%3Fc.db?_pragma=foreign_keys(1)"},
		{"/t/c%20d e.db", nil, "file:/t/c%2520d%20e.db?_pragma=foreign_keys(1)"},
		{"rel/x.db", nil, "file:rel/x.db?_pragma=foreign_keys(1)"},
		{":memory:", nil, "file::memory:?_pragma=foreign_keys(1)"},
	}
	for _, tc := range tests {
		if got := sqlitedsn.Build(tc.path, tc.pragmas...); got != tc.want {
			t.Errorf("Build(%q, %v) = %q, want %q", tc.path, tc.pragmas, got, tc.want)
		}
	}
}

// Integration: each URI-special path opens its own file, and pragmas are applied.
func TestBuildOpensTheNamedFileWithPragmas(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a#b?c")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"x#1.db", "x?2.db", "x%203.db", "x 4.db"} {
		path := filepath.Join(dir, name)
		db, err := sql.Open("sqlite", sqlitedsn.Build(path))
		if err != nil {
			t.Fatal(err)
		}
		var fk int
		if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
			t.Fatalf("%s: foreign_keys = %d, %v", name, fk, err)
		}
		if _, err := db.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: not created at its own path: %v", name, err)
		}
	}
}

// Integration: a read-only DSN reads the file and refuses writes; it never creates a missing file.
func TestReadOnly(t *testing.T) {
	if got, want := sqlitedsn.ReadOnly("/t/a#b.db", true), "file:/t/a%23b.db?_pragma=foreign_keys(1)&mode=ro&immutable=1"; got != want {
		t.Fatalf("ReadOnly = %q, want %q", got, want)
	}
	path := filepath.Join(t.TempDir(), "x?1.db")
	db, err := sql.Open("sqlite", sqlitedsn.Build(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, immutable := range []bool{false, true} {
		ro, err := sql.Open("sqlite", sqlitedsn.ReadOnly(path, immutable))
		if err != nil {
			t.Fatal(err)
		}
		var x int
		if err := ro.QueryRow("SELECT x FROM t").Scan(&x); err != nil || x != 7 {
			t.Fatalf("immutable %v: read %d, %v", immutable, x, err)
		}
		if _, err := ro.Exec("INSERT INTO t VALUES (8)"); err == nil {
			t.Fatalf("immutable %v: a write succeeded", immutable)
		}
		ro.Close()
	}
	missing := filepath.Join(t.TempDir(), "none.db")
	ro, _ := sql.Open("sqlite", sqlitedsn.ReadOnly(missing, false))
	if err := ro.Ping(); err == nil {
		t.Fatal("opened a missing file read-only")
	}
	ro.Close()
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("read-only open created the file")
	}
}
