package docexec

import (
	"errors"
	"sync"

	"github.com/nickstrad/mansio/server/internal/doccompile"
	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// The schema record every document database holds, byte for byte as platform/sqlite.ts and
// platform/ir/executor.ts write it, so database files move between the backends unchanged.
const (
	schemaTableDDL = `CREATE TABLE IF NOT EXISTS "_platform_schema" (id INTEGER PRIMARY KEY CHECK (id = 1), definition TEXT NOT NULL) STRICT`
	schemaInsert   = `INSERT INTO "_platform_schema" VALUES (1, ?)`
	schemaSelect   = `SELECT definition, ` + changesExist + ` FROM "_platform_schema" WHERE id = 1`
	schemaExists   = `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = '_platform_schema'`
)

// ErrUninitialized: the database holds no schema record yet (a new, empty database). Nothing ran; run Init
// with the registered schema the request names, then the request again.
var ErrUninitialized = errors.New("docexec: the database has no schema yet")

// drift is the SchemaDriftError of a request whose fingerprint is not the database's (spec §6.1 step 3):
// expected is the database's fingerprint, actual the request's.
func drift(expected, actual string) *ir.Error {
	return ir.NewError(ir.TagSchemaDrift, "expected", expected, "actual", actual)
}

// Schemas caches decoded schemas by their fingerprint text, so a request costs one map lookup rather than a
// decode and a hash. Safe for concurrent use. Bounded: past max entries it starts over (a database holds one
// schema and an agent serves few apps, so a reset is rare and costs one decode per database).
type Schemas struct {
	max int

	mu sync.Mutex
	m  map[string]*Schema
}

// Schema is a decoded schema with its fingerprint.
type Schema struct {
	Schema      *docschema.Schema
	Fingerprint string
	Text        string
}

// NewSchemas returns a cache of at most max schemas (default 256 when max <= 0).
func NewSchemas(max int) *Schemas {
	if max <= 0 {
		max = 256
	}
	return &Schemas{max: max, m: map[string]*Schema{}}
}

// Get decodes text (the fingerprint text of spec §2) with the declaration rules, from the cache when it can.
// A text that is not a valid schema is an *ir.Error (SchemaError or CompileError: its DDL must compile).
func (c *Schemas) Get(text string) (*Schema, *ir.Error) {
	c.mu.Lock()
	s, ok := c.m[text]
	c.mu.Unlock()
	if ok {
		return s, nil
	}
	s, e := Decode(text)
	if e != nil {
		return nil, e
	}
	c.mu.Lock()
	if len(c.m) >= c.max {
		clear(c.m)
	}
	c.m[text] = s
	c.mu.Unlock()
	return s, nil
}

// Decode decodes a registered schema text (spec §2): strict JSON, the declaration rules, a DDL that compiles.
// The result's Text is the fingerprint text, which equals text only when text is in fingerprint form
// (CanonicalText checks that).
func Decode(text string) (*Schema, *ir.Error) {
	v, e := ir.Parse(text)
	if e != nil {
		return nil, e
	}
	s, e := docschema.Decode(v)
	if e != nil {
		return nil, e
	}
	if _, e := doccompile.DDL(s); e != nil {
		return nil, e
	}
	ft := s.FingerprintText()
	return &Schema{Schema: s, Fingerprint: docschema.Fingerprint(ft), Text: ft}, nil
}

// CanonicalText reports why text cannot be registered: it is not a valid schema, or it is not its own
// fingerprint text (whitespace, or members the decoder reorders), so a stored text always opens as
// platform/sqlite.ts writes it. nil means text is a valid schema in fingerprint form. It is the schema
// registry's Validate hook (controld on register, agentd on fetch).
func CanonicalText(text string) error {
	s, e := Decode(text)
	if e != nil {
		return protocol(e)
	}
	if s.Text != text {
		return protocol(ir.SchemaErr(ir.ReasonTypeMismatch, "schema", "the fingerprint text (JSON.stringify of the schema)", ir.Undefined{}))
	}
	return nil
}

// State is what Check learned about a database: its schema, and whether it has the commit-sequence table yet
// (seq.go).
type State struct {
	*Schema
	changes bool
}

// Check is the fingerprint check (spec §6.1 step 3): it reads the database's schema record and answers the
// decoded schema when its fingerprint is fingerprint. It only reads.
//
//	post: err == nil ⇒ st.Fingerprint == fingerprint
//	post: errors.Is(err, ErrUninitialized): the database has no schema record
//	post: a *Failure with SchemaDriftError: the fingerprints differ
//	post: a *Failure with DecodeError: the stored text is not a valid schema (a damaged file: a server fault)
//	post: a *Failure with StorageError: a statement failed
func Check(tx Tx, cache *Schemas, fingerprint string) (State, error) {
	text, changes, ok, err := stored(tx, false)
	switch {
	case err != nil:
		return State{}, err
	case !ok:
		return State{}, ErrUninitialized
	}
	s, e := cache.Get(text)
	if e != nil {
		return State{}, protocol(ir.NewError(ir.TagDecode, "reason", "stored-schema", "detail", e.Message()))
	}
	// The stored text is the fingerprint text when this package or platform/sqlite.ts wrote it; a file whose
	// text is in another form keeps the fingerprint of its own bytes.
	actual := s.Fingerprint
	if s.Text != text {
		actual = docschema.Fingerprint(text)
	}
	if actual != fingerprint {
		return State{}, protocol(drift(actual, fingerprint))
	}
	return State{Schema: s, changes: changes}, nil
}

// stored is the database's schema text, if it has one, and whether the commit-sequence table exists. probe asks the catalog first: a write closure must
// not run a failing statement (the first statement error poisons its Tx), while a read can try the record
// and ask the catalog only when that fails (one statement on the hot path).
func stored(tx Tx, probe bool) (text string, changes, ok bool, err error) {
	exists := func() (bool, error) {
		rows, err := tx.Query(schemaExists, 1)
		if err != nil {
			return false, storage(err)
		}
		return len(rows.Values) == 1 && rows.Values[0][0] != int64(0), nil
	}
	if probe {
		if ok, err := exists(); err != nil || !ok {
			return "", false, false, err
		}
	}
	rows, err := tx.Query(schemaSelect, 1)
	if err != nil {
		// Most likely no such table (a new database): ask the catalog rather than read the error's text.
		if ok, cerr := exists(); cerr != nil || ok || probe {
			return "", false, false, storage(err)
		}
		return "", false, false, nil
	}
	if len(rows.Values) == 0 {
		return "", false, false, nil
	}
	text, ok = rows.Values[0][0].(string)
	if !ok {
		return "", false, false, protocol(ir.NewError(ir.TagDecode, "reason", "stored-schema", "detail", "definition is not text"))
	}
	return text, rows.Values[0][1] != int64(0), true, nil
}

// Init creates a new database's tables from s and records s as its schema (platform/ir/executor.ts
// openIrDatabase): the DDL, the commit-sequence table (seq.go), then _platform_schema with the fingerprint text. A database that already has a
// schema record is left alone: Init succeeds if it is s, and answers SchemaDriftError otherwise. Run it as a
// write closure (it is one transaction's worth of statements).
//
//	post: err == nil ⇒ the database's schema record is s.Text
func Init(tx Tx, s *Schema) error {
	text, _, ok, err := stored(tx, true)
	switch {
	case err != nil:
		return err
	case ok && text == s.Text:
		return nil
	case ok:
		return protocol(drift(docschema.Fingerprint(text), s.Fingerprint))
	}
	ddl, e := doccompile.DDL(s.Schema)
	if e != nil {
		return protocol(e)
	}
	for _, stmt := range append([]string{schemaTableDDL, changesTableDDL}, ddl...) {
		if _, err := tx.Exec(stmt); err != nil {
			return storage(err)
		}
	}
	if _, err := tx.Exec(schemaInsert, s.Text); err != nil {
		return storage(err)
	}
	return nil
}
