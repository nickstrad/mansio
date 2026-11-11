package docexec

import (
	"fmt"
	"maps"
	"slices"

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
	// One statement reads it all: the sequence and each table's last change (at most a row per table).
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

// The sequence's floor (spec §6.5). Like _version (version.go), the sequence must not count on from a restored
// value: a database rolled back by a kill -9 would answer the lost mutations' sequences again, for other
// states, and a client that ran a find before the crash would pass its assert {unchangedSince} against a table
// holding other documents than the ones it saw. So the sequence takes the incarnation's floor (State.Floor,
// the one _version takes), and three rules keep a sequence the name of one state for good:
//
//   - a mutation that changes a row leaves max(seq + 1, floor): what a restored database answers is above
//     every sequence any earlier incarnation answered (floor), lost ones included;
//   - the first such mutation of an incarnation (the one that finds the sequence below the floor) marks every
//     table changed at the floor, not only the ones it wrote. A table not written since the restore keeps the
//     last change the file has, which is at most the restored sequence: without the mark, a guard taken on
//     lost state (a sequence between the restored one and the floor) would hold against it;
//   - until that mutation, such a guard names a sequence above the database's own, and that is a conflict by
//     itself (unreached): the database never reached it, so what the client saw is not what is here.
//
// The price: a guard taken before a restart conflicts once after it, also when the restart lost nothing (the
// lost range cannot be told from the kept one once the sequence has moved on; a client reads again). The
// limits are the version floor's (no floor without a store; 2^21 stamps; an operator restore is not seen).

// next is the sequence a mutation that changes a row leaves, seq being the one it started from.
func (r *run) next(seq int64) int64 { return max(seq+1, int64(r.st.Floor)) }

// assertTable is an assert without an id (spec §5.5): the table's last change must not be after since, and
// the database must have reached since. A row this request changed counts as changed at the sequence the
// request will leave.
func (r *run) assertTable(op *ir.AssertTable) error {
	seq, err := r.sequence()
	if err != nil {
		return err
	}
	if changed, holds := r.unchangedSince(op, seq); !holds {
		return protocol(ir.NewError(ir.TagConflict, "table", op.Table, "check", "unchanged", "expected", float64(op.Since), "actual", float64(changed)))
	}
	return nil
}

// unchangedSince reports whether op's precondition holds as the request sees its table now, and the table's
// last change, seq being the sequence the request started from. It does not hold when the table changed
// after op.Since, nor when op.Since is a sequence the database has not reached (only a state lost in a
// rollback can have answered it).
//
//	pre: seq is r.sequence()'s answer
func (r *run) unchangedSince(op *ir.AssertTable, seq int64) (changed int64, holds bool) {
	changed = r.tables[op.Table]
	if r.touched[op.Table] {
		changed = r.next(seq)
	}
	return changed, changed <= op.Since && op.Since <= seq
}

// commitSequence is the sequence the request answers with: for a mutation that changed a row, the next one,
// recorded in the request's transaction; otherwise the one it saw.
func (r *run) commitSequence() (int64, error) {
	seq, err := r.sequence()
	if err != nil || len(r.touched) == 0 {
		return seq, err
	}
	next := r.next(seq)
	if next > ir.MaxSafeInteger {
		return 0, storage(fmt.Errorf("docexec: no commit sequence is left to issue (the database is at %d, its floor at %.0f); nothing written", seq, r.st.Floor))
	}
	var names []string
	if seq < int64(r.st.Floor) {
		names = slices.Clone(r.s.Names()) // the incarnation's first change: every table counts as changed
	} else {
		names = slices.Collect(maps.Keys(r.touched))
	}
	slices.Sort(names) // one statement text per number of tables, whatever the map's order
	sql, args := changesUpsert, []any{"", next}
	for _, name := range names {
		sql += `, (?, ?)`
		args = append(args, name, next)
	}
	if _, err := r.tx.Exec(sql+changesConflict, args...); err != nil {
		return 0, storage(err)
	}
	return next, nil
}
