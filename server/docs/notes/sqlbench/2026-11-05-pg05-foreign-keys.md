# PG-05: foreign keys and STRICT tables — smoke before/after (2026-11-05)

Change: the actor opens every connection with `foreign_keys(1)`; generated tables and the coldload template are
`STRICT`; the workload deletes only from leaf tables and draws `parent_id` from the parent's seed rows (NULL when
it has none); the `keep_dbs` fingerprint is `v3`. The rule: [coding-style §7](../../coding-style.md#7-sql).

Run: `configs/smoke.json`, the two binaries back to back (machine shared with other agents).

| | before | after |
|---|---|---|
| requests with outcome `error` | 0 | 0 |
| join avg rows, phase 0 → 3 (10 → 500 dbs) | 2.04, 1.47, 1.29, 0.98 | 1.80, 1.27, 1.28, 1.16 |
| ok / stmt_timeout | 35 561 / 1 449 | 35 431 / 1 559 |

Result: enforcing foreign keys and types adds no failed requests: every generated write stays valid
(`TestWritesNeverViolateForeignKeys` checks 20 parent-chain schemas × 300 writes plus `pragma_foreign_key_check`).
Join rows no longer drift down from deleted parents; what remains of the drop is expected (join parent ids are
drawn from the parent's whole id range, which inserts extend with rows that have no children yet). The smoke
run is short and the delete share is 2%, so the difference is small and noisy either way.
