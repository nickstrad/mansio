package front

import (
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// Observing /v1/ops (agentd's event log, IR-4c). With OpsConfig.Observer set, the HTTP adapter traces every
// /v1/ops request and hands the result to the observer once the answer is written:
//
//	HTTP adapter: trace id (X-Trace-Id, or a generated one; echoed), handler name (X-Mansio-Handler),
//	              bytes in and out, status, the whole request's time
//	HandleOps:    the request kind and app, the pipeline's time, and the transaction: when the closure was
//	              submitted, when it began on the actor, when it ended, and each statement it ran
//
// The observer runs on the request's goroutine after the answer is sent, so it must not block (agentd's emits
// to a bounded queue that drops). Statements are recorded on the actor's goroutine; OpsTrace is locked, and
// the observer gets a copy (a timed-out request's closure may still be running).

// HandlerHeader names the SDK handler a /v1/ops request belongs to (informational: the event log's handler
// name). Absent or invalid, the name is OpsName.
const HandlerHeader = "X-Mansio-Handler"

// OpsName is the handler name of a /v1/ops request without a valid HandlerHeader: the Deno shim's route name.
const OpsName = "v1.ops"

// OpsObserver receives one OpsEvent per /v1/ops answer. It must not block.
type OpsObserver func(OpsEvent)

// OpsEvent is one observed /v1/ops request.
type OpsEvent struct {
	Start    time.Time     // the adapter received the request
	Duration time.Duration // until the answer was written
	Method   string
	Path     string
	TraceID  string // the accepted X-Trace-Id or a generated one (also sent back)
	Handler  string // HandlerHeader if valid, else OpsName
	BytesIn  int
	BytesOut int
	Out      OpsOutput
	Ops      OpsTraceSnapshot
}

// Statement is one SQL statement a request's transaction ran.
type Statement struct {
	SQL      string
	Binds    int
	Rows     int64 // rows returned (Query) or changed (Exec)
	Start    time.Time
	Duration time.Duration
	Err      error
}

// OpsTrace collects what HandleOps knows about one request. Set Input.Trace to collect it; nil = off.
type OpsTrace struct {
	mu   sync.Mutex
	snap OpsTraceSnapshot
	now  func() time.Time
}

// OpsTraceSnapshot is an OpsTrace's content.
type OpsTraceSnapshot struct {
	Kind     ir.Kind // "" when the body did not decode
	App      string
	Tenant   string        // from the verified ticket
	Pipeline time.Duration // HandleOps, start to answer
	// The request's transaction (the last attempt, if a new database was created first): submitted to the
	// executor, began on the actor, ended when the reply came back (after the commit) or the wait gave up.
	// Began zero: the closure never ran (refused before, a duplicate mutation, or still queued at the deadline).
	Submitted, Began, Ended time.Time
	Statements              []Statement
}

// Snapshot returns a copy of the trace.
func (t *OpsTrace) Snapshot() OpsTraceSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.snap
	s.Statements = append([]Statement(nil), s.Statements...)
	return s
}

func (t *OpsTrace) update(f func(*OpsTraceSnapshot)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(&t.snap)
}

// wrap is docactor.Call.Wrap: it marks the transaction's start and records each statement.
func (t *OpsTrace) wrap(x docexec.Tx) docexec.Tx {
	now := t.now()
	t.update(func(s *OpsTraceSnapshot) { s.Began, s.Ended, s.Statements = now, time.Time{}, nil })
	return tracedTx{Tx: x, t: t}
}

type tracedTx struct {
	docexec.Tx
	t *OpsTrace
}

func (x tracedTx) Exec(sql string, args ...any) (int64, error) {
	start := x.t.now()
	n, err := x.Tx.Exec(sql, args...)
	x.record(Statement{SQL: sql, Binds: len(args), Rows: n, Start: start, Err: err})
	return n, err
}

func (x tracedTx) Query(sql string, maxRows int, args ...any) (docexec.Rows, error) {
	start := x.t.now()
	r, err := x.Tx.Query(sql, maxRows, args...)
	x.record(Statement{SQL: sql, Binds: len(args), Rows: int64(len(r.Values)), Start: start, Err: err})
	return r, err
}

func (x tracedTx) record(st Statement) {
	st.Duration = x.t.now().Sub(st.Start)
	x.t.update(func(s *OpsTraceSnapshot) { s.Statements = append(s.Statements, st) })
}

// validToken reports whether v is a trace token: 1–128 of [A-Za-z0-9._:-] (platform/http/trace.ts TRACE_TOKEN).
func validToken(v string) bool {
	if len(v) == 0 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}
