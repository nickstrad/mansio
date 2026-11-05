package owner_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/owner/ownertest"
)

// Many goroutines Admit while Fence runs. No permit may be held when the final write-back starts, and no Admit
// may succeed after Fence has returned.
func TestAdmitRacingFenceNoPermitDuringFinalWriteBack(t *testing.T) {
	for round := range 50 {
		w := newWorld(t, "a")
		w.place("k", "a")
		a := w.tab("a")
		var held, heldAtWriteBack, admittedAfterAck atomic.Int64
		var acked atomic.Bool
		w.procs["a"].wb.Set(func(wb *ownertest.WriteBack) {
			wb.OnPublish = func(string, owner.Epoch) { heldAtWriteBack.Store(held.Load()) }
		})
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					p, err := a.Admit("k", 1)
					if err != nil {
						runtime.Gosched()
						continue
					}
					if acked.Load() {
						admittedAfterAck.Add(1)
					}
					held.Add(1)
					runtime.Gosched()
					held.Add(-1)
					p.Done()
				}
			})
		}
		time.Sleep(time.Duration(round%5) * 100 * time.Microsecond)
		_, err := a.Fence(ctx, "k")
		acked.Store(true)
		time.Sleep(200 * time.Microsecond)
		close(stop)
		wg.Wait()
		must(t, err)
		if n := heldAtWriteBack.Load(); n != 0 {
			t.Fatalf("round %d: %d permits held when the final write-back started", round, n)
		}
		if n := admittedAfterAck.Load(); n != 0 {
			t.Fatalf("round %d: %d admits after the fence ack", round, n)
		}
	}
}

func TestConcurrentFencesShareOneWriteBackAndAck(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	acks := make(chan owner.Ack, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			ack, err := a.Fence(ctx, "k")
			if err != nil {
				t.Error(err)
			}
			acks <- ack
		})
	}
	wg.Wait()
	close(acks)
	first := <-acks
	for ack := range acks {
		if ack != first {
			t.Fatalf("acks differ: %+v vs %+v", ack, first)
		}
	}
	if w.procs["a"].wb.Calls != 1 {
		t.Fatalf("write-backs = %d, want 1", w.procs["a"].wb.Calls)
	}
}

// A held permit blocks Fence; a cancelled Fence leaves admission stopped; a later Fence finishes once the
// permit is done.
func TestFenceWaitsForPermitAndSurvivesCancel(t *testing.T) {
	w := newWorld(t, "a")
	w.place("k", "a")
	a := w.tab("a")
	p, err := a.Admit("k", 1)
	must(t, err)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = a.Fence(cctx, "k")
	mustIs(t, err, context.DeadlineExceeded)
	if _, err := a.Admit("k", 1); err != owner.ErrMoving {
		t.Fatalf("Admit after a cancelled fence = %v, want ErrMoving", err)
	}
	if w.procs["a"].wb.Calls != 0 {
		t.Fatal("wrote back while a permit was held")
	}
	done := make(chan error, 1)
	go func() { _, err := a.Fence(ctx, "k"); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("Fence returned while a permit was held: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	p.Done()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Fence did not finish after the permit was done")
	}
	// A Fence waiting for the lifecycle lock honours its context.
	cctx2, cancel2 := contextCanceled()
	defer cancel2()
	w2 := newWorld(t, "a")
	w2.place("k", "a")
	p2, _ := w2.tab("a").Admit("k", 1)
	go func() { _, _ = w2.tab("a").Fence(ctx, "k") }()
	time.Sleep(5 * time.Millisecond)
	_, err = w2.tab("a").Fence(cctx2, "k")
	mustIs(t, err, context.Canceled)
	p2.Done()
}

// Periodic publishes race a fence: no publish at the fenced epoch may land after the ack.
func TestPublishRacingFence(t *testing.T) {
	for range 30 {
		w := newWorld(t, "a")
		w.place("k", "a")
		a := w.tab("a")
		pub := func(c context.Context) (owner.Generation, error) { return w.procs["a"].wb.Publish(c, "k", 1) }
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					_ = w.write("a", "k", 1, "x")
					_, _ = a.Publish(ctx, "k", 1, pub)
				}
			})
		}
		time.Sleep(200 * time.Microsecond)
		ack, err := a.Fence(ctx, "k")
		must(t, err)
		after := w.objs.PublishCount()
		time.Sleep(200 * time.Microsecond)
		close(stop)
		wg.Wait()
		if n := w.objs.PublishCount(); n != after {
			t.Fatalf("%d publishes landed after the fence ack", n-after)
		}
		if g, _, _, _, _ := w.objs.Read("k"); g != ack.Gen {
			t.Fatalf("manifest gen %d, ack gen %d", g, ack.Gen)
		}
	}
}

// A publish that finds a higher epoch stops admission before Publish returns, even with admits racing it.
func TestSupersedeRacingAdmit(t *testing.T) {
	w := newWorld(t, "a", "b")
	w.place("k", "a")
	a := w.tab("a")
	w.pl.Create("k", owner.Place{Owner: "b", Epoch: 2, State: owner.PlaceActive})
	_, err := w.tab("b").Claim(ctx, "k")
	must(t, err)
	var returned atomic.Bool
	var late atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				was := returned.Load()
				if err := a.Do("k", 1, func() error { return nil }); err == nil && was {
					late.Add(1)
				}
			}
		})
	}
	_, err = a.Publish(ctx, "k", 1, func(c context.Context) (owner.Generation, error) { return w.procs["a"].wb.Publish(c, "k", 1) })
	returned.Store(true)
	time.Sleep(time.Millisecond)
	close(stop)
	wg.Wait()
	mustIs(t, err, owner.ErrSuperseded)
	if late.Load() != 0 {
		t.Fatalf("%d admits after Publish reported superseded", late.Load())
	}
}
