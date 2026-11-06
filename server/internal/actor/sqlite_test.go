package actor

import (
	"context"
	"errors"
	"fmt"
	"github.com/nickstrad/mansio/server/internal/errs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Integration tests: the actor against real SQLite files in t.TempDir().

const kvDDL = `CREATE TABLE IF NOT EXISTS kv (id INTEGER PRIMARY KEY, v INTEGER NOT NULL)`

const slowCTE = `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < ?) SELECT COUNT(*) FROM c`

func fillKV(rows int) func(exec func(string, ...any) error) error {
	return func(exec func(string, ...any) error) error {
		for i := 1; i <= rows; i++ {
			if err := exec(`INSERT INTO kv (id, v) VALUES (?, ?)`, i, i*10); err != nil {
				return err
			}
		}
		return nil
	}
}

// seededKV creates a kv database with rows rows (v = 10·id) and returns its path.
func seededKV(t testing.TB, cfg Config, rows int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	if err := Seed(path, cfg, kvDDL, fillKV(rows)); err != nil {
		t.Fatal(err)
	}
	return path
}

func newKV(t testing.TB, cfg Config, rows int) *Actor {
	t.Helper()
	a := New(0, seededKV(t, cfg, rows), cfg)
	t.Cleanup(a.Close)
	return a
}

func startedKV(t testing.TB, cfg Config, rows int) *Actor {
	t.Helper()
	a := newKV(t, cfg, rows)
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	return a
}

func ctx(t testing.TB) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestReadSeesCommittedWrite(t *testing.T) {
	a := startedKV(t, Config{}, 10)
	if r := a.Do(ctx(t), `UPDATE kv SET v = ? WHERE id = ?`, true, 777, 3); r.Err != nil || r.Rows != 1 {
		t.Fatalf("update: %+v", r)
	}
	if r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = ?`, false, 3); r.Err != nil || r.Rows != 1 || r.First != 777 {
		t.Fatalf("read: %+v", r)
	}
	if r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = ?`, false, 999); r.Err != nil || r.Rows != 0 {
		t.Fatalf("missing row: %+v", r)
	}
}

func TestGroupCommit(t *testing.T) {
	tests := []struct {
		name      string
		maxBatch  int
		wantBatch int
	}{
		{"queued writes share one commit", 64, 50},
		{"max_batch 1 commits each write alone", 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newKV(t, Config{MaxBatch: tc.maxBatch}, 0)
			var reqs []*Request
			for i := 1; i <= 50; i++ {
				r := &Request{SQL: `INSERT INTO kv (id, v) VALUES (?, ?)`, Args: []any{i, i}, Write: true}
				if err := a.Submit(r); err != nil {
					t.Fatal(err)
				}
				reqs = append(reqs, r)
			}
			if err := a.Start(true); err != nil {
				t.Fatal(err)
			}
			for _, r := range reqs {
				if res := r.Wait(ctx(t)); res.Err != nil || res.Batch != tc.wantBatch {
					t.Fatalf("want batch %d, got %+v", tc.wantBatch, res)
				}
			}
			if r := a.Do(ctx(t), `SELECT COUNT(*) FROM kv`, false); r.First != 50 {
				t.Fatalf("count = %d", r.First)
			}
		})
	}
}

func TestStatementTimeoutInterruptsAndActorRecovers(t *testing.T) {
	a := startedKV(t, Config{}, 10)
	r := &Request{SQL: slowCTE, Args: []any{int64(1e10)}, StmtTimeout: 50 * time.Millisecond}
	a.Submit(r)
	res := r.Wait(ctx(t))
	if !res.TimedOut || res.Err == nil || res.Exec > time.Second {
		t.Fatalf("want a prompt statement timeout, got %+v", res)
	}
	if r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = 1`, false); r.Err != nil || r.First != 10 {
		t.Fatalf("actor did not recover: %+v", r)
	}
}

// The property the design exists for: a database busy with a slow statement does not delay another database.
func TestSlowDatabaseDoesNotDelayOthers(t *testing.T) {
	slow, fast := startedKV(t, Config{}, 10), startedKV(t, Config{}, 100)
	slowReq := &Request{SQL: slowCTE, Args: []any{int64(1e10)}, StmtTimeout: 400 * time.Millisecond}
	slow.Submit(slowReq)
	time.Sleep(20 * time.Millisecond)
	worst := worstRead(t, fast, 200)
	assertStillRunning(t, slowReq)
	if worst > 50*time.Millisecond {
		t.Fatalf("a fast read took %s while another database was busy", worst)
	}
}

// Within one database: one connection queues reads behind a slow one; a read pool does not.
func TestReadPoolRemovesHeadOfLineBlocking(t *testing.T) {
	tests := []struct {
		name      string
		readConns int
		blocked   bool
	}{
		{"single connection queues behind the slow read", 0, true},
		{"read pool keeps fast reads moving", 2, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := startedKV(t, Config{ReadConns: tc.readConns}, 100)
			slowReq := &Request{SQL: slowCTE, Args: []any{int64(1e10)}, StmtTimeout: 300 * time.Millisecond}
			a.Submit(slowReq)
			time.Sleep(20 * time.Millisecond)
			r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = 1`, false)
			if r.Err != nil {
				t.Fatal(r.Err)
			}
			if blocked := r.QueueWait > 100*time.Millisecond; blocked != tc.blocked {
				t.Fatalf("queued %s; want blocked=%v", r.QueueWait, tc.blocked)
			}
		})
	}
}

func TestWriteCommitsWhileReadPoolRunsASlowRead(t *testing.T) {
	a := startedKV(t, Config{ReadConns: 2}, 10)
	slowReq := &Request{SQL: slowCTE, Args: []any{int64(1e10)}, StmtTimeout: time.Second}
	a.Submit(slowReq)
	time.Sleep(20 * time.Millisecond)
	if r := a.Do(ctx(t), `INSERT INTO kv (id, v) VALUES (1000, 1)`, true); r.Err != nil || r.Exec > 50*time.Millisecond {
		t.Fatalf("write during slow read: %+v", r)
	}
	if r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = 1000`, false); r.First != 1 {
		t.Fatalf("read pool did not see the committed write: %+v", r)
	}
	assertStillRunning(t, slowReq)
}

func TestTimedOutWriteRollsBackItsBatchInSQLite(t *testing.T) {
	a := newKV(t, Config{}, 0)
	w1 := &Request{SQL: `INSERT INTO kv (id, v) VALUES (1, 1)`, Write: true}
	slow := &Request{
		SQL:  `INSERT INTO kv (id, v) WITH RECURSIVE c(x) AS (SELECT 10 UNION ALL SELECT x + 1 FROM c WHERE x < ?) SELECT x, x FROM c`,
		Args: []any{int64(1e10)}, Write: true, StmtTimeout: 50 * time.Millisecond,
	}
	for _, r := range []*Request{w1, slow} {
		a.Submit(r)
	}
	a.Start(true)
	if res := slow.Wait(ctx(t)); !res.TimedOut {
		t.Fatalf("slow write: %+v", res)
	}
	if res := w1.Wait(ctx(t)); !errors.Is(res.Err, ErrAborted) {
		t.Fatalf("batch-mate: %+v", res)
	}
	if res := a.Do(ctx(t), `SELECT COUNT(*) FROM kv`, false); res.First != 0 {
		t.Fatalf("rolled-back batch left %d rows", res.First)
	}
}

func TestIdleDatabaseIsClosedAndReopened(t *testing.T) {
	for _, readConns := range []int{0, 3} {
		t.Run(fmt.Sprintf("read_conns=%d", readConns), func(t *testing.T) {
			a := startedKV(t, Config{ReadConns: readConns, IdleClose: 20 * time.Millisecond}, 5)
			if r := a.Do(ctx(t), `SELECT 1`, false); r.Err != nil || r.Reopened {
				t.Fatalf("%+v", r)
			}
			waitFor(t, func() bool { return !a.IsOpen() })
			r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = 2`, false)
			if r.Err != nil || r.First != 20 || (readConns == 0 && !r.Reopened) {
				t.Fatalf("after reopen: %+v", r)
			}
			if a.Opens() != 2 {
				t.Fatalf("opens = %d", a.Opens())
			}
		})
	}
}

func TestEveryJournalModeOpensAndServes(t *testing.T) {
	for _, mode := range JournalModes {
		for _, sync := range []string{"OFF", "FULL"} {
			t.Run(mode+"/"+sync, func(t *testing.T) {
				a := startedKV(t, Config{JournalMode: mode, Synchronous: sync}, 5)
				if r := a.Do(ctx(t), `INSERT INTO kv (id, v) VALUES (100, 7)`, true); r.Err != nil {
					t.Fatal(r.Err)
				}
				if r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = 100`, false); r.Err != nil || r.First != 7 {
					t.Fatalf("%+v", r)
				}
			})
		}
	}
}

func TestStatementCacheEvictsAndStillServes(t *testing.T) {
	a := startedKV(t, Config{StmtCache: 2}, 10)
	for round := range 3 {
		for k := 1; k <= 5; k++ { // 5 distinct statements through a 2-entry cache
			q := fmt.Sprintf(`SELECT v FROM kv WHERE id = %d`, k)
			if r := a.Do(ctx(t), q, false); r.Err != nil || r.First != int64(k*10) {
				t.Fatalf("round %d, %s: %+v", round, q, r)
			}
		}
	}
}

func TestSeedFailureLeavesNothingAndIsWrapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	err := Seed(path, Config{}, kvDDL, func(exec func(string, ...any) error) error {
		if err := exec(`INSERT INTO kv (id, v) VALUES (1, 1)`); err != nil {
			return err
		}
		return exec(`INSERT INTO nope VALUES (1)`)
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if e, ok := errs.Outermost(err); !ok || e.Module != "actor" {
		t.Fatalf("error is not wrapped by actor: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 0 {
		t.Fatalf("failed seed left files behind: %v", entries)
	}
}

func TestDirectory(t *testing.T) {
	dir := t.TempDir()
	path := func(i int) string { return filepath.Join(dir, fmt.Sprintf("db-%d.sqlite", i)) }
	seed := func(i int) error { return Seed(path(i), Config{}, kvDDL, fillKV(3)) }
	d := NewDirectory(Config{})
	t.Cleanup(d.Close)

	if st, err := d.Grow(10, path, seed, 4); err != nil || st.Added != 10 || d.Len() != 10 {
		t.Fatalf("grow to 10: %+v, %v, len %d", st, err, d.Len())
	}
	if st, _ := d.Grow(5, path, seed, 4); st.Added != 0 || d.Len() != 10 {
		t.Fatal("shrinking Grow must be a no-op")
	}
	bad := errors.New("seed failed")
	failing := func(i int) error {
		if i == 13 {
			return bad
		}
		return seed(i)
	}
	if _, err := d.Grow(20, path, failing, 4); !errors.Is(err, bad) || d.Len() != 10 {
		t.Fatalf("failed Grow: err %v, len %d (must be unchanged)", err, d.Len())
	}
	for i := range d.Len() {
		if d.Get(i).ID != i {
			t.Fatalf("slot %d holds actor %d", i, d.Get(i).ID)
		}
	}
	if s := d.Stats(); s.Open != 10 || s.Queued != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestManyActorsAnswerInParallel(t *testing.T) {
	n := 1000
	if testing.Short() {
		n = 100
	}
	dir := t.TempDir()
	path := func(i int) string { return filepath.Join(dir, fmt.Sprintf("db-%d.sqlite", i)) }
	d := NewDirectory(Config{})
	t.Cleanup(d.Close)
	if _, err := d.Grow(n, path, func(i int) error { return Seed(path(i), Config{}, kvDDL, fillKV(10)) }, runtime.NumCPU()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errc := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			if r := d.Get(i).Do(ctx(t), `SELECT v FROM kv WHERE id = ?`, false, 5); r.Err != nil || r.First != 50 {
				errc <- fmt.Errorf("db %d: %+v", i, r)
			}
		})
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
}

func worstRead(t *testing.T, a *Actor, n int) time.Duration {
	t.Helper()
	var worst time.Duration
	for i := range n {
		t0 := time.Now()
		if r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = ?`, false, 1+i%100); r.Err != nil {
			t.Fatal(r.Err)
		}
		worst = max(worst, time.Since(t0))
	}
	return worst
}

func assertStillRunning(t *testing.T, r *Request) {
	t.Helper()
	select {
	case res := <-r.reply:
		t.Fatalf("slow statement finished first, so the test proves nothing: %+v", res)
	default:
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached within 2s")
}

// Regression: an unescaped '#' or '?' in the path cut the file: URI short, so every such path opened one shared
// file (found when the model fuzz seeds, whose temp dirs contain '#', saw each other's rows).
func TestPathsWithURICharactersOpenTheirOwnFile(t *testing.T) {
	dir := t.TempDir()
	for i, name := range []string{"a#1.sqlite", "a#2.sqlite", "b?x=1.sqlite", "c%20d.sqlite", "e f.sqlite"} {
		path := filepath.Join(dir, name)
		if err := Seed(path, Config{}, kvDDL, fillKV(i+1)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: file not created at its own path: %v", name, err)
		}
		a := New(i, path, Config{})
		if err := a.Start(true); err != nil {
			t.Fatal(err)
		}
		r := a.Do(ctx(t), `SELECT count(*) FROM kv`, false)
		a.Close()
		if r.Err != nil || r.First != int64(i+1) {
			t.Fatalf("%s: count = %d (%v), want %d", name, r.First, r.Err, i+1)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 5*3 {
		t.Fatalf("stray files: %v", entries)
	}
}
