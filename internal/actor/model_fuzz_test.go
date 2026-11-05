package actor

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// FuzzActorModel is a model-based test: a byte script becomes operations on one real actor (real SQLite), and
// a map is the model.
//
// Script: each op is 2 bytes (kind, arg), keys are arg%8:
//
//	kind%5 == 0  queue a write:  INSERT OR REPLACE kv(id=key, v=unique)
//	kind%5 == 1  queue a delete: DELETE FROM kv WHERE id = key
//	kind%5 == 2  queue a read of key
//	kind%5 == 3  flush: wait for every queued op
//	kind%5 == 4  queue an insert that fails if key exists: INSERT OR ROLLBACK (ends the batch's transaction,
//	             PG-01) when arg bit 6 is clear, plain INSERT (fails on its own) when set
//
// The model holds the PG-01 property: a write answered without error is committed, a write answered with an
// error is not. So at each flush the model applies, in queue order, exactly the writes that reported success;
// after the flush every key read on its own must equal the model. A successful insert must find its key absent
// in the model at that point in the order.
//
// Ops queued between flushes may share batches, and the actor runs a batch's writes before its reads, so a read
// may see any value its key held during that group (checked against everything queued for that key in the
// group). arg's bit 7 picks the read pool.
func FuzzActorModel(f *testing.F) {
	f.Add([]byte{0, 1, 2, 1, 3, 0})
	f.Add([]byte{0, 1, 0, 1, 1, 1, 2, 1, 3, 0, 2, 1})
	f.Add([]byte{0, 0x81, 2, 0x81, 1, 1, 3, 0, 0, 2, 0, 3, 2, 2})
	f.Add([]byte{0, 1, 0, 2, 4, 1, 0, 3, 4, 4, 3, 0, 4, 0x41, 0, 5, 3, 0})
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
			insert bool // fails iff key exists
		}
		var group []pending
		held := map[int64][]int64{} // key → every value it may hold during the current group (-1 = absent)
		val := func(k int64) int64 {
			if v, ok := model[k]; ok {
				return v
			}
			return -1
		}
		flush := func(step int) {
			c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, p := range group {
				res := p.req.Wait(c)
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
				case res.Err != nil:
					// Its own error: only an insert may fail, and only if its key existed at some point of the
					// group (possibly through a batch-mate that was later rolled back with it).
					if !p.insert || !slices.ContainsFunc(held[p.key], func(x int64) bool { return x != -1 && x != p.v }) {
						t.Fatalf("step %d: %s %v failed: %v", step, p.req.SQL, p.req.Args, res.Err)
					}
				case p.insert && val(p.key) != -1:
					t.Fatalf("step %d: insert of existing key %d succeeded", step, p.key)
				case p.v == -1:
					delete(model, p.key)
				default:
					model[p.key] = p.v
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
			kind, arg := script[i]%5, script[i+1]
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
			}
		}
		flush(len(script) / 2)
	})
}
