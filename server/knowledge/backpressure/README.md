# backpressure: the B1–B11 fixes from *Backpressure in Microservices*

Owner: the backpressure-fixes run (2026-11-10; state in the repo root `STATE.md`). Plan and findings:
`/root/Software/book-notes-work/backpressure-plan/books/backpressure-in-microservices/APPLICATION-PLAN.md`.

Notes, newest first:

- [2026-11-10 shutdown](2026-11-10-shutdown.md) (B5): `drain_delay` on the edge and controld, controld's forced
  close, the snapshot and LIST contexts.
- [2026-11-10 controld pool budget](2026-11-10-controld-pool-budget.md) (B4): a narrow gate for store loads and
  one for creates on first use; the per-process connection budget.
- [2026-11-10 edge admission](2026-11-10-edge-admission.md) (B3): refusals decided before the body is read;
  `max_in_flight`.
- [2026-11-10 per-queue metrics](2026-11-10-queue-metrics.md) (B1): `mansio_queue_*` families, the fixed
  `queue` list, controld's pgx pool stats.
