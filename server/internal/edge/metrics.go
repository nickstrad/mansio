package edge

import (
	"maps"
	"slices"

	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/metrics"
)

// Register exposes the edge on reg (Release It O1): its own answers by outcome class and code, the agents'
// answers by status class, latency and in-flight (point="edge"), the ticket filter, and per agent its in-flight
// count and breaker (the shared mansio_breaker_* families, dep="agent:<vm>"). Catalog:
// docs/notes/front/2026-11-05-edge.md.
func (e *Edge) Register(reg *metrics.Registry) {
	var agents map[string]int // vm → requests in flight
	reg.OnScrape(func() {
		e.mu.RLock()
		agents = make(map[string]int, len(e.ups))
		for vm, u := range e.ups {
			agents[vm] = u.slots.InUse()
		}
		e.mu.RUnlock()
	})
	vms := func() []string { return slices.Sorted(maps.Keys(agents)) }
	reg.Add("mansio_requests_total", "Answers per integration point by outcome class and wire code (edge: answers the edge made itself).",
		metrics.Counter, func(m *metrics.Emit) {
			amb := e.st.ambiguous.Load()
			for i, c := range front.Codes {
				n := e.st.answers[i].Load()
				if c == front.Expired {
					m.Value(metrics.Labels{"point", "edge", "class", front.OutcomeClass(c, true), "code", string(c)}, float64(amb))
					n -= amb
				}
				m.Value(metrics.Labels{"point", "edge", "class", front.OutcomeClass(c, false), "code", string(c)}, float64(n))
			}
		})
	reg.Add("mansio_edge_agent_answers_total", "Agent answers relayed by the edge, by HTTP status class.", metrics.Counter, func(m *metrics.Emit) {
		for i := 1; i <= 5; i++ {
			m.Value(metrics.Labels{"status", string(rune('0'+i)) + "xx"}, float64(e.st.agent[i].Load()))
		}
	})
	reg.Add("mansio_request_latency_seconds", "Answer latency per integration point.", metrics.Histogram, func(m *metrics.Emit) {
		b, c, s := e.latency.Snapshot()
		m.Histogram(metrics.Labels{"point", "edge"}, b, c, s)
	})
	reg.Add("mansio_in_flight", "Requests in flight per integration point.", metrics.Gauge, func(m *metrics.Emit) {
		m.Value(metrics.Labels{"point", "edge"}, float64(e.st.inFlight.Load()))
	})
	reg.Add("mansio_in_flight_high_water", "Most requests in flight at once since start, per integration point.", metrics.Gauge, func(m *metrics.Emit) {
		m.Value(metrics.Labels{"point", "edge"}, float64(e.st.inFlightHigh.Load()))
	})
	reg.Add("mansio_edge_tickets_total", "Ticket checks at the edge: cache hit, miss (full verify), invalid, rate_limited (source out of tokens), "+
		"verify_busy (no verify slot).", metrics.Counter, func(m *metrics.Emit) {
		for _, v := range []struct {
			result string
			n      int64
		}{{"hit", e.st.hits.Load()}, {"miss", e.st.misses.Load()}, {"invalid", e.st.invalid.Load()},
			{"rate_limited", e.st.limited.Load()}, {"verify_busy", e.verify.Shed()}} {
			m.Value(metrics.Labels{"result", v.result}, float64(v.n))
		}
	})
	reg.Add("mansio_edge_refusals_total", "Requests the edge refused on the way to an agent: unknown_vm, bulkhead_full, breaker_open, "+
		"transport_error, max_in_flight.", metrics.Counter, func(m *metrics.Emit) {
		for _, v := range []struct {
			reason string
			n      int64
		}{{"unknown_vm", e.st.unknownVM.Load()}, {"bulkhead_full", e.st.bulkheadFull.Load()}, {"breaker_open", e.st.open.Load()},
			{"transport_error", e.st.transportErrs.Load()}, {"max_in_flight", e.st.overCap.Load()}} {
			m.Value(metrics.Labels{"reason", v.reason}, float64(v.n))
		}
	})
	reg.Add("mansio_edge_state", "Edge caches: sources with a rate bucket, verified tickets remembered.", metrics.Gauge, func(m *metrics.Emit) {
		m.Value(metrics.Labels{"stat", "sources"}, float64(e.cfg.Limiter.Sources()))
		m.Value(metrics.Labels{"stat", "tickets_cached"}, float64(e.cfg.Gate.Len()))
	})
	reg.Add("mansio_edge_agent_in_flight", "Requests in flight to each agent (its bulkhead in use).", metrics.Gauge, func(m *metrics.Emit) {
		for _, vm := range vms() {
			m.Value(metrics.Labels{"vm", vm}, float64(agents[vm]))
		}
	})
	metrics.Queues(reg, func() []metrics.QueueStat {
		full := func(n int64) map[string]int64 { return map[string]int64{metrics.ReasonFull: n} }
		busiest := 0
		e.mu.RLock()
		limit := e.agentInFlight
		for _, u := range e.ups {
			busiest = max(busiest, u.slots.InUse())
		}
		e.mu.RUnlock()
		return []metrics.QueueStat{
			{Queue: "edge_in_flight", Capacity: e.cfg.MaxInFlight, Depth: int(e.st.inFlight.Load()), Rejected: full(e.st.overCap.Load())},
			// Per agent: the capacity is one agent's, the depth the busiest agent's (each: mansio_edge_agent_in_flight).
			{Queue: "edge_agent", Capacity: limit, Depth: busiest, Rejected: full(e.st.bulkheadFull.Load())},
			e.verify.Stat("edge_verify"),
		}
	})
	metrics.BreakersFunc(reg, func() map[string]*breaker.Breaker {
		e.mu.RLock()
		defer e.mu.RUnlock()
		m := make(map[string]*breaker.Breaker, len(e.ups))
		for vm, u := range e.ups {
			m["agent:"+vm] = u.b
		}
		return m
	})
}
