package docexec

import (
	"github.com/nickstrad/mansio/server/internal/doccompile"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// MaxCurrentBytes bounds a conflict's current state (spec §8.1): the canonical JSON of the entries answered.
const MaxCurrentBytes = ir.MaxBodyBytes

// witness completes a ConflictError with what the request found (spec §8.1), so the client can decide again
// without reading again: seq, the commit sequence of the state the request ran against, and current, the state
// behind every precondition that does not hold there: the failing operation's, then that of each later assert
// that would fail too, all evaluated against the state the failing operation saw (nothing ran after it). An
// assert is skipped when it cannot be evaluated (its id refers to an insert that did not run, its table is
// unknown, its expect is malformed): it would not have answered a conflict. Entries past MaxCurrentBytes are
// left out and currentTruncated says so; a statement that fails here ends the list the same way, and the
// conflict stays the answer.
//
//	pre: e is the ConflictError of ops[at], with its fields as conflict and assertTable build them
func (r *run) witness(ops []ir.Op, at int, e *ir.Error) {
	seq, err := r.sequence()
	if err != nil {
		return
	}
	current := []any{}
	size, full := 0, false
	add := func(entry *ir.Object) {
		if size += len(ir.Canonical(entry)); size > MaxCurrentBytes {
			full = true
			return
		}
		current = append(current, entry)
	}
	// document adds the state of table's document id, behind operation i's precondition.
	document := func(i int, table, id string, doc *ir.Object) {
		var state any // null for a missing document, not a nil *ir.Object
		if doc != nil {
			state = doc
		}
		add(ir.NewObject("op", float64(i), "table", table, "id", id, "doc", state))
	}
	table, _ := e.Field("table").(string)
	if id, ok := e.Field("id").(string); !ok {
		add(ir.NewObject("op", float64(at), "table", table, "changed", e.Field("actual")))
	} else if doc, err := r.load(table, id); err != nil {
		full = true
	} else {
		document(at, table, id, doc)
	}
	for i := at + 1; i < len(ops) && !full; i++ {
		t, unknown := doccompile.TableFor(r.s, ops[i].TableName())
		if unknown != nil {
			continue
		}
		switch op := ops[i].(type) {
		case *ir.AssertTable:
			if changed, holds := r.unchangedSince(op, seq); !holds {
				add(ir.NewObject("op", float64(i), "table", op.Table, "changed", float64(changed)))
			}
		case *ir.Assert:
			if op.ID.IsRef && op.ID.Ref >= at {
				continue
			}
			id := ir.ResolveID(op.ID, r.refs)
			doc, err := r.load(op.Table, id)
			if err != nil {
				full = true
			} else if e := CheckExpect(t, op.Table, id, resolveExpect(op.Expect, r.refs), doc); e != nil && e.Tag == ir.TagConflict {
				document(i, op.Table, id, doc)
			}
		}
	}
	e.Fields.Set("seq", float64(seq))
	e.Fields.Set("current", current)
	if full {
		e.Fields.Set("currentTruncated", true)
	}
}
