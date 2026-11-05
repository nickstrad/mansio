package edge

import (
	"strconv"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/ratelimit"
)

// The ticket filter's per-request costs: a cache hit, a full verify (miss), garbage refused at parse, and a
// limiter charge. Numbers: knowledge/front/2026-11-05-edge.md.
func BenchmarkTicketFilter(b *testing.B) {
	is := newIssuer(b)
	tok := is.mint(b, "vm-a", "db-1", t0, time.Minute)
	b.Run("hit", func(b *testing.B) {
		g := NewGate(is.ring, 0, 0, 1000)
		g.Verify(tok, t0)
		for b.Loop() {
			g.Lookup(tok, t0)
		}
	})
	b.Run("verify", func(b *testing.B) {
		g := NewGate(is.ring, 0, 0, 0)
		for b.Loop() {
			g.Verify(tok, t0)
		}
	})
	b.Run("forged", func(b *testing.B) {
		forged := newIssuer(b).mint(b, "vm-a", "db-1", t0, time.Minute)
		g := NewGate(is.ring, 0, 0, 0)
		for b.Loop() {
			g.Verify(forged, t0)
		}
	})
	b.Run("garbage", func(b *testing.B) {
		g := NewGate(is.ring, 0, 0, 0)
		for b.Loop() {
			g.Verify("garbage-garbage-garbage", t0)
		}
	})
	b.Run("limiter", func(b *testing.B) {
		l := ratelimit.NewSourceLimiter(1e9, 1e9, 1<<20)
		srcs := make([]string, 1024)
		for i := range srcs {
			srcs[i] = "198.51.100." + strconv.Itoa(i%256) + strconv.Itoa(i)
		}
		i := 0
		for b.Loop() {
			l.Allow(srcs[i%len(srcs)], t0)
			i++
		}
	})
}
