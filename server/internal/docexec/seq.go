package docexec

// The commit sequence (spec §6.5). Every document database counts its applied mutations that changed a row:
// _mansio_changes holds the count under the empty table name. A request's answer carries the sequence of the
// state it saw (a query) or left (a mutation), so a client can name the snapshot its reads came from.
//
// The table is created with the database (Init). A database file from before the sequence has none: Check
// reports that, a query then answers sequence 0 without reading, and the first mutation creates the table.
// Writes that do not go through the IR are not counted.
const (
	changesTableDDL = `CREATE TABLE IF NOT EXISTS "_mansio_changes" (table_name TEXT NOT NULL PRIMARY KEY, seq INTEGER NOT NULL) STRICT, WITHOUT ROWID`
	changesExist    = `(SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = '_mansio_changes')`
	seqSelect       = `SELECT seq FROM "_mansio_changes" WHERE table_name = ''`
	seqUpsert       = `INSERT INTO "_mansio_changes" (table_name, seq) VALUES ('', ?) ON CONFLICT (table_name) DO UPDATE SET seq = excluded.seq`
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

// commitSequence is the sequence the request answers with: for a mutation that changed a row, the next one,
// recorded in the request's transaction; otherwise the one it saw.
func (r *run) commitSequence() (int64, error) {
	seq, err := r.sequence()
	if err != nil || !r.changed {
		return seq, err
	}
	if _, err := r.tx.Exec(seqUpsert, seq+1); err != nil {
		return 0, storage(err)
	}
	return seq + 1, nil
}
