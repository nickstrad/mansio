package front_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/front/fronttest"
	"github.com/nickstrad/mansio/server/internal/ir"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/ticket/tickettest"
)

// notesSchema is a registered schema in fingerprint form.
const notesSchema = `{"notes":{"fields":{"title":{"node":{"kind":"string"}},"n":{"node":{"kind":"integer"}}},"storage":"columns","indexes":[]}}`

var notesFP = mustSchema(notesSchema).Fingerprint

func mustSchema(text string) *docexec.Schema {
	s, e := docexec.Decode(text)
	if e != nil {
		panic(e.Message())
	}
	return s
}

// schemaSource is a fake registry: (app, fingerprint) → text, with a fault switch.
type schemaSource struct {
	mu    sync.Mutex
	texts map[[2]string]string
	err   error
	calls int
}

func (s *schemaSource) add(app, text string) string {
	sc := mustSchema(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.texts[[2]string{app, sc.Fingerprint}] = text
	return sc.Fingerprint
}

func (s *schemaSource) Schema(_ context.Context, app, fp string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	text, ok := s.texts[[2]string{app, fp}]
	if !ok {
		return "", front.ErrSchemaUnknown
	}
	return text, nil
}

// seqIDs are server ids s1, s2, …
type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (g *seqIDs) NewID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("s%d", g.n)
}

// opsRig is the whole /v1/ops path on real actors: tickets, admission, StaticHost, ActorExecutor, HTTP.
type opsRig struct {
	s       *front.Server
	srv     *httptest.Server
	host    *front.StaticHost
	adm     *fronttest.Admitter
	iss     tickettest.Issuer
	schemas *schemaSource
	log     *syncBuf
	dir     string
}

func newOpsRig(t testing.TB, cfg front.Config, dbs ...string) *opsRig {
	t.Helper()
	return newObservedOpsRig(t, cfg, nil, dbs...)
}

// newObservedOpsRig is newOpsRig with an OpsObserver.
func newObservedOpsRig(t testing.TB, cfg front.Config, observe front.OpsObserver, dbs ...string) *opsRig {
	t.Helper()
	iss := tickettest.NewIssuer("k1")
	r := &opsRig{iss: iss, schemas: &schemaSource{texts: map[[2]string]string{}}, dir: t.TempDir(),
		adm: fronttest.NewAdmitter(map[string]uint64{}), host: front.NewStaticHost(nil), log: &syncBuf{}}
	for _, db := range dbs {
		r.addDB(t, db)
	}
	v := ticket.Verifier{Keys: ticket.NewKeyRing(iss.Keys()), VM: vm, Skew: 5 * time.Second, MaxTTL: time.Hour}
	// The system clock: the actors run on it, and the executor times its wait on theirs.
	r.s = front.NewServer(cfg, v, r.adm, front.ActorExecutor{Host: r.host}, nil)
	r.s.EnableOps(front.OpsConfig{Schemas: r.schemas, IDs: &seqIDs{}, Observer: observe})
	r.srv = httptest.NewServer(front.NewHandler(r.s, logx.New(r.log, slog.LevelDebug)))
	t.Cleanup(r.srv.Close)
	t.Cleanup(r.host.Close)
	return r
}

// addDB serves a new, empty database db (tenant:app) here at epoch 1.
func (r *opsRig) addDB(t testing.TB, db string) *actor.Actor {
	a := actor.New(0, filepath.Join(r.dir, db+".db"), actor.Config{})
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	r.host.Add(db, a)
	r.adm.Set(db, 1)
	return a
}

func (r *opsRig) tok(t testing.TB, db string, perm ticket.Perm) string {
	c := r.iss.Claims(db, vm, time.Now(), 59*time.Minute)
	c.Perms = perm
	return r.iss.Mint(t, c)
}

// opsReply is one HTTP answer.
type opsReply struct {
	status int
	header http.Header
	body   *ir.Object
	raw    string
}

func (r *opsRig) post(t testing.TB, auth string, body []byte, hdr map[string]string) opsReply {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+front.OpsPath, bytes.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return opsReply{status: res.StatusCode, header: res.Header, body: parseBody(t, b), raw: string(b)}
}

// parseBody parses an answer. It is strict JSON except that an error path may echo a lone surrogate the
// client sent (as JSON.stringify does), which encoding/json reads as U+FFFD.
func parseBody(t testing.TB, b []byte) *ir.Object {
	t.Helper()
	if v, e := ir.Parse(string(b)); e == nil {
		if o, ok := v.(*ir.Object); ok {
			return o
		}
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("body is not JSON: %q", b)
	}
	o, ok := fromJSON(v).(*ir.Object)
	if !ok {
		t.Fatalf("body is not an object: %q", b)
	}
	return o
}

func fromJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		o := ir.NewObject()
		for k, item := range x {
			o.Set(k, fromJSON(item))
		}
		return o
	case []any:
		for i := range x {
			x[i] = fromJSON(x[i])
		}
	}
	return v
}

func (o opsReply) tag() string {
	if e, ok := o.body.Value("error").(*ir.Object); ok {
		s, _ := e.Value("tag").(string)
		return s
	}
	return ""
}

func (o opsReply) field(name string) any {
	e, _ := o.body.Value("error").(*ir.Object)
	if e == nil {
		return ir.Undefined{}
	}
	f, _ := e.Value("fields").(*ir.Object)
	if f == nil {
		return ir.Undefined{}
	}
	return f.Value(name)
}

func request(tenant, app, kind, requestID, fp, ops string) []byte {
	rid := ""
	if requestID != "" {
		rid = `,"requestId":"` + requestID + `"`
	}
	return []byte(`{"v":1,"db":{"tenant":"` + tenant + `","app":"` + app + `"},"kind":"` + kind + `"` + rid + `,"schema":"` + fp + `","ops":` + ops + `}`)
}

// The route end to end on a real actor: a new database is created from the registered schema by its first
// request, then mutations, queries, a duplicate, drift and the protocol's and mansio's refusals.
func TestOpsRoute(t *testing.T) {
	r := newOpsRig(t, front.Config{}, "acme:notes", "acme:other")
	r.schemas.add("notes", notesSchema)
	w, rd := r.tok(t, "acme:notes", ticket.Write), r.tok(t, "acme:notes", ticket.Read)
	mutation := func(id, ops string) []byte { return request("acme", "notes", "mutation", id, notesFP, ops) }
	query := func(ops string) []byte { return request("acme", "notes", "query", "", notesFP, ops) }
	insert := `[{"op":"insert","table":"notes","value":{"title":"a","n":1}},{"op":"patch","table":"notes","id":{"$ref":0},"value":{"n":2}}]`

	first := r.post(t, w, mutation("r1", insert), map[string]string{front.TraceHeader: "t-1"})
	if first.status != 200 || first.raw != `{"result":["s1",{"_version":2}],"seq":1}`+"\n" {
		t.Fatalf("first mutation: %d %s", first.status, first.raw)
	}
	if r.schemas.calls != 1 {
		t.Fatalf("schema fetched %d times, want 1 (the database's creation)", r.schemas.calls)
	}
	dup := r.post(t, w, mutation("r1", `[{"op":"delete","table":"notes","id":"s1"}]`), nil)
	if dup.status != 200 || dup.raw != `{"result":["s1",{"_version":2}],"duplicate":true}`+"\n" {
		t.Fatalf("duplicate: %d %s", dup.status, dup.raw)
	}
	q := r.post(t, rd, query(`[{"op":"get","table":"notes","id":"s1"},{"op":"select","table":"notes","filters":[],"limit":10,"aggregate":{"kind":"count"}}]`), nil)
	if q.status != 200 || !strings.Contains(q.raw, `"_version":2`) || !strings.HasSuffix(q.raw, `,1],"seq":1}`+"\n") {
		t.Fatalf("query: %d %s", q.status, q.raw)
	}
	if r.schemas.calls != 1 {
		t.Fatalf("schema fetched %d times after creation, want 1", r.schemas.calls)
	}

	otherFP := r.schemas.add("notes", `{"lists":{"fields":{"name":{"node":{"kind":"string"}}},"storage":"columns","indexes":[]}}`)
	for _, tc := range []struct {
		name   string
		auth   string
		body   []byte
		hdr    map[string]string
		status int
		tag    string
		op     any
		fields map[string]any
	}{
		{"drift", w, request("acme", "notes", "mutation", "r2", otherFP, `[{"op":"get","table":"lists","id":"x"}]`), nil, 409, ir.TagSchemaDrift, ir.Undefined{},
			map[string]any{"expected": notesFP, "actual": otherFP}},
		{"drift before duplicate", w, request("acme", "notes", "mutation", "r1", otherFP, insert), nil, 409, ir.TagSchemaDrift, ir.Undefined{}, nil},
		{"unregistered schema on a new database", r.tok(t, "acme:other", ticket.Write), request("acme", "other", "mutation", "r1", notesFP, insert), nil,
			409, ir.TagSchemaDrift, ir.Undefined{}, map[string]any{"expected": nil, "actual": notesFP}},
		{"unknown table", rd, query(`[{"op":"get","table":"notes","id":"x"},{"op":"get","table":"nope","id":"x"}]`), nil, 400, ir.TagCompile, 1.0,
			map[string]any{"reason": "unknown-collection"}},
		{"not found", w, mutation("r3", `[{"op":"patch","table":"notes","id":"zz","value":{}}]`), nil, 404, ir.TagNotFound, 0.0, map[string]any{"id": "zz"}},
		{"conflict", w, mutation("r4", `[{"op":"assert","table":"notes","id":"s1","expect":{"version":1}}]`), nil, 409, ir.TagConflict, 0.0,
			map[string]any{"check": "version", "expected": 1.0, "actual": 2.0}},
		{"decode error", w, []byte(`{"v":2}`), nil, 400, ir.TagSchema, ir.Undefined{}, map[string]any{"path": "db"}},
		{"too large", w, bytes.Repeat([]byte(" "), ir.MaxBodyBytes+1), nil, 413, ir.TagTooLarge, ir.Undefined{}, map[string]any{"limit": 65536.0}},
		{"bad timeout header", w, query(`[]`), map[string]string{front.TimeoutHeader: "0"}, 400, ir.TagSchema, ir.Undefined{},
			map[string]any{"path": "header:X-Timeout-Ms", "actual": "0"}},
		{"no ticket", "", query(`[]`), nil, 401, "ticket_invalid", ir.Undefined{}, nil},
		{"ticket for another database", r.tok(t, "acme:other", ticket.Write), mutation("r5", insert), nil, 401, "ticket_invalid", ir.Undefined{}, nil},
		{"read ticket for a mutation", rd, mutation("r5", insert), nil, 401, "ticket_invalid", ir.Undefined{}, nil},
		{"not served here", r.tok(t, "acme:gone", ticket.Read), request("acme", "gone", "query", "", notesFP, `[{"op":"get","table":"notes","id":"x"}]`), nil,
			421, "wrong_owner", ir.Undefined{}, nil},
	} {
		got := r.post(t, tc.auth, tc.body, tc.hdr)
		if got.status != tc.status || got.tag() != tc.tag {
			t.Errorf("%s: %d %s, want %d %s", tc.name, got.status, got.raw, tc.status, tc.tag)
			continue
		}
		e := got.body.Value("error").(*ir.Object)
		if op := e.Value("op"); op != tc.op {
			t.Errorf("%s: op %v, want %v (%s)", tc.name, op, tc.op, got.raw)
		}
		for k, v := range tc.fields {
			if !ir.Equal(got.field(k), v) {
				t.Errorf("%s: fields.%s = %v, want %v (%s)", tc.name, k, got.field(k), v, got.raw)
			}
		}
		if got.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: content type %q", tc.name, got.header.Get("Content-Type"))
		}
	}
	if res, _ := http.Get(r.srv.URL + front.OpsPath); res.StatusCode != 405 {
		t.Errorf("GET: %d", res.StatusCode)
	}
	r.adm.Set("acme:notes", 2)
	if got := r.post(t, rd, query(`[{"op":"get","table":"notes","id":"x"}]`), nil); got.status != 421 || got.tag() != "wrong_owner" {
		t.Errorf("epoch moved on: %d %s", got.status, got.raw)
	}
	r.adm.Set("acme:notes", 1)
	r.schemas.err = front.ErrUnavailable
	if got := r.post(t, r.tok(t, "acme:other", ticket.Read), request("acme", "other", "query", "", notesFP, `[{"op":"get","table":"notes","id":"x"}]`), nil); got.status != 503 ||
		got.tag() != "unavailable" || got.header.Get("Retry-After") == "" {
		t.Errorf("registry down on a new database: %d %s %v", got.status, got.raw, got.header)
	}
	// The request's deadline ends while waiting for the registry: expired, not a Defect, and not ambiguous
	// (nothing ran).
	r.schemas.err = fmt.Errorf("x: %w", context.DeadlineExceeded)
	if got := r.post(t, r.tok(t, "acme:other", ticket.Write), request("acme", "other", "mutation", "r9", notesFP, insert), nil); got.status != 504 ||
		got.tag() != "expired" || got.field("ambiguous") != (ir.Undefined{}) || r.log.String() == "" || strings.Contains(r.log.String(), "request failed (internal)") {
		t.Errorf("registry wait past the deadline: %d %s", got.status, got.raw)
	}
	r.schemas.err = nil
	if st := r.s.Stats(); st.InFlight != 0 {
		t.Errorf("in flight %d", st.InFlight)
	}
}

// A query on a new database creates it too (with a read ticket: creation is the registered schema's DDL,
// not the caller's write), and two schemas never mix: once created, the database answers drift.
func TestOpsQueryCreatesTheDatabase(t *testing.T) {
	r := newOpsRig(t, front.Config{}, "acme:notes")
	r.schemas.add("notes", notesSchema)
	rd := r.tok(t, "acme:notes", ticket.Read)
	got := r.post(t, rd, request("acme", "notes", "query", "", notesFP, `[{"op":"select","table":"notes","filters":[],"limit":5}]`), nil)
	if got.status != 200 || got.raw != `{"result":[[]],"seq":0}`+"\n" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

// A request id applied by a raw SQL write cannot answer a /v1/ops mutation: the client reused an id across
// kinds (a client error, 400 at requestId).
func TestOpsRequestIDOfARawWrite(t *testing.T) {
	r := newOpsRig(t, front.Config{}, "acme:notes")
	r.schemas.add("notes", notesSchema)
	w := r.tok(t, "acme:notes", ticket.Write)
	if got := r.post(t, w, request("acme", "notes", "mutation", "r0", notesFP, `[{"op":"get","table":"notes","id":"x"}]`), nil); got.status != 200 {
		t.Fatalf("create: %d %s", got.status, got.raw)
	}
	a, _ := r.host.Lookup(context.Background(), "acme:notes")
	raw := &actor.Request{SQL: `CREATE TABLE IF NOT EXISTS t (x)`, Write: true, RequestID: "raw-1"}
	if err := a.Submit(raw); err != nil {
		t.Fatal(err)
	}
	if res := raw.Wait(context.Background()); res.Err != nil {
		t.Fatal(res.Err)
	}
	got := r.post(t, w, request("acme", "notes", "mutation", "raw-1", notesFP, `[{"op":"get","table":"notes","id":"x"}]`), nil)
	if got.status != 400 || got.tag() != ir.TagSchema || got.field("path") != "requestId" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

// TestOpsDecodeVectors posts every decode.json vector to the live route: a refused body answers exactly the
// vector's error; an accepted one gets past decode (here: to the fingerprint check of a database not served,
// wrong_owner, or with a ticket for it, a protocol answer that is not a decode error).
func TestOpsDecodeVectors(t *testing.T) {
	r := newOpsRig(t, front.Config{})
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "intermediate-representation", "vectors", "decode.json"))
	if err != nil {
		t.Fatal(err)
	}
	file, _ := ir.Parse(string(data))
	vectors := file.(*ir.Object).Value("vectors").([]any)
	for _, item := range vectors {
		v := item.(*ir.Object)
		name := v.Value("name").(string)
		var body []byte
		if b64, ok := v.Value("base64").(string); ok {
			body, _ = base64.StdEncoding.DecodeString(b64)
		} else {
			body = []byte(v.Value("raw").(string))
		}
		if pad, ok := v.Value("pad").(float64); ok {
			body = append(body, bytes.Repeat([]byte(" "), int(pad)-len(body))...)
		}
		auth := r.tok(t, "acme:notes", ticket.Write)
		if req, e := ir.DecodeBody(body); e == nil {
			auth = r.tok(t, req.DB.Tenant+":"+req.DB.App, ticket.Write)
		}
		got := r.post(t, auth, body, nil)
		expect := v.Value("expect").(*ir.Object)
		if expect.Value("ok") == true {
			if got.status == 400 || got.status == 413 || got.status >= 500 {
				t.Errorf("%s: accepted by decode, answered %d %s", name, got.status, got.raw)
			}
			continue
		}
		want := expect.Value("error").(*ir.Object)
		e := got.body.Value("error").(*ir.Object)
		if got.tag() != want.Value("tag") || float64(got.status) != want.Value("status") {
			t.Errorf("%s: %d %s, want %s", name, got.status, got.raw, ir.Stringify(want))
			continue
		}
		if wantOp, has := want.Get("op"); has != e.Has("op") || (has && e.Value("op") != wantOp) {
			t.Errorf("%s: op in %s, want %v", name, got.raw, wantOp)
		}
		if fields, ok := want.Value("fields").(*ir.Object); ok {
			for _, k := range fields.Keys() {
				if !ir.Equal(got.field(k), fields.Value(k)) {
					t.Errorf("%s: fields.%s in %s, want %s", name, k, got.raw, ir.Stringify(fields.Value(k)))
				}
			}
		}
	}
	if len(vectors) < 100 {
		t.Fatalf("%d decode vectors", len(vectors))
	}
}

// FuzzOpsRoute is FuzzDecodeRequest on the live route: any body, with a valid ticket for a real database holding the
// vector schema, answers a well-formed envelope and never a 5xx or a panic. Seeds are the decode vectors and every
// execute vector's requests, so the fuzzer starts from bodies that run.
func FuzzOpsRoute(f *testing.F) {
	r := newOpsRig(f, front.Config{}, "acme:notes")
	h := front.NewHandler(r.s, logx.New(io.Discard, slog.LevelError))
	for _, name := range []string{"decode.json", "execute.json"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "intermediate-representation", "vectors", name))
		if err != nil {
			f.Fatal(err)
		}
		file, _ := ir.Parse(string(data))
		for _, item := range file.(*ir.Object).Value("vectors").([]any) {
			v := item.(*ir.Object)
			if raw, ok := v.Value("raw").(string); ok {
				f.Add([]byte(raw))
			}
			if s, ok := v.Value("schema").(*ir.Object); ok && strings.HasSuffix(v.Value("name").(string), "[columns]") {
				r.schemas.add("notes", ir.Stringify(s))
			}
			if steps, ok := v.Value("steps").([]any); ok {
				for _, st := range steps {
					f.Add([]byte(ir.Stringify(st.(*ir.Object).Value("request"))))
				}
			}
		}
	}
	auth := "Bearer " + r.tok(f, "acme:notes", ticket.Write)
	f.Fuzz(func(t *testing.T, body []byte) {
		req := httptest.NewRequest(http.MethodPost, front.OpsPath, bytes.NewReader(body))
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("%d %s for %q", rec.Code, rec.Body.Bytes(), body)
		}
		got := parseBody(t, rec.Body.Bytes())
		if _, ok := got.Get("result"); !ok == !got.Has("error") {
			t.Fatalf("not an envelope: %s", rec.Body.Bytes())
		}
	})
}
