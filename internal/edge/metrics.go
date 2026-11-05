package edge

import (
	"maps"
	"slices"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/metrics"
)

// Register exposes the edge on reg (Release It O1): its own answers by outcome class and code, the agents'
// answers by status class, latency and in-flight (point="edge"), the ticket filter, and per agent its in-flight
// count and breaker (the shared tsdb_breaker_* families, dep="agent:<vm>"). Catalog:
// knowledge/front/2026-11-05-edge.md.
func (e *Edge) Register(reg *metrics.Registry) {
	type agentSnap struct {
		inFlight    int
		state       breaker.State
		transitions uint64
	}
	var agents map[string]agentSnap
	reg.OnScrape(func() {
		now := time.Now()
		e.mu.RLock()
		agents = make(map[string]agentSnap, len(e.ups))
		for vm, u := range e.ups {
			agents[vm] = agentSnap{len(u.slots), u.b.State(now), u.b.Transitions()}
		}
		e.mu.RUnlock()
	})
	vms := func() []string { return slices.Sorted(maps.Keys(agents)) }
	reg.Add("tsdb_requests_total", "Answers per integration point by outcome class and wire code (edge: answers the edge made itself).",
		metrics.Counter, func(m *metrics.Emit) {
			for i, c := range front.Codes {
				class := front.OutcomeClass(c, c == front.Expired) // the edge's expired answers are all ambiguous
				m.Value(metrics.Labels{"point", "edge", "class", class, "code", string(c)}, float64(e.st.answers[i].Load()))
			}
		})
	reg.Add("tsdb_edge_agent_answers_total", "Agent answers relayed by the edge, by HTTP status class.", metrics.Counter, func(m *metrics.Emit) {
		for i := 1; i <= 5; i++ {
			m.Value(metrics.Labels{"status", string(rune('0'+i)) + "xx"}, float64(e.st.agent[i].Load()))
		}
	})
	reg.Add("tsdb_request_latency_seconds", "Answer latency per integration point.", metrics.Histogram, func(m *metrics.Emit) {
		b, c, s := e.latency.Snapshot()
		m.Histogram(metrics.Labels{"point", "edge"}, b, c, s)
	})
	reg.Add("tsdb_in_flight", "Requests in flight per integration point.", metrics.Gauge, func(m *metrics.Emit) {
		m.Value(metrics.Labels{"point", "edge"}, float64(e.st.inFlight.Load()))
	})
	reg.Add("tsdb_in_flight_high_water", "Most requests in flight at once since start, per integration point.", metrics.Gauge, func(m *metrics.Emit) {
		m.Value(metrics.Labels{"point", "edge"}, float64(e.st.inFlightHigh.Load()))
	})
	reg.Add("tsdb_edge_tickets_total", "Ticket checks at the edge: cache hit, miss (full verify), invalid, rate_limited (source out of tokens), "+
		"verify_busy (no verify slot).", metrics.Counter, func(m *metrics.Emit) {
		for _, v := range []struct {
			result string
			n      int64
		}{{"hit", e.st.hits.Load()}, {"miss", e.st.misses.Load()}, {"invalid", e.st.invalid.Load()},
			{"rate_limited", e.st.limited.Load()}, {"verify_busy", e.st.verifyBusy.Load()}} {
			m.Value(metrics.Labels{"result", v.result}, float64(v.n))
		}
	})
	reg.Add("tsdb_edge_refusals_total", "Requests the edge refused on the way to an agent: unknown_vm, bulkhead_full, breaker_open, "+
		"transport_error.", metrics.Counter, func(m *metrics.Emit) {
		for _, v := range []struct {
			reason string
			n      int64
		}{{"unknown_vm", e.st.unknownVM.Load()}, {"bulkhead_full", e.st.bulkheadFull.Load()}, {"breaker_open", e.st.open.Load()},
			{"transport_error", e.st.transportErrs.Load()}} {
			m.Value(metrics.Labels{"reason", v.reason}, float64(v.n))
		}
	})
	reg.Add("tsdb_edge_state", "Edge caches: sources with a rate bucket, verified tickets remembered.", metrics.Gauge, func(m *metrics.Emit) {
		m.Value(metrics.Labels{"stat", "sources"}, float64(e.cfg.Limiter.Sources()))
		m.Value(metrics.Labels{"stat", "tickets_cached"}, float64(e.cfg.Gate.Len()))
	})
	reg.Add("tsdb_edge_agent_in_flight", "Requests in flight to each agent (its bulkhead in use).", metrics.Gauge, func(m *metrics.Emit) {
		for _, vm := range vms() {
			m.Value(metrics.Labels{"vm", vm}, float64(agents[vm].inFlight))
		}
	})
	reg.Add("tsdb_breaker_state", "Circuit breaker state per dependency (1 = current).", metrics.Gauge, func(m *metrics.Emit) {
		for _, vm := range vms() {
			for _, s := range []breaker.State{breaker.Closed, breaker.Open, breaker.HalfOpen} {
				v := 0.0
				if s == agents[vm].state {
					v = 1
				}
				m.Value(metrics.Labels{"dep", "agent:" + vm, "state", s.String()}, v)
			}
		}
	})
	reg.Add("tsdb_breaker_transitions_total", "Circuit breaker state changes per dependency.", metrics.Counter, func(m *metrics.Emit) {
		for _, vm := range vms() {
			m.Value(metrics.Labels{"dep", "agent:" + vm}, float64(agents[vm].transitions))
		}
	})
}
