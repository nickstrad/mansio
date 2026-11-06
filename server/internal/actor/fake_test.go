package actor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// faultExec is an in-memory executor with fault switches, so the actor's error branches can be tested without
// SQLite. It models SQLite's transaction state: statements containing "SLOW" block until their context ends (a
// statement timeout) and, like an interrupted SQLite write, end the open transaction; EndsTxn statements fail
// and end it too (INSERT OR ROLLBACK, RAISE(ROLLBACK)). With no transaction, ROLLBACK TO / RELEASE fail with
// "no such savepoint" and COMMIT / ROLLBACK with "no transaction is active", as in SQLite.
type faultExec struct {
	mu            sync.Mutex
	FailBegin     error
	FailCommit    error
	FailSavepoint error            // SAVEPOINT w fails (the transaction stays open)
	FailOn        map[string]error // exec of this SQL fails
	EndsTxn       map[string]bool  // exec of this SQL fails and ends the open transaction
	FailRun       map[string]error // run of this statement fails (PRAGMA, BEGIN of a read closure, ...)
	ran           []string
	closed        bool
	inTxn         bool
	savepoints    int
}

var (
	errNoSavepoint = errors.New("no such savepoint: w")
	errNoTxn       = errors.New("cannot commit - no transaction is active")
)

func (f *faultExec) exec(ctx context.Context, r *Request) Result {
	f.mu.Lock()
	f.ran = append(f.ran, r.SQL)
	err := f.FailOn[r.SQL]
	ends := f.EndsTxn[r.SQL]
	if ends || strings.Contains(r.SQL, "SLOW") && r.Write {
		f.inTxn, f.savepoints = false, 0
	}
	f.mu.Unlock()
	switch {
	case strings.Contains(r.SQL, "SLOW"):
		<-ctx.Done()
		return Result{Err: errors.New("interrupted")}
	case ends:
		return Result{Err: errors.New("UNIQUE constraint failed; transaction rolled back")}
	case err != nil:
		return Result{Err: err}
	}
	return Result{Rows: 1}
}

func (f *faultExec) run(_ context.Context, q string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, q)
	if err := f.FailRun[q]; err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(q, "BEGIN"):
		if f.FailBegin == nil {
			f.inTxn = true
		}
		return f.FailBegin
	case q == "SAVEPOINT w":
		if f.FailSavepoint != nil {
			return f.FailSavepoint
		}
		f.inTxn = true // outside a transaction SQLite starts one
		f.savepoints++
	case q == "ROLLBACK TO w" || q == "RELEASE w":
		if f.savepoints == 0 {
			return errNoSavepoint
		}
		if q == "RELEASE w" {
			f.savepoints--
		}
	case q == "COMMIT" || q == "ROLLBACK":
		if !f.inTxn {
			return errNoTxn
		}
		f.inTxn, f.savepoints = false, 0
		if q == "COMMIT" {
			return f.FailCommit
		}
	}
	return nil
}

func (f *faultExec) backup(_ context.Context, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, "BACKUP "+dst)
	return f.FailOn["BACKUP"]
}

func (f *faultExec) close() { f.mu.Lock(); f.closed = true; f.mu.Unlock() }

func (f *faultExec) statements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

// fakeActor returns an unstarted actor whose connections are f (or fail to open with openErr).
func fakeActor(t *testing.T, cfg Config, f *faultExec, openErr error) *Actor {
	t.Helper()
	a := New(0, "fake", cfg)
	a.open = func(string, Config, bool) (executor, error) {
		if openErr != nil {
			return nil, openErr
		}
		return f, nil
	}
	t.Cleanup(a.Close)
	return a
}

// submitAll queues reqs before the actor starts, so they form one batch, then starts it and waits.
func submitAll(t *testing.T, a *Actor, reqs ...*Request) []Result {
	t.Helper()
	for _, r := range reqs {
		if err := a.Submit(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Start(false); err != nil {
		t.Fatal(err)
	}
	out := make([]Result, len(reqs))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i, r := range reqs {
		out[i] = r.Wait(ctx)
	}
	return out
}

func write(sql string) *Request { return &Request{SQL: sql, Write: true} }

func TestBeginFailureFailsEveryWriteButReadsStillRun(t *testing.T) {
	boom := errors.New("database is locked")
	f := &faultExec{FailBegin: boom}
	res := submitAll(t, fakeActor(t, Config{}, f, nil), write("w1"), &Request{SQL: "r1"}, write("w2"))
	if !errors.Is(res[0].Err, boom) || !errors.Is(res[2].Err, boom) {
		t.Fatalf("writes: %v, %v", res[0].Err, res[2].Err)
	}
	if res[1].Err != nil {
		t.Fatalf("read failed: %v", res[1].Err)
	}
	for _, s := range f.statements() {
		if s == "w1" || s == "w2" || s == "COMMIT" {
			t.Fatalf("ran %q after BEGIN failed: %v", s, f.statements())
		}
	}
}

func TestCommitFailureReachesEveryWrite(t *testing.T) {
	boom := errors.New("disk I/O error")
	own := errors.New("constraint")
	f := &faultExec{FailCommit: boom, FailOn: map[string]error{"w2": own}}
	res := submitAll(t, fakeActor(t, Config{}, f, nil), write("w1"), write("w2"), write("w3"))
	if !errors.Is(res[0].Err, boom) || !errors.Is(res[1].Err, own) || !errors.Is(res[2].Err, boom) {
		t.Fatalf("results: %v / %v / %v", res[0].Err, res[1].Err, res[2].Err)
	}
}

func TestTimedOutWriteRollsBackAndSkipsTheRest(t *testing.T) {
	f := &faultExec{}
	slow := &Request{SQL: "w2 SLOW", Write: true, StmtTimeout: 20 * time.Millisecond}
	res := submitAll(t, fakeActor(t, Config{}, f, nil), write("w1"), slow, write("w3"))
	if !res[1].TimedOut || !errors.Is(res[0].Err, ErrAborted) || !errors.Is(res[2].Err, ErrAborted) {
		t.Fatalf("results: %+v", res)
	}
	got := strings.Join(f.statements(), ",")
	if got != "BEGIN IMMEDIATE,SAVEPOINT w,w1,RELEASE w,SAVEPOINT w,w2 SLOW,ROLLBACK TO w,ROLLBACK" {
		t.Fatalf("statements = %s (w3 must not run; must roll back)", got)
	}
}

func TestOpenFailureFailsTheBatchAndStartIsRetryable(t *testing.T) {
	boom := errors.New("no such file")
	a := fakeActor(t, Config{}, nil, boom)
	if err := a.Start(true); !errors.Is(err, boom) {
		t.Fatalf("Start(true) = %v", err)
	}
	// Start may be called again after a failed open (the CAS was released).
	if err := a.Start(false); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if res := a.Do(context.Background(), "r", false); !errors.Is(res.Err, boom) {
		t.Fatalf("lazy open: %v", res.Err)
	}
}

func TestStartTwiceIsRefused(t *testing.T) {
	a := fakeActor(t, Config{}, &faultExec{}, nil)
	if err := a.Start(false); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(true); !errors.Is(err, errAlreadyStarted) {
		t.Fatalf("second Start = %v", err)
	}
}

func TestExpiredRequestsNeverReachTheConnection(t *testing.T) {
	f := &faultExec{}
	past := time.Now().Add(-time.Millisecond)
	res := submitAll(t, fakeActor(t, Config{}, f, nil),
		&Request{SQL: "xw", Write: true, Deadline: past}, &Request{SQL: "xr", Deadline: past})
	for i, r := range res {
		if !errors.Is(r.Err, ErrExpired) {
			t.Fatalf("request %d: %v", i, r.Err)
		}
	}
	if s := f.statements(); len(s) != 0 {
		t.Fatalf("ran %v", s)
	}
}

func TestFullReadQueueShedsInsteadOfBlocking(t *testing.T) {
	f := &faultExec{}
	a := fakeActor(t, Config{ReadConns: 1, MailboxSize: 2}, f, nil)
	slow := &Request{SQL: "SLOW", StmtTimeout: 300 * time.Millisecond}
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a.Submit(slow)
	time.Sleep(20 * time.Millisecond) // the reader is now busy with slow
	var shed int
	var queued []*Request
	for range 10 { // submit all before waiting, so the read queue (capacity 2) overflows
		r := &Request{SQL: "r"}
		if a.Submit(r) != nil {
			shed++
			continue
		}
		queued = append(queued, r)
	}
	for _, r := range queued {
		if res := r.Wait(ctx); errors.Is(res.Err, ErrMailboxFull) {
			shed++
		}
	}
	if shed == 0 {
		t.Fatal("expected some reads to be shed while the only reader is busy")
	}
}

func TestCloseAnswersQueuedRequestsAndClosesConnection(t *testing.T) {
	f := &faultExec{}
	a := fakeActor(t, Config{}, f, nil)
	r := &Request{SQL: "r"}
	a.Submit(r)
	a.Close()
	if res := r.Wait(context.Background()); !errors.Is(res.Err, ErrClosed) {
		t.Fatalf("queued request: %v", res.Err)
	}
	if err := a.Submit(&Request{SQL: "r"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit after Close: %v", err)
	}
}

// PG-01: a write that ends the transaction aborts the batch; nothing after it runs, so nothing autocommits.
func TestWriteThatEndsTheTransactionStopsTheBatch(t *testing.T) {
	f := &faultExec{EndsTxn: map[string]bool{"w2": true}}
	res := submitAll(t, fakeActor(t, Config{}, f, nil), write("w1"), write("w2"), write("w3"))
	if !errors.Is(res[0].Err, ErrAborted) || res[1].Err == nil || errors.Is(res[1].Err, ErrAborted) || !errors.Is(res[2].Err, ErrAborted) {
		t.Fatalf("results: %v / %v / %v", res[0].Err, res[1].Err, res[2].Err)
	}
	got := strings.Join(f.statements(), ",")
	if got != "BEGIN IMMEDIATE,SAVEPOINT w,w1,RELEASE w,SAVEPOINT w,w2,ROLLBACK TO w,ROLLBACK" {
		t.Fatalf("statements = %s (w3 must not run; no COMMIT)", got)
	}
}

// A write that fails on its own is rolled back to its savepoint; its batch-mates still commit.
func TestFailedWriteRollsBackToItsSavepointAndTheBatchCommits(t *testing.T) {
	own := errors.New("constraint")
	f := &faultExec{FailOn: map[string]error{"w2": own}}
	res := submitAll(t, fakeActor(t, Config{}, f, nil), write("w1"), write("w2"), write("w3"))
	if res[0].Err != nil || !errors.Is(res[1].Err, own) || res[2].Err != nil {
		t.Fatalf("results: %v / %v / %v", res[0].Err, res[1].Err, res[2].Err)
	}
	if s := f.statements(); s[len(s)-1] != "COMMIT" || !slices.Contains(s, "ROLLBACK TO w") {
		t.Fatalf("statements = %v", s)
	}
}

// A SAVEPOINT that fails leaves the transaction's state unknown: the batch is rolled back, not committed.
func TestSavepointFailureRollsBackTheBatch(t *testing.T) {
	boom := errors.New("out of memory")
	f := &faultExec{FailSavepoint: boom}
	res := submitAll(t, fakeActor(t, Config{}, f, nil), write("w1"), write("w2"))
	if !errors.Is(res[0].Err, boom) || !errors.Is(res[1].Err, ErrAborted) {
		t.Fatalf("results: %v / %v", res[0].Err, res[1].Err)
	}
	if got := strings.Join(f.statements(), ","); got != "BEGIN IMMEDIATE,SAVEPOINT w,ROLLBACK" {
		t.Fatalf("statements = %s", got)
	}
}
