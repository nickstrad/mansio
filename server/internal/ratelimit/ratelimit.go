// Package ratelimit holds the per-source token bucket that the edge (tickets it must verify) and controld (API key
// ids it must look up) charge before costly work for an unauthenticated caller. Domain code: no I/O.
package ratelimit

import (
	"net"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
)

// SourceLimiter is a token bucket per request source, charged once for each costly check an unauthenticated
// request needs: the edge charges a ticket it has to verify (a verified-ticket cache miss), controld an API key
// id it has to read from Postgres (not in its key cache). A client that reuses its ticket or key pays once per
// lifetime; a source that sends garbage runs dry after Burst and is refused before the work, while other sources
// and cached credentials from the same source go on (Release It B1: shed, never queue).
//
// Sources are bounded: with MaxSources buckets held, buckets that are full again (idle long enough) are swept,
// at most once per second; if none is, new sources share one overflow bucket. So many-source floods are cut to
// one bucket's rate, and memory stays bounded.
//
//	Invariant: len(buckets) <= max; every bucket's tokens is in [0, burst].
type SourceLimiter struct {
	rate, burst float64
	max         int

	mu        sync.Mutex
	buckets   map[string]*bucket
	overflow  bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewSourceLimiter returns a limiter refilling rate tokens per second up to burst, per source.
//
//	pre: rate > 0, burst >= 1, maxSources >= 1 (validated by the caller's config)
func NewSourceLimiter(rate, burst float64, maxSources int) *SourceLimiter {
	assert.That(rate > 0 && burst >= 1 && maxSources >= 1, "ratelimit: rate %v burst %v max %d", rate, burst, maxSources)
	return &SourceLimiter{rate: rate, burst: burst, max: maxSources, buckets: map[string]*bucket{},
		overflow: bucket{tokens: burst}}
}

// Allow takes one token from src's bucket at now. If the bucket is empty, wait is how long until it holds one.
//
//	post: ok ⇒ wait == 0; !ok ⇒ wait > 0
func (l *SourceLimiter) Allow(src string, now time.Time) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[src]
	if b == nil {
		if len(l.buckets) >= l.max && now.Sub(l.lastSweep) >= time.Second {
			l.sweep(now)
		}
		if len(l.buckets) < l.max {
			b = &bucket{tokens: l.burst, last: now}
			l.buckets[src] = b
		} else {
			b = &l.overflow
		}
	}
	b.refill(now, l.rate, l.burst)
	if b.tokens >= 1 {
		b.tokens--
	} else {
		wait = max(time.Duration((1-b.tokens)/l.rate*float64(time.Second)), time.Millisecond)
	}
	assert.That(b.tokens >= 0 && b.tokens <= l.burst && len(l.buckets) <= l.max, "ratelimit: invariant broken") // no args: no allocation
	return wait == 0, wait
}

func (b *bucket) refill(now time.Time, rate, burst float64) {
	if dt := now.Sub(b.last); dt > 0 {
		b.tokens = min(burst, b.tokens+dt.Seconds()*rate)
		b.last = now
	}
}

// sweep drops the buckets that would be full at now: forgetting them changes nothing.
func (l *SourceLimiter) sweep(now time.Time) {
	l.lastSweep = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// Sources is the number of per-source buckets held (metrics).
func (l *SourceLimiter) Sources() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// SourceKey is the limiter key for a remote address (host:port or host): the IP, with IPv6 cut to its /64 (one
// host usually holds a whole /64, so per-address buckets would be free to multiply).
func SourceKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
