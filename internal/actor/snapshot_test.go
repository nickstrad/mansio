package actor

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

func TestSnapshotRunsOnTheWriterEvenWithAReadPool(t *testing.T) {
	f := &faultExec{}
	a := fakeActor(t, Config{ReadConns: 2}, f, nil)
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	if r := a.Do(context.Background(), "w1", true); r.Err != nil {
		t.Fatal(r.Err)
	}
	c, err := a.Snapshot(context.Background(), "/x/snap")
	if err != nil || c != 1 {
		t.Fatalf("Snapshot = %d, %v; want 1 commit", c, err)
	}
	// The fake executor is shared by writer and readers; what matters is that Snapshot was not queued to the
	// read pool, which a.readQueued would show, and that it completed.
	if a.readQueued.Load() != 0 {
		t.Fatal("snapshot went through the read pool")
	}
}

func TestSnapshotAfterCloseFails(t *testing.T) {
	a := fakeActor(t, Config{}, &faultExec{}, nil)
	a.Close()
	if _, err := a.Snapshot(context.Background(), "/x/snap"); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func countRows(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM kv").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Integration: a real VACUUM INTO on a WAL database with a read pool, while writers keep committing. The
// snapshot holds at least the commits Snapshot reported, and no write is lost from the live database.
func TestSnapshotSQLiteIsConsistentUnderConcurrentWrites(t *testing.T) {
	cfg := Config{ReadConns: 2}
	a := startedKV(t, cfg, 100)
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
				id := 1000 + w*100000 + i
				if r := a.Do(ctx(t), `INSERT INTO kv (id, v) VALUES (?, 0)`, true, id); r.Err == nil {
					mu.Lock()
					acked++
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
	if n := countRows(t, snap); n < 100 {
		t.Fatalf("snapshot has %d rows, want at least the 100 seeded", n)
	}
	if _, err := a.Snapshot(ctx(t), snap); err == nil {
		t.Fatal("VACUUM INTO an existing file succeeded; it must fail rather than overwrite")
	}
	a.Close()
	if n := countRows(t, a.Path); n != 100+acked {
		t.Fatalf("live database has %d rows, want %d (100 seeded + %d acknowledged)", n, 100+acked, acked)
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
