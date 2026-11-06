package docexec

import (
	"errors"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/doccompile"
	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// Tx is what execution needs of a connection; *actor.Tx satisfies it. Errors are the driver's or the actor's,
// unwrapped.
type Tx interface {
	Exec(sql string, args ...any) (int64, error)
	Query(sql string, maxRows int, args ...any) (actor.Rows, error)
	// Now is the request's one clock reading (spec §6.3).
	Now() time.Time
}

// Failure is a request's failure as an error (ir.Error itself is deliberately not an error).
//
//	Invariant: Err != nil; Cause != nil ⇒ Err is a StorageError
type Failure struct {
	// Err is the wire error, with its operation index when it belongs to one.
	Err *ir.Error
	// Cause is the statement's error behind a StorageError (nil for a protocol failure).
	Cause error
}

func (f *Failure) Error() string {
	if f.Cause != nil {
		return f.Err.Message() + ": " + f.Cause.Error()
	}
	return f.Err.Message()
}

func (f *Failure) Unwrap() error { return f.Cause }

// protocol wraps a protocol error.
//
//	pre: e != nil
func protocol(e *ir.Error) error { return &Failure{Err: e} }

// storage wraps a statement's error.
func storage(cause error) error { return &Failure{Err: ir.NewError(ir.TagStorage), Cause: cause} }

// maxRows bounds every statement's rows: a select's limit is at most doccompile.MaxLimit (count answers one
// row, countBy at most limit groups), so more rows would be a compiler bug, answered as a storage failure.
const maxRows = doccompile.MaxLimit

// Run executes req's operations in order on tx against schema s and returns one result per operation (spec §5,
// §6). Run does not check the fingerprint (Check) or the request id (the actor), and does not commit: the
// caller runs it inside one transaction or savepoint and rolls everything back on any error.
//
//	pre: req was decoded by ir.DecodeValue (every $ref names an earlier insert; a query holds reads only), and
//	     s is the database's schema
//	post: err == nil ⇒ len(result) == len(req.Ops)
//	post: err != nil ⇒ err is a *Failure; when it belongs to operation i its Err has op i
//	post: req is not modified
func Run(tx Tx, s *docschema.Schema, req *ir.Request, ids det.IDGen) ([]any, error) {
	r := &run{tx: tx, s: s, ids: ids, now: float64(tx.Now().UnixMilli()), refs: make([]string, 0, len(req.Ops))}
	result := make([]any, 0, len(req.Ops))
	for i, op := range req.Ops {
		v, err := r.step(op)
		if err != nil {
			var f *Failure
			if errors.As(err, &f) {
				f.Err.WithOp(i)
			}
			return nil, err
		}
		id, _ := v.(string)
		if op.Name() != "insert" {
			id = ""
		}
		r.refs = append(r.refs, id)
		result = append(result, v)
	}
	return result, nil
}

// run is one request's execution state.
type run struct {
	tx   Tx
	s    *docschema.Schema
	ids  det.IDGen
	now  float64  // _creationTime of every insert (ms)
	refs []string // refs[i] is insert i's _id ("" for other operations)
}

func (r *run) step(op ir.Op) (any, error) {
	t, e := doccompile.TableFor(r.s, op.TableName())
	if e != nil {
		return nil, protocol(e)
	}
	switch op := op.(type) {
	case *ir.Select:
		return r.selectOp(op)
	case *ir.Get:
		doc, err := r.load(op.Table, ir.ResolveID(op.ID, r.refs))
		if err != nil || doc == nil {
			return nil, err // a missing document answers null
		}
		return doc, nil
	case *ir.Insert:
		return r.insert(t, op)
	case *ir.Patch:
		return r.change(t, op.Table, op.ID, op.Value, op.Expect, "patch")
	case *ir.Delete:
		return r.change(t, op.Table, op.ID, nil, op.Expect, "delete")
	case *ir.Assert:
		return r.change(t, op.Table, op.ID, nil, op.Expect, "assert")
	}
	return nil, protocol(ir.NewError(ir.TagDefect, "detail", "unknown operation "+op.Name()))
}

func (r *run) selectOp(op *ir.Select) (any, error) {
	filters := make([]ir.Filter, len(op.Filters)) // resolved into a copy: the request stays as decoded
	for i, f := range op.Filters {
		filters[i] = ir.Filter{Field: f.Field, Op: f.Op, Value: ir.Resolve(f.Value, r.refs)}
	}
	plan := doccompile.Plan{Table: op.Table, Filters: filters, Order: op.Order, Limit: op.Limit, Aggregate: op.Aggregate}
	sql, e := doccompile.Select(r.s, plan, doccompile.SQLite)
	if e != nil {
		return nil, protocol(e)
	}
	for _, f := range filters {
		if e := docschema.Storable(f.Value, f.Field); e != nil {
			return nil, protocol(e)
		}
	}
	return r.read(plan, sql)
}

// read runs a compiled select and decodes its rows.
func (r *run) read(plan doccompile.Plan, sql doccompile.SQL) (any, error) {
	rows, err := r.tx.Query(sql.Text, maxRows, doccompile.Args(sql.Values)...)
	if err != nil {
		return nil, storage(err)
	}
	out, e := doccompile.Decode(r.s, plan, rowsOf(rows))
	if e != nil {
		return nil, protocol(e)
	}
	return out, nil
}

// load is the current document with _id id, or nil.
func (r *run) load(table, id string) (*ir.Object, error) {
	plan := doccompile.Plan{Table: table, Filters: []ir.Filter{{Field: "_id", Op: "eq", Value: id}}, Limit: 1.0}
	sql, e := doccompile.Select(r.s, plan, doccompile.SQLite)
	if e != nil {
		return nil, protocol(e)
	}
	out, err := r.read(plan, sql)
	if err != nil {
		return nil, err
	}
	if docs := out.([]any); len(docs) > 0 {
		return docs[0].(*ir.Object), nil
	}
	return nil, nil
}

func (r *run) insert(t *docschema.Table, op *ir.Insert) (any, error) {
	value, e := docschema.Body(t, ir.Resolve(op.Value, r.refs), false)
	if e != nil {
		return nil, protocol(e)
	}
	id := op.ID
	if id != "" { // the implicit expect {"exists": false} on a client id (spec §5.2)
		doc, err := r.load(op.Table, id)
		if err != nil {
			return nil, err
		}
		if doc != nil {
			return nil, protocol(conflict(op.Table, id, "exists", false, true))
		}
	} else {
		id = r.ids.NewID()
	}
	sql, e := doccompile.Insert(r.s, op.Table, value, id, r.now)
	if e != nil {
		return nil, protocol(e)
	}
	if err := r.exec(sql); err != nil {
		return nil, err
	}
	return id, nil
}

// change is patch, delete and assert: body (patch), load, expect, then the write (spec §6.1).
func (r *run) change(t *docschema.Table, table string, ref ir.IDRef, value *ir.Object, expect *ir.Expect, kind string) (any, error) {
	id := ir.ResolveID(ref, r.refs)
	var patch *ir.Object
	if kind == "patch" {
		var e *ir.Error
		if patch, e = docschema.Body(t, ir.Resolve(value, r.refs), true); e != nil {
			return nil, protocol(e)
		}
	}
	doc, err := r.load(table, id)
	if err != nil {
		return nil, err
	}
	if expect != nil {
		if e := CheckExpect(t, table, id, resolveExpect(expect, r.refs), doc); e != nil {
			return nil, protocol(e)
		}
	}
	switch kind {
	case "assert":
		return nil, nil
	case "delete":
		if doc != nil {
			sql, e := doccompile.Delete(r.s, table, id)
			if e != nil {
				return nil, protocol(e)
			}
			if err := r.exec(sql); err != nil {
				return nil, err
			}
		}
		return ir.NewObject("deleted", doc != nil), nil
	}
	if doc == nil {
		return nil, protocol(ir.NewError(ir.TagNotFound, "table", table, "id", id))
	}
	merged := ir.NewObject() // {...current body, ...patch}
	for _, f := range t.Fields {
		merged.Set(f.Name, doc.Value(f.Name))
	}
	for _, k := range patch.Keys() {
		merged.Set(k, patch.Value(k))
	}
	sql, e := doccompile.Replace(r.s, table, id, merged)
	if e != nil {
		return nil, protocol(e)
	}
	if err := r.exec(sql); err != nil {
		return nil, err
	}
	return ir.NewObject("_version", doc.Value("_version").(float64)+1), nil
}

func (r *run) exec(sql doccompile.SQL) error {
	if _, err := r.tx.Exec(sql.Text, doccompile.Args(sql.Values)...); err != nil {
		return storage(err)
	}
	return nil
}

// resolveExpect is expect with its fields' $refs resolved, as a copy.
func resolveExpect(e *ir.Expect, refs []string) *ir.Expect {
	if e.Fields == nil {
		return e
	}
	out := *e
	out.Fields = ir.Resolve(e.Fields, refs).(*ir.Object)
	return &out
}

// rowsOf turns the actor's rows into the compiler's.
func rowsOf(rows actor.Rows) []doccompile.Row {
	out := make([]doccompile.Row, len(rows.Values))
	for i, values := range rows.Values {
		row := make(doccompile.Row, len(rows.Columns))
		for j, c := range rows.Columns {
			row[c] = values[j]
		}
		out[i] = row
	}
	return out
}
