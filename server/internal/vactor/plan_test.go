package vactor_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/vactor"
)

var now0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func cand(key string, usedAgo time.Duration, bytes int64, dirty bool) vactor.Candidate {
	return vactor.Candidate{Key: key, LastUsed: now0.Add(-usedAgo), LastWriteBack: now0, Bytes: bytes, Dirty: dirty}
}

func TestPolicyValidate(t *testing.T) {
	if err := (vactor.Policy{}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []vactor.Policy{{WriteBackEvery: -1}, {IdleAfter: -1}, {MaxActive: -1}, {MaxBytes: -1}} {
		if p.Validate() == nil {
			t.Errorf("%+v accepted", p)
		}
	}
}

func TestPlanTick(t *testing.T) {
	stale := cand("s", time.Hour, 10, true)
	stale.Stale = true
	staleClean := cand("sc", time.Hour, 10, false)
	staleClean.Stale = true
	commits := func(c vactor.Candidate, n uint64) vactor.Candidate { c.Commits = n; return c }
	wbAgo := func(c vactor.Candidate, d time.Duration) vactor.Candidate { c.LastWriteBack = now0.Add(-d); return c }
	for _, tc := range []struct {
		name  string
		p     vactor.Policy
		cands []vactor.Candidate
		pass  []vactor.Eviction
		wb    []string
	}{
		{"empty policy does nothing", vactor.Policy{}, []vactor.Candidate{cand("a", time.Hour, 1, true)}, nil, nil},
		{"no candidates", vactor.Policy{IdleAfter: time.Second, MaxActive: 1}, nil, nil, nil},
		{"idle on point", vactor.Policy{IdleAfter: time.Minute}, []vactor.Candidate{cand("a", time.Minute, 1, false)},
			[]vactor.Eviction{{"a", vactor.ReasonIdle}}, nil},
		{"idle off point", vactor.Policy{IdleAfter: time.Minute}, []vactor.Candidate{cand("a", time.Minute-1, 1, false)}, nil, nil},
		{"count budget on point keeps all", vactor.Policy{MaxActive: 2},
			[]vactor.Candidate{cand("a", 3, 1, false), cand("b", 2, 1, false)}, nil, nil},
		{"count budget +1 evicts the LRU", vactor.Policy{MaxActive: 2},
			[]vactor.Candidate{cand("b", 2, 1, false), cand("a", 3, 1, false), cand("c", 1, 1, false)},
			[]vactor.Eviction{{"a", vactor.ReasonBudget}}, nil},
		{"LRU ties broken by key", vactor.Policy{MaxActive: 1},
			[]vactor.Candidate{cand("b", 5, 1, false), cand("a", 5, 1, false)}, []vactor.Eviction{{"a", vactor.ReasonBudget}}, nil},
		{"byte budget on point", vactor.Policy{MaxBytes: 10}, []vactor.Candidate{cand("a", 2, 6, false), cand("b", 1, 4, false)}, nil, nil},
		{"byte budget +1 evicts until under", vactor.Policy{MaxBytes: 9},
			[]vactor.Candidate{cand("a", 2, 6, false), cand("b", 1, 4, false)}, []vactor.Eviction{{"a", vactor.ReasonBudget}}, nil},
		{"idle evictions count toward the budget", vactor.Policy{IdleAfter: time.Minute, MaxActive: 1},
			[]vactor.Candidate{cand("a", time.Hour, 1, false), cand("b", 1, 1, false)}, []vactor.Eviction{{"a", vactor.ReasonIdle}}, nil},
		{"dirty stale is never planned, even idle and over budget", vactor.Policy{IdleAfter: time.Second, MaxActive: 1, WriteBackCommits: 1},
			[]vactor.Candidate{commits(stale, 5), cand("b", 0, 1, false)}, []vactor.Eviction{{"b", vactor.ReasonBudget}}, nil},
		{"clean stale may be passivated", vactor.Policy{IdleAfter: time.Second}, []vactor.Candidate{staleClean},
			[]vactor.Eviction{{"sc", vactor.ReasonIdle}}, nil},
		{"commit trigger on point", vactor.Policy{WriteBackCommits: 3}, []vactor.Candidate{commits(cand("a", 0, 1, true), 3)}, nil, []string{"a"}},
		{"commit trigger off point", vactor.Policy{WriteBackCommits: 3}, []vactor.Candidate{commits(cand("a", 0, 1, true), 2)}, nil, nil},
		{"interval on point", vactor.Policy{WriteBackEvery: time.Minute}, []vactor.Candidate{wbAgo(cand("a", 0, 1, true), time.Minute)}, nil, []string{"a"}},
		{"interval off point", vactor.Policy{WriteBackEvery: time.Minute}, []vactor.Candidate{wbAgo(cand("a", 0, 1, true), time.Minute-1)}, nil, nil},
		{"clean is never written back", vactor.Policy{WriteBackCommits: 1, WriteBackEvery: time.Nanosecond},
			[]vactor.Candidate{commits(wbAgo(cand("a", 0, 1, false), time.Hour), 9)}, nil, nil},
		{"passivated keys are not also written back", vactor.Policy{IdleAfter: time.Minute, WriteBackCommits: 1},
			[]vactor.Candidate{commits(cand("a", time.Hour, 1, true), 5), commits(cand("b", 0, 1, true), 5)},
			[]vactor.Eviction{{"a", vactor.ReasonIdle}}, []string{"b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := vactor.PlanTick(now0, tc.cands, tc.p)
			if !slices.Equal(p.Passivate, tc.pass) || !slices.Equal(p.WriteBack, tc.wb) {
				t.Fatalf("plan = %+v, want passivate %v write back %v", p, tc.pass, tc.wb)
			}
		})
	}
}

// FuzzPlanTick checks PlanTick's postconditions over arbitrary candidates and policies.
func FuzzPlanTick(f *testing.F) {
	f.Add([]byte{3, 2, 100, 5, 10, 1, 20, 2, 30, 3})
	f.Add([]byte{0, 0, 0, 0, 1, 255, 3, 7, 9, 200, 4})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			t.Skip()
		}
		p := vactor.Policy{MaxActive: int(b[0] % 6), MaxBytes: int64(b[1]) * 4, IdleAfter: time.Duration(b[2]%8) * time.Second,
			WriteBackCommits: uint64(b[3] % 4), WriteBackEvery: time.Duration(b[3]/64) * time.Second}
		var cands []vactor.Candidate
		for i := 4; i+1 < len(b); i += 2 {
			c := vactor.Candidate{Key: fmt.Sprint("k", i), LastUsed: now0.Add(-time.Duration(b[i]%16) * time.Second),
				LastWriteBack: now0.Add(-time.Duration(b[i+1]%8) * time.Second), Bytes: int64(b[i+1]),
				Commits: uint64(b[i] % 5), Dirty: b[i]&16 != 0, Stale: b[i]&32 != 0}
			cands = append(cands, c)
		}
		plan := vactor.PlanTick(now0, cands, p) // structural postconditions are asserted inside
		gone := map[string]bool{}
		for _, e := range plan.Passivate {
			gone[e.Key] = true
		}
		count, bytes, unplannable := 0, int64(0), 0
		var oldestKept, newestBudget time.Time
		for _, c := range cands {
			idle := p.IdleAfter > 0 && now0.Sub(c.LastUsed) >= p.IdleAfter
			if idle && !(c.Dirty && c.Stale) && !gone[c.Key] {
				t.Fatalf("idle %+v not passivated", c)
			}
			if gone[c.Key] {
				continue
			}
			count++
			bytes += c.Bytes
			if c.Dirty && c.Stale {
				unplannable++
			} else if oldestKept.IsZero() || c.LastUsed.Before(oldestKept) {
				oldestKept = c.LastUsed
			}
			due := c.Dirty && !c.Stale && ((p.WriteBackEvery > 0 && now0.Sub(c.LastWriteBack) >= p.WriteBackEvery) ||
				(p.WriteBackCommits > 0 && c.Commits >= p.WriteBackCommits))
			if due != slices.Contains(plan.WriteBack, c.Key) {
				t.Fatalf("%+v: due=%v but planned=%v", c, due, !due)
			}
		}
		for _, e := range plan.Passivate {
			if e.Reason == vactor.ReasonBudget {
				for _, c := range cands {
					if c.Key == e.Key && c.LastUsed.After(newestBudget) {
						newestBudget = c.LastUsed
					}
				}
			}
		}
		if count > unplannable {
			if (p.MaxActive > 0 && count > p.MaxActive) || (p.MaxBytes > 0 && bytes > p.MaxBytes) {
				// still over budget: only allowed if every remaining candidate is unplannable
				t.Fatalf("over budget after plan: count %d bytes %d policy %+v (unplannable %d)", count, bytes, p, unplannable)
			}
		}
		if !newestBudget.IsZero() && !oldestKept.IsZero() && oldestKept.Before(newestBudget) {
			t.Fatalf("budget evicted a key used at %v but kept one used at %v (not LRU)", newestBudget, oldestKept)
		}
	})
}
