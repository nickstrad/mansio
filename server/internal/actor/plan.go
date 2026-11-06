package actor

import (
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
)

// This file holds the actor's pure decisions: no SQLite, no goroutines, no clock reads.

// expiredAt reports whether r's deadline passed before now.
func (r *Request) expiredAt(now time.Time) bool { return !r.Deadline.IsZero() && now.After(r.Deadline) }

// split partitions a batch at time now: requests already past their deadline, writes (run first, in one
// transaction), and reads (run after the commit). Order within each part is arrival order.
func split(batch []*Request, now time.Time) (expired, writes, reads []*Request) {
	for _, r := range batch {
		switch {
		case r.expiredAt(now):
			expired = append(expired, r)
		case r.Write:
			writes = append(writes, r)
		default:
			reads = append(reads, r)
		}
	}
	return expired, writes, reads
}

// settle decides each write's final result once its transaction has ended. aborted means the batch transaction
// ended before COMMIT (a write timed out or ended it, see writeBatch) and was rolled back; endErr is the COMMIT
// (or ROLLBACK) error.
//
//	pre:  results holds one entry per write, in order
//	post: aborted ⇒ every write that had not failed on its own reports ErrAborted
//	      !aborted && endErr != nil ⇒ every write that had not failed on its own reports endErr
func settle(results []Result, aborted bool, endErr error) {
	for i := range results {
		switch {
		case results[i].Err != nil:
		case aborted:
			results[i].Err = ErrAborted
		case endErr != nil:
			results[i].Err = endErr
		}
	}
}

// checkEnd is writeBatch's postcondition on the statement that ends a batch: COMMIT never finds the transaction
// already gone, because runInSavepoint checks after every write that it still exists. If COMMIT does report
// "no transaction is active", a write ended the transaction unnoticed and a later write may have autocommitted
// while being reported failed: a bug, not an error to return. (The message is SQLite's; this is the one place
// that reads it, and only to catch our own bug.)
func checkEnd(aborted bool, endErr error) {
	if aborted || endErr == nil { // the common path: no assertion arguments built per batch
		return
	}
	assert.That(!strings.Contains(endErr.Error(), "no transaction is active"),
		"COMMIT found no transaction: a write ended it without the savepoint check noticing: %v", endErr)
}

// IsWrite guesses whether sql writes: anything that is not SELECT, WITH … SELECT, EXPLAIN, VALUES or a read
// PRAGMA. Callers that know better set Request.Write themselves.
func IsWrite(sql string) bool {
	s := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(s, "SELECT"), strings.HasPrefix(s, "EXPLAIN"), strings.HasPrefix(s, "VALUES"):
		return false
	case strings.HasPrefix(s, "WITH"):
		for _, w := range []string{"INSERT", "UPDATE", "DELETE", "REPLACE"} {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	case strings.HasPrefix(s, "PRAGMA"):
		return strings.Contains(s, "=")
	}
	return true
}
