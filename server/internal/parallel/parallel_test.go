package parallel_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/panicx"
	"github.com/nickstrad/mansio/server/internal/parallel"
)

func TestForEachBoundsAndReports(t *testing.T) {
	var now, peak atomic.Int64
	bad := errors.New("bad")
	errs := parallel.ForEach(context.Background(), 20, 3, func(_ context.Context, i int) error {
		n := now.Add(1)
		defer now.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(2 * time.Millisecond)
		if i%5 == 0 {
			return bad
		}
		return nil
	})
	failed := 0
	for i, err := range errs {
		if (err != nil) != (i%5 == 0) {
			t.Fatalf("item %d: %v", i, err)
		}
		if err != nil {
			failed++
		}
	}
	if len(errs) != 20 || failed != 4 || peak.Load() != 3 {
		t.Fatalf("len %d failed %d peak %d", len(errs), failed, peak.Load())
	}
	if errs := parallel.ForEach(context.Background(), 0, 4, nil); len(errs) != 0 {
		t.Fatal("n = 0")
	}
}

func TestForEachStopsOnCtxAndForwardsPanics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	errs := parallel.ForEach(ctx, 10, 1, func(_ context.Context, i int) error {
		if i == 2 {
			cancel()
		}
		return nil
	})
	for i, err := range errs {
		if (i > 2) != errors.Is(err, context.Canceled) {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	defer func() {
		if p, ok := recover().(*panicx.Panic); !ok || p == nil {
			t.Fatalf("panic not forwarded: %v", p)
		}
	}()
	parallel.ForEach(context.Background(), 4, 2, func(context.Context, int) error { panic("boom") })
}
