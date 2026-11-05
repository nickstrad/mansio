package vactor_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
	"github.com/nickstrad/ts-db-go/internal/vactor"
	"github.com/nickstrad/ts-db-go/internal/vactor/vactortest"
)

// FuzzDurableHostModel runs a script of operations against a durable-mode Host (fake store and databases)
// and a plain model, and compares them after every step.
//
// Script: pairs (kind, arg) over 4 keys:
//
//	kind%11  0 Get k   1 Write k   2 Passivate k   3 Crash + restart   4 Tick   5 WriteBack k
//	         6 store fault := faults[arg%9] (one at a time)   7 clock += arg seconds
//	         8 Bump k (another owner raises the stored epoch: k is fenced for us)   9 Discard k
//	         10 Stamp k at our epoch + 1 (we stay the owner; skipped once k is fenced)
//	kind >= 242 (outside the %11 range, so older corpus entries keep their meaning):
//	         GC: the orphan sweeper (durable.Sweep, K = 2, a grace of 1 ns) over every key
//
// Model per key: the published contents (what a fresh activation must see), whether it is warm, its warm
// contents, whether it has unpublished writes, whether it is fenced, and our epoch. Lost replies
// (LosePublishReply, ApplyThenStale, LoseUploadReply) must never fail a write-back or a stamp, and a stamp
// that fails must never leave the key stale: a later write-back still succeeds (OS-02, RI-14). The check after every step: the
// store's manifest names exactly the model's published contents (nothing acknowledged and written back is
// ever lost or replaced by something older), every warm key's local file holds the model's warm contents,
// cold keys have no local file, and the open-database count matches. Owner-side retention (K = 2) runs on
// every publish; after a GC op without faults, each key holds at most K+1 generations older than its
// manifest's last rewrite (OS-06): the current one and K below it.
func FuzzDurableHostModel(f *testing.F) {
	f.Add([]byte{1, 0, 2, 0, 0, 0})                                                               // write, passivate, reactivate
	f.Add([]byte{1, 0, 6, 1, 2, 0, 6, 0, 2, 0, 0, 0})                                             // upload fails: stays warm; then ok
	f.Add([]byte{1, 0, 6, 2, 5, 0, 6, 0, 3, 0, 0, 0})                                             // publish fails (orphan), crash, reactivate
	f.Add([]byte{1, 0, 5, 0, 1, 0, 3, 0, 0, 0})                                                   // write-back, more writes, crash: last published
	f.Add([]byte{1, 0, 6, 3, 5, 0, 6, 0, 1, 0, 2, 0, 0, 0})                                       // lost publish reply
	f.Add([]byte{1, 0, 1, 1, 1, 0, 4, 0, 7, 40, 4, 0, 0, 1})                                      // tick: commit trigger, idle eviction
	f.Add([]byte{1, 0, 8, 0, 2, 0, 5, 0, 9, 0, 0, 0})                                             // fenced: stale write-back, discard, cold get fails
	f.Add([]byte{1, 0, 1, 1, 1, 2, 1, 3, 4, 0, 6, 5, 4, 0})                                       // budget eviction, store down
	f.Add([]byte{1, 0, 6, 7, 10, 0, 6, 0, 1, 0, 5, 0})                                            // warm stamp, reply lost as 412, write back
	f.Add([]byte{6, 7, 10, 1, 6, 0, 1, 1, 2, 1, 0, 1})                                            // cold stamp, reply lost as 412, activate, write
	f.Add([]byte{1, 0, 6, 8, 5, 0, 6, 0, 2, 0, 0, 0})                                             // lost upload reply: adopted by hash
	f.Add([]byte{1, 0, 6, 3, 5, 0, 6, 4, 10, 0, 6, 0, 5, 0})                                      // ambiguous publish, stamp with failing read
	f.Add([]byte{1, 0, 6, 2, 5, 0, 6, 0, 1, 0, 5, 0, 242, 0, 3, 0, 0, 0})                         // orphan, publish, GC, crash, reactivate
	f.Add([]byte{1, 0, 5, 0, 1, 0, 5, 0, 1, 0, 5, 0, 1, 0, 5, 0, 3, 0, 1, 0, 5, 0, 242, 0, 0, 0}) // retention, restart, GC
	f.Fuzz(func(t *testing.T, script []byte) {
		faults := []durabletest.Faults{{}, {FailUpload: true}, {FailPublish: true}, {LosePublishReply: true},
			{FailRead: true}, {Down: true}, {FailFetch: true}, {ApplyThenStale: true}, {LoseUploadReply: true}}
		keys := []string{"t/k0", "t/k1", "t/k2", "t/k3"}
		s, o, clk := durabletest.NewStore(), &vactortest.Opener{}, &clock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
		var server atomic.Int64 // the store's clock: strictly increasing, one second per write
		s.SetClock(func() time.Time { return time.Unix(0, server.Add(int64(time.Second))) })
		gc := durable.GCPolicy{Keep: 2, Grace: time.Nanosecond}
		opts := vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: s, CreateMissing: true, Now: clk.Now, Workers: 2,
			FailBackoff: -1, // tombstones are tested on their own
			Policy:      vactor.Policy{WriteBackCommits: 2, IdleAfter: 30 * time.Second, MaxActive: 3}}
		newHost := func() *vactor.Host {
			h, err := vactor.NewHost(nil, o, opts)
			if err != nil {
				t.Fatal(err)
			}
			return h
		}
		h := newHost()
		defer func() { s.SetFaults(durabletest.Faults{}); h.Close() }()

		published, local := map[string]string{}, map[string]string{}
		warm, dirty, fenced := map[string]bool{}, map[string]bool{}, map[string]bool{}
		known := map[string]bool{} // the host has seen ErrStale for this warm key (and could re-read)
		own := map[string]uint64{} // our ownership epoch per key
		fault := durabletest.Faults{}
		// writeBackFails predicts a write-back of a dirty, unfenced key under the current fault.
		writeBackFails := func() bool { return fault.FailUpload || fault.FailPublish || fault.Down }
		activationFails := func(k string) bool {
			return fenced[k] || fault.FailRead || fault.Down || (fault.FailFetch && published[k] != "")
		}
		// wbOutcome checks the result of a write-back of a warm dirty key and reports whether it published.
		wbOutcome := func(step int, what, k string, err error) bool {
			switch {
			case known[k]:
				if !errors.Is(err, durable.ErrStale) {
					t.Fatalf("step %d: %s(%s) known stale: %v, want ErrStale", step, what, k, err)
				}
			case writeBackFails():
				if err == nil {
					t.Fatalf("step %d: %s(%s) succeeded under %+v", step, what, k, fault)
				}
			case fenced[k]:
				if !errors.Is(err, durable.ErrStale) {
					t.Fatalf("step %d: %s(%s) fenced: %v, want ErrStale", step, what, k, err)
				}
				known[k] = !fault.FailRead // the host marks it stale only once a re-read shows the other writer
			default:
				if err != nil {
					t.Fatalf("step %d: %s(%s): %v (fault %+v)", step, what, k, err, fault)
				}
				return true
			}
			return false
		}
		nWarm := func() int {
			n := 0
			for _, k := range keys {
				if warm[k] {
					n++
				}
			}
			return n
		}

		for i := 0; i+1 < len(script); i += 2 {
			step, arg := i/2, script[i+1]
			key := keys[int(arg)%len(keys)]
			switch k := script[i] % 11; {
			case script[i] >= 242:
				for _, gk := range keys {
					_, err := durable.Sweep(ctx, s, gk, time.Unix(0, server.Load()).Add(time.Hour), gc, false)
					if err != nil && fault == (durabletest.Faults{}) {
						t.Fatalf("step %d: Sweep(%s): %v", step, gk, err)
					}
					if err == nil {
						checkSwept(t, step, s, gk, gc.Keep)
					}
				}
			case k == 0, k == 1:
				res, err := h.GetAt(ctx, key, own[key])
				if warm[key] {
					if err != nil {
						t.Fatalf("step %d: Get(%s) of a warm key: %v", step, key, err)
					}
				} else if activationFails(key) {
					if err == nil {
						t.Fatalf("step %d: Get(%s) succeeded; fenced=%v fault=%+v", step, key, fenced[key], fault)
					}
					if fenced[key] && !fault.FailRead && !fault.Down && !errors.Is(err, durable.ErrStale) {
						t.Fatalf("step %d: Get(%s) of a fenced key: %v, want ErrStale", step, key, err)
					}
					break
				} else {
					if err != nil {
						t.Fatalf("step %d: cold Get(%s): %v (fault %+v)", step, key, err, fault)
					}
					warm[key], local[key], dirty[key] = true, published[key], false
				}
				if script[i]%11 == 1 {
					v := fmt.Sprintf("s%d", step)
					if err := res.DB.Query(ctx, vactortest.Put(v)); err != nil {
						t.Fatalf("step %d: write %s: %v", step, key, err)
					}
					local[key], dirty[key] = v, true
				}
			case k == 2:
				err := h.Passivate(ctx, key)
				if !warm[key] || !dirty[key] {
					if err != nil {
						t.Fatalf("step %d: Passivate(%s) clean/cold: %v", step, key, err)
					}
					warm[key] = false
				} else if wbOutcome(step, "Passivate", key, err) {
					published[key], warm[key], dirty[key] = local[key], false, false
				}
			case k == 3:
				vactor.Crash(h)
				h = newHost()
				for _, k := range keys {
					warm[k], dirty[k], known[k] = false, false, false // unpublished writes are lost: allowed, that is the RPO
				}
			case k == 4:
				rep, err := h.Tick(ctx)
				if err != nil {
					t.Fatalf("step %d: Tick: %v", step, err)
				}
				for _, ev := range rep.Plan.Passivate {
					k := ev.Key
					if !warm[k] || (dirty[k] && known[k]) {
						t.Fatalf("step %d: Tick passivated %s: warm=%v dirty=%v known stale=%v", step, k, warm[k], dirty[k], known[k])
					}
					if !dirty[k] {
						if rep.Errors[k] != nil {
							t.Fatalf("step %d: Tick clean passivation of %s: %v", step, k, rep.Errors[k])
						}
						warm[k] = false
					} else if wbOutcome(step, "Tick passivation", k, rep.Errors[k]) {
						published[k], warm[k], dirty[k] = local[k], false, false
					}
				}
				for _, k := range rep.Plan.WriteBack {
					if !warm[k] || !dirty[k] || known[k] {
						t.Fatalf("step %d: Tick wrote back %s: warm=%v dirty=%v known stale=%v", step, k, warm[k], dirty[k], known[k])
					}
					if wbOutcome(step, "Tick write-back", k, rep.Errors[k]) {
						published[k], dirty[k] = local[k], false
					}
				}
			case k == 5:
				res, err := h.WriteBack(ctx, key)
				if !warm[key] || !dirty[key] {
					if err != nil || res.Wrote {
						t.Fatalf("step %d: WriteBack(%s) clean/cold = %+v, %v", step, key, res, err)
					}
				} else if wbOutcome(step, "WriteBack", key, err) {
					if !res.Wrote {
						t.Fatalf("step %d: WriteBack(%s) = %+v without error", step, key, res)
					}
					published[key], dirty[key] = local[key], false
				}
			case k == 6:
				fault = faults[int(arg)%len(faults)]
				s.SetFaults(fault)
			case k == 7:
				clk.Add(time.Duration(arg) * time.Second)
			case k == 8:
				s.Bump(key, own[key]+1)
				fenced[key] = true
			case k == 9:
				lost := h.Discard(key)
				if lost != (warm[key] && dirty[key]) {
					t.Fatalf("step %d: Discard(%s) lost=%v, model warm=%v dirty=%v", step, key, lost, warm[key], dirty[key])
				}
				warm[key], dirty[key], known[key] = false, false, false
			case k == 10:
				if fenced[key] {
					break // not the owner: it would not stamp
				}
				var gen uint64
				if m, ok := s.Manifest(key); ok {
					gen = m.Generation
				}
				ep := own[key] + 1
				_, err := h.Stamp(ctx, key, ep, gen)
				if err != nil && !fault.Down && !fault.FailPublish && !fault.FailRead {
					t.Fatalf("step %d: Stamp(%s, %d, %d) under %+v: %v", step, key, ep, gen, fault, err)
				}
				if m, ok := s.Manifest(key); ok && m.Epoch == ep {
					own[key] = ep
				} else if err == nil {
					t.Fatalf("step %d: Stamp(%s, %d) succeeded but stored %+v", step, key, ep, m)
				}
			}

			// Compare after every step.
			for _, k := range keys {
				m, ok := s.Manifest(k)
				got := ""
				if ok && m.Generation > 0 {
					b, ok := s.Object(m.Object)
					if !ok {
						t.Fatalf("step %d: manifest of %s names missing %s", step, k, m.Object)
					}
					got = string(b)
				}
				if got != published[k] {
					t.Fatalf("step %d: %s published %q, model %q", step, k, got, published[k])
				}
				b, err := os.ReadFile(h.LocalPath(k))
				if warm[k] && (err != nil || string(b) != local[k]) {
					t.Fatalf("step %d: warm %s local %q (%v), model %q", step, k, b, err, local[k])
				}
				if !warm[k] && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("step %d: cold %s has a local file (%v)", step, k, err)
				}
			}
			if h.Active() != nWarm() {
				t.Fatalf("step %d: Active() = %d, model %d", step, h.Active(), nWarm())
			}
		}
	})
}

// checkSwept is the GC invariant after a sweep of key: of the generations uploaded before the manifest's last
// rewrite, at most keep+1 remain (the current one and keep below it), and the current one is among them.
func checkSwept(t *testing.T, step int, s *durabletest.Store, key string, keep int) {
	t.Helper()
	l, err := s.ListKey(ctx, key)
	if err != nil || l.Manifest == nil {
		return
	}
	m, _ := s.Manifest(key)
	older := 0
	for _, o := range l.Generations {
		if o.Modified.Before(l.Manifest.Modified) {
			older++
		}
	}
	if older > keep+1 {
		t.Fatalf("step %d: after GC %s holds %d generations older than its manifest, want ≤ %d", step, key, older, keep+1)
	}
	if _, ok := s.Object(m.Object); m.Generation > 0 && !ok {
		t.Fatalf("step %d: GC deleted %s's current generation %s", step, key, m.Object)
	}
}
