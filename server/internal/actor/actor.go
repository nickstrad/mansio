// Package actor runs one goroutine per SQLite database ("database actor").
//
// Each actor owns its database's write connection and serves requests from a bounded mailbox, one batch at a
// time. A request is a SQL string plus arguments, or a Go closure that runs several statements on the
// connection as one unit (fn.go). Callers never block on enqueue: a full mailbox returns
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
// A write with a request id applies at most once per database: the id is claimed in _mansio_requests inside the
// write's savepoint, and a retry is answered from the record without running (reqid.go).
//
// A closure write (Request.Fn) runs in the same savepoint slot as a SQL write; a closure read runs in one read
// transaction and cannot write (fn.go).
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

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/errs"
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

// Request is one SQL statement, or one closure, for one database.
type Request struct {
	SQL   string
	Args  []any
	Write bool // runs inside the batch's write transaction; reads never do
	// Fn, if set, runs instead of SQL and Args (fn.go): a write inside its own savepoint in the batch, a read in
	// one read-only snapshot transaction. Its Output lands in Result.Body and Result.Value. StmtTimeout bounds
	// the whole closure; MaxRows is ignored (each Tx.Query takes its own bound).
	Fn Func
	// Check, if set, runs first, in the request's savepoint (a write) or read transaction (a read), before the
	// request id is claimed: a precondition that must hold even for a duplicate (ir/spec/v1.md §6.1: the
	// fingerprint check runs before the duplicate check). Its Tx is read-only. Its error is the request's
	// answer, and the savepoint rolls back.
	Check func(tx *Tx) error

	// StmtTimeout bounds execution of this statement; SQLite is interrupted when it passes. 0 = no limit.
	StmtTimeout time.Duration
	// Deadline is when the caller stops caring. A request still queued at its deadline is answered with
	// ErrExpired without running. Zero = none.
	Deadline time.Time
	// MaxRows > 0 makes a read return its rows: column names in Result.Columns and up to MaxRows rows of
	// copied values (int64, float64, string, []byte or nil) in Result.Values. A read producing more rows stops
	// and fails with ErrTooManyRows. 0 = count rows only (Result.Rows, Result.First). Writes ignore it.
	MaxRows int
	// RequestID, on a write, makes it apply at most once per database (reqid.go): a write whose id this database
	// already applied does not run and is answered with the original's Rows and Duplicate set. Ids are scoped
	// to the database and remembered for Config.RequestTTL. "" = no deduplication. Reads ignore it. A closure
	// write's record is its Output.Body (version 0x02), answered in Result.Body; SQL writes and closures share
	// the id space, and an id applied by the other kind answers ErrRecordKind.
	RequestID string

	enqueued time.Time
	reply    chan Result
	backupTo string // Snapshot: copy the database to this path with the backup API instead of running SQL
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
	Body      []byte        // closure requests: Output.Body, or on a Duplicate the stored one
	Value     any           // closure requests that ran: Output.Value (nil on a Duplicate)
}

// boundBy makes ctx's deadline, if any, r's Deadline and StmtTimeout. The timeout is measured from now, so it
// also covers time spent queued; a deadline already past still gets 1 ms (the queue check answers ErrExpired).
func (r *Request) boundBy(ctx context.Context, now time.Time) {
	if d, ok := ctx.Deadline(); ok {
		r.Deadline = d
		r.StmtTimeout = max(d.Sub(now), time.Millisecond)
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
	reqReady  bool // _mansio_requests is known to exist in the open database
	lastSweep time.Time
	sweepMore bool // the last sweep hit sweepBatch: run again in the next batch

	wStuck bool // a read closure could not switch the writer back from query_only: close it after this batch
}

// New creates an actor; it does not open the database or start the goroutine.
func New(id int, path string, cfg Config) *Actor {
	cfg = cfg.WithDefaults()
	return &Actor{
		ID:      id,
		Path:    path,
		cfg:     cfg,
		open:    openSQLite,
		mailbox: make(chan *Request, cfg.MailboxSize),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Clock is the actor's clock (Config.Clock): callers that wait for its answers time the wait on it.
func (a *Actor) Clock() det.Clock { return a.cfg.Clock }

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
	r.enqueued = a.cfg.Clock.Now()
	select {
	case a.mailbox <- r:
		return nil
	default:
		a.cfg.Queues.refusedMailbox()
		return ErrMailboxFull
	}
}

// Do submits one statement and waits for its result or ctx. ctx's deadline also bounds the work, not only the wait
// (a caller that gave up must not leave work running): it becomes the request's Deadline (still queued then →
// ErrExpired, never run) and its StmtTimeout (SQLite is interrupted when it passes), so a caller that gives up does
// not leave the statement running and holding the connection.
//
//	post: ctx has a deadline d ⇒ the statement is not running after about d (plus the interrupt latency)
func (a *Actor) Do(ctx context.Context, sql string, write bool, args ...any) Result {
	r := &Request{SQL: sql, Args: args, Write: write}
	r.boundBy(ctx, a.cfg.Clock.Now())
	if err := a.Submit(r); err != nil {
		return Result{Err: err}
	}
	return r.Wait(ctx)
}

// DoFn submits one closure request (fn.go) and waits for its result or ctx, bounded by ctx as in Do. write
// picks a write (savepoint in the batch) or a read (read-only snapshot); requestID applies to writes only.
func (a *Actor) DoFn(ctx context.Context, write bool, requestID string, fn Func) Result {
	r := &Request{Fn: fn, Write: write, RequestID: requestID}
	r.boundBy(ctx, a.cfg.Clock.Now())
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

// Snapshot writes a consistent copy of the database to path (which must not exist) with SQLite's online backup
// API, from one read transaction. In WAL mode with a read pool it runs on a reader, so writes go on meanwhile
// (VACUUM INTO on the writer stalled them for the whole copy). Otherwise (no read pool, or a rollback
// journal, where a long reader would block the writer's commit) it runs on the write connection between
// batches, and writes queued meanwhile wait in the mailbox; none are lost. A ctx deadline bounds it as in Do: the
// copy checks it between steps. With a read pool it can be shed (ErrMailboxFull) like any read.
//
//	post: err == nil ⇒ path is a complete database holding at least the first `commits` committed write
//	      transactions (commits is read before the snapshot is queued, so it may hold more)
//	post: err != nil ⇒ nothing is guaranteed about path; the caller removes it
func (a *Actor) Snapshot(ctx context.Context, path string) (commits uint64, err error) {
	commits = a.Commits()
	r := &Request{backupTo: path}
	r.boundBy(ctx, a.cfg.Clock.Now())
	if err := a.Submit(r); err != nil {
		return commits, err
	}
	return commits, r.Wait(ctx).Err
}

func (a *Actor) run() {
	defer close(a.stopped)
	batch := make([]*Request, 0, a.cfg.MaxBatch)
	var idle det.Timer
	if a.cfg.IdleClose > 0 {
		idle = a.cfg.Clock.NewTimer(a.cfg.IdleClose)
	}
	for {
		var idleC <-chan time.Time
		if idle != nil && a.w != nil {
			idle.Reset(a.cfg.IdleClose)
			idleC = idle.C()
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
	clock    det.Clock
}

func (m batchMeta) reply(r *Request, res Result) {
	res.Batch, res.Reopened = m.n, m.reopened
	r.reply <- res // buffered(1): never blocks
}

func (m batchMeta) fail(r *Request, err error) {
	m.reply(r, Result{Err: err, QueueWait: det.Since(m.clock, r.enqueued)})
}

// process runs one batch: writes in one transaction, then reads. Reads never run inside the write transaction,
// so a slow read cannot hold the database's write lock.
func (a *Actor) process(batch []*Request) {
	meta := batchMeta{n: len(batch), clock: a.cfg.Clock}
	if a.w == nil {
		if err := a.openDB(); err != nil {
			for _, r := range batch {
				meta.fail(r, err)
			}
			return
		}
		meta.reopened = true
	}
	now := a.cfg.Clock.Now()
	a.cfg.Queues.pickedUp(batch, now)
	expired, writes, reads := split(batch, now)
	for _, r := range expired {
		meta.fail(r, ErrExpired)
	}
	if len(writes) > 0 {
		a.writeBatch(writes, meta)
	}
	for _, r := range reads {
		switch {
		case r.expiredAt(a.cfg.Clock.Now()): // may have expired while the writes ran
			meta.fail(r, ErrExpired)
		case a.readQ != nil && (r.backupTo == "" || a.cfg.JournalMode == "WAL"):
			a.readQueued.Add(1)
			select {
			case a.readQ <- r:
			default: // every reader busy and their queue full: shed rather than stall the writes behind us
				a.readQueued.Add(-1)
				a.cfg.Queues.refusedRead()
				meta.fail(r, ErrMailboxFull)
			}
		default:
			meta.reply(r, a.run1(a.w, r, time.Time{}, true))
		}
	}
	if a.wStuck { // the writer is stuck query_only (readFn): drop it; the next batch reopens
		a.wStuck = false
		a.closeDB()
	}
}

// writeBatch runs writes in one transaction, each inside its own savepoint (as psql's ON_ERROR_ROLLBACK does).
// A write that fails is rolled back to its savepoint, so its error stays its own and its batch-mates commit.
// A write can also end the whole transaction under us (a statement timeout interrupts it; INSERT OR ROLLBACK,
// a RAISE(ROLLBACK) trigger, or an I/O error rolls it back): the next ROLLBACK TO or RELEASE then finds no
// savepoint. From there the batch is aborted: the rest are never run (so nothing autocommits outside the
// transaction) and settle reports every write that had not failed on its own as ErrAborted.
//
//	post: every write is either committed and answered without error, or not committed and answered with one
func (a *Actor) writeBatch(writes []*Request, meta batchMeta) {
	ctx := context.Background()
	failAll := func(err error) {
		for _, r := range writes {
			meta.fail(r, err)
		}
	}
	if err := a.w.run(ctx, "BEGIN IMMEDIATE"); err != nil {
		failAll(err)
		return
	}
	now := a.cfg.Clock.Now()
	creating := !a.reqReady && hasRequestID(writes)
	if creating {
		if err := a.createRequestTable(ctx); err != nil {
			_ = a.w.run(ctx, "ROLLBACK")
			failAll(err)
			return
		}
	}
	results := make([]Result, len(writes))
	aborted := sweepDue(a.reqReady, a.sweepMore, a.lastSweep, now, a.cfg.RequestSweep) && a.sweep(ctx, now)
	for i, r := range writes {
		if aborted { // the transaction is gone; settle answers these ErrAborted
			results[i] = Result{QueueWait: a.since(r.enqueued)}
			continue
		}
		results[i], aborted = a.runInSavepoint(ctx, r, now)
	}
	end := "COMMIT"
	if aborted {
		end = "ROLLBACK" // usually fails with "no transaction is active"; it matters only if one is left open
	}
	c0 := a.cfg.Clock.Now()
	endErr := a.w.run(ctx, end)
	commit := a.since(c0)
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
func (a *Actor) runInSavepoint(ctx context.Context, r *Request, now time.Time) (res Result, gone bool) {
	if err := a.w.run(ctx, "SAVEPOINT w"); err != nil {
		return Result{Err: err, QueueWait: a.since(r.enqueued)}, true
	}
	res = a.apply(ctx, r, now)
	if res.Err != nil && a.w.run(ctx, "ROLLBACK TO w") != nil {
		return res, true
	}
	return res, a.w.run(ctx, "RELEASE w") != nil
}

// apply runs one write inside its savepoint. With a request id it first claims the id (reqid.go): a claim that
// inserts nothing is a duplicate, answered from the stored result without running the statement. The claim and
// the result record share the savepoint with the statement, so a failure anywhere rolls all three back. now is
// the batch's clock reading: the id's applied_ms and a closure's Tx.Now.
//
//	pre:  a savepoint is open on a.w
//	post: res.Err == nil && !res.Duplicate ⇒ r ran and, with an id, the id row records res.Rows (SQL) or
//	      res.Body (closure)
//	post: res.Duplicate ⇒ res.Err == nil, r did not run and nothing changed
func (a *Actor) apply(ctx context.Context, r *Request, now time.Time) Result {
	if r.Check != nil {
		cctx, cancel := ctx, context.CancelFunc(func() {})
		if r.StmtTimeout > 0 {
			cctx, cancel = det.WithTimeout(ctx, a.cfg.Clock, r.StmtTimeout)
		}
		err := runCheck(cctx, a.w, r, now, a.cfg.Incarnation)
		timedOut := err != nil && cctx.Err() != nil
		cancel()
		if err != nil {
			return Result{Err: err, TimedOut: timedOut, QueueWait: a.since(r.enqueued)}
		}
	}
	if r.RequestID == "" {
		return a.run1(a.w, r, now, true)
	}
	t0 := a.cfg.Clock.Now()
	var res Result
	switch claim := a.w.exec(ctx, &Request{SQL: sqlClaimRequest, Args: []any{r.RequestID, now.UnixMilli()}, Write: true}); {
	case claim.Err != nil:
		res.Err = claim.Err
	case claim.Rows == 0:
		res = a.priorResult(ctx, r.RequestID, r.Fn != nil)
	default:
		res = a.run1(a.w, r, now, true)
		if res.Err == nil {
			res.Err = a.w.exec(ctx, &Request{SQL: sqlRecordResult, Args: []any{encodeRecord(r, res), r.RequestID}, Write: true}).Err
		}
	}
	res.QueueWait, res.Exec = t0.Sub(r.enqueued), a.since(t0) // the claim and record count as execution
	return res
}

// priorResult answers a duplicate of id from its stored result; closure says the duplicate is a closure
// request, which needs a closure's record (and a SQL write a SQL write's).
func (a *Actor) priorResult(ctx context.Context, id string, closure bool) Result {
	prior := a.w.exec(ctx, &Request{SQL: sqlPriorResult, Args: []any{id}, MaxRows: 1})
	if prior.Err != nil {
		return Result{Err: prior.Err}
	}
	var b []byte
	if len(prior.Values) == 1 {
		b, _ = prior.Values[0][0].([]byte)
	}
	rec, err := decodeRecord(b)
	switch {
	case err != nil:
		return Result{Err: err}
	case rec.closure != closure:
		return Result{Err: ErrRecordKind}
	}
	return Result{Rows: rec.rows, Body: rec.body, Duplicate: true}
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
	res, gone := a.runInSavepoint(ctx, r, now)
	a.lastSweep, a.sweepMore = now, res.Err == nil && res.Rows >= sweepBatch
	return gone
}

// reader serves reads from the read pool until the queue is closed, then closes its connection.
func (a *Actor) reader(k executor) {
	defer a.readers.Done()
	defer k.close()
	single := batchMeta{n: 1, clock: a.cfg.Clock}
	for r := range a.readQ {
		a.readQueued.Add(-1)
		if r.expiredAt(a.cfg.Clock.Now()) {
			single.fail(r, ErrExpired)
			continue
		}
		single.reply(r, a.run1(k, r, time.Time{}, false))
	}
}

func (a *Actor) since(t time.Time) time.Duration { return det.Since(a.cfg.Clock, t) }

// run1 executes one statement (or closure, or snapshot) under its statement timeout. now is a write's batch
// time (zero for a read: the closure's clock reading is taken here); onWriter says k is the write connection.
//
//	pre: onWriter ⇔ k == a.w, and only the run goroutine passes onWriter (it owns a.w)
func (a *Actor) run1(k executor, r *Request, now time.Time, onWriter bool) Result {
	t0 := a.cfg.Clock.Now()
	if now.IsZero() {
		now = t0
	}
	ctx := context.Background()
	if r.StmtTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = det.WithTimeout(ctx, a.cfg.Clock, r.StmtTimeout)
		defer cancel()
	}
	var res Result
	switch {
	case r.backupTo != "":
		res.Err = k.backup(ctx, r.backupTo)
	case r.Fn != nil && r.Write:
		res = runFn(ctx, k, r, now, a.cfg.Incarnation)
	case r.Fn != nil:
		var ok bool
		if res, ok = readFn(ctx, k, r, now, a.cfg.Incarnation, onWriter); !ok && onWriter { // only the run goroutine writes wStuck
			a.wStuck = true
		}
	default:
		res = k.exec(ctx, r)
	}
	res.TimedOut = res.Err != nil && ctx.Err() != nil
	res.QueueWait = t0.Sub(r.enqueued)
	res.Exec = a.since(t0)
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
