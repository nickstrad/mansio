package durabletest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable"
)

// NewStoreFunc returns an empty Store under test (a fresh namespace per call).
type NewStoreFunc func(t *testing.T) durable.Store

// RunStoreSuite checks the durable.Store contract. Run it against every implementation, real and fake: a
// unit test against the fake means something only if the real adapter passes the same suite.
func RunStoreSuite(t *testing.T, newStore NewStoreFunc) {
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	file := func(t *testing.T, data []byte) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "snap.db")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	putAt := func(t *testing.T, s durable.Store, key string, gen, epoch uint64, data []byte) durable.Manifest {
		t.Helper()
		m := durable.NextManifest(key, gen, epoch, DigestOf(data), now)
		if err := s.PutGeneration(ctx, m, file(t, data)); err != nil {
			t.Fatalf("PutGeneration(%s): %v", m.Object, err)
		}
		return m
	}
	put := func(t *testing.T, s durable.Store, key string, gen uint64, data []byte) durable.Manifest {
		t.Helper()
		return putAt(t, s, key, gen, 1, data)
	}
	publish := func(t *testing.T, s durable.Store, m durable.Manifest, prev durable.Version) durable.Version {
		t.Helper()
		v, err := s.PublishManifest(ctx, m, prev)
		if err != nil {
			t.Fatalf("PublishManifest(%+v, %q): %v", m, prev, err)
		}
		if v == durable.NoVersion || v == prev {
			t.Fatalf("PublishManifest returned version %q (prev %q)", v, prev)
		}
		return v
	}
	noFile := func(t *testing.T, path string) {
		t.Helper()
		for _, p := range []string{path, path + ".part"} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s exists after a failed fetch (stat err %v)", p, err)
			}
		}
	}

	t.Run("missing manifest is ErrNoManifest", func(t *testing.T) {
		s := newStore(t)
		if _, _, err := s.ReadManifest(ctx, "suite/none"); !errors.Is(err, durable.ErrNoManifest) {
			t.Fatalf("err = %v, want ErrNoManifest", err)
		}
	})

	for _, tc := range []struct {
		name string
		size int
	}{{"empty", 0}, {"one byte", 1}, {"just under 1 MiB", 1<<20 - 1}, {"exactly 1 MiB", 1 << 20}, {"just over 1 MiB", 1<<20 + 1}} {
		t.Run("generation round trip: "+tc.name, func(t *testing.T) {
			s := newStore(t)
			data := make([]byte, tc.size)
			for i := range data {
				data[i] = byte(i*7 + 3)
			}
			m := put(t, s, "suite/rt", 1, data)
			path := filepath.Join(t.TempDir(), "db")
			f, err := s.FetchGeneration(ctx, m, path)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			if !bytes.Equal(got, data) || f.Bytes != int64(len(data)) {
				t.Fatalf("fetched %d bytes (Bytes %d), want %d", len(got), f.Bytes, len(data))
			}
			if f.TTFB < 0 || f.TTFB > f.Total {
				t.Fatalf("TTFB %v outside [0, Total %v]", f.TTFB, f.Total)
			}
			if _, err := os.Stat(path + ".part"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fetch left a .part file")
			}
		})
	}

	t.Run("generations are create-only", func(t *testing.T) {
		s := newStore(t)
		m := put(t, s, "suite/once", 1, []byte("first"))
		err := s.PutGeneration(ctx, durable.NextManifest("suite/once", 1, 1, DigestOf([]byte("second")), now), file(t, []byte("second")))
		if !errors.Is(err, durable.ErrExists) {
			t.Fatalf("second PutGeneration err = %v, want ErrExists", err)
		}
		path := filepath.Join(t.TempDir(), "db")
		if _, err := s.FetchGeneration(ctx, m, path); err != nil {
			t.Fatalf("first generation no longer fetches: %v", err)
		}
		if got, _ := os.ReadFile(path); string(got) != "first" {
			t.Fatalf("generation 1 = %q, want the first upload", got)
		}
	})

	t.Run("stat returns the upload's metadata; a duplicate upload is recognized by its hash", func(t *testing.T) {
		s := newStore(t)
		key := "suite/stat é+%" // non-ASCII and escape characters survive the metadata round trip
		m := putAt(t, s, key, 1, 3, []byte("ours"))
		want := durable.GenInfo{Size: 4, SHA256: DigestOf([]byte("ours")).SHA256, Key: key, Generation: 1, Epoch: 3,
			Format: durable.Format}
		if info, err := s.StatGeneration(ctx, m.Object); err != nil || info != want || !info.Holds(m.Digest()) {
			t.Fatalf("StatGeneration = %+v, %v; want %+v", info, err, want)
		}
		// Our own PUT again (a re-send of a lost reply): ErrExists, and the stat proves it holds our bytes.
		if err := s.PutGeneration(ctx, m, file(t, []byte("ours"))); !errors.Is(err, durable.ErrExists) {
			t.Fatalf("duplicate PutGeneration err = %v, want ErrExists", err)
		}
		if info, err := s.StatGeneration(ctx, m.Object); err != nil || !info.Holds(m.Digest()) || info.Holds(DigestOf([]byte("other"))) {
			t.Fatalf("StatGeneration after a duplicate = %+v, %v", info, err)
		}
		if _, err := s.StatGeneration(ctx, durable.GenObject(key, 2, 3)); !errors.Is(err, durable.ErrNotFound) {
			t.Fatalf("StatGeneration of a missing object: %v, want ErrNotFound", err)
		}
	})

	t.Run("two epochs upload the same generation number: both stored, neither overwritten", func(t *testing.T) {
		s := newStore(t)
		m1 := putAt(t, s, "suite/epochs", 2, 1, []byte("old owner"))
		m2 := putAt(t, s, "suite/epochs", 2, 2, []byte("new owner"))
		if m1.Object == m2.Object {
			t.Fatalf("both epochs named %s", m1.Object)
		}
		for _, m := range []durable.Manifest{m1, m2} {
			path := filepath.Join(t.TempDir(), "db")
			if _, err := s.FetchGeneration(ctx, m, path); err != nil {
				t.Fatalf("FetchGeneration(%s): %v", m.Object, err)
			}
		}
	})

	t.Run("keys whose names nest are independent databases", func(t *testing.T) {
		s := newStore(t)
		outer, inner := "suite/n", "suite/n/gen/x" // in layout v1 inner's manifest lay under outer's gen/
		mo := put(t, s, outer, 1, []byte("outer"))
		mi := put(t, s, inner, 1, []byte("inner"))
		publish(t, s, mo, durable.NoVersion)
		publish(t, s, mi, durable.NoVersion)
		for _, m := range []durable.Manifest{mo, mi} {
			got, _, err := s.ReadManifest(ctx, m.Key)
			path := filepath.Join(t.TempDir(), "db")
			if err != nil || !got.Same(m) {
				t.Fatalf("ReadManifest(%s) = %+v, %v", m.Key, got, err)
			}
			if _, err := s.FetchGeneration(ctx, got, path); err != nil {
				t.Fatalf("FetchGeneration(%s): %v", m.Key, err)
			}
		}
	})

	t.Run("fetch of a missing generation is ErrNotFound and leaves no file", func(t *testing.T) {
		s := newStore(t)
		m := durable.NextManifest("suite/missing", 7, 1, DigestOf([]byte("x")), now)
		path := filepath.Join(t.TempDir(), "db")
		if _, err := s.FetchGeneration(ctx, m, path); !errors.Is(err, durable.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		noFile(t, path)
	})

	t.Run("fetch that does not match the manifest is ErrCorrupt and leaves no file", func(t *testing.T) {
		s := newStore(t)
		m := put(t, s, "suite/corrupt", 1, []byte("real bytes"))
		for name, bad := range map[string]durable.Manifest{
			"wrong hash": func() durable.Manifest { b := m; b.SHA256 = DigestOf([]byte("other")).SHA256; return b }(),
			"wrong size": func() durable.Manifest { b := m; b.Size++; return b }(),
		} {
			path := filepath.Join(t.TempDir(), "db")
			if _, err := s.FetchGeneration(ctx, bad, path); !errors.Is(err, durable.ErrCorrupt) {
				t.Fatalf("%s: err = %v, want ErrCorrupt", name, err)
			}
			noFile(t, path)
		}
	})

	t.Run("fetch overwrites an existing local file", func(t *testing.T) {
		s := newStore(t)
		m := put(t, s, "suite/over", 1, []byte("new"))
		path := file(t, []byte("old and longer"))
		if _, err := s.FetchGeneration(ctx, m, path); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(path); string(got) != "new" {
			t.Fatalf("file = %q", got)
		}
	})

	t.Run("publish then read returns the manifest and its version", func(t *testing.T) {
		s := newStore(t)
		m := put(t, s, "suite/pub", 1, []byte("g1"))
		v := publish(t, s, m, durable.NoVersion)
		got, gv, err := s.ReadManifest(ctx, "suite/pub")
		if err != nil || gv != v || !got.Same(m) || !got.WrittenAt.Equal(m.WrittenAt) {
			t.Fatalf("ReadManifest = %+v, %q, %v; want %+v, %q", got, gv, err, m, v)
		}
		m2 := put(t, s, "suite/pub", 2, []byte("g2"))
		v2 := publish(t, s, m2, v)
		if got, gv, _ := s.ReadManifest(ctx, "suite/pub"); gv != v2 || !got.Same(m2) {
			t.Fatalf("after second publish: %+v %q", got, gv)
		}
	})

	t.Run("publish against a moved version is ErrStale and changes nothing", func(t *testing.T) {
		s := newStore(t)
		m1 := put(t, s, "suite/stale", 1, []byte("g1"))
		v1 := publish(t, s, m1, durable.NoVersion)
		if _, err := s.PublishManifest(ctx, m1, durable.NoVersion); !errors.Is(err, durable.ErrStale) {
			t.Fatalf("create-only publish over an existing manifest: err = %v, want ErrStale", err)
		}
		m2 := put(t, s, "suite/stale", 2, []byte("g2"))
		v2 := publish(t, s, m2, v1)
		m3 := put(t, s, "suite/stale", 3, []byte("g3"))
		if _, err := s.PublishManifest(ctx, m3, v1); !errors.Is(err, durable.ErrStale) {
			t.Fatalf("publish on old version: err = %v, want ErrStale", err)
		}
		if got, gv, _ := s.ReadManifest(ctx, "suite/stale"); gv != v2 || !got.Same(m2) {
			t.Fatalf("a stale publish changed the manifest: %+v %q", got, gv)
		}
	})

	t.Run("publish at a lower epoch than stored is ErrStale even on the current version", func(t *testing.T) {
		s := newStore(t)
		m := put(t, s, "suite/epoch", 1, []byte("g1"))
		m.Epoch = 5
		v := publish(t, s, m, durable.NoVersion)
		low := put(t, s, "suite/epoch", 2, []byte("g2"))
		low.Epoch = 4
		if _, err := s.PublishManifest(ctx, low, v); !errors.Is(err, durable.ErrStale) {
			t.Fatalf("err = %v, want ErrStale", err)
		}
		same := low
		same.Epoch = 5 // equal epoch is allowed (on point)
		publish(t, s, same, v)
	})

	t.Run("concurrent publishes on one version: exactly one wins", func(t *testing.T) {
		s := newStore(t)
		base := put(t, s, "suite/race", 1, []byte("g1"))
		v := publish(t, s, base, durable.NoVersion)
		const n = 8
		ms := make([]durable.Manifest, n)
		for i := range n {
			ms[i] = put(t, s, "suite/race", uint64(2+i), []byte{byte(i)})
		}
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := range n {
			wg.Go(func() { _, errs[i] = s.PublishManifest(ctx, ms[i], v) })
		}
		wg.Wait()
		won := -1
		for i, err := range errs {
			switch {
			case err == nil && won >= 0:
				t.Fatalf("publishes %d and %d both won on version %q", won, i, v)
			case err == nil:
				won = i
			case !errors.Is(err, durable.ErrStale):
				t.Fatalf("publish %d: err = %v, want nil or ErrStale", i, err)
			}
		}
		if won < 0 {
			t.Fatal("no publish won")
		}
		if got, _, _ := s.ReadManifest(ctx, "suite/race"); !got.Same(ms[won]) {
			t.Fatalf("stored %+v, winner %+v", got, ms[won])
		}
	})

	t.Run("delete generations: the named ones go, the rest stay, a missing one is fine", func(t *testing.T) {
		s := newStore(t)
		const key = "suite/del"
		g1, g2, g3 := put(t, s, key, 1, []byte("g1")), put(t, s, key, 2, []byte("g2")), put(t, s, key, 3, []byte("g3"))
		v := publish(t, s, g3, durable.NoVersion)
		if err := s.DeleteGenerations(ctx, key, []string{g1.Object, g2.Object, durable.GenObject(key, 9, 1)}); err != nil {
			t.Fatalf("DeleteGenerations: %v", err)
		}
		for _, o := range []string{g1.Object, g2.Object} {
			if _, err := s.StatGeneration(ctx, o); !errors.Is(err, durable.ErrNotFound) {
				t.Fatalf("%s after delete: %v, want ErrNotFound", o, err)
			}
		}
		if _, err := s.StatGeneration(ctx, g3.Object); err != nil {
			t.Fatalf("an object not named was deleted: %v", err)
		}
		if got, gv, err := s.ReadManifest(ctx, key); err != nil || gv != v || !got.Same(g3) {
			t.Fatalf("delete touched the manifest: %+v %q %v", got, gv, err)
		}
		if err := s.DeleteGenerations(ctx, key, nil); err != nil {
			t.Fatalf("empty delete: %v", err)
		}
	})

	t.Run("delete refuses any name that is not one of the key's generations, and deletes nothing", func(t *testing.T) {
		s := newStore(t)
		const key = "suite/del2"
		g1 := put(t, s, key, 1, []byte("g1"))
		other := put(t, s, "suite/del2/gen", 1, []byte("o1")) // a key whose name nests: another database
		publish(t, s, g1, durable.NoVersion)
		for _, bad := range []string{durable.ManifestObject(key), other.Object, durable.GenPrefix(key) + "x.db", durable.KeyPrefix(key)} {
			if err := s.DeleteGenerations(ctx, key, []string{g1.Object, bad}); err == nil {
				t.Fatalf("DeleteGenerations(%q) succeeded", bad)
			}
		}
		for _, o := range []string{g1.Object, other.Object} {
			if _, err := s.StatGeneration(ctx, o); err != nil {
				t.Fatalf("a refused delete removed %s: %v", o, err)
			}
		}
		if _, _, err := s.ReadManifest(ctx, key); err != nil {
			t.Fatalf("a refused delete removed the manifest: %v", err)
		}
	})

	t.Run("cancelled context fails", func(t *testing.T) {
		s := newStore(t)
		c, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := s.ReadManifest(c, "suite/x"); err == nil || errors.Is(err, durable.ErrNoManifest) {
			t.Fatalf("ReadManifest with a cancelled context: %v", err)
		}
	})
}

// NewGCStoreFunc returns an empty GCStore under test (a fresh namespace per call).
type NewGCStoreFunc func(t *testing.T) durable.GCStore

// RunGCStoreSuite checks the inventory contract (durable.Lister) and one sweep end to end. Run it against
// every GCStore, fake and real.
func RunGCStoreSuite(t *testing.T, newStore NewGCStoreFunc) {
	ctx := context.Background()
	put := func(t *testing.T, s durable.Store, key string, gen uint64, data []byte) durable.Manifest {
		t.Helper()
		p := filepath.Join(t.TempDir(), "snap.db")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		m := durable.NextManifest(key, gen, 1, DigestOf(data), time.Now())
		if err := s.PutGeneration(ctx, m, p); err != nil {
			t.Fatalf("PutGeneration(%s): %v", m.Object, err)
		}
		return m
	}

	t.Run("list a key: manifest version and time, generations sorted, nested keys apart", func(t *testing.T) {
		s := newStore(t)
		const key = "suite/ls"
		if l, err := s.ListKey(ctx, key); err != nil || l.Manifest != nil || len(l.Generations) != 0 {
			t.Fatalf("ListKey of an empty key = %+v, %v", l, err)
		}
		g2, g1 := put(t, s, key, 2, []byte("two")), put(t, s, key, 1, []byte("1"))
		put(t, s, key+"/gen", 1, []byte("nested")) // another database whose name nests
		v, err := s.PublishManifest(ctx, g2, durable.NoVersion)
		if err != nil {
			t.Fatal(err)
		}
		l, err := s.ListKey(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if l.Manifest == nil || l.Manifest.Name != durable.ManifestObject(key) || l.Manifest.Version != v || l.Manifest.Modified.IsZero() {
			t.Fatalf("listed manifest %+v, want version %q", l.Manifest, v)
		}
		if len(l.Generations) != 2 || l.Generations[0].Name != g1.Object || l.Generations[1].Name != g2.Object ||
			l.Generations[0].Size != 1 || l.Generations[1].Size != 3 || l.Generations[0].Modified.IsZero() {
			t.Fatalf("listed generations %+v", l.Generations)
		}
	})

	t.Run("list a shard's keys in name order; an error from fn stops it", func(t *testing.T) {
		s := newStore(t)
		want := map[string][]string{}
		for i := range 6 {
			k := fmt.Sprintf("suite/keys/%d", i)
			put(t, s, k, 1, []byte{byte(i)})
			sh := durable.KeyPrefix(k)[4:6]
			want[sh] = append(want[sh], k)
		}
		for sh, keys := range want {
			slices.SortFunc(keys, func(a, b string) int { return strings.Compare(durable.KeyPrefix(a), durable.KeyPrefix(b)) })
			var got []string
			if err := s.ListKeys(ctx, sh, func(k string) error { got = append(got, k); return nil }); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, keys) {
				t.Fatalf("shard %s: keys %q, want %q", sh, got, keys)
			}
			stop := errors.New("stop")
			n := 0
			if err := s.ListKeys(ctx, sh, func(string) error { n++; return stop }); !errors.Is(err, stop) || n != 1 {
				t.Fatalf("fn error: err %v after %d calls", err, n)
			}
		}
	})

	t.Run("sweep keeps the current and K below it, deletes older ones and old orphans", func(t *testing.T) {
		s := newStore(t)
		const key = "suite/sweep"
		var gens []durable.Manifest
		for g := uint64(1); g <= 6; g++ {
			gens = append(gens, put(t, s, key, g, []byte{byte(g)}))
		}
		// LastModified has 1 s resolution on some stores: publish strictly later.
		time.Sleep(1100 * time.Millisecond)
		if _, err := s.PublishManifest(ctx, gens[4], durable.NoVersion); err != nil { // 5 is current; 6 an orphan
			t.Fatal(err)
		}
		p := durable.GCPolicy{Keep: 2, Grace: time.Nanosecond}
		later := time.Now().Add(time.Hour)
		dry, err := durable.Sweep(ctx, s, key, later, p, true)
		want := []string{gens[0].Object, gens[1].Object, gens[5].Object}
		if err != nil || !slices.Equal(dry.Deleted, want) || dry.Objects != 6 || dry.DeletedBytes != 3 {
			t.Fatalf("dry run = %+v, %v; want to delete %q", dry, err, want)
		}
		if l, _ := s.ListKey(ctx, key); len(l.Generations) != 6 {
			t.Fatalf("a dry run deleted: %d left", len(l.Generations))
		}
		if r, err := durable.Sweep(ctx, s, key, later, p, false); err != nil || !slices.Equal(r.Deleted, want) {
			t.Fatalf("Sweep = %+v, %v", r, err)
		}
		l, err := s.ListKey(ctx, key)
		if err != nil || len(l.Generations) != 3 || l.Generations[0].Name != gens[2].Object || l.Generations[2].Name != gens[4].Object {
			t.Fatalf("after the sweep: %+v, %v", l.Generations, err)
		}
		if r, err := durable.Sweep(ctx, s, key, time.Now(), durable.GCPolicy{Keep: 2}, false); err != nil || len(r.Deleted) != 0 {
			t.Fatalf("a sweep inside the default grace window deleted %q (%v)", r.Deleted, err)
		}
	})
}
