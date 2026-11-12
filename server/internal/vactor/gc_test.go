package vactor_test

import (
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// Owner-side retention keeps the bucket flat: after every write-back the key holds its
// current generation and the K = 2 before it, and nothing else, however many write-backs ran.
func TestRetentionKeepsBucketFlat(t *testing.T) {
	r := newDurable(t, nil)
	var pub []string
	for i := range 20 {
		r.put("k", fmt.Sprint("v", i))
		res, err := r.h.WriteBack(ctx, "k")
		if err != nil || !res.Wrote {
			t.Fatalf("write-back %d = %+v, %v", i, res, err)
		}
		m, _ := r.s.Manifest("k")
		pub = append(pub, m.Object)
		if names, want := r.s.Names("k"), pub[max(0, len(pub)-3):]; !slices.Equal(names, want) {
			t.Fatalf("after write-back %d the store holds %q, want %q", i, names, want)
		}
	}
	if st := r.h.Stats(); st.GenerationsDeleted != 17 || st.RetentionFailures != 0 {
		t.Fatalf("stats %+v, want 17 deleted", st)
	}
	// A restart forgets the history: the new owner deletes only what it published, never older ones.
	if err := r.h.Close(); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.put("k", "after restart")
	if _, err := r.h.WriteBack(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if n := len(r.s.Names("k")); n != 4 {
		t.Fatalf("%d objects after a restart and a write-back, want 4 (2 kept + the old current + the new one)", n)
	}
}

// K at its boundary (Options.GC.Keep): K = 0 keeps only the current generation.
func TestRetentionKeepZero(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) { o.GC.Keep = -1 })
	for i := range 3 {
		r.put("k", fmt.Sprint("v", i))
		if _, err := r.h.WriteBack(ctx, "k"); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := r.s.Manifest("k")
	if names := r.s.Names("k"); !slices.Equal(names, []string{m.Object}) {
		t.Fatalf("store holds %q, want only the current %s", names, m.Object)
	}
}

// A failed retention delete does not fail the write-back (it is published); the object is left for the
// orphan sweeper, which deletes it once the manifest moved on and the grace window passed.
func TestRetentionFailureLeavesItToTheSweeper(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) { o.GC.Keep = -1 })
	var tick atomic.Int64
	r.s.SetClock(func() time.Time { return time.Unix(0, tick.Add(int64(time.Second))) })
	r.put("k", "v1")
	if _, err := r.h.WriteBack(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	first, _ := r.s.Manifest("k")
	r.s.SetFaults(durabletest.Faults{FailDelete: true})
	r.put("k", "v2")
	if res, err := r.h.WriteBack(ctx, "k"); err != nil || !res.Wrote {
		t.Fatalf("write-back with failing deletes = %+v, %v; want published", res, err)
	}
	if st := r.h.Stats(); st.RetentionFailures != 1 || st.GenerationsDeleted != 0 {
		t.Fatalf("stats %+v", st)
	}
	if _, ok := r.s.Object(first.Object); !ok {
		t.Fatal("the failed delete removed the object")
	}
	r.s.SetFaults(durabletest.Faults{})
	res, err := durable.Sweep(ctx, r.s, "k", time.Unix(0, tick.Load()).Add(time.Hour), durable.GCPolicy{Keep: -1, Grace: time.Nanosecond}, false)
	if err != nil || !slices.Equal(res.Deleted, []string{first.Object}) {
		t.Fatalf("Sweep = %+v, %v; want %s deleted", res, err, first.Object)
	}
	if got := r.published("k"); got != "v2" {
		t.Fatalf("published %q after the sweep", got)
	}
}

// GC racing an activation: a sweep that runs while a cold key is activating (manifest read, fetch in
// flight) never deletes the generation the activation read, so the activation succeeds; older ones go.
func TestSweepRacingActivation(t *testing.T) {
	r := newDurable(t, nil)
	var tick atomic.Int64
	r.s.SetClock(func() time.Time { return time.Unix(0, tick.Add(int64(time.Second))) })
	for i := range 4 {
		r.s.Seed("k", 0, []byte(fmt.Sprint("g", i)))
	}
	r.s.SetFaults(durabletest.Faults{Delay: 50 * time.Millisecond}) // every call, the activation's too
	done := make(chan error, 1)
	go func() { _, err := r.h.Get(ctx, "k"); done <- err }()
	res, err := durable.Sweep(ctx, r.s, "k", time.Unix(0, tick.Load()).Add(time.Hour), durable.GCPolicy{Keep: -1, Grace: time.Nanosecond}, false)
	if err != nil || len(res.Deleted) != 3 {
		t.Fatalf("Sweep = %+v, %v; want the 3 old generations deleted", res, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("activation racing the sweep: %v", err)
	}
	if got := r.local("k"); got != "g3" {
		t.Fatalf("activated %q, want g3", got)
	}
}
