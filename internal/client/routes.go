package client

import (
	"context"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/panicx"
)

// Route is where one database lives now, as the locator said: the owner VM's endpoint, the placement epoch
// and generation, and a ticket for that VM at that epoch.
type Route struct {
	DB       string // the database key the agent knows it by (tenant:name)
	VM       string
	Endpoint string // host:port, or a URL with a scheme
	Epoch    uint64
	Gen      uint64
	Ticket   string
	TTL      time.Duration

	refreshAt time.Time // refresh in the background from here on (ahead of TTL, jittered)
	until     time.Time // do not use after this (the ticket expires)
}

// loader resolves one database. It runs in the cache's own goroutine under its own deadline.
type loader func(ctx context.Context, db string, minEpoch uint64) (Route, error)

// routes is the location cache: one route per database, refreshed ahead of its ticket's TTL, with at most one
// resolve in flight per database (single-flight). A load runs under its own deadline (timeout, derived from
// root), so a caller that gives up does not cancel it for the others, and it cannot outlive timeout.
//
// Bounded: at most max databases; inserting past that drops an idle entry (Release It B6).
type routes struct {
	load    loader
	root    context.Context
	timeout time.Duration
	max     int
	now     func() time.Time
	place   func(fetched time.Time, ttl time.Duration) time.Time // refreshAt, with the jitter drawn

	mu sync.Mutex
	m  map[string]*slot // guarded by mu
}

type slot struct {
	r        *Route  // nil until a load succeeds, and after drop
	minEpoch uint64  // a route below this epoch is known stale (set by drop)
	f        *flight // the load in flight, if any
}

// flight is one load; done closes when r, err (or p) are set.
type flight struct {
	minEpoch uint64
	done     chan struct{}
	r        Route
	err      error
	p        *panicx.Panic
}

func newRoutes(root context.Context, load loader, timeout time.Duration, max int, now func() time.Time,
	place func(time.Time, time.Duration) time.Time) *routes {
	return &routes{load: load, root: root, timeout: timeout, max: max, now: now, place: place, m: map[string]*slot{}}
}

// get returns db's route at epoch >= minEpoch. hit reports that it came from the cache without waiting. A
// cached route past its refresh time is still returned, and a background load replaces it.
func (c *routes) get(ctx context.Context, db string, minEpoch uint64) (r Route, hit bool, err error) {
	for try := 0; ; try++ {
		c.mu.Lock()
		s := c.slot(db)
		need := max(minEpoch, s.minEpoch)
		now := c.now()
		if s.r != nil && s.r.Epoch >= need && now.Before(s.r.until) {
			if !now.Before(s.r.refreshAt) && s.f == nil {
				s.f = c.start(db, 0)
			}
			r := *s.r
			c.mu.Unlock()
			return r, try == 0, nil
		}
		f := s.f
		if f == nil || f.minEpoch < need && try > 0 {
			f = c.start(db, need)
			s.f = f
		}
		c.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return Route{}, false, ctx.Err()
		}
		if f.p != nil {
			panic(f.p) // the loader's bug, with its stack, in the caller's goroutine
		}
		if f.err != nil {
			return Route{}, false, f.err
		}
		if f.r.Epoch >= need || try > 0 {
			return f.r, false, nil // a second load still below need: the agent will say so, and we reroute
		}
	}
}

// drop forgets db's route if it is still the one at epoch (a newer one fetched meanwhile stays), and makes the
// next load ask for epoch >= minEpoch (min_epoch: the locator then reads the primary). The primary's answer
// ends the requirement even if its epoch is lower.
func (c *routes) drop(db string, epoch, minEpoch uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.slot(db)
	if s.r != nil && s.r.Epoch == epoch {
		s.r = nil
	}
	s.minEpoch = max(s.minEpoch, minEpoch)
}

// slot returns db's slot, making room if the cache is full.
//
//	pre: c.mu held
func (c *routes) slot(db string) *slot {
	if s := c.m[db]; s != nil {
		return s
	}
	if len(c.m) >= c.max {
		for k, s := range c.m { // map order is random enough: any idle entry may go
			if s.f == nil {
				delete(c.m, k)
				break
			}
		}
	}
	s := &slot{}
	c.m[db] = s
	return s
}

// start runs one load in its own goroutine.
//
//	pre: c.mu held
func (c *routes) start(db string, minEpoch uint64) *flight {
	f := &flight{minEpoch: minEpoch, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		defer func() {
			if p := panicx.From(recover()); p != nil {
				f.p = p
				c.finish(db, f)
			}
		}()
		sent := c.now()
		ctx, cancel := context.WithTimeout(c.root, c.timeout)
		r, err := c.load(ctx, db, minEpoch)
		cancel()
		if err == nil {
			r.until = usableUntil(sent, r.TTL)
			r.refreshAt = c.place(sent, r.TTL)
		}
		f.r, f.err = r, err
		c.finish(db, f)
	}()
	return f
}

// finish records f's result: a route replaces the cached one unless that one is newer.
func (c *routes) finish(db string, f *flight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.m[db]
	if s == nil { // evicted meanwhile (cannot happen while f is its flight, but a replaced flight may land late)
		return
	}
	if s.f == f {
		s.f = nil
	}
	if f.err != nil || f.p != nil {
		return
	}
	if f.minEpoch > 0 && f.minEpoch >= s.minEpoch {
		// The locator read the primary for this load: its epoch is the truth, even below what we asked for (a
		// new owner answers wrong_owner at the current epoch until its claim lands). Stop requiring more.
		s.minEpoch = 0
	}
	if s.r != nil && s.r.Epoch > f.r.Epoch {
		return
	}
	r := f.r
	s.r = &r
}

// size is the number of cached databases (tests).
func (c *routes) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
