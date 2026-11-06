package actor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
)

// runExecutorContract is the executor port's contract. Every implementation the actor may be given (the real
// SQLite adapter and the test fake) must pass it, so tests written against the fake mean something.
// newExec returns an executor on a database that has table kv(id, v) with rows (1, 10) and (2, 20).
func runExecutorContract(t *testing.T, newExec func(t *testing.T) executor) {
	c := context.Background()
	t.Run("write reports rows affected", func(t *testing.T) {
		e := newExec(t)
		if r := e.exec(c, &Request{SQL: `UPDATE kv SET v = 1 WHERE id = 1`, Write: true}); r.Err != nil || r.Rows != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("read reports rows and never panics on bad SQL", func(t *testing.T) {
		e := newExec(t)
		if r := e.exec(c, &Request{SQL: `SELECT v FROM kv WHERE id = 1`}); r.Err != nil || r.Rows != 1 {
			t.Fatalf("%+v", r)
		}
		if r := e.exec(c, &Request{SQL: `SELEKT nonsense`}); r.Err == nil {
			t.Fatal("bad SQL must return an error")
		}
	})
	t.Run("BEGIN then COMMIT", func(t *testing.T) {
		e := newExec(t)
		if err := e.run(c, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		if err := e.run(c, "COMMIT"); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		name  string
		sql   string
		endOK bool // ROLLBACK TO w, RELEASE w and COMMIT succeed afterwards
	}{
		{"a savepoint undoes a failed statement and keeps the transaction", `SELEKT nonsense`, true},
		{"a statement that ends the transaction leaves no savepoint and nothing to commit", endsTxnSQL, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExec(t)
			for _, q := range []string{"BEGIN IMMEDIATE", "SAVEPOINT w"} {
				if err := e.run(c, q); err != nil {
					t.Fatal(err)
				}
			}
			if r := e.exec(c, &Request{SQL: tc.sql, Write: true}); r.Err == nil {
				t.Fatal("the statement must fail")
			}
			for _, q := range []string{"ROLLBACK TO w", "RELEASE w", "COMMIT"} {
				if err := e.run(c, q); (err == nil) != tc.endOK {
					t.Fatalf("%s: err = %v, want success %v", q, err, tc.endOK)
				}
			}
		})
	}
	t.Run("a canceled context ends a statement with an error", func(t *testing.T) {
		e := newExec(t)
		cc, cancel := context.WithTimeout(c, 20*time.Millisecond)
		defer cancel()
		r := e.exec(cc, &Request{SQL: slowCTE + ` /* SLOW */`, Args: []any{int64(1e10)}})
		if r.Err == nil || cc.Err() == nil {
			t.Fatalf("want an error after the context ended: %+v", r)
		}
	})
}

// endsTxnSQL conflicts with row 1, so SQLite rolls back the whole transaction.
const endsTxnSQL = `INSERT OR ROLLBACK INTO kv (id, v) VALUES (1, 0)`

func TestSQLiteExecutorContract(t *testing.T) {
	runExecutorContract(t, func(t *testing.T) executor {
		e, err := openSQLite(seededKV(t, Config{}, 2), Config{}.WithDefaults(), false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(e.close)
		return e
	})
}

func TestFaultExecutorContract(t *testing.T) {
	runExecutorContract(t, func(t *testing.T) executor {
		return &faultExec{
			FailOn:  map[string]error{`SELEKT nonsense`: errors.New("syntax error")},
			EndsTxn: map[string]bool{endsTxnSQL: true},
		}
	})
}

// Each key assertion can fire (mutation thinking: a check that never fails proves nothing).
func TestAssertionsCanFire(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out (-tags noassert)")
	}
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: want a panic", name)
			}
		}()
		f()
	}
	mustPanic("COMMIT finding no transaction is a bug", func() {
		checkEnd(false, errors.New("SQL logic error: cannot commit - no transaction is active (1)"))
	})
	checkEnd(true, errors.New("cannot rollback - no transaction is active")) // off point: an aborted batch's ROLLBACK
	checkEnd(false, errors.New("disk I/O error"))                            // off point: a real COMMIT failure
	mustPanic("parallel forwards a worker panic", func() {
		_ = parallel(0, 10, 3, func(i int) error {
			if i == 7 {
				panic("boom")
			}
			return nil
		})
	})
}
