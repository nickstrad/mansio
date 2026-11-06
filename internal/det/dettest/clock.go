// Package dettest has the simulated implementations of det's ports for tests and the deterministic simulation:
// Clock, a manual clock. Seeded randomness and ids are det.NewRand and det.NewIDs.
package dettest

import (
	"container/heap"
	"context"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/det"
)

// Clock is a manual det.Clock: time stands still until Advance moves it, and Advance fires the timers that
// come due, one at a time in (due time, creation) order, with Now reading each timer's due time while it fires.
// Safe for concurrent use.
//
// Timer channels are buffered (1) and never block Advance: a tick nobody received is dropped, as with
// time.Ticker. AfterFunc and context-deadline callbacks run on the goroutine that calls Advance, after the
// clock's lock is released, so the order of their effects is the order of their due times. A timer made with
// d <= 0 fires at once (a channel timer) or at the next Advance, even Advance(0) (an AfterFunc).
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	seq    uint64
	timers timerHeap
}

var _ det.Clock = (*Clock)(nil)

// NewClock returns a clock reading start.
func NewClock(start time.Time) *Clock { return &Clock{now: start} }

// Now is the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d, firing every timer due by then. pre: d >= 0
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		if len(c.timers) == 0 || c.timers[0].when.After(target) {
			c.now = target
			c.mu.Unlock()
			return
		}
		t := heap.Pop(&c.timers).(*timer)
		if t.when.After(c.now) {
			c.now = t.when
		}
		if t.period > 0 {
			t.when = t.when.Add(t.period)
			c.push(t)
		}
		now, f := c.now, t.f
		if t.ch != nil {
			select {
			case t.ch <- now:
			default:
			}
		}
		c.mu.Unlock()
		if f != nil {
			f()
		}
	}
}

// Next is when the earliest pending timer fires; ok is false when none is pending. A simulation's event loop
// jumps to it with Advance(next.Sub(Now())).
func (c *Clock) Next() (next time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.timers) == 0 {
		return time.Time{}, false
	}
	return c.timers[0].when, true
}

// Pending is the number of timers (and tickers) waiting to fire.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// AwaitPending waits, in real time and for up to 10 s, until at least n timers are pending: a test's way to know
// that a goroutine has reached its wait before calling Advance. It reports whether they were.
func (c *Clock) AwaitPending(n int) bool {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Microsecond) {
		if c.Pending() >= n {
			return true
		}
	}
	return false
}

// NewTimer is time.NewTimer on the clock.
func (c *Clock) NewTimer(d time.Duration) det.Timer {
	t := &timer{c: c, ch: make(chan time.Time, 1), idx: -1}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arm(t, d)
	return t
}

// NewTicker is time.NewTicker on the clock. pre: d > 0
func (c *Clock) NewTicker(d time.Duration) det.Ticker {
	if d <= 0 {
		panic("dettest: non-positive interval for NewTicker")
	}
	t := &timer{c: c, ch: make(chan time.Time, 1), period: d, idx: -1}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arm(t, d)
	return ticker{t}
}

// AfterFunc is time.AfterFunc on the clock; f runs on the goroutine that calls Advance.
func (c *Clock) AfterFunc(d time.Duration, f func()) det.Timer {
	t := &timer{c: c, f: f, idx: -1}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arm(t, max(d, 0))
	return t
}

// WithDeadlineCause is context.WithDeadlineCause on the clock: the context reports deadline t and ends with
// context.DeadlineExceeded (cause: cause, or DeadlineExceeded) when Advance reaches t. Contexts derived from
// it with the context package see the same error and cause. Cancellation from a parent that is not one of
// this clock's contexts arrives asynchronously (as for any context the context package does not own).
func (c *Clock) WithDeadlineCause(parent context.Context, t time.Time, cause error) (context.Context, context.CancelFunc) {
	if cur, ok := parent.Deadline(); ok && !cur.After(t) {
		return context.WithCancel(parent) // the parent ends first, as in the context package
	}
	if cause == nil {
		cause = context.DeadlineExceeded
	}
	inner, cancelInner := context.WithCancelCause(context.WithoutCancel(parent))
	d := &deadlineCtx{inner: inner, cancelInner: cancelInner, deadline: t, done: make(chan struct{})}
	cancel := func() { d.end(context.Canceled, context.Canceled) }
	if !t.After(c.Now()) {
		d.end(context.DeadlineExceeded, cause)
		return d, cancel
	}
	if parent.Done() != nil {
		var stop func() bool
		if p, ok := parent.(*deadlineCtx); ok {
			stop = p.AfterFunc(func() { d.end(p.Err(), context.Cause(p)) })
		} else {
			stop = context.AfterFunc(parent, func() { d.end(parent.Err(), context.Cause(parent)) })
		}
		d.onEnd(stop)
	}
	d.onEnd(c.AfterFunc(t.Sub(c.Now()), func() { d.end(context.DeadlineExceeded, cause) }).Stop)
	return d, cancel
}

// deadlineCtx is a context with a deadline in a Clock's time. Its own done channel (not inner's) makes the
// context package treat it as a foreign parent and propagate through AfterFunc, so derived contexts get
// DeadlineExceeded rather than Canceled. inner only holds the cause for context.Cause.
type deadlineCtx struct {
	inner       context.Context // values of the parent; context.Cause reads inner's cause
	cancelInner context.CancelCauseFunc
	deadline    time.Time
	done        chan struct{}

	mu    sync.Mutex
	err   error
	after []*func()     // AfterFunc callbacks still registered
	stops []func() bool // the timer and the parent watch, stopped on end
}

func (d *deadlineCtx) Deadline() (time.Time, bool) { return d.deadline, true }
func (d *deadlineCtx) Done() <-chan struct{}       { return d.done }
func (d *deadlineCtx) Value(key any) any           { return d.inner.Value(key) }

func (d *deadlineCtx) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// AfterFunc runs f once d ends (at once if it has); stop unregisters f. The context package calls it to
// propagate d's end to derived contexts.
func (d *deadlineCtx) AfterFunc(f func()) (stop func() bool) {
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		f()
		return func() bool { return false }
	}
	p := &f
	d.after = append(d.after, p)
	d.mu.Unlock()
	return func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for i, q := range d.after {
			if q == p {
				d.after = append(d.after[:i], d.after[i+1:]...)
				return true
			}
		}
		return false
	}
}

// onEnd has end call stop (at once if d has ended).
func (d *deadlineCtx) onEnd(stop func() bool) {
	d.mu.Lock()
	if d.err == nil {
		d.stops = append(d.stops, stop)
		stop = nil
	}
	d.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// end ends d with err and cause; only the first call counts. Callbacks run on the caller's goroutine.
func (d *deadlineCtx) end(err, cause error) {
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return
	}
	d.err = err
	d.cancelInner(cause)
	close(d.done)
	after, stops := d.after, d.stops
	d.after, d.stops = nil, nil
	d.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
	for _, f := range after {
		(*f)()
	}
}

// arm schedules t d from now (a channel timer with d <= 0 fires at once). pre: c.mu held, t not pending
func (c *Clock) arm(t *timer, d time.Duration) {
	if d <= 0 && t.ch != nil && t.period == 0 {
		select {
		case t.ch <- c.now:
		default:
		}
		return
	}
	t.when = c.now.Add(d)
	c.push(t)
}

func (c *Clock) push(t *timer) {
	c.seq++
	t.seq = c.seq
	heap.Push(&c.timers, t)
}

// stop unschedules t and drains its channel (no stale value after Stop or Reset). pre: c.mu held
func (c *Clock) stop(t *timer) bool {
	pending := t.idx >= 0
	if pending {
		heap.Remove(&c.timers, t.idx)
	}
	if t.ch != nil {
		select {
		case <-t.ch:
		default:
		}
	}
	return pending
}

type timer struct {
	c      *Clock
	when   time.Time
	seq    uint64
	period time.Duration // > 0: a ticker
	ch     chan time.Time
	f      func()
	idx    int // index in c.timers; -1 when not pending
}

func (t *timer) C() <-chan time.Time { return t.ch }

func (t *timer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	return t.c.stop(t)
}

func (t *timer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	pending := t.c.stop(t)
	t.c.arm(t, d)
	return pending
}

type ticker struct{ t *timer }

func (k ticker) C() <-chan time.Time { return k.t.ch }
func (k ticker) Stop()               { k.t.Stop() }

// Reset is time.Ticker.Reset: the next tick is d from now, then every d. pre: d > 0
func (k ticker) Reset(d time.Duration) {
	if d <= 0 {
		panic("dettest: non-positive interval for Ticker.Reset")
	}
	k.t.c.mu.Lock()
	defer k.t.c.mu.Unlock()
	k.t.c.stop(k.t)
	k.t.period = d
	k.t.c.arm(k.t, d)
}

// timerHeap orders pending timers by due time, then creation (or re-arm) order.
type timerHeap []*timer

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	if !h[i].when.Equal(h[j].when) {
		return h[i].when.Before(h[j].when)
	}
	return h[i].seq < h[j].seq
}
func (h timerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx, h[j].idx = i, j
}
func (h *timerHeap) Push(x any) {
	t := x.(*timer)
	t.idx = len(*h)
	*h = append(*h, t)
}
func (h *timerHeap) Pop() any {
	old := *h
	t := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	t.idx = -1
	return t
}
