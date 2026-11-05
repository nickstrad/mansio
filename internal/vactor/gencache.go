package vactor

import (
	"container/list"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/durable"
)

// genCache is the local generation cache (object-storage guide CA1, plan OS-10): generation files kept on
// local disk after a passivation, named by content, <dir>/<sha256>.db, within a byte budget (least recently
// cached goes first). A generation is immutable, so a cached file is never stale: it is used only when the
// manifest an activation just read names exactly that sha256 and size; anything else is a miss.
//
// A file moves in and out: put renames a passivated database file in, take renames it out to the key's local
// path (SQLite will write to it, so it cannot stay shared). Renames stay within the cache directory's file
// system, so both are metadata operations.
//
// Invariant: bytes is the sum of the entries' sizes, bytes <= max, and every entry's file exists.
type genCache struct {
	dir string
	max int64

	mu    sync.Mutex
	bytes int64
	lru   *list.List               // of *cached, most recently put first
	index map[string]*list.Element // sha256 → element of lru
}

type cached struct {
	sha  string
	size int64
}

func newGenCache(dir string, max int64) (*genCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &genCache{dir: dir, max: max, lru: list.New(), index: map[string]*list.Element{}}, nil
}

func (c *genCache) path(sha string) string { return filepath.Join(c.dir, sha+".db") }

// put moves the file at src, which holds exactly d, into the cache, evicting the least recently cached
// entries until it fits. A file larger than the whole budget, or a generation already cached, is not taken.
//
//	post: true ⇒ src no longer exists and d is cached; false ⇒ src is untouched
func (c *genCache) put(src string, d durable.Digest) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.index[d.SHA256]; ok || d.Size > c.max {
		return false
	}
	for c.bytes+d.Size > c.max {
		c.drop(c.lru.Back())
	}
	if err := os.Rename(src, c.path(d.SHA256)); err != nil {
		return false
	}
	c.index[d.SHA256] = c.lru.PushFront(&cached{d.SHA256, d.Size})
	c.bytes += d.Size
	c.invariant()
	return true
}

// take moves the cached file for d to dst, if there is one of d's size (a hit).
//
//	post: true ⇒ dst holds the generation d and it is no longer cached; false ⇒ dst is untouched
func (c *genCache) take(d durable.Digest, dst string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[d.SHA256]
	if !ok {
		return false
	}
	if el.Value.(*cached).size != d.Size {
		c.drop(el) // cannot be the file the manifest names
		return false
	}
	c.forget(el)
	err := os.Rename(c.path(d.SHA256), dst)
	c.invariant()
	return err == nil
}

// drop evicts el and deletes its file. pre: c.mu held.
func (c *genCache) drop(el *list.Element) {
	c.forget(el)
	os.Remove(c.path(el.Value.(*cached).sha))
}

// forget removes el from the index. pre: c.mu held.
func (c *genCache) forget(el *list.Element) {
	e := el.Value.(*cached)
	c.lru.Remove(el)
	delete(c.index, e.sha)
	c.bytes -= e.size
}

// usage returns the entries and bytes cached (tests and gauges).
func (c *genCache) usage() (n int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.index), c.bytes
}

func (c *genCache) invariant() {
	if !assert.Enabled {
		return
	}
	var sum int64
	for el := c.lru.Front(); el != nil; el = el.Next() {
		sum += el.Value.(*cached).size
	}
	assert.That(sum == c.bytes && c.bytes <= c.max && c.lru.Len() == len(c.index),
		"genCache: %d bytes counted, %d summed, max %d, %d entries, %d indexed", c.bytes, sum, c.max, c.lru.Len(), len(c.index))
}
