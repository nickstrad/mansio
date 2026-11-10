package main

import (
	"flag"
	"fmt"
	"testing"

	"github.com/nickstrad/mansio/server/internal/capacity"
)

var updateCapacity = flag.Bool("update", false, "rewrite the edge's section of docs/capacity.md from the config defaults")

// capacityRows is the edge's part of the capacity table, outermost first, from the config defaults.
func capacityRows() []capacity.Row {
	c := defaults()
	const agentInFlight = 4096 // edge.New's default for agent_in_flight 0; max_in_flight defaults to twice it
	q := func(queue string) string { return fmt.Sprintf("`mansio_queue_*{queue=%q}`", queue) }
	deadline := fmt.Sprintf("the request's deadline (`default_timeout` %v, at most `max_timeout` %v) plus `reply_grace` %v", c.DefaultTimeout, c.MaxTimeout, c.ReplyGrace)
	return []capacity.Row{
		{Queue: "edge_in_flight", Protects: "the edge's memory: request bodies in flight (`max_in_flight` × `max_body`)",
			Capacity: fmt.Sprintf("%d requests (`max_in_flight`, default 2 × `agent_in_flight`); bodies up to %d MiB (`max_body`)", 2*agentInFlight, c.MaxBody>>20),
			Workers:  "one goroutine per request", Policy: "`shed` 503 + `Retry-After` before the body is read", Deadline: deadline,
			Metric: q("edge_in_flight") + ", `mansio_in_flight{point=\"edge\"}`", Lever: capacity.NoLever,
			FirstLook: "whether the agents are slow (requests pile up at the edge): `mansio_edge_agent_in_flight` and the agents' admission"},
		{Queue: "edge_source_rate", Protects: "the verify CPU, from one source sending many new tickets",
			Capacity: fmt.Sprintf("%g new tickets per second per source, burst %g (`rate_limit`); %d sources tracked", c.RateLimit.PerSecond, c.RateLimit.Burst, c.RateLimit.MaxSources),
			Workers:  "none (a token bucket)", Policy: "`shed` 503 with `Retry-After` = the time to the next token", Deadline: "none",
			Metric: "`mansio_edge_tickets_total{result=\"rate_limited\"}`", Lever: capacity.NoLever,
			FirstLook: "one client without a ticket cache, or a flood of invalid tickets (`result=\"invalid\"`)"},
		{Queue: "edge_verify", Protects: "the edge's CPU (full ticket verifies)",
			Capacity: "2 × GOMAXPROCS slots (`verify_slots`), no queue", Workers: "the slots", Policy: "`shed` 503 at once", Deadline: "none (about 26 µs each)",
			Metric: q("edge_verify") + ", `mansio_edge_tickets_total`", Lever: capacity.NoLever,
			FirstLook: "the ticket cache's hit ratio (`result=\"hit\"` against `miss`): a cold cache after a restart refills in seconds"},
		{Queue: "edge_agent", Protects: "each agent (requests in flight to it)",
			Capacity: fmt.Sprintf("%d per agent (`agent_in_flight`), no queue", agentInFlight), Workers: "the slots",
			Policy:   "`shed` 503 before the body is read; an agent that cannot be reached opens its breaker: `unavailable` 503 with the cool-down as `Retry-After`",
			Deadline: deadline, Metric: q("edge_agent") + ", `mansio_edge_agent_in_flight`, `mansio_edge_refusals_total`",
			Lever:     "`POST /admin/agent:limit?n=N` on `admin_listen` (loopback)",
			FirstLook: "that agent's own admission and latency; lower the limit to take load off agents that are struggling"},
	}
}

// The edge section of docs/capacity.md is what the config defaults generate (run with -update after a change).
func TestCapacityTable(t *testing.T) {
	sec := capacity.Section("edge", "the operator of the edge", capacityRows())
	if err := capacity.Check("../../docs/capacity.md", "edge", sec, *updateCapacity); err != nil {
		t.Fatal(err)
	}
}
