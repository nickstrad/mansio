package client

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
)

// newTestRoutes returns a cache whose loader answers epoch (settable) with TTL 10 s, refreshing at exactly 80%.
func newTestRoutes(t *testing.T, max int) (*routes, *dettest.Clock, *atomic.Int64, *atomic.Uint64) {
	clk := dettest.NewClock(time.Unix(1000, 0))
	loads := &atomic.Int64{}
	epoch := &atomic.Uint64{}
	epoch.Store(1)
	load := func(ctx context.Context, db string, minEpoch uint64) (Route, error) {
		loads.Add(1)
		if _, ok := ctx.Deadline(); !ok {
			t.Error("a load without a deadline")
		}
		if db == "boom" {
			return Route{}, errors.New("locator down")
		}
		return Route{DB: db, Epoch: max64(epoch.Load(), minEpoch), TTL: 10 * time.Second}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newRoutes(ctx, load, time.Second, max, clk, func(at time.Time, ttl time.Duration) time.Time {
		return refreshAt(at, ttl, 0.8, 0.1, 0.5)
	}), clk, loads, epoch
}

func max64(a, b uint64) uint64 { return max(a, b) }

func waitLoads(t *testing.T, loads *atomic.Int64, n int64) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); loads.Load() < n; {
		if time.Now().After(deadline) {
			t.Fatalf("%d loads, want %d", loads.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// Refreshed ahead of TTL: before 8 s the cached route is served without a load; from 8 s it is still served
// (a hit) while one background load replaces it; at 9 s (TTL - margin) it is no longer used.
func TestRoutesRefreshAheadOfTTL(t *testing.T) {
	c, clk, loads, epoch := newTestRoutes(t, 10)
	ctx := context.Background()
	if r, hit, err := c.get(ctx, "d"); err != nil || hit || r.Epoch != 1 {
		t.Fatalf("first get: %+v %v %v", r, hit, err)
	}
	clk.Advance(8*time.Second - 1) // off point
	if _, hit, _ := c.get(ctx, "d"); !hit || loads.Load() != 1 {
		t.Fatalf("before refreshAt: hit %v, %d loads", hit, loads.Load())
	}
	epoch.Store(2)
	clk.Advance(1) // on point
	if r, hit, _ := c.get(ctx, "d"); !hit || r.Epoch != 1 {
		t.Fatalf("at refreshAt: %+v hit %v; want the cached route", r, hit)
	}
	waitLoads(t, loads, 2)
	for deadline := time.Now().Add(2 * time.Second); ; {
		if r, _, _ := c.get(ctx, "d"); r.Epoch == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background refresh never replaced the route")
		}
		time.Sleep(time.Millisecond)
	}
	// The refreshed route was fetched at t0+8s; it is unusable from t0+8s+9s on.
	clk.Advance(9 * time.Second)
	if _, hit, _ := c.get(ctx, "d"); hit {
		t.Fatal("a route past its ticket's TTL was used")
	}
}

// drop with min_epoch: the next get loads at least that epoch; a drop for an older epoch keeps a newer route.
func TestRoutesDrop(t *testing.T) {
	c, _, loads, epoch := newTestRoutes(t, 10)
	ctx := context.Background()
	c.get(ctx, "d")
	epoch.Store(5)
	c.drop("d", 4, 0) // not the cached epoch (1): kept
	if _, hit, _ := c.get(ctx, "d"); !hit {
		t.Fatal("a drop for another epoch dropped the route")
	}
	c.drop("d", 1, 2)
	r, hit, _ := c.get(ctx, "d")
	if hit || r.Epoch < 2 || loads.Load() != 2 {
		t.Fatalf("after drop: %+v hit %v loads %d", r, hit, loads.Load())
	}
}

// Loads fail without caching the failure; the cache stays bounded.
func TestRoutesErrorsAndBound(t *testing.T) {
	c, _, loads, _ := newTestRoutes(t, 3)
	ctx := context.Background()
	for range 2 {
		if _, _, err := c.get(ctx, "boom"); err == nil {
			t.Fatal("want the loader's error")
		}
	}
	if loads.Load() != 2 {
		t.Fatalf("a failed load was cached: %d loads", loads.Load())
	}
	for _, db := range []string{"a", "b", "c", "d", "e"} {
		c.get(ctx, db)
	}
	if n := c.size(); n > 3 {
		t.Fatalf("cache holds %d entries, max 3", n)
	}
}

// A caller that leaves does not cancel the load for the others.
func TestRoutesCallerLeaves(t *testing.T) {
	release := make(chan struct{})
	var loads atomic.Int64
	c := newRoutes(context.Background(), func(ctx context.Context, db string, _ uint64) (Route, error) {
		loads.Add(1)
		<-release
		return Route{DB: db, Epoch: 1, TTL: time.Minute}, ctx.Err()
	}, time.Second, 10, det.System(), func(at time.Time, ttl time.Duration) time.Time { return at.Add(ttl / 2) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := c.get(ctx, "d"); done <- err }()
	waitLoads(t, &loads, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("leaving caller: %v", err)
	}
	got := make(chan error, 1)
	go func() { _, _, err := c.get(context.Background(), "d"); got <- err }()
	close(release)
	if err := <-got; err != nil || loads.Load() != 1 {
		t.Fatalf("the second caller: %v, %d loads; want the first load's route", err, loads.Load())
	}
}

// A panic in the loader reaches the caller with its stack.
func TestRoutesLoaderPanic(t *testing.T) {
	c := newRoutes(context.Background(), func(context.Context, string, uint64) (Route, error) { panic("bug") },
		time.Second, 10, det.System(), func(at time.Time, _ time.Duration) time.Time { return at })
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	c.get(context.Background(), "d")
}

// The primary's answer ends a min_epoch requirement even when its epoch is lower: a new owner answers
// wrong_owner at the current epoch until its claim lands, and asking for epoch + 1 forever would turn every
// lookup into a primary read.
func TestRoutesPrimaryAnswerEndsMinEpoch(t *testing.T) {
	c, _, loads, _ := newTestRoutes(t, 10)
	c.load = func(_ context.Context, db string, _ uint64) (Route, error) {
		loads.Add(1)
		return Route{DB: db, Epoch: 1, TTL: 10 * time.Second}, nil // the primary stays at epoch 1
	}
	ctx := context.Background()
	c.get(ctx, "d")
	c.drop("d", 1, 2)
	if r, _, _ := c.get(ctx, "d"); r.Epoch != 1 || loads.Load() != 2 {
		t.Fatalf("%+v after %d loads", r, loads.Load())
	}
	if _, hit, _ := c.get(ctx, "d"); !hit || loads.Load() != 2 {
		t.Fatalf("hit %v, %d loads: the min_epoch requirement outlived the primary's answer", hit, loads.Load())
	}
}
