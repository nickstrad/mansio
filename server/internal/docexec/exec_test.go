package docexec_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/docexec/docactor"
	"github.com/nickstrad/mansio/server/internal/ir"
)

const notes = `{"notes":{"fields":{"title":{"node":{"kind":"string"}},"n":{"node":{"kind":"integer"}}},"storage":"columns","indexes":[]}}`

func mustDecode(t testing.TB, text string) *docexec.Schema {
	t.Helper()
	s, e := docexec.Decode(text)
	if e != nil {
		t.Fatalf("schema: %s", e.Message())
	}
	return s
}

func body(kind, id, fp, ops string) []byte {
	rid := ""
	if id != "" {
		rid = `,"requestId":"` + id + `"`
	}
	return []byte(`{"v":1,"db":{"tenant":"a","app":"b"},"kind":"` + kind + `"` + rid + `,"schema":"` + fp + `","ops":` + ops + `}`)
}

// A new database has no schema: Check answers ErrUninitialized (nothing ran), Init creates it, Check then
// passes; Init with another schema, or a request with another fingerprint, is drift with the database's
// fingerprint as expected.
func TestCheckAndInit(t *testing.T) {
	s := mustDecode(t, notes)
	other := mustDecode(t, strings.Replace(notes, `"n"`, `"m"`, 1))
	d := openDB(t, nil, actor.Config{})
	d.schema = s
	if got := d.exec(body("query", "", s.Fingerprint, `[{"op":"get","table":"notes","id":"x"}]`), &listIDs{}, nil); got.err == nil ||
		got.err.Tag != "unexpected" || !strings.Contains(ir.ToString(got.err.Field("detail")), "no schema yet") {
		t.Fatalf("uninitialized query: %+v", got)
	}
	call := &docactor.Call{Req: &ir.Request{Kind: ir.Mutation, Schema: s.Fingerprint}, Schemas: d.cache}
	if res := d.do(call.Request()); !errors.Is(res.Err, docexec.ErrUninitialized) {
		t.Fatalf("uninitialized mutation: %v", res.Err)
	}
	for range 2 { // idempotent
		if res := d.do(docactor.InitRequest(s)); res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	res := d.do(docactor.InitRequest(other))
	var f *docexec.Failure
	if !errors.As(res.Err, &f) || f.Err.Tag != ir.TagSchemaDrift || f.Err.Field("expected") != s.Fingerprint || f.Err.Field("actual") != other.Fingerprint {
		t.Fatalf("init with another schema: %v", res.Err)
	}
	got := d.exec(body("mutation", "r1", other.Fingerprint, `[{"op":"insert","table":"notes","value":{"title":"a","n":1}}]`), &listIDs{}, nil)
	if got.err == nil || got.err.Tag != ir.TagSchemaDrift || got.err.HasOp || got.err.Field("expected") != s.Fingerprint || got.err.Field("actual") != other.Fingerprint {
		t.Fatalf("drift: %+v", got)
	}
	if got := d.exec(body("query", "", s.Fingerprint, `[{"op":"get","table":"notes","id":"x"}]`), &listIDs{}, nil); got.err != nil || !ir.Equal(got.result, []any{nil}) {
		t.Fatalf("after init: %+v", got)
	}
}

// CanonicalText is the registry's Validate hook: valid schemas in fingerprint form only.
func TestCanonicalText(t *testing.T) {
	for _, tc := range []struct {
		text string
		ok   bool
	}{
		{notes, true},
		{" " + notes, false}, // whitespace: not the fingerprint text
		{`{"t":{}}`, false},  // not a schema
		{`{"t":{"fields":{},"storage":"columns","indexes":[]}}`, true},
		{`{"t":{"storage":"columns","fields":{},"indexes":[]}}`, false},                                         // members out of the fingerprint order
		{`{"_t":{"fields":{},"storage":"columns","indexes":[]}}`, false},                                        // reserved table
		{`{"t":{"fields":{"e":{"node":{"kind":"enum","values":[]}}},"storage":"columns","indexes":[]}}`, false}, // DDL fails
	} {
		if err := docexec.CanonicalText(tc.text); (err == nil) != tc.ok {
			t.Errorf("%s: %v, want ok=%v", tc.text, err, tc.ok)
		}
	}
}

func TestUUIDv7(t *testing.T) {
	clock := dettest.NewClock(time.UnixMilli(0x0123456789ab))
	g := docexec.NewUUIDv7(clock, det.NewRand(1))
	seen := map[string]bool{}
	for range 1000 {
		id := g.NewID()
		if !ir.UUIDv7.MatchString(id) || !strings.HasPrefix(id, "01234567-89ab-7") || seen[id] {
			t.Fatalf("id %q", id)
		}
		seen[id] = true
	}
	if a, b := docexec.NewUUIDv7(clock, det.NewRand(7)).NewID(), docexec.NewUUIDv7(clock, det.NewRand(7)).NewID(); a != b {
		t.Fatalf("seeded ids differ: %s %s", a, b)
	}
}

// failingTx fails statement number at (1-based) with err; earlier statements run.
type failingTx struct {
	docexec.Tx
	at, n int
	err   error
}

func (f *failingTx) fail() error {
	f.n++
	if f.n == f.at {
		return f.err
	}
	return nil
}

func (f *failingTx) Exec(sql string, args ...any) (int64, error) {
	if err := f.fail(); err != nil {
		return 0, err
	}
	return f.Tx.Exec(sql, args...)
}

func (f *failingTx) Query(sql string, maxRows int, args ...any) (docexec.Rows, error) {
	if err := f.fail(); err != nil {
		return docexec.Rows{}, err
	}
	return f.Tx.Query(sql, maxRows, args...)
}

// A failed statement is a StorageError at its operation with the statement's error as Cause, and the request
// leaves no trace: no rows, no request-id record. The request itself is never modified by execution.
func TestStatementFailureLeavesNoTrace(t *testing.T) {
	s := mustDecode(t, notes)
	d := openDB(t, s, actor.Config{})
	ops := `[{"op":"insert","table":"notes","value":{"title":"a","n":1}},{"op":"select","table":"notes","filters":[{"field":"_id","op":"eq","value":{"$ref":0}}],"limit":5},` +
		`{"op":"patch","table":"notes","id":{"$ref":0},"value":{"n":2},"expect":{"fields":{"title":"a"}}}]`
	req, e := ir.DecodeBody(body("mutation", "r1", s.Fingerprint, ops))
	if e != nil {
		t.Fatal(e.Message())
	}
	before := ir.Canonical(req.JSON())
	boom := errors.New("boom")
	for at := 1; at <= 7; at++ { // insert, select, load, update, then the sequence's read and write: 6 statements; 7 runs clean
		call := &docactor.Call{Req: req, Schemas: d.cache, IDs: &listIDs{ids: []string{"s1"}},
			Wrap: func(tx docexec.Tx) docexec.Tx { return &failingTx{Tx: tx, at: at, err: boom} }}
		res := d.do(call.Request())
		var f *docexec.Failure
		switch {
		case at == 7:
			if res.Err != nil || string(res.Body) != `["s1",[{"_creationTime":0,"_id":"s1","_version":1,"n":1,"title":"a"}],{"_version":2}]` {
				t.Fatalf("clean run: %v %s", res.Err, res.Body)
			}
		case !errors.As(res.Err, &f) || !errors.Is(res.Err, boom) || f.Err.Tag != ir.TagStorage || f.Err.HasOp != (at <= 4):
			t.Fatalf("statement %d: %v", at, res.Err)
		default:
			if wantOp := map[int]int{1: 0, 2: 1, 3: 2, 4: 2}[at]; f.Err.Op != wantOp { // the sequence belongs to no operation
				t.Errorf("statement %d: op %d, want %d", at, f.Err.Op, wantOp)
			}
			if rows := d.dump()["notes"].([]any); len(rows) != 0 {
				t.Fatalf("statement %d: rows left %s", at, ir.Stringify(rows))
			}
			if reqs := d.requests(); len(reqs) != 0 {
				t.Fatalf("statement %d: request ids left %s", at, ir.Stringify(reqs))
			}
		}
		if after := ir.Canonical(req.JSON()); after != before {
			t.Fatalf("the request changed:\n%s\n%s", before, after)
		}
	}
}

// A database file from before the commit sequence has no _mansio_changes table: a query answers sequence 0
// without reading it, and the first mutation creates it and counts from there (spec §6.5).
func TestSequenceOnAFileWithoutTheTable(t *testing.T) {
	s := mustDecode(t, notes)
	d := openDB(t, s, actor.Config{})
	if res := d.do(&actor.Request{Write: true, SQL: `DROP TABLE "_mansio_changes"`}); res.Err != nil {
		t.Fatal(res.Err)
	}
	query := body("query", "", s.Fingerprint, `[{"op":"select","table":"notes","filters":[],"limit":5,"aggregate":{"kind":"count"}}]`)
	if got := d.exec(query, &listIDs{}, nil); got.err != nil || got.seq != 0 {
		t.Fatalf("query before any mutation: %+v", got)
	}
	insert := `[{"op":"insert","table":"notes","value":{"title":"a","n":1}}]`
	for i, id := range []string{"s1", "s2"} {
		got := d.exec(body("mutation", "r"+id, s.Fingerprint, insert), &listIDs{ids: []string{id}}, nil)
		if got.err != nil || got.seq != int64(i+1) {
			t.Fatalf("mutation %d: %+v", i+1, got)
		}
	}
	if got := d.exec(query, &listIDs{}, nil); got.err != nil || got.seq != 2 || !ir.Equal(got.result, []any{2.0}) {
		t.Fatalf("query after two mutations: %+v", got)
	}
}

// A conflict's current state is bounded (spec §8.1): entries past MaxCurrentBytes are left out, and
// currentTruncated tells the client that a precondition missing from the list may fail too.
func TestConflictCurrentIsBounded(t *testing.T) {
	s := mustDecode(t, notes)
	d := openDB(t, s, actor.Config{})
	big := strings.Repeat("x", 30_000)
	for _, id := range []string{"s1", "s2", "s3"} {
		ops := `[{"op":"insert","table":"notes","value":{"title":"` + big + `","n":1}}]`
		if got := d.exec(body("mutation", "r"+id, s.Fingerprint, ops), &listIDs{ids: []string{id}}, nil); got.err != nil {
			t.Fatalf("insert %s: %s", id, got.err.Message()[:80])
		}
	}
	stale := func(ids ...string) []byte {
		var ops []string
		for _, id := range ids {
			ops = append(ops, `{"op":"assert","table":"notes","id":"`+id+`","expect":{"version":9}}`)
		}
		return body("query", "", s.Fingerprint, "["+strings.Join(ops, ",")+"]")
	}
	got := d.exec(stale("s1", "s2"), &listIDs{}, nil)
	if got.err == nil || got.err.Tag != ir.TagConflict || len(got.err.Field("current").([]any)) != 2 || got.err.Field("currentTruncated") != (ir.Undefined{}) {
		t.Fatalf("two documents fit: %v", got.err.Fields.Keys())
	}
	got = d.exec(stale("s1", "s2", "s3"), &listIDs{}, nil)
	if got.err == nil || got.err.Field("seq") != 3.0 || len(got.err.Field("current").([]any)) != 2 || got.err.Field("currentTruncated") != true {
		t.Fatalf("the third does not: %v", got.err.Fields.Keys())
	}
}
