package front

import (
	"context"
	"fmt"
	"time"

	"github.com/nickstrad/ts-db-go/internal/actor"
)

// Admitter is the ownership port (contract 2; fencing's internal/owner implements it through an adapter).
type Admitter interface {
	// Admit lets one request for db at the ticket's epoch in, or refuses it.
	//
	//	post: err == nil ⇒ release != nil; the pipeline calls it exactly once, when the request has finished
	//	      (so a fence can drain in-flight work)
	//	post: err != nil ⇒ errors.Is(err, ErrWrongOwner) (epoch mismatch, or db not owned here),
	//	      errors.Is(err, ErrFenced), or errors.Is(err, ErrUnavailable) (e.g. not reconciled since restart);
	//	      anything else is reported as internal
	Admit(db string, epoch uint64) (release func(), err error)
}

// StaticAdmitter owns each database in the map at that epoch and nothing else. It is the stand-in until fencing's
// owner store is wired in; it tracks nothing in flight (release is a no-op).
type StaticAdmitter map[string]uint64

// Admit implements Admitter.
func (s StaticAdmitter) Admit(db string, epoch uint64) (func(), error) {
	switch e, ok := s[db]; {
	case !ok:
		return nil, fmt.Errorf("%w: %s is not served here", ErrWrongOwner, db)
	case e != epoch:
		return nil, fmt.Errorf("%w: %s is at epoch %d, ticket has %d", ErrWrongOwner, db, e, epoch)
	}
	return func() {}, nil
}

// Call is one unit of work for an Executor, already validated, authorized and admitted.
type Call struct {
	Tenant      string
	DB          string
	Epoch       uint64 // the ticket's placement epoch (admitted by the Admitter)
	Gen         uint64 // the ticket's generation
	RequestID   string
	Kind        Kind
	SQL         string // today: parameterized SQL; later an operation (see the package comment)
	Args        []any
	Deadline    time.Time     // the caller stops caring at this instant; never zero
	StmtTimeout time.Duration // bound on execution of the statement; > 0
	MaxRows     int           // reads: more rows is ErrTooManyRows; > 0
}

// Reply is a successful result.
type Reply struct {
	Columns []string
	Rows    [][]any // reads: the rows; writes: nil
	Count   int64   // reads: len(Rows); writes: rows affected
}

// Executor runs calls. Today: one statement on the database's actor (ActorExecutor). Later: a handler executor
// that runs a TypeScript handler inside the actor's write transaction (knowledge/front/…-handler-executor.md).
//
// Contract (fronttest.RunExecutorSuite checks it against every implementation):
//
//	post: returns by Deadline plus a bounded grace, whatever the statement does
//	post: err == nil ⇒ reads: Count == len(Rows) <= MaxRows, every row has len(Columns) values
//	post: errors.Is(err, ErrUnknownDB): db is not hosted here; nothing ran
//	post: errors.Is(err, ErrShed | ErrUnavailable | ErrExpired): nothing ran
//	post: errors.Is(err, ErrStmtTimeout | ErrAborted | ErrStatement | ErrTooManyRows): nothing changed
//	post: errors.Is(err, ErrNoReply): no answer in time; a write may have committed
//	post: any other error: unexpected (internal)
type Executor interface {
	Exec(ctx context.Context, c Call) (Reply, error)
}

// Host finds the actor that serves a database on this VM. StaticHost is the simple one; writeback's virtual-actor
// host (internal/vactor) plugs in through an adapter later.
type Host interface {
	// Lookup returns db's started actor, or an error wrapping ErrUnknownDB.
	Lookup(ctx context.Context, db string) (*actor.Actor, error)
}

// Clock is the time port. nil means time.Now.
type Clock func() time.Time
