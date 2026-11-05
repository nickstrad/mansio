package actor

import (
	"encoding/binary"
	"errors"
	"time"
)

// Request ids (CP-2, PG-09; art-of-postgresql plan §5.3). Each database holds the ids of the writes it applied
// in _tsdb_requests, so the record travels with the file: through a crash (it commits with the write), a
// snapshot, a write-back and a move. A write with Request.RequestID claims its id inside its own savepoint
// before its statement runs (SQL G-8, G-10): the claim inserting a row means "new, apply"; inserting nothing
// means "applied before", and the statement never runs. A write that fails is rolled back to its savepoint
// together with its claim, so only applied writes leave an id row.
//
// The table is created lazily, inside the first batch that carries an id after each open, so opening a database
// never changes its file. Retention is one batched, set-based delete on the actor (SQL G-17), never
// row-at-a-time.

// RequestTableDDL creates the request-id table and the index retention uses. Every statement is idempotent.
// The _tsdb_ prefix is reserved: front refuses client SQL that names it.
var RequestTableDDL = []string{
	`create table if not exists _tsdb_requests (
  request_id text    not null primary key,
  applied_ms integer not null,
  result     blob
) strict, without rowid`,
	`create index if not exists _tsdb_requests_applied on _tsdb_requests (applied_ms)`,
}

const (
	// sqlClaimRequest returns one row iff the id is new.
	sqlClaimRequest = `insert into _tsdb_requests (request_id, applied_ms) values (?, ?)
  on conflict (request_id) do nothing returning request_id`
	sqlRecordResult = `update _tsdb_requests set result = ? where request_id = ?`
	sqlPriorResult  = `select result from _tsdb_requests where request_id = ?`
	// sqlSweepRequests deletes up to ? of the oldest ids applied before ?, through the applied_ms index.
	sqlSweepRequests = `delete from _tsdb_requests where request_id in (
  select request_id from _tsdb_requests where applied_ms < ? order by applied_ms limit ?)`
)

// sweepBatch bounds the rows one sweep deletes, so retention never holds a batch's write lock for long. A sweep
// that hits the bound runs again in the next batch.
const sweepBatch = 1000

// errBadPriorResult: the stored result of an applied request cannot be decoded (a bug or a damaged file).
var errBadPriorResult = errors.New("actor: request id was applied but its stored result is unreadable")

// outcomeV1 tags the stored result encoding: one version byte, then the rows affected as a varint.
const outcomeV1 = 1

// encodeOutcome is what a retry of an applied write is answered with: today the rows it affected.
func encodeOutcome(res Result) []byte {
	return binary.AppendVarint([]byte{outcomeV1}, res.Rows)
}

// decodeOutcome is the inverse of encodeOutcome.
//
//	post: err == nil ⇒ encodeOutcome(Result{Rows: rows}) == b
func decodeOutcome(b []byte) (rows int64, err error) {
	if len(b) < 2 || b[0] != outcomeV1 {
		return 0, errBadPriorResult
	}
	rows, n := binary.Varint(b[1:])
	if n != len(b)-1 {
		return 0, errBadPriorResult
	}
	return rows, nil
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
