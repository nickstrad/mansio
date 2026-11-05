package edge

import (
	"crypto/ed25519"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// Gate verifies tickets for the edge. Unlike an agent, the edge does not know the audience in advance: the
// ticket names it. So the gate checks everything that does not need the agent's state (framing, the signature,
// the claims, the time window, the lifetime cap) for whichever VM the ticket names; the agent then checks the
// audience against itself, the database and the permission against the request, and the epoch against its
// owner table.
//
// Verified tickets are remembered (up to Max, an arbitrary one evicted when full), so a client reusing its ticket
// pays the Ed25519 check once; Lookup tells a hit from a miss so the caller can charge only misses to the
// source's rate limit. A hit is used only while Keys still maps its kid to the same key: removing a key from the
// ring revokes its cached tickets at once (as ticket.Cache does).
//
//	Invariant: len(entries) <= max; every entry verified under entry.pub.
type Gate struct {
	keys   ticket.KeySet
	skew   time.Duration
	maxTTL time.Duration
	max    int

	mu      sync.RWMutex
	entries map[string]verified
}

type verified struct {
	c   ticket.Claims
	pub ed25519.PublicKey
}

// NewGate returns a gate over keys. skew is the tolerated clock difference with the issuer (>= 0), maxTTL the
// longest lifetime accepted (0 = no cap), entries the verified tickets remembered (0 = none).
func NewGate(keys ticket.KeySet, skew, maxTTL time.Duration, entries int) *Gate {
	return &Gate{keys: keys, skew: max(skew, 0), maxTTL: maxTTL, max: entries, entries: map[string]verified{}}
}

// Lookup checks token against the verified tickets. hit reports that it verified before under a key still in
// the ring; then err is ticket.ErrExpired or ErrNotYetValid if it is out of its time window now, else nil.
//
//	post: !hit ⇒ err == nil (the caller must Verify)
func (g *Gate) Lookup(token string, now time.Time) (c ticket.Claims, hit bool, err error) {
	g.mu.RLock()
	e, ok := g.entries[token]
	g.mu.RUnlock()
	if !ok {
		return ticket.Claims{}, false, nil
	}
	if pub, live := g.keys.Key(e.c.KID); !live || !pub.Equal(e.pub) {
		g.forget(token)
		return ticket.Claims{}, false, nil
	}
	switch {
	case !now.Before(time.Unix(e.c.EXP, 0).Add(g.skew)):
		g.forget(token) // it will never be valid again
		return ticket.Claims{}, true, ticket.ErrExpired
	case now.Before(time.Unix(e.c.IAT, 0).Add(-g.skew)):
		return ticket.Claims{}, true, ticket.ErrNotYetValid
	}
	return e.c, true, nil
}

// Verify fully checks token for the VM it names and remembers it if valid.
//
//	post: err == nil ⇒ the signature is valid under Keys.Key(c.KID), c.Validate() == nil,
//	      iat - skew <= now < exp + skew, exp - iat <= maxTTL (if set)
//	post: err != nil ⇒ ticket.Invalid(err)
func (g *Gate) Verify(token string, now time.Time) (ticket.Claims, error) {
	// Parse reads the claims from unverified bytes, only to name the audience and the database; Verify then
	// checks the signature over exactly those bytes, so nothing unverified is returned.
	claimed, err := ticket.Parse(token)
	if err != nil {
		return ticket.Claims{}, err
	}
	v := ticket.Verifier{Keys: g.keys, VM: claimed.VM, Skew: g.skew, MaxTTL: g.maxTTL}
	c, err := v.Verify(token, now, ticket.Want{DB: claimed.DBID, Perm: claimed.Perms})
	if err != nil {
		return ticket.Claims{}, err
	}
	if g.max > 0 {
		pub, _ := g.keys.Key(c.KID)
		g.mu.Lock()
		if _, ok := g.entries[token]; !ok && len(g.entries) >= g.max {
			for t := range g.entries {
				delete(g.entries, t)
				break
			}
		}
		g.entries[token] = verified{c, pub}
		g.mu.Unlock()
	}
	return c, nil
}

func (g *Gate) forget(token string) {
	g.mu.Lock()
	delete(g.entries, token)
	g.mu.Unlock()
}

// Len is the number of verified tickets remembered (metrics).
func (g *Gate) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.entries)
}
