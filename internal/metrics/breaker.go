package metrics

import (
	"maps"
	"slices"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
)

// Breakers exposes each remote dependency's breaker: its state (one series per state, 1 for the current one)
// and its transitions (Release It C4, O1). Shared by agentd and controld so dashboards read both alike.
func Breakers(reg *Registry, deps map[string]*breaker.Breaker) {
	reg.Add("tsdb_breaker_state", "Circuit breaker state per dependency (1 = current).", Gauge, func(e *Emit) {
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
	reg.Add("tsdb_breaker_transitions_total", "Circuit breaker state changes per dependency.", Counter, func(e *Emit) {
		for _, dep := range slices.Sorted(maps.Keys(deps)) {
			e.Value(Labels{"dep", dep}, float64(deps[dep].Transitions()))
		}
	})
}
