package metrics

import (
	"maps"
	"slices"
	"time"

	"github.com/nickstrad/mansio/server/internal/breaker"
)

// Breakers exposes each remote dependency's breaker: its state (one series per state, 1 for the current one)
// and its transitions (Release It C4, O1). Shared by agentd, controld and the edge so dashboards read them alike.
func Breakers(reg *Registry, deps map[string]*breaker.Breaker) {
	BreakersFunc(reg, func() map[string]*breaker.Breaker { return deps })
}

// BreakersFunc is Breakers for a set that changes while serving (the edge's agents): deps is called once per
// scrape and must return a map the caller no longer writes.
func BreakersFunc(reg *Registry, deps func() map[string]*breaker.Breaker) {
	var snap map[string]*breaker.Breaker
	reg.OnScrape(func() { snap = deps() })
	reg.Add("mansio_breaker_state", "Circuit breaker state per dependency (1 = current).", Gauge, func(e *Emit) {
		deps := snap
		now := time.Now()
		for _, dep := range slices.Sorted(maps.Keys(deps)) {
			cur := deps[dep].State(now)
			for _, s := range []breaker.State{breaker.Closed, breaker.Open, breaker.HalfOpen} {
				v := 0.0
				if s == cur {
					v = 1
				}
				e.Value(Labels{"dep", dep, "state", s.String()}, v)
			}
		}
	})
	reg.Add("mansio_breaker_transitions_total", "Circuit breaker state changes per dependency.", Counter, func(e *Emit) {
		deps := snap
		for _, dep := range slices.Sorted(maps.Keys(deps)) {
			e.Value(Labels{"dep", dep}, float64(deps[dep].Transitions()))
		}
	})
}
