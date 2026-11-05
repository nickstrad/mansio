package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	for _, body := range []string{"one", "two, longer"} { // create, then replace
		if err := WriteAtomic(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != body {
			t.Fatalf("read %q %v, want %q", b, err, body)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("temp files left: %v", ents)
	}
	if err := WriteAtomic(filepath.Join(dir, "missing", "f"), []byte("x"), 0o600); err == nil {
		t.Fatal("wrote into a missing directory")
	}
}
