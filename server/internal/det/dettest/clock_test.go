package dettest_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
)

type key struct{}

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func fired(c <-chan time.Time) (time.Time, bool) {
	select {
	case v := <-c:
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestTimersFireInOrderAtTheirTime(t *testing.T) {
	clk := dettest.NewClock(t0)
	var order []string
	var seen []time.Time
	note := func(name string) func() {
		return func() { order = append(order, name); seen = append(seen, clk.Now()) }
	}
	clk.AfterFunc(2*time.Second, note("b"))
	clk.AfterFunc(time.Second, note("a"))
	clk.AfterFunc(2*time.Second, note("c")) // same time as b: creation order
	clk.AfterFunc(time.Second, func() { clk.AfterFunc(500*time.Millisecond, note("nested")) })
	if next, ok := clk.Next(); !ok || !next.Equal(t0.Add(time.Second)) {
		t.Fatalf("Next = %v, %v", next, ok)
	}
	clk.Advance(3 * time.Second)
	want := []string{"a", "nested", "b", "c"}
	if len(order) != len(want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order %v, want %v", order, want)
		}
	}
	if !seen[1].Equal(t0.Add(1500*time.Millisecond)) || !clk.Now().Equal(t0.Add(3*time.Second)) {
		t.Fatalf("times %v, now %v", seen, clk.Now())
	}
	if clk.Pending() != 0 {
		t.Fatalf("%d pending", clk.Pending())
	}
}

func TestTimerStopReset(t *testing.T) {
	clk := dettest.NewClock(t0)
	tm := clk.NewTimer(time.Second)
	if _, ok := fired(tm.C()); ok {
		t.Fatal("fired early")
	}
	clk.Advance(time.Second)
	if v, ok := fired(tm.C()); !ok || !v.Equal(t0.Add(time.Second)) {
		t.Fatalf("fired %v %v", v, ok)
	}
	if tm.Reset(time.Second) {
		t.Fatal("Reset of a fired timer reported pending")
	}
	clk.Advance(time.Second) // fires; value left in the channel
	if tm.Stop() {
		t.Fatal("Stop of a fired timer reported pending")
	}
	if _, ok := fired(tm.C()); ok {
		t.Fatal("stale value after Stop")
	}
	tm.Reset(time.Minute)
	if !tm.Stop() {
		t.Fatal("Stop of a pending timer reported not pending")
	}
	clk.Advance(time.Hour)
	if _, ok := fired(tm.C()); ok {
		t.Fatal("stopped timer fired")
	}
	if _, ok := fired(clk.NewTimer(0).C()); !ok {
		t.Fatal("NewTimer(0) did not fire at once")
	}
}

func TestTicker(t *testing.T) {
	clk := dettest.NewClock(t0)
	tk := clk.NewTicker(time.Second)
	clk.Advance(3500 * time.Millisecond) // three ticks; the channel holds one, like time.Ticker
	if v, ok := fired(tk.C()); !ok || !v.Equal(t0.Add(time.Second)) {
		t.Fatalf("tick %v %v", v, ok)
	}
	tk.Reset(10 * time.Second)
	clk.Advance(9 * time.Second)
	if _, ok := fired(tk.C()); ok {
		t.Fatal("tick before the reset interval")
	}
	clk.Advance(time.Second)
	if _, ok := fired(tk.C()); !ok {
		t.Fatal("no tick after the reset interval")
	}
	tk.Stop()
	clk.Advance(time.Minute)
	if _, ok := fired(tk.C()); ok || clk.Pending() != 0 {
		t.Fatal("stopped ticker ticked")
	}
}

func TestDeadlineContext(t *testing.T) {
	clk := dettest.NewClock(t0)
	cause := errors.New("my cause")
	ctx, cancel := clk.WithDeadlineCause(context.Background(), t0.Add(time.Second), cause)
	defer cancel()
	if dl, ok := ctx.Deadline(); !ok || !dl.Equal(t0.Add(time.Second)) {
		t.Fatalf("Deadline = %v %v", dl, ok)
	}
	child, cancelChild := det.WithTimeout(ctx, clk, time.Hour) // the parent's deadline is earlier: kept
	defer cancelChild()
	std, cancelStd := context.WithCancel(ctx) // derived with the context package
	defer cancelStd()
	sub, cancelSub := det.WithTimeout(context.WithValue(ctx, key{}, 1), clk, 2*time.Second)
	defer cancelSub()
	if dl, _ := child.Deadline(); !dl.Equal(t0.Add(time.Second)) {
		t.Fatalf("child deadline %v", dl)
	}
	clk.Advance(999 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("ended early: %v", ctx.Err())
	}
	clk.Advance(time.Millisecond)
	<-child.Done()
	<-std.Done() // ended synchronously by Advance: AfterFunc propagation
	<-sub.Done()
	if !errors.Is(std.Err(), context.DeadlineExceeded) || context.Cause(std) != cause {
		t.Fatalf("std child err %v cause %v", std.Err(), context.Cause(std))
	}
	if sub.Value(key{}) != 1 || !errors.Is(sub.Err(), context.DeadlineExceeded) {
		t.Fatalf("sub value %v err %v", sub.Value(key{}), sub.Err())
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) || context.Cause(ctx) != cause {
		t.Fatalf("err %v cause %v", ctx.Err(), context.Cause(ctx))
	}
	if !errors.Is(child.Err(), context.DeadlineExceeded) || context.Cause(child) != cause {
		t.Fatalf("child err %v cause %v", child.Err(), context.Cause(child))
	}

	ctx2, cancel2 := det.WithTimeout(context.Background(), clk, time.Minute)
	cancel2()
	clk.Advance(time.Hour)
	if !errors.Is(ctx2.Err(), context.Canceled) || clk.Pending() != 0 {
		t.Fatalf("canceled ctx err %v, %d timers pending", ctx2.Err(), clk.Pending())
	}

	parent, cancelParent := context.WithCancel(context.Background())
	ctx4, cancel4 := det.WithTimeout(parent, clk, time.Minute)
	defer cancel4()
	cancelParent()
	<-ctx4.Done()
	if !errors.Is(ctx4.Err(), context.Canceled) {
		t.Fatalf("after parent cancel: %v", ctx4.Err())
	}

	past, cancel3 := det.WithDeadline(context.Background(), clk, clk.Now())
	defer cancel3()
	if !errors.Is(past.Err(), context.DeadlineExceeded) {
		t.Fatalf("past deadline err %v", past.Err())
	}
}

// A callback that advances the clock further leaves it there: the outer Advance never moves time back.
func TestReentrantAdvanceNeverMovesBack(t *testing.T) {
	clk := dettest.NewClock(t0)
	clk.AfterFunc(time.Second, func() { clk.Advance(10 * time.Second) })
	clk.Advance(2 * time.Second)
	if got := clk.Now(); !got.Equal(t0.Add(11 * time.Second)) {
		t.Fatalf("Now = %v, want t0+11s", got.Sub(t0))
	}
}

// Concurrent Advance calls: time only moves forward, every due timer fires, and the clock ends at or past the
// furthest target.
func TestConcurrentAdvanceIsMonotonic(t *testing.T) {
	clk := dettest.NewClock(t0)
	const n = 200
	var fired sync.WaitGroup
	fired.Add(n)
	for i := range n {
		clk.AfterFunc(time.Duration(i)*time.Millisecond, fired.Done)
	}
	var wg sync.WaitGroup
	var back atomic.Bool
	for range 4 {
		wg.Go(func() {
			prev := clk.Now()
			for range 100 {
				clk.Advance(time.Millisecond)
				if now := clk.Now(); now.Before(prev) {
					back.Store(true)
				} else {
					prev = now
				}
			}
		})
	}
	wg.Wait()
	fired.Wait()
	if back.Load() || clk.Now().Before(t0.Add(100*time.Millisecond)) || clk.Pending() != 0 {
		t.Fatalf("back %v, now t0+%v, pending %d", back.Load(), clk.Now().Sub(t0), clk.Pending())
	}
}

// A big jump with a fast ticker costs one tick, not one per period (coalesced like time.Ticker).
func TestTickerCoalescesBigJumps(t *testing.T) {
	clk := dettest.NewClock(t0)
	tk := clk.NewTicker(time.Millisecond)
	start := time.Now()
	clk.Advance(24*time.Hour + 500*time.Microsecond)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Advance(24h) with a 1 ms ticker took %v", el)
	}
	if v, ok := fired(tk.C()); !ok || !v.Equal(t0.Add(time.Millisecond)) {
		t.Fatalf("first tick %v %v: want t0+1ms (later ones in the jump dropped)", v, ok)
	}
	if next, _ := clk.Next(); !next.Equal(t0.Add(24*time.Hour + time.Millisecond)) {
		t.Fatalf("next tick at t0+%v, want t0+24h+1ms", next.Sub(t0))
	}
	clk.Advance(time.Millisecond)
	if v, ok := fired(tk.C()); !ok || !v.Equal(t0.Add(24*time.Hour+time.Millisecond)) {
		t.Fatalf("tick after the jump: %v %v", v, ok)
	}
}

// A context derived with the context package from an already-ended clock context, racing with Advance, does
// not deadlock (the context package calls AfterFunc holding the child's lock) and gets the deadline error.
func TestDerivedContextRacingWithAdvance(t *testing.T) {
	for range 200 {
		clk := dettest.NewClock(t0)
		ctx, cancel := det.WithTimeout(context.Background(), clk, time.Millisecond)
		go clk.Advance(time.Millisecond)
		child, cancelChild := context.WithCancel(ctx)
		select {
		case <-child.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("derived context never ended: deadlock in AfterFunc")
		}
		if !errors.Is(child.Err(), context.DeadlineExceeded) {
			t.Fatalf("child err %v", child.Err())
		}
		cancelChild()
		cancel()
	}
	// Derived after the end: AfterFunc runs the propagation asynchronously.
	clk := dettest.NewClock(t0)
	ctx, cancel := det.WithTimeout(context.Background(), clk, time.Millisecond)
	defer cancel()
	clk.Advance(time.Millisecond)
	child, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	<-child.Done()
	if !errors.Is(child.Err(), context.DeadlineExceeded) {
		t.Fatalf("late child err %v", child.Err())
	}
}

// A past deadline under an ended parent reports the parent's error and cause, as the context package does.
func TestEndedParentWinsOverPastDeadline(t *testing.T) {
	clk := dettest.NewClock(t0)
	why := errors.New("parent gone")
	parent, cancelParent := context.WithCancelCause(context.Background())
	cancelParent(why)
	ctx, cancel := det.WithDeadline(parent, clk, t0.Add(-time.Second))
	defer cancel()
	if !errors.Is(ctx.Err(), context.Canceled) || context.Cause(ctx) != why {
		t.Fatalf("err %v cause %v", ctx.Err(), context.Cause(ctx))
	}
}
