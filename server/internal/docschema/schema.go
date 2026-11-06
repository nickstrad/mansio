package docschema

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/nickstrad/mansio/server/internal/ir"
)

// Node kinds.
const (
	KindString  = "string"
	KindNumber  = "number"
	KindInteger = "integer"
	KindBoolean = "boolean"
	KindEnum    = "enum"
	KindArray   = "array"
)

// Storage layouts.
const (
	Columns = "columns"
	JSON    = "json"
)

// Node is a value descriptor (core/schema.ts Node). Load builds nodes that ignore the declaration rules
// (an array without Item, an empty enum, an odd Length), and every function here is total over them.
type Node struct {
	Kind     string
	Nullable bool
	// Values are the enum members; HasValues distinguishes an absent list from an empty one (the DDL
	// treats them differently).
	Values    []string
	HasValues bool
	Item      *Node
	// Length is the fixed array length; nil when absent. A length that is not a number is NaN, which no
	// array length equals.
	Length *float64
	// raw is the node's JSON as registered (member order, nullable: false and all), which the fingerprint
	// text repeats verbatim. Decode and Load always set it.
	raw *ir.Object
}

// Field is a declared field.
type Field struct {
	Name string
	Node *Node
}

// Index is a declared secondary index.
type Index struct {
	Name   string
	Fields []string
}

// Table is a table descriptor.
type Table struct {
	Fields  []Field // in declaration order
	Storage string  // Columns or JSON (Load keeps any other text, which the compiler treats as TS does)
	Indexes []Index
	byName  map[string]*Node
}

// Field returns the declared field's node.
func (t *Table) Field(name string) (*Node, bool) {
	n, ok := t.byName[name]
	return n, ok
}

func newTable(fields []Field) *Table {
	t := &Table{Fields: fields, Storage: Columns, byName: make(map[string]*Node, len(fields))}
	for _, f := range fields {
		t.byName[f.Name] = f.Node
	}
	return t
}

// Schema is the tables of one database, in declaration order.
type Schema struct {
	names  []string
	tables map[string]*Table
}

// Names returns the table names in order. The caller must not modify the slice.
func (s *Schema) Names() []string { return s.names }

// Table returns the named table.
func (s *Schema) Table(name string) (*Table, bool) {
	t, ok := s.tables[name]
	return t, ok
}

func (s *Schema) add(name string, t *Table) {
	if s.tables == nil {
		s.tables = map[string]*Table{}
	}
	if _, ok := s.tables[name]; !ok {
		s.names = append(s.names, name)
	}
	s.tables[name] = t
}

// WithStorage reprojects every table onto one layout (core/schema.ts withStorage).
func (s *Schema) WithStorage(storage string) *Schema {
	out := &Schema{}
	for _, name := range s.names {
		t := *s.tables[name]
		t.Storage = storage
		out.add(name, &t)
	}
	return out
}

var identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Identifier accepts the SQL identifiers this repo interpolates into generated SQL.
func Identifier(name string) *ir.Error {
	if !identifierRE.MatchString(name) {
		return ir.SchemaErr("invalid-identifier", name, "identifier", name)
	}
	return nil
}

// NUL is U+0000, the one character the text storage contract refuses (ADR 0005).
const NUL = "\x00"

// defineTable applies defineTable's rules: identifier names, no _ prefix, no enum member holding U+0000.
func defineTable(fields []Field) (*Table, *ir.Error) {
	for _, f := range fields {
		if err := Identifier(f.Name); err != nil {
			return nil, err
		}
		if strings.HasPrefix(f.Name, "_") {
			return nil, ir.SchemaErr("reserved-field", f.Name, "unreserved name", ir.Undefined{})
		}
		n := f.Node
		for n != nil && n.Kind == KindArray {
			n = n.Item
		}
		if n != nil {
			for _, v := range n.Values {
				if strings.Contains(v, NUL) {
					return nil, ir.SchemaErr(ir.ReasonTypeMismatch, f.Name, "enum values without U+0000", v)
				}
			}
		}
	}
	return newTable(fields), nil
}

// addIndex applies Table.addIndex's rules and returns the table with the index appended.
func (t *Table) addIndex(name string, fields []string) (*Table, *ir.Error) {
	if err := Identifier(name); err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, ir.SchemaErr("empty-index", name, "one or more fields", ir.Undefined{})
	}
	for _, f := range fields {
		if n, ok := t.byName[f]; !ok || n.Kind == KindArray {
			return nil, ir.SchemaErr("unindexable-field", f, "scalar field", ir.Undefined{})
		}
	}
	for _, idx := range t.Indexes {
		if idx.Name == name {
			return nil, ir.SchemaErr("duplicate-index", name, "unused index name", ir.Undefined{})
		}
	}
	out := *t
	out.Indexes = append(append([]Index(nil), t.Indexes...), Index{Name: name, Fields: fields})
	return &out, nil
}

// defineSchema applies defineSchema's table-name rules: identifiers, not _-prefixed, not sqlite_-prefixed
// in any case.
func defineSchema(s *Schema) *ir.Error {
	for _, name := range s.names {
		if err := Identifier(name); err != nil {
			return err
		}
		if strings.HasPrefix(name, "_") || strings.HasPrefix(strings.ToLower(name), "sqlite_") {
			return ir.SchemaErr("reserved-table", name, "unreserved name", ir.Undefined{})
		}
	}
	return nil
}

// JSON is the registered form: JSON.stringify of the core/schema.ts schema. Tables and fields keep their
// order, each table's members are fields, storage, indexes (the Table class's), and each node is the
// registered node object verbatim.
func (s *Schema) JSON() *ir.Object {
	out := ir.NewObject()
	for _, name := range s.names {
		t := s.tables[name]
		fields := ir.NewObject()
		for _, f := range t.Fields {
			fields.Set(f.Name, ir.NewObject("node", f.Node.raw))
		}
		indexes := make([]any, len(t.Indexes))
		for i, idx := range t.Indexes {
			names := make([]any, len(idx.Fields))
			for j, f := range idx.Fields {
				names[j] = f
			}
			indexes[i] = ir.NewObject("name", idx.Name, "fields", names)
		}
		out.Set(name, ir.NewObject("fields", fields, "storage", t.Storage, "indexes", indexes))
	}
	return out
}

// FingerprintText is what _platform_schema.definition stores and the request's schema hashes (spec §2).
func (s *Schema) FingerprintText() string { return ir.Stringify(s.JSON()) }

// Fingerprint is sha256: + the lowercase hex SHA-256 of text's UTF-8 bytes.
func Fingerprint(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}
