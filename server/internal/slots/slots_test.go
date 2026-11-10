package slots_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/slots"
)

// On and off points of the limit, the refusal count, and a limit changed while slots are held.
func TestPoolLimit(t *testing.T) {
	p := slots.New(2)
	if !p.TryAcquire() || !p.TryAcquire() {
		t.Fatal("the pool refused within its limit")
	}
	if p.TryAcquire() || p.Shed() != 1 || p.InUse() != 2 {
		t.Fatalf("a third caller: in use %d, shed %d; want refused and counted", p.InUse(), p.Shed())
	}
	// Lowered to 1 with 2 held: the holders finish, nobody new until fewer than 1 is in use.
	if prev := p.SetLimit(1); prev != 2 || p.Limit() != 1 {
		t.Fatalf("SetLimit(1) = %d, limit %d", prev, p.Limit())
	}
	p.Release()
	if p.TryAcquire() {
		t.Fatal("admitted at the lowered limit with one slot still held")
	}
	p.Release()
	if !p.TryAcquire() {
		t.Fatal("refused with nothing in use")
	}
	// Raised: the next caller gets the new room at once.
	p.SetLimit(3)
	if !p.TryAcquire() || !p.TryAcquire() || p.TryAcquire() {
		t.Fatalf("after raising to 3: in use %d", p.InUse())
	}
	st := p.Stat("verify")
	if st.Queue != "verify" || st.Capacity != 3 || st.Depth != 3 || st.Rejected[metrics.ReasonFull] != 3 {
		t.Fatalf("%+v", st)
	}
}

// Under concurrency never more than the limit hold a slot, and every caller either held one or was counted.
func TestPoolConcurrent(t *testing.T) {
	const limit, callers, rounds = 4, 16, 2000
	p := slots.New(limit)
	var holding, peak, got atomic.Int64
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			for range rounds {
				if !p.TryAcquire() {
					continue
				}
				got.Add(1)
				n := holding.Add(1)
				for m := peak.Load(); n > m && !peak.CompareAndSwap(m, n); m = peak.Load() {
				}
				holding.Add(-1)
				p.Release()
			}
		})
	}
	wg.Wait()
	if peak.Load() > limit || p.InUse() != 0 || got.Load()+p.Shed() != callers*rounds {
		t.Fatalf("peak %d (limit %d), in use %d, %d held + %d shed of %d", peak.Load(), limit, p.InUse(), got.Load(), p.Shed(), callers*rounds)
	}
}
