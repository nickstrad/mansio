package doccompile

import (
	"strings"

	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// Plan is core/plan.ts's SelectPlan. Limit is a JSON value (a number from the IR decoder; anything from a
// corpus), checked by Normalise.
type Plan struct {
	Table     string
	Filters   []ir.Filter
	Order     *ir.Order
	Limit     any
	Aggregate *ir.Aggregate
}

// PlanOf is the plan of a decoded select op. Filter values must already be resolved (ir.Resolve).
func PlanOf(s *ir.Select) Plan {
	return Plan{Table: s.Table, Filters: s.Filters, Order: s.Order, Limit: s.Limit, Aggregate: s.Aggregate}
}

// The limit bounds (core/plan.ts).
const (
	MinLimit = 1
	MaxLimit = 1000
)

// system are the metadata columns every table has, in both layouts.
var system = map[string]*docschema.Node{
	"_id":           {Kind: docschema.KindString},
	"_creationTime": {Kind: docschema.KindInteger},
	"_version":      {Kind: docschema.KindInteger},
}

func compileErr(kv ...any) *ir.Error { return ir.NewError(ir.TagCompile, kv...) }

// TableFor looks a collection up.
func TableFor(s *docschema.Schema, name string) (*docschema.Table, *ir.Error) {
	t, ok := s.Table(name)
	if !ok {
		return nil, compileErr("reason", "unknown-collection", "table", name)
	}
	return t, nil
}

// FieldNode is the node behind a field name: a metadata column or a declared field.
func FieldNode(t *docschema.Table, name string) (*docschema.Node, *ir.Error) {
	if n, ok := system[name]; ok {
		return n, nil
	}
	n, ok := t.Field(name)
	if !ok {
		return nil, compileErr("reason", "unknown-field", "field", name)
	}
	return n, nil
}

// Scalar is FieldNode, refusing arrays (stored as JSON text, so comparing one would compare text).
func Scalar(t *docschema.Table, name string) (*docschema.Node, *ir.Error) {
	n, err := FieldNode(t, name)
	if err != nil {
		return nil, err
	}
	if n.Kind == docschema.KindArray {
		return nil, compileErr("reason", "array-predicate", "field", name)
	}
	return n, nil
}

// Expression is the SQL that reads one field: a quoted column (columns layout, or a metadata column), or
// the dialect's JSON extraction from "doc" (json layout), with a REAL cast for a number on SQLite (see
// core/compile/select.ts expression). Filters, ordering, countBy keys and index keys all use it.
func Expression(t *docschema.Table, name string, d Dialect) (string, *ir.Error) {
	n, err := Scalar(t, name)
	if err != nil {
		return "", err
	}
	if _, meta := system[name]; t.Storage == docschema.Columns || meta {
		return Quote(name)
	}
	if err := docschema.Identifier(name); err != nil {
		return "", err
	}
	path := Literal("$." + name)
	if d == SQLite {
		extract := `json_extract("doc", ` + path + ")"
		if n.Kind == docschema.KindNumber {
			return "CAST(" + extract + " AS REAL)", nil
		}
		return extract, nil
	}
	extract := `json_extract_string("doc", ` + path + ")"
	switch n.Kind {
	case docschema.KindInteger:
		return "CAST(" + extract + " AS BIGINT)", nil
	case docschema.KindNumber:
		return "CAST(" + extract + " AS DOUBLE)", nil
	case docschema.KindBoolean:
		return "CAST(CAST(" + extract + " AS BOOLEAN) AS INTEGER)", nil
	}
	return extract, nil
}

// Encode is a validated value as a bind value: arrays as JSON text, booleans as 0/1.
func Encode(n *docschema.Node, value any) any {
	switch {
	case value == nil:
		return nil
	case n.Kind == docschema.KindArray:
		return ir.Stringify(value)
	case n.Kind == docschema.KindBoolean:
		if value == true {
			return 1.0
		}
		return 0.0
	}
	return value
}

// ColumnType is the STRICT column type of a node.
func ColumnType(n *docschema.Node) string {
	switch n.Kind {
	case docschema.KindInteger, docschema.KindBoolean:
		return "INTEGER"
	case docschema.KindNumber:
		return "REAL"
	}
	return "TEXT"
}

// Normalise checks the limit (an integer in 1..1000) and drops an aggregate plan's ordering.
func Normalise(p Plan) (Plan, *ir.Error) {
	if f, ok := p.Limit.(float64); !ok || !ir.IsInteger(f) || f < MinLimit || f > MaxLimit {
		return p, compileErr("reason", "invalid-limit", "table", p.Table, "detail", "1..1000, got "+ir.ToString(p.Limit))
	}
	if p.Aggregate != nil {
		p.Order = nil
	}
	return p, nil
}

// Select compiles a read plan (core/compile/select.ts compileSelect). count takes no LIMIT (it counts every
// match); countBy limits groups. Ordering appends "_id" ASC as the tie-break, NULLs first ascending and
// last descending.
func Select(s *docschema.Schema, plan Plan, d Dialect) (SQL, *ir.Error) {
	t, err := TableFor(s, plan.Table)
	if err != nil {
		return SQL{}, err
	}
	p, err := Normalise(plan)
	if err != nil {
		return SQL{}, err
	}
	values := []any{}
	bind := func(v any) string {
		values = append(values, v)
		return Placeholder(d, len(values))
	}
	var where []string
	for _, f := range p.Filters {
		n, err := Scalar(t, f.Field)
		if err != nil {
			return SQL{}, err
		}
		if _, err := docschema.Validate(n, f.Value, f.Field); err != nil {
			return SQL{}, err
		}
		expr, err := Expression(t, f.Field, d)
		if err != nil {
			return SQL{}, err
		}
		if f.Op == "eq" && f.Value == nil {
			where = append(where, expr+" IS NULL")
			continue
		}
		if f.Op != "eq" && f.Op != "gt" {
			return SQL{}, compileErr("reason", "unsupported-operator", "table", plan.Table, "field", f.Field, "detail", f.Op)
		}
		if f.Op == "gt" && (f.Value == nil || (n.Kind != docschema.KindInteger && n.Kind != docschema.KindNumber)) {
			return SQL{}, compileErr("reason", "gt-requires-numeric", "table", plan.Table, "field", f.Field, "detail", n.Kind)
		}
		op := ">"
		if f.Op == "eq" {
			op = "="
		}
		where = append(where, expr+" "+op+" "+bind(Encode(n, f.Value)))
	}
	target, err := Quote(p.Table)
	if err != nil {
		return SQL{}, err
	}
	if d == DuckDB {
		target = `"app".` + target
	}
	projection := "*"
	kind := ""
	if p.Aggregate != nil {
		kind = p.Aggregate.Kind
	}
	switch kind {
	case "count":
		projection = `COUNT(*) AS "count"`
	case "countBy":
		grouped, err := Expression(t, p.Aggregate.Field, d)
		if err != nil {
			return SQL{}, err
		}
		projection = grouped + ` AS "value", COUNT(*) AS "count"`
	}
	text := "SELECT " + projection + " FROM " + target
	if len(where) > 0 {
		text += " WHERE " + strings.Join(where, " AND ")
	}
	switch {
	case kind == "countBy":
		text += ` GROUP BY 1 ORDER BY "value" ASC NULLS FIRST`
	case p.Aggregate == nil && p.Order != nil:
		field, dir := p.Order.Field, p.Order.Direction
		if dir != "asc" && dir != "desc" {
			return SQL{}, compileErr("reason", "invalid-ordering", "table", plan.Table, "field", field, "detail", dir)
		}
		expr, err := Expression(t, field, d)
		if err != nil {
			return SQL{}, err
		}
		nulls := "FIRST"
		if dir == "desc" {
			nulls = "LAST"
		}
		text += " ORDER BY " + expr + " " + strings.ToUpper(dir) + " NULLS " + nulls + `, "_id" ASC`
	case p.Aggregate == nil:
		text += ` ORDER BY "_id" ASC`
	}
	if kind != "count" {
		text += " LIMIT " + bind(p.Limit)
	}
	return SQL{Text: text, Values: values}, nil
}
