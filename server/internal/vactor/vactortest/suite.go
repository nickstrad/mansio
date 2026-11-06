package vactortest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/mansio/server/internal/vactor"
)

// NewFetcherFunc returns a Fetcher under test and a way to store objects in its backing store.
type NewFetcherFunc func(t *testing.T) (f vactor.Fetcher, put func(key string, data []byte))

// RunFetcherSuite checks the vactor.Fetcher contract. Run it against every implementation, real and fake.
func RunFetcherSuite(t *testing.T, newFetcher NewFetcherFunc) {
	ctx := context.Background()
	fetchOK := func(t *testing.T, f vactor.Fetcher, key string, want []byte) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "db")
		got, err := f.Fetch(ctx, key, path)
		if err != nil {
			t.Fatalf("Fetch(%q): %v", key, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fetched file: %v", err)
		}
		if !bytes.Equal(data, want) || got.Bytes != int64(len(want)) {
			t.Fatalf("Fetch(%q): file %d bytes, Bytes = %d; want %d bytes", key, len(data), got.Bytes, len(want))
		}
		if got.TTFB < 0 || got.TTFB > got.Total {
			t.Fatalf("Fetch(%q): TTFB %v not within [0, Total %v]", key, got.TTFB, got.Total)
		}
		if _, err := os.Stat(path + ".part"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Fetch(%q) left a .part file (stat err %v)", key, err)
		}
		return path
	}

	// Sizes around the boundaries an implementation is likely to have: empty, one byte, and either side
	// of a 1 MiB copy buffer.
	for _, tc := range []struct {
		name string
		size int
	}{
		{"empty object", 0},
		{"one byte", 1},
		{"just under 1 MiB", 1<<20 - 1},
		{"exactly 1 MiB", 1 << 20},
		{"just over 1 MiB", 1<<20 + 1},
	} {
		t.Run("writes exact bytes: "+tc.name, func(t *testing.T) {
			f, put := newFetcher(t)
			data := make([]byte, tc.size)
			for i := range data {
				data[i] = byte(i*7 + 3)
			}
			put("a/obj.sqlite", data)
			fetchOK(t, f, "a/obj.sqlite", data)
		})
	}

	t.Run("missing object is ErrNotFound and leaves no file", func(t *testing.T) {
		f, _ := newFetcher(t)
		path := filepath.Join(t.TempDir(), "db")
		_, err := f.Fetch(ctx, "no/such/key", path)
		if !errors.Is(err, vactor.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		for _, p := range []string{path, path + ".part"} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s exists after a failed fetch (stat err %v)", p, err)
			}
		}
	})

	t.Run("overwrites an existing local file", func(t *testing.T) {
		f, put := newFetcher(t)
		put("k", []byte("new"))
		path := filepath.Join(t.TempDir(), "db")
		if err := os.WriteFile(path, []byte("old and longer"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Fetch(ctx, "k", path); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(path); string(data) != "new" {
			t.Fatalf("file = %q, want %q", data, "new")
		}
	})

	t.Run("cancelled context fails and leaves no file", func(t *testing.T) {
		f, put := newFetcher(t)
		put("k", []byte("data"))
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		path := filepath.Join(t.TempDir(), "db")
		if _, err := f.Fetch(cctx, "k", path); err == nil {
			t.Fatal("Fetch with a cancelled context succeeded")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("file exists after a cancelled fetch (stat err %v)", err)
		}
	})
}
