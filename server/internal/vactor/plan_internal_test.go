package vactor

import (
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/assert"
)

// Every assertion in checkPlan must be able to fire.
func TestCheckPlanAssertionsFire(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out")
	}
	cands := []Candidate{{Key: "a", Dirty: true}, {Key: "s", Dirty: true, Stale: true}}
	for name, bad := range map[string]Plan{
		"unknown passivation":    {Passivate: []Eviction{{"x", ReasonIdle}}},
		"twice":                  {Passivate: []Eviction{{"a", ReasonIdle}, {"a", ReasonBudget}}},
		"dirty stale":            {Passivate: []Eviction{{"s", ReasonIdle}}},
		"write-back + passivate": {Passivate: []Eviction{{"a", ReasonIdle}}, WriteBack: []string{"a"}},
		"write-back of stale":    {WriteBack: []string{"s"}},
	} {
		func() {
			defer func() {
				if p, _ := recover().(string); !strings.Contains(p, "PlanTick") {
					t.Errorf("%s: recovered %q, want a PlanTick assertion", name, p)
				}
			}()
			checkPlan(cands, bad)
		}()
	}
}
