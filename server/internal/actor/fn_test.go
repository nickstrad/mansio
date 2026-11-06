package actor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det/dettest"
	"github.com/nickstrad/mansio/server/internal/panicx"
)

// Closure requests (fn.go): spec tables for the statement guard, fault tests for the statement order, and
// integration tests on real SQLite for the semantics IR-2's executor relies on.

func TestClosureSafe(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{`SELECT 1`, true},
		{`  select * from kv where id = ?`, true},
		{`INSERT INTO kv (id, v) VALUES (?, ?)`, true},
		{`WITH x AS (SELECT 1) SELECT * FROM x`, true},
		{`SELECT 1;`, true},           // a trailing ';' (on point)
		{`SELECT 1; -- done`, true},   // then a comment
		{`SELECT 1; /* c */ ;`, true}, // then a block comment and an empty statement
		{`SELECT ';COMMIT' FROM kv`, true},
		{`SELECT "a;b" FROM kv`, true},
		{`SELECT [a;b], ` + "`c;d`" + ` FROM kv`, true},
		{`SELECT 'it''s; fine'`, true},
		{`/* lead */ SELECT 1`, true},
		{"-- lead\nSELECT 1", true},
		{`SELECT 1 /* unterminated`, true},
		{`UPDATE kv SET v = 1 WHERE id = 1 RETURNING v`, true},
		{`ROLLBACKS`, true}, // not a keyword: SQLite rejects it on its own
		{`SELECT 1;; ;`, true},
		{`SELECT 'unterminated; COMMIT`, true}, // one (malformed) statement: SQLite rejects it
		{`SELECT [a`, true},
		// Off points.
		{``, false},
		{`   `, false},
		{`-- only a comment`, false},
		{`/* only a comment`, false},
		{`;SELECT 1`, false},
		{`'x'; COMMIT`, false}, // not starting with a keyword
		{`(SELECT 1)`, false},
		{`SELECT 1; COMMIT`, false},
		{`SELECT 1;SELECT 2`, false},
		{`SELECT 'x'; DELETE FROM kv`, false},
		{`SELECT 1; -- c` + "\nCOMMIT", false},
		{`BEGIN`, false},
		{`begin immediate`, false},
		{`COMMIT`, false},
		{`END`, false},
		{`ROLLBACK TO w`, false},
		{`rollback`, false},
		{`SAVEPOINT w`, false},
		{`RELEASE w`, false},
		{`/* x */ RELEASE w`, false},
		{"-- x\n  commit", false},
		{`PRAGMA query_only = 0`, false},
		{`ATTACH 'x' AS y`, false},
		{`DETACH y`, false},
		{`VACUUM`, false},
	} {
		if got := closureSafe(tc.sql); got != tc.want {
			t.Errorf("closureSafe(%q) = %v, want %v", tc.sql, got, tc.want)
		}
	}
}

// fnWrite is a closure write running stmts in order, answering body.
func fnWrite(body string, stmts ...string) *Request {
	return &Request{Write: true, Fn: func(tx *Tx) (Output, error) {
		for _, q := range stmts {
			if _, err := tx.Exec(q); err != nil {
				return Output{}, err
			}
		}
		return Output{Body: []byte(body)}, nil
	}}
}

// A closure write runs in its own savepoint between the claim and the record, like a SQL write.
func TestClosureWriteStatementOrder(t *testing.T) {
	f := &faultExec{}
	r := fnWrite(`["ok"]`, "c1", "c2")
	r.RequestID = "id-1"
	res := submitAll(t, fakeActor(t, Config{}, f, nil), r, write("w2"))
	if res[0].Err != nil || string(res[0].Body) != `["ok"]` || res[1].Err != nil {
		t.Fatalf("results %+v", res)
	}
	want := []string{"BEGIN IMMEDIATE", RequestTableDDL[0], RequestTableDDL[1],
		"SAVEPOINT w", sqlClaimRequest, "c1", "c2", sqlRecordResult, "RELEASE w",
		"SAVEPOINT w", "w2", "RELEASE w", "COMMIT"}
	if got := f.statements(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("statements:\n got %q\nwant %q", got, want)
	}
}

// A closure that fails (an error it returns, a failing statement, or a panic) rolls back only its savepoint.
func TestFailingClosureRollsBackOnlyItsSavepoint(t *testing.T) {
	own := errors.New("conflict")
	stmtErr := errors.New("constraint")
	for _, tc := range []struct {
		name  string
		fn    Func
		check func(error) bool
	}{
		{"returned error", func(tx *Tx) (Output, error) {
			tx.Exec("c1")
			return Output{}, own
		}, func(err error) bool { return errors.Is(err, own) }},
		{"failing statement", func(tx *Tx) (Output, error) {
			_, err := tx.Exec("bad")
			return Output{}, err
		}, func(err error) bool { return errors.Is(err, stmtErr) }},
		{"panic", func(tx *Tx) (Output, error) {
			tx.Exec("c1")
			panic("bug")
		}, func(err error) bool {
			var p *panicx.Panic
			return errors.As(err, &p) && p.Value == "bug" && strings.Contains(p.Stack, "fn_test.go")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &faultExec{FailOn: map[string]error{"bad": stmtErr}}
			res := submitAll(t, fakeActor(t, Config{}, f, nil), write("w1"), &Request{Write: true, Fn: tc.fn}, write("w3"))
			if res[0].Err != nil || !tc.check(res[1].Err) || res[2].Err != nil {
				t.Fatalf("results: %v / %v / %v", res[0].Err, res[1].Err, res[2].Err)
			}
			s := f.statements()
			if !strings.Contains(strings.Join(s, "|"), "|ROLLBACK TO w|RELEASE w|SAVEPOINT w|w3|") || s[len(s)-1] != "COMMIT" {
				t.Fatalf("statements = %v", s)
			}
		})
	}
}

// A closure statement that ends the transaction aborts the batch, as a SQL write's does (PG-01).
func TestClosureThatEndsTheTransactionAbortsTheBatch(t *testing.T) {
	f := &faultExec{EndsTxn: map[string]bool{"ends": true}}
	res := submitAll(t, fakeActor(t, Config{}, f, nil), write("w1"), fnWrite(`[]`, "ends", "after"), write("w3"))
	if !errors.Is(res[0].Err, ErrAborted) || res[1].Err == nil || errors.Is(res[1].Err, ErrAborted) || !errors.Is(res[2].Err, ErrAborted) {
		t.Fatalf("results: %v / %v / %v", res[0].Err, res[1].Err, res[2].Err)
	}
	if got := strings.Join(f.statements(), ","); got != "BEGIN IMMEDIATE,SAVEPOINT w,w1,RELEASE w,SAVEPOINT w,ends,ROLLBACK TO w,ROLLBACK" {
		t.Fatalf("statements = %s", got)
	}
}

// A read closure on the write connection runs query_only inside BEGIN … ROLLBACK; a writer that cannot be
// switched back is closed and reopened by the next batch.
func TestReadClosureOnTheWriter(t *testing.T) {
	read := func() *Request {
		return &Request{Fn: func(tx *Tx) (Output, error) {
			_, err := tx.Query("r1", 1)
			return Output{Body: []byte(`[1]`)}, err
		}}
	}
	f := &faultExec{}
	res := submitAll(t, fakeActor(t, Config{}, f, nil), read())
	if res[0].Err != nil || string(res[0].Body) != `[1]` {
		t.Fatalf("%+v", res[0])
	}
	want := "PRAGMA query_only = 1,BEGIN,r1,ROLLBACK,PRAGMA query_only = 0"
	if got := strings.Join(f.statements(), ","); got != want {
		t.Fatalf("statements = %s, want %s", got, want)
	}

	t.Run("setup fails", func(t *testing.T) {
		boom := errors.New("busy")
		f := &faultExec{FailRun: map[string]error{"BEGIN": boom}}
		res := submitAll(t, fakeActor(t, Config{}, f, nil), read())
		if !errors.Is(res[0].Err, boom) || strings.Contains(strings.Join(f.statements(), ","), "r1") {
			t.Fatalf("%v, statements %v", res[0].Err, f.statements())
		}
	})
	t.Run("stuck query_only", func(t *testing.T) {
		f := &faultExec{FailRun: map[string]error{"PRAGMA query_only = 0": errors.New("oom")}}
		a := fakeActor(t, Config{}, f, nil)
		if res := submitAll(t, a, read()); res[0].Err != nil {
			t.Fatal(res[0].Err)
		}
		delete(f.FailRun, "PRAGMA query_only = 0")
		if res := a.Do(context.Background(), "w", true); res.Err != nil || !res.Reopened || a.Opens() != 2 {
			t.Fatalf("next write: %+v, opens %d (want a reopened writer)", res, a.Opens())
		}
	})
}

// Integration: closures on real SQLite.

// count reads count(*) from kv.
func count(t *testing.T, a *Actor) int64 {
	t.Helper()
	r := a.Do(ctx(t), `SELECT count(*) FROM kv`, false)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	return r.First
}

func TestClosureWriteRunsSeveralStatementsAndReadsItsOwnWrites(t *testing.T) {
	a := startedKV(t, Config{}, 2)
	res := a.DoFn(ctx(t), true, "", func(tx *Tx) (Output, error) {
		if _, err := tx.Exec(`INSERT INTO kv (id, v) VALUES (?, ?)`, 3, 30); err != nil {
			return Output{}, err
		}
		n, err := tx.Exec(`UPDATE kv SET v = v + 1 WHERE id IN (1, 3)`)
		if err != nil || n != 2 {
			return Output{}, fmt.Errorf("update: %d, %v", n, err)
		}
		rows, err := tx.Query(`SELECT id, v FROM kv ORDER BY id`, 10)
		if err != nil {
			return Output{}, err
		}
		return Output{Body: []byte(fmt.Sprint(rows.Values)), Value: tx.Statements()}, nil
	})
	if res.Err != nil || string(res.Body) != "[[1 11] [2 20] [3 31]]" || res.Value != 3 || res.Duplicate {
		t.Fatalf("%+v", res)
	}
	if n := count(t, a); n != 3 {
		t.Fatalf("count %d", n)
	}
}

func TestClosureDuplicateAnswersTheStoredBodyWithoutRunning(t *testing.T) {
	clk := dettest.NewClock(time.Unix(1_000_000, 0))
	a := startedKV(t, Config{Clock: clk}, 0)
	runs := 0
	fn := func(v int) Func {
		return func(tx *Tx) (Output, error) {
			runs++
			if _, err := tx.Exec(`INSERT INTO kv (id, v) VALUES (?, ?)`, v, tx.Now().UnixMilli()); err != nil {
				return Output{}, err
			}
			return Output{Body: fmt.Appendf(nil, `[%d]`, v), Value: "fresh"}, nil
		}
	}
	first := a.DoFn(ctx(t), true, "req-1", fn(1))
	if first.Err != nil || string(first.Body) != "[1]" || first.Value != "fresh" || first.Duplicate {
		t.Fatalf("first: %+v", first)
	}
	clk.Advance(time.Minute)
	again := a.DoFn(ctx(t), true, "req-1", fn(2)) // a different payload: the first answer still wins (spec §7)
	if again.Err != nil || string(again.Body) != "[1]" || again.Value != nil || !again.Duplicate || runs != 1 {
		t.Fatalf("duplicate: %+v, runs %d", again, runs)
	}
	// One clock reading: the row's _creationTime stand-in and the id's applied_ms are the same instant.
	r := a.Do(ctx(t), `SELECT (SELECT v FROM kv WHERE id = 1) = (SELECT applied_ms FROM _mansio_requests WHERE request_id = 'req-1')
	  AND (SELECT applied_ms FROM _mansio_requests) = ?`, false, time.Unix(1_000_000, 0).UnixMilli())
	if r.Err != nil || r.First != 1 || count(t, a) != 1 {
		t.Fatalf("clock reading not shared: %+v, count %d", r, count(t, a))
	}
	// The stored record is version 0x02 + the body, byte for byte.
	q := &Request{SQL: `SELECT hex(result) FROM _mansio_requests WHERE request_id = 'req-1'`, MaxRows: 1}
	if err := a.Submit(q); err != nil {
		t.Fatal(err)
	}
	if got, want := q.Wait(ctx(t)), fmt.Sprintf("%X", encodeBody([]byte("[1]"))); got.Err != nil || got.Values[0][0] != want {
		t.Fatalf("record %+v, want %s", got, want)
	}
}

// A failed closure leaves no trace: no rows, no id row; a retry with the same id runs again.
func TestFailedClosureLeavesNoTraceAndItsIDStaysUnused(t *testing.T) {
	a := startedKV(t, Config{}, 0)
	conflict := errors.New("ConflictError")
	res := a.DoFn(ctx(t), true, "req-1", func(tx *Tx) (Output, error) {
		tx.Exec(`INSERT INTO kv (id, v) VALUES (1, 1)`)
		return Output{}, conflict
	})
	if !errors.Is(res.Err, conflict) || count(t, a) != 0 {
		t.Fatalf("%+v, count %d", res, count(t, a))
	}
	if ids := idList(t, a, ctx(t)); ids != "" {
		t.Fatalf("id rows after a failed closure: %q", ids)
	}
	res = a.DoFn(ctx(t), true, "req-1", func(tx *Tx) (Output, error) {
		_, err := tx.Exec(`INSERT INTO kv (id, v) VALUES (1, 1)`)
		return Output{Body: []byte(`[]`)}, err
	})
	if res.Err != nil || res.Duplicate || count(t, a) != 1 {
		t.Fatalf("retry: %+v", res)
	}
}

// SQL writes and closures share one id space; each kind refuses the other's record.
func TestRequestIDKindMismatch(t *testing.T) {
	a := startedKV(t, Config{}, 0)
	r := &Request{SQL: `INSERT INTO kv (id, v) VALUES (1, 1)`, Write: true, RequestID: "sql-1"}
	a.Submit(r)
	if res := r.Wait(ctx(t)); res.Err != nil {
		t.Fatal(res.Err)
	}
	if res := a.DoFn(ctx(t), true, "fn-1", func(*Tx) (Output, error) { return Output{Body: []byte(`[]`)}, nil }); res.Err != nil {
		t.Fatal(res.Err)
	}
	if res := a.DoFn(ctx(t), true, "sql-1", func(*Tx) (Output, error) { return Output{}, nil }); !errors.Is(res.Err, ErrRecordKind) {
		t.Fatalf("closure with a SQL write's id: %+v", res)
	}
	r = &Request{SQL: `INSERT INTO kv (id, v) VALUES (2, 2)`, Write: true, RequestID: "fn-1"}
	a.Submit(r)
	if res := r.Wait(ctx(t)); !errors.Is(res.Err, ErrRecordKind) || count(t, a) != 1 {
		t.Fatalf("SQL write with a closure's id: %+v", res)
	}
}

// A read closure never writes: Exec is refused, a writing Query fails, and nothing commits; on the writer and
// on the read pool.
func TestReadClosureCannotWrite(t *testing.T) {
	for _, readConns := range []int{0, 1} {
		t.Run(fmt.Sprintf("read_conns=%d", readConns), func(t *testing.T) {
			a := startedKV(t, Config{ReadConns: readConns}, 2)
			var execErr, queryErr error
			res := a.DoFn(ctx(t), false, "ignored", func(tx *Tx) (Output, error) {
				_, execErr = tx.Exec(`DELETE FROM kv`)
				_, queryErr = tx.Query(`DELETE FROM kv RETURNING id`, 10)
				rows, err := tx.Query(`SELECT count(*) FROM kv`, 1)
				if err != nil {
					return Output{}, err
				}
				return Output{Value: rows.Values[0][0]}, nil
			})
			if res.Err != nil || res.Value != int64(2) || !readTx(t, a).ReadOnly() {
				t.Fatalf("%+v", res)
			}
			if !errors.Is(execErr, ErrReadOnly) || queryErr == nil {
				t.Fatalf("exec %v, query %v: both must be refused", execErr, queryErr)
			}
			if n := count(t, a); n != 2 {
				t.Fatalf("count %d after a read closure", n)
			}
			// The writer is writable again afterwards.
			if r := a.Do(ctx(t), `DELETE FROM kv WHERE id = 1`, true); r.Err != nil || r.Rows != 1 {
				t.Fatalf("write after read closure: %+v", r)
			}
		})
	}
}

// readTx returns the Tx a read closure on a sees (for its flags).
func readTx(t *testing.T, a *Actor) *Tx {
	t.Helper()
	var got *Tx
	if res := a.DoFn(ctx(t), false, "", func(tx *Tx) (Output, error) { got = tx; return Output{}, nil }); res.Err != nil {
		t.Fatal(res.Err)
	}
	return got
}

// A read closure sees one snapshot: a write committed between its two queries is invisible to the second.
func TestReadClosureSeesOneSnapshot(t *testing.T) {
	a := startedKV(t, Config{ReadConns: 1}, 2)
	var before, after any
	res := a.DoFn(ctx(t), false, "", func(tx *Tx) (Output, error) {
		r1, err := tx.Query(`SELECT count(*) FROM kv`, 1)
		if err != nil {
			return Output{}, err
		}
		before = r1.Values[0][0]
		// On the read pool the writer is free: commit a row now (a test-only breach of the "don't touch the
		// actor" contract, safe here because this closure is not on the run goroutine).
		if w := a.Do(context.Background(), `INSERT INTO kv (id, v) VALUES (3, 30)`, true); w.Err != nil {
			return Output{}, w.Err
		}
		r2, err := tx.Query(`SELECT count(*) FROM kv`, 1)
		if err != nil {
			return Output{}, err
		}
		after = r2.Values[0][0]
		return Output{}, nil
	})
	if res.Err != nil || before != int64(2) || after != int64(2) || count(t, a) != 3 {
		t.Fatalf("%+v: before %v after %v (want one snapshot), count %d", res, before, after, count(t, a))
	}
}

// A closure cannot leave its savepoint; a dead Tx and an unbounded query are refused.
func TestClosureGuards(t *testing.T) {
	a := startedKV(t, Config{}, 1)
	var kept *Tx
	var errs []error
	res := a.DoFn(ctx(t), true, "", func(tx *Tx) (Output, error) {
		kept = tx
		for _, q := range []string{`COMMIT`, `RELEASE w`, `ROLLBACK`, `SAVEPOINT x`, `BEGIN`, `PRAGMA query_only = 1`,
			`DELETE FROM kv; COMMIT`} {
			_, err := tx.Exec(q)
			errs = append(errs, err)
		}
		_, err := tx.Query(`COMMIT`, 1)
		errs = append(errs, err)
		_, err = tx.Query(`SELECT 1`, 0)
		if !errors.Is(err, ErrMaxRows) {
			return Output{}, fmt.Errorf("maxRows 0: %v", err)
		}
		if tx.Statements() != 0 {
			return Output{}, fmt.Errorf("refused statements counted: %d", tx.Statements())
		}
		_, err = tx.Exec(`UPDATE kv SET v = 99`)
		return Output{}, err
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	for i, err := range errs {
		if !errors.Is(err, ErrTxControl) {
			t.Errorf("statement %d: %v, want ErrTxControl", i, err)
		}
	}
	if _, err := kept.Exec(`DELETE FROM kv`); !errors.Is(err, ErrTxDone) {
		t.Fatalf("Tx after return: %v", err)
	}
	if r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = 1`, false); r.First != 99 {
		t.Fatalf("the closure's own write did not commit: %+v", r)
	}
}

// StmtTimeout bounds the whole closure: a slow statement inside it is interrupted and aborts the batch.
func TestClosureStatementTimeout(t *testing.T) {
	a := newKV(t, Config{}, 1)
	slow := &Request{Write: true, StmtTimeout: 50 * time.Millisecond, Fn: func(tx *Tx) (Output, error) {
		if _, err := tx.Exec(`UPDATE kv SET v = 5`); err != nil {
			return Output{}, err
		}
		_, err := tx.Query(slowCTE, 1, 1<<40)
		return Output{}, err
	}}
	res := submitAll(t, a, slow)
	if res[0].Err == nil || !res[0].TimedOut {
		t.Fatalf("%+v", res[0])
	}
	if r := a.Do(ctx(t), `SELECT v FROM kv WHERE id = 1`, false); r.First != 10 {
		t.Fatalf("timed-out closure's write committed: %+v", r)
	}
}
