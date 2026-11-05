package front

import (
	"container/list"
	"fmt"

	"github.com/nickstrad/ts-db-go/internal/assert"
)

// AdmissionLimits are the caps per tenant and per database (option 4 §3.2: "subject to mailbox, tenant and VM
// caps"). A request runs when both its tenant and its database are under their in-flight caps; otherwise it
// waits if both queues have room; otherwise it is shed. The actor's own mailbox is a further cap behind these.
type AdmissionLimits struct {
	TenantInFlight int `json:"tenant_in_flight"` // default 256
	TenantQueue    int `json:"tenant_queue"`     // default 1024
	DBInFlight     int `json:"db_in_flight"`     // default 64
	DBQueue        int `json:"db_queue"`         // default 256
}

// WithDefaults fills zero (or negative) caps.
func (l AdmissionLimits) WithDefaults() AdmissionLimits {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&l.TenantInFlight, 256)
	def(&l.TenantQueue, 1024)
	def(&l.DBInFlight, 64)
	def(&l.DBQueue, 256)
	return l
}

// decision is what admission does with a new request.
type decision uint8

const (
	run  decision = iota // a slot was taken
	wait                 // queued; the waiter is granted later or cancelled
	shed                 // over cap: refused, nothing taken
)

// dbKey names a database within its tenant, so a database's counts always belong to one tenant.
type dbKey struct{ tenant, db string }

type counts struct{ inFlight, queued int }

type tenantState struct {
	counts
	waiters list.List // of *waiter, arrival order
}

// waiter is a queued request.
type waiter struct {
	key     dbKey
	elem    *list.Element // in its tenant's list; nil once granted or cancelled
	granted bool
	ch      chan struct{} // closed when granted (set by the concurrent wrapper)
}

// limiter is the pure admission state: no locks, no clock, no channels touched. Admission wraps it.
//
//	Invariant (per tenant t, per database d of t):
//	  t.inFlight <= TenantInFlight, t.queued <= TenantQueue, d.inFlight <= DBInFlight, d.queued <= DBQueue
//	  t.inFlight = Σ d.inFlight, t.queued = Σ d.queued = len(t.waiters)
//	  work-conserving: no waiter could run now (its tenant or its database is at its in-flight cap)
//	  states with all counts zero are deleted (memory is bounded by active tenants and databases)
type limiter struct {
	lim     AdmissionLimits
	tenants map[string]*tenantState
	dbs     map[dbKey]*counts
}

func newLimiter(l AdmissionLimits) *limiter {
	return &limiter{lim: l.WithDefaults(), tenants: map[string]*tenantState{}, dbs: map[dbKey]*counts{}}
}

func (l *limiter) state(k dbKey) (*tenantState, *counts) {
	t := l.tenants[k.tenant]
	if t == nil {
		t = &tenantState{}
		l.tenants[k.tenant] = t
	}
	d := l.dbs[k]
	if d == nil {
		d = &counts{}
		l.dbs[k] = d
	}
	return t, d
}

func (l *limiter) runnable(t *tenantState, d *counts) bool {
	return t.inFlight < l.lim.TenantInFlight && d.inFlight < l.lim.DBInFlight
}

// acquire admits one request for k.
//
//	post: run ⇒ a slot is held (release it once); wait ⇒ w is queued; shed ⇒ nothing changed
//	post: shed ⇔ the request cannot run now and its tenant or database queue is full
func (l *limiter) acquire(k dbKey) (decision, *waiter) {
	t, d := l.state(k)
	switch {
	case l.runnable(t, d):
		t.inFlight++
		d.inFlight++
		l.checkTenant(k.tenant)
		return run, nil
	case t.queued < l.lim.TenantQueue && d.queued < l.lim.DBQueue:
		w := &waiter{key: k}
		w.elem = t.waiters.PushBack(w)
		t.queued++
		d.queued++
		l.checkTenant(k.tenant)
		return wait, w
	}
	l.gc(k)
	return shed, nil
}

// release frees k's slot and grants waiters (oldest first) that can now run. Each granted waiter holds a slot.
//
//	pre: a slot for k is held
func (l *limiter) release(k dbKey) []*waiter {
	t, d := l.tenants[k.tenant], l.dbs[k]
	assert.That(t != nil && d != nil && d.inFlight > 0, "limiter: release of %v without a slot", k)
	t.inFlight--
	d.inFlight--
	var granted []*waiter
	for e := t.waiters.Front(); e != nil && t.inFlight < l.lim.TenantInFlight; {
		next := e.Next()
		w := e.Value.(*waiter)
		if wd := l.dbs[w.key]; wd.inFlight < l.lim.DBInFlight {
			t.waiters.Remove(e)
			w.elem, w.granted = nil, true
			t.queued--
			wd.queued--
			t.inFlight++
			wd.inFlight++
			granted = append(granted, w)
		}
		e = next
	}
	l.checkTenant(k.tenant)
	l.gc(k)
	return granted
}

// cancel removes a waiter that gave up. It returns false if w was already granted (the caller then holds a
// slot and must release it).
func (l *limiter) cancel(w *waiter) bool {
	if w.granted {
		return false
	}
	assert.That(w.elem != nil, "limiter: cancel of a waiter that is neither queued nor granted")
	t, d := l.tenants[w.key.tenant], l.dbs[w.key]
	t.waiters.Remove(w.elem)
	w.elem = nil
	t.queued--
	d.queued--
	l.checkTenant(w.key.tenant)
	l.gc(w.key)
	return true
}

// gc deletes empty states.
func (l *limiter) gc(k dbKey) {
	if d := l.dbs[k]; d != nil && *d == (counts{}) {
		delete(l.dbs, k)
	}
	if t := l.tenants[k.tenant]; t != nil && t.counts == (counts{}) {
		delete(l.tenants, k.tenant)
	}
}

// checkTenant asserts the invariant for one tenant (O(its waiters); only with assertions enabled).
func (l *limiter) checkTenant(tenant string) {
	if assert.Enabled {
		err := l.tenantProblem(tenant)
		assert.That(err == nil, "limiter: %v", err)
	}
}

// tenantProblem returns the first broken invariant for tenant, or nil. Callers outside assertions use it in
// tests and the model fuzz.
func (l *limiter) tenantProblem(tenant string) error {
	t := l.tenants[tenant]
	if t == nil {
		return nil
	}
	if t.inFlight > l.lim.TenantInFlight || t.queued > l.lim.TenantQueue || t.inFlight < 0 || t.queued < 0 {
		return fmt.Errorf("tenant %q counts %+v over caps %+v", tenant, t.counts, l.lim)
	}
	if t.waiters.Len() != t.queued {
		return fmt.Errorf("tenant %q: %d waiters, queued %d", tenant, t.waiters.Len(), t.queued)
	}
	for e := t.waiters.Front(); e != nil; e = e.Next() {
		w := e.Value.(*waiter)
		d := l.dbs[w.key]
		if d == nil || d.queued < 1 || d.inFlight > l.lim.DBInFlight || d.queued > l.lim.DBQueue {
			return fmt.Errorf("database %v counts %+v inconsistent with a waiter", w.key, d)
		}
		if l.runnable(t, d) {
			return fmt.Errorf("waiter for %v could run but waits (not work-conserving)", w.key)
		}
	}
	return nil
}
