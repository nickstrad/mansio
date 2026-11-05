package front

import (
	"container/list"
	"fmt"
	"runtime"

	"github.com/nickstrad/ts-db-go/internal/assert"
)

// AdmissionLimits are the caps per database, per tenant and per VM (option 4 §3.2: "subject to mailbox, tenant
// and VM caps"; Release It B3: caps nest db ≤ tenant ≤ VM). A request runs when its database, its tenant and the
// VM are all under their in-flight caps; otherwise it waits if all three queues have room; otherwise it is shed.
// The actor's own mailbox is a further cap behind these. Without the VM cap the process's in-flight count would
// be tenants × TenantInFlight, which is unbounded.
type AdmissionLimits struct {
	TenantInFlight int `json:"tenant_in_flight"` // default 256
	TenantQueue    int `json:"tenant_queue"`     // default 1024
	DBInFlight     int `json:"db_in_flight"`     // default 64
	DBQueue        int `json:"db_queue"`         // default 256
	VMInFlight     int `json:"vm_in_flight"`     // default 4 × GOMAXPROCS × DBInFlight
	VMQueue        int `json:"vm_queue"`         // default 4 × VMInFlight
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
	def(&l.VMInFlight, 4*runtime.GOMAXPROCS(0)*l.DBInFlight)
	def(&l.VMQueue, 4*l.VMInFlight)
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
	vmElem  *list.Element // in the VM-wide arrival list; nil once granted or cancelled
	granted bool
	ch      chan struct{} // closed when granted (set by the concurrent wrapper)
}

// limiter is the pure admission state: no locks, no clock, no channels touched. Admission wraps it.
//
//	Invariant (per tenant t, per database d of t, and the VM v):
//	  t.inFlight <= TenantInFlight, t.queued <= TenantQueue, d.inFlight <= DBInFlight, d.queued <= DBQueue
//	  t.inFlight = Σ d.inFlight, t.queued = Σ d.queued = len(t.waiters)
//	  v.inFlight = Σ t.inFlight <= VMInFlight, v.queued = Σ t.queued = len(l.waiters) <= VMQueue
//	  work-conserving: no waiter could run now (its database, its tenant or the VM is at its in-flight cap)
//	  states with all counts zero are deleted (memory is bounded by active tenants and databases)
type limiter struct {
	lim     AdmissionLimits
	vm      counts
	waiters list.List // of *waiter, VM-wide arrival order
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
	return l.vm.inFlight < l.lim.VMInFlight && t.inFlight < l.lim.TenantInFlight && d.inFlight < l.lim.DBInFlight
}

// acquire admits one request for k.
//
//	post: run ⇒ a slot is held (release it once); wait ⇒ w is queued; shed ⇒ nothing changed
//	post: shed ⇔ the request cannot run now and its database, tenant or VM queue is full
func (l *limiter) acquire(k dbKey) (decision, *waiter) {
	t, d := l.state(k)
	switch {
	case l.runnable(t, d):
		l.take(t, d)
		l.checkTenant(k.tenant)
		return run, nil
	case t.queued < l.lim.TenantQueue && d.queued < l.lim.DBQueue && l.vm.queued < l.lim.VMQueue:
		w := &waiter{key: k}
		w.elem = t.waiters.PushBack(w)
		w.vmElem = l.waiters.PushBack(w)
		t.queued++
		d.queued++
		l.vm.queued++
		l.checkTenant(k.tenant)
		return wait, w
	}
	l.gc(k)
	return shed, nil
}

func (l *limiter) take(t *tenantState, d *counts) {
	t.inFlight++
	d.inFlight++
	l.vm.inFlight++
}

// grant moves a queued waiter to running.
func (l *limiter) grant(w *waiter) {
	t, d := l.tenants[w.key.tenant], l.dbs[w.key]
	t.waiters.Remove(w.elem)
	l.waiters.Remove(w.vmElem)
	w.elem, w.vmElem, w.granted = nil, nil, true
	t.queued--
	d.queued--
	l.vm.queued--
	l.take(t, d)
}

// release frees k's slot and grants waiters that can now run: first k's tenant's (oldest first), then, if the
// VM was at its cap, other tenants' waiters in VM-wide arrival order (they may have waited only for the VM).
// Each granted waiter holds a slot.
//
//	pre: a slot for k is held
func (l *limiter) release(k dbKey) []*waiter {
	t, d := l.tenants[k.tenant], l.dbs[k]
	assert.That(t != nil && d != nil && d.inFlight > 0 && l.vm.inFlight > 0, "limiter: release of %v without a slot", k)
	vmWasFull := l.vm.inFlight >= l.lim.VMInFlight
	t.inFlight--
	d.inFlight--
	l.vm.inFlight--
	var granted []*waiter
	for e := t.waiters.Front(); e != nil && t.inFlight < l.lim.TenantInFlight && l.vm.inFlight < l.lim.VMInFlight; {
		next := e.Next()
		if w := e.Value.(*waiter); l.dbs[w.key].inFlight < l.lim.DBInFlight {
			l.grant(w)
			granted = append(granted, w)
		}
		e = next
	}
	// While the VM was not full, every waiter was blocked by its own tenant or database (work-conserving), and
	// this release changed only k's tenant: no other tenant can have become runnable, so skip the O(waiters) scan.
	for e := l.waiters.Front(); vmWasFull && e != nil && l.vm.inFlight < l.lim.VMInFlight; {
		next := e.Next()
		if w := e.Value.(*waiter); w.key.tenant != k.tenant && l.runnable(l.tenants[w.key.tenant], l.dbs[w.key]) {
			l.grant(w)
			granted = append(granted, w)
			l.checkTenant(w.key.tenant)
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
	l.waiters.Remove(w.vmElem)
	w.elem, w.vmElem = nil, nil
	t.queued--
	d.queued--
	l.vm.queued--
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

// tenantProblem returns the first broken invariant for tenant (plus the O(1) VM bounds), or nil. Callers outside
// assertions use it in tests and the model fuzz; vmProblem checks the VM sums.
func (l *limiter) tenantProblem(tenant string) error {
	if l.vm.inFlight > l.lim.VMInFlight || l.vm.queued > l.lim.VMQueue || l.vm.inFlight < 0 || l.vm.queued < 0 ||
		l.vm.queued != l.waiters.Len() {
		return fmt.Errorf("VM counts %+v (%d waiters) over caps %+v", l.vm, l.waiters.Len(), l.lim)
	}
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

// vmProblem checks the VM counts against the sums over tenants (O(tenants); tests and the model fuzz).
func (l *limiter) vmProblem() error {
	var sum counts
	for _, t := range l.tenants {
		sum.inFlight += t.inFlight
		sum.queued += t.queued
	}
	if sum != l.vm {
		return fmt.Errorf("VM counts %+v, tenants sum to %+v", l.vm, sum)
	}
	return l.tenantProblem("")
}
