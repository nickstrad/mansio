// Package fronttest holds fakes for the front's ports, with fault switches, and the Executor contract suite.
package fronttest

import (
	"fmt"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/front"
)

// Admitter is a fake ownership port: databases have epochs; switches make it fail, block or panic.
// It counts admissions and releases and fails the release check if one is called twice.
type Admitter struct {
	mu      sync.Mutex
	epochs  map[string]uint64
	Err     error         // returned by every Admit when set (e.g. front.ErrFenced)
	Panic   bool          // Admit panics
	Delay   time.Duration // Admit sleeps first
	NoRel   bool          // return a nil release with a nil error (a broken adapter)
	admits  int
	open    int
	doubles int
}

// NewAdmitter returns an admitter where every database in epochs is owned at that epoch.
func NewAdmitter(epochs map[string]uint64) *Admitter { return &Admitter{epochs: epochs} }

// Set changes db's epoch (0 = not owned here).
func (a *Admitter) Set(db string, epoch uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.epochs[db] = epoch
}

// SetErr sets the error switch.
func (a *Admitter) SetErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Err = err
}

// Admit implements front.Admitter.
func (a *Admitter) Admit(db string, epoch uint64) (func(), error) {
	a.mu.Lock()
	delay, p, err, norel := a.Delay, a.Panic, a.Err, a.NoRel
	owned := a.epochs[db]
	a.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if p {
		panic("fronttest: Admitter panics on purpose")
	}
	if err != nil {
		return nil, err
	}
	if owned == 0 || owned != epoch {
		return nil, fmt.Errorf("%w: db %s at epoch %d, ticket epoch %d", front.ErrWrongOwner, db, owned, epoch)
	}
	if norel {
		return nil, nil
	}
	a.mu.Lock()
	a.admits++
	a.open++
	a.mu.Unlock()
	var once bool
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if once {
			a.doubles++
			return
		}
		once = true
		a.open--
	}, nil
}

// Counts returns admissions, admissions not yet released, and releases called more than once.
func (a *Admitter) Counts() (admits, open, doubles int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.admits, a.open, a.doubles
}

// Clock is a settable clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts at t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now reads the clock; use (*Clock).Now as a front.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
