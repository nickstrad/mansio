package docschema

import (
	"math"
	"slices"
	"strings"

	"github.com/nickstrad/mansio/server/internal/ir"
)

// expectation is what the node demanded, in core/schema.ts's wording ("integer or null").
func expectation(n *Node) string {
	if n.Nullable {
		return n.Kind + " or null"
	}
	return n.Kind
}

// Validate checks one value against one node (core/schema.ts validate) and returns the value; an array
// is validated element-wise, so the path accumulates indices (grid[1]) and the first failing element
// decides. U+0000 is a valid string here; Storable is the storage rule.
func Validate(n *Node, value any, path string) (any, *ir.Error) {
	if value == nil && n.Nullable {
		return nil, nil
	}
	valid := false
	switch n.Kind {
	case KindString:
		_, valid = value.(string)
	case KindNumber:
		f, ok := value.(float64)
		valid = ok && !math.IsInf(f, 0) && !math.IsNaN(f)
	case KindInteger:
		valid = ir.IsSafeInteger(value)
	case KindBoolean:
		_, valid = value.(bool)
	case KindEnum:
		s, ok := value.(string)
		valid = ok && n.HasValues && slices.Contains(n.Values, s)
	case KindArray:
		if n.Item == nil {
			return nil, ir.SchemaErr(ir.ReasonTypeMismatch, path, "array with an item descriptor", value)
		}
		list, ok := value.([]any)
		if ok && (n.Length == nil || float64(len(list)) == *n.Length) {
			items := make([]any, len(list))
			for i, item := range list {
				v, err := Validate(n.Item, item, ir.Element(path, i))
				if err != nil {
					return nil, err
				}
				items[i] = v
			}
			return items, nil
		}
	}
	if !valid {
		return nil, ir.SchemaErr(ir.ReasonTypeMismatch, path, expectation(n), value)
	}
	return value, nil
}

// firstNul finds the first string inside value holding U+0000, walking arrays and objects.
func firstNul(value any, path string) (string, string, bool) {
	switch x := value.(type) {
	case string:
		return path, x, strings.Contains(x, NUL)
	case []any:
		for i, item := range x {
			if p, s, ok := firstNul(item, ir.Element(path, i)); ok {
				return p, s, true
			}
		}
	case *ir.Object:
		for _, k := range x.Keys() {
			if p, s, ok := firstNul(x.Value(k), ir.Member(path, k)); ok {
				return p, s, true
			}
		}
	}
	return "", "", false
}

// Storable is the text storage contract (ADR 0005, core/schema.ts storable): no string anywhere in value
// may hold U+0000. path is the value's path ("" for a document body, whose keys are then the paths).
func Storable(value any, path string) *ir.Error {
	if p, s, ok := firstNul(value, path); ok {
		return ir.SchemaErr(ir.ReasonTypeMismatch, p, "text without U+0000", s)
	}
	return nil
}

// ParseObject checks a whole object against a table's fields (core/schema.ts parseObject): unknown keys
// first, in member order; then each declared field in order. With partial (a patch) absent keys are
// skipped; otherwise a missing field fails its node. The result holds the validated fields in declaration
// order.
func ParseObject(t *Table, input any, partial bool) (*ir.Object, *ir.Error) {
	obj, ok := input.(*ir.Object)
	if !ok {
		return nil, ir.SchemaErr(ir.ReasonNotAnObject, "", "object", input)
	}
	for _, k := range obj.Keys() {
		if _, declared := t.byName[k]; !declared {
			return nil, ir.SchemaErr(ir.ReasonUnknownField, k, "declared field", obj.Value(k))
		}
	}
	out := ir.NewObject()
	for _, f := range t.Fields {
		if partial && !obj.Has(f.Name) {
			continue
		}
		v, err := Validate(f.Node, obj.Value(f.Name), f.Name)
		if err != nil {
			return nil, err
		}
		out.Set(f.Name, v)
	}
	return out, nil
}

// Body is a document body for an insert (partial false) or a patch (partial true): ParseObject, then the
// storage contract (core/ir/semantics.ts body).
func Body(t *Table, value any, partial bool) (*ir.Object, *ir.Error) {
	parsed, err := ParseObject(t, value, partial)
	if err != nil {
		return nil, err
	}
	if err := Storable(parsed, ""); err != nil {
		return nil, err
	}
	return parsed, nil
}
