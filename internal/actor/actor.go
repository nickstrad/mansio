// Package actor runs one goroutine per SQLite database ("database actor").
//
// Each actor owns its database's write connection and serves requests from a bounded mailbox, one batch at a
// time. A request is a SQL string plus arguments. Callers never block on enqueue: a full mailbox returns
// ErrMailboxFull at once. The actor never blocks on reply: every reply channel has a buffer of one.
//
// A batch runs every write in one transaction (group commit: N queued writes, one commit), answers the writes
// after the commit, then runs the reads. Reads run on the write connection (ReadConns = 0) or on a small pool of
// read connections (ReadConns > 0); in WAL mode readers run alongside each other and the writer, so a slow read
// no longer delays everything queued behind it on the same database.
//
// The driver is modernc.org/sqlite (pure Go, no cgo), so a running statement occupies a goroutine, not an OS
// thread, and the Go scheduler can preempt it: a slow query on one database cannot stall the others.
//
// A write with a request id applies at most once per database: the id is claimed in _tsdb_requests inside the
// write's savepoint, and a retry is answered from the record without running (reqid.go, CP-2).
//
// Per-request errors are returned in Result.Err as the driver produced them (or as one of this package's
// sentinels); they are data for the caller to classify, not wrapped. Errors from opening or seeding a database
// are wrapped (module "actor").
package actor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
)

var (
	ErrMailboxFull = errors.New("actor: mailbox full")
	ErrClosed      = errors.New("actor: closed")
	ErrExpired     = errors.New("actor: deadline passed while queued")
	// ErrAborted: another write in the batch ended the transaction (timeout, OR ROLLBACK, I/O error), so this
	// write was rolled back or never run. Nothing of it is committed; retrying is safe.
	ErrAborted = errors.New("actor: batch transaction aborted by another write")
	// ErrTooManyRows: a read with Request.MaxRows > 0 produced more than MaxRows rows.
	ErrTooManyRows = errors.New("actor: read returned more rows than its limit")
)

// Request is one SQL statement for one database.
type Request struct {
	SQL   string
	Args  []any
	Write bool // runs inside the batch's write transaction; reads never do

	// StmtTimeout bounds execution of this statement; SQLite is interrupted when it passes. 0 = no limit.
	StmtTimeout time.Duration
	// Deadline is when the caller stops caring. A request still queued at its deadline is answered with
	// ErrExpired without running. Zero = none.
	Deadline time.Time
	// MaxRows > 0 makes a read return its rows: column names in Result.Columns and up to MaxRows rows of
	// copied values (int64, float64, string, []byte or nil) in Result.Values. A read producing more rows stops
	// and fails with ErrTooManyRows. 0 = count rows only (Result.Rows, Result.First). Writes ignore it.
	MaxRows int
	// RequestID, on a write, makes it apply at most once per database (CP-2): a write whose id this database
	// already applied does not run and is answered with the original's Rows and Duplicate set. Ids are scoped
	// to the database and remembered for Config.RequestTTL. "" = no deduplication. Reads ignore it.
	RequestID string

	enqueued time.Time
	reply    chan Result
	onWriter bool // a read that must run on the write connection (Snapshot); never sent to the read pool
}

// Result carries the answer and where the time went.
type Result struct {
	Rows      int64 // rows returned (read) or rows affected (write)
	First     int64 // first column of the first row, if it is an integer
	Err       error
	QueueWait time.Duration // enqueue → this request starts executing
	Exec      time.Duration // time inside SQLite for this request
	Commit    time.Duration // commit time of the batch (writes only)
	Batch     int           // requests in the same batch (1 for reads on the read pool)
	Reopened  bool          // the database had been passivated and was reopened for this batch
	TimedOut  bool          // the statement timeout interrupted it
	Duplicate bool          // a write whose RequestID was applied before: it did not run; Rows is the original's
	Columns   []string      // reads with Request.MaxRows > 0: column names
	Values    [][]any       // reads with Request.MaxRows > 0: one slice per row, len(Values) == Rows
}

// boundBy makes ctx's deadline, if any, r's Deadline and StmtTimeout. The timeout is measured from now, so it
// also covers time spent queued; a deadline already past still gets 1 ms (the queue check answers ErrExpired).
func (r *Request) boundBy(ctx context.Context) {
	if d, ok := ctx.Deadline(); ok {
		r.Deadline = d
		r.StmtTimeout = max(time.Until(d), time.Millisecond)
	}
}

// Wait returns the result, or ctx's error if it ends first.
func (r *Request) Wait(ctx context.Context) Result {
	select {
	case res := <-r.reply:
		return res
	case <-ctx.Done():
		return Result{Err: ctx.Err()}
	}
}

// Actor owns one SQLite database.
type Actor struct {
	ID   int
	Path string
	cfg  Config
	open opener

	mailbox  chan *Request
	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
	started  atomic.Bool
	closing  atomic.Bool

	// Owned by the run goroutine (or by the caller before Start).
	w       executor      // write connection (also serves reads when ReadConns = 0); nil when closed
	readQ   chan *Request // to the read pool; nil when ReadConns = 0 or closed
	readers sync.WaitGroup

	readQueued atomic.Int64 // requests waiting for a reader; read by any goroutine
	isOpen     atomic.Bool
	opens      atomic.Int64
	commits    atomic.Uint64 // write transactions committed since New

	// Request ids (reqid.go); owned by the run goroutine.
	now       func() time.Time // clock for applied_ms and retention; time.Now outside tests
	reqReady  bool             // _tsdb_requests is known to exist in the open database
	lastSweep time.Time
	sweepMore bool // the last sweep hit sweepBatch: run again in the next batch
}

// New creates an actor; it does not open the database or start the goroutine.
func New(id int, path string, cfg Config) *Actor {
	cfg = cfg.WithDefaults()
	return &Actor{
		ID:      id,
		Path:    path,
		cfg:     cfg,
		open:    openSQLite,
		now:     time.Now,
		mailbox: make(chan *Request, cfg.MailboxSize),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Start starts the actor goroutine, opening the database first if openNow (otherwise on the first request).
func (a *Actor) Start(openNow bool) error {
	if !a.started.CompareAndSwap(false, true) {
		return errAlreadyStarted
	}
	if openNow {
		if err := a.openDB(); err != nil {
			a.started.Store(false)
			return err
		}
	}
	go a.run()
	return nil
}

// Submit enqueues r without blocking. It returns ErrMailboxFull if the mailbox is full, ErrClosed after Close.
func (a *Actor) Submit(r *Request) error {
	if a.closing.Load() {
		return ErrClosed
	}
	if r.reply == nil {
		r.reply = make(chan Result, 1)
	}
	r.enqueued = time.Now()
	select {
	case a.mailbox <- r:
		return nil
	default:
		return ErrMailboxFull
	}
}

// Do submits one statement and waits for its result or ctx. ctx's deadline also bounds the work, not only the
// wait (waiting is not working, Release It T5): it becomes the request's Deadline (still queued then → ErrExpired,
// never run) and its StmtTimeout (SQLite is interrupted when it passes), so a caller that gives up does not leave
// the statement running and holding the connection.
//
//	post: ctx has a deadline d ⇒ the statement is not running after about d (plus the interrupt latency)
func (a *Actor) Do(ctx context.Context, sql string, write bool, args ...any) Result {
	r := &Request{SQL: sql, Args: args, Write: write}
	r.boundBy(ctx)
	if err := a.Submit(r); err != nil {
		return Result{Err: err}
	}
	return r.Wait(ctx)
}

// Close stops the actor and closes the database. Requests still queued get ErrClosed.
func (a *Actor) Close() {
	a.closing.Store(true)
	a.stopOnce.Do(func() { close(a.stop) })
	if a.started.Load() {
		<-a.stopped
		return
	}
	a.drain()
	a.closeDB()
}

// IsOpen reports whether the database is open now; Opens counts opens; QueueLen is requests waiting.
func (a *Actor) IsOpen() bool  { return a.isOpen.Load() }
func (a *Actor) Opens() int64  { return a.opens.Load() }
func (a *Actor) QueueLen() int { return len(a.mailbox) + int(a.readQueued.Load()) }

// Commits counts write transactions this actor committed since New (a batch that rolled back or whose COMMIT
// failed does not count). It survives IdleClose reopens and Close. One atomic load: cheap enough to use as a
// dirty check ("any writes since the last copy?").
//
//	post: monotonic; it increases only after a COMMIT returned success, before any write in it is answered
func (a *Actor) Commits() uint64 { return a.commits.Load() }

// Snapshot writes a consistent copy of the database to path (which must not exist) with VACUUM INTO, on the
// actor's own write connection, between batches. Writes queued meanwhile wait in the mailbox; none are lost.
// It always uses the write connection: VACUUM INTO fails on a query_only reader. A ctx deadline bounds it as in
// Do.
//
//	post: err == nil ⇒ path is a complete database holding at least the first `commits` committed write
//	      transactions (commits is read before the snapshot is queued, so it may hold more)
//	post: err != nil ⇒ nothing is guaranteed about path; the caller removes it
func (a *Actor) Snapshot(ctx context.Context, path string) (commits uint64, err error) {
	commits = a.Commits()
	r := &Request{SQL: "VACUUM INTO ?", Args: []any{path}, onWriter: true}
	r.boundBy(ctx)
	if err := a.Submit(r); err != nil {
		return commits, err
	}
	return commits, r.Wait(ctx).Err
}

func (a *Actor) run() {
	defer close(a.stopped)
	batch := make([]*Request, 0, a.cfg.MaxBatch)
	var idle *time.Timer
	if a.cfg.IdleClose > 0 {
		idle = time.NewTimer(a.cfg.IdleClose)
	}
	for {
		var idleC <-chan time.Time
		if idle != nil && a.w != nil {
			idle.Reset(a.cfg.IdleClose)
			idleC = idle.C
		}
		select {
		case r := <-a.mailbox:
			batch = append(batch[:0], r)
		case <-idleC:
			a.closeDB()
			continue
		case <-a.stop:
			a.drain()
			a.closeDB()
			return
		}
	fill:
		for len(batch) < a.cfg.MaxBatch {
			select {
			case r := <-a.mailbox:
				batch = append(batch, r)
			default:
				break fill
			}
		}
		a.process(batch)
		clear(batch)
	}
}

func (a *Actor) drain() {
	for {
		select {
		case r := <-a.mailbox:
			r.reply <- Result{Err: ErrClosed}
		default:
			return
		}
	}
}

// batchMeta stamps every reply of one batch.
type batchMeta struct {
	n        int
	reopened bool
}

func (m batchMeta) reply(r *Request, res Result) {
	res.Batch, res.Reopened = m.n, m.reopened
	r.reply <- res // buffered(1): never blocks
}

func (m batchMeta) fail(r *Request, err error) {
	m.reply(r, Result{Err: err, QueueWait: time.Since(r.enqueued)})
}

// process runs one batch: writes in one transaction, then reads. Reads never run inside the write transaction,
// so a slow read cannot hold the database's write lock.
func (a *Actor) process(batch []*Request) {
	meta := batchMeta{n: len(batch)}
	if a.w == nil {
		if err := a.openDB(); err != nil {
			for _, r := range batch {
				meta.fail(r, err)
			}
			return
		}
		meta.reopened = true
	}
	expired, writes, reads := split(batch, time.Now())
	for _, r := range expired {
		meta.fail(r, ErrExpired)
	}
	if len(writes) > 0 {
		a.writeBatch(writes, meta)
	}
	for _, r := range reads {
		switch {
		case r.expiredAt(time.Now()): // may have expired while the writes ran
			meta.fail(r, ErrExpired)
		case a.readQ != nil && !r.onWriter:
			a.readQueued.Add(1)
			select {
			case a.readQ <- r:
			default: // every reader busy and their queue full: shed rather than stall the writes behind us
				a.readQueued.Add(-1)
				meta.fail(r, ErrMailboxFull)
			}
		default:
			meta.reply(r, a.run1(a.w, r))
		}
	}
}

// writeBatch runs writes in one transaction, each inside its own savepoint (SQL G-8, psql's ON_ERROR_ROLLBACK).
// A write that fails is rolled back to its savepoint, so its error stays its own and its batch-mates commit.
// A write can also end the whole transaction under us (a statement timeout interrupts it; INSERT OR ROLLBACK,
// a RAISE(ROLLBACK) trigger, or an I/O error rolls it back): the next ROLLBACK TO or RELEASE then finds no
// savepoint. From there the batch is aborted: the rest are never run (so nothing autocommits outside the
// transaction) and settle reports every write that had not failed on its own as ErrAborted.
//
//	post: every write is either committed and answered without error, or not committed and answered with one
func (a *Actor) writeBatch(writes []*Request, meta batchMeta) {
	ctx := context.Background()
	if err := a.w.run(ctx, "BEGIN IMMEDIATE"); err != nil {
		for _, r := range writes {
			meta.fail(r, err)
		}
		return
	}
	now := a.now()
	creating := !a.reqReady && hasRequestID(writes)
	if creating {
		if err := a.createRequestTable(ctx); err != nil {
			_ = a.w.run(ctx, "ROLLBACK")
			for _, r := range writes {
				meta.fail(r, err)
			}
			return
		}
	}
	results := make([]Result, len(writes))
	aborted := sweepDue(a.reqReady, a.sweepMore, a.lastSweep, now, a.cfg.RequestSweep) && a.sweep(ctx, now)
	for i, r := range writes {
		if aborted { // the transaction is gone; settle answers these ErrAborted
			results[i] = Result{QueueWait: time.Since(r.enqueued)}
			continue
		}
		results[i], aborted = a.runInSavepoint(ctx, r, now.UnixMilli())
	}
	end := "COMMIT"
	if aborted {
		end = "ROLLBACK" // usually fails with "no transaction is active"; it matters only if one is left open
	}
	c0 := time.Now()
	endErr := a.w.run(ctx, end)
	commit := time.Since(c0)
	checkEnd(aborted, endErr)
	if !aborted && endErr == nil {
		a.commits.Add(1) // before the replies: a caller that saw its write acknowledged sees it counted
		a.reqReady = a.reqReady || creating
	}
	settle(results, aborted, endErr)
	for i, r := range writes {
		results[i].Commit = commit
		meta.reply(r, results[i])
	}
}

// runInSavepoint runs one write inside SAVEPOINT w. gone reports that the batch transaction no longer exists
// (or its state is unknown), so no further statement may run in it.
//
//	pre:  a transaction is open on a.w
//	post: !gone ⇒ the transaction is still open and holds r's effects iff res.Err == nil
func (a *Actor) runInSavepoint(ctx context.Context, r *Request, nowMS int64) (res Result, gone bool) {
	if err := a.w.run(ctx, "SAVEPOINT w"); err != nil {
		return Result{Err: err, QueueWait: time.Since(r.enqueued)}, true
	}
	res = a.apply(ctx, r, nowMS)
	if res.Err != nil && a.w.run(ctx, "ROLLBACK TO w") != nil {
		return res, true
	}
	return res, a.w.run(ctx, "RELEASE w") != nil
}

// apply runs one write inside its savepoint. With a request id it first claims the id (reqid.go): a claim that
// inserts nothing is a duplicate, answered from the stored result without running the statement. The claim and
// the result record share the savepoint with the statement, so a failure anywhere rolls all three back.
//
//	pre:  a savepoint is open on a.w
//	post: res.Err == nil && !res.Duplicate ⇒ r ran and, with an id, the id row records res.Rows
//	post: res.Duplicate ⇒ res.Err == nil, r did not run and nothing changed
func (a *Actor) apply(ctx context.Context, r *Request, nowMS int64) Result {
	if r.RequestID == "" {
		return a.run1(a.w, r)
	}
	t0 := time.Now()
	claim := a.w.exec(ctx, &Request{SQL: sqlClaimRequest, Args: []any{r.RequestID, nowMS}})
	switch {
	case claim.Err != nil:
		return Result{Err: claim.Err, QueueWait: t0.Sub(r.enqueued), Exec: time.Since(t0)}
	case claim.Rows == 0:
		res := Result{Duplicate: true, QueueWait: t0.Sub(r.enqueued)}
		prior := a.w.exec(ctx, &Request{SQL: sqlPriorResult, Args: []any{r.RequestID}, MaxRows: 1})
		res.Err = prior.Err
		if prior.Err == nil {
			var b []byte
			if len(prior.Values) == 1 {
				b, _ = prior.Values[0][0].([]byte)
			}
			res.Rows, res.Err = decodeOutcome(b)
		}
		res.Exec = time.Since(t0)
		return res
	}
	res := a.run1(a.w, r)
	if res.Err == nil {
		if rec := a.w.exec(ctx, &Request{SQL: sqlRecordResult, Args: []any{encodeOutcome(res), r.RequestID}, Write: true}); rec.Err != nil {
			res.Err = rec.Err
		}
	}
	res.QueueWait, res.Exec = t0.Sub(r.enqueued), time.Since(t0)
	return res
}

// createRequestTable runs RequestTableDDL in the open batch transaction; a.reqReady is set once it commits.
func (a *Actor) createRequestTable(ctx context.Context) error {
	for _, q := range RequestTableDDL {
		if err := a.w.run(ctx, q); err != nil {
			return errs.Wrap("actor", err, "cannot create the request-id table in %s", a.Path)
		}
	}
	return nil
}

// sweep deletes request ids older than Config.RequestTTL, at most sweepBatch of them, in its own savepoint at
// the start of a write batch. A sweep that fails on its own is rolled back and retried at the next due time; it
// never fails the batch's writes. gone reports that it ended the batch transaction (see runInSavepoint).
func (a *Actor) sweep(ctx context.Context, now time.Time) (gone bool) {
	cutoff := now.Add(-a.cfg.RequestTTL).UnixMilli()
	r := &Request{SQL: sqlSweepRequests, Args: []any{cutoff, sweepBatch}, Write: true, enqueued: now}
	res, gone := a.runInSavepoint(ctx, r, 0)
	a.lastSweep, a.sweepMore = now, res.Err == nil && res.Rows >= sweepBatch
	return gone
}

// reader serves reads from the read pool until the queue is closed, then closes its connection.
func (a *Actor) reader(k executor) {
	defer a.readers.Done()
	defer k.close()
	single := batchMeta{n: 1}
	for r := range a.readQ {
		a.readQueued.Add(-1)
		if r.expiredAt(time.Now()) {
			single.fail(r, ErrExpired)
			continue
		}
		single.reply(r, a.run1(k, r))
	}
}

// run1 executes one statement under its statement timeout.
func (a *Actor) run1(k executor, r *Request) Result {
	t0 := time.Now()
	ctx := context.Background()
	if r.StmtTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.StmtTimeout)
		defer cancel()
	}
	res := k.exec(ctx, r)
	res.TimedOut = res.Err != nil && ctx.Err() != nil
	res.QueueWait = t0.Sub(r.enqueued)
	res.Exec = time.Since(t0)
	return res
}

func (a *Actor) openDB() error {
	w, err := a.open(a.Path, a.cfg, false)
	if err != nil {
		return err
	}
	a.w = w
	a.reqReady, a.sweepMore = false, false
	if n := a.cfg.ReadConns; n > 0 {
		a.readQ = make(chan *Request, a.cfg.MailboxSize)
		for range n {
			k, err := a.open(a.Path, a.cfg, true)
			if err != nil {
				a.closeDB()
				return err
			}
			a.readers.Add(1)
			go a.reader(k)
		}
	}
	a.isOpen.Store(true)
	a.opens.Add(1)
	return nil
}

func (a *Actor) closeDB() {
	if a.readQ != nil {
		close(a.readQ) // readers finish what is queued, then close their connections
		a.readers.Wait()
		a.readQ = nil
	}
	if a.w != nil {
		a.w.close()
		a.w = nil
	}
	a.isOpen.Store(false)
}
