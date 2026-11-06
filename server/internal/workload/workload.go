// Package workload turns a database's schema into SQL statements for different kinds of work. It is pure: it
// builds SQL text and arguments; callers execute them.
//
// Each Class makes one shape of statement against a randomly chosen table of the target database: point reads,
// index lookups, range and filter scans, aggregates, joins, top-N sorts, inserts, updates, deletes, upserts,
// bulk inserts, and statements meant to exceed the statement timeout. A Generator precomputes every SQL string
// for its schema once, so Next only draws arguments, and tracks roughly how many ids each table has so reads
// mostly hit rows and inserts extend the range.
package workload

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/schema"
)

// Class names a kind of statement.
type Class string

const (
	PointRead   Class = "point_read"
	IndexLookup Class = "index_lookup"
	RangeScan   Class = "range_scan"
	FilterScan  Class = "filter_scan" // predicate on an unindexed column: full table scan
	Aggregate   Class = "aggregate"
	Join        Class = "join"
	TopN        Class = "top_n" // ORDER BY an arbitrary column: sort unless indexed
	Insert      Class = "insert"
	Update      Class = "update"
	Delete      Class = "delete"
	Upsert      Class = "upsert"
	BulkInsert  Class = "bulk_insert"
	SlowCTE     Class = "slow_cte"   // CPU-only recursive CTE sized to blow the statement timeout
	CrossJoin   Class = "cross_join" // three-way cartesian product: slow on any non-trivial table
)

// Info describes a class.
type Info struct {
	Write      bool
	ExpectSlow bool // expected to hit the statement timeout
}

var classes = map[Class]Info{
	PointRead: {}, IndexLookup: {}, RangeScan: {}, FilterScan: {}, Aggregate: {}, Join: {}, TopN: {},
	Insert: {Write: true}, Update: {Write: true}, Delete: {Write: true}, Upsert: {Write: true}, BulkInsert: {Write: true},
	SlowCTE: {ExpectSlow: true}, CrossJoin: {ExpectSlow: true},
}

// Classes lists every class, sorted by name; a class's position is its stable index (see Index).
var Classes = slices.Sorted(maps.Keys(classes))

var classIndex = func() map[Class]int {
	m := make(map[Class]int, len(Classes))
	for i, c := range Classes {
		m[c] = i
	}
	return m
}()

// Index returns c's position in Classes, or -1.
func Index(c Class) int {
	if i, ok := classIndex[c]; ok {
		return i
	}
	return -1
}

// Lookup returns a class's info, and whether it exists.
func Lookup(c Class) (Info, bool) { i, ok := classes[c]; return i, ok }

// DefaultMix is a read-heavy serving mix with a trickle of queries that should time out.
func DefaultMix() map[Class]float64 {
	return map[Class]float64{
		PointRead: 35, IndexLookup: 12, RangeScan: 6, FilterScan: 3, Aggregate: 3, Join: 5, TopN: 2,
		Insert: 12, Update: 10, Delete: 2, Upsert: 6, BulkInsert: 3, SlowCTE: 0.5, CrossJoin: 0.5,
	}
}

// Options size generated statements. The JSON names are the config file's "workload" section.
type Options struct {
	SlowCTEIterations int64 `json:"slow_cte_iterations"` // default 50,000,000: beyond any sane statement timeout
	BulkMin           int   `json:"bulk_min"`            // rows per bulk insert; default 5
	BulkMax           int   `json:"bulk_max"`            // default 50
	RangeMax          int   `json:"range_max"`           // max ids in a range scan; default 200
}

// DefaultOptions are the documented defaults.
func DefaultOptions() Options {
	return Options{SlowCTEIterations: 50_000_000, BulkMin: 5, BulkMax: 50, RangeMax: 200}
}

// Validate reports options that cannot generate statements.
func (o Options) Validate() error {
	if o.SlowCTEIterations < 1 || o.BulkMin < 1 || o.BulkMax < o.BulkMin || o.RangeMax < 1 {
		return fmt.Errorf("need slow_cte_iterations >= 1, 1 <= bulk_min <= bulk_max, range_max >= 1; got %+v", o)
	}
	return nil
}

// Statement is one generated request.
type Statement struct {
	Class      Class // the class actually made (a class the schema cannot support falls back; see Next)
	Table      string
	SQL        string
	Args       []any
	Write      bool
	ExpectSlow bool
}

// likePatterns are the filter_scan arguments for text columns: a digit anywhere in the value.
var likePatterns = []string{"%0%", "%1%", "%2%", "%3%", "%4%", "%5%", "%6%", "%7%", "%8%", "%9%"}

const slowCTESQL = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < ?) SELECT COUNT(*) FROM c"

// colSQL is a column and a statement built for it.
type colSQL struct {
	col schema.Column
	sql string
}

// tablePlan is every statement for one table, built once.
type tablePlan struct {
	t                 *schema.Table
	pointRead, rangeS string
	deleteS, upsert   string
	crossJoin, insert string
	topN              []string // one per sortable column, or ORDER BY id
	update            []colSQL
	indexLookup       []colSQL
	filterScan        []colSQL
	aggregate         []string
	join              string // non-empty if the table has a parent
	bulk              map[int]string
}

func newTablePlan(s *schema.Schema, t *schema.Table) tablePlan {
	n := t.Name
	p := tablePlan{
		t:         t,
		pointRead: fmt.Sprintf("SELECT * FROM %s WHERE id = ?", n),
		rangeS:    fmt.Sprintf("SELECT * FROM %s WHERE id BETWEEN ? AND ?", n),
		deleteS:   fmt.Sprintf("DELETE FROM %s WHERE id = ?", n),
		crossJoin: fmt.Sprintf("SELECT COUNT(*) FROM %s a, %s b, %s c WHERE a.id + b.id + c.id > ?", n, n, n),
		insert:    t.InsertSQL(1),
		bulk:      map[int]string{},
	}
	assert.That(len(t.Columns) >= 1, "newTablePlan: table %s has no columns (schema options require min_columns >= 1)", n)
	names := append([]string{"id"}, t.ColumnNames()...)
	sets := make([]string, 0, len(names))
	for _, c := range names[1:] {
		sets = append(sets, c+" = excluded."+c)
	}
	p.upsert = fmt.Sprintf("INSERT INTO %s (%s) VALUES %s ON CONFLICT (id) DO UPDATE SET %s",
		n, strings.Join(names, ", "), schema.Placeholders(len(names)), strings.Join(sets, ", "))

	var groups, nums []schema.Column
	for _, c := range t.Columns {
		p.update = append(p.update, colSQL{c, fmt.Sprintf("UPDATE %s SET %s = ? WHERE id = ?", n, c.Name)})
		if c.Indexed {
			p.indexLookup = append(p.indexLookup, colSQL{c, fmt.Sprintf("SELECT * FROM %s WHERE %s = ? LIMIT 50", n, c.Name)})
		}
		if c.Type == schema.Blob {
			continue
		}
		p.topN = append(p.topN, fmt.Sprintf("SELECT * FROM %s ORDER BY %s DESC LIMIT 20", n, c.Name))
		if !c.Indexed {
			op := "LIKE"
			if c.Numeric() {
				op = ">"
			}
			p.filterScan = append(p.filterScan, colSQL{c, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s %s ?", n, c.Name, op)})
		}
		if c.Cardinality <= 1000 {
			groups = append(groups, c)
		}
		if c.Numeric() {
			nums = append(nums, c)
		}
	}
	if len(p.topN) == 0 {
		p.topN = []string{fmt.Sprintf("SELECT * FROM %s ORDER BY id DESC LIMIT 20", n)}
	}
	for _, g := range groups {
		if len(nums) == 0 {
			p.aggregate = append(p.aggregate, fmt.Sprintf("SELECT %s, COUNT(*) AS n FROM %s GROUP BY %s ORDER BY n DESC LIMIT 20", g.Name, n, g.Name))
		}
		for _, v := range nums {
			p.aggregate = append(p.aggregate, fmt.Sprintf(
				"SELECT %s, COUNT(*) AS n, AVG(%s), MAX(%s) FROM %s GROUP BY %s ORDER BY n DESC LIMIT 20", g.Name, v.Name, v.Name, n, g.Name))
		}
	}
	if len(p.aggregate) == 0 {
		p.aggregate = []string{fmt.Sprintf("SELECT COUNT(*), MAX(id) FROM %s", n)}
	}
	if t.HasParent() {
		p.join = fmt.Sprintf("SELECT c.*, p.id AS parent FROM %s c JOIN %s p ON p.id = c.parent_id WHERE c.parent_id = ? LIMIT 100",
			n, s.Tables[t.Parent].Name)
	}
	return p
}

// Generator produces statements for one database's schema. Not safe for concurrent use.
//
// Foreign keys are enforced (SQL G-13), so the generator keeps every statement valid under them: deletes go only
// to leaf tables (no other table references them), so a parent row is never deleted, and parent_id values are
// drawn from the parent's seed rows, which therefore always exist. (nextID would over-count when inserts fail
// or are shed, and a dangling parent_id would fail the insert.)
//
//	Invariant: nextID[i] >= 1 for every table i; len(leaves) >= 1 (the last table is never a parent).
type Generator struct {
	opts     Options
	plans    []tablePlan
	nextID   []int64 // per table: one past the largest id we believe exists
	children []int   // tables with a parent
	leaves   []int   // tables no other table references: the only delete targets
}

// NewGenerator precomputes every statement for s.
//
//	pre: opts.Validate() == nil
func NewGenerator(s *schema.Schema, opts Options) *Generator {
	assert.That(opts.Validate() == nil, "NewGenerator: invalid options %+v", opts)
	g := &Generator{opts: opts, nextID: make([]int64, len(s.Tables))}
	referenced := make([]bool, len(s.Tables))
	for i := range s.Tables {
		t := &s.Tables[i]
		g.plans = append(g.plans, newTablePlan(s, t))
		g.nextID[i] = int64(t.SeedRows) + 1
		if t.HasParent() {
			g.children = append(g.children, i)
			referenced[t.Parent] = true
		}
	}
	for i, ref := range referenced {
		if !ref {
			g.leaves = append(g.leaves, i)
		}
	}
	assert.That(len(g.leaves) >= 1, "NewGenerator: no leaf table (parents must be earlier tables)")
	return g
}

// id draws an id that probably exists in table ti.
func (g *Generator) id(r *rand.Rand, ti int) int64 { return 1 + r.Int64N(max(g.nextID[ti]-1, 1)) }

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

// parentRows bounds t's parent_id: the parent's seed rows, which are never deleted (see Generator).
func (g *Generator) parentRows(t *schema.Table) int64 {
	if !t.HasParent() {
		return 0
	}
	return int64(g.plans[t.Parent].t.SeedRows)
}

// Next makes one statement of class c, against a random table (a delete: a random leaf table). A class the schema
// cannot support falls back: index_lookup without an indexed column → point_read; filter_scan without an
// unindexed scalar column → range_scan; join without a child table → index_lookup. Statement.Class reports the
// class actually made.
//
//	pre: c is a known class
func (g *Generator) Next(r *rand.Rand, c Class) Statement {
	info, ok := classes[c]
	assert.That(ok, "Next: unknown class %q", c)
	ti := r.IntN(len(g.plans))
	p := &g.plans[ti]
	st := Statement{Class: c, Table: p.t.Name, Write: info.Write, ExpectSlow: info.ExpectSlow}
	switch c {
	case PointRead:
		st.SQL, st.Args = p.pointRead, []any{g.id(r, ti)}
	case IndexLookup:
		if len(p.indexLookup) == 0 {
			return g.Next(r, PointRead)
		}
		x := pick(r, p.indexLookup)
		st.SQL, st.Args = x.sql, []any{schema.Value(r, x.col)}
	case RangeScan:
		lo := g.id(r, ti)
		st.SQL, st.Args = p.rangeS, []any{lo, lo + int64(1+r.IntN(g.opts.RangeMax))}
	case FilterScan:
		if len(p.filterScan) == 0 {
			return g.Next(r, RangeScan)
		}
		x := pick(r, p.filterScan)
		st.SQL = x.sql
		if x.col.Numeric() {
			st.Args = []any{schema.Value(r, x.col)}
		} else {
			st.Args = []any{likePatterns[r.IntN(len(likePatterns))]}
		}
	case Aggregate:
		st.SQL = pick(r, p.aggregate)
	case Join:
		if len(g.children) == 0 {
			return g.Next(r, IndexLookup)
		}
		ci := pick(r, g.children)
		cp := &g.plans[ci]
		st.Table, st.SQL, st.Args = cp.t.Name, cp.join, []any{g.id(r, cp.t.Parent)}
	case TopN:
		st.SQL = pick(r, p.topN)
	case Insert:
		st.SQL, st.Args = p.insert, p.t.AppendRow(nil, r, g.parentRows(p.t))
		g.nextID[ti]++
	case BulkInsert:
		n := g.opts.BulkMin + r.IntN(g.opts.BulkMax-g.opts.BulkMin+1)
		if p.bulk[n] == "" {
			p.bulk[n] = p.t.InsertSQL(n)
		}
		st.SQL = p.bulk[n]
		st.Args = make([]any, 0, n*(len(p.t.Columns)+1))
		for range n {
			st.Args = p.t.AppendRow(st.Args, r, g.parentRows(p.t))
		}
		g.nextID[ti] += int64(n)
	case Update:
		x := pick(r, p.update)
		st.SQL, st.Args = x.sql, []any{schema.Value(r, x.col), g.id(r, ti)}
	case Delete: // leaf tables only, so no foreign key ever blocks it
		li := pick(r, g.leaves)
		lp := &g.plans[li]
		st.Table, st.SQL, st.Args = lp.t.Name, lp.deleteS, []any{g.id(r, li)}
	case Upsert:
		st.SQL, st.Args = p.upsert, p.t.AppendRow([]any{g.id(r, ti)}, r, g.parentRows(p.t))
	case SlowCTE:
		st.SQL, st.Args = slowCTESQL, []any{g.opts.SlowCTEIterations}
	case CrossJoin:
		st.SQL, st.Args = p.crossJoin, []any{int64(-1)}
	}
	return st
}
