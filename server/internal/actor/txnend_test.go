package actor

import (
	"errors"
	"path/filepath"
	"testing"
)

// PG-01 / SQL G-8: a statement can end the batch's transaction on its own (INSERT OR ROLLBACK, RAISE(ROLLBACK)
// in a trigger). Every write must then be either committed and ok, or not committed and not ok — checked by
// reading the table back. Before savepoints, a write after the one that ended the transaction autocommitted
// and was reported failed (a client retry duplicated it).
func TestWriteThatEndsTheTransactionNeverMisreportsBatchMates(t *testing.T) {
	const trigger = `CREATE TRIGGER kv_veto BEFORE INSERT ON kv WHEN NEW.v = -1 BEGIN SELECT RAISE(ROLLBACK, 'veto'); END`
	tests := []struct {
		name  string
		ender string // statement that ends the transaction; inserting a duplicate of id 1 or the vetoed v
	}{
		{"INSERT OR ROLLBACK duplicate", `INSERT OR ROLLBACK INTO kv (id, v) VALUES (1, 99)`},
		{"RAISE(ROLLBACK) trigger", `INSERT INTO kv (id, v) VALUES (50, -1)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db.sqlite")
			if err := Seed(path, Config{}, kvDDL+";\n"+trigger, fillKV(1)); err != nil { // row (1, 10)
				t.Fatal(err)
			}
			a := New(0, path, Config{})
			t.Cleanup(a.Close)
			reqs := []*Request{
				{SQL: `INSERT INTO kv (id, v) VALUES (2, 20)`, Write: true},
				{SQL: tc.ender, Write: true},
				{SQL: `INSERT INTO kv (id, v) VALUES (3, 30)`, Write: true},
				{SQL: `INSERT INTO kv (id, v) VALUES (4, 40)`, Write: true},
			}
			res := submitAll(t, a, reqs...)
			if res[1].Err == nil || errors.Is(res[1].Err, ErrAborted) {
				t.Fatalf("the transaction-ending write must fail: %+v", res[1])
			}
			for i, id := range map[int]int64{0: 2, 2: 3, 3: 4} { // write index → the id it inserts
				r := a.Do(ctx(t), `SELECT COUNT(*) FROM kv WHERE id = ?`, false, id)
				if r.Err != nil {
					t.Fatal(r.Err)
				}
				committed := r.First == 1
				if committed != (res[i].Err == nil) {
					t.Errorf("write %d (id %d): committed=%v but err=%v", i, id, committed, res[i].Err)
				}
			}
			if r := a.Do(ctx(t), `SELECT COUNT(*) FROM kv WHERE id = 1 AND v = 10`, false); r.First != 1 {
				t.Errorf("seed row changed: %+v", r)
			}
		})
	}
}
