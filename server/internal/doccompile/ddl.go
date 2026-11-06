package doccompile

import (
	"strings"

	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// DDL is the schema's SQLite DDL (core/compile/ddl.ts compileDDL): one STRICT table per collection, with
// CHECKs for enums, booleans and JSON columns, then its indexes on the same expressions queries use.
func DDL(s *docschema.Schema) ([]string, *ir.Error) {
	var statements []string
	for _, name := range s.Names() {
		t, _ := s.Table(name)
		columns := []string{
			`"_id" TEXT PRIMARY KEY NOT NULL`,
			`"_creationTime" INTEGER NOT NULL`,
			`"_version" INTEGER NOT NULL CHECK ("_version" >= 1)`,
		}
		if t.Storage == docschema.JSON {
			columns = append(columns, `"doc" TEXT NOT NULL CHECK (json_valid("doc"))`)
		} else {
			for _, f := range t.Fields {
				column, err := Quote(f.Name)
				if err != nil {
					return nil, err
				}
				n := f.Node
				col := column + " " + ColumnType(n)
				if !n.Nullable {
					col += " NOT NULL"
				}
				if n.Kind == docschema.KindEnum {
					// Zero values cannot be declared through the builders, and IN () is not SQL: refuse.
					// Absent values (never declarable either) just omit the CHECK, as TS does.
					if n.HasValues && len(n.Values) == 0 {
						return nil, compileErr("reason", "empty-enum", "field", f.Name, "table", name, "detail", "enum declares no values")
					}
					if len(n.Values) > 0 {
						allowed := make([]string, len(n.Values))
						for i, v := range n.Values {
							allowed[i] = literal(v)
						}
						col += " CHECK (" + column + " IN (" + strings.Join(allowed, ", ") + "))"
					}
				}
				if n.Kind == docschema.KindBoolean {
					col += " CHECK (" + column + " IN (0, 1))"
				}
				if n.Kind == docschema.KindArray {
					col += " CHECK (" + column + " IS NULL OR json_valid(" + column + "))"
				}
				columns = append(columns, col)
			}
		}
		target, err := Quote(name)
		if err != nil {
			return nil, err
		}
		statements = append(statements, "CREATE TABLE IF NOT EXISTS "+target+" ("+strings.Join(columns, ", ")+") STRICT")
		for _, idx := range t.Indexes {
			indexName, err := Quote("idx_" + name + "_" + idx.Name)
			if err != nil {
				return nil, err
			}
			keys := make([]string, 0, len(idx.Fields))
			for _, f := range idx.Fields {
				key, err := Expression(t, f, SQLite)
				if err != nil {
					return nil, err
				}
				keys = append(keys, key)
			}
			statements = append(statements, "CREATE INDEX IF NOT EXISTS "+indexName+" ON "+target+" ("+strings.Join(keys, ", ")+")")
		}
	}
	return statements, nil
}
