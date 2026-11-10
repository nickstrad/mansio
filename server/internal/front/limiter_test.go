package front

import (
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
)

var small = AdmissionLimits{TenantInFlight: 2, TenantQueue: 2, DBInFlight: 1, DBQueue: 1}

// On and off points of each cap.
func TestLimiterCaps(t *testing.T) {
	a, b, c := dbKey{"t1", "a"}, dbKey{"t1", "b"}, dbKey{"t1", "c"}
	l := newLimiter(small)
	steps := []struct {
		key  dbKey
		want decision
		why  string
	}{
		{a, run, "a: db and tenant under caps"},
		{a, wait, "a: db at in-flight cap (on), queue empty"},
		{a, shed, "a: db queue full (off point of DBQueue)"},
		{b, run, "b: tenant reaches its in-flight cap (on)"},
		{c, wait, "c: tenant at cap; tenant queue 1 of 2"},
		{c, shed, "c: db queue full"},
		{dbKey{"t1", "d"}, shed, "d: tenant queue full (off point of TenantQueue)"},
		{dbKey{"t2", "a"}, run, "another tenant with the same db name is independent"},
	}
	for _, s := range steps {
		if d, _ := l.acquire(s.key); d != s.want {
			t.Fatalf("%s: got %d, want %d", s.why, d, s.want)
		}
	}
	// Releasing b frees a tenant slot: a's waiter (oldest) cannot run (db a full), c's can.
	granted := l.release(b)
	if len(granted) != 1 || granted[0].key != c {
		t.Fatalf("granted %v, want c's waiter", granted)
	}
	// Releasing a: a's waiter runs.
	if granted = l.release(a); len(granted) != 1 || granted[0].key != a {
		t.Fatalf("granted %v, want a's waiter", granted)
	}
	l.release(a)
	l.release(c)
	l.release(dbKey{"t2", "a"})
	if len(l.tenants) != 0 || len(l.dbs) != 0 {
		t.Fatalf("state left after everything released: %v %v", l.tenants, l.dbs)
	}
}

// On and off points of the VM caps, and a VM slot freed by one tenant going to another tenant's waiter.
func TestLimiterVMCap(t *testing.T) {
	lim := AdmissionLimits{TenantInFlight: 2, TenantQueue: 2, DBInFlight: 2, DBQueue: 2, VMInFlight: 2, VMQueue: 1}
	l := newLimiter(lim)
	t1, t2, t3, t3b := dbKey{"t1", "a"}, dbKey{"t2", "a"}, dbKey{"t3", "a"}, dbKey{"t3", "b"}
	steps := []struct {
		key  dbKey
		want decision
		why  string
	}{
		{t1, run, "t1: VM 1 of 2"},
		{t2, run, "t2: VM reaches its in-flight cap (on point)"},
		{t3, wait, "t3: VM full though t3 and its db are idle; VM queue 1 of 1 (on point)"},
		{t3b, shed, "t3/b: VM queue full (off point of VMQueue)"},
	}
	for _, s := range steps {
		if d, _ := l.acquire(s.key); d != s.want {
			t.Fatalf("%s: got %d, want %d", s.why, d, s.want)
		}
		if err := l.vmProblem(); err != nil {
			t.Fatalf("%s: %v", s.why, err)
		}
	}
	if g := l.release(t1); len(g) != 1 || g[0].key != t3 {
		t.Fatalf("release by t1 granted %v, want t3's waiter (it waited only for the VM)", g)
	}
	if l.vm != (counts{inFlight: 2}) {
		t.Fatalf("VM counts %+v", l.vm)
	}
	if g := l.release(t2); len(g) != 0 {
		t.Fatalf("granted %v with no waiters", g)
	}
	if d, _ := l.acquire(t3b); d != run {
		t.Fatal("VM back under its cap (off point of VMInFlight): want run")
	}
	l.release(t3)
	l.release(t3b)
	if l.vm != (counts{}) || len(l.tenants) != 0 || l.vmProblem() != nil {
		t.Fatalf("state left: vm %+v tenants %v", l.vm, l.tenants)
	}
}

func TestAdmissionLimitsDefaultsNest(t *testing.T) {
	d := AdmissionLimits{}.WithDefaults()
	if d.DBInFlight > d.TenantInFlight || d.TenantInFlight > d.VMInFlight || d.VMQueue != 4*d.VMInFlight {
		t.Fatalf("defaults do not nest db <= tenant <= VM: %+v", d)
	}
	if got := (AdmissionLimits{DBInFlight: 3}).WithDefaults(); got.VMInFlight%3 != 0 {
		t.Fatalf("VM default not a multiple of db_in_flight: %+v", got)
	}
}

func TestLimiterCancel(t *testing.T) {
	l := newLimiter(small)
	a := dbKey{"t", "a"}
	l.acquire(a)
	_, w := l.acquire(a)
	if !l.cancel(w) {
		t.Fatal("cancel of a queued waiter failed")
	}
	_, w = l.acquire(a)
	if g := l.release(a); len(g) != 1 || g[0] != w {
		t.Fatalf("granted %v", g)
	}
	if l.cancel(w) {
		t.Fatal("cancel of a granted waiter succeeded; the caller would leak its slot")
	}
	l.release(a)
	if len(l.dbs) != 0 {
		t.Fatal("state left")
	}
}

// Each assertion in the limiter can fire.
func TestLimiterAssertionsFire(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out")
	}
	expectPanic := func(name, want string, fn func()) {
		t.Helper()
		defer func() {
			p, _ := recover().(string)
			if !strings.Contains(p, want) {
				t.Errorf("%s: panic %q, want it to mention %q", name, p, want)
			}
		}()
		fn()
	}
	expectPanic("release without slot", "without a slot", func() { newLimiter(small).release(dbKey{"t", "a"}) })
	expectPanic("cancel of a dead waiter", "neither queued", func() { newLimiter(small).cancel(&waiter{}) })
	expectPanic("broken counts", "counts", func() {
		l := newLimiter(small)
		l.acquire(dbKey{"t", "a"})
		l.tenants["t"].inFlight = 99 // corrupt: over cap
		l.checkTenant("t")
	})
	expectPanic("VM over its cap", "VM counts", func() {
		l := newLimiter(small)
		l.acquire(dbKey{"t", "a"})
		l.vm.inFlight = 1 << 30
		l.checkTenant("t")
	})
	expectPanic("queued count drift", "waiters", func() {
		l := newLimiter(small)
		l.acquire(dbKey{"t", "a"})
		l.tenants["t"].queued = 1
		l.checkTenant("t")
	})
	expectPanic("not work-conserving", "could run", func() {
		l := newLimiter(small)
		l.acquire(dbKey{"t", "a"})
		l.acquire(dbKey{"t", "a"}) // waits
		l.dbs[dbKey{"t", "a"}].inFlight = 0
		l.tenants["t"].inFlight = 0
		l.checkTenant("t")
	})
	expectPanic("waiter db missing", "inconsistent", func() {
		l := newLimiter(small)
		l.acquire(dbKey{"t", "a"})
		l.acquire(dbKey{"t", "a"})
		delete(l.dbs, dbKey{"t", "a"})
		l.checkTenant("t")
	})
}

// FuzzLimiterModel runs a script of acquire / release / cancel against the limiter and a trivial model (counts
// plus a FIFO of waiters, re-scanned after every release) and compares after every step: the decision, who
// got granted, every count, and the invariant. Shed must happen exactly when the request cannot run and a
// queue is full.
func FuzzLimiterModel(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 0x40, 0x41, 0x40, 0x80, 0x40})
	f.Add([]byte{0, 0, 0, 0, 1, 2, 3, 0, 0, 0})
	f.Add([]byte{0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0x40, 0x41, 0x80, 0x81, 0x40})
	f.Add([]byte{3, 7, 11, 2, 0x45, 0x83, 0x47, 0x40, 0x40, 0x40, 0x40})
	f.Fuzz(func(t *testing.T, script []byte) {
		// Three tenants could hold 6 slots and queue 9: the VM caps (3 and 4) bind.
		lim := AdmissionLimits{TenantInFlight: 2, TenantQueue: 3, DBInFlight: 1, DBQueue: 2, VMInFlight: 3, VMQueue: 4}
		l := newLimiter(lim)
		type mw struct {
			key dbKey
			w   *waiter
		}
		var (
			mT       = map[string]*counts{}
			mD       = map[dbKey]*counts{}
			mWaiters []mw // global arrival order
			running  []dbKey
			vm       counts
		)
		get := func(k dbKey) (*counts, *counts) {
			if mT[k.tenant] == nil {
				mT[k.tenant] = &counts{}
			}
			if mD[k] == nil {
				mD[k] = &counts{}
			}
			return mT[k.tenant], mD[k]
		}
		for i, b := range script {
			op, arg := b>>6, int(b&0x3f)
			switch op {
			case 0, 3: // acquire on one of 3 tenants × 3 dbs
				k := dbKey{[]string{"t0", "t1", "t2"}[arg%3], []string{"a", "b", "c"}[arg/3%3]}
				tc, dc := get(k)
				want := shed
				switch {
				case vm.inFlight < lim.VMInFlight && tc.inFlight < lim.TenantInFlight && dc.inFlight < lim.DBInFlight:
					want = run
				case vm.queued < lim.VMQueue && tc.queued < lim.TenantQueue && dc.queued < lim.DBQueue:
					want = wait
				}
				d, w := l.acquire(k)
				if d != want {
					t.Fatalf("step %d: acquire %v = %d, model %d (t %+v d %+v)", i, k, d, want, *tc, *dc)
				}
				switch d {
				case run:
					tc.inFlight++
					dc.inFlight++
					vm.inFlight++
					running = append(running, k)
				case wait:
					tc.queued++
					dc.queued++
					vm.queued++
					mWaiters = append(mWaiters, mw{k, w})
				}
			case 1: // release a running request
				if len(running) == 0 {
					continue
				}
				j := arg % len(running)
				k := running[j]
				running = append(running[:j], running[j+1:]...)
				tc, dc := get(k)
				tc.inFlight--
				dc.inFlight--
				vm.inFlight--
				// The releasing tenant's waiters first, then everyone else's, each in arrival order; the model
				// always scans everything (the limiter skips the second pass when the VM was not full).
				var want []*waiter
				for _, sameTenant := range []bool{true, false} {
					for j := 0; j < len(mWaiters); {
						m := mWaiters[j]
						mt, md := get(m.key)
						if (m.key.tenant == k.tenant) == sameTenant && vm.inFlight < lim.VMInFlight &&
							mt.inFlight < lim.TenantInFlight && md.inFlight < lim.DBInFlight {
							mt.queued--
							md.queued--
							vm.queued--
							mt.inFlight++
							md.inFlight++
							vm.inFlight++
							running = append(running, m.key)
							want = append(want, m.w)
							mWaiters = append(mWaiters[:j], mWaiters[j+1:]...)
							continue
						}
						j++
					}
				}
				got := l.release(k)
				if len(got) != len(want) {
					t.Fatalf("step %d: release %v granted %d, model %d", i, k, len(got), len(want))
				}
				for j := range got {
					if got[j] != want[j] {
						t.Fatalf("step %d: grant order differs at %d", i, j)
					}
				}
			case 2: // cancel a waiter
				if len(mWaiters) == 0 {
					continue
				}
				j := arg % len(mWaiters)
				m := mWaiters[j]
				mWaiters = append(mWaiters[:j], mWaiters[j+1:]...)
				tc, dc := get(m.key)
				tc.queued--
				dc.queued--
				vm.queued--
				if !l.cancel(m.w) {
					t.Fatalf("step %d: cancel of a queued waiter failed", i)
				}
			}
			for name, c := range mT {
				if got := l.tenants[name]; (got == nil) != (*c == counts{}) || (got != nil && got.counts != *c) {
					t.Fatalf("step %d: tenant %s = %+v, model %+v", i, name, got, *c)
				}
				if err := l.tenantProblem(name); err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
			}
			if l.vm != vm {
				t.Fatalf("step %d: VM %+v, model %+v", i, l.vm, vm)
			}
			if err := l.vmProblem(); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
			for k, c := range mD {
				if got := l.dbs[k]; (got == nil) != (*c == counts{}) || (got != nil && got.counts != *c) {
					t.Fatalf("step %d: db %v = %+v, model %+v", i, k, got, *c)
				}
			}
		}
	})
}

// lateness: the wait and finish estimates per level at its cap, the slowest deciding; nothing without a rate.
func TestLimiterLateness(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	lim := AdmissionLimits{DBInFlight: 2, DBQueue: 100, TenantInFlight: 4, TenantQueue: 100, VMInFlight: 8, VMQueue: 100}
	k := dbKey{"t", "d"}
	// fill takes n slots for db and queues q more, then records perSec completions for it in the last whole second.
	fill := func(l *limiter, db dbKey, n, q, perSec int) {
		for range n + q {
			l.acquire(db)
		}
		for range perSec {
			l.done(db, now.Add(-time.Second))
		}
	}
	for _, tc := range []struct {
		name         string
		setup        func(l *limiter)
		vmRate       float64
		wait, finish time.Duration
		known        bool
	}{
		{"nothing held: runs now", func(l *limiter) {}, 100, 0, 0, false},
		{"database below its cap", func(l *limiter) { fill(l, k, 1, 0, 10) }, 100, 0, 0, false},
		{"database at its cap, no completion measured", func(l *limiter) { fill(l, k, 2, 3, 0) }, 100, 0, 0, false},
		// 2 in flight, 3 queued, 10/s: the 4 ahead-and-self drain in 400 ms, the 2 in flight are 200 ms more.
		{"database at its cap", func(l *limiter) { fill(l, k, 2, 3, 10) }, 100, 400 * time.Millisecond, 600 * time.Millisecond, true},
		{"database at its cap, empty queue (on point)", func(l *limiter) { fill(l, k, 2, 0, 10) }, 100, 100 * time.Millisecond, 300 * time.Millisecond, true},
		// Another database of the tenant fills the tenant (4): this one could run by its own cap, the tenant decides.
		{"tenant at its cap", func(l *limiter) { fill(l, dbKey{"t", "x"}, 2, 0, 4); fill(l, dbKey{"t", "y"}, 2, 0, 4) }, 100,
			125 * time.Millisecond, 625 * time.Millisecond, true},
		// The VM is full with other tenants' work: its rate (16/s over 8 in flight) decides.
		{"VM at its cap", func(l *limiter) {
			for _, tn := range []string{"a", "b", "c", "e"} {
				fill(l, dbKey{tn, "d"}, 2, 0, 0)
			}
		}, 16, 62500 * time.Microsecond, 562500 * time.Microsecond, true},
	} {
		l := newLimiter(lim)
		tc.setup(l)
		wait, finish, known := l.lateness(k, now, tc.vmRate)
		if wait != tc.wait || finish != tc.finish || known != tc.known {
			t.Errorf("%s: wait %v finish %v known %v; want %v %v %v", tc.name, wait, finish, known, tc.wait, tc.finish, tc.known)
		}
		if l.dbs[dbKey{"t", "unseen"}] != nil {
			t.Errorf("%s: lateness created state", tc.name)
		}
	}
}
