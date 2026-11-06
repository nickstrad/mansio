package vactor_test

import (
	"testing"

	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// OS-10: a re-warm of an idle database whose manifest has not moved is a local hit (1 manifest GET, no
// download), after a dirty passivation (the uploaded file is cached under its hash) and after a clean one.
func TestGenCacheRewarm(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) { o.GenCacheBytes = 1 << 20 })
	r.put("k", "v1")
	for round := range 3 { // dirty passivation, then clean ones
		if err := r.h.Passivate(ctx, "k"); err != nil {
			t.Fatal(err)
		}
		_, _, fetches := r.s.Counts()
		res, err := r.h.Get(ctx, "k")
		if err != nil || res.Act == nil || !res.Act.LocalHit {
			t.Fatalf("round %d: re-warm = %+v, %v; want a local hit", round, res.Act, err)
		}
		if _, _, f := r.s.Counts(); f != fetches {
			t.Fatalf("round %d: a cache hit downloaded", round)
		}
		if got := r.local("k"); got != "v1" {
			t.Fatalf("round %d: re-warmed %q", round, got)
		}
	}
	if st := r.h.Stats(); st.LocalHits != 3 {
		t.Fatalf("stats %+v", st)
	}
}

// The cache is trusted only when the manifest names its hash: after another writer published a new
// generation, the cached file is a miss and the new one is fetched.
func TestGenCacheMissWhenManifestMoved(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) { o.GenCacheBytes = 1 << 20 })
	r.put("k", "v1")
	if err := r.h.Passivate(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	r.s.Seed("k", 0, []byte("v2")) // a new generation published elsewhere (same epoch: a restart, a move back)
	res, err := r.h.Get(ctx, "k")
	if err != nil || res.Act.LocalHit {
		t.Fatalf("Get = %+v, %v; want a fetch", res.Act, err)
	}
	if got := r.local("k"); got != "v2" {
		t.Fatalf("activated %q, want v2", got)
	}
}

// Nothing is cached when it is off, when the passivation fails (the key stays warm), or when the file is not
// the published generation; and Discard never caches.
func TestGenCacheNotFilled(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		r := newDurable(t, nil)
		r.put("k", "v1")
		r.h.Passivate(ctx, "k")
		if res, err := r.h.Get(ctx, "k"); err != nil || res.Act.LocalHit {
			t.Fatalf("Get = %+v, %v", res.Act, err)
		}
	})
	t.Run("failed passivation", func(t *testing.T) {
		r := newDurable(t, func(o *vactor.Options) { o.GenCacheBytes = 1 << 20 })
		r.put("k", "v1")
		r.s.SetFaults(durabletest.Faults{FailUpload: true})
		if err := r.h.Passivate(ctx, "k"); err == nil {
			t.Fatal("Passivate with failing uploads succeeded")
		}
		r.s.SetFaults(durabletest.Faults{})
		if err := r.h.Passivate(ctx, "k"); err != nil {
			t.Fatal(err)
		}
		if res, err := r.h.Get(ctx, "k"); err != nil || !res.Act.LocalHit || r.local("k") != "v1" {
			t.Fatalf("Get after the retried passivation = %+v, %v", res.Act, err)
		}
	})
	t.Run("discard", func(t *testing.T) {
		r := newDurable(t, func(o *vactor.Options) { o.GenCacheBytes = 1 << 20 })
		r.put("k", "v1")
		r.h.WriteBack(ctx, "k")
		r.put("k", "v2")
		r.h.Discard("k")
		res, err := r.h.Get(ctx, "k")
		if err != nil || res.Act.LocalHit || r.local("k") != "v1" {
			t.Fatalf("Get after Discard = %+v, %v; want a fetch of v1", res.Act, err)
		}
	})
}
