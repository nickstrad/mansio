package durable_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
)

// PlanGC spec table (OS-06 acceptance): on and off points at K and at both grace bounds.
func TestPlanGC(t *testing.T) {
	const key = "t/a"
	const grace = 10 * time.Minute
	mod := t0                // the manifest's LastModified
	now := t0.Add(time.Hour) // far past every grace window unless a case says otherwise
	cur := durable.NextManifest(key, 10, 3, dg, t0)
	old := mod.Add(-grace - time.Second) // uploaded long before the manifest was rewritten
	obj := func(gen, epoch uint64, at time.Time) durable.StoredObject {
		return durable.StoredObject{Name: durable.GenObject(key, gen, epoch), Size: 1, Modified: at}
	}
	name := func(gen, epoch uint64) string { return durable.GenObject(key, gen, epoch) }
	below := []durable.StoredObject{obj(7, 1, old), obj(8, 1, old), obj(9, 2, old), obj(10, 3, old)}

	for _, tc := range []struct {
		name string
		m    durable.Manifest
		objs []durable.StoredObject
		now  time.Time
		keep int
		want []string
	}{
		{"K=2 keeps the current and 2 below; the 3rd below goes (on point)", cur, below, now, 2, []string{name(7, 1)}},
		{"K=3 keeps all (off point)", cur, below, now, 3, nil},
		{"K=1 deletes the 2nd and 3rd below", cur, below, now, 1, []string{name(7, 1), name(8, 1)}},
		{"K=0 keeps only the current one", cur, below, now, -1, []string{name(7, 1), name(8, 1), name(9, 2)}},
		{"the current one is kept even when it is all there is", cur, []durable.StoredObject{obj(10, 3, old)}, now, -1, nil},
		{"two epochs of one number count as two below", cur, []durable.StoredObject{obj(9, 1, old), obj(9, 2, old), obj(10, 3, old)}, now, 1,
			[]string{name(9, 1)}},
		{"an orphan above the current one goes once old", cur, []durable.StoredObject{obj(11, 3, old)}, now, 2, []string{name(11, 3)}},
		{"upload + grace just before the manifest's rewrite: deleted (on point)", cur,
			[]durable.StoredObject{obj(11, 3, mod.Add(-grace-time.Nanosecond))}, now, 2, []string{name(11, 3)}},
		{"upload + grace exactly at the rewrite: kept (off point)", cur,
			[]durable.StoredObject{obj(11, 3, mod.Add(-grace))}, now, 2, nil},
		{"uploaded after the manifest's last rewrite: kept", cur, []durable.StoredObject{obj(11, 3, mod.Add(time.Second))}, now.Add(time.Hour), 2, nil},
		{"older than grace now (on point): deleted", cur, []durable.StoredObject{obj(11, 3, old)}, old.Add(grace + time.Nanosecond), 2,
			[]string{name(11, 3)}},
		{"exactly grace old now (off point): kept", cur, []durable.StoredObject{obj(11, 3, old)}, old.Add(grace), 2, nil},
		{"no LastModified: kept", cur, []durable.StoredObject{obj(11, 3, time.Time{})}, now, 2, nil},
		{"names that are not this key's generations: kept", cur, []durable.StoredObject{
			{Name: durable.GenPrefix(key) + "x.db", Modified: old},
			{Name: durable.GenPrefix(key) + "00000000000000000011-e00000000000000000003.db/wal/1", Modified: old},
			{Name: durable.GenObject("t/b", 1, 1), Modified: old},
			{Name: durable.ManifestObject(key), Modified: old}}, now, 2, nil},
		{"a generation-0 manifest: every old object is an orphan", durable.Manifest{Key: key, Epoch: 4},
			[]durable.StoredObject{obj(1, 1, old), obj(2, 4, old)}, now, 2, []string{name(1, 1), name(2, 4)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := durable.PlanGC(tc.m, mod, tc.objs, tc.now, durable.GCPolicy{Keep: tc.keep, Grace: grace}.Normalize())
			if !slices.Equal(got, tc.want) {
				t.Fatalf("PlanGC = %q, want %q", got, tc.want)
			}
		})
	}
}

// PlanRetention keeps the current published object and K before it (guide GC2), at the K boundary.
func TestPlanRetention(t *testing.T) {
	pub := []string{"g1", "g2", "g3", "g4"}
	for _, tc := range []struct {
		pub        []string
		keep       int
		wantKeep   []string
		wantDelete []string
	}{
		{nil, 2, nil, nil},
		{pub[:1], 2, pub[:1], nil},
		{pub[:3], 2, pub[:3], nil},                 // exactly K+1: nothing to drop (off point)
		{pub, 2, pub[1:], pub[:1]},                 // K+2: the oldest goes (on point)
		{pub, -1, pub[3:], pub[:3]},                // K = 0
		{pub, 5, pub, nil},                         // K beyond the history
		{pub, 0 /* default 2 */, pub[1:], pub[:1]}, // zero means the default
	} {
		keep, drop := durable.PlanRetention(tc.pub, durable.GCPolicy{Keep: tc.keep}.Normalize())
		if !slices.Equal(keep, tc.wantKeep) || !slices.Equal(drop, tc.wantDelete) {
			t.Errorf("PlanRetention(%q, K=%d) = %q, %q; want %q, %q", tc.pub, tc.keep, keep, drop, tc.wantKeep, tc.wantDelete)
		}
	}
}

func TestGCPolicyNormalize(t *testing.T) {
	for _, tc := range []struct{ in, want durable.GCPolicy }{
		{durable.GCPolicy{}, durable.DefaultGCPolicy},
		{durable.GCPolicy{Keep: -1, Grace: -1}, durable.GCPolicy{Keep: -1, Grace: durable.DefaultGCPolicy.Grace}},
		{durable.GCPolicy{Keep: 1, Grace: time.Nanosecond}, durable.GCPolicy{Keep: 1, Grace: time.Nanosecond}},
	} {
		if got := tc.in.Normalize(); got != tc.want || got.Normalize() != got {
			t.Errorf("Normalize(%+v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseGenObject(t *testing.T) {
	const key = "t/a"
	for _, tc := range []struct {
		name      string
		gen, ep   uint64
		ok        bool
		different string
	}{
		{name: durable.GenObject(key, 1, 0), gen: 1, ep: 0, ok: true},
		{name: durable.GenObject(key, 1<<64-1, 1<<64-1), gen: 1<<64 - 1, ep: 1<<64 - 1, ok: true},
		{name: durable.GenObject("t/b", 1, 1)},
		{name: durable.GenPrefix(key) + "00000000000000000001-e00000000000000000001.db.part"},
		{name: durable.GenPrefix(key) + "00000000000000000001-x00000000000000000001.db"},
		{name: durable.GenPrefix(key) + "+0000000000000000001-e00000000000000000001.db"},
		{name: durable.GenPrefix(key) + "99999999999999999999-e00000000000000000001.db"}, // overflows uint64
		{name: durable.ManifestObject(key)},
	} {
		g, e, ok := durable.ParseGenObject(key, tc.name)
		if ok != tc.ok || g != tc.gen || e != tc.ep {
			t.Errorf("ParseGenObject(%q) = %d, %d, %v; want %d, %d, %v", tc.name, g, e, ok, tc.gen, tc.ep, tc.ok)
		}
	}
}

func TestShards(t *testing.T) {
	sh := durable.Shards()
	if len(sh) != 256 || sh[0] != "00" || sh[255] != "ff" || !slices.IsSorted(sh) {
		t.Fatalf("Shards = %d values, %q … %q", len(sh), sh[0], sh[len(sh)-1])
	}
	for _, s := range []string{"0", "000", "FF", "g0", ""} {
		if durable.ValidShard(s) {
			t.Errorf("ValidShard(%q) = true", s)
		}
	}
	if !durable.ValidShard(durable.KeyPrefix("x")[4:6]) {
		t.Error("a KeyPrefix shard is not valid")
	}
}

// Sweep leaves a key alone when the manifest moved between the listing and the read, or when the key has
// no manifest (dropping is not GC: GC5a); a failed delete is reported, not hidden.
func TestSweepSkipsAndFailures(t *testing.T) {
	s := durabletest.NewStore()
	clk := t0
	s.SetClock(func() time.Time { return clk })
	p := durable.GCPolicy{Keep: 1, Grace: time.Minute}
	m1 := s.Seed("k", 1, []byte("g1"))
	clk = clk.Add(time.Hour)
	s.Seed("k", 1, []byte("g2"))
	clk = clk.Add(time.Hour)
	m3 := s.Seed("k", 1, []byte("g3"))
	later := clk.Add(time.Hour)

	if r, err := durable.Sweep(ctx, s, "none", later, p, false); err != nil || r.Skipped != "no manifest" {
		t.Fatalf("Sweep of a key with no manifest = %+v, %v", r, err)
	}
	s.SetFaults(durabletest.Faults{FailList: true})
	if _, err := durable.Sweep(ctx, s, "k", later, p, false); err == nil {
		t.Fatal("Sweep with a failing listing succeeded")
	}
	s.SetFaults(durabletest.Faults{FailDelete: true})
	if _, err := durable.Sweep(ctx, s, "k", later, durable.GCPolicy{Keep: -1, Grace: time.Minute}, false); err == nil {
		t.Fatal("Sweep with a failing delete succeeded")
	}
	s.SetFaults(durabletest.Faults{})
	if r, err := durable.Sweep(ctx, s, "k", later, p, false); err != nil || len(r.Deleted) != 0 {
		t.Fatalf("Sweep = %+v, %v; g1 was already deleted by the partial delete", r, err)
	}
	if _, ok := s.Object(m1.Object); ok {
		t.Fatal("g1 survived")
	}
	if _, ok := s.Object(m3.Object); !ok {
		t.Fatal("the current generation was deleted")
	}

	moved := &movingStore{Store: s}
	if r, err := durable.Sweep(ctx, moved, "k", later, p, false); err != nil || r.Skipped != "manifest moved" || len(r.Deleted) != 0 {
		t.Fatalf("Sweep across a manifest move = %+v, %v", r, err)
	}
}

// movingStore publishes a new manifest between the listing and the read, as a writer racing the sweeper.
type movingStore struct{ *durabletest.Store }

func (m *movingStore) ListKey(c context.Context, key string) (durable.Listing, error) {
	l, err := m.Store.ListKey(c, key)
	m.Store.Bump(key, 7)
	return l, err
}

// K = 0 survives normalizing twice (dbtool normalizes, then Sweep does): only the current one stays.
func TestSweepKeepZeroAfterNormalize(t *testing.T) {
	s := durabletest.NewStore()
	clk := t0
	s.SetClock(func() time.Time { clk = clk.Add(time.Hour); return clk })
	for i := range 3 {
		s.Seed("k", 1, []byte{byte(i)})
	}
	p := durable.GCPolicy{Keep: -1, Grace: time.Minute}.Normalize()
	r, err := durable.Sweep(ctx, s, "k", clk.Add(time.Hour), p, false)
	if err != nil || len(r.Deleted) != 2 {
		t.Fatalf("Sweep with K = 0 = %+v, %v; want 2 deleted", r, err)
	}
}
