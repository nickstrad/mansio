# sqlbench — one goroutine (actor) per SQLite database, under load

Owner: sqlbench agent (live plan/status: `/Users/nick/Software/mansio/server/STATE.md`). Code: `cmd/bench/sqlbench`,
`internal/{actor,schema,workload,retry,load,stats}`, `queries/report.sql`, `queries/compare.sql`, `configs/*.json`
(not `configs/coldload/`).

Question it answers: if mansio's SQLite work stops sharing one thread / one lock, how much does that buy, and
what is the next limit?

## Findings (newest first)

### 2026-11-05 — durable commit on Linux (CP-8)

[Linux fsync](2026-11-05-linux-fsync.md): in Docker Desktop's Linux VM a `synchronous=FULL` commit costs ~93 µs
(one txn per write) vs 60 µs on macOS without `F_FULLFSYNC`; group commit brings FULL to 4.3 µs per write.
Indicative only (virtual disk); re-measure on target hardware.

### 2026-11-04 — one actor's ceiling vs Cloudflare's per-object limit

See [2026-11-04-cloudflare-durable-objects.md](2026-11-04-cloudflare-durable-objects.md). One actor: ~151k point
reads/s, ~162k ops/s at 20% writes, 319k writes/s with group commit (58k without). Cloudflare's documented soft
limit is ~1,000 req/s per Durable Object; their speed tricks (same-thread synchronous SQLite, write coalescing
behind output gates, durability by shipping the WAL to a 3-of-5 quorum instead of fsync) map onto ours, and WAL
shipping is the piece we do not have yet.

Note (2026-11-04): the user retired the global-lock baseline below as a known-bad design; it is no longer run.

### 2026-11-04 — actor vs global lock vs actor + read pool

Machine: Apple Silicon, 18 cores, 128 GB, macOS, Go 1.26.4, `modernc.org/sqlite` (pure Go), DuckDB 2.0.0.
Configs (snapshots as run; they predate the 2026-11-04 refactor and hold the retired `"mode"` field, so they no
longer load. Runnable versions: `configs/compare-*.json`): `compare-actor.json`, `compare-actor-readpool.json` (`read_conns: 4`), `compare-global-lock.json`
(copies in this folder) — identical seed, schemas, mix and schedule; only the mode/read pool differ.
Workload: default mix incl. ≈1% `slow_cte`/`cross_join` that run until the 200 ms statement timeout; uniform
traffic; 2048 requests in flight max; ~915k scheduled requests per run.
Command: `bin/sqlbench -config configs/<c>.json -out results/<c>` then `scripts/compare.sh`.
Full output: [2026-11-04-compare-output.txt](2026-11-04-compare-output.txt). Raw: `results/compare-*/`.

| Phase (dbs, target qps) | actor: ok qps / ok% / p50 / p99    | actor + 4 read conns          | global lock                 |
| ----------------------- | ---------------------------------- | ----------------------------- | --------------------------- |
| 1k dbs, ramp → 11k avg  | 10,893 / 99.1% / 0.18 / 134 ms     | 10,793 / 98.1% / 4.0 / 222 ms | 220 / 2.0% / 567 / 2,121 ms |
| 1k dbs, hold 20k        | 19,798 / 99.0% / 0.79 / 167 ms     | 17,886 / 89.4% / 60 / 455 ms  | 45 / 0.2%                   |
| 5k dbs, ramp → 30k avg  | 8,856 / 29.6% / 21 / 1,327 ms      | 13,883 / 46.3% / 0.11 / 722 ms | 17 / 0.06%                 |

Measured conclusions:

1. Removing the shared lock is the dominant effect: ~20k ok qps vs ~220 on the same machine and work. Under one
   lock a single 200 ms statement stalls every database (lock-wait p99 1–1.8 s).
2. The next limit is CPU (~14 of 18 cores busy at saturation), much of it spent on the slow 1% until they are
   interrupted. Past ~20k qps the 2048 in-flight cap fills (`gen_full`).
3. Within one database, a single connection causes head-of-line blocking: p99 is almost all queue wait behind
   slow queries on that same database. A 4-connection read pool fixes it at 5k dbs (p50 21 → 0.11 ms, +57%
   throughput) but lets more doomed slow queries run at once (stmt timeouts 9.5k → 61.6k), which hurt the 1k-db
   hold phase, and costs memory and fds (peak RSS 3.8 → 7.1 GB, fds 15k → 55k at 5k dbs).
4. Cost per open database (actor, one connection): ~3 fds (db, wal, shm), ~2–3 goroutines, ~0.6–0.8 MB RSS at
   5k dbs (from `samples.csv` deltas; peak RSS only, so an estimate).
5. OS threads reached 1,506 in actor mode even with pure-Go SQLite: blocking file syscalls hold a thread each.

Caveat: the global-lock mode is a worst case (every statement on one thread). mansio already runs unbounded reads
on a 32-permit blocking pool and has per-file writer queues, so it is not an exact replica of mansio.

Not yet measured: `ramp` (to 10k dbs), `retry-storm` (good vs no-jitter vs bad clients under overload), `hot`
(zipf), lower statement timeouts, admission control for expensive query classes.
