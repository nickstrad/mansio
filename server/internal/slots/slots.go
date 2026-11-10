// Package slots is a pool of call slots that never queues (Release It B1, B2): a caller that finds none free
// is refused at once and counted. Its limit can change while it is in use, so an operator can narrow the
// load on a struggling dependency without a restart (Release It C4; Backpressure in Microservices ch. 4
// "Incident Actions": the levers must exist before the incident).
//
// Error module: none. A zero or negative limit is a bug and panics.
package slots

import (
	"sync/atomic"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/metrics"
)

// Pool is the slots. Safe for concurrent use; the request path takes no lock.
//
//	Invariant: InUse() <= the largest limit in force while its holders acquired
type Pool struct {
	limit, inUse, shed atomic.Int64
}

// New returns a pool of limit slots.
//
//	pre: limit >= 1
func New(limit int) *Pool {
	assert.That(limit >= 1, "slots.New(%d): limit not validated", limit)
	p := &Pool{}
	p.limit.Store(int64(limit))
	return p
}

// TryAcquire takes a slot, or counts the refusal and reports false.
//
//	post: true ⇒ the caller calls Release exactly once
func (p *Pool) TryAcquire() bool {
	if p.inUse.Add(1) > p.limit.Load() {
		p.inUse.Add(-1)
		p.shed.Add(1)
		return false
	}
	return true
}

// Release returns a slot.
func (p *Pool) Release() {
	n := p.inUse.Add(-1)
	assert.That(n >= 0, "slots: Release without a slot")
}

// SetLimit changes the limit and returns the previous one. Lowering it lets the holders finish and admits
// nobody until fewer than n are in use; raising it applies to the next caller.
//
//	pre: n >= 1
func (p *Pool) SetLimit(n int) (prev int) {
	assert.That(n >= 1, "slots.SetLimit(%d): limit not validated", n)
	return int(p.limit.Swap(int64(n)))
}

func (p *Pool) Limit() int  { return int(p.limit.Load()) }
func (p *Pool) InUse() int  { return int(p.inUse.Load()) }
func (p *Pool) Shed() int64 { return p.shed.Load() }

// Stat is the pool for metrics.Queues, as queue.
func (p *Pool) Stat(queue string) metrics.QueueStat {
	return metrics.QueueStat{Queue: queue, Capacity: p.Limit(), Depth: p.InUse(), Rejected: map[string]int64{metrics.ReasonFull: p.Shed()}}
}
