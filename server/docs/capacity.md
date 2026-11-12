# Capacity table: every bound on a request's path

What is bounded, what is refused, and which metric proves it, per boundary: one page to review capacity and, for
each queue, a runbook entry an operator can act on when it fills. The three sections between the markers are
**generated from each daemon's config defaults**: a `TestCapacityTable` in `cmd/agentd`, `cmd/controld` and
`cmd/edge` fails when a default and this document disagree. After changing a default or a bound, regenerate:

```sh
go test ./cmd/agentd ./cmd/controld ./cmd/edge -run TestCapacityTable -update
```

How to read a row. **Capacity** is how much may wait (for a slot pool with no queue: how much may run) and the
config key that sets it. **Workers** is what drains it. **When full** is what the caller sees; what a client
library then does by itself is the [retry contract](../../docs/retry-contract.md). **Lives under** is the
deadline that ends work in it. **Metric**: every queue reports `mansio_queue_depth`, `mansio_queue_capacity`
and `mansio_queue_rejected_total{reason}` under its `queue` label (`mansio_queue_wait_seconds` for `admission`
and `mailbox`); alert rules over them are in [`deploy/prometheus/backpressure.rules.yml`](../deploy/prometheus/backpressure.rules.yml).
**Runtime lever** is how an operator changes the bound without a restart; a lever lasts until the process
restarts, when the config file's value applies again.

## One request, outermost bound first

    client ─► edge: edge_in_flight ─► edge_source_rate / edge_verify ─► edge_agent (per agent, breaker)
           ─► agentd: [credential requests only: verify ─► credential_place] ─► admission (db ≤ tenant ≤ VM)
              ─► mailbox (per database) ─► read (per database, with a read pool)
              cold database: fetch ─► S3            warm database, in the background: write_back ─► S3

    client ─► controld: resolve ─► [cache miss: resolve_load ─► Postgres] ─► [first use: resolve_create ─► agent lifecycle]
    operator ─► controld: controld_api ─► Postgres, agent lifecycle

The caps nest: a database's in-flight cap is at most its tenant's, which is at most the VM's; the edge's
per-agent bulkhead is at least the agent's `vm_in_flight` + `vm_queue`, so the agent, not the edge, decides
admission; controld's `resolve_load` + `resolve_create` + `controld_api` stay below `pg.max_conns` (the config
is refused otherwise).

## agentd

<!-- capacity:agentd:begin -->
| Queue              | Protects                                                                                              | Capacity (config)                                                                                                        | Workers                                                                                                                                     | When full                                                                                                                                                          | Lives under                                                                                  | Metric                                                                                                                                          | Runtime lever                                                                                      |
| ------------------ | ----------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| `verify`           | the tenancy store, the schema registry and the placement authority (credential requests on `/v1/ops`) | 64 slots (`verify_slots`), no queue                                                                                      | the slots                                                                                                                                   | `shed` 503 + `Retry-After` at once                                                                                                                                 | the request's deadline (`default_timeout` 2s, at most `max_timeout` 30s)                     | `mansio_queue_*{queue="verify"}`                                                                                                                | `agentctl limit verify N` (`POST /admin/limit?queue=verify&n=N`)                                   |
| `credential_place` | the placement authority (a credential request for a database not active here: claim or create)        | 16 flights (`lifecycle_concurrency`), one per database; creates one at a time                                            | one goroutine per flight                                                                                                                    | a request for another database is `shed` 503 at once; requests for a database in flight wait for it                                                                | the flight: `lifecycle_timeout` 30s; each waiter: its own request deadline                   | `mansio_queue_*{queue="credential_place"}`                                                                                                      | `agentctl limit credential_place N` (`POST /admin/limit?queue=credential_place&n=N`)               |
| `admission`        | the actors and the CPU: statements in flight per database, tenant and VM                              | queued: 256 per database (`admission.db_queue`), 1024 per tenant (`tenant_queue`), 4 × in flight for the VM (`vm_queue`) | in flight: 64 per database (`db_in_flight`), 256 per tenant (`tenant_in_flight`), 4 × GOMAXPROCS × db_in_flight for the VM (`vm_in_flight`) | queues in arrival order; `shed` 503 + `Retry-After` when a queue is full or the request would finish after its deadline; `expired` 504 if it runs out while queued | the request's deadline (`default_timeout` 2s, at most `max_timeout` 30s)                     | `mansio_queue_*{queue="admission"}`, `mansio_admission`                                                                                         | none (config, restart); `agentctl drain` sheds everything                                          |
| `mailbox`          | one database's writer (its SQLite connection)                                                         | 256 per database (`actor.mailbox_size`)                                                                                  | one goroutine per database; a batch commits up to 64 writes together (`actor.max_batch`)                                                    | `shed` 503 at once; a request found past its deadline at pickup is `expired` without running                                                                       | the request's deadline; a statement at most `statement_timeout` 1s                           | `mansio_queue_*{queue="mailbox"}`, `mansio_actor_queue_depth`                                                                                   | none (config, restart)                                                                             |
| `read`             | one database's read connections                                                                       | 256 per database (`actor.mailbox_size`), only with a read pool                                                           | `actor.read_conns` readers per database (default 0: reads share the writer)                                                                 | `shed` 503 at once, so reads never stall the writes behind them                                                                                                    | as the mailbox                                                                               | `mansio_queue_*{queue="read"}`                                                                                                                  | none (config, restart)                                                                             |
| `fetch`            | S3 (cold database fetches) and the local disk and network                                             | 256 activations queued (package default, no config key)                                                                  | 16 fetches or 1 GiB in flight                                                                                                               | `unavailable` 503 + `Retry-After` at once; a database whose last activation failed fails fast for a jittered backoff (`reason="failure_backoff"`)                  | the activation: 30 s plus size / 32 MiB/s                                                    | `mansio_queue_*{queue="fetch"}`, `mansio_vactor{stat="fetch_*"}`                                                                                | `agentctl fetch-limit N` (`POST /admin/fetch:limit?n=N`); `agentctl breaker s3 trip` stops fetches |
| `lifecycle`        | S3 (a fence writes back) and the placement authority, for controld's claim, fence and forget          | 16 slots (`lifecycle_concurrency`), no queue                                                                             | the slots                                                                                                                                   | `unavailable` 503 + `Retry-After` at once                                                                                                                          | `lifecycle_timeout` 30s                                                                      | `mansio_queue_*{queue="lifecycle"}`                                                                                                             | `agentctl limit lifecycle N` (`POST /admin/limit?queue=lifecycle&n=N`)                             |
| `tenancy_admin`    | the tenancy store (the tenant admin API on the query listener)                                        | 16 slots, no queue (no config key)                                                                                       | the slots                                                                                                                                   | 503 `shed` + `Retry-After` at once                                                                                                                                 | 10 s per call; the body within `body_read_timeout`                                           | `mansio_queue_*{queue="tenancy_admin"}`                                                                                                         | `agentctl limit tenancy_admin N` (`POST /admin/limit?queue=tenancy_admin&n=N`)                     |
| `tenancy_auth`     | the tenancy store and the password-hash CPU (sign-in and sign-up)                                     | 16 slots, no queue; before them 1 sign-in per second per source, burst 10                                                | the slots                                                                                                                                   | 503 at once (429 + `Retry-After` from the per-source limit)                                                                                                        | 10 s per call                                                                                | `mansio_queue_*{queue="tenancy_auth"}`                                                                                                          | `agentctl limit tenancy_auth N` (`POST /admin/limit?queue=tenancy_auth&n=N`)                       |
| `admin`            | nothing remote: the operator's own controls on the loopback admin listener                            | 4 slots, no queue (fixed)                                                                                                | the slots                                                                                                                                   | `unavailable` 503 at once                                                                                                                                          | `lifecycle_timeout` 30s                                                                      | `mansio_queue_*{queue="admin"}`                                                                                                                 | none (config, restart)                                                                             |
| `write_back`       | S3 (publishing warm databases)                                                                        | not a queue: every dirty database is a candidate each tick                                                               | 8 write-backs at once (package default)                                                                                                     | writes keep being acknowledged while write-back fails; the unpublished window grows (see the README's durability section)                                          | `maintenance.write_back_timeout` 1m0s per write-back                                         | `mansio_vactor_dirty`, `mansio_vactor_oldest_dirty_age_seconds`, `mansio_vactor_events_total{event="writeback_failure"}`                        | `agentctl breaker s3 trip|reset`; `agentctl backup KEY` forces one                                 |
| `events`           | the event log's disk writer and its volume; the request path never waits on it                        | 8192 records (`event_log.buffer`); on disk 4 GiB (`event_log.max_total_bytes`) and 45 days (`event_log.max_age`)         | one writer goroutine                                                                                                                        | queue full: the newest record is dropped and counted, and the next write records how many; disk over its limits: the oldest files are deleted when a new one opens | none (telemetry); at shutdown the writer gets 2 s and counts what it could not write as lost | `mansio_eventlog_queue_depth`, `mansio_eventlog_records_total{result="dropped|lost"}`, `mansio_eventlog_bytes`, `mansio_eventlog_retired_total` | none (config, restart)                                                                             |

### Runbooks: agentd

#### agentd: `verify`

- Service: agentd
- Queue name: `verify`
- Owner: the operator of the agent VM
- Policy: `shed` 503 + `Retry-After` at once
- Capacity: 64 slots (`verify_slots`), no queue
- Workers: the slots
- Downstream dependency: the tenancy store, the schema registry and the placement authority (credential requests on `/v1/ops`)
- Expected full behavior: `shed` 503 + `Retry-After` at once Work in it lives under: the request's deadline (`default_timeout` 2s, at most `max_timeout` 30s).
- Primary dashboard: `mansio_queue_*{queue="verify"}`
- Rollback or feature flag: `agentctl limit verify N` (`POST /admin/limit?queue=verify&n=N`)
- First look: the tenancy breaker and store latency (`mansio_breaker_state{dep="tenancy"}`), then `credential_place`

#### agentd: `credential_place`

- Service: agentd
- Queue name: `credential_place`
- Owner: the operator of the agent VM
- Policy: a request for another database is `shed` 503 at once; requests for a database in flight wait for it
- Capacity: 16 flights (`lifecycle_concurrency`), one per database; creates one at a time
- Workers: one goroutine per flight
- Downstream dependency: the placement authority (a credential request for a database not active here: claim or create)
- Expected full behavior: a request for another database is `shed` 503 at once; requests for a database in flight wait for it Work in it lives under: the flight: `lifecycle_timeout` 30s; each waiter: its own request deadline.
- Primary dashboard: `mansio_queue_*{queue="credential_place"}`
- Rollback or feature flag: `agentctl limit credential_place N` (`POST /admin/limit?queue=credential_place&n=N`)
- First look: placement store latency and its breaker; a first-use storm shows as refusals here with `verify` calm

#### agentd: `admission`

- Service: agentd
- Queue name: `admission`
- Owner: the operator of the agent VM
- Policy: queues in arrival order; `shed` 503 + `Retry-After` when a queue is full or the request would finish after its deadline; `expired` 504 if it runs out while queued
- Capacity: queued: 256 per database (`admission.db_queue`), 1024 per tenant (`tenant_queue`), 4 × in flight for the VM (`vm_queue`)
- Workers: in flight: 64 per database (`db_in_flight`), 256 per tenant (`tenant_in_flight`), 4 × GOMAXPROCS × db_in_flight for the VM (`vm_in_flight`)
- Downstream dependency: the actors and the CPU: statements in flight per database, tenant and VM
- Expected full behavior: queues in arrival order; `shed` 503 + `Retry-After` when a queue is full or the request would finish after its deadline; `expired` 504 if it runs out while queued Work in it lives under: the request's deadline (`default_timeout` 2s, at most `max_timeout` 30s).
- Primary dashboard: `mansio_queue_*{queue="admission"}`, `mansio_admission`
- Rollback or feature flag: none (config, restart); `agentctl drain` sheds everything
- First look: `reason`: `would_expire` means statements are slow (check `mansio_request_latency_seconds`), `full` means a burst beyond the queue

#### agentd: `mailbox`

- Service: agentd
- Queue name: `mailbox`
- Owner: the operator of the agent VM
- Policy: `shed` 503 at once; a request found past its deadline at pickup is `expired` without running
- Capacity: 256 per database (`actor.mailbox_size`)
- Workers: one goroutine per database; a batch commits up to 64 writes together (`actor.max_batch`)
- Downstream dependency: one database's writer (its SQLite connection)
- Expected full behavior: `shed` 503 at once; a request found past its deadline at pickup is `expired` without running Work in it lives under: the request's deadline; a statement at most `statement_timeout` 1s.
- Primary dashboard: `mansio_queue_*{queue="mailbox"}`, `mansio_actor_queue_depth`
- Rollback or feature flag: none (config, restart)
- First look: it sits behind admission's per-database cap, so refusals here mean admission's `db_in_flight` + `db_queue` exceed the mailbox: fix the config

#### agentd: `read`

- Service: agentd
- Queue name: `read`
- Owner: the operator of the agent VM
- Policy: `shed` 503 at once, so reads never stall the writes behind them
- Capacity: 256 per database (`actor.mailbox_size`), only with a read pool
- Workers: `actor.read_conns` readers per database (default 0: reads share the writer)
- Downstream dependency: one database's read connections
- Expected full behavior: `shed` 503 at once, so reads never stall the writes behind them Work in it lives under: as the mailbox.
- Primary dashboard: `mansio_queue_*{queue="read"}`
- Rollback or feature flag: none (config, restart)
- First look: slow reads holding every reader: `mansio_request_latency_seconds`, then `statement_timeout`

#### agentd: `fetch`

- Service: agentd
- Queue name: `fetch`
- Owner: the operator of the agent VM
- Policy: `unavailable` 503 + `Retry-After` at once; a database whose last activation failed fails fast for a jittered backoff (`reason="failure_backoff"`)
- Capacity: 256 activations queued (package default, no config key)
- Workers: 16 fetches or 1 GiB in flight
- Downstream dependency: S3 (cold database fetches) and the local disk and network
- Expected full behavior: `unavailable` 503 + `Retry-After` at once; a database whose last activation failed fails fast for a jittered backoff (`reason="failure_backoff"`) Work in it lives under: the activation: 30 s plus size / 32 MiB/s.
- Primary dashboard: `mansio_queue_*{queue="fetch"}`, `mansio_vactor{stat="fetch_*"}`
- Rollback or feature flag: `agentctl fetch-limit N` (`POST /admin/fetch:limit?n=N`); `agentctl breaker s3 trip` stops fetches
- First look: `mansio_s3_latency_seconds` and the S3 breaker; lower the fetch limit if the disk or network is the bottleneck

#### agentd: `lifecycle`

- Service: agentd
- Queue name: `lifecycle`
- Owner: the operator of the agent VM
- Policy: `unavailable` 503 + `Retry-After` at once
- Capacity: 16 slots (`lifecycle_concurrency`), no queue
- Workers: the slots
- Downstream dependency: S3 (a fence writes back) and the placement authority, for controld's claim, fence and forget
- Expected full behavior: `unavailable` 503 + `Retry-After` at once Work in it lives under: `lifecycle_timeout` 30s.
- Primary dashboard: `mansio_queue_*{queue="lifecycle"}`
- Rollback or feature flag: `agentctl limit lifecycle N` (`POST /admin/limit?queue=lifecycle&n=N`)
- First look: what controld is doing (a restore moves many databases); slow fences are slow write-backs: `mansio_s3_latency_seconds`

#### agentd: `tenancy_admin`

- Service: agentd
- Queue name: `tenancy_admin`
- Owner: the operator of the agent VM
- Policy: 503 `shed` + `Retry-After` at once
- Capacity: 16 slots, no queue (no config key)
- Workers: the slots
- Downstream dependency: the tenancy store (the tenant admin API on the query listener)
- Expected full behavior: 503 `shed` + `Retry-After` at once Work in it lives under: 10 s per call; the body within `body_read_timeout`.
- Primary dashboard: `mansio_queue_*{queue="tenancy_admin"}`
- Rollback or feature flag: `agentctl limit tenancy_admin N` (`POST /admin/limit?queue=tenancy_admin&n=N`)
- First look: the tenancy store's latency and breaker

#### agentd: `tenancy_auth`

- Service: agentd
- Queue name: `tenancy_auth`
- Owner: the operator of the agent VM
- Policy: 503 at once (429 + `Retry-After` from the per-source limit)
- Capacity: 16 slots, no queue; before them 1 sign-in per second per source, burst 10
- Workers: the slots
- Downstream dependency: the tenancy store and the password-hash CPU (sign-in and sign-up)
- Expected full behavior: 503 at once (429 + `Retry-After` from the per-source limit) Work in it lives under: 10 s per call.
- Primary dashboard: `mansio_queue_*{queue="tenancy_auth"}`
- Rollback or feature flag: `agentctl limit tenancy_auth N` (`POST /admin/limit?queue=tenancy_auth&n=N`)
- First look: a sign-in flood (429s) or slow password hashing; lower the limit to protect query CPU

#### agentd: `admin`

- Service: agentd
- Queue name: `admin`
- Owner: the operator of the agent VM
- Policy: `unavailable` 503 at once
- Capacity: 4 slots, no queue (fixed)
- Workers: the slots
- Downstream dependency: nothing remote: the operator's own controls on the loopback admin listener
- Expected full behavior: `unavailable` 503 at once Work in it lives under: `lifecycle_timeout` 30s.
- Primary dashboard: `mansio_queue_*{queue="admin"}`
- Rollback or feature flag: none (config, restart)
- First look: a passivate or backup holding slots; they end at `lifecycle_timeout`

#### agentd: `write_back`

- Service: agentd
- Queue name: `write_back`
- Owner: the operator of the agent VM
- Policy: writes keep being acknowledged while write-back fails; the unpublished window grows (see the README's durability section)
- Capacity: not a queue: every dirty database is a candidate each tick
- Workers: 8 write-backs at once (package default)
- Downstream dependency: S3 (publishing warm databases)
- Expected full behavior: writes keep being acknowledged while write-back fails; the unpublished window grows (see the README's durability section) Work in it lives under: `maintenance.write_back_timeout` 1m0s per write-back.
- Primary dashboard: `mansio_vactor_dirty`, `mansio_vactor_oldest_dirty_age_seconds`, `mansio_vactor_events_total{event="writeback_failure"}`
- Rollback or feature flag: `agentctl breaker s3 trip|reset`; `agentctl backup KEY` forces one
- First look: the S3 breaker and `mansio_s3_requests_total{class="5xx"}`: the dirty age is the data a host loss would lose

#### agentd: `events`

- Service: agentd
- Queue name: `events`
- Owner: the operator of the agent VM
- Policy: queue full: the newest record is dropped and counted, and the next write records how many; disk over its limits: the oldest files are deleted when a new one opens
- Capacity: 8192 records (`event_log.buffer`); on disk 4 GiB (`event_log.max_total_bytes`) and 45 days (`event_log.max_age`)
- Workers: one writer goroutine
- Downstream dependency: the event log's disk writer and its volume; the request path never waits on it
- Expected full behavior: queue full: the newest record is dropped and counted, and the next write records how many; disk over its limits: the oldest files are deleted when a new one opens Work in it lives under: none (telemetry); at shutdown the writer gets 2 s and counts what it could not write as lost.
- Primary dashboard: `mansio_eventlog_queue_depth`, `mansio_eventlog_records_total{result="dropped|lost"}`, `mansio_eventlog_bytes`, `mansio_eventlog_retired_total`
- Rollback or feature flag: none (config, restart)
- First look: a slow or full disk under `event_log.dir`; `mansio_eventlog_retired_total` rising means files are deleted at the writer's size or age limit, before a collector shipped them
<!-- capacity:agentd:end -->

## controld

<!-- capacity:controld:begin -->
| Queue            | Protects                                                                              | Capacity (config)                                                                        | Workers         | When full                                                                                                            | Lives under                                                          | Metric                                                                                                            | Runtime lever                                                                     |
| ---------------- | ------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------- | --------------- | -------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| `resolve`        | controld itself: every resolve, most of them answered from the placement cache        | 256 slots (`locator.concurrency`), no queue                                              | the slots       | `unavailable` 503 + `Retry-After` at once                                                                            | `locator.timeout` 3s                                                 | `mansio_queue_*{queue="resolve"}`, `controld_locator_*`                                                           | none (config, restart)                                                            |
| `resolve_load`   | Postgres: placement and API-key reads on a cache miss, and the agents' schema reads   | 10 slots (`locator.load_concurrency`), no queue; callers of a load in flight share it    | the slots       | resolve: `shed` 503 + `Retry-After` at once; schema read: `unavailable` 503                                          | a load: `locator.timeout` 3s                                         | `mansio_queue_*{queue="resolve_load"}`, `controld_pg_pool_*`                                                      | `controld limit -queue resolve_load -n N` (`POST /v1/limits/resolve_load`)        |
| `resolve_create` | Postgres and an agent's lifecycle slots (create on first use)                         | 2 slots (`locator.create_concurrency`), no queue                                         | the slots       | `shed` 503 + `Retry-After` at once; nothing created                                                                  | the resolve's; a create it could not finish is recorded and resumed  | `mansio_queue_*{queue="resolve_create"}`                                                                          | `controld limit -queue resolve_create -n N` (`POST /v1/limits/resolve_create`)    |
| `controld_api`   | Postgres and the agents (the admin API: create, move, suspend, resume, drop, restore) | 16 slots (`api_concurrency`), no queue                                                   | the slots       | `unavailable` 503 + `Retry-After` at once                                                                            | `api_timeout` 2m0s; a flow it did not finish is recorded and resumed | `mansio_queue_*{queue="controld_api"}`                                                                            | `controld limit -queue controld_api -n N` (`POST /v1/limits/controld_api`)        |
| `pg pool`        | Postgres's connection limit                                                           | 32 connections per controld (`pg.max_conns`); callers are budgeted below it: 10 + 2 + 16 | the connections | a caller beyond the pool waits inside pgxpool until its call timeout; the budget exists so that this does not happen | 2.5 s per call                                                       | `controld_pg_pool_conns`, `controld_pg_pool_acquires_waited_total`, `controld_pg_pool_acquire_wait_seconds_total` | lower the three pools above with `controld limit`; the pool size itself is config |
| `tenancy_admin`  | the tenancy store (the tenant admin API, when the `tenancy` section is set)           | 16 slots, no queue (no config key)                                                       | the slots       | 503 `shed` + `Retry-After` at once                                                                                   | 10 s per call                                                        | `mansio_queue_*{queue="tenancy_admin"}`                                                                           | `controld limit -queue tenancy_admin -n N` (`POST /v1/limits/tenancy_admin`)      |
| `tenancy_auth`   | the tenancy store and the password-hash CPU (sign-in and sign-up)                     | 16 slots, no queue; before them 1 sign-in per second per source, burst 10                | the slots       | 503 at once (429 + `Retry-After` from the per-source limit)                                                          | 10 s per call                                                        | `mansio_queue_*{queue="tenancy_auth"}`                                                                            | `controld limit -queue tenancy_auth -n N` (`POST /v1/limits/tenancy_auth`)        |

### Runbooks: controld

#### controld: `resolve`

- Service: controld
- Queue name: `resolve`
- Owner: the operator of the control plane
- Policy: `unavailable` 503 + `Retry-After` at once
- Capacity: 256 slots (`locator.concurrency`), no queue
- Workers: the slots
- Downstream dependency: controld itself: every resolve, most of them answered from the placement cache
- Expected full behavior: `unavailable` 503 + `Retry-After` at once Work in it lives under: `locator.timeout` 3s.
- Primary dashboard: `mansio_queue_*{queue="resolve"}`, `controld_locator_*`
- Rollback or feature flag: none (config, restart)
- First look: cache hit ratio (`controld_locator_hits_total` against `loads_total`) and whether the listener is connected (`controld_locator_listening`)

#### controld: `resolve_load`

- Service: controld
- Queue name: `resolve_load`
- Owner: the operator of the control plane
- Policy: resolve: `shed` 503 + `Retry-After` at once; schema read: `unavailable` 503
- Capacity: 10 slots (`locator.load_concurrency`), no queue; callers of a load in flight share it
- Workers: the slots
- Downstream dependency: Postgres: placement and API-key reads on a cache miss, and the agents' schema reads
- Expected full behavior: resolve: `shed` 503 + `Retry-After` at once; schema read: `unavailable` 503 Work in it lives under: a load: `locator.timeout` 3s.
- Primary dashboard: `mansio_queue_*{queue="resolve_load"}`, `controld_pg_pool_*`
- Rollback or feature flag: `controld limit -queue resolve_load -n N` (`POST /v1/limits/resolve_load`)
- First look: Postgres latency and the placement breaker; `controld_pg_pool_acquires_waited_total` should stop growing once the pool is warm (the budget keeps callers below it)

#### controld: `resolve_create`

- Service: controld
- Queue name: `resolve_create`
- Owner: the operator of the control plane
- Policy: `shed` 503 + `Retry-After` at once; nothing created
- Capacity: 2 slots (`locator.create_concurrency`), no queue
- Workers: the slots
- Downstream dependency: Postgres and an agent's lifecycle slots (create on first use)
- Expected full behavior: `shed` 503 + `Retry-After` at once; nothing created Work in it lives under: the resolve's; a create it could not finish is recorded and resumed.
- Primary dashboard: `mansio_queue_*{queue="resolve_create"}`
- Rollback or feature flag: `controld limit -queue resolve_create -n N` (`POST /v1/limits/resolve_create`)
- First look: agent health (`controld_agent_up`) and the agents' `lifecycle` queue; past `locator.max_dbs` the answer is `quota_exceeded`, not this

#### controld: `controld_api`

- Service: controld
- Queue name: `controld_api`
- Owner: the operator of the control plane
- Policy: `unavailable` 503 + `Retry-After` at once
- Capacity: 16 slots (`api_concurrency`), no queue
- Workers: the slots
- Downstream dependency: Postgres and the agents (the admin API: create, move, suspend, resume, drop, restore)
- Expected full behavior: `unavailable` 503 + `Retry-After` at once Work in it lives under: `api_timeout` 2m0s; a flow it did not finish is recorded and resumed.
- Primary dashboard: `mansio_queue_*{queue="controld_api"}`
- Rollback or feature flag: `controld limit -queue controld_api -n N` (`POST /v1/limits/controld_api`)
- First look: what holds the slots: a restore moves many databases; an agent that answers slowly (`mansio_breaker_state{dep="agent:*"}`)

#### controld: `pg pool`

- Service: controld
- Queue name: `pg pool`
- Owner: the operator of the control plane
- Policy: a caller beyond the pool waits inside pgxpool until its call timeout; the budget exists so that this does not happen
- Capacity: 32 connections per controld (`pg.max_conns`); callers are budgeted below it: 10 + 2 + 16
- Workers: the connections
- Downstream dependency: Postgres's connection limit
- Expected full behavior: a caller beyond the pool waits inside pgxpool until its call timeout; the budget exists so that this does not happen Work in it lives under: 2.5 s per call.
- Primary dashboard: `controld_pg_pool_conns`, `controld_pg_pool_acquires_waited_total`, `controld_pg_pool_acquire_wait_seconds_total`
- Rollback or feature flag: lower the three pools above with `controld limit`; the pool size itself is config
- First look: N controld instances × (`max_conns` + 1) against Postgres's `max_connections`

#### controld: `tenancy_admin`

- Service: controld
- Queue name: `tenancy_admin`
- Owner: the operator of the control plane
- Policy: 503 `shed` + `Retry-After` at once
- Capacity: 16 slots, no queue (no config key)
- Workers: the slots
- Downstream dependency: the tenancy store (the tenant admin API, when the `tenancy` section is set)
- Expected full behavior: 503 `shed` + `Retry-After` at once Work in it lives under: 10 s per call.
- Primary dashboard: `mansio_queue_*{queue="tenancy_admin"}`
- Rollback or feature flag: `controld limit -queue tenancy_admin -n N` (`POST /v1/limits/tenancy_admin`)
- First look: Postgres latency; these calls share the pool's spare connections

#### controld: `tenancy_auth`

- Service: controld
- Queue name: `tenancy_auth`
- Owner: the operator of the control plane
- Policy: 503 at once (429 + `Retry-After` from the per-source limit)
- Capacity: 16 slots, no queue; before them 1 sign-in per second per source, burst 10
- Workers: the slots
- Downstream dependency: the tenancy store and the password-hash CPU (sign-in and sign-up)
- Expected full behavior: 503 at once (429 + `Retry-After` from the per-source limit) Work in it lives under: 10 s per call.
- Primary dashboard: `mansio_queue_*{queue="tenancy_auth"}`
- Rollback or feature flag: `controld limit -queue tenancy_auth -n N` (`POST /v1/limits/tenancy_auth`)
- First look: a sign-in flood or slow password hashing
<!-- capacity:controld:end -->

## edge

<!-- capacity:edge:begin -->
| Queue              | Protects                                                                   | Capacity (config)                                                                               | Workers                   | When full                                                                                                                                    | Lives under                                                                                    | Metric                                                                                            | Runtime lever                                              |
| ------------------ | -------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------- | ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| `edge_in_flight`   | the edge's memory: request bodies in flight (`max_in_flight` × `max_body`) | 8192 requests (`max_in_flight`, default 2 × `agent_in_flight`); bodies up to 1 MiB (`max_body`) | one goroutine per request | `shed` 503 + `Retry-After` before the body is read                                                                                           | the request's deadline (`default_timeout` 2s, at most `max_timeout` 30s) plus `reply_grace` 1s | `mansio_queue_*{queue="edge_in_flight"}`, `mansio_in_flight{point="edge"}`                        | none (config, restart)                                     |
| `edge_source_rate` | the verify CPU, from one source sending many new tickets                   | 20 new tickets per second per source, burst 50 (`rate_limit`); 100000 sources tracked           | none (a token bucket)     | `shed` 503 with `Retry-After` = the time to the next token                                                                                   | none                                                                                           | `mansio_edge_tickets_total{result="rate_limited"}`                                                | none (config, restart)                                     |
| `edge_verify`      | the edge's CPU (full ticket verifies)                                      | 2 × GOMAXPROCS slots (`verify_slots`), no queue                                                 | the slots                 | `shed` 503 at once                                                                                                                           | none (about 26 µs each)                                                                        | `mansio_queue_*{queue="edge_verify"}`, `mansio_edge_tickets_total`                                | none (config, restart)                                     |
| `edge_agent`       | each agent (requests in flight to it)                                      | 4096 per agent (`agent_in_flight`), no queue                                                    | the slots                 | `shed` 503 before the body is read; an agent that cannot be reached opens its breaker: `unavailable` 503 with the cool-down as `Retry-After` | the request's deadline (`default_timeout` 2s, at most `max_timeout` 30s) plus `reply_grace` 1s | `mansio_queue_*{queue="edge_agent"}`, `mansio_edge_agent_in_flight`, `mansio_edge_refusals_total` | `POST /admin/agent:limit?n=N` on `admin_listen` (loopback) |

### Runbooks: edge

#### edge: `edge_in_flight`

- Service: edge
- Queue name: `edge_in_flight`
- Owner: the operator of the edge
- Policy: `shed` 503 + `Retry-After` before the body is read
- Capacity: 8192 requests (`max_in_flight`, default 2 × `agent_in_flight`); bodies up to 1 MiB (`max_body`)
- Workers: one goroutine per request
- Downstream dependency: the edge's memory: request bodies in flight (`max_in_flight` × `max_body`)
- Expected full behavior: `shed` 503 + `Retry-After` before the body is read Work in it lives under: the request's deadline (`default_timeout` 2s, at most `max_timeout` 30s) plus `reply_grace` 1s.
- Primary dashboard: `mansio_queue_*{queue="edge_in_flight"}`, `mansio_in_flight{point="edge"}`
- Rollback or feature flag: none (config, restart)
- First look: whether the agents are slow (requests pile up at the edge): `mansio_edge_agent_in_flight` and the agents' admission

#### edge: `edge_source_rate`

- Service: edge
- Queue name: `edge_source_rate`
- Owner: the operator of the edge
- Policy: `shed` 503 with `Retry-After` = the time to the next token
- Capacity: 20 new tickets per second per source, burst 50 (`rate_limit`); 100000 sources tracked
- Workers: none (a token bucket)
- Downstream dependency: the verify CPU, from one source sending many new tickets
- Expected full behavior: `shed` 503 with `Retry-After` = the time to the next token Work in it lives under: none.
- Primary dashboard: `mansio_edge_tickets_total{result="rate_limited"}`
- Rollback or feature flag: none (config, restart)
- First look: one client without a ticket cache, or a flood of invalid tickets (`result="invalid"`)

#### edge: `edge_verify`

- Service: edge
- Queue name: `edge_verify`
- Owner: the operator of the edge
- Policy: `shed` 503 at once
- Capacity: 2 × GOMAXPROCS slots (`verify_slots`), no queue
- Workers: the slots
- Downstream dependency: the edge's CPU (full ticket verifies)
- Expected full behavior: `shed` 503 at once Work in it lives under: none (about 26 µs each).
- Primary dashboard: `mansio_queue_*{queue="edge_verify"}`, `mansio_edge_tickets_total`
- Rollback or feature flag: none (config, restart)
- First look: the ticket cache's hit ratio (`result="hit"` against `miss`): a cold cache after a restart refills in seconds

#### edge: `edge_agent`

- Service: edge
- Queue name: `edge_agent`
- Owner: the operator of the edge
- Policy: `shed` 503 before the body is read; an agent that cannot be reached opens its breaker: `unavailable` 503 with the cool-down as `Retry-After`
- Capacity: 4096 per agent (`agent_in_flight`), no queue
- Workers: the slots
- Downstream dependency: each agent (requests in flight to it)
- Expected full behavior: `shed` 503 before the body is read; an agent that cannot be reached opens its breaker: `unavailable` 503 with the cool-down as `Retry-After` Work in it lives under: the request's deadline (`default_timeout` 2s, at most `max_timeout` 30s) plus `reply_grace` 1s.
- Primary dashboard: `mansio_queue_*{queue="edge_agent"}`, `mansio_edge_agent_in_flight`, `mansio_edge_refusals_total`
- Rollback or feature flag: `POST /admin/agent:limit?n=N` on `admin_listen` (loopback)
- First look: that agent's own admission and latency; lower the limit to take load off agents that are struggling
<!-- capacity:edge:end -->

## Alerts

[`deploy/prometheus/backpressure.rules.yml`](../deploy/prometheus/backpressure.rules.yml) holds four
saturation and failure rules and two "alert on broken protection, not merely active protection" rules, written
against the series above:

| Alert | Fires when | Severity | Start with |
|---|---|---|---|
| `MansioQueueNearCapacity` | a queue's depth is over 80 % of its capacity for 10 min | warning | that queue's runbook |
| `MansioQueueNearCapacityAndRejecting` | the same, and it is refusing work | warning | that queue's runbook |
| `MansioUnexpectedDrops` | the event log dropped or lost records for 5 min | warning | the `events` runbook |
| `MansioFailuresHigh` | 5xx answers, failed write-backs or failed activations are sustained | critical | the agent's log (`log_id`), the S3 breaker |
| `MansioQueueAgeTooHigh` | p95 wait in `admission` or `mailbox` is over 1 s (half the default 2 s deadline) | critical | `admission` runbook |
| `MansioSheddingButDependencySlow` | refusals are active and S3 p99 is still over 1 s | critical | lower `fetch`, trip the S3 breaker |
| `MansioUnpublishedWritesOld` | the oldest acknowledged write not yet in S3 is older than 60 s (healthy: about 11 s) | critical | the `write_back` runbook |

Shedding by itself does not page: a queue that refuses work while its dependency recovers is protection
working. The pages are for protection that is not enough (the dependency is still slow, the wait is past the
useful budget) or for work lost or at risk (drops, failures, acknowledged writes not yet in S3).
