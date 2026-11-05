package owner

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/panicx"
	"github.com/nickstrad/ts-db-go/internal/retry"
)

// ReconcileOptions bound a reconcile round (Release It H1: startup reconciles with bounded concurrency and a
// per-key timeout, so one slow authority row cannot delay every key). Zero fields take the defaults.
type ReconcileOptions struct {
	Workers int           // keys reconciled at once; default 16
	PerKey  time.Duration // deadline for one key (authority read, manifest read, record write); default 5 s
	// Backoff between rounds of ReconcileUntilDone: exponential from Base to Max, equal jitter. Defaults
	// 100 ms and 5 s.
	Backoff retry.Policy
}

func (o *ReconcileOptions) normalize() {
	if o.Workers <= 0 {
		o.Workers = 16
	}
	if o.PerKey <= 0 {
		o.PerKey = 5 * time.Second
	}
	o.Backoff.Backoff, o.Backoff.Jitter, o.Backoff.MaxAttempts = retry.Exponential, retry.Equal, retry.MaxAttemptsCap
	if o.Backoff.Base <= 0 {
		o.Backoff.Base = 100 * time.Millisecond
	}
	if o.Backoff.Max <= 0 {
		o.Backoff.Max = 5 * time.Second
	}
	_ = o.Backoff.Normalize() // cannot fail: every field it checks is set above
}

// ReconcileReport says what one reconcile round did.
type ReconcileReport struct {
	Round  int // 1 for the first round of ReconcileUntilDone (and for ReconcileAll)
	Tried  int // keys that were Closed when the round started
	Failed int // of those, keys that stay closed (authority or storage unreadable); retried next round
	Took   time.Duration
}

// ReconcileAll reconciles every Closed key, at most o.Workers at a time, each under its own o.PerKey deadline
// derived from ctx. A key admits requests the moment its own reconcile succeeds, while the others are still
// running (they answer ErrUnavailable until theirs does). It returns the first error with the count of keys
// that stay closed.
//
//	post: rep.Failed == 0 ⇔ err == nil
func (t *Table) ReconcileAll(ctx context.Context, o ReconcileOptions) (ReconcileReport, error) {
	o.normalize()
	start := time.Now()
	var keys []string
	for _, k := range t.Keys() {
		if t.State(k).State == Closed {
			keys = append(keys, k)
		}
	}
	var (
		failed atomic.Int64
		mu     sync.Mutex
		first  error
		pan    *panicx.Panic
		wg     sync.WaitGroup
		next   atomic.Int64
	)
	for range min(o.Workers, len(keys)) {
		wg.Go(func() {
			defer func() {
				if p := panicx.From(recover()); p != nil {
					mu.Lock()
					pan = p
					mu.Unlock()
				}
			}()
			for i := int(next.Add(1)) - 1; i < len(keys); i = int(next.Add(1)) - 1 {
				kctx, cancel := context.WithTimeout(ctx, o.PerKey)
				err := t.Reconcile(kctx, keys[i])
				cancel()
				if err != nil {
					failed.Add(1)
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	if pan != nil {
		panic(pan) // a bug in a worker: re-raise in the caller (Cox-Buday ch. 4)
	}
	rep := ReconcileReport{Round: 1, Tried: len(keys), Failed: int(failed.Load()), Took: time.Since(start)}
	if first != nil {
		return rep, errs.Wrap(module, first, "%d of %d key(s) stay closed after reconcile; safe to retry", rep.Failed, rep.Tried)
	}
	return rep, nil
}

// ReconcileUntilDone runs ReconcileAll rounds until one leaves no key closed, waiting a jittered, growing
// backoff between rounds (Release It H1: retried in the background; the agent serves meanwhile). report, if
// set, gets every round's report and error on this goroutine.
//
//	post: err == nil ⇒ the last round reconciled every key; otherwise err wraps ctx.Err()
func (t *Table) ReconcileUntilDone(ctx context.Context, o ReconcileOptions, report func(ReconcileReport, error)) error {
	o.normalize()
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	var prev time.Duration
	for round := 1; ; round++ {
		rep, err := t.ReconcileAll(ctx, o)
		rep.Round = round
		if report != nil {
			report(rep, err)
		}
		if err == nil {
			return nil
		}
		prev = o.Backoff.Delay(rng, min(round+1, retry.MaxAttemptsCap), prev)
		select {
		case <-ctx.Done():
			return errs.Wrap(module, ctx.Err(), "reconcile stopped after %d round(s); %d key(s) stay closed", round, rep.Failed)
		case <-time.After(prev):
		}
	}
}
