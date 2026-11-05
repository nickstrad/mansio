package vactor_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
	"github.com/nickstrad/ts-db-go/internal/vactor"
)

// countingStore counts every call that reaches the store, failed ones included.
type countingStore struct {
	durable.Store
	n atomic.Int64
}

func (c *countingStore) ReadManifest(ctx context.Context, key string) (durable.Manifest, durable.Version, error) {
	c.n.Add(1)
	return c.Store.ReadManifest(ctx, key)
}

func (c *countingStore) FetchGeneration(ctx context.Context, m durable.Manifest, path string) (durable.Fetched, error) {
	c.n.Add(1)
	return c.Store.FetchGeneration(ctx, m, path)
}

// TestS3OutageCallsBounded (plan RI-03): the store is down for 10 s while 100 cold keys get 1,000 req/s.
// Without protection that is 10,000 store calls. With the breaker and per-key tombstones the store sees at
// most one call per key before the breaker opens plus one probe per cool-down; with tombstones alone, at
// most one call per key per backoff step. With the breaker, every failure is ErrUnavailable (503 on the
// wire). After the store heals, every key activates within one cool-down plus the longest backoff.
func TestS3OutageCallsBounded(t *testing.T) {
	const keys, requests, perRequest = 100, 10_000, time.Millisecond // 1,000 req/s for 10 s
	for _, tc := range []struct {
		name     string
		breaker  bool
		maxCalls int64
		wantOpen bool
	}{
		{"breaker and tombstones", true, keys + int64(10*time.Second/(5*time.Second)) + 1, true},
		{"tombstones only", false, keys * 11, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var count *countingStore
			r := newDurable(t, func(o *vactor.Options) {
				count = &countingStore{Store: o.Store}
				o.Store = count
				if tc.breaker {
					b, err := breaker.New(breaker.Config{}) // the S3 defaults: 50% of ≥ 20 calls in 10 s, cool-down 5 s
					if err != nil {
						t.Fatal(err)
					}
					o.Store = durable.WithBreaker(count, b, nil, o.Now)
				}
			})
			fake := r.s
			fake.SetFaults(durabletest.Faults{Down: true})

			sawOpen := false
			for i := range requests {
				_, err := r.h.Get(ctx, fmt.Sprint(i%keys))
				if err == nil {
					t.Fatalf("request %d succeeded against a down store", i)
				}
				if tc.breaker && !errors.Is(err, durable.ErrUnavailable) {
					t.Fatalf("request %d: %v; want durable.ErrUnavailable (503 on the wire)", i, err)
				}
				if !tc.breaker && !errors.Is(err, durabletest.ErrInjected) {
					t.Fatalf("request %d: %v; want the store's failure", i, err)
				}
				sawOpen = sawOpen || errors.Is(err, breaker.ErrOpen)
				r.clk.Add(perRequest)
			}
			calls := count.n.Load()
			t.Logf("%d requests over 10 s → %d store calls", requests, calls)
			if calls > tc.maxCalls {
				t.Fatalf("the store saw %d calls, want ≤ %d", calls, tc.maxCalls)
			}
			if sawOpen != tc.wantOpen {
				t.Fatalf("saw the breaker open: %v, want %v", sawOpen, tc.wantOpen)
			}

			fake.SetFaults(durabletest.Faults{})
			r.clk.Add(5*time.Second + 5*time.Second) // one cool-down + MaxFailBackoff
			for k := range keys {
				if _, err := r.h.Get(ctx, fmt.Sprint(k)); err != nil {
					t.Fatalf("key %d after the store healed: %v", k, err)
				}
			}
		})
	}
}

// Shedding and an open breaker are not the key's failures: no tombstone, the next Get tries at once.
func TestNoTombstoneForShedOrOpenBreaker(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) {
		b, _ := breaker.New(breaker.Config{MinCalls: 1, FailureRate: 1})
		o.Store = durable.WithBreaker(o.Store, b, nil, o.Now)
	})
	fake := r.s
	fake.SetFaults(durabletest.Faults{Down: true})
	if _, err := r.h.Get(ctx, "a"); !errors.Is(err, durable.ErrUnavailable) { // opens the breaker; "a" is tombstoned
		t.Fatal(err)
	}
	_, err := r.h.Get(ctx, "b")
	if !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("Get while open = %v, want breaker.ErrOpen", err)
	}
	var hint interface{ RetryAfter() time.Duration }
	if !errors.As(err, &hint) || hint.RetryAfter() <= 0 {
		t.Fatalf("no retry hint in %v", err)
	}
	fake.SetFaults(durabletest.Faults{})
	r.clk.Add(5 * time.Second) // the cool-down; "a"'s tombstone (≤ 100 ms) is over too
	if _, err := r.h.Get(ctx, "b"); err != nil {
		t.Fatalf("\"b\" was tombstoned by a breaker refusal: %v", err)
	}
}
