# sqlbench

sqlbench measures one goroutine (actor) per SQLite database under load: if SQLite work stops sharing one
thread or one lock, how much does that buy, and what is the next limit? Code: `cmd/bench/sqlbench`,
`internal/{actor,schema,workload,retry,load,stats}`, `queries/report.sql`, `queries/compare.sql`,
`configs/*.json` (not `configs/coldload/`).

- [How Cloudflare Durable Objects get their per-object speed](cloudflare-durable-objects.md): one actor's
  measured ceiling against Cloudflare's per-object limit, and which of their techniques map onto ours.
- [Commit cost with a real fsync](linux-fsync.md): a durable commit in Docker Desktop's Linux VM against
  macOS, and how group commit hides it.
- [Savepoints, foreign keys and seeding](savepoints-foreign-keys-and-seeding.md): a savepoint per write,
  foreign keys with STRICT tables, and a multi-row seed fill that was reverted; each measured before and
  after.
- [`compare-output.txt`](compare-output.txt): the full report of the actor vs global lock vs read pool
  comparison below.

## Key numbers

- **Durable commit on Linux** (measured 2026-11-05, Docker Desktop's Linux VM on an Apple M5 Max): a
  `synchronous=FULL` commit costs ~93 µs (one txn per write) vs 60 µs on macOS without `F_FULLFSYNC`; group
  commit brings FULL to 4.3 µs per write. Indicative only (virtual disk); measure again on target hardware.
- **One actor's ceiling** (measured 2026-11-04, Apple M5 Max): ~151k point reads/s, ~162k ops/s at 20% writes,
  319k writes/s with group commit (58k without). Cloudflare's documented soft limit is ~1,000 req/s per
  Durable Object; their speed tricks (same-thread synchronous SQLite, write coalescing behind output gates,
  durability by shipping the WAL to a 3-of-5 quorum instead of fsync) map onto ours, and WAL shipping is the
  piece not built.

## Actor vs global lock vs actor + read pool

Measured 2026-11-04. Machine: Apple Silicon, 18 cores, 128 GB, macOS, Go 1.26.4, `modernc.org/sqlite` (pure
Go), DuckDB 2.0.0. The global-lock baseline is a known-bad design and is no longer run; its config no longer
exists.

Configs: `configs/compare-actor.json` and `configs/compare-actor-readpool.json` (`read_conns: 4`), plus a
global-lock config, all with identical seed, schemas, mix and schedule; only the mode and read pool differ.
The configs as run held a `"mode"` field that the config loader no longer accepts; the runnable versions are
the ones in `configs/`. Schedule: 2048 workers, `keep_dbs`, three 15 s phases: `1k-dbs` (1000 dbs, 2,000 →
20,000 qps), `1k-hold` (1000 dbs, 20,000 qps step), `5k-dbs` (5000 dbs, 20,000 → 40,000 qps). Statement
timeout 200 ms, request timeout 2 s. Workload: default mix incl. ≈1% `slow_cte`/`cross_join` that run until
the 200 ms statement timeout; uniform traffic; 2048 requests in flight max; ~915k scheduled requests per run.
Command: `bin/sqlbench -config configs/<c>.json -out results/<c>` then `scripts/compare.sh`. Full output:
[compare-output.txt](compare-output.txt).

| Phase (dbs, target qps) | actor: ok qps / ok% / p50 / p99 | actor + 4 read conns           | global lock                 |
| ----------------------- | ------------------------------- | ------------------------------ | --------------------------- |
| 1k dbs, ramp → 11k avg  | 10,893 / 99.1% / 0.18 / 134 ms  | 10,793 / 98.1% / 4.0 / 222 ms  | 220 / 2.0% / 567 / 2,121 ms |
| 1k dbs, hold 20k        | 19,798 / 99.0% / 0.79 / 167 ms  | 17,886 / 89.4% / 60 / 455 ms   | 45 / 0.2%                   |
| 5k dbs, ramp → 30k avg  | 8,856 / 29.6% / 21 / 1,327 ms   | 13,883 / 46.3% / 0.11 / 722 ms | 17 / 0.06%                  |

Measured conclusions:

1. Removing the shared lock is the dominant effect: ~20k ok qps vs ~220 on the same machine and work. Under
   one lock a single 200 ms statement stalls every database (lock-wait p99 1–1.8 s).
2. The next limit is CPU (~14 of 18 cores busy at saturation), much of it spent on the slow 1% until they are
   interrupted. Past ~20k qps the 2048 in-flight cap fills (`gen_full`).
3. Within one database, a single connection causes head-of-line blocking: p99 is almost all queue wait behind
   slow queries on that same database. A 4-connection read pool fixes it at 5k dbs (p50 21 → 0.11 ms, +57%
   throughput) but lets more doomed slow queries run at once (stmt timeouts 9.5k → 61.6k), which hurt the
   1k-db hold phase, and costs memory and fds (peak RSS 3.8 → 7.1 GB, fds 15k → 55k at 5k dbs).
4. Cost per open database (actor, one connection): ~3 fds (db, wal, shm), ~2–3 goroutines, ~0.6–0.8 MB RSS at
   5k dbs (from `samples.csv` deltas; peak RSS only, so an estimate).
5. OS threads reached 1,506 in actor mode even with pure-Go SQLite: blocking file syscalls hold a thread each.

Caveat: the global-lock mode is a worst case (every statement on one thread). The platform it was compared
with at the time already ran unbounded reads on a 32-permit blocking pool and had per-file writer queues, so
it was not an exact replica of that platform.

Not measured: `ramp` (to 10k dbs), `retry-storm` (good vs no-jitter vs bad clients under overload), `hot`
(zipf), lower statement timeouts, admission control for expensive query classes.
