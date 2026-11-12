package actor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A caller's ctx deadline bounds the statement, not only the wait (waiting is not working).
func TestDoWithDeadlineInterruptsStatement(t *testing.T) {
	a := startedKV(t, Config{}, 10)
	c, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if res := a.Do(c, slowCTE, false, int64(1e10)); res.Err == nil {
		t.Fatalf("slow read under a 50 ms deadline: %+v", res)
	}
	t0 := time.Now()
	r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = 1`, false)
	if r.Err != nil || r.First != 10 {
		t.Fatalf("next request: %+v", r)
	}
	if d := time.Since(t0); d > 100*time.Millisecond {
		t.Fatalf("next request took %s: the abandoned statement kept the connection", d)
	}
}

// A write whose caller's deadline passes mid-statement is interrupted and rolled back.
func TestDoWithDeadlineRollsBackASlowWrite(t *testing.T) {
	a := startedKV(t, Config{}, 0)
	c, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	slowInsert := `INSERT INTO kv (id, v) WITH RECURSIVE c(x) AS (SELECT 10 UNION ALL SELECT x + 1 FROM c WHERE x < ?) SELECT x, x FROM c`
	if res := a.Do(c, slowInsert, true, int64(1e10)); res.Err == nil {
		t.Fatalf("slow write under a 50 ms deadline: %+v", res)
	}
	waitFor(t, func() bool {
		r := a.Do(ctx(t), `SELECT COUNT(*) FROM kv`, false)
		return r.Err == nil && r.Rows == 1 && r.First == 0
	})
}

// A request still queued when its caller's deadline passes is never run.
func TestDoWithDeadlineNeverRunsAnExpiredRequest(t *testing.T) {
	a := startedKV(t, Config{}, 0)
	slow := &Request{SQL: slowCTE, Args: []any{int64(1e10)}, StmtTimeout: 200 * time.Millisecond}
	a.Submit(slow)
	time.Sleep(20 * time.Millisecond) // the connection is now busy with slow
	c, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if res := a.Do(c, `INSERT INTO kv (id, v) VALUES (1, 1)`, true); !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("queued write: %+v", res)
	}
	slow.Wait(ctx(t))
	if r := a.Do(ctx(t), `SELECT COUNT(*) FROM kv`, false); r.Err != nil || r.First != 0 {
		t.Fatalf("the expired write ran: %+v", r)
	}
}

// Off point: no deadline leaves both unset (the statement runs unbounded, as before).
func TestBoundByWithoutDeadlineSetsNothing(t *testing.T) {
	var r Request
	r.boundBy(context.Background(), time.Now())
	if !r.Deadline.IsZero() || r.StmtTimeout != 0 {
		t.Fatalf("%+v", r)
	}
	c, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r.boundBy(c, time.Now())
	if r.StmtTimeout != time.Millisecond || r.Deadline.IsZero() {
		t.Fatalf("past deadline: %+v", r)
	}
}
