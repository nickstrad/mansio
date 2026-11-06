package vactor

import (
	"cmp"
	"slices"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/errs"
)

// Policy says when warm databases are written back and when they are passivated. Zero fields are off.
type Policy struct {
	// WriteBackEvery writes back a dirty database once this long has passed since its last write-back (or
	// activation). Bounds the data a crash can lose (RPO) in time, plus one maintenance tick.
	WriteBackEvery time.Duration
	// WriteBackCommits writes back a dirty database once it has this many commits since its last write-back.
	// Bounds the RPO in commits, plus the commits of one maintenance tick.
	WriteBackCommits uint64
	// IdleAfter passivates a database not used (Get) for this long.
	IdleAfter time.Duration
	// MaxActive and MaxBytes are the warm budget: when over, passivate least recently used first.
	MaxActive int
	MaxBytes  int64
}

// Validate checks a policy at the edge.
func (p Policy) Validate() error {
	if p.WriteBackEvery < 0 || p.IdleAfter < 0 || p.MaxActive < 0 || p.MaxBytes < 0 {
		return errs.Wrap(module, nil, "invalid policy %+v: durations and budgets must be >= 0", p)
	}
	return nil
}

// Candidate is one warm database as the planner sees it.
type Candidate struct {
	Key           string
	LastUsed      time.Time // last Get
	LastWriteBack time.Time // last successful write-back, or activation
	Bytes         int64     // local file size (with WAL)
	Commits       uint64    // commits since the last write-back
	Dirty         bool      // writes since the last published generation
	Stale         bool      // a write-back hit ErrStale: a higher epoch owns the key; only Discard drops it
}

// Reason says why a database is passivated.
type Reason string

const (
	ReasonIdle   Reason = "idle"
	ReasonBudget Reason = "budget"
)

// Eviction is one planned passivation.
type Eviction struct {
	Key    string
	Reason Reason
}

// Plan is what one maintenance tick does. Passivations write back first.
type Plan struct {
	Passivate []Eviction
	WriteBack []string // keys, sorted
}

// PlanTick decides one maintenance tick. Pure: same input, same plan.
//
//	pre:  p was validated; keys in cands are unique
//	post: no key appears twice in the plan, and every planned key is a candidate
//	post: a dirty stale candidate is never planned (writing it back cannot succeed; passivating would lose it)
//	post: idle ⇔ now - LastUsed >= IdleAfter (IdleAfter > 0)
//	post: after the plan, warm count <= MaxActive and warm bytes <= MaxBytes, unless only unplannable
//	      (dirty stale) candidates remain
//	post: budget evictions take least recently used first (ties by key)
//	post: WriteBack holds exactly the dirty, non-stale, not-passivated candidates that are due
func PlanTick(now time.Time, cands []Candidate, p Policy) Plan {
	var plan Plan
	var keep []Candidate
	count, bytes := 0, int64(0)
	for _, c := range cands {
		count++
		bytes += c.Bytes
		switch {
		case c.Dirty && c.Stale:
			// stays warm; fencing decides (Discard)
		case p.IdleAfter > 0 && now.Sub(c.LastUsed) >= p.IdleAfter:
			plan.Passivate = append(plan.Passivate, Eviction{c.Key, ReasonIdle})
			count--
			bytes -= c.Bytes
			continue
		}
		keep = append(keep, c)
	}
	over := func() bool { return (p.MaxActive > 0 && count > p.MaxActive) || (p.MaxBytes > 0 && bytes > p.MaxBytes) }
	if over() {
		lru := slices.Clone(keep)
		slices.SortFunc(lru, func(a, b Candidate) int {
			return cmp.Or(a.LastUsed.Compare(b.LastUsed), cmp.Compare(a.Key, b.Key))
		})
		evicted := map[string]bool{}
		for _, c := range lru {
			if !over() {
				break
			}
			if c.Dirty && c.Stale {
				continue
			}
			plan.Passivate = append(plan.Passivate, Eviction{c.Key, ReasonBudget})
			evicted[c.Key] = true
			count--
			bytes -= c.Bytes
		}
		keep = slices.DeleteFunc(keep, func(c Candidate) bool { return evicted[c.Key] })
	}
	for _, c := range keep {
		if !c.Dirty || c.Stale {
			continue
		}
		due := (p.WriteBackEvery > 0 && now.Sub(c.LastWriteBack) >= p.WriteBackEvery) ||
			(p.WriteBackCommits > 0 && c.Commits >= p.WriteBackCommits)
		if due {
			plan.WriteBack = append(plan.WriteBack, c.Key)
		}
	}
	slices.Sort(plan.WriteBack)
	if assert.Enabled {
		checkPlan(cands, plan)
	}
	return plan
}

// checkPlan asserts the plan's structural postconditions (O(n); assertion builds only).
func checkPlan(cands []Candidate, plan Plan) {
	byKey := make(map[string]Candidate, len(cands))
	for _, c := range cands {
		byKey[c.Key] = c
	}
	seen := map[string]bool{}
	for _, e := range plan.Passivate {
		c, ok := byKey[e.Key]
		assert.That(ok && !seen[e.Key], "PlanTick: passivation of %q is not a candidate or is planned twice", e.Key)
		assert.That(!(c.Dirty && c.Stale), "PlanTick: planned to passivate dirty stale %q", e.Key)
		seen[e.Key] = true
	}
	for _, k := range plan.WriteBack {
		c, ok := byKey[k]
		assert.That(ok && !seen[k] && c.Dirty && !c.Stale, "PlanTick: bad write-back of %q", k)
		seen[k] = true
	}
}
