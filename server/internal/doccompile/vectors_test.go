package doccompile_test

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/doccompile"
	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

var (
	repoRoot  = filepath.Join("..", "..", "..")
	vectorDir = filepath.Join(repoRoot, "ir", "vectors")
)

func parseFile(t testing.TB, path string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, perr := ir.Parse(string(data))
	if perr != nil {
		t.Fatalf("%s: %v", path, perr)
	}
	return v
}

// planOf reads a SelectPlan's JSON as the TypeScript compiler sees it: members read as they are, names
// stringified, an absent filter value Undefined.
func planOf(v any) doccompile.Plan {
	o, ok := v.(*ir.Object)
	if !ok {
		return doccompile.Plan{Limit: ir.Undefined{}}
	}
	p := doccompile.Plan{Table: ir.ToString(o.Value("table")), Limit: o.Value("limit")}
	if list, ok := o.Value("filters").([]any); ok {
		for _, item := range list {
			f, ok := item.(*ir.Object)
			if !ok {
				continue
			}
			p.Filters = append(p.Filters, ir.Filter{
				Field: ir.ToString(f.Value("field")), Op: ir.ToString(f.Value("op")), Value: f.Value("value"),
			})
		}
	}
	if order, ok := o.Value("order").(*ir.Object); ok {
		p.Order = &ir.Order{Field: ir.ToString(order.Value("field")), Direction: ir.ToString(order.Value("direction"))}
	}
	if agg, ok := o.Value("aggregate").(*ir.Object); ok {
		p.Aggregate = &ir.Aggregate{Kind: ir.ToString(agg.Value("kind")), Field: ir.ToString(agg.Value("field"))}
	}
	return p
}

// outcome is a result in the vectors' form: {key: value} or {"error": {tag, fields}}.
func outcome(key string, value any, err *ir.Error) *ir.Object {
	if err != nil {
		return ir.NewObject("error", ir.NewObject("tag", err.Tag, "fields", err.Fields))
	}
	return ir.NewObject(key, value)
}

func statements(list []string) any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

func TestCompileVectors(t *testing.T) {
	vectors := parseFile(t, filepath.Join(vectorDir, "compile.json")).(*ir.Object).Value("vectors").([]any)
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, item := range vectors {
		v := item.(*ir.Object)
		name := v.Value("name").(string)
		s, serr := docschema.Decode(v.Value("schema"))
		if serr != nil {
			t.Fatalf("%s: schema: %v", name, serr)
		}
		table, _ := v.Value("table").(string)
		id, _ := v.Value("id").(string)
		var got *ir.Object
		switch v.Value("kind") {
		case "ddl":
			list, err := doccompile.DDL(s)
			got = outcome("statements", statements(list), err)
		case "select":
			sql, err := doccompile.Select(s, planOf(v.Value("plan")), doccompile.SQLite)
			got = outcome("sql", sql.JSON(), err)
		case "insert":
			meta := v.Value("meta").(*ir.Object)
			sql, err := doccompile.Insert(s, table, v.Value("value"), meta.Value("_id").(string), meta.Value("_creationTime").(float64))
			got = outcome("sql", sql.JSON(), err)
		case "replace":
			sql, err := doccompile.Replace(s, table, id, v.Value("value"))
			got = outcome("sql", sql.JSON(), err)
		default:
			sql, err := doccompile.Delete(s, table, id)
			got = outcome("sql", sql.JSON(), err)
		}
		if want := v.Value("expect"); ir.Canonical(got) != ir.Canonical(want) {
			t.Errorf("%s:\n got %s\nwant %s", name, ir.Canonical(got), ir.Canonical(want))
		}
	}
}

// corpusLine recompiles one corpus case and returns the line Go would write: the case's own members with
// select and ddl replaced by the Go compiler's outputs (ir/vectors/README.md).
func corpusLine(line string) (string, *ir.Error) {
	v, err := ir.Parse(line)
	if err != nil {
		return "", err
	}
	c := v.(*ir.Object)
	s := docschema.Load(c.Value("schema"))
	sql, serr := doccompile.Select(s, planOf(c.Value("plan")), doccompile.SQLite)
	c.Set("select", outcome("sql", sql.JSON(), serr))
	list, derr := doccompile.DDL(s)
	c.Set("ddl", outcome("statements", statements(list), derr))
	return ir.Stringify(c), nil
}

// runCorpus compares every case of a corpus file and returns the number of cases.
func runCorpus(t *testing.T, path string) int {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	if !sc.Scan() {
		t.Fatalf("%s: empty", path)
	}
	header, perr := ir.Parse(sc.Text())
	if perr != nil || header.(*ir.Object).Value("format") != "mansio/ir-compile-corpus" {
		t.Fatalf("%s: bad header %q", path, sc.Text())
	}
	lines := make(chan string, 1024)
	var mu sync.Mutex
	var failures, n int
	var wg sync.WaitGroup
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for line := range lines {
				got, err := corpusLine(line)
				if err != nil || got != line {
					mu.Lock()
					failures++
					if failures <= 5 {
						t.Errorf("%s: mismatch (parse error %v)\n got %s\nwant %s", path, err, got, line)
					}
					mu.Unlock()
				}
			}
		}()
	}
	for sc.Scan() {
		n++
		lines <- sc.Text()
	}
	close(lines)
	wg.Wait()
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if failures > 0 {
		t.Errorf("%s: %d of %d cases differ", path, failures, n)
	}
	if want := header.(*ir.Object).Value("count"); want != float64(n) {
		t.Errorf("%s: %d cases, header says %v", path, n, want)
	}
	return n
}

// TestCompileCorpus runs the committed differential corpus. MANSIO_IR_CORPUS, a comma-separated list of
// corpus files, replaces it for larger local runs (scripts/dev/ir-differential.sh).
func TestCompileCorpus(t *testing.T) {
	paths := []string{filepath.Join(vectorDir, "generated", "compile-corpus.jsonl")}
	if env := os.Getenv("MANSIO_IR_CORPUS"); env != "" {
		paths = strings.Split(env, ",")
	}
	start, total := time.Now(), 0
	for _, path := range paths {
		total += runCorpus(t, path)
	}
	t.Logf("%d corpus cases in %v", total, time.Since(start).Round(time.Millisecond))
}

// TestGeneratedSQL regenerates core/compile/GENERATED_SQL.txt the way scripts/explain.ts does (`deno task explain`):
// the app schema (testdata/app_schema.json, JSON.stringify of examples/tic-tac-toe/schema.ts) and the SDK plan of
// db.find("games", {status: "playing"}).sort("_creationTime", "desc").limit(20)
// (testdata/explain_plan.json), in both layouts and both dialects. Byte-identical to the committed file.
func TestGeneratedSQL(t *testing.T) {
	schema, err := docschema.Decode(parseFile(t, filepath.Join("testdata", "app_schema.json")))
	if err != nil {
		t.Fatal(err)
	}
	planJSON := parseFile(t, filepath.Join("testdata", "explain_plan.json"))
	var b strings.Builder
	b.WriteString("QUERY PLAN " + ir.StringifyIndent(planJSON, "  ") + "\n")
	for _, storage := range []string{docschema.Columns, docschema.JSON} {
		physical := schema.WithStorage(storage)
		ddl, err := doccompile.DDL(physical)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("\n" + strings.ToUpper(storage) + " DDL\n" + strings.Join(ddl, ";\n") + ";\n")
		for _, d := range []doccompile.Dialect{doccompile.SQLite, doccompile.DuckDB} {
			sql, err := doccompile.Select(physical, planOf(planJSON), d)
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString(string(d) + " " + ir.StringifyIndent(sql.JSON(), "  ") + "\n")
		}
	}
	want, rerr := os.ReadFile(filepath.Join(repoRoot, "core", "compile", "GENERATED_SQL.txt"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got := b.String(); got != string(want) {
		t.Errorf("core/compile/GENERATED_SQL.txt differs:\n%s", got)
	}
}
