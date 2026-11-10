package main

import (
	"flag"
	"fmt"
	"testing"

	"github.com/nickstrad/mansio/server/internal/capacity"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancyhttp"
)

var updateCapacity = flag.Bool("update", false, "rewrite controld's section of docs/capacity.md from the config defaults")

// capacityRows is controld's part of the capacity table, from the config defaults.
func capacityRows() []capacity.Row {
	c := defaults()
	l := c.Locator
	q := func(queue string) string { return fmt.Sprintf("`mansio_queue_*{queue=%q}`", queue) }
	limit := func(queue string) string {
		return fmt.Sprintf("`controld limit -queue %s -n N` (`POST /v1/limits/%s`)", queue, queue)
	}
	return []capacity.Row{
		{Queue: "resolve", Protects: "controld itself: every resolve, most of them answered from the placement cache",
			Capacity: fmt.Sprintf("%d slots (`locator.concurrency`), no queue", l.Concurrency), Workers: "the slots",
			Policy: "`unavailable` 503 + `Retry-After` at once", Deadline: fmt.Sprintf("`locator.timeout` %v", l.Timeout),
			Metric: q("resolve") + ", `controld_locator_*`", Lever: capacity.NoLever,
			FirstLook: "cache hit ratio (`controld_locator_hits_total` against `loads_total`) and whether the listener is connected (`controld_locator_listening`)"},
		{Queue: "resolve_load", Protects: "Postgres: placement and API-key reads on a cache miss, and the agents' schema reads",
			Capacity: fmt.Sprintf("%d slots (`locator.load_concurrency`), no queue; callers of a load in flight share it", l.LoadConcurrency), Workers: "the slots",
			Policy:   "resolve: `shed` 503 + `Retry-After` at once; schema read: `unavailable` 503",
			Deadline: fmt.Sprintf("a load: `locator.timeout` %v", l.Timeout), Metric: q("resolve_load") + ", `controld_pg_pool_*`", Lever: limit("resolve_load"),
			FirstLook: "Postgres latency and the placement breaker; `controld_pg_pool_acquires_waited_total` must stay flat (the budget keeps callers below the pool)"},
		{Queue: "resolve_create", Protects: "Postgres and an agent's lifecycle slots (create on first use)",
			Capacity: fmt.Sprintf("%d slots (`locator.create_concurrency`), no queue", l.CreateConcurrency), Workers: "the slots",
			Policy: "`shed` 503 + `Retry-After` at once; nothing created", Deadline: "the resolve's; a create it could not finish is recorded and resumed",
			Metric: q("resolve_create"), Lever: limit("resolve_create"),
			FirstLook: "agent health (`controld_agent_up`) and the agents' `lifecycle` queue; past `locator.max_dbs` the answer is `quota_exceeded`, not this"},
		{Queue: "controld_api", Protects: "Postgres and the agents (the admin API: create, move, suspend, resume, drop, restore)",
			Capacity: fmt.Sprintf("%d slots (`api_concurrency`), no queue", c.APIConcurrency), Workers: "the slots",
			Policy: "`unavailable` 503 + `Retry-After` at once", Deadline: fmt.Sprintf("`api_timeout` %v; a flow it did not finish is recorded and resumed", c.APITimeout),
			Metric: q("controld_api"), Lever: limit("controld_api"),
			FirstLook: "what holds the slots: a restore moves many databases; an agent that answers slowly (`mansio_breaker_state{dep=\"agent:*\"}`)"},
		{Queue: "pg pool", Protects: "Postgres's connection limit",
			Capacity: fmt.Sprintf("%d connections per controld (`pg.max_conns`); callers are budgeted below it: %d + %d + %d", c.PG.MaxConns, l.LoadConcurrency,
				l.CreateConcurrency, c.APIConcurrency),
			Workers:  "the connections",
			Policy:   "a caller beyond the pool waits inside pgxpool until its call timeout; the budget exists so that this does not happen",
			Deadline: "2.5 s per call", Metric: "`controld_pg_pool_conns`, `controld_pg_pool_acquires_waited_total`, `controld_pg_pool_acquire_wait_seconds_total`",
			Lever:     "lower the three pools above with `controld limit`; the pool size itself is config",
			FirstLook: "N controld instances × (`max_conns` + 1) against Postgres's `max_connections`"},
		{Queue: "tenancy_admin", Protects: "the tenancy store (the tenant admin API, when the `tenancy` section is set)",
			Capacity: fmt.Sprintf("%d slots, no queue (no config key)", tenancyhttp.DefaultConcurrency), Workers: "the slots",
			Policy: "503 `shed` + `Retry-After` at once", Deadline: "10 s per call", Metric: q("tenancy_admin"), Lever: limit("tenancy_admin"),
			FirstLook: "Postgres latency; these calls share the pool's spare connections"},
		{Queue: "tenancy_auth", Protects: "the tenancy store and the password-hash CPU (sign-in and sign-up)",
			Capacity: fmt.Sprintf("%d slots, no queue; before them 1 sign-in per second per source, burst 10", tenancyhttp.DefaultConcurrency), Workers: "the slots",
			Policy: "503 at once (429 + `Retry-After` from the per-source limit)", Deadline: "10 s per call", Metric: q("tenancy_auth"), Lever: limit("tenancy_auth"),
			FirstLook: "a sign-in flood or slow password hashing"},
	}
}

// The controld section of docs/capacity.md is what the config defaults generate (run with -update after a change).
func TestCapacityTable(t *testing.T) {
	sec := capacity.Section("controld", "the operator of the control plane", capacityRows())
	if err := capacity.Check("../../docs/capacity.md", "controld", sec, *updateCapacity); err != nil {
		t.Fatal(err)
	}
}
