package ticket

import (
	"crypto/ed25519"
	"sync"
	"time"
)

// Cache remembers tickets whose signature verified, so a client reusing its ticket for its lifetime pays the
// Ed25519 check (~25 µs) once instead of on every request. A hit skips only the signature: every other rule
// (validity, time, audience, database, permission) runs again on each call, and the hit is used only while
// Keys still maps the ticket's kid to the same public key, so removing a key from the ring revokes its cached
// tickets at once.
//
//	Invariant: len(entries) <= Max; every entry's token verified under entry.pub.
type Cache struct {
	V   Verifier
	Max int // entries kept; when full, an arbitrary entry is evicted. <= 0 disables the cache

	mu      sync.RWMutex
	entries map[string]cached
}

type cached struct {
	c   Claims
	pub ed25519.PublicKey
}

// NewCache returns a cache of up to max verified tickets in front of v.
func NewCache(v Verifier, max int) *Cache {
	return &Cache{V: v, Max: max, entries: map[string]cached{}}
}

// Verify is Verifier.Verify with the signature check memoized. Same postconditions.
func (k *Cache) Verify(token string, now time.Time, want Want) (Claims, error) {
	if k.Max <= 0 {
		return k.V.Verify(token, now, want)
	}
	k.mu.RLock()
	e, ok := k.entries[token]
	k.mu.RUnlock()
	if ok {
		if pub, live := k.V.Keys.Key(e.c.KID); live && pub.Equal(e.pub) {
			if err := k.V.check(e.c, now, want); err != nil {
				if err == ErrExpired {
					k.forget(token)
				}
				return Claims{}, err
			}
			return e.c, nil
		}
		k.forget(token)
	}
	c, err := k.V.Verify(token, now, want)
	if err != nil {
		return Claims{}, err
	}
	pub, _ := k.V.Keys.Key(c.KID)
	k.mu.Lock()
	if len(k.entries) >= k.Max {
		for t := range k.entries {
			delete(k.entries, t)
			break
		}
	}
	k.entries[token] = cached{c, pub}
	k.mu.Unlock()
	return c, nil
}

func (k *Cache) forget(token string) {
	k.mu.Lock()
	delete(k.entries, token)
	k.mu.Unlock()
}

// Len is the number of cached tickets.
func (k *Cache) Len() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.entries)
}
