# Savepoints, foreign keys and seeding: measured before and after

Three changes to the SQLite actor and the bench workload, each measured against the build before it: a
savepoint per write in a group commit, foreign keys with STRICT tables, and multi-row seed inserts (reverted).

Each comparison ran the two binaries back to back on one machine (2026-11-05) while other builds loaded it;
the overloaded and short runs below are noisy for that reason.

## Savepoint per write

Change: `actor.writeBatch` wraps each write in `SAVEPOINT w` / `RELEASE w` (`ROLLBACK TO w` on error) and
stops the batch when a write ended the transaction.

Run: `configs/compare-actor.json` (2048 workers, keep_dbs). **The machine was shared (load average ~30 at the
end), so phase 2 is noise-dominated.** All requests (ok and failed) included; latency = send → reply.

| phase                                | writes  | write p99 ms before → after | commit p99 ms before → after | ok % before → after |
| ------------------------------------ | ------- | --------------------------- | ---------------------------- | ------------------- |
| 0 `1k-dbs` (ramp to 20k qps)         | 54 632  | 131.6 → 131.2               | 2.25 → 2.06                  | 99.01 → 98.97       |
| 1 `1k-hold` (20k qps)                | 98 926  | 162.9 → 161.2               | 6.12 → 4.84                  | 99.01 → 99.00       |
| 2 `5k-dbs` (overload, 20k → 40k qps) | 148 182 | 1684.6 → 546.0              | 1226.6 → 173.3               | 46.81 → 70.78       |

Result: in the phases that are not overloaded, write p99 and commit p99 change by ≤ 2% (commit is lower), well
inside a 10% bar, so savepoints stay on for every batch (no gating to batches > 1). Phase 2 is overloaded in
both runs and dominated by machine load; do not read the improvement as caused by the change. The extra
statements per write (two on success, three on failure) are prepared once and cached (`sqliteConn.stmt`).

## Foreign keys and STRICT tables

Change: the actor opens every connection with `foreign_keys(1)`; generated tables and the coldload template
are `STRICT`; the workload deletes only from leaf tables and draws `parent_id` from the parent's seed rows
(NULL when it has none); the `keep_dbs` fingerprint is `v3`.

Run: `configs/smoke.json`.

|                                           | before                 | after                  |
| ----------------------------------------- | ---------------------- | ---------------------- |
| requests with outcome `error`             | 0                      | 0                      |
| join avg rows, phase 0 → 3 (10 → 500 dbs) | 2.04, 1.47, 1.29, 0.98 | 1.80, 1.27, 1.28, 1.16 |
| ok / stmt_timeout                         | 35 561 / 1 449         | 35 431 / 1 559         |

Result: enforcing foreign keys and types adds no failed requests: every generated write stays valid
(`TestWritesNeverViolateForeignKeys` checks 20 parent-chain schemas × 300 writes plus
`pragma_foreign_key_check`). Join rows no longer drift down from deleted parents; what remains of the drop is
expected (join parent ids are drawn from the parent's whole id range, which inserts extend with rows that have
no children yet). The smoke run is short and the delete share is 2%, so the difference is small and noisy
either way.

## Multi-row seed fill: measured, reverted

Tried: `schema.Fill` sending fixed chunks of 100 rows (`InsertSQL(100)`, one cached statement per table) plus
one shorter tail, instead of one row per `exec`. Same rows in the same order (same RNG sequence).

Run: a seed-only config (ramp's schema section: 1–5 tables, 2–8 columns, 50–500 rows; 2000 fresh databases,
`keep_dbs` false, one 1 s phase), the two binaries alternating, two runs each, `seed_ms` from `phases.csv`.
18-core Mac, load average ~5–8 from other builds.

| run | before (row at a time) | after (100-row chunks) |
| --- | ---------------------- | ---------------------- |
| 1   | 3144.7 ms (1.57 ms/db) | 2707.9 ms (1.35 ms/db) |
| 2   | 3135.5 ms (1.57 ms/db) | 2691.1 ms (1.35 ms/db) |

Result: seed time drops **~14%**, below the 20% that would justify the change, so it was reverted. SQLite has
no network round trip; the per-row cost saved is driver overhead only, and the rest of seeding (file create,
WAL, commit, checkpoint on close, rename) dominates. Revisit only if seeding becomes a bottleneck at much
larger seed row counts.
