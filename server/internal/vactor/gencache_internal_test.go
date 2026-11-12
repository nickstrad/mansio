package vactor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
)

// genCache spec: hit, miss, a different hash or size is a miss, eviction at the budget's on/off
// points, and a file over the budget is never cached.
func TestGenCache(t *testing.T) {
	dir := t.TempDir()
	c, err := newGenCache(filepath.Join(dir, "gen"), 10)
	if err != nil {
		t.Fatal(err)
	}
	file := func(data string) (string, string) {
		p := filepath.Join(dir, "f-"+data)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return p, data
	}
	put := func(data string) bool {
		p, _ := file(data)
		ok := c.put(p, durabletest.DigestOf([]byte(data)))
		if _, err := os.Stat(p); ok == (err == nil) {
			t.Fatalf("put(%q) = %v but the source %v", data, ok, err)
		}
		return ok
	}
	take := func(data string) bool {
		dst := filepath.Join(dir, "dst")
		os.Remove(dst)
		ok := c.take(durabletest.DigestOf([]byte(data)), dst)
		if got, err := os.ReadFile(dst); ok != (err == nil) || ok && string(got) != data {
			t.Fatalf("take(%q) = %v, dst %q %v", data, ok, got, err)
		}
		return ok
	}
	usage := func(n int, b int64) {
		t.Helper()
		if gn, gb := c.usage(); gn != n || gb != b {
			t.Fatalf("usage %d entries %d bytes, want %d %d", gn, gb, n, b)
		}
	}

	if take("none") {
		t.Fatal("hit on an empty cache")
	}
	if !put("aaaa") || !put("bbbbbb") { // 4 + 6 = 10: exactly the budget (on point)
		t.Fatal("puts within the budget refused")
	}
	usage(2, 10)
	if put("aaaa") {
		t.Fatal("a generation already cached was taken twice")
	}
	if !put("c") { // 11 > 10 (off point): evicts the least recently cached, aaaa
		t.Fatal("put over the budget refused instead of evicting")
	}
	usage(2, 7)
	if take("aaaa") {
		t.Fatal("hit on an evicted generation")
	}
	if put("kkkkkkkkkkk") { // 11 bytes: larger than the whole budget
		t.Fatal("a file larger than the budget was cached")
	}
	if !take("bbbbbb") || take("bbbbbb") {
		t.Fatal("take must hit once and then miss (the file moved out)")
	}
	usage(1, 1)
	// The manifest names the same sha256 with another size: a miss, and the entry is dropped.
	d := durabletest.DigestOf([]byte("c"))
	d.Size++
	if c.take(d, filepath.Join(dir, "x")) {
		t.Fatal("hit with the wrong size")
	}
	usage(0, 0)
	if entries, _ := os.ReadDir(c.dir); len(entries) != 0 {
		t.Fatalf("files left in the cache: %v", entries)
	}
}
