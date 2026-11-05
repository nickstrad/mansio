package vactor_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/vactor"
	"github.com/nickstrad/ts-db-go/internal/vactor/vactortest"
)

// FuzzHostModel runs a script of operations against a Host (with fakes) and a plain model, and compares
// them after every step.
//
// Script: byte 0 picks KeepLocal (odd = keep). Then each op is two bytes (kind, arg) over 4 keys:
//
//	kind%5  0 Get k   1 Passivate k   2 toggle fetch Down   3 toggle open failure   4 PassivateAll
//
// Model: which keys are active, which have a local file, and how many fetches each key has needed.
// Ops are sequential; concurrency is covered by the -race tests in host_test.go.
func FuzzHostModel(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0})       // get, get again, passivate, get
	f.Add([]byte{1, 0, 0, 1, 0, 0, 0})             // keep local: get, passivate, get = local hit
	f.Add([]byte{0, 2, 0, 0, 1, 2, 0, 0, 1})       // fetch down, get fails, up, get
	f.Add([]byte{0, 3, 0, 0, 2, 3, 0, 0, 2})       // open fails, get fails, ok, get
	f.Add([]byte{1, 0, 0, 0, 1, 0, 2, 4, 0, 0, 1}) // several keys, passivate all
	f.Add([]byte{1, 0, 0, 1, 0, 2, 0, 0, 0, 2, 0}) // keep local, then fetch down: local hit still works
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) == 0 {
			t.Skip()
		}
		keep := script[0]%2 == 1
		fetcher, opener := vactortest.NewFetcher(), &vactortest.Opener{}
		keys := []string{"t/k0", "t/k1", "t/k2", "t/k3"}
		for _, k := range keys {
			fetcher.Put(k, []byte(k))
		}
		h, err := vactor.NewHost(fetcher, opener, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), KeepLocal: keep})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()

		active, onDisk, fetches := map[string]bool{}, map[string]bool{}, map[string]int{}
		down, failOpen := false, false
		for i := 1; i+1 < len(script); i += 2 {
			step, key := i/2, keys[int(script[i+1])%len(keys)]
			switch script[i] % 5 {
			case 0:
				res, err := h.Get(ctx, key)
				localHit := keep && onDisk[key]
				if !active[key] && !localHit {
					fetches[key]++ // a cold Get always tries one fetch, even if it fails
				}
				switch {
				case active[key]:
					if err != nil || res.Act != nil {
						t.Fatalf("step %d: Get(%s) on an active key = %+v, %v; want the open db, no activation", step, key, res, err)
					}
				case down && !localHit, failOpen:
					if err == nil {
						t.Fatalf("step %d: Get(%s) succeeded with down=%v failOpen=%v", step, key, down, failOpen)
					}
					if down && !localHit {
						onDisk[key] = false // the fetch failed before writing anything
					} else {
						onDisk[key] = keep // fetched or local, then open failed: kept only with KeepLocal
					}
				default:
					if err != nil || res.Act == nil || res.Act.LocalHit != localHit {
						t.Fatalf("step %d: cold Get(%s) = %+v, %v; want an activation with LocalHit=%v", step, key, res.Act, err, localHit)
					}
					active[key], onDisk[key] = true, true
				}
			case 1:
				h.Passivate(ctx, key)
				if active[key] {
					onDisk[key] = keep
				}
				active[key] = false
			case 2:
				down = !down
				fetcher.SetDown(down)
			case 3:
				failOpen = !failOpen
				opener.Set(failOpen, false)
			case 4:
				h.PassivateAll(2)
				for k := range active {
					if active[k] {
						onDisk[k] = keep
					}
					active[k] = false
				}
			}
			checkModel(t, step, h, fetcher, opener, keys, active, onDisk, fetches)
		}
	})
}

func checkModel(t *testing.T, step int, h *vactor.Host, f *vactortest.Fetcher, o *vactortest.Opener,
	keys []string, active, onDisk map[string]bool, fetches map[string]int) {
	t.Helper()
	nActive := 0
	for _, k := range keys {
		if active[k] {
			nActive++
		}
		if got := f.Calls(k); got != fetches[k] {
			t.Fatalf("step %d: fetches(%s) = %d, model %d", step, k, got, fetches[k])
		}
		_, err := os.Stat(h.LocalPath(k))
		if onFile := err == nil; onFile != (active[k] || onDisk[k]) {
			t.Fatalf("step %d: local file %s exists=%v, model active=%v onDisk=%v", step, k, onFile, active[k], onDisk[k])
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("step %d: stat: %v", step, err)
		}
	}
	if h.Active() != nActive {
		t.Fatalf("step %d: Active() = %d, model %d", step, h.Active(), nActive)
	}
	if opened, closed := o.Counts(); opened-closed != nActive {
		t.Fatalf("step %d: open dbs = %d (opened %d closed %d), model %d", step, opened-closed, opened, closed, nActive)
	}
	if s := h.Stats(); s.Fetches != int64(sum(fetches)) {
		t.Fatalf("step %d: Stats.Fetches = %d, model %d", step, s.Fetches, sum(fetches))
	}
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
