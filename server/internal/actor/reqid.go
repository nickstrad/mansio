package actor

import (
	"encoding/binary"
	"errors"
	"time"
)

// Request ids. Each database holds the ids of the writes it applied
// in _mansio_requests, so the record travels with the file: through a crash (it commits with the write), a
// snapshot, a write-back and a move. A write with Request.RequestID claims its id inside its own savepoint
// before its statement runs: the claim inserting a row means "new, apply"; inserting nothing
// means "applied before", and the statement never runs. A write that fails is rolled back to its savepoint
// together with its claim, so only applied writes leave an id row.
//
// The table is created lazily, inside the first batch that carries an id after each open, so opening a database
// never changes its file. Retention is one batched, set-based delete on the actor, never
// row-at-a-time.

// ReservedPrefix starts the name of every table the actor keeps in a tenant database. Clients must not touch
// them: front refuses client SQL that names the prefix.
const ReservedPrefix = "_mansio_"

// RequestTableDDL creates the request-id table and the index retention uses. Every statement is idempotent.
var RequestTableDDL = []string{
	`create table if not exists _mansio_requests (
  request_id text    not null primary key,
  applied_ms integer not null,
  result     blob
) strict, without rowid`,
	`create index if not exists _mansio_requests_applied on _mansio_requests (applied_ms)`,
}

const (
	// sqlClaimRequest affects one row iff the id is new. The plan's form adds "returning request_id"; the
	// decision is the same single statement, but a RETURNING has to go through a query cursor, which measured
	// ~1.7 µs more per write (BenchmarkWritesWithRequestID), so the actor reads sqlite3_changes() instead.
	sqlClaimRequest = `insert into _mansio_requests (request_id, applied_ms) values (?, ?)
  on conflict (request_id) do nothing`
	sqlRecordResult = `update _mansio_requests set result = ? where request_id = ?`
	sqlPriorResult  = `select result from _mansio_requests where request_id = ?`
	// sqlSweepRequests deletes up to ? of the oldest ids applied before ?, through the applied_ms index.
	sqlSweepRequests = `delete from _mansio_requests where request_id in (
  select request_id from _mansio_requests where applied_ms < ? order by applied_ms limit ?)`
)

// sweepBatch bounds the rows one sweep deletes, so retention never holds a batch's write lock for long. A sweep
// that hits the bound runs again in the next batch.
const sweepBatch = 1000

// errBadPriorResult: the stored result of an applied request cannot be decoded (a bug or a damaged file).
var errBadPriorResult = errors.New("actor: request id was applied but its stored result is unreadable")

// Stored result encodings: one version byte, then the payload (intermediate-representation/spec/v1.md §7).
const (
	outcomeV1 = 1 // a SQL write: the rows it affected, as a varint
	outcomeV2 = 2 // a closure write: its Output.Body as is (for the IR, the canonical JSON of the result array)
)

// encodeOutcome is what a retry of an applied SQL write is answered with: the rows it affected.
func encodeOutcome(res Result) []byte {
	return binary.AppendVarint([]byte{outcomeV1}, res.Rows)
}

// encodeBody is what a retry of an applied closure write is answered with: its Body, byte for byte.
func encodeBody(body []byte) []byte {
	return append([]byte{outcomeV2}, body...)
}

// encodeRecord is r's stored result once it applied with res.
func encodeRecord(r *Request, res Result) []byte {
	if r.Fn != nil {
		return encodeBody(res.Body)
	}
	return encodeOutcome(res)
}

// record is a decoded stored result.
type record struct {
	closure bool   // version 0x02
	rows    int64  // version 0x01
	body    []byte // version 0x02; non-nil (empty for an empty Body)
}

// decodeRecord is the inverse of encodeOutcome and encodeBody.
//
//	post: err == nil && !rec.closure ⇒ encodeOutcome(Result{Rows: rec.rows}) == b
//	post: err == nil && rec.closure ⇒ encodeBody(rec.body) == b
func decodeRecord(b []byte) (rec record, err error) {
	switch {
	case len(b) >= 1 && b[0] == outcomeV2:
		return record{closure: true, body: b[1:]}, nil
	case len(b) >= 2 && b[0] == outcomeV1:
		rows, n := binary.Varint(b[1:])
		if n == len(b)-1 {
			return record{rows: rows}, nil
		}
	}
	return record{}, errBadPriorResult
}

// hasRequestID reports whether any write carries a request id.
func hasRequestID(writes []*Request) bool {
	for _, r := range writes {
		if r.RequestID != "" {
			return true
		}
	}
	return false
}

// sweepDue decides whether this batch should run retention: the table is known to exist, and either the last
// sweep is older than every, or it was cut short by sweepBatch.
func sweepDue(ready, more bool, last, now time.Time, every time.Duration) bool {
	return ready && (more || now.Sub(last) >= every)
}
