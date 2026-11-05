package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/retry"
)

// The one retry layer for S3 (Release It R1, object-storage guide R2). minio-go's own retries are off
// (Config.MaxRetries 1), so each attempt below is one HTTP request. Only idempotent requests come through
// retryIdempotent; conditional PUTs are sent once and an ambiguous outcome is left to the caller to resolve by
// re-reading.

// idempotentRetry is the backoff for GET, HEAD, LIST and DELETE: 100 ms ×2 with full jitter, 5 s cap, 5
// attempts in all.
var idempotentRetry = func() retry.Policy {
	p := retry.Policy{MaxAttempts: 5, Backoff: retry.Exponential, Base: 100 * time.Millisecond, Max: 5 * time.Second,
		Jitter: retry.Full}
	if err := p.Normalize(); err != nil {
		panic(err) // constant input
	}
	return p
}()

// retryIdempotent runs fn until it succeeds, fails with a non-Transient error, runs out of attempts, or the
// next backoff would end past ctx's deadline (never retry past the caller's deadline, T4).
//
//	pre:  fn is idempotent: running it twice has the effect of running it once
//	post: the error returned is fn's last error
func (s *Store) retryIdempotent(ctx context.Context, op, name string, fn func(context.Context) error) error {
	var rng *rand.Rand
	var prev time.Duration
	for n := 1; ; n++ {
		t0 := time.Now()
		err := fn(ctx)
		kind := Classify(err)
		if err == nil || kind != Transient || n == idempotentRetry.MaxAttempts {
			s.observe(op, name, kind, time.Since(t0), false, err)
			return err
		}
		if rng == nil {
			rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
		}
		d := idempotentRetry.Delay(rng, n+1, prev)
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= d {
			s.observe(op, name, kind, time.Since(t0), false, err)
			return err
		}
		s.observe(op, name, kind, time.Since(t0), true, err)
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return err
		}
		prev = d
	}
}

// sendOnce runs a non-idempotent request (a PUT) exactly once.
func (s *Store) sendOnce(ctx context.Context, op, name string, fn func(context.Context) error) error {
	t0 := time.Now()
	err := fn(ctx)
	s.observe(op, name, Classify(err), time.Since(t0), false, err)
	return err
}

// attempt is one request's budget (Release It T1, T3): a deadline from its start, which a transfer can
// extend once it knows its size, and stall timers on the bodies it moves. Its context ends when either
// fires, with an ErrTimeout cause; the caller's own context ending stays Canceled.
type attempt struct {
	parent   context.Context
	ctx      context.Context
	cancel   context.CancelCauseFunc
	start    time.Time
	deadline *time.Timer
	stalls   []*stallReader
}

// begin starts an attempt that must finish within d.
func (s *Store) begin(parent context.Context, d time.Duration) *attempt {
	ctx, cancel := context.WithCancelCause(parent)
	a := &attempt{parent: parent, ctx: ctx, cancel: cancel, start: time.Now()}
	a.deadline = time.AfterFunc(d, func() { cancel(fmt.Errorf("%w: no answer within %s", ErrTimeout, d)) })
	return a
}

// within moves the attempt's deadline to total after its start (a transfer that now knows its size).
func (a *attempt) within(total time.Duration) {
	if a.deadline.Stop() { // not fired yet
		d := total - time.Since(a.start)
		a.deadline = time.AfterFunc(d, func() {
			a.cancel(fmt.Errorf("%w: transfer not done within %s", ErrTimeout, total))
		})
	}
}

// rearm restarts the deadline at d from now (a listing that just received a page).
func (a *attempt) rearm(d time.Duration) {
	if a.deadline.Stop() {
		a.deadline = time.AfterFunc(d, func() { a.cancel(fmt.Errorf("%w: no progress within %s", ErrTimeout, d)) })
	}
}

// watch returns r with a stall detector: the attempt ends if no byte moves through r for d, counted from
// the first Read (an upload's body is first read only once the connection is ready). The detector stops at
// EOF or once size bytes (size >= 0) went through, so a request body that was fully sent does not stall
// while the server answers (ResponseHeaderTimeout bounds that wait).
func (a *attempt) watch(r io.Reader, size int64, d time.Duration) io.Reader {
	sr := &stallReader{r: r, d: d, left: size, stall: func() { a.cancel(fmt.Errorf("%w: no bytes for %s", ErrTimeout, d)) }}
	a.stalls = append(a.stalls, sr)
	return sr
}

// end releases the attempt and returns err, joined with the timeout cause if this attempt's own deadline
// or a stall (not the caller) ended it.
func (a *attempt) end(err error) error {
	a.deadline.Stop()
	for _, sr := range a.stalls {
		sr.stop()
	}
	cause := context.Cause(a.ctx)
	a.cancel(nil)
	if err != nil && a.parent.Err() == nil && errors.Is(cause, ErrTimeout) && !errors.Is(err, ErrTimeout) {
		return errors.Join(cause, err)
	}
	return err
}

// stallReader is read by the HTTP transport's goroutine for uploads, so its timer is guarded.
type stallReader struct {
	r     io.Reader
	d     time.Duration
	stall func()

	mu   sync.Mutex
	t    *time.Timer // nil until the first Read
	left int64       // bytes still expected; < 0 unknown
	done bool
}

func (s *stallReader) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.t == nil && !s.done {
		s.t = time.AfterFunc(s.d, s.stall)
	}
	s.mu.Unlock()
	n, err := s.r.Read(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.left > 0 {
		s.left = max(s.left-int64(n), 0)
	}
	switch {
	case s.done:
	case err == io.EOF || s.left == 0:
		s.done = true
		s.t.Stop()
	case n > 0:
		s.t.Reset(s.d)
	}
	return n, err
}

func (s *stallReader) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	if s.t != nil {
		s.t.Stop()
	}
}

// transferTime is the size-derived budget for moving size bytes: OpTimeout + size/MinThroughput (T3).
func (s *Store) transferTime(size int64) time.Duration {
	return s.cfg.OpTimeout + time.Duration(float64(max(size, 0))/float64(s.cfg.MinThroughput)*float64(time.Second))
}
