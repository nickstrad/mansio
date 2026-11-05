package durable_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
	"github.com/nickstrad/ts-db-go/internal/errs"
)

func newBreaker(t testing.TB) *breaker.Breaker {
	t.Helper()
	b, err := breaker.New(breaker.Config{MinCalls: 2, FailureRate: 0.5, CoolDown: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The guarded store keeps the Store contract.
func TestBreakerStoreContract(t *testing.T) {
	durabletest.RunStoreSuite(t, func(t *testing.T) durable.Store {
		return durable.WithBreaker(durabletest.NewStore(), newBreaker(t), nil, nil)
	})
}

// Spec table for the default classifier: answers are not failures; timeouts are.
func TestIsSystemFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmt.Errorf("x: %w", durable.ErrStale), false},
		{durable.ErrExists, false},
		{durable.ErrNoManifest, false},
		{durable.ErrNotFound, false},
		{durable.ErrCorrupt, false},
		{context.Canceled, true},                                // WithBreaker ignores it only when the caller's ctx ended
		{fmt.Errorf("get: %w", context.DeadlineExceeded), true}, // a store timeout
		{durabletest.ErrInjected, true},
		{errors.New("connection refused"), true},
	} {
		if got := durable.IsSystemFailure(tc.err); got != tc.want {
			t.Errorf("IsSystemFailure(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// A down store: system failures are marked ErrUnavailable and open the breaker; then calls fail fast without
// reaching the store, with a retry hint; answers do not count; after the cool-down one probe heals it.
func TestBreakerStoreOpensFailsFastAndHeals(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	fake := durabletest.NewStore()
	var trans []breaker.Transition
	b, _ := breaker.New(breaker.Config{MinCalls: 2, FailureRate: 0.5, CoolDown: time.Second,
		OnChange: func(tr breaker.Transition) { trans = append(trans, tr) }})
	s := durable.WithBreaker(fake, b, nil, clock)
	ctx := context.Background()

	// Answers (no manifest) are successes: many of them never open it.
	for range 10 {
		if _, _, err := s.ReadManifest(ctx, "k"); !errors.Is(err, durable.ErrNoManifest) || errors.Is(err, durable.ErrUnavailable) {
			t.Fatalf("an answer was changed: %v", err)
		}
	}
	if b.State(now) != breaker.Closed {
		t.Fatal("answers opened the breaker")
	}

	fake.SetFaults(durabletest.Faults{Down: true})
	now = now.Add(11 * time.Second) // a fresh window
	for range 2 {
		_, _, err := s.ReadManifest(ctx, "k")
		if !errors.Is(err, durable.ErrUnavailable) || !errors.Is(err, durabletest.ErrInjected) || err.Error() != errors.Unwrap(err).Error() {
			t.Fatalf("a system failure = %v; want ErrUnavailable with the cause and message kept", err)
		}
	}
	if b.State(now) != breaker.Open || len(trans) != 1 {
		t.Fatalf("state %s, transitions %+v", b.State(now), trans)
	}

	fake.SetFaults(durabletest.Faults{})
	dir := t.TempDir()
	_, err := s.FetchGeneration(ctx, durable.Manifest{Key: "k", Generation: 1, Object: "o"}, filepath.Join(dir, "f"))
	if !errors.Is(err, durable.ErrUnavailable) || !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("call while open = %v, want ErrUnavailable / breaker.ErrOpen", err)
	}
	var hint interface{ RetryAfter() time.Duration }
	if !errors.As(err, &hint) || hint.RetryAfter() != time.Second {
		t.Fatalf("no retry hint of 1 s in %v", err)
	}
	if e, ok := errs.Outermost(err); !ok || e.Module != "durable" {
		t.Fatalf("not a well-formed durable error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "f")); !os.IsNotExist(err) {
		t.Fatal("a refused fetch left a file")
	}
	if _, _, fetches := fake.Counts(); fetches != 0 {
		t.Fatal("a refused call reached the store")
	}

	now = now.Add(time.Second)
	if _, _, err := s.ReadManifest(ctx, "k"); !errors.Is(err, durable.ErrNoManifest) {
		t.Fatalf("probe after the cool-down: %v", err)
	}
	if b.State(now) != breaker.Closed {
		t.Fatalf("a good probe left the breaker %s", b.State(now))
	}
}

// A call whose own ctx ended is ignored: a caller giving up does not trip the breaker.
func TestBreakerStoreIgnoresCallerCancel(t *testing.T) {
	fake := durabletest.NewStore()
	fake.SetFaults(durabletest.Faults{Delay: time.Hour})
	b := newBreaker(t)
	s := durable.WithBreaker(fake, b, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 5 {
		if _, _, err := s.ReadManifest(ctx, "k"); !errors.Is(err, context.Canceled) || errors.Is(err, durable.ErrUnavailable) {
			t.Fatalf("cancelled call = %v", err)
		}
	}
	if b.State(time.Now()) != breaker.Closed {
		t.Fatal("cancelled calls opened the breaker")
	}
}

// A custom classifier decides what counts: here nothing does.
func TestBreakerStoreCustomClassifier(t *testing.T) {
	fake := durabletest.NewStore()
	fake.SetFaults(durabletest.Faults{Down: true})
	b := newBreaker(t)
	s := durable.WithBreaker(fake, b, func(error) bool { return false }, nil)
	for range 5 {
		if _, _, err := s.ReadManifest(context.Background(), "k"); errors.Is(err, durable.ErrUnavailable) {
			t.Fatalf("an error the classifier rejects was marked unavailable: %v", err)
		}
	}
	if b.State(time.Now()) != breaker.Closed {
		t.Fatal("the classifier was not used")
	}
}
