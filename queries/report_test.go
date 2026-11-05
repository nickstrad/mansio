// Golden tests for the DuckDB reports (SQL G-20): each .sql file runs against the fixture run in
// testdata/run and must print exactly testdata/<name>.expected. Rewrite the expected files with
//
//	go test ./queries -update
//
// and review the diff. The tests skip when the duckdb CLI is not on PATH (brew install duckdb).
package queries_test

import (
	"bytes"
	"context"
	"flag"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/bench"
	"github.com/nickstrad/ts-db-go/internal/load"
)

var update = flag.Bool("update", false, "rewrite testdata/*.expected from the current output")

// duckdb runs script (stdin) in dir with one thread, so row order and float sums are reproducible.
func duckdb(t *testing.T, dir, script string) []byte {
	t.Helper()
	bin, err := exec.LookPath("duckdb")
	if err != nil {
		t.Skip("duckdb CLI not on PATH (brew install duckdb)")
	}
	args := []string{"-box", "-cmd", "SET threads = 1"}
	if exec.Command(bin, "-no-agent", "-c", "SELECT 1").Run() == nil { // newer CLIs: plain output for agents too
		args = append([]string{"-no-agent"}, args...)
	}
	in, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir, cmd.Stdin = dir, bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() > 0 {
		t.Fatalf("duckdb < %s in %s: %v\n%s", script, dir, err, stderr.String())
	}
	return out
}

// tree lays out a repo-shaped directory: queries/*.sql and results/<run>/ holding the fixture, one per name.
func tree(t *testing.T, runs ...string) string {
	t.Helper()
	root := t.TempDir()
	copyDir(t, ".", filepath.Join(root, "queries"), ".sql")
	for _, r := range runs {
		copyDir(t, filepath.Join("testdata", "run"), filepath.Join(root, "results", r), ".csv")
	}
	return root
}

func copyDir(t *testing.T, from, to, ext string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ext {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".expected")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from %s (run with -update and review the diff)\n--- got ---\n%s", name, path, got)
	}
}

func TestReportGolden(t *testing.T) {
	root := tree(t, "fixture")
	run := filepath.Join(root, "results", "fixture")
	golden(t, "report", duckdb(t, run, filepath.Join(root, "queries", "report.sql")))
}

func TestCompareGolden(t *testing.T) {
	root := tree(t, "run-a", "run-b")
	golden(t, "compare", duckdb(t, root, filepath.Join(root, "queries", "compare.sql")))
}

// The header each writer produces equals the column list views.sql declares, in order, so renaming a column
// in internal/stats or internal/bench fails here rather than in a report nobody has read yet.
func TestViewsMatchTheWritersHeaders(t *testing.T) {
	views, err := os.ReadFile("views.sql")
	if err != nil {
		t.Fatal(err)
	}
	declared := declaredColumns(t, string(views))
	out := tinyRun(t)
	for file, macro := range map[string]string{
		"requests.csv": "requests_csv", "samples.csv": "samples_csv", "phases.csv": "phases_csv", "schemas.csv": "schemas_csv",
	} {
		b, err := os.ReadFile(filepath.Join(out, file))
		if err != nil {
			t.Fatal(err)
		}
		header, _, _ := strings.Cut(string(b), "\n")
		if want := strings.Join(declared[macro], ","); header != want {
			t.Errorf("%s header\n  %s\nviews.sql %s\n  %s", file, header, macro, want)
		}
		fixture, err := os.ReadFile(filepath.Join("testdata", "run", file))
		if err != nil {
			t.Fatal(err)
		}
		if fh, _, _ := strings.Cut(string(fixture), "\n"); fh != header {
			t.Errorf("testdata/run/%s is stale: header %s, writer %s (regenerate the fixture)", file, fh, header)
		}
	}
}

var (
	macroRE  = regexp.MustCompile(`(?s)CREATE OR REPLACE MACRO (\w+)\(path\) AS TABLE.*?columns = \{(.*?)\}`)
	columnRE = regexp.MustCompile(`'(\w+)': '\w+'`)
)

func declaredColumns(t *testing.T, sql string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, m := range macroRE.FindAllStringSubmatch(sql, -1) {
		for _, c := range columnRE.FindAllStringSubmatch(m[2], -1) {
			out[m[1]] = append(out[m[1]], c[1])
		}
	}
	if len(out) != 4 {
		t.Fatalf("views.sql: found macros %v, want 4", out)
	}
	return out
}

// tinyRun runs a sub-second sqlbench load and returns its output directory.
func tinyRun(t *testing.T) string {
	t.Helper()
	body := `{"name":"tiny","workers":4,"keep_dbs":false,"sample_every":"50ms",
		"schema":{"min_tables":1,"max_tables":2,"min_rows":5,"max_rows":20},
		"phases":[{"name":"one","dbs":2,"duration":"150ms","start_qps":100}]}`
	c, err := load.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c.OutDir, c.DBDir = filepath.Join(dir, "out"), filepath.Join(dir, "dbs")
	if err := os.MkdirAll(c.OutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := bench.Run(context.Background(), c, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return c.OutDir
}
