package doccompile_test

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
	"testing/quick"

	_ "modernc.org/sqlite"

	"github.com/nickstrad/mansio/server/internal/doccompile"
	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
	"github.com/nickstrad/mansio/server/internal/sqlitedsn"
)

// propSchema has one field of every scalar kind (nullable and not) plus both array shapes.
const propSchema = `{"t":{"fields":{
 "title":{"node":{"kind":"string"}},
 "owner":{"node":{"kind":"string","nullable":true}},
 "score":{"node":{"kind":"number"}},
 "w":{"node":{"kind":"number","nullable":true}},
 "rank":{"node":{"kind":"integer"}},
 "n":{"node":{"kind":"integer","nullable":true}},
 "done":{"node":{"kind":"boolean"}},
 "tag":{"node":{"kind":"enum","values":["a","b","c"],"nullable":true}},
 "tags":{"node":{"kind":"array","item":{"kind":"string"}}},
 "grid":{"node":{"kind":"array","item":{"kind":"integer","nullable":true},"length":3}}
},"storage":"columns","indexes":[{"name":"by_rank","fields":["rank","title"]}]}}`

var scalarFields = []string{"title", "owner", "score", "w", "rank", "n", "done", "tag", "_id", "_creationTime", "_version"}

func mustSchema(t testing.TB, text string) *docschema.Schema {
	t.Helper()
	v, err := ir.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	s, derr := docschema.Decode(v)
	if derr != nil {
		t.Fatal(derr)
	}
	return s
}

// gen draws document values from small domains, so filters hit and orderings tie.
type gen struct{ r *rand.Rand }

func (g gen) pick(n int) int { return g.r.IntN(n) }

var (
	texts   = []any{"", "a", "b", "B", "ab", "é", "😀", "O'Reilly", `"q"`, "z\x01"}
	numbers = []any{-1.5, 0.0, 0.5, 2.0, 3.0, 1e21, 9007199254740992.0, 1693240866440448768.0, -1e-7}
	ints    = []any{0.0, 1.0, 2.0, -3.0, float64(ir.MaxSafeInteger), -float64(ir.MaxSafeInteger)}
)

func (g gen) from(list []any, nullable bool) any {
	if nullable && g.pick(4) == 0 {
		return nil
	}
	return list[g.pick(len(list))]
}

func (g gen) value(field string) any {
	switch field {
	case "title", "_id":
		return g.from(texts, false)
	case "owner":
		return g.from(texts, true)
	case "score":
		return g.from(numbers, false)
	case "w":
		return g.from(numbers, true)
	case "rank", "_creationTime":
		return g.from(ints, false)
	case "n":
		return g.from(ints, true)
	case "_version":
		return float64(1 + g.pick(3))
	case "done":
		return g.pick(2) == 0
	case "tag":
		return g.from([]any{"a", "b", "c"}, true)
	case "tags":
		out := []any{}
		for i := g.pick(3); i > 0; i-- {
			out = append(out, g.from(texts, false))
		}
		return out
	}
	return []any{g.from(ints, true), g.from(ints, true), g.from(ints, true)} // grid
}

func (g gen) doc(i int) *ir.Object {
	d := ir.NewObject()
	for _, f := range []string{"title", "owner", "score", "w", "rank", "n", "done", "tag", "tags", "grid"} {
		d.Set(f, g.value(f))
	}
	d.Set("_id", fmt.Sprintf("d%03d", i))
	d.Set("_creationTime", float64(1000+g.pick(3)))
	d.Set("_version", 1.0)
	return d
}

func body(d *ir.Object) *ir.Object {
	out := ir.NewObject()
	for _, k := range d.Keys() {
		if !strings.HasPrefix(k, "_") {
			out.Set(k, d.Value(k))
		}
	}
	return out
}

// database is an in-memory SQLite database holding s's DDL and docs, written through doccompile.Insert.
func database(t testing.TB, s *docschema.Schema, docs []*ir.Object) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqlitedsn.Build(":memory:"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, derr := doccompile.DDL(s)
	if derr != nil {
		t.Fatal(derr)
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for _, d := range docs {
		ins, ierr := doccompile.Insert(s, "t", body(d), d.Value("_id").(string), d.Value("_creationTime").(float64))
		if ierr != nil {
			t.Fatal(ierr)
		}
		if _, err := db.Exec(ins.Text, doccompile.Args(ins.Values)...); err != nil {
			t.Fatalf("%s: %v", ins.Text, err)
		}
	}
	return db
}

func query(t testing.TB, db *sql.DB, q doccompile.SQL) []doccompile.Row {
	t.Helper()
	rows, err := db.Query(q.Text, doccompile.Args(q.Values)...)
	if err != nil {
		t.Fatalf("%s: %v", q.Text, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []doccompile.Row
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := doccompile.Row{}
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func run(t testing.TB, s *docschema.Schema, db *sql.DB, p doccompile.Plan) any {
	t.Helper()
	q, err := doccompile.Select(s, p, doccompile.SQLite)
	if err != nil {
		t.Fatalf("compile %+v: %v", p, err)
	}
	got, derr := doccompile.Decode(s, p, query(t, db, q))
	if derr != nil {
		t.Fatalf("decode %s: %v", q.Text, derr)
	}
	return got
}

func layouts(t *testing.T) map[string]*docschema.Schema {
	s := mustSchema(t, propSchema)
	return map[string]*docschema.Schema{docschema.Columns: s, docschema.JSON: s.WithStorage(docschema.JSON)}
}

// TestDecodeInvertsEncode: a valid document written by Insert reads back, through Select and Decode,
// as exactly the same document, in both layouts (spec §6.4; plan §6.2 "decode is the inverse of encode").
func TestDecodeInvertsEncode(t *testing.T) {
	for name, s := range layouts(t) {
		t.Run(name, func(t *testing.T) {
			prop := func(seed uint64) bool {
				g := gen{rand.New(rand.NewPCG(seed, 1))}
				docs := make([]*ir.Object, 1+g.pick(8))
				for i := range docs {
					docs[i] = g.doc(i)
				}
				got := run(t, s, database(t, s, docs), doccompile.Plan{Table: "t", Limit: 1000.0})
				want := make([]any, len(docs))
				for i, d := range docs {
					want[i] = d
				}
				if !ir.Equal(got, want) {
					t.Logf("seed %d:\n got %s\nwant %s", seed, ir.Stringify(got), ir.Stringify(want))
					return false
				}
				return true
			}
			if err := quick.Check(prop, &quick.Config{MaxCount: 200}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// compare orders two field values as SQLite does for the compiled expression: NULL lowest, numbers
// (booleans as 0/1) numerically, text by bytes.
func compare(a, b any) int {
	num := func(v any) (float64, bool) {
		switch x := v.(type) {
		case float64:
			return x, true
		case bool:
			if x {
				return 1, true
			}
			return 0, true
		}
		return 0, false
	}
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	if x, ok := num(a); ok {
		y, _ := num(b)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	return strings.Compare(a.(string), b.(string))
}

// reference evaluates a plan over docs as spec §5.1 says it means.
func reference(docs []*ir.Object, p doccompile.Plan) any {
	var hits []*ir.Object
	for _, d := range docs {
		ok := true
		for _, f := range p.Filters {
			v := d.Value(f.Field)
			switch {
			case f.Op == "eq" && f.Value == nil:
				ok = ok && v == nil
			case f.Op == "eq":
				ok = ok && v != nil && compare(v, f.Value) == 0
			default:
				ok = ok && v != nil && compare(v, f.Value) > 0
			}
		}
		if ok {
			hits = append(hits, d)
		}
	}
	limit := int(p.Limit.(float64))
	if p.Aggregate != nil && p.Aggregate.Kind == "count" {
		return float64(len(hits))
	}
	if p.Aggregate != nil {
		var keys []any
		counts := map[string]float64{}
		for _, d := range hits {
			v := d.Value(p.Aggregate.Field)
			k := ir.Stringify(v)
			if _, seen := counts[k]; !seen {
				keys = append(keys, v)
			}
			counts[k]++
		}
		sort.SliceStable(keys, func(i, j int) bool { return compare(keys[i], keys[j]) < 0 })
		groups := []any{}
		for i := 0; i < len(keys) && i < limit; i++ {
			groups = append(groups, ir.NewObject("value", keys[i], "count", counts[ir.Stringify(keys[i])]))
		}
		return groups
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if p.Order != nil {
			c := compare(a.Value(p.Order.Field), b.Value(p.Order.Field))
			if p.Order.Direction == "desc" {
				// DESC NULLS LAST: reversing the ascending comparison puts NULL (the lowest) last.
				c = -c
			}
			if c != 0 {
				return c < 0
			}
		}
		return a.Value("_id").(string) < b.Value("_id").(string)
	})
	out := []any{}
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i])
	}
	return out
}

func (g gen) plan(docs []*ir.Object) doccompile.Plan {
	numeric := []string{"score", "w", "rank", "n", "_creationTime", "_version"}
	p := doccompile.Plan{Table: "t", Limit: float64(1 + g.pick(12))}
	for i := g.pick(3); i > 0; i-- {
		if g.pick(3) == 0 {
			field := numeric[g.pick(len(numeric))]
			v := g.value(field)
			if v == nil {
				v = 0.0
			}
			p.Filters = append(p.Filters, ir.Filter{Field: field, Op: "gt", Value: v})
			continue
		}
		field := scalarFields[g.pick(len(scalarFields))]
		v := g.value(field)
		if len(docs) > 0 && g.pick(2) == 0 {
			v = docs[g.pick(len(docs))].Value(field) // a value some document holds
		}
		p.Filters = append(p.Filters, ir.Filter{Field: field, Op: "eq", Value: v})
	}
	switch g.pick(4) {
	case 0:
		p.Aggregate = &ir.Aggregate{Kind: "count"}
	case 1:
		p.Aggregate = &ir.Aggregate{Kind: "countBy", Field: scalarFields[g.pick(8)]}
	}
	if g.pick(3) > 0 {
		p.Order = &ir.Order{Field: scalarFields[g.pick(len(scalarFields))], Direction: []string{"asc", "desc"}[g.pick(2)]}
	}
	return p
}

// TestSelectMatchesReference: a compiled select's rows, run on modernc SQLite and decoded, equal a small
// reference evaluator over the same documents: filters, ordering with NULLs first ascending / last
// descending and the _id tie-break, limit, count (which ignores limit and order) and countBy grouping.
func TestSelectMatchesReference(t *testing.T) {
	for name, s := range layouts(t) {
		t.Run(name, func(t *testing.T) {
			prop := func(seed uint64) bool {
				g := gen{rand.New(rand.NewPCG(seed, 2))}
				docs := make([]*ir.Object, g.pick(15))
				for i := range docs {
					docs[i] = g.doc(i)
				}
				db := database(t, s, docs)
				for k := 0; k < 10; k++ {
					p := g.plan(docs)
					got, want := run(t, s, db, p), reference(docs, p)
					if !ir.Equal(got, want) {
						q, _ := doccompile.Select(s, p, doccompile.SQLite)
						t.Logf("seed %d: %s %s\n got %s\nwant %s", seed, q.Text, ir.Stringify(q.Values), ir.Stringify(got), ir.Stringify(want))
						return false
					}
				}
				return true
			}
			if err := quick.Check(prop, &quick.Config{MaxCount: 200}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArgs(t *testing.T) {
	got := doccompile.Args([]any{1.0, 1.5, float64(ir.MaxSafeInteger), float64(ir.MaxSafeInteger) + 1, "x", nil, math.Inf(1)})
	want := []any{int64(1), 1.5, int64(ir.MaxSafeInteger), float64(ir.MaxSafeInteger) + 1, "x", nil, math.Inf(1)}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Args[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}
