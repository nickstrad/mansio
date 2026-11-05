package durabletest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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

	t.Run("cancelled context fails", func(t *testing.T) {
		s := newStore(t)
		c, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := s.ReadManifest(c, "suite/x"); err == nil || errors.Is(err, durable.ErrNoManifest) {
			t.Fatalf("ReadManifest with a cancelled context: %v", err)
		}
	})
}
