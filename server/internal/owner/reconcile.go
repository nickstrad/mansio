package owner

import (
	"cmp"
	"context"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/parallel"
	"github.com/nickstrad/mansio/server/internal/retry"
)

// ReconcileOptions bound a reconcile round (startup reconciles with bounded concurrency and a
// per-key timeout, so one slow authority row cannot delay every key). Zero fields take the defaults.
type ReconcileOptions struct {
	Workers int           // keys reconciled at once; default 16
	PerKey  time.Duration // deadline for one key (authority read, manifest read, record write); default 5 s
	// Backoff between rounds of ReconcileUntilDone: exponential from Base to Max, equal jitter. Defaults
	// 100 ms and 5 s.
	Backoff retry.Policy
	Clock   det.Clock // per-key deadlines, round timing and the backoff wait; default det.System()
	Rand    det.Rand  // backoff jitter; default det.SystemRand()
}

func (o *ReconcileOptions) normalize() {
	if o.Workers <= 0 {
		o.Workers = 16
	}
	if o.PerKey <= 0 {
		o.PerKey = 5 * time.Second
	}
	o.Backoff.Backoff, o.Backoff.Jitter = retry.Exponential, retry.Equal
	if o.Backoff.Base <= 0 {
		o.Backoff.Base = 100 * time.Millisecond
	}
	if o.Backoff.Max <= 0 {
		o.Backoff.Max = 5 * time.Second
	}
	_ = o.Backoff.Normalize() // cannot fail: every field it checks is set above
	o.Clock, o.Rand = det.Or(o.Clock), det.RandOr(o.Rand)
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
	start := o.Clock.Now()
	var keys []string
	for _, k := range t.Keys() {
		if t.State(k).State == Closed {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys) // not map order: with few workers the order decides which keys an outage hits (DST replay)
	var first error
	failed := 0
	for _, err := range parallel.ForEach(ctx, len(keys), o.Workers, func(ctx context.Context, i int) error {
		kctx, cancel := det.WithTimeout(ctx, o.Clock, o.PerKey)
		defer cancel()
		return t.Reconcile(kctx, keys[i])
	}) {
		if err != nil {
			failed++
			first = cmp.Or(first, err)
		}
	}
	rep := ReconcileReport{Round: 1, Tried: len(keys), Failed: failed, Took: det.Since(o.Clock, start)}
	if first != nil {
		return rep, errs.Wrap(module, first, "%d of %d key(s) stay closed after reconcile; safe to retry", rep.Failed, rep.Tried)
	}
	return rep, nil
}

// ReconcileUntilDone runs ReconcileAll rounds until one leaves no key closed, waiting a jittered, growing
// backoff between rounds (retried in the background; the agent serves meanwhile). report, if
// set, gets every round's report and error on this goroutine.
//
//	post: err == nil ⇒ the last round reconciled every key; otherwise err wraps ctx.Err()
func (t *Table) ReconcileUntilDone(ctx context.Context, o ReconcileOptions, report func(ReconcileReport, error)) error {
	o.normalize()
	rng := rand.New(o.Rand)
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
		prev = o.Backoff.Delay(rng, round+1, prev)
		if err := det.Sleep(ctx, o.Clock, prev); err != nil {
			return errs.Wrap(module, err, "reconcile stopped after %d round(s); %d key(s) stay closed", round, rep.Failed)
		}
	}
}
