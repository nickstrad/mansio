# Backpressure

How the server bounds every queue and refuses work it cannot finish: admission and shedding in agentd
(`internal/front`), the edge (`internal/edge`), controld's resolve gates and Postgres pool budget
(`internal/control/locator`, `cmd/controld`), the retry contract both clients follow
(`internal/front/contract.go`), shutdown, event-log retention, and the per-queue metrics, alerts and runtime
levers that make all of it visible (`internal/metrics`, `docs/capacity.md`,
`deploy/prometheus/backpressure.rules.yml`).

- [Per-queue metrics](queue-metrics.md): the `mansio_queue_*` families, the fixed `queue` label list,
  controld's pgx pool stats.
- [Edge: refuse before reading the body](edge-refuses-before-body.md): refusals decided before the body is
  read; `max_in_flight`.
- [controld: callers budgeted below the Postgres pool](controld-pool-budget.md): a narrow gate for store
  loads, one for creates on first use, and the per-process connection budget.
- [Shutdown: readiness, drain delay and deadlines](shutdown.md): `drain_delay` on the edge and controld,
  controld's forced close, the snapshot and LIST contexts.
- [/v1/ops: nothing remote before admission without a bound](ops-pre-admission.md): verify slots for the
  credential path, per-database claim flights instead of a global mutex, drain before the body.
- [Admission sheds what would expire in its queue](admission-sheds-what-would-expire.md): a request that
  cannot finish before its deadline is shed at admission instead of queued; before/after numbers.
- [One retry contract for both clients](retry-contract.md): one generated table, shared vectors, and why the
  locator's `moving` is not charged to the retry budget.
- [Event-log retention in the writer](event-log-retention.md): the writer bounds its own files by bytes and
  age and counts what a timed-out close loses.
- [The durability promise, stated and made visible](durability-promise.md): the write-back window stated
  exactly and the dirty-age alert; refusing writes past a dirty age is not implemented.
- [Capacity table, alerts, runbooks and levers](capacity-table-and-alerts.md): `docs/capacity.md` generated
  from the config defaults, alert rules, runbooks, and the runtime levers.

To run the Postgres- and S3-gated Go suites that cover controld, the locator and the clients' retry loops, see
[gated suites](../testing/gated-suites.md).
