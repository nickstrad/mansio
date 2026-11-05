package vactor

import (
	"context"
	"slices"
	"sync"

	"github.com/nickstrad/ts-db-go/internal/assert"
)

// fetchGate is the fetch bulkhead (Release It B1, B2): at most maxN fetches and maxBytes bytes in flight, and
// at most maxQueue activations waiting for a slot. Waiters are served in arrival order, so a large fetch is
// not starved by a stream of small ones. A full queue sheds: acquire fails at once with ErrBusy.
//
// Invariant: n <= maxN; bytes <= maxBytes unless one fetch larger than maxBytes runs alone (n == 1);
// len(queue) <= maxQueue; a queued waiter exists only while the head does not fit.
type fetchGate struct {
	maxN     int
	maxBytes int64
	maxQueue int

	mu    sync.Mutex
	n     int
	bytes int64
	queue []*waiter
}

type waiter struct {
	bytes   int64
	ready   chan struct{} // closed when granted
	granted bool
}

func newFetchGate(maxN int, maxBytes int64, maxQueue int) *fetchGate {
	assert.That(maxN >= 1 && maxBytes >= 1 && maxQueue >= 0, "fetchGate(%d, %d, %d): limits not normalized", maxN, maxBytes, maxQueue)
	return &fetchGate{maxN: maxN, maxBytes: maxBytes, maxQueue: maxQueue}
}

// acquire takes a slot for a fetch of size bytes (0: unknown), waiting in line while the gate is full.
//
//	post: err == nil ⇒ the caller holds the slot and must call release exactly once
//	post: the queue is full ⇒ ErrBusy at once; ctx ends while waiting ⇒ ctx.Err(); either way nothing is held
func (g *fetchGate) acquire(ctx context.Context, bytes int64) (release func(), err error) {
	release = func() { g.release(bytes) }
	g.mu.Lock()
	if len(g.queue) == 0 && g.fits(bytes) {
		g.take(bytes)
		g.invariant()
		g.mu.Unlock()
		return release, nil
	}
	if len(g.queue) >= g.maxQueue {
		g.mu.Unlock()
		return nil, errShed
	}
	w := &waiter{bytes: bytes, ready: make(chan struct{})}
	g.queue = append(g.queue, w)
	g.mu.Unlock()

	select {
	case <-w.ready:
		return release, nil
	case <-ctx.Done():
	}
	g.mu.Lock()
	if w.granted { // granted while we gave up: hand the slot back
		g.mu.Unlock()
		release()
		return nil, ctx.Err()
	}
	g.queue = slices.DeleteFunc(g.queue, func(x *waiter) bool { return x == w })
	g.wake() // the head may have changed
	g.invariant()
	g.mu.Unlock()
	return nil, ctx.Err()
}

// shedding reports whether a new waiter would be refused now (GetAt's fail-fast check, guide F1).
func (g *fetchGate) shedding() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.queue) >= g.maxQueue && !(len(g.queue) == 0 && g.n < g.maxN)
}

// fits reports whether a fetch of size bytes may start now. pre: g.mu held.
func (g *fetchGate) fits(bytes int64) bool {
	return g.n < g.maxN && (g.n == 0 || g.bytes+bytes <= g.maxBytes)
}

func (g *fetchGate) take(bytes int64) { g.n++; g.bytes += bytes }

func (g *fetchGate) release(bytes int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n--
	g.bytes -= bytes
	g.wake()
	g.invariant()
}

// wake grants slots to waiters at the head of the line while they fit. pre: g.mu held.
func (g *fetchGate) wake() {
	for len(g.queue) > 0 && g.fits(g.queue[0].bytes) {
		w := g.queue[0]
		g.queue = g.queue[1:]
		g.take(w.bytes)
		w.granted = true
		close(w.ready)
	}
}

func (g *fetchGate) invariant() {
	if !assert.Enabled || g.n >= 0 && g.n <= g.maxN && g.bytes >= 0 && (g.bytes <= g.maxBytes || g.n == 1) &&
		len(g.queue) <= g.maxQueue && (len(g.queue) == 0 || !g.fits(g.queue[0].bytes)) {
		return // checked without boxing the arguments below (this runs on every acquire and release)
	}
	assert.That(g.n >= 0 && g.n <= g.maxN && g.bytes >= 0, "fetchGate: n=%d bytes=%d (max %d)", g.n, g.bytes, g.maxN)
	assert.That(g.bytes <= g.maxBytes || g.n == 1, "fetchGate: %d bytes in flight over %d with %d fetches", g.bytes, g.maxBytes, g.n)
	assert.That(len(g.queue) <= g.maxQueue, "fetchGate: queue %d over %d", len(g.queue), g.maxQueue)
	assert.That(len(g.queue) == 0 || !g.fits(g.queue[0].bytes), "fetchGate: head of the queue fits but waits")
}

// inFlight reports the fetches and bytes in flight and the waiters (tests, and RI-11's gauges).
func (g *fetchGate) inFlight() (n int, bytes int64, queued int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n, g.bytes, len(g.queue)
}
