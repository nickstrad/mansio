package front

import (
	"strings"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/assert"
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
	f.Add([]byte{0, 0, 0, 0, 1, 2, 3, 0, 0, 0})
	f.Add([]byte{0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0x40, 0x41, 0x80, 0x81, 0x40})
	f.Add([]byte{3, 7, 11, 2, 0x45, 0x83, 0x47, 0x40, 0x40, 0x40, 0x40})
	f.Fuzz(func(t *testing.T, script []byte) {
		lim := AdmissionLimits{TenantInFlight: 2, TenantQueue: 3, DBInFlight: 1, DBQueue: 2}
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
			case 0, 3: // acquire on one of 2 tenants × 3 dbs
				k := dbKey{[]string{"t0", "t1"}[arg%2], []string{"a", "b", "c"}[arg/2%3]}
				tc, dc := get(k)
				want := shed
				switch {
				case tc.inFlight < lim.TenantInFlight && dc.inFlight < lim.DBInFlight:
					want = run
				case tc.queued < lim.TenantQueue && dc.queued < lim.DBQueue:
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
					running = append(running, k)
				case wait:
					tc.queued++
					dc.queued++
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
				var want []*waiter
				for j := 0; j < len(mWaiters); {
					m := mWaiters[j]
					mt, md := get(m.key)
					if m.key.tenant == k.tenant && mt.inFlight < lim.TenantInFlight && md.inFlight < lim.DBInFlight {
						mt.queued--
						md.queued--
						mt.inFlight++
						md.inFlight++
						running = append(running, m.key)
						want = append(want, m.w)
						mWaiters = append(mWaiters[:j], mWaiters[j+1:]...)
						continue
					}
					j++
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
			for k, c := range mD {
				if got := l.dbs[k]; (got == nil) != (*c == counts{}) || (got != nil && *got != *c) {
					t.Fatalf("step %d: db %v = %+v, model %+v", i, k, got, *c)
				}
			}
		}
	})
}
