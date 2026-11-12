# backpressure: the B1–B11 fixes from *Backpressure in Microservices*

Owner: the backpressure-fixes run (2026-11-10; state in `agent-work/STATE.md`). Plan and findings:
`/root/Software/book-notes-work/backpressure-plan/books/backpressure-in-microservices/APPLICATION-PLAN.md`.

Notes, newest first:

- [2026-11-10 running the gated suites](2026-11-10-running-the-gated-suites.md): throwaway Postgres and
  SeaweedFS for `MANSIO_PG_URL` / `COLDLOAD_S3` on the VM, and the bucket that must be created by hand.
- [2026-11-10 event-log retention](2026-11-10-eventlog-retention.md) (B9, event-log part): the writer bounds its
  own files by bytes and age and counts what a timed-out close loses; the warm-set default is gated.
- [2026-11-10 durability promise](2026-11-10-durability-promise.md) (B10 (a)): the write-back window stated
  exactly, the dirty-age alert; part (b) is gated and not built.
- [2026-11-10 capacity table](2026-11-10-capacity-table.md) (B11): `docs/capacity.md` generated from the config
  defaults, alert rules, runbooks, and the runtime levers.
- [2026-11-10 retry contract](2026-11-10-retry-contract.md) (B8): one generated table, shared vectors, and what
  changed in the two clients.
- [2026-11-10 admission would-expire](2026-11-10-admission-would-expire.md) (B7): a request that cannot finish
  before its deadline is shed at admission instead of queued; before/after numbers.
- [2026-11-10 /v1/ops pre-admission](2026-11-10-ops-pre-admission.md) (B6): verify slots for the credential path,
  per-database claim flights instead of a global mutex, drain before the body.
- [2026-11-10 shutdown](2026-11-10-shutdown.md) (B5): `drain_delay` on the edge and controld, controld's forced
  close, the snapshot and LIST contexts.
- [2026-11-10 controld pool budget](2026-11-10-controld-pool-budget.md) (B4): a narrow gate for store loads and
  one for creates on first use; the per-process connection budget.
- [2026-11-10 edge admission](2026-11-10-edge-admission.md) (B3): refusals decided before the body is read;
  `max_in_flight`.
- [2026-11-10 per-queue metrics](2026-11-10-queue-metrics.md) (B1): `mansio_queue_*` families, the fixed
  `queue` list, controld's pgx pool stats.
