package workload_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nickstrad/ts-db-go/internal/actor"
	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/schema"
	"github.com/nickstrad/ts-db-go/internal/sqlitedsn"
	"github.com/nickstrad/ts-db-go/internal/workload"
)

// seeded builds s in a private in-memory database with foreign keys enforced, as the actor opens every database.
func seeded(t testing.TB, s *schema.Schema, seed uint64) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqlitedsn.Build(":memory:"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(s.DDL()); err != nil {
		t.Fatalf("DDL: %v\n%s", err, s.DDL())
	}
	if err := s.Fill(seed, func(q string, args ...any) error { _, err := db.Exec(q, args...); return err }); err != nil {
		t.Fatal(err)
	}
	return db
}

// run executes st under timeout and reports the error and whether the timeout ended it.
func run(db *sql.DB, st workload.Statement, timeout time.Duration) (err error, timedOut bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if st.Write {
		_, err = db.ExecContext(ctx, st.SQL, st.Args...)
	} else {
		var rows *sql.Rows
		if rows, err = db.QueryContext(ctx, st.SQL, st.Args...); err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
	}
	return err, ctx.Err() != nil
}

// Statement timeouts for checkRuns. An expected-slow class gets a short one, so the test does not wait for a
// cross join to finish; any other class must succeed within a generous one. 30 ms for every class flaked
// under load (go test -race ./... runs packages in parallel), failing fast statements that were merely
// descheduled (CP-8).
const (
	slowTimeout = 30 * time.Millisecond
	fastTimeout = 5 * time.Second
)

// checkRuns asserts st's contract: its Write flag matches its SQL, and it succeeds unless it is expected to be
// slow and the timeout ended it.
func checkRuns(t testing.TB, db *sql.DB, s *schema.Schema, st workload.Statement) {
	t.Helper()
	if want := actor.IsWrite(st.SQL); want != st.Write {
		t.Fatalf("%s: Write=%v but SQL looks like write=%v: %s", st.Class, st.Write, want, st.SQL)
	}
	timeout := fastTimeout
	if st.ExpectSlow {
		timeout = slowTimeout
	}
	if err, timedOut := run(db, st, timeout); err != nil && !(st.ExpectSlow && timedOut) {
		t.Fatalf("%s: %v\nSQL: %s\nargs: %v\nDDL:\n%s", st.Class, err, st.SQL, st.Args, s.DDL())
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

func TestClassTable(t *testing.T) {
	if !slices.IsSorted(workload.Classes) || len(workload.Classes) != 14 {
		t.Fatalf("Classes must be the 14 classes, sorted: %v", workload.Classes)
	}
	for i, c := range workload.Classes {
		if workload.Index(c) != i {
			t.Fatalf("Index(%s) = %d, want %d", c, workload.Index(c), i)
		}
	}
	if workload.Index("nope") != -1 {
		t.Fatal("Index of an unknown class must be -1")
	}
	if _, ok := workload.Lookup("nope"); ok {
		t.Fatal("Lookup of an unknown class must fail")
	}
	for c := range workload.DefaultMix() {
		if _, ok := workload.Lookup(c); !ok {
			t.Errorf("unknown class %q in DefaultMix", c)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	ok := workload.DefaultOptions()
	with := func(f func(*workload.Options)) workload.Options { o := ok; f(&o); return o }
	tests := []struct {
		name  string
		opts  workload.Options
		valid bool
	}{
		{"defaults", ok, true},
		{"zero value", workload.Options{}, false},
		{"bulk_min 1 (on point)", with(func(o *workload.Options) { o.BulkMin = 1 }), true},
		{"bulk_min 0 (off point)", with(func(o *workload.Options) { o.BulkMin = 0 }), false},
		{"bulk_max = bulk_min (on point)", with(func(o *workload.Options) { o.BulkMin, o.BulkMax = 7, 7 }), true},
		{"bulk_max below bulk_min", with(func(o *workload.Options) { o.BulkMin, o.BulkMax = 7, 6 }), false},
		{"range_max 0", with(func(o *workload.Options) { o.RangeMax = 0 }), false},
		{"slow_cte_iterations 0", with(func(o *workload.Options) { o.SlowCTEIterations = 0 }), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.opts.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate(%+v) = %v, want valid=%v", tc.opts, err, tc.valid)
			}
		})
	}
}

// Classes a schema cannot support fall back to the documented class (see Generator.Next).
func TestFallbacks(t *testing.T) {
	// One root table, one indexed integer column, nothing unindexed, no children.
	indexedOnly := &schema.Schema{Tables: []schema.Table{{Name: "t", Parent: -1, SeedRows: 5,
		Columns: []schema.Column{{Name: "a", Type: schema.Integer, Cardinality: 5, Indexed: true}}}}}
	// One root table with an unindexed blob column: no index, nothing filterable.
	blobOnly := &schema.Schema{Tables: []schema.Table{{Name: "t", Parent: -1, SeedRows: 5,
		Columns: []schema.Column{{Name: "b", Type: schema.Blob, Cardinality: 5}}}}}
	tests := []struct {
		name string
		s    *schema.Schema
		ask  workload.Class
		want workload.Class
	}{
		{"index_lookup without an indexed column", blobOnly, workload.IndexLookup, workload.PointRead},
		{"index_lookup with one (off point)", indexedOnly, workload.IndexLookup, workload.IndexLookup},
		{"filter_scan without an unindexed scalar", indexedOnly, workload.FilterScan, workload.RangeScan},
		{"filter_scan skips blobs", blobOnly, workload.FilterScan, workload.RangeScan},
		{"join without a child table", indexedOnly, workload.Join, workload.IndexLookup},
		{"join chain falls through twice", blobOnly, workload.Join, workload.PointRead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := workload.NewGenerator(tc.s, workload.DefaultOptions())
			st := g.Next(rand.New(rand.NewPCG(1, 1)), tc.ask)
			if st.Class != tc.want {
				t.Fatalf("Next(%s) made %s, want %s", tc.ask, st.Class, tc.want)
			}
			checkRuns(t, seeded(t, tc.s, 1), tc.s, st)
		})
	}
}

func TestBulkInsertSizeStaysInBounds(t *testing.T) {
	s := schema.Generate(2, 0, schema.DefaultOptions())
	opts := workload.DefaultOptions()
	opts.BulkMin, opts.BulkMax = 3, 4
	g := workload.NewGenerator(s, opts)
	r := rand.New(rand.NewPCG(2, 2))
	seen := map[int]bool{}
	for range 200 {
		st := g.Next(r, workload.BulkInsert)
		perRow := len(s.Tables[slices.IndexFunc(s.Tables, func(t schema.Table) bool { return t.Name == st.Table })].ColumnNames())
		rows := len(st.Args) / perRow
		if rows < 3 || rows > 4 || len(st.Args)%perRow != 0 {
			t.Fatalf("bulk insert of %d args (%d per row)", len(st.Args), perRow)
		}
		seen[rows] = true
	}
	if !seen[3] || !seen[4] {
		t.Fatalf("both ends of [3, 4] should occur: %v", seen)
	}
}

// --- assertions can fire ---------------------------------------------------------------------------------------

func TestAssertionsCanFire(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out (-tags noassert)")
	}
	s := schema.Generate(1, 0, schema.DefaultOptions())
	mustPanic(t, "NewGenerator with invalid options", func() { workload.NewGenerator(s, workload.Options{}) })
	g := workload.NewGenerator(s, workload.DefaultOptions())
	mustPanic(t, "Next with an unknown class", func() { g.Next(rand.New(rand.NewPCG(1, 1)), "nope") })
	col := []schema.Column{{Name: "x", Type: schema.Integer, Cardinality: 2}}
	cycle := &schema.Schema{Tables: []schema.Table{{Name: "a", Parent: 1, Columns: col}, {Name: "b", Parent: 0, Columns: col}}}
	mustPanic(t, "NewGenerator on a schema with no leaf table", func() { workload.NewGenerator(cycle, workload.DefaultOptions()) })
}

// --- integration and properties ------------------------------------------------------------------------------------

// Every class, on many random schemas, produces SQL that SQLite accepts. Schemas run in parallel; the
// expected-slow classes run once per schema with a short timeout (TestSlowClassesExceedATimeout covers the
// timeout itself).
func TestEveryClassRunsOnRandomSchemas(t *testing.T) {
	schemas := 40
	if testing.Short() {
		schemas = 10
	}
	opts := schema.DefaultOptions()
	opts.MaxRows = 120 // keep cross joins small enough to sometimes finish
	for db := range schemas {
		t.Run(fmt.Sprint("db", db), func(t *testing.T) {
			t.Parallel()
			s := schema.Generate(42, db, opts)
			conn := seeded(t, s, 42)
			g := workload.NewGenerator(s, workload.DefaultOptions())
			r := rand.New(rand.NewPCG(7, uint64(db)))
			for _, class := range workload.Classes {
				reps := 5
				if info, _ := workload.Lookup(class); info.ExpectSlow {
					reps = 1
				}
				for range reps {
					checkRuns(t, conn, s, g.Next(r, class))
				}
			}
		})
	}
}

func TestSlowClassesExceedATimeout(t *testing.T) {
	opts := schema.DefaultOptions()
	opts.MinRows, opts.MaxRows = 400, 400
	s := schema.Generate(1, 0, opts)
	conn := seeded(t, s, 1)
	g := workload.NewGenerator(s, workload.DefaultOptions())
	r := rand.New(rand.NewPCG(1, 1))
	for _, class := range []workload.Class{workload.SlowCTE, workload.CrossJoin} {
		t0 := time.Now()
		err, timedOut := run(conn, g.Next(r, class), 50*time.Millisecond)
		if err == nil || !timedOut {
			t.Fatalf("%s finished in %s (err %v); it should outlast a 50ms timeout", class, time.Since(t0), err)
		}
		if d := time.Since(t0); d > time.Second {
			t.Fatalf("%s took %s to be interrupted", class, d)
		}
	}
}

// The generator tracks ids: after inserts, point reads still almost always hit a row.
func TestInsertsExtendTheIDRange(t *testing.T) {
	opts := schema.DefaultOptions()
	opts.MinTables, opts.MaxTables, opts.MinRows, opts.MaxRows = 1, 1, 10, 10
	s := schema.Generate(3, 0, opts)
	conn := seeded(t, s, 3)
	g := workload.NewGenerator(s, workload.DefaultOptions())
	r := rand.New(rand.NewPCG(3, 3))
	for range 20 {
		st := g.Next(r, workload.Insert)
		if _, err := conn.Exec(st.SQL, st.Args...); err != nil {
			t.Fatal(err)
		}
	}
	hits := 0
	for range 200 {
		st := g.Next(r, workload.PointRead)
		var n int
		if err := conn.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM (%s)", st.SQL), st.Args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		hits += n
	}
	if hits < 190 {
		t.Fatalf("point reads hit %d/200 rows; id tracking is off", hits)
	}
}

// Property: for any schema and any sequence of classes, every generated statement runs on that schema's
// database (or, for the expected-slow classes, is ended by the timeout).
func FuzzStatementsRun(f *testing.F) {
	f.Add(uint64(1), uint16(0), []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11})
	f.Add(uint64(9), uint16(17), []byte{8, 8, 8, 3, 3, 3})
	f.Fuzz(func(t *testing.T, seed uint64, db uint16, script []byte) {
		if len(script) > 64 {
			t.Skip()
		}
		opts := schema.DefaultOptions()
		opts.MinRows, opts.MaxRows = 0, 20
		s := schema.Generate(seed, int(db), opts)
		conn := seeded(t, s, seed)
		g := workload.NewGenerator(s, workload.DefaultOptions())
		r := rand.New(rand.NewPCG(seed, uint64(db)))
		for _, b := range script {
			c := workload.Classes[int(b)%len(workload.Classes)]
			if c == workload.SlowCTE {
				continue // always runs to the timeout; covered by TestSlowClassesExceedATimeout
			}
			checkRuns(t, conn, s, g.Next(r, c))
		}
	})
}

// PG-05: with foreign keys enforced, a long random mix of every write class never violates one: deletes go only
// to leaf tables, and parent_id values always name an existing parent row. ParentProb 1 makes every table after
// the first a child, so the chain of parents is as deep as it gets.
func TestWritesNeverViolateForeignKeys(t *testing.T) {
	opts := schema.Options{MinTables: 2, MaxTables: 5, MinColumns: 1, MaxColumns: 4, MinRows: 0, MaxRows: 20, IndexProb: 0.3, ParentProb: 1}
	writes := []workload.Class{workload.Insert, workload.BulkInsert, workload.Update, workload.Delete, workload.Upsert}
	for db := range 20 {
		s := schema.Generate(9, db, opts)
		conn := seeded(t, s, 9)
		g := workload.NewGenerator(s, workload.DefaultOptions())
		referenced := map[string]bool{}
		for _, tb := range s.Tables {
			if tb.HasParent() {
				referenced[s.Tables[tb.Parent].Name] = true
			}
		}
		r := rand.New(rand.NewPCG(9, uint64(db)))
		for range 300 {
			st := g.Next(r, writes[r.IntN(len(writes))])
			if st.Class == workload.Delete && referenced[st.Table] {
				t.Fatalf("db %d: delete targets %s, which other tables reference", db, st.Table)
			}
			if _, err := conn.Exec(st.SQL, st.Args...); err != nil {
				t.Fatalf("db %d: %s: %v\nSQL: %s\nargs: %v\nDDL:\n%s", db, st.Class, err, st.SQL, st.Args, s.DDL())
			}
		}
		var violations int
		if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
			t.Fatalf("db %d: %d foreign key violations (%v)", db, violations, err)
		}
	}
}
