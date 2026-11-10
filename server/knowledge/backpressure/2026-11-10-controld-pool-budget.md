# controld: callers budgeted below the Postgres pool (B4, 2026-11-10)

Code: `internal/control/locator/locator.go` (`Config.LoadSlots`, `ErrBusy`), `cmd/controld/config.go`,
`cmd/controld/schemas.go` (`resolveApp`), `cmd/controld/serve.go`. Tests: `TestLoadSlotsBoundStoreReads`
(overload scenario), `TestCreateOnFirstUseIsBoundedByItsOwnSlots`, `TestConfig` (budget on and off points).

Before: 256 resolve slots + 16 admin slots sat in front of a 20-connection pool, so on a cold cache or a slow
Postgres the callers beyond 20 waited inside pgxpool for up to the call timeout and then timed out.

Now resolve admission has two gates:

| Gate | Config | Default | What takes a slot | Full → |
|---|---|---|---|---|
| wide | `locator.concurrency` | 256 | every resolve (a cache hit needs no Postgres) | `unavailable` 503, as before |
| narrow | `locator.load_concurrency` | 10 | a placement or API-key load the locator starts on a miss, and an agent's schema read | resolve: `shed` 503 + `Retry-After`; schema read: `unavailable` 503 |
| create | `locator.create_concurrency` | 2 | a create on first use (registry check, count, create, agent claim) | `shed` 503 + `Retry-After` |

Callers that join a load already in flight take no slot. The narrow gate is taken without waiting, so the
refusal is immediate (measured in the test: 8 of 10 cold keys refused while the store was still blocked).

Budget per process, enforced when the config is parsed: `locator.load_concurrency` +
`locator.create_concurrency` + `api_concurrency` must be below `pg.max_conns`. Defaults: 10 + 2 + 16 = 28 of
32; the other four are for the heartbeat, the resumer and tenancy. `pg.max_conns` went from 20 to 32 instead of
cutting `api_concurrency` to 4 as the plan's example did: `TestGate` creates databases 8 at a time through the
admin API and admin calls spend most of their time waiting on agents, not on Postgres. For N controld
instances keep N × (`max_conns` + 1 listener connection) under Postgres's `max_connections`.

Metrics: `mansio_queue_capacity` / `mansio_queue_rejected_total` for `queue="resolve_load"` and
`"resolve_create"`, and the `controld_pg_pool_*` families from B1 to see that the pool itself no longer queues
(`controld_pg_pool_acquires_waited_total` should stay flat).
