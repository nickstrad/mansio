# STATE — ts-db-go SQLite actor PoC

Last updated: 2026-11-04 (keep this file current; newest status at the top of "Status").

## Goal

Decide whether rewriting all or part of ts-db (`/Users/nick/Software/ts-db`, TypeScript/Deno) in Go fixes its
main scaling problem: every tenant's SQLite work shares one JavaScript event loop and a few global locks (see
`/Users/nick/Software/ts-db/docs/architecture/scaling_issues.md`, issues 2–5). This PoC measures one Go process
that runs **one goroutine (actor) per SQLite database**, with no shared lock. (A one-global-lock baseline was
measured once, F1, then retired at the user's request.) It is greenfield: nothing here
imports ts-db.

Questions the runs should answer:

1. How many open databases and requests/s can one process sustain, and at what latency?
2. Does a slow or timed-out query on one database delay the others, or other requests on the same database?
3. What does each open database cost (memory, file descriptors, goroutines)?
4. Does group commit (many queued writes, one transaction) help under write load?
5. How do client retry patterns (good: backoff + jitter + budget; bad: immediate, retry everything) change load
   and outcomes when databases are overloaded?

## Design (as built)

- `internal/actor`: one goroutine per database, one write connection plus an optional read pool
  (`read_conns`), bounded mailbox. `Submit` never blocks (full
  mailbox → `ErrMailboxFull` = "shed"). Each batch runs all writes in one `BEGIN IMMEDIATE … COMMIT` (group
  commit), then each read on its own, outside the transaction, so a slow read never holds the write lock.
  Per-statement timeout interrupts SQLite via context; a request still queued past its deadline is answered
  `ErrExpired` without running; a timed-out write rolls back its batch (`ErrAborted` for batch-mates). Optional
  idle passivation (close the file after `idle_close`, reopen on demand). `journal_mode` (default WAL) and
  `synchronous` (default NORMAL) are configurable.
  Driver: `modernc.org/sqlite` (pure Go, no cgo), so a running statement holds a goroutine, not an OS thread.
- `internal/schema`: random but reproducible schema per database (1–N tables, typed columns, secondary indexes,
  parent/child links), seeded from (run seed, db index); `Catalog` tracks every database's schema.
- `internal/workload`: turns a schema into SQL strings per class — reads (`point_read`, `index_lookup`,
  `range_scan`, `filter_scan`, `aggregate`, `join`, `top_n`), writes (`insert`, `update`, `delete`, `upsert`,
  `bulk_insert`), and queries meant to time out (`slow_cte`, `cross_join`).
- `internal/retry`: client retry policies — max attempts, backoff (none/fixed/exponential), base/max delay,
  jitter (none/full/equal/decorrelated), which errors to retry, retry budget, total timeout.
- `internal/load`: JSON config, phases (db count, rate start→end in qps or qpm, shape linear/step/sine, mix,
  distribution uniform/zipf, client shares), picker.
- `internal/stats`: streams `requests.csv` (one row per attempt) through a non-blocking background writer
  (flushes every 1 s; drops and counts if it falls behind, never blocks the load) and `samples.csv` (goroutines,
  OS threads, heap, peak RSS, fds, CPU, GC pauses, scheduler latency, queue depth, in-flight) on a timer.
- `internal/outcome`: the outcome codes of an attempt (`ok`, `shed`, `stmt_timeout`, ...) and sets of them; used
  by retry policies, records and the CSV.
- `internal/bench`: one run. Grows databases per phase (`actor.Seed` builds under a temp name and renames), paces requests
  with `load.Pacer` (1 ms ticks, open loop), a worker pool sends them, and failed attempts are re-queued by timer
  per the client's policy and budget. One outstanding-work counter drives the drain, and a worker panic is
  re-raised in the caller as `*panicx.Panic`; on interrupt, pending backoffs are cancelled. Pure parts are in `bench/plan.go`.
- `cmd/sqlbench`: `realMain` with exit codes 0/1/2/70/130, a JSON log `results/<run>/sqlbench.log` with log
  IDs, and Ctrl-C handling via `signal.NotifyContext`.
- Analysis is offline with the DuckDB CLI: `queries/report.sql` (one run, 16 sections), `queries/compare.sql`
  (all runs side by side). DuckDB is not a runtime dependency.

## Coordination with the coldload agent

Another agent worked in this repo. It finished on 2026-11-04 20:09 and archived its state file to
`docs/state_files/STATE-coldload.md`; its work is committed on branch `coldload`. Rules both follow: `knowledge/AGENTS.md`.

- **I own**: `STATE.md`, `cmd/sqlbench/`, `internal/{actor,bench,outcome,schema,workload,retry,load,stats}/`,
  the `sqlbench` section of `Makefile`, `queries/report.sql`,
  `queries/compare.sql`, `scripts/{run,report,compare}.sh`, `configs/*.json` (top level), `results/` except
  `results/coldload/`, `knowledge/sqlbench/`, `README.md`.
- **They own**: `docs/state_files/STATE-coldload.md`, the rest of `Makefile`, `cmd/coldload/`, `internal/{vactor,objstore}/`, `configs/coldload/`,
  `scripts/coldload/`, `queries/coldload/`, `results/coldload/`, `deploy/seaweedfs/`, `knowledge/coldload/`.
- **Shared API they import — keep stable**: `actor.New(id, path, Config)`, `(*Actor).Start(openNow bool)`,
  `(*Actor).Do(ctx, sql string, write bool, args ...any) Result`, `(*Actor).Close()`, `actor.Config`;
  maybe `stats.NewSampler`, `stats.Gauges`, `stats.NewCSV`. Any change to these is logged here first, under
  "API changes".
- `go.mod`/`go.sum`: additions only.

**Shared coordination log:** `shared_state.md` (append-only, both agents). Append with
`scripts/shared-note.sh sqlbench "<topic>" < body.md` — never edit it by hand, so appends cannot interleave.
**Locks:** use `scripts/lock.sh <lock> <command...>` for anything both agents may write or run: `shared_state`
(via shared-note.sh), `go.mod`, `knowledge/index.md`, `knowledge/AGENTS.md`, and `results/.bench-lock` for any
benchmark run (`scripts/run.sh` does this). Stale locks (owner pid dead) are cleared automatically.
**Heavy CPU (benchmarks and any `-fuzz` longer than a few seconds) only under `scripts/lock.sh results/.bench-lock`.**
Coding/testing rules from coldload's `knowledge/testing/` and `knowledge/concurrency-in-go/` apply here too.
Pattern and scripts documented for reuse in `knowledge/multi-agent-coordination/`.
**Shared packages:** `internal/assert`, `internal/errs`, `internal/logx` are owned by coldload (see
shared_state.md 19:50); sqlbench uses them and only adds helpers additively.

API changes (newest first):
- 2026-11-04 (simplify + testing pass): the actor API coldload uses (`New`, `Start`, `Do`, `Close`, `IsWrite`,
  `Config{ReadConns, CacheKiB}`) is unchanged. Changes:
  - `NewDirectory(cfg)` lost its dir argument. `Grow(n, path, seed, workers)` is all-or-nothing.
    `Directory.Stats()` replaces `OpenCount`/`QueueDepth`.
  - `actor.Seed(path, cfg, ddl, fill)` is atomic (temp name + rename: path is complete or untouched); errors wrapped.
  - The DSN now escapes the path; paths with `#` or `?` used to open the wrong file.
  - `stats.NewRequestWriter(path, RecordFormat)`, and `Record.Err` became `Record.Outcome` (`outcome.Code`).
  - `stats.NewCSV(path, header...)`, and `Row(values...)` returns an error.
  - `schema.Options.MinColumns` must be at least 1.
  - The CSV column names are unchanged.
- 2026-11-04: **Removed** `actor.Config.GlobalLock` and `actor.Result.LockWait` (the global-lock baseline is
  retired at the user's request). **Added** `actor.Config.JournalMode` (default "WAL"; WAL, DELETE, TRUNCATE,
  PERSIST, MEMORY, OFF) and `actor.JournalModes` / `actor.SynchronousModes`; `Synchronous` now also accepts
  EXTRA and is upper-cased. Opening a connection now fails if SQLite did not apply the requested journal mode.
  `requests.csv` lost the `lock_us` column. New/Start/Do/Close signatures unchanged.
- 2026-11-04: `actor.Config` gained `ReadConns int` (read pool per database; 0 = old behavior). Additive; no
  signature changed. `Result` gained nothing new since `LockWait`/`TimedOut`.

## Files to read first

`README.md` (usage, config reference, output columns), `internal/actor/actor.go` (the design under test),
`queries/report.sql` (what is measured), `configs/*.json`.

## How to run

```sh
cd /Users/nick/Software/ts-db-go
make sqlbench-check                             # gofmt, vet, -race, -tags noassert, e2e
make sqlbench-fuzz FUZZTIME=30s                 # every fuzz target, under the bench lock
make sqlbench-cover                             # per-function coverage, lowest first
scripts/run.sh configs/smoke.json               # build, run, print report (~30 s)
bin/sqlbench -config configs/ramp.json -dry-run # print a plan without running
scripts/report.sh results/<run>                 # report for one run
scripts/compare.sh                              # compare every run under results/
```

Configs: `smoke` (quick check), `ramp` (100 → 10k databases, up to 60k qps), `spike` (sine waves + spike),
`hot` (zipf-skewed traffic, write-heavy phase), `compare-actor` / `compare-actor-readpool` (identical except the
read pool), `retry-storm` (overload phases, one client profile at a time: good / no-jitter / bad).

## Status

- 2026-11-04: Built and tested. All packages pass `go test ./...` (actor 14 tests incl. "slow database does not
  delay others" and "global lock makes others wait"; workload runs every class on 40 random schemas; config,
  retry, stats tests). Smoke run OK: 36,943 requests on up to 500 databases, 0 records dropped; bad clients
  retried 8.7% of requests (they retry timeouts), good clients 0%.
- 2026-11-04: Smoke report showed p50 ~0.1 ms but p99 140–270 ms, almost all queue wait: requests queue behind
  `slow_cte`/`cross_join` on the same database because an actor had one connection (cross-database isolation
  works; within a database everything was serialized). Added `actor.read_conns` (per-database read pool; WAL
  readers run concurrently with each other and the writer). Tests for both behaviors pass, all with `-race`.
  `queries/report.sql` validated against real output (fixed a GROUP BY bug; scripts no longer hide DuckDB errors).
- 2026-11-04: Ran `compare-actor`, `compare-actor-readpool`, `compare-global-lock` (same seed, schemas, mix,
  schedule; ~915k scheduled requests each). Results in `results/compare-*`, summary under Findings.
- 2026-11-04: User: drop the global-lock baseline ("clearly a bad design") — removed from code, configs, CSV and
  queries. Researched how Cloudflare Durable Objects get per-object speed (notes:
  `knowledge/sqlbench/2026-11-04-cloudflare-durable-objects.md`) and benchmarked one actor's ceiling (F2).
  Made `actor.journal_mode` configurable (was hard-coded WAL); warns if `read_conns > 0` without WAL.
- 2026-11-04: Simplify + testing pass (user request).
  - Split entity from integration: pure `actor/plan.go`, `retry.Decide`, `load.Pacer`, `bench/plan.go` and
    `stats.RecordFormat`; I/O lives in the actor loop, `bench/run.go` and the stats writers.
  - Typed enums replace strings (outcome codes, backoff, jitter, shape, distribution). Config defaults are
    applied before decoding. The run loop moved from `main.go` to `internal/bench`.
  - Adopted coldload's error/log conventions: `errs.Wrap` at module boundaries, log once at the top with a log
    ID, exit codes, panic forwarding.
  - Tests: spec tables, an executor contract suite (fake + real), assertion-can-fire tests, 7 fuzz targets,
    realMain and e2e tests. Coverage of the sqlbench packages: 93%. All pass with `-race` and `-tags noassert`.
  - Bugs found by the new tests:
    - (1) The DSN did not escape `#`/`?` in paths, so two databases could share one file. Fixed, and
      coldload was told (their `template.go` has the same pattern).
    - (2) `min_columns: 0` could produce an uninsertable table. `Validate` now rejects it.
    - (3) A failed seed left an empty file that a kept directory would reuse. `actor.Seed` is now atomic.
  - Smoke rerun: each phase sent its scheduled count (±1), 37,049 attempts recorded, 0 dropped; the DuckDB
    report reads the new files unchanged.
  - Archived configs in `knowledge/sqlbench/*.json` predate the refactor (they hold the retired `"mode"` field);
    the runnable versions are in `configs/`.
- Next: (a) WAL shipping with an output gate (reply after the WAL frames of a group commit are acked by a
  replica/object store) — coordinate with coldload, who owns `internal/objstore`; (b) a lower-overhead driver
  path than database/sql; (c) `ramp`, `retry-storm`, `hot`; (d) admission control for expensive classes.

## Findings

### F2 — one actor's ceiling, and Cloudflare's per-object limit (2026-11-04)

Cloudflare documents a soft limit of ~1,000 req/s per Durable Object (not 3,500). One of our actors (one
goroutine, one connection, pure-Go SQLite) does ~151k point reads/s through the mailbox (207k calling SQLite
directly), ~162k ops/s at 20% writes, and 319k writes/s with group commit (58k without; 16.5k without batching
when every commit fsyncs). SQLite is not the per-database bottleneck; group commit is the biggest write lever.
Details, sources and what to reuse: `knowledge/sqlbench/2026-11-04-cloudflare-durable-objects.md`.

### F1 — actor vs global lock vs actor + read pool (global-lock baseline since retired) (2026-11-04, `configs/compare-*.json`)

Default mix (≈1% `slow_cte`/`cross_join` that run until the 200 ms statement timeout), uniform traffic, 2048
in-flight cap, `scripts/compare.sh`:

| Phase (dbs, target qps) | actor: ok qps / ok% / p50 / p99 | actor + 4 read conns     | global lock                   |
| ----------------------- | ------------------------------- | ------------------------ | ----------------------------- |
| 1k dbs, ramp → 11k avg  | 10,893 / 99.1% / 0.18 / 134 ms  | 10,793 / 98.1% / 4.0 / 222 ms | 220 / 2.0% / 567 / 2,121 ms |
| 1k dbs, hold 20k        | 19,798 / 99.0% / 0.79 / 167 ms  | 17,886 / 89.4% / 60 / 455 ms  | 45 / 0.2%                   |
| 5k dbs, ramp → 30k avg  | 8,856 / 29.6% / 21 / 1,327 ms   | 13,883 / 46.3% / 0.11 / 722 ms | 17 / 0.06%                 |

- **Removing the shared lock is the whole game.** Same machine, same work: ~20k ok qps vs ~220 with one lock.
  One 200 ms statement under a global lock stalls every database (lock wait p99 1–1.8 s).
- **Next limit is CPU, not coordination.** ~14 of 18 cores busy at saturation; the slow 1% burn CPU until
  interrupted. Past ~20k qps the 2048 in-flight cap fills (`gen_full`).
- **Within one database, one connection = head-of-line blocking.** p99 is ~all queue wait behind slow queries.
  A read pool fixes that at 5k dbs (p50 21 ms → 0.11 ms, +57% throughput) but lets more doomed slow queries run
  at once (stmt timeouts 9.5k → 61.6k), which hurt the 1k-db hold phase, and costs memory/fds (5 conns/db:
  peak RSS 3.8 → 7.1 GB, fds 15k → 55k at 5k dbs).
- **Per database cost** (actor, one conn): ~3 fds, ~2–3 goroutines, ~0.6–0.8 MB RSS at 5k dbs.
- `os_threads` reached 1,506 in actor mode even with pure-Go SQLite: blocking file syscalls (fsync/read) each
  hold an OS thread while in progress.

Caveat on the baseline: ts-db already runs unbounded reads off the event loop (32-permit blocking pool) and has
per-file writer queues. The global-lock mode is the worst case (every statement on one thread), closest to
ts-db's synchronous write/point-read path, not an exact replica.

Machine: Apple Silicon, 18 cores, 128 GB, macOS, Go 1.26.4, DuckDB 2.0.0, open-file limit 245,760.

## Open questions / limits

- `modernc.org/sqlite` is a C-to-Go translation: slower per statement than cgo `mattn/go-sqlite3`, but no OS
  thread per call. A cgo variant (build tag) would show the other side of that trade.
- The global-lock baseline models "one thread does all SQLite work"; it does not model ts-db's other issues
  (per-request Postgres auth, handle-cache lock). Those are design issues a Go rewrite would not fix by itself.
- Peak RSS only (macOS has no cheap current-RSS call); per-database memory is estimated from deltas.
- Reads in a batch run after that batch's writes commit (they see them). Writes run before reads in the batch,
  regardless of arrival order.

## Related files in ts-db

- `/Users/nick/Software/ts-db/docs/architecture/scaling_issues.md` — what won't scale today.
- `/Users/nick/Software/ts-db/docs/architecture/scale-out-direct-actors.md` — option 4 (actor per database).
- Both on ts-db branch `docs/scale-out-and-testing` (not merged to `main`).
