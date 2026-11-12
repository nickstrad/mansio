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
)

// idList returns the request ids in a's _mansio_requests, sorted and comma-joined.
func idList(t *testing.T, a *Actor, c context.Context) string {
	t.Helper()
	r := &Request{SQL: `SELECT coalesce(group_concat(request_id, ','), '') FROM (SELECT request_id FROM _mansio_requests ORDER BY 1)`, MaxRows: 1}
	r.boundBy(c, time.Now())
	if err := a.Submit(r); err != nil {
		t.Fatal(err)
	}
	res := r.Wait(c)
	if res.Err != nil || len(res.Values) != 1 {
		t.Fatalf("id rows: %+v", res)
	}
	s, _ := res.Values[0][0].(string)
	return s
}

// FuzzActorModel is a model-based test: a byte script becomes operations on one real actor (real SQLite), and
// a map is the model.
//
// Script: each op is 2 bytes (kind, arg), keys are arg%8:
//
//	kind%6 == 0  queue a write:  INSERT OR REPLACE kv(id=key, v=unique)
//	kind%6 == 1  queue a delete: DELETE FROM kv WHERE id = key
//	kind%6 == 2  queue a read of key
//	kind%6 == 3  flush: wait for every queued op
//	kind%6 == 5  queue a write with request id "id-<(arg>>3)&3>": (arg>>5)&3 picks INSERT OR REPLACE
//	             (0, 1), plain INSERT (2, fails if key exists) or DELETE (3). Four ids, so ids repeat often.
//	kind%6 == 4  queue an insert that fails if key exists: INSERT OR ROLLBACK (ends the batch's transaction,
//	             see txnend_test.go) when arg bit 6 is clear, plain INSERT (fails on its own) when set
//
// The model holds the batch property: a write answered without error is committed, a write answered with an
// error is not. So at each flush the model applies, in queue order, exactly the writes that reported success;
// after the flush every key read on its own must equal the model. A successful insert must find its key absent
// in the model at that point in the order.
//
// Ops queued between flushes may share batches, and the actor runs a batch's writes before its reads, so a read
// may see any value its key held during that group (checked against everything queued for that key in the
// group). arg's bit 7 picks the read pool.
//
// Request ids: a write whose id the model has applied must answer Duplicate with the original's row
// count and change nothing; a write answered without Duplicate must carry an id the model has not applied; a
// write that fails on its own is never a duplicate. After each flush the database's id rows equal the model's
// applied ids: a failed or rolled-back write leaves none.
func FuzzActorModel(f *testing.F) {
	f.Add([]byte{0, 1, 2, 1, 3, 0})
	f.Add([]byte{0, 1, 0, 1, 1, 1, 2, 1, 3, 0, 2, 1})
	f.Add([]byte{0, 0x81, 2, 0x81, 1, 1, 3, 0, 0, 2, 0, 3, 2, 2})
	f.Add([]byte{0, 1, 0, 2, 4, 1, 0, 3, 4, 4, 3, 0, 4, 0x41, 0, 5, 3, 0})
	// Request ids: the same id twice in a group, across a flush, a failing insert then its retry, a delete.
	f.Add([]byte{5, 0x01, 5, 0x01, 3, 0, 5, 0x02, 5, 0x41, 3, 0, 5, 0x49, 0, 1, 5, 0x49, 3, 0, 5, 0x71, 5, 0x71, 3, 0})
	f.Add([]byte{0, 3, 5, 0x43, 4, 3, 5, 0x0b, 3, 0, 5, 0x43, 5, 0x0b, 3, 0})
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 400 {
			t.Skip()
		}
		readConns := 0
		if len(script) > 1 && script[1]&0x80 != 0 {
			readConns = 2
		}
		a := startedKV(t, Config{ReadConns: readConns, MaxBatch: 8}, 0)
		model := map[int64]int64{}
		type pending struct {
			req    *Request
			key    int64
			v      int64 // value written; -1 for a delete
			read   bool
			insert bool   // fails iff key exists
			id     string // request id; "" = none
		}
		applied := map[string]int64{} // request id → rows its write affected
		var group []pending
		held := map[int64][]int64{} // key → every value it may hold during the current group (-1 = absent)
		val := func(k int64) int64 {
			if v, ok := model[k]; ok {
				return v
			}
			return -1
		}
		apply := func(p pending) { // the model applies p's write
			if p.v == -1 {
				delete(model, p.key)
			} else {
				model[p.key] = p.v
			}
		}
		flush := func(step int) {
			c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, p := range group {
				res := p.req.Wait(c)
				prior, seen := applied[p.id] // ids are never "", so seen ⇒ p carries an applied id
				switch {
				case p.read:
					if res.Err != nil {
						t.Fatalf("step %d: %s failed: %v", step, p.req.SQL, res.Err)
					}
					got := int64(-1)
					if res.Rows == 1 {
						got = res.First
					}
					if !slices.Contains(held[p.key], got) {
						t.Fatalf("step %d: read key %d = %d, want one of %v", step, p.key, got, held[p.key])
					}
				case errors.Is(res.Err, ErrAborted):
					// Not committed: a batch-mate ended the transaction. The read-back below checks it.
				case res.Err != nil && seen:
					t.Fatalf("step %d: write with applied id %s failed instead of answering duplicate: %v", step, p.id, res.Err)
				case res.Err != nil:
					// Its own error: only an insert may fail, and only if its key existed at some point of the
					// group (possibly through a batch-mate that was later rolled back with it).
					if !p.insert || !slices.ContainsFunc(held[p.key], func(x int64) bool { return x != -1 && x != p.v }) {
						t.Fatalf("step %d: %s %v failed: %v", step, p.req.SQL, p.req.Args, res.Err)
					}
				case res.Duplicate:
					if !seen || prior != res.Rows {
						t.Fatalf("step %d: duplicate for id %q (applied: %v, rows %d), model %v", step, p.id, seen, res.Rows, applied)
					}
				case seen:
					t.Fatalf("step %d: id %s applied twice", step, p.id)
				case p.insert && val(p.key) != -1:
					t.Fatalf("step %d: insert of existing key %d succeeded", step, p.key)
				case p.id != "":
					want := int64(1)
					if p.v == -1 && val(p.key) == -1 {
						want = 0 // delete of an absent key
					}
					if res.Rows != want {
						t.Fatalf("step %d: %s affected %d rows, want %d", step, p.req.SQL, res.Rows, want)
					}
					applied[p.id] = res.Rows
					apply(p)
				default:
					apply(p)
				}
			}
			group, held = group[:0], map[int64][]int64{}
			for k := int64(0); k < 8; k++ {
				r := a.Do(c, `SELECT v FROM kv WHERE id = ?`, false, k)
				got := int64(-1)
				if r.Rows == 1 {
					got = r.First
				}
				if r.Err != nil || got != val(k) {
					t.Fatalf("step %d: after flush key %d = %d (%+v), model %d", step, k, got, r, val(k))
				}
			}
			if len(applied) > 0 {
				ids := slices.Sorted(maps.Keys(applied))
				if got := idList(t, a, c); got != strings.Join(ids, ",") {
					t.Fatalf("step %d: id rows %q, model %v", step, got, ids)
				}
			}
		}
		submit := func(p pending) {
			for a.Submit(p.req) != nil { // mailbox full: let the actor drain
				time.Sleep(time.Millisecond)
			}
			group = append(group, p)
			if !p.read {
				held[p.key] = append(held[p.key], p.v)
			}
		}
		for i := 0; i+1 < len(script); i += 2 {
			kind, arg := script[i]%6, script[i+1]
			key := int64(arg % 8)
			if _, ok := held[key]; !ok {
				held[key] = []int64{val(key)}
			}
			v := int64(i + 1)
			switch kind {
			case 0:
				submit(pending{req: &Request{SQL: `INSERT OR REPLACE INTO kv (id, v) VALUES (?, ?)`, Args: []any{key, v}, Write: true}, key: key, v: v})
			case 1:
				submit(pending{req: &Request{SQL: `DELETE FROM kv WHERE id = ?`, Args: []any{key}, Write: true}, key: key, v: -1})
			case 2:
				submit(pending{req: &Request{SQL: `SELECT v FROM kv WHERE id = ?`, Args: []any{key}}, key: key, read: true})
			case 3:
				flush(i / 2)
			case 4:
				q := `INSERT OR ROLLBACK INTO kv (id, v) VALUES (?, ?)`
				if arg&0x40 != 0 {
					q = `INSERT INTO kv (id, v) VALUES (?, ?)`
				}
				submit(pending{req: &Request{SQL: q, Args: []any{key, v}, Write: true}, key: key, v: v, insert: true})
			case 5:
				id := fmt.Sprintf("id-%d", (arg>>3)&3)
				p := pending{key: key, v: v, id: id}
				switch (arg >> 5) & 3 {
				case 0, 1:
					p.req = &Request{SQL: `INSERT OR REPLACE INTO kv (id, v) VALUES (?, ?)`, Args: []any{key, v}}
				case 2:
					p.req, p.insert = &Request{SQL: `INSERT INTO kv (id, v) VALUES (?, ?)`, Args: []any{key, v}}, true
				case 3:
					p.req, p.v = &Request{SQL: `DELETE FROM kv WHERE id = ?`, Args: []any{key}}, -1
				}
				p.req.Write, p.req.RequestID = true, id
				submit(p)
			}
		}
		flush(len(script) / 2)
	})
}
