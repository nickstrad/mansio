package docexec_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// FuzzDocExecModel (plan §6.3): bytes become a script of requests (selects, gets, inserts with and without
// client ids, patches and deletes with and without expect, asserts, $refs, duplicate request ids, wrong
// fingerprints, injected statement failures, groups of mutations submitted together so they share a
// group-commit batch). Each runs on a real actor and on a pure model (tables of documents in maps). After every
// request the answers, error tags and fields, and the whole state (every row, every _version, the request-id
// table) must agree. The invariants the comparison enforces:
//
//   - a failed mutation leaves no trace: no rows, no versions, no request-id row;
//   - _version goes up by exactly one per applied patch;
//   - a duplicate request id answers the first result and changes nothing, whatever its payload;
//   - preconditions are checked before any write of the request is visible (a failed expect rolls back the
//     request's earlier writes, and no other request ever sees them).
func FuzzDocExecModel(f *testing.F) {
	for _, seed := range [][]byte{
		{},
		{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		[]byte("insert patch delete assert select get ref dup fail group"),
		{1, 1, 2, 2, 3, 3, 0, 0, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11},
		{5, 0, 3, 1, 1, 2, 0, 0, 1, 1, 5, 0, 3, 1, 1, 2, 0, 0, 1, 1, 9, 9, 9, 9},
	} {
		f.Add(seed)
	}
	f.Fuzz(runModel)
}

// TestDocExecModelRandom runs seeded random scripts in every mode (layout, plain actor or durable host, read
// pool), so plain go test covers what the fuzzer explores.
func TestDocExecModelRandom(t *testing.T) {
	n := 64
	if testing.Short() {
		n = 8
	}
	r := rand.New(rand.NewPCG(1, 2))
	for i := range n {
		data := make([]byte, 64+r.IntN(400))
		for j := range data {
			data[j] = byte(r.Uint32())
		}
		data[0] = byte(i % 8)
		t.Run(strconv.Itoa(i), func(t *testing.T) { runModel(t, data) })
	}
}

// runModel is one script: data[0] is the mode (bit 0 json layout, bit 1 durable host, bit 2 read pool).
func runModel(t *testing.T, data []byte) {
	{
		var mode byte
		if len(data) > 0 {
			mode, data = data[0], data[1:]
		}
		layout := "columns"
		if mode&1 == 1 {
			layout = "json"
		}
		cfg := actor.Config{}
		if mode&4 != 0 {
			cfg.ReadConns = 2 // reads on the read pool, writes on the writer
		}
		s := mustDecode(t, strings.ReplaceAll(modelSchema, `"columns"`, `"`+layout+`"`))
		durable := mode&2 != 0
		var d *db
		if durable {
			d = openDurable(t, s, cfg)
		} else {
			d = openDB(t, s, cfg)
		}
		m := newModel(s.Fingerprint)
		published := m.clone() // durable mode: the state of the last published generation
		g := &gen{src: data, m: m}
		for reqNo := 1; !g.done() && reqNo <= 40; reqNo++ {
			d.at(int64(reqNo) * 1000)
			if durable { // passivation, write-back and crashes between requests (plan §6.3)
				switch g.byte() % 8 {
				case 0: // write back: publish what is there
					if _, err := d.h.WriteBack(context.Background(), durableKey); err != nil {
						t.Fatalf("write-back: %v", err)
					}
					published = m.clone()
				case 1: // passivate: publish and close; the next request reactivates from the store
					if err := d.h.Passivate(context.Background(), durableKey); err != nil {
						t.Fatalf("passivate: %v", err)
					}
					published = m.clone()
				case 2: // crash: everything since the last publish is lost, request ids included
					d.crash()
					*m = *published.clone()
				}
			}
			group := 1
			if g.byte()%7 == 0 {
				group = 2 + int(g.byte()%2)
			}
			script := make([]step, group)
			for i := range script {
				script[i] = g.request(reqNo*10+i, s.Fingerprint, group > 1)
			}
			reals := d.runGroup(script)
			for i, st := range script {
				want := m.apply(st, d.clock.Now().UnixMilli(), reqNo*10+i)
				compareOutcome(t, st, reals[i], want)
			}
			compareState(t, d, m)
		}
	}
}

// modelSchema: one table of every scalar kind the script uses, and a second table.
const modelSchema = `{"notes":{"fields":{"title":{"node":{"kind":"string"}},"n":{"node":{"kind":"integer"}},` +
	`"tag":{"node":{"kind":"enum","values":["a","b"],"nullable":true}}},"storage":"columns","indexes":[]},` +
	`"lists":{"fields":{"name":{"node":{"kind":"string"}}},"storage":"columns","indexes":[]}}`

// step is one request of the script.
type step struct {
	body    []byte
	req     *ir.Request
	failAt  int // inject a failure at this statement (1-based); 0 = none
	seqBase int // server ids are "s<seqBase>-<k>"
}

// runGroup runs a group of requests: one alone, or several mutations submitted before any is awaited, so the
// actor runs them in one batch, in order, each in its own savepoint.
func (d *db) runGroup(script []step) []outcome {
	a := d.actor()
	reqs := make([]*actor.Request, len(script))
	for i, st := range script {
		at := st.failAt
		call := &docexec.Call{Req: st.req, Schemas: d.cache, IDs: &prefixIDs{prefix: "s" + strconv.Itoa(st.seqBase) + "-"}}
		if at > 0 {
			call.Wrap = func(tx docexec.Tx) docexec.Tx { return &failingTx{Tx: tx, at: at, err: errInjected} }
		}
		reqs[i] = call.Request()
		if err := a.Submit(reqs[i]); err != nil {
			d.t.Fatal(err)
		}
	}
	out := make([]outcome, len(script))
	for i, r := range reqs {
		res := r.Wait(context.Background())
		var f *docexec.Failure
		switch {
		case errors.As(res.Err, &f):
			out[i] = outcome{err: f.Err}
		case res.Err != nil:
			d.t.Fatalf("request %d: unexpected %v", i, res.Err)
		default:
			v, _ := ir.Parse(string(res.Body))
			out[i] = outcome{result: v, duplicate: res.Duplicate}
		}
	}
	return out
}

var errInjected = errors.New("injected statement failure")

type prefixIDs struct {
	prefix string
	n      int
}

func (p *prefixIDs) NewID() string { p.n++; return p.prefix + strconv.Itoa(p.n) }

// compareOutcome compares the actor's answer with the model's: the result and duplicate flag byte for byte
// (canonical JSON), or the error's tag, op and the fields the model states.
func compareOutcome(t *testing.T, st step, real outcome, want modelOutcome) {
	t.Helper()
	name := string(st.body)
	switch {
	case (real.err == nil) != (want.err == nil):
		t.Fatalf("%s:\n real %s\nmodel %s", name, describe(real.err, real.result), describe(want.err, want.result))
	case real.err != nil:
		if real.err.Tag != want.err.Tag || real.err.HasOp != want.err.HasOp || real.err.Op != want.err.Op {
			t.Fatalf("%s:\n real %s\nmodel %s", name, ir.Stringify(real.err.Wire()), ir.Stringify(want.err.Wire()))
		}
		for _, k := range want.err.Fields.Keys() {
			if !ir.Equal(real.err.Field(k), want.err.Field(k)) {
				t.Fatalf("%s: fields.%s\n real %s\nmodel %s", name, k, ir.Stringify(real.err.Wire()), ir.Stringify(want.err.Wire()))
			}
		}
	default:
		if got, w := ir.Canonical(real.result), ir.Canonical(want.result); got != w || real.duplicate != want.duplicate {
			t.Fatalf("%s:\n real %s dup=%v\nmodel %s dup=%v", name, got, real.duplicate, w, want.duplicate)
		}
	}
}

func describe(e *ir.Error, v any) string {
	if e != nil {
		return ir.Stringify(e.Wire())
	}
	return ir.Stringify(v)
}

// compareState compares every row and the request-id table with the model.
func compareState(t *testing.T, d *db, m *model) {
	t.Helper()
	got := d.dump()
	for _, table := range []string{"notes", "lists"} {
		if g, w := ir.Canonical(got[table]), ir.Canonical(m.rows(table)); g != w {
			t.Fatalf("table %s:\n real %s\nmodel %s", table, g, w)
		}
	}
	var ids []string
	for _, r := range d.requests() {
		o := r.(*ir.Object)
		ids = append(ids, o.Value("request_id").(string)+"@"+ir.Stringify(o.Value("applied_ms"))+"="+o.Value("result_hex").(string))
	}
	if g, w := strings.Join(ids, "\n"), m.requestRows(); g != w {
		t.Fatalf("request ids:\n real %s\nmodel %s", g, w)
	}
}

// gen turns bytes into requests. It reads the model (the ids that exist) so scripts hit real documents.
type gen struct {
	src     []byte
	pos     int
	m       *model
	clients int
}

func (g *gen) done() bool { return g.pos >= len(g.src) }

func (g *gen) byte() byte {
	if g.done() {
		return 0
	}
	b := g.src[g.pos]
	g.pos++
	return b
}

func (g *gen) pick(options ...string) string { return options[int(g.byte())%len(options)] }

// clientID is a canonical UUIDv7: a fresh one, or one already used (a conflict).
func (g *gen) clientID() string {
	if g.clients > 0 && g.byte()%3 == 0 {
		return uuidOf(1 + int(g.byte())%g.clients)
	}
	g.clients++
	return uuidOf(g.clients)
}

func uuidOf(n int) string { return fmt.Sprintf("01900000-0000-7000-8000-%012x", n) }

// request builds one request. In a group every request is a mutation (reads run after a batch's writes, which
// a sequential model would not see).
func (g *gen) request(seqBase int, fp string, mutationOnly bool) step {
	kind := "mutation"
	if !mutationOnly && g.byte()%4 == 0 {
		kind = "query"
	}
	if g.byte() == 255 {
		fp = "sha256:" + strings.Repeat("0", 64)
	}
	var ops []string
	var inserts []int // op indexes of inserts, for $refs
	for i, n := 0, 1+int(g.byte()%4); i < n; i++ {
		op := g.op(kind, inserts)
		if strings.HasPrefix(op, `{"op":"insert"`) {
			inserts = append(inserts, i)
		}
		ops = append(ops, op)
	}
	rid := ""
	if kind == "mutation" {
		rid = `,"requestId":"r` + strconv.Itoa(int(g.byte()%10)) + `"`
	}
	body := []byte(`{"v":1,"db":{"tenant":"a","app":"b"},"kind":"` + kind + `"` + rid + `,"schema":"` + fp + `","ops":[` + strings.Join(ops, ",") + `]}`)
	req, e := ir.DecodeBody(body)
	if e != nil {
		panic("model script built an undecodable request: " + e.Message() + ": " + string(body))
	}
	failAt := 0
	if b := g.byte(); b%5 == 0 {
		failAt = 1 + int(b/5)%6
	}
	return step{body: body, req: req, failAt: failAt, seqBase: seqBase}
}

// id is an id position: an existing or deleted document's id, a $ref to an earlier insert, or a missing id.
func (g *gen) id(inserts []int) string {
	switch b := g.byte(); {
	case len(inserts) > 0 && b%4 == 0:
		return `{"$ref":` + strconv.Itoa(inserts[int(g.byte())%len(inserts)]) + `}`
	case len(g.m.seen) > 0 && b%4 != 3:
		return strconv.Quote(g.m.seen[int(g.byte())%len(g.m.seen)])
	}
	return `"missing"`
}

// value is a scalar for field, sometimes invalid for it, sometimes a $ref.
func (g *gen) value(field string, inserts []int) string {
	if field == "title" || field == "name" {
		if len(inserts) > 0 && g.byte()%5 == 0 {
			return `{"$ref":` + strconv.Itoa(inserts[0]) + `}`
		}
		return g.pick(`"a"`, `"b"`, `"c"`)
	}
	if field == "n" {
		return g.pick("0", "1", "2", "3", "1.5")
	}
	return g.pick("null", `"a"`, `"b"`, `"z"`)
}

func (g *gen) table() string { return g.pick("notes", "notes", "notes", "lists", "nope") }

func (g *gen) body(table string, partial bool, inserts []int) string {
	fields := []string{"title", "n", "tag"}
	if table == "lists" {
		fields = []string{"name"}
	}
	var members []string
	for _, f := range fields {
		if partial && g.byte()%2 == 0 {
			continue
		}
		members = append(members, `"`+f+`":`+g.value(f, inserts))
	}
	if g.byte()%11 == 0 {
		members = append(members, `"extra":1`) // an unknown field
	}
	return "{" + strings.Join(members, ",") + "}"
}

func (g *gen) expect(patch bool, inserts []int) string {
	switch g.byte() % 6 {
	case 0:
		return `,"expect":{"version":` + g.pick("1", "2", "3") + `}`
	case 1:
		if patch {
			return `,"expect":{"exists":true}`
		}
		return `,"expect":{"exists":` + g.pick("true", "false") + `}`
	case 2:
		f := g.pick("title", "title", "n", "zz", "_version")
		v := g.value(f, inserts)
		if f == "zz" || f == "_version" {
			v = "1"
		}
		return `,"expect":{"fields":{"` + f + `":` + v + `}}`
	case 3:
		return `,"expect":{"version":` + g.pick("1", "2") + `,"fields":{"title":` + g.value("title", inserts) + `}}`
	}
	return ""
}

func (g *gen) op(kind string, inserts []int) string {
	which := int(g.byte())
	if kind == "query" {
		which %= 2
	} else {
		which %= 8
	}
	table := g.table()
	switch which {
	case 0:
		return g.selectOp(table, inserts)
	case 1:
		return `{"op":"get","table":"` + table + `","id":` + g.id(inserts) + `}`
	case 2, 3:
		id := ""
		if which == 3 {
			id = `,"id":"` + g.clientID() + `"`
		}
		return `{"op":"insert","table":"` + table + `","value":` + g.body(table, false, inserts) + id + `}`
	case 4, 5:
		return `{"op":"patch","table":"` + table + `","id":` + g.id(inserts) + `,"value":` + g.body(table, true, inserts) + g.expect(true, inserts) + `}`
	case 6:
		return `{"op":"delete","table":"` + table + `","id":` + g.id(inserts) + g.expect(false, inserts) + `}`
	}
	e := g.expect(false, inserts)
	if e == "" {
		e = `,"expect":{"exists":true}`
	}
	return `{"op":"assert","table":"` + table + `","id":` + g.id(inserts) + e + `}`
}

func (g *gen) selectOp(table string, inserts []int) string {
	var filters string
	switch g.byte() % 4 {
	case 1:
		filters = `{"field":"n","op":"eq","value":` + g.pick("0", "1", "2") + `}`
	case 2:
		filters = `{"field":"_id","op":"eq","value":` + g.id(inserts) + `}`
	case 3:
		filters = `{"field":"n","op":"gt","value":` + g.pick("0", "1") + `}`
	}
	if table == "lists" && filters != "" && !strings.Contains(filters, "_id") {
		filters = "" // lists has no n
	}
	order := g.pick("", "", `,"order":{"field":"n","direction":"asc"}`, `,"order":{"field":"n","direction":"desc"}`)
	if table == "lists" {
		order = ""
	}
	agg := g.pick("", "", `,"aggregate":{"kind":"count"}`)
	return `{"op":"select","table":"` + table + `","filters":[` + filters + `],"limit":` + g.pick("1", "2", "5") + order + agg + `}`
}

// model is the pure reference: per table, documents by id; the applied request ids and their stored results.
type model struct {
	fp      string // the database's fingerprint
	stmts   int    // statements issued by the request running now
	tables  map[string]map[string]map[string]any
	applied map[string]appliedReq
	seen    []string // every id ever answered by an insert (deleted ones stay: they make misses)
}

type appliedReq struct {
	ms   int64
	body string
}

type modelOutcome struct {
	result      any
	duplicate   bool
	err         *ir.Error
	storageOnly bool // the injected failure is reached: err is the StorageError the actor answers
}

var modelFields = map[string][]string{"notes": {"title", "n", "tag"}, "lists": {"name"}}

func newModel(fp string) *model {
	return &model{fp: fp, tables: map[string]map[string]map[string]any{"notes": {}, "lists": {}}, applied: map[string]appliedReq{}}
}

// apply runs st on the model (on a copy, kept only if the request succeeds) and returns its outcome. The model
// counts the statements docexec issues (one per select, get and load; one per write), so it knows where an
// injected failure lands: at statement failAt, if the request gets that far, it fails as a StorageError of the
// operation issuing it, and changes nothing.
func (m *model) apply(st step, now int64, seqBase int) modelOutcome {
	req := st.req
	if req.Schema != m.fp {
		return modelOutcome{err: ir.NewError(ir.TagSchemaDrift, "expected", m.fp, "actual", req.Schema)}
	}
	if req.Kind == ir.Mutation {
		if prior, ok := m.applied[req.RequestID]; ok {
			v, _ := ir.Parse(prior.body)
			return modelOutcome{result: v, duplicate: true}
		}
	}
	work := m.clone()
	work.applied = m.applied // read only in run
	result, e, upTo := work.run(req, now, seqBase)
	for i, n := range upTo {
		if st.failAt > 0 && st.failAt <= n {
			return modelOutcome{storageOnly: true, err: ir.NewError(ir.TagStorage).WithOp(i)}
		}
	}
	if e != nil {
		return modelOutcome{err: e}
	}
	if req.Kind == ir.Mutation {
		m.tables, m.seen = work.tables, work.seen
		m.applied[req.RequestID] = appliedReq{ms: now, body: ir.Canonical(result)}
	}
	return modelOutcome{result: result}
}

func (m *model) clone() *model {
	c := &model{fp: m.fp, tables: map[string]map[string]map[string]any{}, applied: maps.Clone(m.applied), seen: slices.Clone(m.seen)}
	for name, docs := range m.tables {
		c.tables[name] = map[string]map[string]any{}
		for id, doc := range docs {
			cp := map[string]any{}
			for k, v := range doc {
				cp[k] = v
			}
			c.tables[name][id] = cp
		}
	}
	return c
}

// run executes req's operations; upTo[i] is how many statements had been issued once operation i ended (or
// failed).
func (m *model) run(req *ir.Request, now int64, seqBase int) (result []any, err *ir.Error, upTo []int) {
	m.stmts = 0
	refs := make([]string, len(req.Ops))
	draws := 0
	for i, op := range req.Ops {
		v, e := m.step(op, refs, now, func() string { draws++; return fmt.Sprintf("s%d-%d", seqBase, draws) })
		upTo = append(upTo, m.stmts)
		if e != nil {
			return nil, e.WithOp(i), upTo
		}
		if id, ok := v.(string); ok && op.Name() == "insert" {
			refs[i] = id
		}
		result = append(result, v)
	}
	return result, nil, upTo
}

func resolve(v any, refs []string) any { return ir.Resolve(v, refs) }

func (m *model) step(op ir.Op, refs []string, now int64, draw func() string) (any, *ir.Error) {
	fields, ok := modelFields[op.TableName()]
	if !ok {
		return nil, ir.NewError(ir.TagCompile, "reason", "unknown-collection", "table", op.TableName())
	}
	docs := m.tables[op.TableName()]
	switch op := op.(type) {
	case *ir.Select:
		m.stmts++
		return m.selectOp(op, docs, refs)
	case *ir.Get:
		m.stmts++
		return docOrNil(docs[ir.ResolveID(op.ID, refs)]), nil
	case *ir.Insert:
		body, e := validBody(fields, resolve(op.Value, refs).(*ir.Object), false)
		if e != nil {
			return nil, e
		}
		id := op.ID
		if id != "" {
			m.stmts++
			if docs[id] != nil {
				return nil, ir.NewError(ir.TagConflict, "table", op.Table, "id", id, "check", "exists", "expected", false, "actual", true)
			}
		} else {
			id = draw()
		}
		m.stmts++
		body["_id"], body["_creationTime"], body["_version"] = id, float64(now), 1.0
		docs[id] = body
		m.seen = append(m.seen, id)
		return id, nil
	case *ir.Patch:
		id := ir.ResolveID(op.ID, refs)
		patch, e := validBody(fields, resolve(op.Value, refs).(*ir.Object), true)
		if e != nil {
			return nil, e
		}
		m.stmts++
		doc := docs[id]
		if e := modelExpect(fields, op.Table, id, op.Expect, refs, doc); e != nil {
			return nil, e
		}
		if doc == nil {
			return nil, ir.NewError(ir.TagNotFound, "table", op.Table, "id", id)
		}
		m.stmts++
		for k, v := range patch {
			doc[k] = v
		}
		doc["_version"] = doc["_version"].(float64) + 1
		return ir.NewObject("_version", doc["_version"]), nil
	case *ir.Delete:
		id := ir.ResolveID(op.ID, refs)
		m.stmts++
		doc := docs[id]
		if e := modelExpect(fields, op.Table, id, op.Expect, refs, doc); e != nil {
			return nil, e
		}
		if doc != nil {
			m.stmts++
			delete(docs, id)
		}
		return ir.NewObject("deleted", doc != nil), nil
	case *ir.Assert:
		id := ir.ResolveID(op.ID, refs)
		m.stmts++
		if e := modelExpect(fields, op.Table, id, op.Expect, refs, docs[id]); e != nil {
			return nil, e
		}
		return nil, nil
	}
	panic("unknown op")
}

// validBody is the model's body rule for the script's fields: unknown members first, then each declared
// field in order (title/name: a string; n: a safe integer; tag: null, "a" or "b").
func validBody(fields []string, in *ir.Object, partial bool) (map[string]any, *ir.Error) {
	for _, k := range in.Keys() {
		if !slices.Contains(fields, k) {
			return nil, ir.NewError(ir.TagSchema, "reason", ir.ReasonUnknownField, "path", k)
		}
	}
	out := map[string]any{}
	for _, f := range fields {
		v, ok := in.Get(f)
		if !ok {
			if partial {
				continue
			}
			return nil, ir.NewError(ir.TagSchema, "reason", ir.ReasonTypeMismatch, "path", f)
		}
		if !validValue(f, v) {
			return nil, ir.NewError(ir.TagSchema, "reason", ir.ReasonTypeMismatch, "path", f)
		}
		out[f] = v
	}
	return out, nil
}

func validValue(field string, v any) bool {
	switch field {
	case "title", "name":
		_, ok := v.(string)
		return ok
	case "n":
		return ir.IsSafeInteger(v)
	}
	return v == nil || v == "a" || v == "b"
}

func modelExpect(fields []string, table, id string, e *ir.Expect, refs []string, doc map[string]any) *ir.Error {
	if e == nil {
		return nil
	}
	var want *ir.Object
	if e.Fields != nil {
		want = resolve(e.Fields, refs).(*ir.Object)
		for _, k := range want.Keys() {
			if !slices.Contains(fields, k) {
				return ir.NewError(ir.TagCompile, "reason", "unknown-field", "field", k, "table", table)
			}
			if !validValue(k, want.Value(k)) {
				return ir.NewError(ir.TagSchema, "reason", ir.ReasonTypeMismatch, "path", k)
			}
		}
	}
	conflict := func(check string, expected, actual any) *ir.Error {
		return ir.NewError(ir.TagConflict, "table", table, "id", id, "check", check, "expected", expected, "actual", actual)
	}
	mustExist := e.Version != nil || e.Fields != nil
	decided := mustExist
	if e.Exists != nil {
		mustExist, decided = *e.Exists, true
	}
	switch {
	case decided && mustExist && doc == nil:
		return conflict("exists", true, false)
	case decided && !mustExist && doc != nil:
		return conflict("exists", false, true)
	case doc == nil:
		return nil
	}
	if e.Version != nil && doc["_version"] != float64(*e.Version) {
		return conflict("version", float64(*e.Version), doc["_version"])
	}
	if want != nil {
		actual := ir.NewObject()
		differs := false
		for _, k := range want.Keys() {
			actual.Set(k, doc[k])
			differs = differs || doc[k] != want.Value(k)
		}
		if differs {
			return conflict("fields", want, actual)
		}
	}
	return nil
}

func docOrNil(doc map[string]any) any {
	if doc == nil {
		return nil
	}
	o := ir.NewObject()
	for k, v := range doc {
		o.Set(k, v)
	}
	return o
}

func (m *model) selectOp(op *ir.Select, docs map[string]map[string]any, refs []string) (any, *ir.Error) {
	var rows []map[string]any
	for _, doc := range docs {
		keep := true
		for _, f := range op.Filters {
			v := resolve(f.Value, refs)
			switch f.Op {
			case "eq":
				keep = keep && doc[f.Field] == v
			case "gt":
				keep = keep && doc[f.Field].(float64) > v.(float64)
			}
		}
		if keep {
			rows = append(rows, doc)
		}
	}
	if op.Aggregate != nil {
		return float64(len(rows)), nil
	}
	slices.SortFunc(rows, func(a, b map[string]any) int {
		if op.Order != nil {
			c := cmp.Compare(a[op.Order.Field].(float64), b[op.Order.Field].(float64))
			if op.Order.Direction == "desc" {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return strings.Compare(a["_id"].(string), b["_id"].(string))
	})
	out := []any{}
	for i, doc := range rows {
		if i == int(op.Limit) {
			break
		}
		out = append(out, docOrNil(doc))
	}
	return out, nil
}

// rows is a table's documents ordered by _id.
func (m *model) rows(table string) []any {
	ids := make([]string, 0, len(m.tables[table]))
	for id := range m.tables[table] {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := []any{}
	for _, id := range ids {
		out = append(out, docOrNil(m.tables[table][id]))
	}
	return out
}

// requestRows is the expected _mansio_requests table, as compareState renders the real one.
func (m *model) requestRows() string {
	ids := make([]string, 0, len(m.applied))
	for id := range m.applied {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var lines []string
	for _, id := range ids {
		a := m.applied[id]
		lines = append(lines, id+"@"+strconv.FormatInt(a.ms, 10)+"="+fmt.Sprintf("02%x", a.body))
	}
	return strings.Join(lines, "\n")
}
