// Package docactor runs docexec on internal/actor: an IR request as an actor closure (Request.Check is the
// fingerprint check, before the actor's duplicate check; Request.Fn runs the operations and answers the
// canonical JSON the actor records for a mutation), and database creation (InitFn). It is the glue between the
// domain package docexec and the actor, kept apart so docexec depends on no adapter.
//
// Role: adapter (thin; the decisions are docexec's). Errors are docexec's, unwrapped.
package docactor

import (
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// tx is *actor.Tx as a docexec.Tx.
type tx struct{ t *actor.Tx }

func (x tx) Exec(sql string, args ...any) (int64, error) { return x.t.Exec(sql, args...) }

func (x tx) Query(sql string, maxRows int, args ...any) (docexec.Rows, error) {
	r, err := x.t.Query(sql, maxRows, args...)
	return docexec.Rows{Columns: r.Columns, Values: r.Values}, err
}

func (x tx) Now() time.Time { return x.t.Now() }

// Tx adapts an actor closure's Tx to docexec.
func Tx(t *actor.Tx) docexec.Tx { return tx{t} }

// Call is one decoded request as an actor closure: Check is the fingerprint check (it runs before the actor's
// duplicate check, spec §6.1), Fn runs the operations. Use a Call for one actor request only: Check hands what
// it found to Fn.
type Call struct {
	Req     *ir.Request
	Schemas *docexec.Schemas
	// IDs draws server ids for inserts without a client id (docexec.NewUUIDv7 in production).
	IDs det.IDGen
	// Wrap, if set, wraps the actor's Tx for Fn (tests inject statement failures with it).
	Wrap func(docexec.Tx) docexec.Tx

	state docexec.State
}

// Request is the actor request for c: a write closure with the request id for a mutation, a read closure for a
// query. The caller sets Deadline and StmtTimeout.
func (c *Call) Request() *actor.Request {
	r := &actor.Request{Fn: c.Fn, Check: c.Check, Write: c.Req.Kind == ir.Mutation}
	if r.Write {
		r.RequestID = c.Req.RequestID
	}
	return r
}

// Check implements actor.Request.Check.
func (c *Call) Check(t *actor.Tx) error {
	st, err := docexec.Check(Tx(t), c.Schemas, c.Req.Schema)
	c.state = st
	return err
}

// Fn implements actor.Func: Output.Body is the canonical JSON of the result array (stored by the actor as the
// request-id record of a mutation), Output.Value the docexec.Answer (the array and the commit sequence).
//
//	pre: Check succeeded on the same request
func (c *Call) Fn(t *actor.Tx) (actor.Output, error) {
	x := Tx(t)
	if c.Wrap != nil {
		x = c.Wrap(x)
	}
	answer, err := docexec.Run(x, c.state, c.Req, c.IDs)
	if err != nil {
		return actor.Output{}, err
	}
	return actor.Output{Body: []byte(ir.Canonical(answer.Result)), Value: answer}, nil
}

// InitFn is the write closure that initializes a new database with s (docexec.Init).
func InitFn(s *docexec.Schema) actor.Func {
	return func(t *actor.Tx) (actor.Output, error) { return actor.Output{}, docexec.Init(Tx(t), s) }
}

// InitRequest is InitFn as an actor request (no request id).
func InitRequest(s *docexec.Schema) *actor.Request { return &actor.Request{Write: true, Fn: InitFn(s)} }
