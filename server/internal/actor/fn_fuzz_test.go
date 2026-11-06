package actor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/panicx"
)

var errInjected = errors.New("injected closure failure")

// FuzzActorClosureModel is FuzzActorModel for closure requests (fn.go): a byte script becomes closure writes,
// closure reads and SQL writes on one real actor (real SQLite), and a map is the model.
//
// Script: each op is 3 bytes (kind, a, b). k1 = a%8, k2 = (a>>3)%8, v is unique per op. A write carries request
// id "id-<(b>>4)&3>" when b bit 7 is set (four ids shared by closures and SQL writes, so they repeat and
// collide across kinds).
//
//	kind%5 == 0  closure write: k1 = v, k2 = v+1, then count(*); Body {"n":count,"v":v}. b&7 injects a
//	             failure after the writes: 3 returns an error, 4 runs a failing statement (NOT NULL), 5 panics,
//	             6 runs INSERT OR ROLLBACK (ends the batch transaction, PG-01), 7 does that too but ignores the
//	             error, writes again and returns success (the poisoned Tx must refuse the write and fail the
//	             request; nothing may autocommit); 0–2 succeed. Bit 3 first
//	             tries COMMIT, which must be refused (ErrTxControl) without harm.
//	kind%5 == 1  closure read of k1 (ReadConns from the script's first byte): it also tries to write, which must
//	             be refused (ErrReadOnly); Body is the value or -1
//	kind%5 == 2  flush: wait for every queued op, then check the whole state
//	kind%5 == 3  SQL write k1 = v (the raw path, record version 0x01)
//	kind%5 == 4  closure delete of k1; Body {"d":rows affected}
//
// The model holds the PG-01 property (a write answered without error committed, one answered with an error did
// not), so at each flush it applies, in queue order, exactly the writes that reported success; a closure's
// Body must equal what the model computes at that point. Request ids: an id applied by the same kind answers
// Duplicate with the first answer (Body byte for byte, or rows) and changes nothing; an id applied by the other
// kind answers ErrRecordKind (also when that other request was a batch-mate later rolled back: an own error
// stays); a failed, aborted or panicked closure leaves no rows and no id row. After each
// flush the table equals the model, and _mansio_requests holds exactly the applied ids with their records.
func FuzzActorClosureModel(f *testing.F) {
	f.Add([]byte{0, 0x09, 0x80, 2, 0, 0, 0, 0x09, 0x80, 1, 1, 0, 2, 0, 0})
	// Each failure mode, with an id, then the same id succeeding.
	f.Add([]byte{0, 1, 0x83, 0, 2, 0x84, 0, 3, 0x85, 0, 4, 0x86, 2, 0, 0, 0, 1, 0x80, 0, 2, 0x80, 2, 0, 0})
	// Cross-kind ids, a delete, reads around writes, a refused COMMIT.
	f.Add([]byte{3, 1, 0x90, 0, 2, 0x90, 4, 1, 0xa0, 2, 0, 0, 1, 1, 0, 0, 0x12, 0x08, 4, 1, 0xa0, 2, 0, 0})
	f.Add([]byte{0x80, 1, 0, 1, 1, 0, 0, 1, 6, 1, 1, 0, 0, 2, 0, 2, 0, 0})
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 600 {
			t.Skip()
		}
		readConns := 0
		if len(script) > 0 && script[0]&0x80 != 0 {
			readConns = 2
		}
		a := startedKV(t, Config{ReadConns: readConns, MaxBatch: 8}, 0)
		model := map[int64]int64{}
		type rec struct {
			closure bool
			body    string
			rows    int64
		}
		applied := map[string]rec{}
		type pending struct {
			req     *Request
			closure bool
			read    bool
			del     bool
			mode    byte // closure write failure mode (b&7)
			k1, k2  int64
			v       int64
			id      string
			refused *bool // the closure saw its forbidden statement refused
		}
		var group []pending
		held := map[int64][]int64{}
		val := func(k int64) int64 {
			if v, ok := model[k]; ok {
				return v
			}
			return -1
		}
		hold := func(k, v int64) {
			if _, ok := held[k]; !ok {
				held[k] = []int64{val(k)}
			}
			held[k] = append(held[k], v)
		}
		flush := func(step int) {
			c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for j, p := range group {
				res := p.req.Wait(c)
				prior, seen := applied[p.id]
				fail := func(format string, args ...any) {
					t.Helper()
					t.Fatalf("step %d (k1 %d k2 %d v %d id %q mode %d): %s; result %+v", step, p.k1, p.k2, p.v, p.id, p.mode,
						fmt.Sprintf(format, args...), res)
				}
				if p.refused != nil && !*p.refused && res.Err == nil && !res.Duplicate {
					fail("a forbidden statement was not refused")
				}
				switch {
				case p.read:
					var got int64
					if res.Err != nil {
						fail("read failed")
					}
					if _, err := fmt.Sscan(string(res.Body), &got); err != nil {
						fail("read body: %v", err)
					}
					if !slices.Contains(held[p.k1], got) {
						fail("read %d, want one of %v (model %d)", got, held[p.k1], val(p.k1))
					}
				case errors.Is(res.Err, ErrAborted):
					// Not committed: a batch-mate ended the transaction; the read-back below checks it.
				case seen && prior.closure != p.closure:
					if !errors.Is(res.Err, ErrRecordKind) {
						fail("an id applied by the other kind must answer ErrRecordKind")
					}
				case seen:
					switch {
					case res.Err != nil || !res.Duplicate:
						fail("applied id %s must answer duplicate", p.id)
					case p.closure && string(res.Body) != prior.body:
						fail("duplicate body %q, want the first %q", res.Body, prior.body)
					case !p.closure && res.Rows != prior.rows:
						fail("duplicate rows %d, want %d", res.Rows, prior.rows)
					}
				case res.Duplicate:
					fail("duplicate for an id the model has not applied (%v)", slices.Sorted(maps.Keys(applied)))
				case errors.Is(res.Err, ErrRecordKind) && slices.ContainsFunc(group[:j], func(q pending) bool {
					return q.id == p.id && q.closure != p.closure
				}):
					// A batch-mate of the other kind claimed the id before this one ran, and its batch was then
					// rolled back: an own error stays (as for a constraint failure caused by a rolled-back
					// batch-mate in FuzzActorModel), and nothing of this request ran.
				case res.Err != nil:
					var pp *panicx.Panic
					ok := p.closure && !p.del && (p.mode == 3 && errors.Is(res.Err, errInjected) ||
						p.mode == 4 || p.mode == 6 || p.mode == 7 || p.mode == 5 && errors.As(res.Err, &pp))
					if !ok {
						fail("unexpected error %v", res.Err)
					}
				case p.closure && !p.del && p.mode >= 3:
					fail("an injected failure succeeded")
				case p.del:
					rows := int64(0)
					if val(p.k1) != -1 {
						rows = 1
					}
					delete(model, p.k1)
					if want := fmt.Sprintf(`{"d":%d}`, rows); string(res.Body) != want {
						fail("body %q, want %q", res.Body, want)
					}
					if p.id != "" {
						applied[p.id] = rec{closure: true, body: string(res.Body)}
					}
				case p.closure:
					model[p.k1], model[p.k2] = p.v, p.v+1
					if want := fmt.Sprintf(`{"n":%d,"v":%d}`, len(model), p.v); string(res.Body) != want {
						fail("body %q, want %q", res.Body, want)
					}
					if p.id != "" {
						applied[p.id] = rec{closure: true, body: string(res.Body)}
					}
				default:
					model[p.k1] = p.v
					if res.Rows != 1 {
						fail("SQL write affected %d rows", res.Rows)
					}
					if p.id != "" {
						applied[p.id] = rec{rows: 1}
					}
				}
			}
			group, held = group[:0], map[int64][]int64{}
			// The whole state through one read closure: the table, and the id records.
			state := a.DoFn(c, false, "", func(tx *Tx) (Output, error) {
				kv, err := tx.Query(`SELECT id, v FROM kv ORDER BY id`, 100)
				if err != nil {
					return Output{}, err
				}
				var ids Rows
				exists, err := tx.Query(`SELECT count(*) FROM sqlite_schema WHERE name = '_mansio_requests'`, 1)
				if err != nil {
					return Output{}, err
				}
				if exists.Values[0][0] == int64(1) {
					if ids, err = tx.Query(`SELECT request_id, result FROM _mansio_requests ORDER BY request_id`, 100); err != nil {
						return Output{}, err
					}
				}
				return Output{Value: [2]Rows{kv, ids}}, nil
			})
			if state.Err != nil {
				t.Fatalf("step %d: state read: %v", step, state.Err)
			}
			rows := state.Value.([2]Rows)
			got := map[int64]int64{}
			for _, r := range rows[0].Values {
				got[r[0].(int64)] = r[1].(int64)
			}
			if !maps.Equal(got, model) {
				t.Fatalf("step %d: table %v, model %v", step, got, model)
			}
			var gotIDs []string
			for _, r := range rows[1].Values {
				id := r[0].(string)
				gotIDs = append(gotIDs, id)
				want := encodeOutcome(Result{Rows: applied[id].rows})
				if applied[id].closure {
					want = encodeBody([]byte(applied[id].body))
				}
				if b, _ := r[1].([]byte); string(b) != string(want) {
					t.Fatalf("step %d: id %s record %q, want %q", step, id, b, want)
				}
			}
			if want := slices.Sorted(maps.Keys(applied)); strings.Join(gotIDs, ",") != strings.Join(want, ",") {
				t.Fatalf("step %d: id rows %v, model %v", step, gotIDs, want)
			}
		}
		submit := func(p pending) {
			for a.Submit(p.req) != nil { // mailbox full: let the actor drain
				time.Sleep(time.Millisecond)
			}
			group = append(group, p)
		}
		for i := 0; i+2 < len(script); i += 3 {
			kind, ab, b := script[i]%5, script[i+1], script[i+2]
			p := pending{k1: int64(ab % 8), k2: int64((ab >> 3) % 8), v: int64(i+1) * 10, mode: b & 7}
			if b&0x80 != 0 && kind != 1 {
				p.id = fmt.Sprintf("id-%d", (b>>4)&3)
			}
			switch kind {
			case 0:
				p.closure, p.refused = true, new(bool)
				tryCommit, refused := b&0x08 != 0, p.refused
				*refused = !tryCommit
				p.req = &Request{Write: true, RequestID: p.id, Fn: func(tx *Tx) (Output, error) {
					if tryCommit {
						_, err := tx.Exec(`COMMIT`)
						*refused = errors.Is(err, ErrTxControl)
					}
					for _, kv := range [][2]int64{{p.k1, p.v}, {p.k2, p.v + 1}} {
						if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (id, v) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
							return Output{}, err
						}
					}
					n, err := tx.Query(`SELECT count(*) FROM kv`, 1)
					if err != nil {
						return Output{}, err
					}
					switch p.mode {
					case 3:
						return Output{}, errInjected
					case 4:
						if _, err := tx.Exec(`INSERT INTO kv (id, v) VALUES (100, NULL)`); err != nil {
							return Output{}, err
						}
					case 5:
						panic("injected")
					case 6:
						if _, err := tx.Exec(`INSERT OR ROLLBACK INTO kv (id, v) VALUES (100, NULL)`); err != nil {
							return Output{}, err
						}
					case 7: // swallow the error that ended the transaction, write again, claim success
						tx.Exec(`INSERT OR ROLLBACK INTO kv (id, v) VALUES (100, NULL)`)
						if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (id, v) VALUES (?, ?)`, p.k1, p.v+2); !errors.Is(err, tx.Err()) {
							return Output{}, fmt.Errorf("a statement after the poisoning error ran: %v", err)
						}
					}
					return Output{Body: fmt.Appendf(nil, `{"n":%d,"v":%d}`, n.Values[0][0], p.v)}, nil
				}}
				hold(p.k1, p.v)
				hold(p.k2, p.v+1)
			case 1:
				p.read, p.closure, p.refused = true, true, new(bool)
				refused := p.refused
				p.req = &Request{Fn: func(tx *Tx) (Output, error) {
					_, err := tx.Exec(`DELETE FROM kv`)
					*refused = errors.Is(err, ErrReadOnly)
					r, err := tx.Query(`SELECT v FROM kv WHERE id = ?`, 1, p.k1)
					if err != nil {
						return Output{}, err
					}
					v := int64(-1)
					if len(r.Values) == 1 {
						v = r.Values[0][0].(int64)
					}
					return Output{Body: fmt.Append(nil, v)}, nil
				}}
				if _, ok := held[p.k1]; !ok {
					held[p.k1] = []int64{val(p.k1)}
				}
			case 2:
				flush(i / 3)
				continue
			case 3:
				p.req = &Request{SQL: `INSERT OR REPLACE INTO kv (id, v) VALUES (?, ?)`, Args: []any{p.k1, p.v}, Write: true, RequestID: p.id}
				hold(p.k1, p.v)
			case 4:
				p.closure, p.del = true, true
				p.req = &Request{Write: true, RequestID: p.id, Fn: func(tx *Tx) (Output, error) {
					n, err := tx.Exec(`DELETE FROM kv WHERE id = ?`, p.k1)
					return Output{Body: fmt.Appendf(nil, `{"d":%d}`, n)}, err
				}}
				hold(p.k1, -1)
			}
			submit(p)
		}
		flush(len(script) / 3)
	})
}
