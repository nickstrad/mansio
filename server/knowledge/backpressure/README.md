# backpressure: the B1–B11 fixes from *Backpressure in Microservices*

Owner: the backpressure-fixes run (2026-11-10; state in the repo root `STATE.md`). Plan and findings:
`/root/Software/book-notes-work/backpressure-plan/books/backpressure-in-microservices/APPLICATION-PLAN.md`.

Notes, newest first:

- [2026-11-10 edge admission](2026-11-10-edge-admission.md) (B3): refusals decided before the body is read;
  `max_in_flight`.
- [2026-11-10 per-queue metrics](2026-11-10-queue-metrics.md) (B1): `mansio_queue_*` families, the fixed
  `queue` list, controld's pgx pool stats.
