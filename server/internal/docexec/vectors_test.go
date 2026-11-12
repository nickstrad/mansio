package docexec_test

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
	"github.com/nickstrad/mansio/server/internal/doccompile"
	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/docexec/docactor"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/ir"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/actordb"
)

// vectorDir is the shared corpus at the repo root (intermediate-representation/vectors/README.md).
var vectorDir = filepath.Join("..", "..", "..", "intermediate-representation", "vectors")

func loadVectors(t testing.TB, name string) []*ir.Object {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(vectorDir, name))
	if err != nil {
		t.Fatal(err)
	}
	v, perr := ir.Parse(string(data))
	if perr != nil {
		t.Fatalf("%s: %v", name, perr)
	}
	var out []*ir.Object
	for _, item := range v.(*ir.Object).Value("vectors").([]any) {
		out = append(out, item.(*ir.Object))
	}
	return out
}

// listIDs hands out a step's serverIds in order and counts any it did not have.
type listIDs struct {
	ids  []string
	over int
}

func (l *listIDs) NewID() string {
	if len(l.ids) == 0 {
		l.over++
		return "exhausted"
	}
	id := l.ids[0]
	l.ids = l.ids[1:]
	return id
}

// db is one test database on a manual clock: a real actor on a file, or (durable) a key in a vactor.Host over
// real SQLite actors and the fake object store, which can passivate, write back and crash.
type db struct {
	t      testing.TB
	a      *actor.Actor // nil in durable mode: the host's current actor serves
	clock  *dettest.Clock
	schema *docexec.Schema
	cache  *docexec.Schemas

	h     *vactor.Host
	store *durabletest.Store
	cfg   actor.Config
}

const durableKey = "t/doc"

// openDurable is a durable-mode database: a host over the fake store, created and initialized with s.
func openDurable(t testing.TB, s *docexec.Schema, cfg actor.Config) *db {
	t.Helper()
	clock := dettest.NewClock(time.UnixMilli(0))
	cfg.Clock = clock
	d := &db{t: t, clock: clock, schema: s, cache: docexec.NewSchemas(0), store: durabletest.NewStore(), cfg: cfg}
	d.newHost()
	return d
}

// newHost starts a host with an empty cache (a fresh process) and initializes the database if its published
// generation has no schema yet, as the route does on ErrUninitialized.
func (d *db) newHost() {
	h, err := vactor.NewHost(nil, actordb.Opener{Cfg: d.cfg}, vactor.Options{CacheDir: filepath.Join(d.t.TempDir(), "c"), Store: d.store,
		CreateMissing: true, FailBackoff: -1})
	if err != nil {
		d.t.Fatal(err)
	}
	d.h = h
	d.t.Cleanup(func() { h.Close() })
	if res := d.do(docactor.InitRequest(d.schema)); res.Err != nil {
		d.t.Fatalf("init: %v", res.Err)
	}
}

// abandon stops the host without publishing anything (the store refuses its shutdown's write-backs): what it
// had not written back is lost.
func (d *db) abandon() {
	d.store.SetFaults(durabletest.Faults{Down: true})
	d.h.Close()
	d.store.SetFaults(durabletest.Faults{})
}

// crash abandons the host, then starts a new one: the database is back at its last published generation.
func (d *db) crash() {
	d.abandon()
	d.newHost()
}

// actor is the actor serving the database now (activating it in durable mode).
func (d *db) actor() *actor.Actor {
	if d.h == nil {
		return d.a
	}
	res, err := d.h.Get(context.Background(), durableKey)
	if err != nil {
		d.t.Fatalf("activate: %v", err)
	}
	return res.DB.(*actordb.DB).Actor()
}

func openDB(t testing.TB, s *docexec.Schema, cfg actor.Config) *db {
	t.Helper()
	clock := dettest.NewClock(time.UnixMilli(0))
	cfg.Clock = clock
	a := actor.New(1, filepath.Join(t.TempDir(), "db.sqlite"), cfg)
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	d := &db{t: t, a: a, clock: clock, schema: s, cache: docexec.NewSchemas(0)}
	if s != nil {
		if res := d.do(docactor.InitRequest(s)); res.Err != nil {
			t.Fatalf("init: %v", res.Err)
		}
	}
	return d
}

func (d *db) do(r *actor.Request) actor.Result {
	if err := d.actor().Submit(r); err != nil {
		return actor.Result{Err: err}
	}
	return r.Wait(context.Background())
}

// at moves the clock to ms (never back).
func (d *db) at(ms int64) {
	if now := d.clock.Now().UnixMilli(); ms > now {
		d.clock.Advance(time.Duration(ms-now) * time.Millisecond)
	}
}

// seed writes documents as given, bypassing the protocol (README: execute.json seed).
func (d *db) seed(tables *ir.Object) {
	d.t.Helper()
	res := d.do(&actor.Request{Write: true, Fn: func(tx *actor.Tx) (actor.Output, error) {
		for _, table := range tables.Keys() {
			for _, item := range tables.Value(table).([]any) {
				doc := item.(*ir.Object)
				body := ir.NewObject()
				for _, k := range doc.Keys() {
					if k[0] != '_' {
						body.Set(k, doc.Value(k))
					}
				}
				sql, e := doccompile.Insert(d.schema.Schema, table, body, doc.Value("_id").(string), doc.Value("_creationTime").(float64))
				if e != nil {
					return actor.Output{}, errors.New(e.Message())
				}
				if _, err := tx.Exec(sql.Text, doccompile.Args(sql.Values)...); err != nil {
					return actor.Output{}, err
				}
				q, _ := doccompile.Quote(table)
				if _, err := tx.Exec("UPDATE "+q+` SET "_version" = ? WHERE "_id" = ?`, int64(doc.Value("_version").(float64)), doc.Value("_id")); err != nil {
					return actor.Output{}, err
				}
			}
		}
		return actor.Output{}, nil
	}})
	if res.Err != nil {
		d.t.Fatalf("seed: %v", res.Err)
	}
}

// dump is every row of every table, ordered by _id, decoded.
func (d *db) dump() map[string]any {
	d.t.Helper()
	out := map[string]any{}
	res := d.do(&actor.Request{Fn: func(tx *actor.Tx) (actor.Output, error) {
		for _, table := range d.schema.Schema.Names() {
			plan := doccompile.Plan{Table: table, Limit: 1000.0}
			sql, e := doccompile.Select(d.schema.Schema, plan, doccompile.SQLite)
			if e != nil {
				return actor.Output{}, errors.New(e.Message())
			}
			rows, err := tx.Query(sql.Text, 1000, doccompile.Args(sql.Values)...)
			if err != nil {
				return actor.Output{}, err
			}
			docs, e := doccompile.Decode(d.schema.Schema, plan, toRows(rows))
			if e != nil {
				return actor.Output{}, errors.New(e.Message())
			}
			out[table] = docs
		}
		return actor.Output{}, nil
	}})
	if res.Err != nil {
		d.t.Fatalf("dump: %v", res.Err)
	}
	return out
}

// requests is _mansio_requests ordered by id, result as lowercase hex (README: final.requests).
func (d *db) requests() []any {
	d.t.Helper()
	var out []any
	res := d.do(&actor.Request{Fn: func(tx *actor.Tx) (actor.Output, error) {
		exists, err := tx.Query(`SELECT count(*) FROM sqlite_master WHERE name = '_mansio_requests'`, 1)
		if err != nil || exists.Values[0][0] == int64(0) {
			return actor.Output{}, err
		}
		rows, err := tx.Query("select request_id, applied_ms, result from _mansio_requests order by request_id", 10_000)
		if err != nil {
			return actor.Output{}, err
		}
		for _, r := range rows.Values {
			out = append(out, ir.NewObject("request_id", r[0], "applied_ms", float64(r[1].(int64)), "result_hex", hex.EncodeToString(r[2].([]byte))))
		}
		return actor.Output{}, nil
	}})
	if res.Err != nil {
		d.t.Fatalf("requests: %v", res.Err)
	}
	return out
}

func toRows(rows actor.Rows) []doccompile.Row {
	out := make([]doccompile.Row, len(rows.Values))
	for i, values := range rows.Values {
		row := doccompile.Row{}
		for j, c := range rows.Columns {
			row[c] = values[j]
		}
		out[i] = row
	}
	return out
}

// outcome is one request's answer: the result and duplicate flag, or the wire error.
type outcome struct {
	result    any
	seq       int64 // meaningless on a duplicate
	duplicate bool
	err       *ir.Error
}

// seqOf is the commit sequence a request that ran answered with.
func seqOf(res actor.Result) int64 {
	answer, _ := res.Value.(docexec.Answer)
	return answer.Seq
}

// exec runs one request body through decode, Check (initializing nothing: vector databases are initialized at
// open) and the actor, as the route does.
func (d *db) exec(body []byte, ids *listIDs, wrap func(docexec.Tx) docexec.Tx) outcome {
	d.t.Helper()
	req, e := ir.DecodeBody(body)
	if e != nil {
		return outcome{err: e}
	}
	call := &docactor.Call{Req: req, Schemas: d.cache, IDs: ids, Wrap: wrap}
	res := d.do(call.Request())
	if res.Err != nil {
		var f *docexec.Failure
		if errors.As(res.Err, &f) {
			return outcome{err: f.Err}
		}
		return outcome{err: ir.NewError("unexpected", "detail", res.Err.Error())}
	}
	v, pe := ir.Parse(string(res.Body))
	if pe != nil {
		d.t.Fatalf("result body %q: %v", res.Body, pe)
	}
	return outcome{result: v, duplicate: res.Duplicate, seq: seqOf(res)}
}

// assertWireError is the README's comparison: tag, status and op exactly; fields as a subset.
func assertWireError(t testing.TB, name string, got *ir.Error, want *ir.Object) {
	t.Helper()
	wire := ir.Stringify(got.Wire())
	if got.Tag != want.Value("tag") {
		t.Errorf("%s: tag %s, want %v", name, wire, want.Value("tag"))
	}
	if float64(got.Status()) != want.Value("status") {
		t.Errorf("%s: status %d, want %v (%s)", name, got.Status(), want.Value("status"), wire)
	}
	wantOp, hasOp := want.Get("op")
	if hasOp != got.HasOp || (hasOp && wantOp != float64(got.Op)) {
		t.Errorf("%s: op %v/%v, want %v (%s)", name, got.HasOp, got.Op, wantOp, wire)
	}
	if fields, ok := want.Value("fields").(*ir.Object); ok {
		for _, k := range fields.Keys() {
			if !ir.Equal(got.Field(k), fields.Value(k)) {
				t.Errorf("%s: fields.%s = %s, want %s (%s)", name, k, ir.Stringify(got.Field(k)), ir.Stringify(fields.Value(k)), wire)
			}
		}
	}
}

func decodeSchema(t testing.TB, v *ir.Object) *docexec.Schema {
	t.Helper()
	s, e := docexec.Decode(ir.Stringify(v.Value("schema")))
	if e != nil {
		t.Fatalf("schema: %v", e)
	}
	if s.Fingerprint != v.Value("fingerprint") {
		t.Fatalf("fingerprint %s, want %v", s.Fingerprint, v.Value("fingerprint"))
	}
	return s
}

// TestExecuteVectors runs every execute.json vector against a real actor.
func TestExecuteVectors(t *testing.T) {
	vectors := loadVectors(t, "execute.json")
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range vectors {
		name := v.Value("name").(string)
		t.Run(name, func(t *testing.T) {
			s := decodeSchema(t, v)
			d := openDB(t, s, actor.Config{})
			d.seed(v.Value("seed").(*ir.Object))
			for i, item := range v.Value("steps").([]any) {
				step := item.(*ir.Object)
				d.at(int64(step.Value("clock").(float64)))
				ids := &listIDs{}
				for _, id := range step.Value("serverIds").([]any) {
					ids.ids = append(ids.ids, id.(string))
				}
				got := d.exec([]byte(ir.Stringify(step.Value("request"))), ids, nil)
				expect := step.Value("expect").(*ir.Object)
				if want, ok := expect.Value("error").(*ir.Object); ok {
					if got.err == nil {
						t.Fatalf("step %d: ok %s, want %s", i, ir.Stringify(got.result), ir.Stringify(want))
					}
					assertWireError(t, name, got.err, want)
				} else {
					if got.err != nil {
						t.Fatalf("step %d: %s, want %s", i, ir.Stringify(got.err.Wire()), ir.Stringify(expect))
					}
					if !ir.Equal(got.result, expect.Value("result")) {
						t.Errorf("step %d: result %s, want %s", i, ir.Stringify(got.result), ir.Stringify(expect.Value("result")))
					}
					if want := expect.Value("duplicate") == true; got.duplicate != want {
						t.Errorf("step %d: duplicate %v, want %v", i, got.duplicate, want)
					}
					if want, ok := expect.Get("seq"); ok && want != float64(got.seq) {
						t.Errorf("step %d: seq %d, want %v", i, got.seq, want)
					}
				}
				if len(ids.ids) != 0 || ids.over != 0 {
					t.Errorf("step %d: server ids: %d unused, %d over", i, len(ids.ids), ids.over)
				}
			}
			final := v.Value("final").(*ir.Object)
			got := d.dump()
			for _, table := range s.Schema.Names() {
				want, ok := final.Get(table)
				if !ok {
					want = []any{}
				}
				if !ir.Equal(got[table], want) {
					t.Errorf("final %s:\n got %s\nwant %s", table, ir.Stringify(got[table]), ir.Stringify(want))
				}
			}
			if want, ok := v.Get("finalRequests"); ok {
				got := d.requests()
				if got == nil {
					got = []any{}
				}
				if !ir.Equal(got, want) {
					t.Errorf("final requests:\n got %s\nwant %s", ir.Stringify(got), ir.Stringify(want))
				}
			}
		})
	}
}
