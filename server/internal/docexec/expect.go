package docexec

import (
	"github.com/nickstrad/mansio/server/internal/doccompile"
	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// conflict is a failed precondition (core/intermediate-representation/errors.ts PreconditionConflict under the ConflictError tag).
func conflict(table, id, check string, expected, actual any) *ir.Error {
	return ir.NewError(ir.TagConflict, "table", table, "id", id, "check", check, "expected", expected, "actual", actual)
}

// CheckExpect evaluates expect against the current document (nil when it does not exist), in the order
// existence, version, fields (core/intermediate-representation/semantics.ts checkExpect, spec §5.4). expect.fields names declared
// scalar fields only, and each value is validated against its node and the storage contract before anything
// is compared, whether or not the document exists, so a malformed precondition fails the same way every time.
//
//	pre: expect's $refs are resolved
//	post: nil ⇔ every precondition holds
func CheckExpect(t *docschema.Table, table, id string, expect *ir.Expect, doc *ir.Object) *ir.Error {
	var keys []string
	if expect.Fields != nil {
		keys = expect.Fields.Keys()
	}
	for _, field := range keys {
		// Metadata columns are not comparable here: version is the precondition on _version.
		if _, ok := t.Field(field); !ok {
			return ir.NewError(ir.TagCompile, "reason", "unknown-field", "field", field, "table", table)
		}
		node, e := doccompile.Scalar(t, field)
		if e != nil {
			return e
		}
		value := expect.Fields.Value(field)
		if _, e := docschema.Validate(node, value, field); e != nil {
			return e
		}
		if e := docschema.Storable(value, field); e != nil {
			return e
		}
	}
	mustExist, decided := false, false
	switch {
	case expect.Exists != nil:
		mustExist, decided = *expect.Exists, true
	case expect.Version != nil || expect.Fields != nil:
		mustExist, decided = true, true
	}
	if decided && mustExist && doc == nil {
		return conflict(table, id, "exists", true, false)
	}
	if decided && !mustExist && doc != nil {
		return conflict(table, id, "exists", false, true)
	}
	if doc == nil {
		return nil
	}
	if expect.Version != nil {
		if stored := doc.Value("_version"); stored != float64(*expect.Version) {
			return conflict(table, id, "version", float64(*expect.Version), stored)
		}
	}
	differs := false
	for _, k := range keys {
		// Strict equality of scalars, as JavaScript's !==: validation admits only strings, numbers, booleans
		// and null here, which compare by value as Go interfaces (0 equals -0, as in JS).
		if doc.Value(k) != expect.Fields.Value(k) {
			differs = true
			break
		}
	}
	if differs {
		actual := ir.NewObject()
		for _, k := range keys {
			actual.Set(k, doc.Value(k))
		}
		return conflict(table, id, "fields", expect.Fields, actual)
	}
	return nil
}
