package doccompile

import (
	"math"
	"strconv"
	"strings"

	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// Row is one result row: column name to driver value (nil, int64, float64, string, []byte or bool).
type Row map[string]any

func decodeErr(kv ...any) *ir.Error { return ir.NewError(ir.TagDecode, kv...) }

// number is JavaScript's Number(v) for a driver value.
func number(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case int64:
		return float64(x)
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return math.NaN()
}

// SafeNumber narrows a driver integer to a safe integer (core/compile/decode.ts safeNumber); _creationTime,
// _version and every count go through it.
func SafeNumber(v any, field, table string) (float64, *ir.Error) {
	n := number(v)
	if !ir.IsSafeInteger(n) {
		return 0, decodeErr("reason", "unsafe-integer", "field", field, "table", table, "actual", driverJSON(v))
	}
	return n, nil
}

// driverJSON is a driver value as a JSON value, for an error's actual.
func driverJSON(v any) any {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case []byte:
		return string(x)
	case nil, float64, string, bool:
		return x
	}
	return ir.Undefined
}

func parseJSON(v any, field, table string) (any, *ir.Error) {
	var text string
	switch x := v.(type) {
	case string:
		text = x
	case []byte:
		text = string(x)
	default:
		text = ir.ToString(driverJSON(v))
	}
	out, err := ir.Parse(text)
	if err != nil {
		return nil, decodeErr("reason", "malformed-json", "field", field, "table", table, "actual", driverJSON(v))
	}
	return out, nil
}

// DecodeValue is one stored column back to its document value, per the field's node.
func DecodeValue(n *docschema.Node, v any, field, table string) (any, *ir.Error) {
	if v == nil {
		return nil, nil
	}
	switch n.Kind {
	case docschema.KindInteger:
		return SafeNumber(v, field, table)
	case docschema.KindNumber:
		return number(v), nil
	case docschema.KindBoolean:
		return v == true || v == int64(1) || v == 1.0, nil
	case docschema.KindArray:
		return parseJSON(v, field, table)
	}
	if b, ok := v.([]byte); ok {
		return string(b), nil
	}
	return driverJSON(v), nil
}

// Decode turns the rows of a compiled plan into what the plan asked for (core/compile/decode.ts decode):
// a float64 for count, []any of {value, count} for countBy, []any of documents otherwise. Bodies are
// re-validated against the schema, so a drifted row fails loudly.
func Decode(s *docschema.Schema, plan Plan, rows []Row) (any, *ir.Error) {
	t, err := TableFor(s, plan.Table)
	if err != nil {
		return nil, err
	}
	kind := ""
	if plan.Aggregate != nil {
		kind = plan.Aggregate.Kind
	}
	switch kind {
	case "count":
		var v any = ir.Undefined
		if len(rows) > 0 {
			if c, ok := rows[0]["count"]; ok {
				v = c
			}
		}
		return SafeNumber(v, "count", plan.Table)
	case "countBy":
		field := plan.Aggregate.Field
		n, err := FieldNode(t, field)
		if err != nil {
			return nil, err
		}
		groups := make([]any, 0, len(rows))
		for _, row := range rows {
			value, err := DecodeValue(n, row["value"], field, plan.Table)
			if err != nil {
				return nil, err
			}
			count, err := SafeNumber(row["count"], "count", plan.Table)
			if err != nil {
				return nil, err
			}
			groups = append(groups, ir.NewObject("value", value, "count", count))
		}
		return groups, nil
	}
	docs := make([]any, 0, len(rows))
	for _, row := range rows {
		var body any
		if t.Storage == docschema.JSON {
			if body, err = parseJSON(row["doc"], "doc", plan.Table); err != nil {
				return nil, err
			}
		} else {
			o := ir.NewObject()
			for _, f := range t.Fields {
				v, ok := row[f.Name]
				if !ok {
					return nil, decodeErr("reason", "missing-column", "field", f.Name, "table", plan.Table)
				}
				dv, err := DecodeValue(f.Node, v, f.Name, plan.Table)
				if err != nil {
					return nil, err
				}
				o.Set(f.Name, dv)
			}
			body = o
		}
		doc, err := docschema.ParseObject(t, body, false)
		if err != nil {
			return nil, err
		}
		doc.Set("_id", driverJSON(row["_id"]))
		created, err := SafeNumber(row["_creationTime"], "_creationTime", plan.Table)
		if err != nil {
			return nil, err
		}
		doc.Set("_creationTime", created)
		version, err := SafeNumber(row["_version"], "_version", plan.Table)
		if err != nil {
			return nil, err
		}
		doc.Set("_version", version)
		docs = append(docs, doc)
	}
	return docs, nil
}
