package durable

import (
	"context"
	"errors"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/det"
	"github.com/nickstrad/ts-db-go/internal/errs"
)

// ErrUnavailable: the store cannot serve now. Either its circuit breaker is open (nothing was sent; the error
// carries a RetryAfter hint), or the call hit a system failure (timeout, connection error, 5xx; for a write
// the outcome may be ambiguous, as the cause says). Not an answer about the data: retry later (Release It
// C3, F2). agentd answers it 503 unavailable.
var ErrUnavailable = errors.New("durable: storage unavailable")

// IsSystemFailure is the default breaker classifier for WithBreaker: an error that says the store is failing,
// not an answer about the data (Release It C2): anything but the store's answers (ErrStale, ErrExists,
// ErrNoManifest, ErrNotFound, ErrCorrupt, ErrBadManifest). A timeout counts; a call whose caller's ctx ended
// is ignored by WithBreaker before any classifier runs. Adapters with a finer classification pass their own;
// the S3 one is objstore.Classify(err) == objstore.Transient.
func IsSystemFailure(err error) bool {
	if err == nil {
		return false
	}
	for _, answer := range []error{ErrStale, ErrExists, ErrNoManifest, ErrNotFound, ErrCorrupt, ErrBadManifest} {
		if errors.Is(err, answer) {
			return false
		}
	}
	return true
}

// WithBreaker returns s guarded by b: a call b refuses fails at once with ErrUnavailable (wrapping
// breaker.ErrOpen) and a RetryAfter hint, and a call that fails with a system failure (isFailure; nil means
// IsSystemFailure) counts against b and is marked ErrUnavailable, its chain kept. Answers count as successes;
// a call whose ctx ended (the caller gave up or its deadline passed) is ignored. now is the breaker's clock (nil: det.System().Now).
func WithBreaker(s Store, b *breaker.Breaker, isFailure func(error) bool, now func() time.Time) Store {
	if isFailure == nil {
		isFailure = IsSystemFailure
	}
	if now == nil {
		now = det.System().Now
	}
	return &guarded{s: s, b: b, isFailure: isFailure, now: now}
}

type guarded struct {
	s         Store
	b         *breaker.Breaker
	isFailure func(error) bool
	now       func() time.Time
}

// call runs one store call (op on name) through the breaker.
func (g *guarded) call(ctx context.Context, op, name string, fn func() error) error {
	p, wait, ok := g.b.Allow(g.now())
	if !ok {
		return errs.Wrap(module, &openError{wait: wait}, "cannot %s %s: the storage circuit breaker is open; nothing sent, retry after %v", op, name, wait)
	}
	err := fn()
	o := breaker.Success
	switch {
	case err == nil:
	case ctx.Err() != nil:
		o = breaker.Ignore // the caller gave up: says nothing about the store
	case g.isFailure(err):
		o = breaker.Failure
	}
	g.b.Done(p, g.now(), o)
	if o == breaker.Failure {
		return unavailable{err}
	}
	return err
}

func (g *guarded) ReadManifest(ctx context.Context, key string) (m Manifest, v Version, err error) {
	err = g.call(ctx, "read the manifest of", key, func() error { m, v, err = g.s.ReadManifest(ctx, key); return err })
	return m, v, err
}

func (g *guarded) PutGeneration(ctx context.Context, m Manifest, path string) error {
	return g.call(ctx, "upload", m.Object, func() error { return g.s.PutGeneration(ctx, m, path) })
}

func (g *guarded) StatGeneration(ctx context.Context, object string) (info GenInfo, err error) {
	err = g.call(ctx, "stat", object, func() error { info, err = g.s.StatGeneration(ctx, object); return err })
	return info, err
}

func (g *guarded) FetchGeneration(ctx context.Context, m Manifest, path string) (f Fetched, err error) {
	err = g.call(ctx, "fetch", m.Object, func() error { f, err = g.s.FetchGeneration(ctx, m, path); return err })
	return f, err
}

func (g *guarded) PublishManifest(ctx context.Context, m Manifest, prev Version) (v Version, err error) {
	err = g.call(ctx, "publish the manifest of", m.Key, func() error { v, err = g.s.PublishManifest(ctx, m, prev); return err })
	return v, err
}

func (g *guarded) DeleteGenerations(ctx context.Context, key string, names []string) error {
	if err := CheckDeletable(key, names); err != nil {
		return err // our own refusal says nothing about the store
	}
	return g.call(ctx, "delete generations of", key, func() error { return g.s.DeleteGenerations(ctx, key, names) })
}

// openError is a call the breaker refused.
type openError struct{ wait time.Duration }

func (e *openError) Error() string { return "storage circuit breaker open" }
func (e *openError) Is(target error) bool {
	return target == ErrUnavailable || target == breaker.ErrOpen
}
func (e *openError) RetryAfter() time.Duration { return e.wait } // front.RetryHint

// unavailable marks a system failure as ErrUnavailable without changing its message or chain.
type unavailable struct{ err error }

func (e unavailable) Error() string        { return e.err.Error() }
func (e unavailable) Unwrap() error        { return e.err }
func (e unavailable) Is(target error) bool { return target == ErrUnavailable }
