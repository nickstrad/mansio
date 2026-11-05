package vactor_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/faultnet"
	"github.com/nickstrad/ts-db-go/internal/objstore"
	"github.com/nickstrad/ts-db-go/internal/objstore/s3test"
	"github.com/nickstrad/ts-db-go/internal/vactor"
	"github.com/nickstrad/ts-db-go/internal/vactor/vactortest"
)

// s3Transient is the S3 breaker classifier: only Transient errors (timeouts, connection errors, 5xx, 429)
// are system failures (Release It C2).
func s3Transient(err error) bool { return objstore.Classify(err) == objstore.Transient }

// faultRig is a durable host over the real S3 adapter, reaching the in-process misbehaving S3 (s3test)
// through a faultnet proxy, with an S3 breaker; host and breaker share a fake clock.
type faultRig struct {
	h     *vactor.Host
	fake  *s3test.Server
	proxy *faultnet.Proxy
	b     *breaker.Breaker
	clk   *clock
}

func newFaultRig(t *testing.T, mod func(*vactor.Options)) *faultRig {
	t.Helper()
	fake := s3test.New()
	t.Cleanup(fake.Close)
	proxy, err := faultnet.New(fake.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxy.Close() })
	st, err := objstore.New(objstore.Config{Endpoint: proxy.Addr(), AccessKey: "a", SecretKey: "b", Bucket: "bkt",
		OpTimeout: 100 * time.Millisecond, StallTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	b, err := breaker.New(breaker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	opts := vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), CreateMissing: true, Now: clk.Now,
		Store: durable.WithBreaker(objstore.NewDurable(st, ""), b, s3Transient, clk.Now)}
	if mod != nil {
		mod(&opts)
	}
	h, err := vactor.NewHost(nil, &vactortest.Opener{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fake.Heal(); proxy.Heal(); h.Close() })
	return &faultRig{h: h, fake: fake, proxy: proxy, b: b, clk: clk}
}

// seed writes key's first generation and passivates it, so the next Get is a cold activation.
func (r *faultRig) seed(t *testing.T, key string) {
	t.Helper()
	res, err := r.h.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.DB.Query(ctx, vactortest.Put("v-"+key)); err != nil {
		t.Fatal(err)
	}
	if err := r.h.Passivate(ctx, key); err != nil {
		t.Fatal(err)
	}
}

// Guide H4 rows "half-open" and "headers, then stall" for activation: a cold Get fails by the activation
// deadline (not later) as unavailable (ErrBusy, or ErrUnavailable when objstore's own stall timeout fires
// first), leaves no file, and the key activates once the network heals and its backoff ends (plan RI-02,
// RI-03). faultnet's Frozen also freezes the pooled connection the seed step left open.
func TestActivationDeadlineThroughS3Faults(t *testing.T) {
	const timeout = 300 * time.Millisecond
	for _, tc := range []struct {
		name   string
		inject func(r *faultRig)
	}{
		{"faultnet half-open", func(r *faultRig) { r.proxy.Set(faultnet.Frozen(), 0) }},
		{"s3test headers then stall", func(r *faultRig) {
			r.fake.Inject(s3test.Fault{Mode: s3test.HeadersThenStall, Method: "GET"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFaultRig(t, func(o *vactor.Options) { o.ActivationTimeout = timeout })
			r.seed(t, "t/a")
			tc.inject(r)
			t0 := time.Now()
			_, err := r.h.Get(ctx, "t/a")
			took := time.Since(t0)
			if !errors.Is(err, vactor.ErrBusy) && !errors.Is(err, durable.ErrUnavailable) {
				t.Fatalf("Get = %v, want ErrBusy or durable.ErrUnavailable", err)
			}
			if took > timeout+200*time.Millisecond {
				t.Fatalf("Get took %v, want ≈ %v", took, timeout)
			}
			if exists(r.h.LocalPath("t/a")) || exists(r.h.LocalPath("t/a")+".part") {
				t.Fatal("a timed-out activation left a file")
			}
			r.fake.Heal()
			r.proxy.Heal()
			r.clk.Add(10 * time.Second) // past the key's backoff
			if _, err := r.h.Get(ctx, "t/a"); err != nil {
				t.Fatalf("Get after the network healed: %v", err)
			}
		})
	}
}

// Guide H4 row "5xx / throttling" for the breaker: under a 503 storm, 100 cold keys each cost S3 at most
// one retried call before the breaker opens; 1,000 more requests then fail fast without reaching S3, all as
// ErrUnavailable (503 on the wire); after the storm and one cool-down, every key activates.
func TestBreakerUnder503Storm(t *testing.T) {
	const keys = 100
	r := newFaultRig(t, nil)
	r.fake.Inject(s3test.Fault{Mode: s3test.Status, Status: 503})

	var wg sync.WaitGroup
	errc := make(chan error, keys)
	for k := range keys {
		wg.Go(func() { _, err := r.h.Get(ctx, fmt.Sprintf("t/%d", k)); errc <- err })
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if !errors.Is(err, durable.ErrUnavailable) {
			t.Fatalf("Get under a 503 storm = %v, want durable.ErrUnavailable", err)
		}
	}
	if r.b.State(r.clk.Now()) != breaker.Open {
		t.Fatalf("breaker %s after %d failing keys", r.b.State(r.clk.Now()), keys)
	}
	first := r.fake.Count("GET")
	if first > keys*5 { // objstore's one retry layer: ≤ 5 attempts per idempotent call
		t.Fatalf("%d GETs for %d keys: more than one retry layer", first, keys)
	}

	for i := range 1000 {
		if _, err := r.h.Get(ctx, fmt.Sprintf("t/%d", i%keys)); !errors.Is(err, durable.ErrUnavailable) {
			t.Fatalf("request %d while open = %v", i, err)
		}
	}
	if more := r.fake.Count("GET") - first; more != 0 {
		t.Fatalf("%d more GETs reached S3 while the breaker was open", more)
	}
	t.Logf("503 storm: %d keys → %d GETs, then 1,000 requests → 0 GETs", keys, first)

	r.fake.Heal()
	r.clk.Add(5*time.Second + 5*time.Second) // one cool-down + MaxFailBackoff
	for k := range keys {
		if _, err := r.h.Get(ctx, fmt.Sprintf("t/%d", k)); err != nil {
			t.Fatalf("key %d after the storm: %v", k, err)
		}
	}
	if r.b.State(r.clk.Now()) != breaker.Closed {
		t.Fatal("breaker not closed after the heal")
	}
}
