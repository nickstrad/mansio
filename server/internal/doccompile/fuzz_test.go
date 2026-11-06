package doccompile_test

import (
	"bufio"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/doccompile"
	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
	"github.com/nickstrad/mansio/server/internal/sqlitedsn"
)

// selectToken is every token a compiled select may contain (the SQL audit of tests/property's compiler
// suites): checked identifiers, placeholders, a closed keyword list, the GROUP BY ordinal, punctuation,
// and single-quoted JSON paths of checked identifiers. A value that reached the text unbound would be a
// token outside this set.
var selectToken = regexp.MustCompile(`^(?:"[A-Za-z_][A-Za-z0-9_]*"|\?|'\$\.[A-Za-z_][A-Za-z0-9_]*'|` +
	`(?:SELECT|FROM|WHERE|AND|IS|NULLS|NULL|ORDER|BY|ASC|DESC|FIRST|LAST|LIMIT|COUNT|AS|GROUP|CAST|REAL|json_extract|1)\b|[(),*=>.])`)

func auditSelect(text string) string {
	for rest := strings.TrimSpace(text); rest != ""; rest = strings.TrimLeft(rest, " ") {
		m := selectToken.FindString(rest)
		if m == "" {
			return rest
		}
		rest = rest[len(m):]
	}
	return ""
}

// sqliteSafe reports whether the schema's DDL can run in SQLite at all. Two kinds of junk schema produce
// DDL SQLite refuses or silently skips, whichever compiler wrote it: names that differ only in case (or
// shadow a metadata column), since SQLite names are case-insensitive; and an index with no fields, which
// compileDDL renders as ON "t" () (only decodeSchema's addIndex refuses it).
func sqliteSafe(s *docschema.Schema) bool {
	tables := map[string]bool{}
	for _, name := range s.Names() {
		if tables[strings.ToLower(name)] {
			return false
		}
		tables[strings.ToLower(name)] = true
		t, _ := s.Table(name)
		for _, idx := range t.Indexes {
			if len(idx.Fields) == 0 {
				return false
			}
		}
		columns := map[string]bool{"_id": true, "_creationtime": true, "_version": true, "doc": t.Storage == docschema.JSON}
		for _, f := range t.Fields {
			if columns[strings.ToLower(f.Name)] && t.Storage != docschema.JSON {
				return false
			}
			columns[strings.ToLower(f.Name)] = true
		}
	}
	return true
}

func addCompileSeeds(f *testing.F) {
	vectors := parseFile(f, filepath.Join(vectorDir, "compile.json")).(*ir.Object).Value("vectors").([]any)
	for _, item := range vectors {
		v := item.(*ir.Object)
		if v.Value("kind") == "select" {
			f.Add([]byte(ir.Stringify(ir.NewObject("schema", v.Value("schema"), "plan", v.Value("plan")))))
		}
	}
	file, err := os.Open(filepath.Join(vectorDir, "generated", "compile-corpus.jsonl"))
	if err != nil {
		f.Fatal(err)
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	sc.Scan() // header
	for n := 0; n < 300 && sc.Scan(); n++ {
		c, _ := ir.Parse(sc.Text())
		f.Add([]byte(ir.Stringify(ir.NewObject("schema", c.(*ir.Object).Value("schema"), "plan", c.(*ir.Object).Value("plan")))))
	}
}

// FuzzCompile: over any schema JSON (loaded without declaration checks) and any plan, Select and DDL never
// panic; a compiled select puts every value in a bind parameter (its text passes the token audit, and
// there is one placeholder per value, each a string, number or null); and when the DDL runs in SQLite, the
// select prepares against it.
func FuzzCompile(f *testing.F) {
	addCompileSeeds(f)
	db, err := sql.Open("sqlite", sqlitedsn.Build(":memory:"))
	if err != nil {
		f.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	f.Fuzz(func(t *testing.T, data []byte) {
		v, perr := ir.Parse(string(data))
		c, ok := v.(*ir.Object)
		if perr != nil || !ok {
			return
		}
		s := docschema.Load(c.Value("schema"))
		q, serr := doccompile.Select(s, planOf(c.Value("plan")), doccompile.SQLite)
		ddl, derr := doccompile.DDL(s)
		if serr == nil {
			if bad := auditSelect(q.Text); bad != "" {
				t.Fatalf("unexpected text %q in %s", bad, q.Text)
			}
			if n := strings.Count(q.Text, "?"); n != len(q.Values) {
				t.Fatalf("%d placeholders, %d values: %s", n, len(q.Values), q.Text)
			}
			for _, value := range q.Values {
				switch value.(type) {
				case string, float64, nil:
				default:
					t.Fatalf("bind value %#v", value)
				}
			}
		}
		if derr != nil || !sqliteSafe(s) {
			return
		}
		conn, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(t.Context(), "BEGIN"); err != nil {
			t.Fatal(err)
		}
		defer conn.ExecContext(t.Context(), "ROLLBACK") //nolint:errcheck // the transaction only scopes the DDL
		for _, stmt := range ddl {
			if _, err := conn.ExecContext(t.Context(), stmt); err != nil {
				if strings.Contains(err.Error(), "already") {
					return // an index named like a table: SQLite's namespace, not the compiler's
				}
				t.Fatalf("DDL %s: %v", stmt, err)
			}
		}
		if serr == nil {
			stmt, err := conn.PrepareContext(t.Context(), q.Text)
			if err != nil {
				t.Fatalf("select does not prepare: %s: %v", q.Text, err)
			}
			stmt.Close()
		}
	})
}
