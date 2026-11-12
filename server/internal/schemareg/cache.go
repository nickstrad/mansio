package schemareg

import (
	"container/list"

	"github.com/nickstrad/mansio/server/internal/assert"
)

// cache is a least-recently-used map of (app, fingerprint) → schema text, bounded by entries and by bytes (so a
// flood of distinct schemas cannot exhaust memory). Entries never go stale: a fingerprint names one text forever,
// and a registration is never withdrawn, so there is no TTL. Not safe for concurrent use (Client holds its lock).
// Pure: no I/O, no clock.
//
//	Invariant: len(items) == lru.Len() <= maxEntries; bytes == Σ len(text) <= maxBytes
type cache struct {
	maxEntries int
	maxBytes   int64
	bytes      int64
	lru        *list.List // of *cached, most recent at the front
	items      map[cacheKey]*list.Element
}

type cacheKey struct{ app, fingerprint string }

type cached struct {
	key  cacheKey
	text string
}

func newCache(maxEntries int, maxBytes int64) *cache {
	return &cache{maxEntries: maxEntries, maxBytes: maxBytes, lru: list.New(), items: map[cacheKey]*list.Element{}}
}

func (c *cache) get(k cacheKey) (string, bool) {
	e, ok := c.items[k]
	if !ok {
		return "", false
	}
	c.lru.MoveToFront(e)
	return e.Value.(*cached).text, true
}

// put adds k (or refreshes it), evicting the least recently used entries until both bounds hold. A text larger
// than maxBytes is not cached.
func (c *cache) put(k cacheKey, text string) {
	if int64(len(text)) > c.maxBytes {
		return
	}
	if e, ok := c.items[k]; ok {
		c.lru.MoveToFront(e)
		return
	}
	c.items[k] = c.lru.PushFront(&cached{key: k, text: text})
	c.bytes += int64(len(text))
	for c.lru.Len() > c.maxEntries || c.bytes > c.maxBytes {
		old := c.lru.Remove(c.lru.Back()).(*cached)
		delete(c.items, old.key)
		c.bytes -= int64(len(old.text))
	}
	c.invariant()
}

func (c *cache) len() int { return c.lru.Len() }

func (c *cache) invariant() {
	if !assert.Enabled {
		return
	}
	var sum int64
	for e := c.lru.Front(); e != nil; e = e.Next() {
		sum += int64(len(e.Value.(*cached).text))
	}
	assert.That(len(c.items) == c.lru.Len() && c.lru.Len() <= c.maxEntries && c.bytes == sum && c.bytes <= c.maxBytes,
		"schemareg cache: %d items, %d in lru (max %d), %d bytes counted, %d summed (max %d)",
		len(c.items), c.lru.Len(), c.maxEntries, c.bytes, sum, c.maxBytes)
}
