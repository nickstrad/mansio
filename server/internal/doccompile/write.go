package doccompile

import (
	"strings"

	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// Insert compiles a new document (core/compile/write.ts compileInsert): the body is validated in full,
// _version starts at 1. The json layout stores the validated body as one JSON "doc".
func Insert(s *docschema.Schema, table string, body any, id string, creationTime float64) (SQL, *ir.Error) {
	t, err := TableFor(s, table)
	if err != nil {
		return SQL{}, err
	}
	parsed, err := docschema.ParseObject(t, body, false)
	if err != nil {
		return SQL{}, err
	}
	columns := []string{"_id", "_creationTime", "_version"}
	values := []any{id, creationTime, 1.0}
	if t.Storage == docschema.JSON {
		columns = append(columns, "doc")
		values = append(values, ir.Stringify(parsed))
	} else {
		for _, f := range t.Fields {
			columns = append(columns, f.Name)
			values = append(values, Encode(f.Node, parsed.Value(f.Name)))
		}
	}
	target, err := Quote(table)
	if err != nil {
		return SQL{}, err
	}
	quoted := make([]string, len(columns))
	for i, c := range columns {
		if quoted[i], err = Quote(c); err != nil {
			return SQL{}, err
		}
	}
	binds := strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", ")
	return SQL{Text: "INSERT INTO " + target + " (" + strings.Join(quoted, ", ") + ") VALUES (" + binds + ")", Values: values}, nil
}

// Replace compiles a full-body replace by id (a patch is read, merge and Replace); _version is bumped in
// SQL.
func Replace(s *docschema.Schema, table, id string, body any) (SQL, *ir.Error) {
	t, err := TableFor(s, table)
	if err != nil {
		return SQL{}, err
	}
	parsed, err := docschema.ParseObject(t, body, false)
	if err != nil {
		return SQL{}, err
	}
	var sets []string
	var values []any
	if t.Storage == docschema.JSON {
		sets = append(sets, `"doc" = ?`)
		values = append(values, ir.Stringify(parsed))
	} else {
		for _, f := range t.Fields {
			column, err := Quote(f.Name)
			if err != nil {
				return SQL{}, err
			}
			sets = append(sets, column+" = ?")
			values = append(values, Encode(f.Node, parsed.Value(f.Name)))
		}
	}
	sets = append(sets, `"_version" = "_version" + 1`)
	values = append(values, id)
	target, err := Quote(table)
	if err != nil {
		return SQL{}, err
	}
	return SQL{Text: "UPDATE " + target + " SET " + strings.Join(sets, ", ") + ` WHERE "_id" = ?`, Values: values}, nil
}

// Delete compiles a delete by id. The collection is still looked up, so a typo fails rather than no-ops.
func Delete(s *docschema.Schema, table, id string) (SQL, *ir.Error) {
	if _, err := TableFor(s, table); err != nil {
		return SQL{}, err
	}
	target, err := Quote(table)
	if err != nil {
		return SQL{}, err
	}
	return SQL{Text: "DELETE FROM " + target + ` WHERE "_id" = ?`, Values: []any{id}}, nil
}
