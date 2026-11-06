package vactor_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/vactortest"
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
//	kind 230..241: WriteBack k whose publish gets no reply but stays in flight (durabletest LandLate, arg+1 s);
//	         it lands on a later clock step if the manifest has not moved
//
// Crash + restart restarts like agentd: the new host starts empty, then every key we own is re-stamped at our
// epoch before it serves (owner.Reconcile's reopen stamp), so a publish the crashed host still had in flight
// must never land afterwards.
//
// Model per key: the published contents (what a fresh activation must see), whether it is warm, its warm
// contents, whether it has unpublished writes, whether it is fenced, and our epoch. Lost replies
// (LosePublishReply, ApplyThenStale, LoseUploadReply) must never fail a write-back or a stamp, and a stamp
// that fails must never leave the key stale: a later write-back still succeeds (OS-02, RI-14). The check after every step: the
// store's manifest names exactly the model's published contents (nothing acknowledged and written back is
// ever lost or replaced by something older), every warm key's local file holds the model's warm contents,
// cold keys have no local file, and the open-database count matches. Owner-side retention (K = 2) runs on
// every publish, and the generation cache (OS-10) is on with a budget that forces evictions; after a GC op
// without faults, each key holds at most K+1 generations older than its
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
	f.Add([]byte{1, 0, 230, 0, 7, 4, 2, 0, 0, 0})                                                 // publish in flight lands late
	f.Add([]byte{1, 0, 5, 0, 1, 0, 230, 0, 3, 0, 7, 4, 0, 0, 1, 0, 5, 0})                         // in flight, crash, restamp: zombie refused
	f.Add([]byte{1, 0, 6, 8, 5, 0, 6, 0, 2, 0, 0, 0})                                             // lost upload reply: adopted by hash
	f.Add([]byte{1, 0, 6, 3, 5, 0, 6, 4, 10, 0, 6, 0, 5, 0})                                      // ambiguous publish, stamp with failing read
	f.Add([]byte{1, 0, 6, 2, 5, 0, 6, 0, 1, 0, 5, 0, 242, 0, 3, 0, 0, 0})                         // orphan, publish, GC, crash, reactivate
	f.Add([]byte{1, 0, 5, 0, 1, 0, 5, 0, 1, 0, 5, 0, 1, 0, 5, 0, 3, 0, 1, 0, 5, 0, 242, 0, 0, 0}) // retention, restart, GC
	f.Fuzz(func(t *testing.T, script []byte) {
		faults := []durabletest.Faults{{}, {FailUpload: true}, {FailPublish: true}, {LosePublishReply: true},
			{FailRead: true}, {Down: true}, {FailFetch: true}, {ApplyThenStale: true}, {LoseUploadReply: true}}
		keys := []string{"t/k0", "t/k1", "t/k2", "t/k3"}
		s, o, clk := durabletest.NewStore(), &vactortest.Opener{}, newClock()
		var server atomic.Int64 // the store's clock: strictly increasing, one second per write
		s.UseClock(clk)         // in-flight publishes land on the clock
		s.SetClock(func() time.Time { return time.Unix(0, server.Add(int64(time.Second))) })
		gc := durable.GCPolicy{Keep: 2, Grace: time.Nanosecond}
		opts := vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: s, CreateMissing: true, Clock: clk, Workers: 2,
			GenCacheBytes: 12, // the generation cache on (OS-10), small enough to evict: values are "s<step>"
			FailBackoff:   -1, // tombstones are tested on their own
			Policy:        vactor.Policy{WriteBackCommits: 2, IdleAfter: 30 * time.Second, MaxActive: 3}}
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
		// pend counts this warm key's publishes the host keeps pending (one that failed without the re-read
		// showing another writer: under FailPublish, or a warm stamp under Down); with vactor.MaxPending of them
		// it settles them by a re-read before it sends more, and fails if that re-read fails.
		pend := map[string]int{}
		own := map[string]uint64{} // our ownership epoch per key
		// inflight: contents of this key's publishes still in flight (LandLate); one may land on a clock step.
		inflight := map[string][]string{}
		// landed: one of them landed unseen, so the host's next publish gets 412 and needs a re-read to adopt it.
		landed := map[string]bool{}
		// tags: the LandTag of each in-flight publish, parallel to inflight. The host forgets in-flight publishes
		// when it settles maxPending of them by a re-read that shows none applied (settlePending); the model
		// assumes those never land (dropped), an open finding: knowledge/fencing/2026-11-06-reopen-restamp.md.
		tags := map[string][]uint64{}
		var dropMu sync.Mutex
		dropped := map[uint64]bool{}
		s.SetLandIf(func(tag uint64) bool { dropMu.Lock(); defer dropMu.Unlock(); return !dropped[tag] })
		forget := func(k string) {
			dropMu.Lock()
			for _, g := range tags[k] {
				dropped[g] = true
			}
			dropMu.Unlock()
			inflight[k], tags[k] = nil, nil
		}
		fault := durabletest.Faults{}
		settled := func(k string) bool { return pend[k] < vactor.MaxPending || !fault.FailRead && !fault.Down }
		// writeBackFails predicts a write-back of a dirty, unfenced key under the current fault.
		writeBackFails := func(k string) bool {
			return fault.FailUpload || fault.FailPublish || fault.Down || !settled(k) || landed[k] && fault.FailRead
		}
		activationFails := func(k string) bool {
			return fenced[k] || fault.FailRead || fault.Down || (fault.FailFetch && published[k] != "")
		}
		// wbOutcome checks the result of a write-back of a warm dirty key and reports whether it published.
		wbOutcome := func(step int, what, k string, err error) bool {
			if pend[k] >= vactor.MaxPending && settled(k) && !fault.FailUpload {
				pend[k] = 0 // the host re-read first: nothing of ours applied, and it forgets them
				forget(k)
			}
			if (fault.FailPublish || landed[k] && fault.FailRead && !fault.FailUpload && !fault.Down) && !fenced[k] && !known[k] {
				defer func() { pend[k]++ }() // published nothing, re-read showed our version: kept pending
			}
			switch {
			case known[k]:
				if !errors.Is(err, durable.ErrStale) {
					t.Fatalf("step %d: %s(%s) known stale: %v, want ErrStale", step, what, k, err)
				}
			case writeBackFails(k):
				if err == nil {
					t.Fatalf("step %d: %s(%s) succeeded under %+v", step, what, k, fault)
				}
			case fenced[k] && fault.FailRead:
				// 412 and no re-read: our own write (a pending one, or a re-send) may have caused it, so the
				// outcome is unknown, never ErrStale (guide R4); the host marks it stale once a re-read works.
				if err == nil || errors.Is(err, durable.ErrStale) {
					t.Fatalf("step %d: %s(%s) fenced, re-read failing: %v, want an unknown outcome", step, what, k, err)
				}
			case fenced[k]:
				if !errors.Is(err, durable.ErrStale) {
					t.Fatalf("step %d: %s(%s) fenced: %v, want ErrStale", step, what, k, err)
				}
				known[k] = true
			default:
				if err != nil {
					t.Fatalf("step %d: %s(%s): %v (fault %+v)", step, what, k, err, fault)
				}
				pend[k], inflight[k], tags[k], landed[k] = 0, nil, nil, false // the manifest moved: an in-flight publish cannot land now
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
			case script[i] >= 230 && script[i] < 242:
				if fault != (durabletest.Faults{}) || !warm[key] || !dirty[key] || fenced[key] || known[key] {
					break
				}
				if pend[key] >= vactor.MaxPending {
					pend[key] = 0 // settled by a re-read first
					forget(key)
				}
				s.SetFaults(durabletest.Faults{LandLate: (time.Duration(arg) + 1) * time.Second, LandTag: uint64(step) + 1})
				_, err := h.WriteBack(ctx, key)
				s.SetFaults(fault)
				if err == nil || errors.Is(err, durable.ErrStale) {
					t.Fatalf("step %d: WriteBack(%s) with its publish in flight: %v, want an unknown outcome", step, key, err)
				}
				pend[key]++
				inflight[key], tags[key] = append(inflight[key], local[key]), append(tags[key], uint64(step)+1)
			case script[i] >= 242:
				if len(inflight[keys[0]])+len(inflight[keys[1]])+len(inflight[keys[2]])+len(inflight[keys[3]]) > 0 {
					break // a publish in flight protects its upload (the sweeper's grace window, longer than 1 ns)
				}
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
				// A generation-cache hit needs no fetch, so FailFetch alone cannot fail it; the check after the
				// step still demands the published contents.
				cacheHit := err == nil && res.Act != nil && res.Act.LocalHit && !fenced[key] && !fault.FailRead && !fault.Down
				if warm[key] {
					if err != nil {
						t.Fatalf("step %d: Get(%s) of a warm key: %v", step, key, err)
					}
				} else if activationFails(key) && !cacheHit {
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
					warm[key], local[key], dirty[key], pend[key] = true, published[key], false, 0
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
				s.SetFaults(durabletest.Faults{}) // the restart's reconcile retries until its stamps succeed
				for _, k := range keys {
					warm[k], dirty[k], known[k], pend[k], inflight[k], tags[k], landed[k] = false, false, false, 0, nil, nil, false // unpublished writes are lost: the RPO
					if fenced[k] {
						continue // reconcile supersedes it: no stamp
					}
					var gen uint64
					if m, ok := s.Manifest(k); ok {
						gen = m.Generation
					}
					if _, err := h.Stamp(ctx, k, own[k], gen); err != nil {
						t.Fatalf("step %d: reopen stamp of %s: %v", step, k, err)
					}
				}
				s.SetFaults(fault)
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
				for _, k := range keys { // a publish still in flight may have landed: it holds one of these
					if got := publishedOf(t, s, k); got != published[k] && slices.Contains(inflight[k], got) {
						published[k], inflight[k], tags[k], landed[k] = got, nil, nil, true
					}
				}
			case k == 8:
				s.Bump(key, own[key]+1)
				fenced[key], inflight[key], tags[key], landed[key] = true, nil, nil, false
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
				switch {
				case !warm[key] || err == nil:
					pend[key] = 0 // a cold stamp keeps nothing pending; a stamp that applied settled the rest
				case !settled(key): // the settling re-read failed: nothing sent
				case fault.Down || fault.FailPublish:
					if pend[key] >= vactor.MaxPending {
						pend[key] = 0
					}
					pend[key]++
				}
				if err != nil && !fault.Down && !fault.FailPublish && !fault.FailRead {
					t.Fatalf("step %d: Stamp(%s, %d, %d) under %+v: %v", step, key, ep, gen, fault, err)
				}
				if m, ok := s.Manifest(key); !(ok && m.Epoch == ep) && err == nil {
					t.Fatalf("step %d: Stamp(%s, %d) succeeded but stored %+v", step, key, ep, m)
				}
				// The authority granted us ep before we stamped, applied or not: another owner (Bump) only ever
				// gets a higher epoch. (Bumping to ep itself would forge a manifest identical to our pending stamp.)
				own[key] = ep
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

// publishedOf is what key's manifest names now ("" for none or generation 0).
func publishedOf(t *testing.T, s *durabletest.Store, key string) string {
	m, ok := s.Manifest(key)
	if !ok || m.Generation == 0 {
		return ""
	}
	b, ok := s.Object(m.Object)
	if !ok {
		t.Fatalf("manifest of %s names missing %s", key, m.Object)
	}
	return string(b)
}
