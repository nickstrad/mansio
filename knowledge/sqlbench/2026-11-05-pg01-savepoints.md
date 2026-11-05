# PG-01: savepoint per write — compare-actor before/after (2026-11-05)

Change: `actor.writeBatch` wraps each write in `SAVEPOINT w` / `RELEASE w` (`ROLLBACK TO w` on error) and
stops the batch when a write ended the transaction (see `docs/books/art-of-postgresql/plan.md` PG-01).

Run: `configs/compare-actor.json` (2048 workers, keep_dbs), the two binaries back to back on the same machine.
**The machine was shared with other agents (load average ~30 at the end), so phase 2 is noise-dominated.**
All requests (ok and failed) included; latency = send → reply.

| phase | writes | write p99 ms before → after | commit p99 ms before → after | ok % before → after |
|---|---|---|---|---|
| 0 `1k-dbs` (ramp to 20k qps) | 54 632 | 131.6 → 131.2 | 2.25 → 2.06 | 99.01 → 98.97 |
| 1 `1k-hold` (20k qps) | 98 926 | 162.9 → 161.2 | 6.12 → 4.84 | 99.01 → 99.00 |
| 2 `5k-dbs` (overload, 20k → 40k qps) | 148 182 | 1684.6 → 546.0 | 1226.6 → 173.3 | 46.81 → 70.78 |

Result: in the phases that are not overloaded, write p99 and commit p99 change by ≤ 2% (commit is lower), well
inside the 10% gate, so savepoints stay on for every batch (no gating to batches > 1). Phase 2 is overloaded
in both runs and dominated by machine load; do not read the improvement as caused by the change.
The three extra statements per write are prepared once and cached (`sqliteConn.stmt`).
