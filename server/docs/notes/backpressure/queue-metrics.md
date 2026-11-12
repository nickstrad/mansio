# Per-queue metrics

The `mansio_queue_*` families that show, for every bounded queue and slot pool, its depth, capacity and
refusals.

Adds to the metric catalog in [the front metrics note](../front/metrics.md). Code:
`internal/metrics/queue.go`, `internal/actor/queues.go`, `cmd/agentd/metrics.go`, `cmd/controld/serve.go`,
`internal/edge/metrics.go`. Tested by `TestQueuesExposition`, `TestOverloadIsVisiblePerQueue` and
`TestMetricCatalogAfterFaultScript`.

| Family                        | Type      | Labels                                                                                                              | Source                                                                                                    |
| ----------------------------- | --------- | ------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `mansio_queue_depth`          | gauge     | `queue`                                                                                                             | waiting now (a slot pool: slots in use); absent for `mailbox` and `read`, whose bound is per database     |
| `mansio_queue_capacity`       | gauge     | `queue` (fixed list below)                                                                                          | each queue's or slot pool's limit, to read its depth against                                              |
| `mansio_queue_rejected_total` | counter   | `queue`, `reason` = full, would_expire ([admission](admission-sheds-what-would-expire.md)), failure_backoff (fetch) | refusals counted where they happen                                                                        |
| `mansio_queue_wait_seconds`   | histogram | `queue` = admission, mailbox                                                                                        | enqueue → pickup; admission counts every request it did not shed (0 when it ran at once)                  |
| `mansio_vactor_dirty`         | gauge     |                                                                                                                     | warm databases with unpublished writes at the last tick (beside `mansio_vactor_oldest_dirty_age_seconds`) |
| `mansio_actor_queue_depth`    | gauge     | `stat` = total, max, p99                                                                                            | now also in durable mode: the open databases (`vactor.Host.OpenDBs`) and the canary                       |

`metrics.Queues` (`internal/metrics/queue.go`) is one set of families for every bounded queue and slot pool,
so "which boundary is refusing" is one query. The `queue` label is a fixed list, never a tenant, database or
path:

| `queue`                         | Process                       | Boundary                                                                  | Capacity                            |
| ------------------------------- | ----------------------------- | ------------------------------------------------------------------------- | ----------------------------------- |
| `admission`                     | agentd                        | `front.Admission`                                                         | the VM queue (`admission.vm_queue`) |
| `mailbox`                       | agentd                        | every actor's mailbox (`actor.Queues`, shared by the process's actors)    | one actor's `actor.mailbox_size`    |
| `read`                          | agentd                        | every actor's read-pool queue                                             | one actor's `actor.mailbox_size`    |
| `fetch`                         | agentd, durable               | the fetch bulkhead's queue; `failure_backoff` counts tombstone fail-fasts | `MaxFetchQueue`                     |
| `lifecycle`                     | agentd, durable               | `/v1/lifecycle/*` slots                                                   | `lifecycle_concurrency`             |
| `admin`                         | agentd                        | `/admin/*` slots on the admin listener                                    | 4                                   |
| `tenancy_admin`, `tenancy_auth` | agentd, controld (tenancy on) | tenant admin API and sign-in slots                                        | 16 each                             |
| `resolve`                       | controld                      | resolve and schema reads                                                  | `locator.concurrency`               |
| `controld_api`                  | controld                      | the admin API                                                             | `api_concurrency`                   |

controld also exports its Postgres pools (`pool` = placement, placement-replica), because callers beyond
`pg.max_conns` wait inside pgxpool: `controld_pg_pool_acquires_total`,
`controld_pg_pool_acquires_waited_total`, `controld_pg_pool_acquires_canceled_total`,
`controld_pg_pool_acquire_wait_seconds_total` and `controld_pg_pool_conns{state="in_use|idle|max"}`.

The [edge](edge-refuses-before-body.md) reports `edge_in_flight` (`max_in_flight`), `edge_agent` (one agent's
`agent_in_flight`) and `edge_verify` (`verify_slots`) in the same families.

The full list of queues with their capacities, policies and levers is the generated table in
[`docs/capacity.md`](../../capacity.md), which supersedes the list above where they differ (it adds
`resolve_load`, `resolve_create`, `verify` and `credential_place`).
