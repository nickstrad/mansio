// Package schema generates a random but reproducible schema for each database and the seed data for it.
//
// Every database gets its own tables, column names and types, indexes and parent/child links, derived from
// (run seed, database index), so a schema can be regenerated instead of stored. The package is pure: it builds
// SQL text and argument values; callers execute them.
package schema

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"

	"github.com/nickstrad/ts-db-go/internal/assert"
)

// Type is a SQLite column type.
type Type string

const (
	Integer Type = "INTEGER"
	Real    Type = "REAL"
	Text    Type = "TEXT"
	Blob    Type = "BLOB"
)

// Column is one non-key column.
type Column struct {
	Name        string
	Type        Type
	Cardinality int64 // distinct values the generator draws from (low ⇒ good GROUP BY, high ⇒ selective lookups)
	Indexed     bool
	NotNull     bool
}

// Numeric reports whether comparisons and AVG make sense.
func (c Column) Numeric() bool { return c.Type == Integer || c.Type == Real }

// Table has an INTEGER PRIMARY KEY id, columns, and optionally a parent_id pointing at another table's id.
type Table struct {
	Name     string
	Columns  []Column
	Parent   int // index of the parent table in Schema.Tables, or -1
	SeedRows int
}

// HasParent reports whether the table has a parent_id column.
func (t *Table) HasParent() bool { return t.Parent >= 0 }

// Schema is one database's tables.
//
//	Invariant: every Table.Parent is -1 or the index of an earlier table.
type Schema struct {
	DB     int
	Tables []Table
}

// Options bound the random shape. The JSON names are the config file's "schema" section.
type Options struct {
	MinTables  int     `json:"min_tables"`
	MaxTables  int     `json:"max_tables"`
	MinColumns int     `json:"min_columns"`
	MaxColumns int     `json:"max_columns"`
	MinRows    int     `json:"min_rows"`
	MaxRows    int     `json:"max_rows"`
	IndexProb  float64 `json:"index_prob"`  // chance a column gets a secondary index
	ParentProb float64 `json:"parent_prob"` // chance a table (after the first) gets a parent_id
}

// DefaultOptions are small enough to seed thousands of databases quickly.
func DefaultOptions() Options {
	return Options{MinTables: 1, MaxTables: 5, MinColumns: 2, MaxColumns: 8, MinRows: 50, MaxRows: 500, IndexProb: 0.3, ParentProb: 0.5}
}

// Validate reports options that cannot generate a schema.
func (o Options) Validate() error {
	switch {
	case o.MinTables < 1 || o.MaxTables < o.MinTables:
		return fmt.Errorf("tables: need 1 <= min_tables <= max_tables, got %d..%d", o.MinTables, o.MaxTables)
	case o.MinColumns < 1 || o.MaxColumns < o.MinColumns: // a root table with no columns has nothing to insert
		return fmt.Errorf("columns: need 1 <= min_columns <= max_columns, got %d..%d", o.MinColumns, o.MaxColumns)
	case o.MinRows < 0 || o.MaxRows < o.MinRows:
		return fmt.Errorf("rows: need 0 <= min_rows <= max_rows, got %d..%d", o.MinRows, o.MaxRows)
	case o.IndexProb < 0 || o.IndexProb > 1 || o.ParentProb < 0 || o.ParentProb > 1:
		return fmt.Errorf("index_prob and parent_prob must be in [0, 1], got %g and %g", o.IndexProb, o.ParentProb)
	}
	return nil
}

var nouns = []string{
	"orders", "users", "items", "events", "invoices", "games", "moves", "players", "sessions", "notes",
	"tasks", "projects", "comments", "payments", "devices", "readings", "accounts", "messages", "files", "tags",
}

var attrs = []string{
	"status", "score", "amount", "price", "kind", "region", "label", "title", "count", "level",
	"rating", "weight", "color", "owner", "source", "priority", "body", "token", "flag", "total",
}

// rng is the deterministic generator for database db under run seed, for one purpose (schema or data).
func rng(seed uint64, db int, purpose uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed^purpose, uint64(db)*0x9E3779B97F4A7C15+1))
}

const (
	purposeSchema = 0
	purposeData   = 0xA5A5A5A5
)

// Generate builds database db's schema. Same (seed, db, opts) ⇒ same schema.
//
//	pre: opts.Validate() == nil
func Generate(seed uint64, db int, opts Options) *Schema {
	assert.That(opts.Validate() == nil, "Generate: invalid options %+v", opts)
	r := rng(seed, db, purposeSchema)
	between := func(lo, hi int) int { return lo + r.IntN(hi-lo+1) }

	s := &Schema{DB: db}
	used := map[string]bool{}
	for t := range between(opts.MinTables, opts.MaxTables) {
		name := nouns[r.IntN(len(nouns))]
		for used[name] {
			name = fmt.Sprintf("%s_%d", nouns[r.IntN(len(nouns))], t)
		}
		used[name] = true
		tb := Table{Name: name, Parent: -1, SeedRows: between(opts.MinRows, opts.MaxRows)}
		if t > 0 && r.Float64() < opts.ParentProb {
			tb.Parent = r.IntN(t)
		}
		assert.That(tb.Parent < t, "Generate: table %d has parent %d (must be earlier)", t, tb.Parent)
		cols := map[string]bool{"id": true, "parent_id": true}
		for c := range between(opts.MinColumns, opts.MaxColumns) {
			cn := attrs[r.IntN(len(attrs))]
			for cols[cn] {
				cn = fmt.Sprintf("%s_%d", attrs[r.IntN(len(attrs))], c)
			}
			cols[cn] = true
			col := Column{
				Name:        cn,
				Type:        []Type{Integer, Integer, Real, Text, Text, Blob}[r.IntN(6)],
				Cardinality: []int64{5, 20, 1000, 1_000_000}[r.IntN(4)],
				NotNull:     r.IntN(2) == 0,
			}
			col.Indexed = col.Type != Blob && r.Float64() < opts.IndexProb
			tb.Columns = append(tb.Columns, col)
		}
		s.Tables = append(s.Tables, tb)
	}
	return s
}

// DDL is the CREATE TABLE / CREATE INDEX script.
func (s *Schema) DDL() string {
	var b strings.Builder
	for _, t := range s.Tables {
		fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n  id INTEGER PRIMARY KEY", t.Name)
		if t.HasParent() {
			fmt.Fprintf(&b, ",\n  parent_id INTEGER REFERENCES %s(id)", s.Tables[t.Parent].Name)
		}
		for _, c := range t.Columns {
			fmt.Fprintf(&b, ",\n  %s %s", c.Name, c.Type)
			if c.NotNull {
				b.WriteString(" NOT NULL")
			}
		}
		b.WriteString("\n);\n")
		if t.HasParent() {
			fmt.Fprintf(&b, "CREATE INDEX IF NOT EXISTS %s_parent_id ON %s(parent_id);\n", t.Name, t.Name)
		}
		for _, c := range t.Columns {
			if c.Indexed {
				fmt.Fprintf(&b, "CREATE INDEX IF NOT EXISTS %s_%s ON %s(%s);\n", t.Name, c.Name, t.Name, c.Name)
			}
		}
	}
	return b.String()
}

// Value draws a value for column c.
func Value(r *rand.Rand, c Column) any {
	n := r.Int64N(c.Cardinality)
	switch c.Type {
	case Integer:
		return n
	case Real:
		return float64(n) + r.Float64()
	case Text:
		prefix := attrs[n%int64(len(attrs))]
		return string(strconv.AppendInt(append([]byte(prefix), '-'), n, 10))
	default:
		b := make([]byte, 16+r.IntN(240))
		for i := 0; i < len(b); i += 8 {
			v := r.Uint64()
			for j := i; j < min(i+8, len(b)); j++ {
				b[j] = byte(v)
				v >>= 8
			}
		}
		return b
	}
}

// ColumnNames is the insertable column list: parent_id (if any), then the columns.
func (t *Table) ColumnNames() []string {
	names := make([]string, 0, len(t.Columns)+1)
	if t.HasParent() {
		names = append(names, "parent_id")
	}
	for _, c := range t.Columns {
		names = append(names, c.Name)
	}
	return names
}

// Placeholders is "(?, ?, ...)" with n marks.
func Placeholders(n int) string {
	return "(" + strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + ")"
}

// InsertSQL is "INSERT INTO t (parent_id?, cols...) VALUES (?, ...), ..." for rows rows.
//
//	pre: rows >= 1
func (t *Table) InsertSQL(rows int) string {
	assert.That(rows >= 1, "InsertSQL: %d rows", rows)
	names := t.ColumnNames()
	row := Placeholders(len(names))
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES %s", t.Name, strings.Join(names, ", "),
		strings.TrimSuffix(strings.Repeat(row+", ", rows), ", "))
}

// AppendRow appends one row's insert arguments to args; parentRows bounds parent_id.
func (t *Table) AppendRow(args []any, r *rand.Rand, parentRows int64) []any {
	if t.HasParent() {
		args = append(args, 1+r.Int64N(max(parentRows, 1)))
	}
	for _, c := range t.Columns {
		args = append(args, Value(r, c))
	}
	return args
}

// Exec runs one SQL statement with arguments.
type Exec func(q string, args ...any) error

// Fill inserts every table's SeedRows rows through exec, deterministically for this database and run seed.
func (s *Schema) Fill(seed uint64, exec Exec) error {
	r := rng(seed, s.DB, purposeData)
	for i := range s.Tables {
		t := &s.Tables[i]
		var parentRows int64
		if t.HasParent() {
			parentRows = int64(s.Tables[t.Parent].SeedRows)
		}
		q := t.InsertSQL(1)
		args := make([]any, 0, len(t.Columns)+1)
		for range t.SeedRows {
			args = t.AppendRow(args[:0], r, parentRows)
			if err := exec(q, args...); err != nil {
				return fmt.Errorf("seed %s: %w", t.Name, err)
			}
		}
	}
	return nil
}

// Catalog holds every database's schema, indexed by database number. Not safe for concurrent Ensure.
type Catalog struct {
	Seed    uint64
	Options Options
	schemas []*Schema
}

func NewCatalog(seed uint64, opts Options) *Catalog { return &Catalog{Seed: seed, Options: opts} }

// Ensure generates schemas for databases 0..n-1.
func (c *Catalog) Ensure(n int) {
	for i := len(c.schemas); i < n; i++ {
		c.schemas = append(c.schemas, Generate(c.Seed, i, c.Options))
	}
}

// Get returns database db's schema.
//
//	pre: db < n of an earlier Ensure(n)
func (c *Catalog) Get(db int) *Schema { return c.schemas[db] }

// Fingerprint identifies the generator inputs and version, so a kept database directory is reused only when it
// was built the same way.
func (c *Catalog) Fingerprint() string {
	o := c.Options
	return fmt.Sprintf("v2-s%d-t%d_%d-c%d_%d-r%d_%d-i%g-p%g",
		c.Seed, o.MinTables, o.MaxTables, o.MinColumns, o.MaxColumns, o.MinRows, o.MaxRows, o.IndexProb, o.ParentProb)
}
