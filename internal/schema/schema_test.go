package schema_test

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/schema"
)

// memDB is a private in-memory database (one connection, so every statement sees the same database) with
// foreign keys enforced, as the actor opens every database.
func memDB(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// build creates s in db and fills it.
func build(t testing.TB, db *sql.DB, s *schema.Schema, seed uint64) {
	t.Helper()
	if _, err := db.Exec(s.DDL()); err != nil {
		t.Fatalf("DDL: %v\n%s", err, s.DDL())
	}
	if err := s.Fill(seed, func(q string, args ...any) error { _, err := db.Exec(q, args...); return err }); err != nil {
		t.Fatalf("Fill: %v\n%s", err, s.DDL())
	}
}

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: want an assertion panic", name)
		}
	}()
	f()
}

// --- specification tests -------------------------------------------------------------------------------------

func TestOptionsValidate(t *testing.T) {
	ok := schema.DefaultOptions()
	with := func(f func(*schema.Options)) schema.Options { o := ok; f(&o); return o }
	tests := []struct {
		name  string
		opts  schema.Options
		valid bool
	}{
		{"defaults", ok, true},
		{"min_tables 1 (on point)", with(func(o *schema.Options) { o.MinTables, o.MaxTables = 1, 1 }), true},
		{"min_tables 0 (off point)", with(func(o *schema.Options) { o.MinTables = 0 }), false},
		{"max_tables below min", with(func(o *schema.Options) { o.MinTables, o.MaxTables = 3, 2 }), false},
		{"min_columns 1 (on point)", with(func(o *schema.Options) { o.MinColumns, o.MaxColumns = 1, 1 }), true},
		{"min_columns 0 (off point)", with(func(o *schema.Options) { o.MinColumns = 0 }), false},
		{"max_columns below min", with(func(o *schema.Options) { o.MinColumns, o.MaxColumns = 4, 3 }), false},
		{"zero rows allowed", with(func(o *schema.Options) { o.MinRows, o.MaxRows = 0, 0 }), true},
		{"negative min_rows", with(func(o *schema.Options) { o.MinRows = -1 }), false},
		{"max_rows below min", with(func(o *schema.Options) { o.MinRows, o.MaxRows = 5, 4 }), false},
		{"index_prob 1 (on point)", with(func(o *schema.Options) { o.IndexProb = 1 }), true},
		{"index_prob just above 1", with(func(o *schema.Options) { o.IndexProb = 1.0001 }), false},
		{"parent_prob 0 (on point)", with(func(o *schema.Options) { o.ParentProb = 0 }), true},
		{"parent_prob negative", with(func(o *schema.Options) { o.ParentProb = -0.0001 }), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.opts.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate(%+v) = %v, want valid=%v", tc.opts, err, tc.valid)
			}
		})
	}
}

func TestPlaceholders(t *testing.T) {
	for n, want := range map[int]string{0: "()", 1: "(?)", 3: "(?, ?, ?)"} {
		if got := schema.Placeholders(n); got != want {
			t.Errorf("Placeholders(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestInsertSQLAndColumnNames(t *testing.T) {
	child := schema.Table{Name: "moves", Parent: 0, Columns: []schema.Column{{Name: "x"}, {Name: "y"}}}
	root := schema.Table{Name: "games", Parent: -1, Columns: []schema.Column{{Name: "z"}}}
	tests := []struct {
		name  string
		t     schema.Table
		rows  int
		cols  []string
		query string
	}{
		{"child, one row", child, 1, []string{"parent_id", "x", "y"}, "INSERT INTO moves (parent_id, x, y) VALUES (?, ?, ?)"},
		{"child, two rows", child, 2, []string{"parent_id", "x", "y"}, "INSERT INTO moves (parent_id, x, y) VALUES (?, ?, ?), (?, ?, ?)"},
		{"root has no parent_id", root, 1, []string{"z"}, "INSERT INTO games (z) VALUES (?)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.t.ColumnNames(); !reflect.DeepEqual(got, tc.cols) {
				t.Errorf("ColumnNames = %v, want %v", got, tc.cols)
			}
			if got := tc.t.InsertSQL(tc.rows); got != tc.query {
				t.Errorf("InsertSQL(%d) = %q, want %q", tc.rows, got, tc.query)
			}
		})
	}
}

func TestAppendRowBoundsParentID(t *testing.T) {
	child := schema.Table{Name: "c", Parent: 0, Columns: []schema.Column{{Name: "x", Type: schema.Integer, Cardinality: 3}}}
	r := rand.New(rand.NewPCG(1, 1))
	for _, parents := range []int64{-1, 0, 1, 7} {
		for range 200 {
			args := child.AppendRow(nil, r, parents)
			if len(args) != 2 {
				t.Fatalf("%d args, want 2", len(args))
			}
			if parents < 1 { // no parent row exists: NULL, which a foreign key accepts
				if args[0] != nil {
					t.Fatalf("parents %d: parent_id %v, want NULL", parents, args[0])
				}
				continue
			}
			if p := args[0].(int64); p < 1 || p > parents {
				t.Fatalf("parent_id %d outside [1, %d]", p, parents)
			}
		}
	}
}

func TestValueMatchesTheColumnType(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	for _, tc := range []struct {
		typ  schema.Type
		kind reflect.Kind
	}{
		{schema.Integer, reflect.Int64}, {schema.Real, reflect.Float64}, {schema.Text, reflect.String}, {schema.Blob, reflect.Slice},
	} {
		col := schema.Column{Type: tc.typ, Cardinality: 5}
		for range 50 {
			v := schema.Value(r, col)
			if k := reflect.TypeOf(v).Kind(); k != tc.kind {
				t.Fatalf("%s: got %T", tc.typ, v)
			}
			if n, ok := v.(int64); ok && (n < 0 || n >= col.Cardinality) {
				t.Fatalf("integer %d outside [0, %d)", n, col.Cardinality)
			}
			if b, ok := v.([]byte); ok && (len(b) < 16 || len(b) > 255) {
				t.Fatalf("blob of %d bytes", len(b))
			}
		}
	}
}

func TestFillReportsTheFailingTable(t *testing.T) {
	s := schema.Generate(1, 0, schema.DefaultOptions())
	boom := errors.New("disk full")
	err := s.Fill(1, func(string, ...any) error { return boom })
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), s.Tables[0].Name) {
		t.Fatalf("Fill error = %v; want it to wrap the cause and name table %s", err, s.Tables[0].Name)
	}
}

func TestCatalog(t *testing.T) {
	a := schema.NewCatalog(1, schema.DefaultOptions())
	a.Ensure(10)
	a.Ensure(4) // asking for fewer is a no-op
	for i := range 10 {
		if got := a.Get(i); got.DB != i || !reflect.DeepEqual(got, schema.Generate(1, i, schema.DefaultOptions())) {
			t.Fatalf("Get(%d) is not Generate(1, %d)", i, i)
		}
	}
	moreRows := schema.DefaultOptions()
	moreRows.MaxRows++
	if a.Fingerprint() == schema.NewCatalog(2, schema.DefaultOptions()).Fingerprint() ||
		a.Fingerprint() == schema.NewCatalog(1, moreRows).Fingerprint() {
		t.Fatal("fingerprint ignores the seed or the options")
	}
}

// --- assertions can fire ---------------------------------------------------------------------------------------

func TestAssertionsCanFire(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out (-tags noassert)")
	}
	mustPanic(t, "Generate with invalid options", func() { schema.Generate(1, 0, schema.Options{}) })
	mustPanic(t, "InsertSQL with zero rows", func() { (&schema.Table{Name: "x", Parent: -1}).InsertSQL(0) })
}

// --- properties ---------------------------------------------------------------------------------------------------

// checkSchema asserts Generate's postconditions for opts.
func checkSchema(t testing.TB, s *schema.Schema, o schema.Options) {
	t.Helper()
	if n := len(s.Tables); n < o.MinTables || n > o.MaxTables {
		t.Fatalf("%d tables, want %d..%d", n, o.MinTables, o.MaxTables)
	}
	names := map[string]bool{}
	for i, tb := range s.Tables {
		if names[tb.Name] {
			t.Fatalf("duplicate table %s", tb.Name)
		}
		names[tb.Name] = true
		if tb.Parent < -1 || tb.Parent >= i {
			t.Fatalf("table %d has parent %d; must be -1 or an earlier table", i, tb.Parent)
		}
		if n := len(tb.Columns); n < o.MinColumns || n > o.MaxColumns {
			t.Fatalf("%s: %d columns, want %d..%d", tb.Name, n, o.MinColumns, o.MaxColumns)
		}
		if tb.SeedRows < o.MinRows || tb.SeedRows > o.MaxRows {
			t.Fatalf("%s: %d rows, want %d..%d", tb.Name, tb.SeedRows, o.MinRows, o.MaxRows)
		}
		cols := map[string]bool{"id": true, "parent_id": true}
		for _, c := range tb.Columns {
			if cols[c.Name] {
				t.Fatalf("%s: duplicate or reserved column %s", tb.Name, c.Name)
			}
			cols[c.Name] = true
			if c.Indexed && c.Type == schema.Blob {
				t.Fatalf("%s.%s: blob columns are never indexed", tb.Name, c.Name)
			}
		}
	}
}

func TestGenerateIsDeterministicAndVaried(t *testing.T) {
	opts := schema.DefaultOptions()
	if !reflect.DeepEqual(schema.Generate(9, 3, opts), schema.Generate(9, 3, opts)) {
		t.Fatal("same seed and db gave different schemas")
	}
	distinct := map[string]bool{}
	for db := range 50 {
		s := schema.Generate(9, db, opts)
		checkSchema(t, s, opts)
		distinct[s.DDL()] = true
	}
	if len(distinct) < 45 {
		t.Fatalf("only %d distinct schemas out of 50", len(distinct))
	}
}

func TestParentProbOneLinksEveryLaterTable(t *testing.T) {
	opts := schema.Options{MinTables: 2, MaxTables: 3, MinColumns: 1, MaxColumns: 4, MinRows: 5, MaxRows: 9, IndexProb: 1, ParentProb: 1}
	for db := range 100 {
		s := schema.Generate(1, db, opts)
		checkSchema(t, s, opts)
		for i, tb := range s.Tables[1:] {
			if !tb.HasParent() {
				t.Fatalf("db %d table %d: parent_prob 1 but no parent", db, i+1)
			}
		}
	}
}

// Fill is deterministic: the same seed issues the same statements with the same arguments, one per seed row.
func TestFillIsDeterministic(t *testing.T) {
	s := schema.Generate(4, 2, schema.DefaultOptions())
	record := func() []string {
		var calls []string
		_ = s.Fill(4, func(q string, args ...any) error { calls = append(calls, fmt.Sprint(q, args)); return nil })
		return calls
	}
	a, b := record(), record()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two Fills with one seed differ")
	}
	want := 0
	for _, tb := range s.Tables {
		want += tb.SeedRows
	}
	if len(a) != want {
		t.Fatalf("%d inserts, want %d", len(a), want)
	}
}

// Integration: generated DDL and seed rows are valid SQLite, and every table gets exactly its rows.
func TestBuiltDatabasesHaveEveryTableAndRow(t *testing.T) {
	for db := range 25 {
		t.Run(fmt.Sprint("db", db), func(t *testing.T) {
			t.Parallel()
			s := schema.Generate(5, db, schema.DefaultOptions())
			conn := memDB(t)
			build(t, conn, s, 5)
			for _, tb := range s.Tables {
				var n int
				if err := conn.QueryRow("SELECT COUNT(*) FROM " + tb.Name).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != tb.SeedRows {
					t.Fatalf("%s: %d rows, want %d", tb.Name, n, tb.SeedRows)
				}
			}
		})
	}
}

// Property: any valid options produce a schema that meets Generate's postconditions and whose DDL and seed data
// SQLite accepts.
func FuzzGenerate(f *testing.F) {
	f.Add(uint64(1), uint16(0), uint8(0), uint8(4), uint8(1), uint8(7), uint8(77), uint8(128))
	f.Add(uint64(7), uint16(99), uint8(0), uint8(0), uint8(0), uint8(0), uint8(255), uint8(255))
	f.Fuzz(func(t *testing.T, seed uint64, db uint16, minT, spanT, minC, spanC, indexP, parentP uint8) {
		o := schema.Options{
			MinTables: 1 + int(minT%4), MinColumns: 1 + int(minC%6), MaxRows: 5,
			IndexProb: float64(indexP) / 255, ParentProb: float64(parentP) / 255,
		}
		o.MaxTables = o.MinTables + int(spanT%4)
		o.MaxColumns = o.MinColumns + int(spanC%6)
		s := schema.Generate(seed, int(db), o)
		checkSchema(t, s, o)
		build(t, memDB(t), s, seed)
	})
}
