package det

import (
	"context"
	"time"
)

// Clock is the time port: the wall clock in production, a simulated one in tests and DST.
//
// Contexts with deadlines come from the clock too (WithTimeout, WithDeadline), so a deadline read back with
// ctx.Deadline() is comparable with Now() and expires in the clock's time.
type Clock interface {
	Now() time.Time
	// NewTimer is time.NewTimer on this clock.
	NewTimer(d time.Duration) Timer
	// NewTicker is time.NewTicker on this clock. pre: d > 0
	NewTicker(d time.Duration) Ticker
	// AfterFunc is time.AfterFunc on this clock: f runs once, d from now, unless the timer is stopped first.
	// The returned timer's C is nil.
	AfterFunc(d time.Duration, f func()) Timer
	// WithDeadlineCause is context.WithDeadlineCause on this clock: the context ends at t in the clock's time.
	WithDeadlineCause(ctx context.Context, t time.Time, cause error) (context.Context, context.CancelFunc)
}

// Timer is a *time.Timer (Go 1.23 semantics: after Stop or Reset no stale value is received).
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Ticker is a *time.Ticker.
type Ticker interface {
	C() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// System is the wall clock: time.Now and the runtime's timers.
func System() Clock { return system{} }

// Or returns c, or System() when c is nil: the default for an optional Clock field.
func Or(c Clock) Clock {
	if c == nil {
		return system{}
	}
	return c
}

// Since is time.Since on c.
func Since(c Clock, t time.Time) time.Duration { return c.Now().Sub(t) }

// Until is time.Until on c.
func Until(c Clock, t time.Time) time.Duration { return t.Sub(c.Now()) }

// WithTimeout is context.WithTimeout on c.
func WithTimeout(ctx context.Context, c Clock, d time.Duration) (context.Context, context.CancelFunc) {
	return c.WithDeadlineCause(ctx, c.Now().Add(d), nil)
}

// WithDeadline is context.WithDeadline on c.
func WithDeadline(ctx context.Context, c Clock, t time.Time) (context.Context, context.CancelFunc) {
	return c.WithDeadlineCause(ctx, t, nil)
}

// Sleep waits d on c, or until ctx ends. It returns nil after a full wait and ctx.Err() otherwise; d <= 0
// returns ctx.Err() at once.
func Sleep(ctx context.Context, c Clock, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := c.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type system struct{}

func (system) Now() time.Time                            { return time.Now() }
func (system) NewTimer(d time.Duration) Timer            { return sysTimer{time.NewTimer(d)} }
func (system) NewTicker(d time.Duration) Ticker          { return sysTicker{time.NewTicker(d)} }
func (system) AfterFunc(d time.Duration, f func()) Timer { return sysTimer{time.AfterFunc(d, f)} }
func (system) WithDeadlineCause(ctx context.Context, t time.Time, cause error) (context.Context, context.CancelFunc) {
	return context.WithDeadlineCause(ctx, t, cause)
}

type sysTimer struct{ t *time.Timer }

func (s sysTimer) C() <-chan time.Time        { return s.t.C }
func (s sysTimer) Stop() bool                 { return s.t.Stop() }
func (s sysTimer) Reset(d time.Duration) bool { return s.t.Reset(d) }

type sysTicker struct{ t *time.Ticker }

func (s sysTicker) C() <-chan time.Time   { return s.t.C }
func (s sysTicker) Stop()                 { s.t.Stop() }
func (s sysTicker) Reset(d time.Duration) { s.t.Reset(d) }
