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
)

var (
	ErrMailboxFull = errors.New("actor: mailbox full")
	ErrClosed      = errors.New("actor: closed")
	ErrExpired     = errors.New("actor: deadline passed while queued")
	ErrAborted     = errors.New("actor: batch transaction aborted by another write's timeout")
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

	enqueued time.Time
	reply    chan Result
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

// Do submits one statement and waits for its result or ctx.
func (a *Actor) Do(ctx context.Context, sql string, write bool, args ...any) Result {
	r := &Request{SQL: sql, Args: args, Write: write}
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
		case a.readQ != nil:
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

// writeBatch runs writes in one transaction. A write interrupted by its timeout makes SQLite roll back the whole
// transaction, so the rest are skipped and the batch is rolled back; settle decides every write's outcome.
func (a *Actor) writeBatch(writes []*Request, meta batchMeta) {
	ctx := context.Background()
	if err := a.w.run(ctx, "BEGIN IMMEDIATE"); err != nil {
		for _, r := range writes {
			meta.fail(r, err)
		}
		return
	}
	results := make([]Result, len(writes))
	aborted := false
	for i, r := range writes {
		if aborted { // SQLite already rolled the transaction back; settle answers these ErrAborted
			results[i] = Result{QueueWait: time.Since(r.enqueued)}
			continue
		}
		results[i] = a.run1(a.w, r)
		aborted = results[i].TimedOut
	}
	end := "COMMIT"
	if aborted {
		end = "ROLLBACK"
	}
	c0 := time.Now()
	endErr := a.w.run(ctx, end)
	commit := time.Since(c0)
	settle(results, aborted, endErr)
	for i, r := range writes {
		results[i].Commit = commit
		meta.reply(r, results[i])
	}
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
