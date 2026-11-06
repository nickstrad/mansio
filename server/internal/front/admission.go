package front

import (
	"context"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
)

// Admission is the concurrent wrapper around the pure limiter: one mutex, and a channel per waiter.
type Admission struct {
	mu  sync.Mutex
	l   *limiter
	now func() time.Time

	// Guarded by mu: for Stats (Release It O1: in use, high water, times full) and the shed Retry-After.
	shed            int64
	inFlightHW, qHW int
	drained         drainRate
}

// NewAdmission returns admission with lim's caps (zero fields take defaults).
func NewAdmission(lim AdmissionLimits) *Admission { return newAdmission(lim, det.System()) }

func newAdmission(lim AdmissionLimits, clock det.Clock) *Admission {
	return &Admission{l: newLimiter(lim), now: clock.Now}
}

// Acquire takes a slot for (tenant, db), waiting in the queue until ctx ends if both are at their caps.
//
//	post: err == nil ⇒ release != nil; call it exactly once
//	post: errors.Is(err, ErrShed): the queues were full; errors.Is(err, ErrExpired): ctx ended while queued.
//	      Either way nothing is held.
func (a *Admission) Acquire(ctx context.Context, tenant, db string) (release func(), err error) {
	k := dbKey{tenant, db}
	now := a.now() // outside the lock: the request path takes it
	a.mu.Lock()
	d, w := a.l.acquire(k)
	if w != nil {
		w.ch = make(chan struct{})
	}
	var drain time.Duration
	if d == shed {
		a.shed++
		drain = queueDrain(a.l.vm.queued, a.drained.perSec(now))
	}
	a.inFlightHW, a.qHW = max(a.inFlightHW, a.l.vm.inFlight), max(a.qHW, a.l.vm.queued)
	a.mu.Unlock()
	switch d {
	case shed:
		return nil, WithRetryAfter(ErrShed, drain) // Retry-After: when the VM queue should have drained
	case run:
		return a.releaser(k), nil
	}
	select {
	case <-w.ch:
		return a.releaser(k), nil
	case <-ctx.Done():
	}
	a.mu.Lock()
	cancelled := a.l.cancel(w)
	a.mu.Unlock()
	if !cancelled { // granted while we gave up: hand the slot straight back
		a.releaser(k)()
	}
	return nil, ErrExpired
}

func (a *Admission) releaser(k dbKey) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			now := a.now()
			a.mu.Lock()
			granted := a.l.release(k)
			a.drained.done(now)
			a.inFlightHW = max(a.inFlightHW, a.l.vm.inFlight)
			a.mu.Unlock()
			for _, w := range granted {
				close(w.ch)
			}
		})
	}
}

// AdmissionStats are the current totals across tenants, and counters since NewAdmission.
type AdmissionStats struct {
	Tenants, DBs, InFlight, Queued     int
	InFlightHighWater, QueuedHighWater int
	Shed                               int64   // requests refused because a queue was full (times full)
	DrainPerSec                        float64 // completions in the last whole second (the shed Retry-After's rate)
}

// Idle reports whether nothing is held or queued.
func (s AdmissionStats) Idle() bool {
	return s.Tenants == 0 && s.DBs == 0 && s.InFlight == 0 && s.Queued == 0
}

// Stats returns the current totals (O(1)).
func (a *Admission) Stats() AdmissionStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AdmissionStats{Tenants: len(a.l.tenants), DBs: len(a.l.dbs), InFlight: a.l.vm.inFlight, Queued: a.l.vm.queued,
		InFlightHighWater: a.inFlightHW, QueuedHighWater: a.qHW, Shed: a.shed,
		DrainPerSec: a.drained.perSec(a.now())}
}

// drainRate counts completions per wall-clock second. The rate is the last whole second's count: an estimate
// of how fast the VM queue drains, recent enough to follow load and stable within a second.
type drainRate struct {
	sec       int64 // the current second (Unix)
	cur, prev int64 // completions in sec, and in sec-1
}

func (d *drainRate) roll(now time.Time) {
	switch sec := now.Unix(); {
	case sec == d.sec:
	case sec == d.sec+1:
		d.sec, d.prev, d.cur = sec, d.cur, 0
	default:
		d.sec, d.prev, d.cur = sec, 0, 0
	}
}

func (d *drainRate) done(now time.Time) {
	d.roll(now)
	d.cur++
}

// perSec is the completions in the last whole second before now; 0 when there were none (rate unknown).
func (d *drainRate) perSec(now time.Time) float64 {
	d.roll(now)
	return float64(d.prev)
}

// queueDrain estimates when a shed request could be admitted: the queue ahead of it, plus itself, at the
// measured rate (Release It R3: the server says when to come back). 0 = no estimate (the Spec floor applies).
//
//	post: perSec <= 0 ⇒ 0; otherwise (queued+1)/perSec seconds
func queueDrain(queued int, perSec float64) time.Duration {
	if perSec <= 0 {
		return 0
	}
	return time.Duration(float64(queued+1) / perSec * float64(time.Second))
}
