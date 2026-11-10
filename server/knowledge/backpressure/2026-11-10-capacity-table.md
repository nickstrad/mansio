# Capacity table, alerts, runbooks and levers (B11, 2026-11-10)

- **The table**: [`docs/capacity.md`](../../docs/capacity.md). One document; its agentd, controld and edge
  sections are generated from each daemon's `defaults()` by `TestCapacityTable` in `cmd/agentd`,
  `cmd/controld` and `cmd/edge` (`internal/capacity` renders and checks). A changed default fails the test
  until the section is regenerated with `-update`. Each row: queue, what it protects, capacity and its config
  key, workers, full-queue policy, the deadline it lives under, its metric, its runtime lever; then a runbook
  per queue in the book's template (ch. 8).
- **Alerts**: `deploy/prometheus/backpressure.rules.yml`, the book's four rules (ch. 8) and its two
  broken-protection rules (ch. 4), over `mansio_queue_depth / mansio_queue_capacity`,
  `mansio_queue_rejected_total`, `mansio_queue_wait_seconds`, the event log's drops, 5xx and write-back
  failures, and S3 p99. `TestAlertRulesUseRealSeries` (cmd/agentd) fails if a rule names a series agentd does
  not export. Not run through `promtool` (not installed on this VM); the YAML parses.
- **`mansio_queue_depth`** is new: the per-queue families now carry depth beside capacity (a slot pool
  reports slots in use). `mailbox` and `read` have none: their bound is per database, and
  `mansio_actor_queue_depth{stat="max"}` is the deepest actor.
- **One pool type**: `internal/slots.Pool` (non-queuing, counted, resizable) replaced the channel semaphore +
  counter pairs in agentd, controld, the locator, tenancyhttp and the edge's per-agent bulkhead.
- **Levers added** (each lasts until restart; logged at WARN; shown at once in `mansio_queue_capacity`):

| Process | Queues | How |
|---|---|---|
| agentd | `verify`, `lifecycle`, `credential_place`, `tenancy_admin`, `tenancy_auth` | `agentctl limit QUEUE N` → `POST /admin/limit?queue=Q&n=N` on `admin_listen` |
| agentd | `fetch` (existing) | `agentctl fetch-limit N` |
| controld | `resolve_load`, `resolve_create`, `controld_api`, `tenancy_admin`, `tenancy_auth` | `controld limit -queue Q -n N` → `POST /v1/limits/Q` (admin token); the pool budget is enforced here too |
| edge | `edge_agent` | `POST /admin/agent:limit?n=N` on the new loopback-only `admin_listen` (default off) |

No lever, on purpose: `admission`, `mailbox`, `read`, `events` and the edge's own caps protect the local
process, not a dependency (the plan asks for a lever "where the queue protects a dependency"); `agentctl drain`
is the blunt local control.
