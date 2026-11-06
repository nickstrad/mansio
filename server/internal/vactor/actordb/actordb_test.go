package actordb_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/actordb"
)

// Integration: the durable host over real SQLite actors and the fake store.

func host(t *testing.T, s *durabletest.Store, cfg actor.Config) *vactor.Host {
	t.Helper()
	h, err := vactor.NewHost(nil, actordb.Opener{Cfg: cfg}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: s, CreateMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func do(t *testing.T, h *vactor.Host, key, q string, args ...any) actor.Result {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := h.Get(c, key)
	if err != nil {
		t.Fatal(err)
	}
	r := res.DB.(*actordb.DB).Actor().Do(c, q, actor.IsWrite(q), args...)
	if r.Err != nil {
		t.Fatalf("%s: %v", q, r.Err)
	}
	return r
}

func TestOpenerNeverCreatesAMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "none.db")
	if _, err := (actordb.Opener{}).Open(p); err == nil {
		t.Fatal("Open of a missing file succeeded")
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("Open created the file")
	}
}

func TestSQLiteWriteBackSurvivesCrashAndPassivation(t *testing.T) {
	for _, cfg := range []actor.Config{{}, {ReadConns: 2}} {
		s := durabletest.NewStore()
		h := host(t, s, cfg)
		do(t, h, "t/a", `CREATE TABLE kv (id INTEGER PRIMARY KEY, v TEXT)`)
		for i := range 50 {
			do(t, h, "t/a", `INSERT INTO kv (id, v) VALUES (?, 'x')`, i)
		}
		res, err := h.WriteBack(context.Background(), "t/a")
		if err != nil || !res.Wrote || res.Generation != 1 {
			t.Fatalf("WriteBack = %+v, %v", res, err)
		}
		if res, _ := h.WriteBack(context.Background(), "t/a"); res.Wrote {
			t.Fatal("a clean database was written back again")
		}
		do(t, h, "t/a", `INSERT INTO kv (id, v) VALUES (100, 'after')`)
		if err := h.Passivate(context.Background(), "t/a"); err != nil { // uploads the closed file
			t.Fatal(err)
		}
		h2 := host(t, s, cfg) // a restart: empty cache
		if n := do(t, h2, "t/a", `SELECT count(*) FROM kv`).First; n != 51 {
			t.Fatalf("after restart: %d rows, want 51 (ReadConns %d)", n, cfg.ReadConns)
		}
		if m, _ := s.Manifest("t/a"); m.Generation != 2 {
			t.Fatalf("manifest %+v", m)
		}
	}
}

func TestSQLiteCrashLosesOnlyUnpublishedWrites(t *testing.T) {
	s := durabletest.NewStore()
	h := host(t, s, actor.Config{})
	do(t, h, "t/a", `CREATE TABLE kv (id INTEGER PRIMARY KEY)`)
	do(t, h, "t/a", `INSERT INTO kv VALUES (1)`)
	if _, err := h.WriteBack(context.Background(), "t/a"); err != nil {
		t.Fatal(err)
	}
	do(t, h, "t/a", `INSERT INTO kv VALUES (2)`) // acknowledged, never written back
	h2 := host(t, s, actor.Config{})             // another process, as after a crash of h
	if n := do(t, h2, "t/a", `SELECT count(*) FROM kv`).First; n != 1 {
		t.Fatalf("%d rows; want the 1 published", n)
	}
}

// OS-10 over real SQLite: a passivated database, clean or dirty, is re-warmed from the generation cache
// (its bytes are exactly the published generation's), and its rows are intact.
func TestGenCacheRewarmRealSQLite(t *testing.T) {
	s := durabletest.NewStore()
	h, err := vactor.NewHost(nil, actordb.Opener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: s,
		CreateMissing: true, GenCacheBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	do(t, h, "k", `CREATE TABLE t (v TEXT)`)
	do(t, h, "k", `INSERT INTO t VALUES ('a')`)
	for round := range 2 { // dirty, then clean
		if err := h.Passivate(context.Background(), "k"); err != nil {
			t.Fatal(err)
		}
		res, err := h.Get(context.Background(), "k")
		if err != nil || !res.Act.LocalHit {
			t.Fatalf("round %d: re-warm = %+v, %v; want a local hit", round, res.Act, err)
		}
		if n := do(t, h, "k", `SELECT count(*) FROM t`).First; n != 1 {
			t.Fatalf("round %d: %d rows", round, n)
		}
	}
}
