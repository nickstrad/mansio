package docschema

import (
	"math"
	"slices"

	"github.com/nickstrad/mansio/server/internal/ir"
)

var kinds = []string{KindString, KindNumber, KindInteger, KindBoolean, KindEnum, KindArray}

func mismatch(path, expected string, actual any) *ir.Error {
	return ir.SchemaErr(ir.ReasonTypeMismatch, path, expected, actual)
}

func record(value any, path string, allowed []string) (*ir.Object, *ir.Error) {
	o, ok := value.(*ir.Object)
	if !ok {
		return nil, ir.SchemaErr(ir.ReasonNotAnObject, path, "object", value)
	}
	if allowed != nil {
		for _, k := range o.Keys() {
			if !slices.Contains(allowed, k) {
				return nil, ir.SchemaErr(ir.ReasonUnknownField, ir.Member(path, k), "declared field", ir.Undefined{})
			}
		}
	}
	return o, nil
}

func decodeNode(value any, path string) (*Node, *ir.Error) {
	n, err := record(value, path, []string{"kind", "nullable", "values", "item", "length"})
	if err != nil {
		return nil, err
	}
	kind, ok := n.Value("kind").(string)
	if !ok || !slices.Contains(kinds, kind) {
		return nil, mismatch(ir.Member(path, "kind"), "string | number | integer | boolean | enum | array", n.Value("kind"))
	}
	out := &Node{Kind: kind, raw: n}
	if v, ok := n.Get("nullable"); ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, mismatch(ir.Member(path, "nullable"), "boolean", v)
		}
		out.Nullable = b
	}
	if v, ok := n.Get("values"); ok {
		list, isList := v.([]any)
		valid := kind == KindEnum && isList
		seen := map[string]bool{}
		for _, item := range list {
			s, isStr := item.(string)
			if !isStr || seen[s] {
				valid = false
				break
			}
			seen[s] = true
			out.Values = append(out.Values, s)
		}
		if !valid {
			return nil, mismatch(ir.Member(path, "values"), "distinct strings, on an enum", v)
		}
		out.HasValues = true
	} else if kind == KindEnum {
		return nil, mismatch(ir.Member(path, "values"), "required member", ir.Undefined{})
	}
	if kind == KindArray {
		item, ok := n.Get("item")
		if !ok {
			return nil, mismatch(ir.Member(path, "item"), "required member", ir.Undefined{})
		}
		if out.Item, err = decodeNode(item, ir.Member(path, "item")); err != nil {
			return nil, err
		}
		if v, ok := n.Get("length"); ok {
			if !ir.IsSafeInteger(v) || v.(float64) < 0 {
				return nil, mismatch(ir.Member(path, "length"), "non-negative safe integer", v)
			}
			l := v.(float64)
			out.Length = &l
		}
	} else {
		for _, k := range []string{"item", "length"} {
			if n.Has(k) {
				return nil, mismatch(ir.Member(path, k), "absent unless kind is array", ir.Undefined{})
			}
		}
	}
	return out, nil
}

// Decode reads the registered schema JSON (spec §2, core/intermediate-representation/schema.ts decodeSchema): strict shapes, then
// the declaration rules of defineTable, addIndex and defineSchema, in the TypeScript order (each table's
// nodes, then its fields' rules, its storage and its indexes; table names last). DDL rules (an empty
// enum) are doccompile.DDL's.
func Decode(value any) (*Schema, *ir.Error) {
	tables, err := record(value, "schema", nil)
	if err != nil {
		return nil, err
	}
	s := &Schema{}
	for _, name := range tables.Keys() {
		path := ir.Member("schema", name)
		t, err := record(tables.Value(name), path, []string{"fields", "storage", "indexes"})
		if err != nil {
			return nil, err
		}
		rawFields, err := record(t.Value("fields"), ir.Member(path, "fields"), nil)
		if err != nil {
			return nil, err
		}
		var fields []Field
		for _, field := range rawFields.Keys() {
			at := ir.Member(ir.Member(path, "fields"), field)
			wrapped, err := record(rawFields.Value(field), at, []string{"node"})
			if err != nil {
				return nil, err
			}
			node, err := decodeNode(wrapped.Value("node"), ir.Member(at, "node"))
			if err != nil {
				return nil, err
			}
			fields = append(fields, Field{Name: field, Node: node})
		}
		table, err := defineTable(fields)
		if err != nil {
			return nil, err
		}
		storage, _ := t.Value("storage").(string)
		if storage != Columns && storage != JSON {
			return nil, mismatch(ir.Member(path, "storage"), `"columns" | "json"`, t.Value("storage"))
		}
		table.Storage = storage
		list, ok := t.Value("indexes").([]any)
		if !ok {
			return nil, mismatch(ir.Member(path, "indexes"), "array", t.Value("indexes"))
		}
		for i, item := range list {
			at := ir.Element(ir.Member(path, "indexes"), i)
			idx, err := record(item, at, []string{"name", "fields"})
			if err != nil {
				return nil, err
			}
			idxName, ok := idx.Value("name").(string)
			if !ok {
				return nil, mismatch(ir.Member(at, "name"), "string", idx.Value("name"))
			}
			names, ok := stringList(idx.Value("fields"))
			if !ok {
				return nil, mismatch(ir.Member(at, "fields"), "array of strings", idx.Value("fields"))
			}
			if table, err = table.addIndex(idxName, names); err != nil {
				return nil, err
			}
		}
		s.add(name, table)
	}
	if err := defineSchema(s); err != nil {
		return nil, err
	}
	return s, nil
}

func stringList(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

// Load is for the differential corpus and fuzzing only; a server never loads a schema with it (use Decode).
// It reads schema JSON into tables without any declaration check, as scripts/intermediate-representation/gen-compile-corpus.ts's
// tablesOf does (new Table(fields, storage, indexes)): illegal names, empty enums, itemless arrays and odd
// lengths all load, and the compiler must be total over them. Shapes the TypeScript would crash on (a
// table that is not an object, a field without a node) are skipped; their behaviour is not specified.
// An absent storage is "columns" (the constructor's default); a non-string storage is "", which the
// compiler, like TS, treats as neither layout.
func Load(value any) *Schema {
	s := &Schema{}
	tables, ok := value.(*ir.Object)
	if !ok {
		return s
	}
	for _, name := range tables.Keys() {
		t, ok := tables.Value(name).(*ir.Object)
		if !ok {
			continue
		}
		var fields []Field
		if raw, ok := t.Value("fields").(*ir.Object); ok {
			for _, field := range raw.Keys() {
				wrapped, ok := raw.Value(field).(*ir.Object)
				if !ok {
					continue
				}
				if node, ok := loadNode(wrapped.Value("node")); ok {
					fields = append(fields, Field{Name: field, Node: node})
				}
			}
		}
		table := newTable(fields)
		switch storage := t.Value("storage").(type) {
		case string:
			table.Storage = storage
		default:
			if storage != (ir.Undefined{}) {
				table.Storage = ""
			}
		}
		if list, ok := t.Value("indexes").([]any); ok {
			for _, item := range list {
				idx, ok := item.(*ir.Object)
				if !ok {
					continue
				}
				var names []string
				if list, ok := idx.Value("fields").([]any); ok {
					for _, f := range list {
						names = append(names, ir.ToString(f))
					}
				}
				table.Indexes = append(table.Indexes, Index{Name: ir.ToString(idx.Value("name")), Fields: names})
			}
		}
		s.add(name, table)
	}
	return s
}

func loadNode(value any) (*Node, bool) {
	n, ok := value.(*ir.Object)
	if !ok {
		return nil, false
	}
	kind, _ := n.Value("kind").(string)
	out := &Node{Kind: kind, Nullable: truthy(n.Value("nullable")), raw: n}
	if list, ok := n.Value("values").([]any); ok {
		out.HasValues = true
		for _, v := range list {
			out.Values = append(out.Values, ir.ToString(v))
		}
	}
	if item, ok := n.Get("item"); ok {
		out.Item, _ = loadNode(item)
	}
	if l, ok := n.Get("length"); ok {
		f, isNum := l.(float64)
		if !isNum {
			f = math.NaN()
		}
		out.Length = &f
	}
	return out, true
}

// truthy is JavaScript's ToBoolean for a JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return v != (ir.Undefined{})
}
