// Package parallel runs a function over n items on a bounded number of goroutines, the Cox-Buday way: every
// worker reports to the caller, a panic in one is recovered there and re-raised in the caller once all have
// stopped, and ctx stops new items from starting.
//
// Error module: none (it returns the callers' own errors and ctx.Err()).
package parallel

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/nickstrad/mansio/server/internal/panicx"
)

// ForEach calls fn(ctx, i) for every i in [0, n) on at most workers goroutines and returns each item's error
// (nil for success). Once ctx ends, items not started yet are not run; their error is ctx.Err().
//
//	post: len(errs) == n
//	post: a panic in fn is re-raised here (as a *panicx.Panic) after every worker has stopped
func ForEach(ctx context.Context, n, workers int, fn func(ctx context.Context, i int) error) []error {
	errs := make([]error, n)
	var next atomic.Int64
	var first atomic.Pointer[panicx.Panic]
	var wg sync.WaitGroup
	for range min(max(workers, 1), n) {
		wg.Go(func() {
			defer func() {
				if p := panicx.From(recover()); p != nil {
					first.CompareAndSwap(nil, p)
				}
			}()
			for i := int(next.Add(1)) - 1; i < n; i = int(next.Add(1)) - 1 {
				if err := ctx.Err(); err != nil {
					errs[i] = err
					continue
				}
				errs[i] = fn(ctx, i)
			}
		})
	}
	wg.Wait()
	if p := first.Load(); p != nil {
		panic(p)
	}
	return errs
}
