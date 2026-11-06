package fronttest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
)

// SuiteDB is the database the suite expects the executor to serve (schema KVSchema, rows KVRows). MissingDB
// must not be served.
const (
	SuiteDB   = "suite-db"
	MissingDB = "missing-db"
)

// RunExecutorSuite checks the Executor contract (see front.Executor). newExec returns a fresh executor serving
// SuiteDB; it runs once per subtest.
func RunExecutorSuite(t *testing.T, newExec func(t *testing.T) front.Executor) {
	var ids atomic.Int64
	call := func(kind front.Kind, sql string, args ...any) front.Call { // a fresh request id per call
		return front.Call{Tenant: "t", DB: SuiteDB, RequestID: fmt.Sprintf("r%d", ids.Add(1)), Kind: kind, SQL: sql,
			Args: args, Deadline: time.Now().Add(5 * time.Second), StmtTimeout: time.Second, MaxRows: 100}
	}
	ctx := context.Background()

	t.Run("read one row", func(t *testing.T) {
		r, err := newExec(t).Exec(ctx, call(front.KindRead, GetSQL, int64(2)))
		if err != nil || r.Count != 1 || len(r.Rows) != 1 || fmt.Sprint(r.Rows[0]) != "[2 v2]" || len(r.Columns) != 2 {
			t.Fatalf("%+v %v", r, err)
		}
	})
	t.Run("read no rows", func(t *testing.T) {
		r, err := newExec(t).Exec(ctx, call(front.KindRead, GetSQL, int64(99)))
		if err != nil || r.Count != 0 || len(r.Rows) != 0 {
			t.Fatalf("%+v %v", r, err)
		}
	})
	t.Run("rows at MaxRows (on point)", func(t *testing.T) {
		c := call(front.KindRead, ScanSQL)
		c.MaxRows = len(KVRows)
		r, err := newExec(t).Exec(ctx, c)
		if err != nil || r.Count != int64(len(KVRows)) {
			t.Fatalf("%+v %v", r, err)
		}
		for _, row := range r.Rows {
			if len(row) != len(r.Columns) {
				t.Fatalf("row %v vs columns %v", row, r.Columns)
			}
		}
	})
	t.Run("rows over MaxRows (off point)", func(t *testing.T) {
		c := call(front.KindRead, ScanSQL)
		c.MaxRows = len(KVRows) - 1
		if _, err := newExec(t).Exec(ctx, c); !errors.Is(err, front.ErrTooManyRows) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("write then read sees it", func(t *testing.T) {
		e := newExec(t)
		if r, err := e.Exec(ctx, call(front.KindWrite, PutSQL, int64(10), "ten")); err != nil || r.Count != 1 {
			t.Fatalf("write: %+v %v", r, err)
		}
		r, err := e.Exec(ctx, call(front.KindRead, GetSQL, int64(10)))
		if err != nil || r.Count != 1 || r.Rows[0][1] != "ten" {
			t.Fatalf("read: %+v %v", r, err)
		}
	})
	t.Run("constraint violation is ErrStatement and changes nothing", func(t *testing.T) {
		e := newExec(t)
		if _, err := e.Exec(ctx, call(front.KindWrite, PutSQL, int64(1), "dup")); !errors.Is(err, front.ErrStatement) {
			t.Fatalf("err %v", err)
		}
		r, _ := e.Exec(ctx, call(front.KindRead, GetSQL, int64(1)))
		if r.Count != 1 || r.Rows[0][1] != "v1" {
			t.Fatalf("row changed: %+v", r)
		}
	})
	t.Run("same request_id applies once and answers the original count", func(t *testing.T) {
		e := newExec(t)
		c := call(front.KindWrite, PutSQL, int64(30), "once")
		if r, err := e.Exec(ctx, c); err != nil || r.Count != 1 || r.Duplicate {
			t.Fatalf("first: %+v %v", r, err)
		}
		// The retry would fail on the key if it ran; it must not run.
		if r, err := e.Exec(ctx, c); err != nil || r.Count != 1 || !r.Duplicate {
			t.Fatalf("retry: %+v %v", r, err)
		}
		// Ids are per database and per write: a read with the same id is just a read.
		read := call(front.KindRead, GetSQL, int64(30))
		read.RequestID = c.RequestID
		if r, err := e.Exec(ctx, read); err != nil || r.Count != 1 || r.Duplicate {
			t.Fatalf("read with the id: %+v %v", r, err)
		}
	})
	t.Run("failed write leaves its request_id unused", func(t *testing.T) {
		e := newExec(t)
		c := call(front.KindWrite, PutSQL, int64(1), "dup")
		if _, err := e.Exec(ctx, c); !errors.Is(err, front.ErrStatement) {
			t.Fatalf("err %v", err)
		}
		c.Args = []any{int64(31), "fixed"}
		if r, err := e.Exec(ctx, c); err != nil || r.Duplicate || r.Count != 1 {
			t.Fatalf("retry with the same id: %+v %v", r, err)
		}
	})
	t.Run("bad statement is ErrStatement", func(t *testing.T) {
		if _, err := newExec(t).Exec(ctx, call(front.KindRead, BadSQL)); !errors.Is(err, front.ErrStatement) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("unknown database", func(t *testing.T) {
		c := call(front.KindRead, GetSQL, int64(1))
		c.DB = MissingDB
		if _, err := newExec(t).Exec(ctx, c); !errors.Is(err, front.ErrUnknownDB) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("deadline already passed: expired, write not applied", func(t *testing.T) {
		e := newExec(t)
		c := call(front.KindWrite, PutSQL, int64(20), "late")
		c.Deadline = time.Now().Add(-time.Millisecond)
		if _, err := e.Exec(ctx, c); !errors.Is(err, front.ErrExpired) {
			t.Fatalf("err %v", err)
		}
		if r, _ := e.Exec(ctx, call(front.KindRead, GetSQL, int64(20))); r.Count != 0 {
			t.Fatalf("expired write was applied: %+v", r)
		}
	})
	t.Run("statement timeout interrupts and returns by the deadline", func(t *testing.T) {
		c := call(front.KindRead, SlowSQL)
		c.StmtTimeout = 50 * time.Millisecond
		t0 := time.Now()
		_, err := newExec(t).Exec(ctx, c)
		if !errors.Is(err, front.ErrStmtTimeout) || time.Since(t0) > 2*time.Second {
			t.Fatalf("err %v after %v", err, time.Since(t0))
		}
	})
	t.Run("concurrent calls", func(t *testing.T) {
		e := newExec(t)
		var wg sync.WaitGroup
		errc := make(chan error, 40)
		for i := range 40 {
			wg.Go(func() {
				var err error
				if i%2 == 0 {
					_, err = e.Exec(ctx, call(front.KindWrite, PutSQL, int64(100+i), "x"))
				} else {
					_, err = e.Exec(ctx, call(front.KindRead, GetSQL, int64(1)))
				}
				errc <- err
			})
		}
		wg.Wait()
		close(errc)
		for err := range errc {
			if err != nil {
				t.Fatal(err)
			}
		}
		r, err := e.Exec(ctx, call(front.KindRead, ScanSQL))
		if err != nil || r.Count != int64(len(KVRows)+20) {
			t.Fatalf("%d rows after 20 writes, %v", r.Count, err)
		}
	})
}
