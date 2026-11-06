package actor

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/sqlitedsn"
)

// Commits and Snapshot are the additive hooks vactor's write-back uses (posted in shared_state.md by the
// writeback agent, 2026-11-05).

func TestCommitsCountsOnlyCommittedWriteTransactions(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *faultExec
		reqs []*Request
		want uint64
	}{
		{"one batch of writes = one commit", &faultExec{}, []*Request{write("w1"), write("w2")}, 1},
		{"reads only: none", &faultExec{}, []*Request{{SQL: "r1"}}, 0},
		{"commit failed: none", &faultExec{FailCommit: errors.New("disk I/O")}, []*Request{write("w1")}, 0},
		{"begin failed: none", &faultExec{FailBegin: errors.New("locked")}, []*Request{write("w1")}, 0},
		{"a write failed but the batch committed: one", &faultExec{FailOn: map[string]error{"w1": errors.New("constraint")}},
			[]*Request{write("w1"), write("w2")}, 1},
		{"rolled back by a timeout: none", &faultExec{},
			[]*Request{write("w1"), {SQL: "w2 SLOW", Write: true, StmtTimeout: 10 * time.Millisecond}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fakeActor(t, Config{}, tc.f, nil)
			submitAll(t, a, tc.reqs...)
			if got := a.Commits(); got != tc.want {
				t.Fatalf("Commits = %d, want %d", got, tc.want)
			}
		})
	}
}

// blockingBackup is a fault executor whose backup blocks until release closes, so a test can act while a
// snapshot is in progress.
type blockingBackup struct {
	faultExec
	started, release chan struct{}
}

func (b *blockingBackup) backup(ctx context.Context, dst string) error {
	close(b.started)
	<-b.release
	return b.faultExec.backup(ctx, dst)
}

// snapshotActor returns a started actor whose writer is w and whose readers are r.
func snapshotActor(t *testing.T, cfg Config, w, r executor) *Actor {
	t.Helper()
	a := New(0, "fake", cfg)
	a.open = func(_ string, _ Config, readOnly bool) (executor, error) {
		if readOnly {
			return r, nil
		}
		return w, nil
	}
	t.Cleanup(a.Close)
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	return a
}

// In WAL mode with a read pool the snapshot runs on a reader, so a write commits while the copy is in progress
// (CP-8: VACUUM INTO on the writer made every write wait for the whole copy).
func TestSnapshotOnAReaderDoesNotBlockWrites(t *testing.T) {
	w := &faultExec{}
	r := &blockingBackup{started: make(chan struct{}), release: make(chan struct{})}
	a := snapshotActor(t, Config{ReadConns: 1}, w, r)
	done := make(chan error, 1)
	go func() { _, err := a.Snapshot(context.Background(), "/x/snap"); done <- err }()
	<-r.started
	if res := a.Do(context.Background(), "w1", true); res.Err != nil {
		t.Fatalf("write during snapshot: %v", res.Err)
	}
	close(r.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.statements(), "BACKUP /x/snap") || slices.Contains(w.statements(), "BACKUP /x/snap") {
		t.Fatalf("backup ran on the wrong connection: reader %v, writer %v", r.statements(), w.statements())
	}
}

// Without WAL a reader's long read transaction would block the writer's commit, and with no read pool there is
// no reader: either way the snapshot runs on the writer, between batches.
func TestSnapshotRunsOnTheWriterWithoutWALOrReadPool(t *testing.T) {
	for _, cfg := range []Config{{ReadConns: 2, JournalMode: "DELETE"}, {ReadConns: 0}} {
		w, r := &faultExec{}, &faultExec{}
		a := snapshotActor(t, cfg, w, r)
		if res := a.Do(context.Background(), "w1", true); res.Err != nil {
			t.Fatal(res.Err)
		}
		c, err := a.Snapshot(context.Background(), "/x/snap")
		if err != nil || c != 1 {
			t.Fatalf("%+v: Snapshot = %d, %v; want 1 commit", cfg, c, err)
		}
		if !slices.Contains(w.statements(), "BACKUP /x/snap") || slices.Contains(r.statements(), "BACKUP /x/snap") {
			t.Fatalf("%+v: backup not on the writer: reader %v, writer %v", cfg, r.statements(), w.statements())
		}
	}
}

func TestSnapshotErrorIsReturned(t *testing.T) {
	w := &faultExec{FailOn: map[string]error{"BACKUP": errors.New("disk full")}}
	a := snapshotActor(t, Config{}, w, w)
	if _, err := a.Snapshot(context.Background(), "/x/snap"); err == nil || err.Error() != "disk full" {
		t.Fatalf("err = %v, want disk full", err)
	}
}

func TestSnapshotAfterCloseFails(t *testing.T) {
	a := fakeActor(t, Config{}, &faultExec{}, nil)
	a.Close()
	if _, err := a.Snapshot(context.Background(), "/x/snap"); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

// checkSnapshot opens a snapshot read-only and checks it is intact and one committed state (sum(v) == 0).
func checkSnapshot(t *testing.T, path string) {
	t.Helper()
	db := openRO(t, path)
	var ok string
	var sum int64
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity_check = %q, %v", ok, err)
	}
	if err := db.QueryRow("SELECT total(v) FROM kv").Scan(&sum); err != nil || sum != 0 {
		t.Fatalf("sum(v) = %d, %v; want 0 (one committed state)", sum, err)
	}
}

// openRO opens path read-only; closed at the end of the test.
func openRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqlitedsn.Build(path, "query_only(1)"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func countRows(t *testing.T, path string) int {
	t.Helper()
	db := openRO(t, path)
	var n int
	if err := db.QueryRow("SELECT count(*) FROM kv").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Integration: a real backup on a WAL database with a read pool, one page per step, while writers keep
// committing. Every write keeps sum(v) == 0 (a pair of rows ±x in one statement), so a copy that mixed two
// states would break it. The snapshot is intact, holds at least the commits Snapshot reported, and no write is
// lost from the live database.
func TestSnapshotSQLiteIsConsistentUnderConcurrentWrites(t *testing.T) {
	defer func(n int32) { backupStepPages = n }(backupStepPages)
	backupStepPages = 1
	cfg := Config{ReadConns: 2}
	a := startedKV(t, cfg, 0)
	for i := range 2000 { // a few hundred pages, so the copy takes many steps
		if r := a.Do(ctx(t), `INSERT INTO kv (id, v) VALUES (?, ?), (?, ?)`, true, -1-2*i, i, -2-2*i, -i); r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	seeded := 4000
	var wg sync.WaitGroup
	var mu sync.Mutex
	acked := 0
	stop := make(chan struct{})
	for w := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := 2 * (1 + w*100000 + i)
				if r := a.Do(ctx(t), `INSERT INTO kv (id, v) VALUES (?, ?), (?, ?)`, true, id, i, id+1, -i); r.Err == nil {
					mu.Lock()
					acked += 2
					mu.Unlock()
				}
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	before := a.Commits()
	snap := filepath.Join(t.TempDir(), "snap.db")
	c, err := a.Snapshot(ctx(t), snap)
	if err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	if c < before {
		t.Fatalf("Snapshot reported %d commits, fewer than %d before it", c, before)
	}
	if n := countRows(t, snap); n < seeded {
		t.Fatalf("snapshot has %d rows, want at least the %d seeded", n, seeded)
	}
	checkSnapshot(t, snap)
	if _, err := a.Snapshot(ctx(t), snap); err == nil {
		t.Fatal("a snapshot over an existing file succeeded; it must fail rather than overwrite")
	}
	a.Close()
	if n := countRows(t, a.Path); n != seeded+acked {
		t.Fatalf("live database has %d rows, want %d (%d seeded + %d acknowledged)", n, seeded+acked, seeded, acked)
	}
	if a.Commits() == 0 {
		t.Fatal("Commits = 0 after acknowledged writes")
	}
}

func TestCommitsSurviveIdleCloseReopen(t *testing.T) {
	a := startedKV(t, Config{IdleClose: 5 * time.Millisecond}, 1)
	if r := a.Do(ctx(t), `UPDATE kv SET v = 1`, true); r.Err != nil {
		t.Fatal(r.Err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for a.IsOpen() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r := a.Do(ctx(t), `UPDATE kv SET v = 2`, true); r.Err != nil || !r.Reopened {
		t.Fatalf("%+v", r)
	}
	if a.Commits() != 2 {
		t.Fatalf("Commits = %d, want 2", a.Commits())
	}
}
