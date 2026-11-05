package actor

import (
	"bytes"
	"errors"
	"testing"
)

// Request.MaxRows (added for the network front): on and off points of the row limit, value copying, and that
// MaxRows = 0 keeps the count-only behavior.
func TestMaxRowsCollectsUpToTheLimit(t *testing.T) {
	a := startedKV(t, Config{}, 5)
	tests := []struct {
		name    string
		sql     string
		args    []any
		max     int
		rows    int64
		err     error
		collect bool
	}{
		{"count only (MaxRows 0)", `SELECT id, v FROM kv`, nil, 0, 5, nil, false},
		{"no rows", `SELECT id, v FROM kv WHERE id > ?`, []any{99}, 1, 0, nil, true},
		{"one row, limit 1 (on point)", `SELECT id, v FROM kv WHERE id = ?`, []any{3}, 1, 1, nil, true},
		{"exactly the limit (on point)", `SELECT id, v FROM kv`, nil, 5, 5, nil, true},
		{"one over the limit (off point)", `SELECT id, v FROM kv`, nil, 4, 5, ErrTooManyRows, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Request{SQL: tc.sql, Args: tc.args, MaxRows: tc.max}
			if err := a.Submit(r); err != nil {
				t.Fatal(err)
			}
			res := r.Wait(ctx(t))
			if !errors.Is(res.Err, tc.err) || res.Rows != tc.rows {
				t.Fatalf("rows %d err %v, want %d %v", res.Rows, res.Err, tc.rows, tc.err)
			}
			if !tc.collect {
				if res.Values != nil {
					t.Fatalf("values collected: %v", res.Values)
				}
				return
			}
			if len(res.Columns) != 2 || res.Columns[0] != "id" || int64(len(res.Values)) != res.Rows {
				t.Fatalf("columns %v, %d values for %d rows", res.Columns, len(res.Values), res.Rows)
			}
			for _, row := range res.Values {
				id, v := row[0].(int64), row[1].(int64)
				if v != id*10 {
					t.Fatalf("row %v", row)
				}
			}
			if res.Rows > 0 && res.First != res.Values[0][0].(int64) {
				t.Fatalf("First %d, first row %v", res.First, res.Values[0])
			}
		})
	}
}

func TestMaxRowsCopiesTextAndBlobValues(t *testing.T) {
	a := startedKV(t, Config{}, 0)
	blob := []byte{0, 1, 2, 255}
	r := &Request{SQL: `SELECT 'x', ?, NULL, 1.5`, Args: []any{blob}, MaxRows: 1}
	if err := a.Submit(r); err != nil {
		t.Fatal(err)
	}
	res := r.Wait(ctx(t))
	if res.Err != nil || len(res.Values) != 1 {
		t.Fatalf("%+v", res)
	}
	row := res.Values[0]
	if row[0] != "x" || !bytes.Equal(row[1].([]byte), blob) || row[2] != nil || row[3] != 1.5 {
		t.Fatalf("row %#v", row)
	}
	blob[0] = 9 // the result must not alias the argument or a driver buffer
	if row[1].([]byte)[0] != 0 {
		t.Fatal("blob value aliases its source")
	}
}
