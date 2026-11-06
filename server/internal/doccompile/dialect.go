package doccompile

import (
	"strconv"
	"strings"

	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// Dialect is a SQL dialect: SQLite is the writer, DuckDB the reader.
type Dialect string

const (
	SQLite Dialect = "sqlite"
	DuckDB Dialect = "duckdb"
)

// SQL is a parameterized statement. Values are positional, in placeholder order, and are JSON values:
// string, float64 or nil (booleans already 0/1, arrays already JSON text). Use Args to bind them.
type SQL struct {
	Text   string
	Values []any
}

// JSON is {"text", "values"}, the vectors' form.
func (s SQL) JSON() *ir.Object {
	values := s.Values
	if values == nil {
		values = []any{}
	}
	return ir.NewObject("text", s.Text, "values", values)
}

// Quote quotes an identifier for interpolation. It is checked, not escaped: only
// ^[A-Za-z_][A-Za-z0-9_]*$ gets through.
func Quote(name string) (string, *ir.Error) {
	if err := docschema.Identifier(name); err != nil {
		return "", err
	}
	return `"` + name + `"`, nil
}

// literal is a single-quoted SQL string literal; only ever applied to schema-derived text.
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// placeholder is the index-th (1-based) bind placeholder.
func placeholder(d Dialect, index int) string {
	if d == SQLite {
		return "?"
	}
	return "$p" + strconv.Itoa(index)
}

// Args converts Values to driver arguments the way the TypeScript SQLite binding does: an integral number
// within the safe range binds as INTEGER (int64), any other number as REAL.
func Args(values []any) []any {
	out := make([]any, len(values))
	for i, v := range values {
		if ir.IsSafeInteger(v) {
			out[i] = int64(v.(float64))
		} else {
			out[i] = v
		}
	}
	return out
}
