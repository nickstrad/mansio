package vactor

import (
	"container/list"
	"fmt"
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

	mu      sync.Mutex
	bytes   int64
	lru     *list.List               // of *cached, most recently put first
	index   map[string]*list.Element // sha256 → element of lru
	evicted uint64                   // names files renamed aside for deletion
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
	if !c.wantsLocked(d) {
		c.mu.Unlock()
		return false
	}
	var victims []string
	for c.bytes+d.Size > c.max {
		victims = append(victims, c.evict(c.lru.Back()))
	}
	ok := os.Rename(src, c.path(d.SHA256)) == nil
	if ok {
		c.index[d.SHA256] = c.lru.PushFront(&cached{d.SHA256, d.Size})
		c.bytes += d.Size
	}
	c.invariant()
	c.mu.Unlock()
	for _, v := range victims { // unlinking a large file can be slow: not under c.mu, which take needs
		os.Remove(v)
	}
	return ok
}

// wants reports whether put would take a file holding d: not cached yet, and within the budget. A caller
// checks it before it spends a hash on the file.
func (c *genCache) wants(d durable.Digest) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wantsLocked(d)
}

func (c *genCache) wantsLocked(d durable.Digest) bool {
	_, cached := c.index[d.SHA256]
	return !cached && d.Size <= c.max
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
		os.Remove(c.evict(el)) // cannot be the file the manifest names; small or not, this is rare
		return false
	}
	c.forget(el)
	err := os.Rename(c.path(d.SHA256), dst)
	c.invariant()
	return err == nil
}

// evict removes el from the cache and renames its file aside (a unique name: the sha256 may be cached
// again before the caller deletes it). Returns the name to delete. pre: c.mu held.
func (c *genCache) evict(el *list.Element) string {
	c.forget(el)
	c.evicted++
	aside := filepath.Join(c.dir, fmt.Sprintf("evicted-%d", c.evicted))
	os.Rename(c.path(el.Value.(*cached).sha), aside)
	return aside
}

// forget removes el from the index. pre: c.mu held.
func (c *genCache) forget(el *list.Element) {
	e := el.Value.(*cached)
	c.lru.Remove(el)
	delete(c.index, e.sha)
	c.bytes -= e.size
}

// usage returns the entries and bytes cached (tests).
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
