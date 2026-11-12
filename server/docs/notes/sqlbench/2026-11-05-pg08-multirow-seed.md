# PG-08: multi-row seed fill — measured, reverted (null result, 2026-11-05)

Tried: `schema.Fill` sending fixed chunks of 100 rows (`InsertSQL(100)`, one cached statement per table) plus one
shorter tail, instead of one row per `exec`. Same rows in the same order (same RNG sequence).

Run: a seed-only config (ramp's schema section: 1–5 tables, 2–8 columns, 50–500 rows; 2000 fresh databases,
`keep_dbs` false, one 1 s phase), the two binaries alternating, two runs each, `seed_ms` from `phases.csv`.
18-core Mac, load average ~5–8 from other agents.

| run | before (row at a time) | after (100-row chunks) |
|---|---|---|
| 1 | 3144.7 ms (1.57 ms/db) | 2707.9 ms (1.35 ms/db) |
| 2 | 3135.5 ms (1.57 ms/db) | 2691.1 ms (1.35 ms/db) |

Result: seed time drops **~14%**, below the plan's 20% bar, so the change was reverted (plan PG-08: "if it drops
< 20%, revert and record the null result"). SQLite has no network round trip; the per-row cost saved is driver
overhead only, and the rest of seeding (file create, WAL, commit, checkpoint on close, rename) dominates.
Revisit only if seeding becomes a bottleneck at much larger seed row counts.
