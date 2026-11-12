package actor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/panicx"
)

// Closure requests. A Request with Fn set runs a Go function on the actor's
// connection instead of one SQL string, so a request can run several statements, read rows and return a
// result, as one unit:
//
//   - a write (Request.Write) runs inside its own savepoint in the group-commit batch, exactly where a SQL write
//     runs: group commit, Deadline expiry, StmtTimeout, mailbox shedding, the request id (claimed before Fn
//     runs, recorded after it) and the savepoint rollback all apply unchanged. Fn returning an error rolls back
//     only its savepoint (its own statements and its id claim); its batch-mates still commit.
//   - a read runs in one read transaction (BEGIN … ROLLBACK) after the batch's writes, on the read pool when
//     there is one, so every statement sees one snapshot of committed state, and it can never write: Tx.Exec is
//     refused, the connection is query_only, and the transaction is rolled back, never committed.
//
// A write's request id records the closure's Output.Body as version 0x02 followed by the bytes (intermediate-representation/spec/v1.md
// §7: canonical JSON of the result array); a retry with the same id does not run Fn and is answered with that
// Body and Duplicate set.

// Func is a closure request's body. It runs on the actor goroutine with exclusive use of the connection.
//
// Contract (the caller's; the actor checks what is cheap to check and documents the rest):
//
//	pre: Fn uses only tx for database work, and does not keep tx (or rows' slices it was not given) after it
//	     returns: a Tx is dead once Fn returns
//	pre: Fn does not touch this actor (Submit/Do on it would deadlock: the actor is busy running Fn) and does no
//	     slow I/O: a write holds the database's write lock for its whole batch
//	pre: Fn is deterministic given the database state and tx.Now(): it reads no other clock and no randomness
//	     (request ids replay the stored Body, and a retry after RequestTTL runs Fn again)
//	pre: with a request id, Output.Body is the canonical JSON of the result (intermediate-representation/spec/v1.md §9)
//	pre: Fn creates no TEMP objects (refused when the guard sees TEMP/TEMPORARY in a CREATE): a temp trigger or
//	     table would outlive the closure on the long-lived connection
//	post (actor): Fn's transaction-control statements are refused (ErrTxControl), so Fn cannot leave or end its
//	     savepoint; in a write closure the first statement error poisons the Tx: every later statement answers
//	     that error and the request fails with it even if Fn returns nil (a statement that ended the transaction,
//	     INSERT OR ROLLBACK, RAISE(ROLLBACK), an I/O error or an interrupt, would otherwise let later statements
//	     autocommit); such an error aborts the batch as for a SQL write; a panic in Fn is recovered and answered
//	     as a *panicx.Panic error (a bug), with Fn's statements rolled back
type Func func(tx *Tx) (Output, error)

// Output is what a closure request answers.
type Output struct {
	// Body is the result as bytes (for the IR: the canonical JSON of the result array). With a request id it
	// is stored and answered again, byte for byte, on a duplicate (Result.Body).
	Body []byte
	// Value is an optional in-process value for the caller (Result.Value). It is never stored: a duplicate
	// answers Body only, with Value nil.
	Value any
}

// Rows is a closure's query result: column names and one slice of copied values per row (int64, float64,
// string, []byte or nil), as Result.Columns and Result.Values.
type Rows struct {
	Columns []string
	Values  [][]any
}

var (
	// ErrReadOnly: a read closure (Request.Write false) tried to write.
	ErrReadOnly = errors.New("actor: write refused in a read-only closure")
	// ErrTxControl: a closure statement would control the transaction (BEGIN, COMMIT, ROLLBACK, SAVEPOINT,
	// RELEASE, END), change connection state (PRAGMA, ATTACH, DETACH, VACUUM) or hold more than one statement.
	// The connection owner owns the transaction; a closure must not leave its savepoint.
	ErrTxControl = errors.New("actor: transaction control or multiple statements refused in a closure")
	// ErrTxDone: a closure used its Tx after returning.
	ErrTxDone = errors.New("actor: closure transaction used after the closure returned")
	// ErrMaxRows: Tx.Query was called without a positive row bound (every result set is bounded).
	ErrMaxRows = errors.New("actor: closure query needs maxRows > 0")
	// ErrRecordKind: a request id was claimed by a request of the other kind (a SQL write and a closure share
	// one id space), so its stored result cannot answer this one. Nothing ran. The claim may be a batch-mate's
	// that was then rolled back with its batch (the answer is an own error, so it stays): a retry may then
	// succeed. Callers map it as a client error (the id was reused across kinds), not a server fault.
	ErrRecordKind = errors.New("actor: request id was applied by a request of another kind")
)

// Tx is a closure's handle on its connection, valid only while the closure runs. Not safe for concurrent use
// (a closure runs on the actor goroutine; it must not hand tx to another goroutine).
type Tx struct {
	k        executor
	ctx      context.Context // bounded by the request's StmtTimeout
	now      time.Time
	readOnly bool
	done     atomic.Bool // set when the closure returns; a stray use from any goroutine then answers ErrTxDone
	failed   error       // a write closure's first statement error: it poisons the Tx (see Func)
	stmts    int
	inc      uint64
}

// Incarnation is the actor's Config.Incarnation: which restore of the database file the closure runs on.
func (tx *Tx) Incarnation() uint64 { return tx.inc }

// Now is the request's one clock reading (the actor's Config.Clock): for a write, the batch's time, which is
// also the request id's applied_ms; for a read, when it started. Use it for _creationTime (intermediate-representation/spec/v1.md §6.3).
func (tx *Tx) Now() time.Time { return tx.now }

// ReadOnly reports whether this is a read closure (Exec is refused).
func (tx *Tx) ReadOnly() bool { return tx.readOnly }

// Statements is how many statements the closure has run (refused ones not counted).
func (tx *Tx) Statements() int { return tx.stmts }

// Exec runs one writing statement and reports the rows it affected. Errors are the driver's (or ErrReadOnly,
// ErrTxControl, ErrTxDone), unwrapped, so the caller classifies them as for Result.Err.
func (tx *Tx) Exec(sql string, args ...any) (int64, error) {
	if err := tx.check(sql); err != nil {
		return 0, err
	}
	if tx.readOnly {
		return 0, ErrReadOnly
	}
	tx.stmts++
	res := tx.k.exec(tx.ctx, &Request{SQL: sql, Args: args, Write: true})
	tx.fail(res.Err)
	return res.Rows, res.Err
}

// Query runs one statement and returns up to maxRows rows; one more row fails it with ErrTooManyRows. In a read
// closure a writing statement (INSERT … RETURNING) fails: the connection is query_only.
//
//	pre: maxRows > 0 (else ErrMaxRows)
func (tx *Tx) Query(sql string, maxRows int, args ...any) (Rows, error) {
	if err := tx.check(sql); err != nil {
		return Rows{}, err
	}
	if maxRows <= 0 {
		return Rows{}, ErrMaxRows
	}
	tx.stmts++
	res := tx.k.exec(tx.ctx, &Request{SQL: sql, Args: args, MaxRows: maxRows})
	tx.fail(res.Err)
	return Rows{Columns: res.Columns, Values: res.Values}, res.Err
}

// Err is the error that poisoned a write closure's Tx (its first statement error), or nil.
func (tx *Tx) Err() error { return tx.failed }

// fail poisons a write closure's Tx with its first statement error. A read closure is not poisoned: it runs in
// a transaction that is rolled back whatever happens, on a connection that cannot write.
func (tx *Tx) fail(err error) {
	if err != nil && !tx.readOnly && tx.failed == nil {
		tx.failed = err
	}
}

func (tx *Tx) check(sql string) error {
	switch {
	case tx.done.Load():
		return ErrTxDone
	case tx.failed != nil:
		return tx.failed
	case !closureSafe(sql):
		return ErrTxControl
	}
	return nil
}

// runFn runs r.Fn on k (after r.Check, if any, on the same transaction). A panic is recovered into a
// *panicx.Panic error (the statements it ran are rolled back by the caller with the rest of its savepoint or
// read transaction).
//
//	post: the Tx given to Fn is dead (every later use answers ErrTxDone)
//	post: a statement error poisoned the Tx ⇒ res.Err != nil, even if Fn returned nil
func runFn(ctx context.Context, k executor, r *Request, now time.Time, inc uint64) Result {
	return withTx(ctx, k, !r.Write, now, inc, r.Fn)
}

// runCheck runs r.Check on k in a read-only Tx (Exec refused); nil when there is none.
func runCheck(ctx context.Context, k executor, r *Request, now time.Time, inc uint64) error {
	if r.Check == nil {
		return nil
	}
	return withTx(ctx, k, true, now, inc, func(tx *Tx) (Output, error) { return Output{}, r.Check(tx) }).Err
}

// withTx gives fn a fresh Tx on k (inc is the actor's Config.Incarnation) and kills it afterwards.
func withTx(ctx context.Context, k executor, readOnly bool, now time.Time, inc uint64, fn Func) (res Result) {
	tx := &Tx{k: k, ctx: ctx, now: now, readOnly: readOnly, inc: inc}
	defer func() {
		tx.done.Store(true)
		if p := panicx.From(recover()); p != nil {
			res = Result{Err: p}
		}
	}()
	out, err := fn(tx)
	switch {
	case err != nil:
		return Result{Err: err}
	case tx.failed != nil: // Fn swallowed a statement error: the transaction may be gone
		return Result{Err: tx.failed}
	}
	return Result{Body: out.Body, Value: out.Value}
}

// readFn runs a read closure in one read transaction on k: BEGIN (deferred: the first read pins the snapshot)
// … ROLLBACK, so nothing it did can commit. onWriter is set when k is the write connection, which is not
// query_only by itself: it is switched to query_only around the closure, so a writing statement fails instead
// of taking the write lock. ok is false if the connection could not be put back (query_only stuck on): the
// caller closes it.
//
//	pre:  no transaction is open on k
//	post: no transaction is open on k, and nothing the closure did is committed
func readFn(ctx context.Context, k executor, r *Request, now time.Time, inc uint64, onWriter bool) (res Result, ok bool) {
	bg := context.Background() // setup and cleanup run whatever the closure's deadline
	if onWriter {
		if err := k.run(bg, "PRAGMA query_only = 1"); err != nil {
			return Result{Err: err}, true
		}
		defer func() { ok = k.run(bg, "PRAGMA query_only = 0") == nil }()
	}
	if err := k.run(bg, "BEGIN"); err != nil {
		return Result{Err: err}, true
	}
	if err := runCheck(ctx, k, r, now, inc); err != nil {
		res = Result{Err: err}
	} else {
		res = runFn(ctx, k, r, now, inc)
	}
	_ = k.run(bg, "ROLLBACK") // fails only if a statement already ended the transaction: nothing left open
	return res, true
}

// closureSafe reports whether sql is one statement that does not control the transaction or the connection:
// its first word is not BEGIN, COMMIT, END, ROLLBACK, SAVEPOINT, RELEASE, PRAGMA, ATTACH, DETACH or VACUUM,
// a CREATE has no unquoted word TEMP or TEMPORARY (a temp object would outlive the closure on the connection),
// and nothing but whitespace, comments and semicolons follows its first top-level ';'. Comments and quoted
// strings and identifiers are skipped, so a ';' inside them does not count. One pass, no allocation beyond the
// first word.
func closureSafe(sql string) bool {
	i := skipTrivia(sql, 0)
	j := i
	for j < len(sql) && isWordByte(sql[j]) {
		j++
	}
	first := strings.ToUpper(sql[i:j])
	if j == i || txControl[first] { // empty, or not starting with a keyword
		return false
	}
	create := first == "CREATE"
	for k := j; k < len(sql); {
		switch c := sql[k]; {
		case create && isWordByte(c): // CREATE TEMP …, CREATE TABLE temp.x …: refused
			w := k
			for k < len(sql) && isWordByte(sql[k]) {
				k++
			}
			if word := strings.ToUpper(sql[w:k]); word == "TEMP" || word == "TEMPORARY" {
				return false
			}
		case c == ';':
			for k = skipTrivia(sql, k+1); k < len(sql) && sql[k] == ';'; k = skipTrivia(sql, k+1) {
			}
			return k == len(sql)
		case c == '\'' || c == '"' || c == '`' || c == '[':
			k = skipQuoted(sql, k)
		case strings.HasPrefix(sql[k:], "--") || strings.HasPrefix(sql[k:], "/*"):
			k = skipTrivia(sql, k)
		default:
			k++
		}
	}
	return true
}

// skipTrivia returns the index of the first byte at or after i that is not whitespace or inside a comment (an
// unterminated comment runs to the end).
func skipTrivia(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\f' || s[i] == '\v':
			i++
		case strings.HasPrefix(s[i:], "--"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				return len(s)
			}
			i += j
		case strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return len(s)
			}
			i += j + 4
		default:
			return i
		}
	}
	return i
}

// skipQuoted returns the index just past the quoted string or identifier opening at s[i] ('…', "…", `…` with
// doubled-quote escapes, or […]); an unterminated one runs to the end.
//
//	pre: s[i] is one of ' " ` [
func skipQuoted(s string, i int) int {
	closing := s[i]
	if closing == '[' {
		closing = ']'
	}
	for i++; i < len(s); i++ {
		if s[i] != closing {
			continue
		}
		if closing != ']' && i+1 < len(s) && s[i+1] == closing { // a doubled quote is an escaped one
			i++
			continue
		}
		return i + 1
	}
	return len(s)
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// txControl are the first keywords a closure statement may not have.
var txControl = map[string]bool{
	"BEGIN": true, "COMMIT": true, "END": true, "ROLLBACK": true, "SAVEPOINT": true, "RELEASE": true,
	"PRAGMA": true, "ATTACH": true, "DETACH": true, "VACUUM": true,
}
