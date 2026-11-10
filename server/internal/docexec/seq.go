package docexec

import (
	"sort"

	"github.com/nickstrad/mansio/server/internal/ir"
)

// The commit sequence (spec §6.5). Every document database counts its applied mutations that changed a row:
// _mansio_changes holds the count under the empty table name, and under each table's name the sequence of the
// last mutation that changed a row of that table. A request's answer carries the sequence of the state it saw
// (a query) or left (a mutation), so a client can name the snapshot its reads came from; an assert without an
// id checks that a table has not changed since a sequence (spec §5.5), which is how a client guards a select.
//
// The table is created with the database (Init). A database file from before the sequence has none: Check
// reports that, a query then answers sequence 0 without reading, and the first mutation creates the table.
// Writes that do not go through the IR are not counted.
const (
	changesTableDDL = `CREATE TABLE IF NOT EXISTS "_mansio_changes" (table_name TEXT NOT NULL PRIMARY KEY, seq INTEGER NOT NULL) STRICT, WITHOUT ROWID`
	changesExist    = `(SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = '_mansio_changes')`
	seqSelect       = `SELECT seq FROM "_mansio_changes" WHERE table_name = ''`
	changesSelect   = `SELECT table_name, seq FROM "_mansio_changes"`
	changesUpsert   = `INSERT INTO "_mansio_changes" (table_name, seq) VALUES (?, ?)`
	changesConflict = ` ON CONFLICT (table_name) DO UPDATE SET seq = excluded.seq`
)

// sequence is the database's commit sequence as r's transaction sees it, read once per request. A mutation on
// a database without the table creates it first: a write closure must not run a failing statement.
func (r *run) sequence() (int64, error) {
	if r.seqKnown {
		return r.seq, nil
	}
	if !r.st.changes {
		if !r.write {
			r.seqKnown = true
			return 0, nil
		}
		if _, err := r.tx.Exec(changesTableDDL); err != nil {
			return 0, storage(err)
		}
	}
	if !r.guards { // the common request: one row
		rows, err := r.tx.Query(seqSelect, 1)
		if err != nil {
			return 0, storage(err)
		}
		if len(rows.Values) == 1 {
			r.seq, _ = rows.Values[0][0].(int64)
		}
		r.seqKnown = true
		return r.seq, nil
	}
	// A request with a table assert reads every row once: the sequence and each table's last change.
	rows, err := r.tx.Query(changesSelect, len(r.s.Names())+1)
	if err != nil {
		return 0, storage(err)
	}
	for _, row := range rows.Values {
		name, _ := row[0].(string)
		seq, _ := row[1].(int64)
		if name == "" {
			r.seq = seq
		} else {
			r.tables[name] = seq
		}
	}
	r.seqKnown = true
	return r.seq, nil
}

// assertTable is an assert without an id (spec §5.5): the table's last change must not be after since. A row
// this request changed counts as changed at the sequence the request will leave.
func (r *run) assertTable(op *ir.AssertTable) error {
	seq, err := r.sequence()
	if err != nil {
		return err
	}
	changed := r.tables[op.Table]
	if r.touched[op.Table] {
		changed = seq + 1
	}
	if changed > op.Since {
		return protocol(ir.NewError(ir.TagConflict, "table", op.Table, "check", "unchanged", "expected", float64(op.Since), "actual", float64(changed)))
	}
	return nil
}

// commitSequence is the sequence the request answers with: for a mutation that changed a row, the next one,
// recorded in the request's transaction; otherwise the one it saw.
func (r *run) commitSequence() (int64, error) {
	seq, err := r.sequence()
	if err != nil || len(r.touched) == 0 {
		return seq, err
	}
	names := make([]string, 0, len(r.touched))
	for name := range r.touched {
		names = append(names, name)
	}
	sort.Strings(names) // one statement text per number of tables, whatever the map's order
	sql, args := changesUpsert, []any{"", seq + 1}
	for _, name := range names {
		sql += `, (?, ?)`
		args = append(args, name, seq+1)
	}
	if _, err := r.tx.Exec(sql+changesConflict, args...); err != nil {
		return 0, storage(err)
	}
	return seq + 1, nil
}
