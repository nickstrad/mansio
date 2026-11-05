package front

import (
	"context"
	"sync"
)

// Admission is the concurrent wrapper around the pure limiter: one mutex, and a channel per waiter.
type Admission struct {
	mu sync.Mutex
	l  *limiter
}

// NewAdmission returns admission with lim's caps (zero fields take defaults).
func NewAdmission(lim AdmissionLimits) *Admission { return &Admission{l: newLimiter(lim)} }

// Acquire takes a slot for (tenant, db), waiting in the queue until ctx ends if both are at their caps.
//
//	post: err == nil ⇒ release != nil; call it exactly once
//	post: errors.Is(err, ErrShed): the queues were full; errors.Is(err, ErrExpired): ctx ended while queued.
//	      Either way nothing is held.
func (a *Admission) Acquire(ctx context.Context, tenant, db string) (release func(), err error) {
	k := dbKey{tenant, db}
	a.mu.Lock()
	d, w := a.l.acquire(k)
	if w != nil {
		w.ch = make(chan struct{})
	}
	a.mu.Unlock()
	switch d {
	case shed:
		return nil, ErrShed
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
			a.mu.Lock()
			granted := a.l.release(k)
			a.mu.Unlock()
			for _, w := range granted {
				close(w.ch)
			}
		})
	}
}

// AdmissionStats are the current totals across tenants.
type AdmissionStats struct{ Tenants, DBs, InFlight, Queued int }

// Stats scans the state once.
func (a *Admission) Stats() AdmissionStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := AdmissionStats{Tenants: len(a.l.tenants), DBs: len(a.l.dbs)}
	for _, t := range a.l.tenants {
		s.InFlight += t.inFlight
		s.Queued += t.queued
	}
	return s
}
