package pg

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestConstraintsGolden runs schema/test/constraints.sql in a fresh migrated schema and diffs the output
// with constraints.expected (SQL G-20, G-21): every constraint rejects its off point. Each statement is
// echoed with its preceding comments, then its result: a command tag, rows, or the error's message,
// SQLSTATE and constraint name (no DETAIL: it quotes timestamps).
func TestConstraintsGolden(t *testing.T) {
	_, schema := newPlacement(t, nil)
	c := adminConn(t, schema)
	// Start from the migrated schema without newPlacement's VM fixtures: the script registers its own.
	if _, err := c.Exec(ctx, "delete from vms"); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("schema/test/constraints.sql")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, stmt := range splitStatements(string(script)) {
		out.WriteString(stmt.text)
		if stmt.sql == "" {
			continue
		}
		res, err := c.PgConn().Exec(ctx, stmt.sql).ReadAll()
		if err != nil {
			var pe *pgconn.PgError
			if !asPgError(err, &pe) {
				t.Fatalf("%s: %v", stmt.sql, err)
			}
			fmt.Fprintf(&out, "ERROR:  %s\n  sqlstate %s", pe.Message, pe.Code)
			if pe.ConstraintName != "" {
				fmt.Fprintf(&out, ", constraint %s", pe.ConstraintName)
			}
			out.WriteString("\n")
			continue
		}
		for _, r := range res {
			if len(r.FieldDescriptions) == 0 {
				fmt.Fprintf(&out, "%s\n", r.CommandTag)
				continue
			}
			names := make([]string, len(r.FieldDescriptions))
			for i, f := range r.FieldDescriptions {
				names[i] = f.Name
			}
			out.WriteString(strings.Join(names, " | ") + "\n")
			for _, row := range r.Rows {
				vals := make([]string, len(row))
				for i, v := range row {
					vals[i] = string(v)
					if v == nil {
						vals[i] = "NULL"
					}
				}
				out.WriteString(strings.Join(vals, " | ") + "\n")
			}
			fmt.Fprintf(&out, "(%d rows)\n", len(r.Rows))
		}
	}
	const golden = "schema/test/constraints.expected"
	if *update {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it, then review the diff)", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("constraints output differs from %s (rerun with -update and review the diff):\n%s", golden, out.String())
	}
}

type statement struct {
	text string // the lines as written, comments included
	sql  string // the statement to run; empty for trailing comments
}

// splitStatements splits a script into statements ending with ";" at the end of a line. Enough for the
// test scripts (no function bodies); keeps comments with the statement that follows them.
func splitStatements(script string) []statement {
	var out []statement
	var text, sql strings.Builder
	for _, line := range strings.SplitAfter(script, "\n") {
		text.WriteString(line)
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "--") {
			continue
		}
		sql.WriteString(line)
		if strings.HasSuffix(trim, ";") {
			out = append(out, statement{text: text.String(), sql: strings.TrimSpace(sql.String())})
			text.Reset()
			sql.Reset()
		}
	}
	if text.Len() > 0 {
		out = append(out, statement{text: text.String()})
	}
	return out
}

func asPgError(err error, pe **pgconn.PgError) bool {
	e, ok := err.(*pgconn.PgError)
	if ok {
		*pe = e
	}
	return ok
}
