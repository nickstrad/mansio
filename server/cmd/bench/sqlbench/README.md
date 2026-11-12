# sqlbench: one goroutine per SQLite database

The load test the server's actor design came from. It measures how far one Go process can go running **one goroutine (actor) per SQLite
database**, with no lock shared between databases.

Paths and commands below are relative to `server/`.

## Run

```sh
make sqlbench-check                        # gofmt, vet, tests with -race and -tags noassert, end-to-end test
make sqlbench-run CONFIG=smoke             # build, run ~30 s under the bench lock, print the DuckDB report
scripts/run.sh configs/smoke.json          # the same without make
bin/sqlbench -config configs/ramp.json -dry-run
scripts/report.sh results/<run>            # report for one run
scripts/compare.sh                         # every run under results/ side by side
```

Needs Go 1.26+ and, for analysis only, the DuckDB CLI (`brew install duckdb`). The benchmark itself writes CSV
files and never loads DuckDB.

Exit codes: 0 done, 1 failed (one line on stderr with a log ID to find in `sqlbench.log`), 2 bad usage,
70 bug (report the log ID), 130 interrupted (Ctrl-C stops sending, finishes what is in flight, writes every file).

## What a run does

1. For each phase, grows the set of databases to `dbs`. Each new database gets a random schema (tables, typed
   columns, indexes, parent/child links) and seed data, reproducible from `seed` and its index.
2. Sends requests at the phase's rate (open loop: the schedule does not slow down when replies do). Each request
   is a SQL string generated for that database's schema, from a weighted mix of workload classes.
3. Each request goes to its database's actor. The actor batches queued writes into one transaction (group
   commit), runs reads after the commit or on its read pool, interrupts statements at `statement_timeout`, drops
   requests that waited past `request_timeout`, and sheds when its mailbox is full. Nothing blocks the caller.
4. Each client profile applies its retry policy to failed attempts.
5. Every attempt is streamed to `requests.csv`; resources are sampled to `samples.csv`.

## Config

JSON, unknown fields rejected. Everything has a default; a minimal config is just `phases`.

```jsonc
{
  "name": "ramp",
  "workers": 2048,                 // max requests in flight
  "statement_timeout": "200ms",    // SQLite is interrupted after this
  "request_timeout": "2s",         // a request still queued after this is dropped unrun
  "sample_every": "250ms",
  "record_fraction": 1.0,          // sample requests.csv if runs get huge
  "seed": 1,
  "keep_dbs": true,                // reuse seeded databases next run (same seed + schema settings)
  "actor": { "mailbox_size": 256, "max_batch": 64, "read_conns": 0, "idle_close": "0s",
             "journal_mode": "WAL",       // WAL | DELETE | TRUNCATE | PERSIST | MEMORY | OFF
             "synchronous": "NORMAL",     // OFF | NORMAL | FULL | EXTRA
             "cache_kib": 256, "stmt_cache": 32 },
  "schema": { "min_tables": 1, "max_tables": 5, "min_columns": 2, "max_columns": 8,
              "min_rows": 50, "max_rows": 500, "index_prob": 0.3, "parent_prob": 0.5 },
  "workload": { "slow_cte_iterations": 50000000, "bulk_min": 5, "bulk_max": 50, "range_max": 200 },
  "mix": { "point_read": 35, "index_lookup": 12, "range_scan": 6, "filter_scan": 3, "aggregate": 3,
           "join": 5, "top_n": 2, "insert": 12, "update": 10, "delete": 2, "upsert": 6,
           "bulk_insert": 3, "slow_cte": 0.5, "cross_join": 0.5 },
  "distribution": { "kind": "uniform" },          // or { "kind": "zipf", "s": 1.2 } for hot databases
  "clients": [
    { "name": "good", "share": 0.7, "retry": { "max_attempts": 3, "backoff": "exponential", "base": "20ms",
      "max": "1s", "jitter": "full", "retry_on": ["shed", "aborted", "expired"], "budget_ratio": 0.1,
      "total_timeout": "3s" } },
    { "name": "bad", "share": 0.3, "retry": { "max_attempts": 10, "backoff": "none", "jitter": "none",
      "retry_on": ["any"] } }
  ],
  "phases": [
    { "name": "grow", "dbs": 1000, "duration": "20s", "start_qps": 1000, "end_qps": 20000 },
    { "name": "hold", "dbs": 1000, "duration": "20s", "start_qpm": 1200000, "shape": "step" },
    { "name": "wave", "dbs": 5000, "duration": "40s", "start_qps": 5000, "end_qps": 40000,
      "shape": "sine", "period": "20s", "mix": { "point_read": 1, "insert": 1 },
      "client_shares": { "bad": 1 } }
  ]
}
```

- **Rate**: `start_qps`/`end_qps` or `start_qpm`/`end_qpm`. `shape`: `linear` (start → end), `step` (hold
  start), `sine` (oscillate start ↔ end with `period`). Up/down patterns are consecutive phases.
- **Workload classes**: reads `point_read`, `index_lookup`, `range_scan`, `filter_scan` (unindexed, full scan),
  `aggregate`, `join`, `top_n`; writes `insert`, `update`, `delete`, `upsert`, `bulk_insert`; meant to time
  out: `slow_cte` (CPU-only recursive CTE), `cross_join` (three-way cartesian product).
- **Retry**: `backoff` none/fixed/exponential; `jitter` none/full/equal/decorrelated; `retry_on` any of `shed`,
  `aborted`, `expired`, `stmt_timeout`, `client_timeout`, `error`, or `any`; `budget_ratio` caps retries per
  first attempt; `total_timeout` stops retrying after that long. Per-phase `client_shares` switches profiles.

- **Journal mode**: `read_conns > 0` only helps in WAL (readers run alongside the writer); in rollback modes
  readers and the writer block each other, and the run prints a warning. The requested mode is verified on open.

Shipped configs: `smoke`, `ramp`, `spike`, `hot`, `retry-storm`, `compare-actor`, `compare-actor-readpool`.

## Output (`results/<run>/`)

| File           | One row per                       | Key columns                                                                                          |
| -------------- | --------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `requests.csv` | attempt                           | phase, client, attempt, final, db, class, err, batch, latency/e2e/queue/exec/commit µs               |
| `samples.csv`  | sample tick                       | goroutines, OS threads, heap, peak RSS, fds, CPU seconds, GC pause p99, scheduler p99, queues        |
| `phases.csv`   | phase                             | dbs, setup cost (seed/open ms), start/end, target rate, sent, generator-full                         |
| `schemas.csv`  | table of each database            | columns, indexes, parent, seed rows, column types                                                    |
| `run.json`     | run                               | resolved config, Go version, CPUs, file-descriptor limit                                             |
| `sqlbench.log` | event                             | JSON lines, UTC: plan, phases, summary; failures with log ID, error chain and stack                  |

`err` values: `ok`, `shed` (mailbox or read queue full), `stmt_timeout`, `expired` (waited past its deadline,
never ran), `client_timeout`, `gen_full` (load generator had no free worker), `aborted` (batch rolled back
because another write in it timed out), `closed`, `error`.

## Layout

```
cmd/bench/sqlbench        command line: flags, logging, exit codes (realMain)
internal/bench      one run: grow databases, pace requests, workers, retries, drain, write files
internal/actor      the actor: mailbox, group commit, read pool, timeouts, passivation
internal/outcome    the outcome codes of an attempt (ok, shed, stmt_timeout, ...) and sets of them
internal/schema     random per-database schemas and seed data
internal/workload   SQL generation per workload class
internal/retry      retry policies and budgets
internal/load       config, rate shapes and pacer, request picker
internal/stats      streaming CSV writer and resource sampler
queries/            report.sql (one run), compare.sql (all runs)
configs/            run configs
```

## Tests

The approach follows `docs/notes/testing/` and `docs/notes/concurrency-in-go/`. Each package keeps its decisions
in pure code: `actor/plan.go`, `retry`, `load`, `bench/plan.go` and `stats.RecordFormat`. The I/O sits around
that core.

- **Specification tables** with on and off points at every boundary: config validation, rate shapes, the
  pacer, retry decisions, outcome classification, CSV rows.
- **Contracts**: the actor's connection port (`executor`) has one contract suite. It runs against the real
  SQLite adapter and against the fault-injecting fake (`actor/contract_test.go`), so tests written against the
  fake mean something.
- **Fakes with fault switches** (`actor/fake_test.go`) cover failure paths SQLite cannot be made to hit on
  demand: BEGIN/COMMIT failure, open failure, a statement blocked until its timeout.
- **Properties (fuzzing)**:
  - `FuzzActorModel`: a byte script drives a real actor and is checked against a map model.
  - `FuzzGenerate`: any valid options give a schema SQLite accepts.
  - `FuzzStatementsRun`: every generated statement runs.
  - `FuzzPacerSendsTheIntegratedRate`, `FuzzDelayBounds`, `FuzzRecordFormatRoundTrip`, `FuzzParse`.
- **Assertions**: pre/postconditions and invariants use `internal/assert`. They compile out with
  `-tags noassert`, and every suite passes in both builds. `TestAssertionsCanFire` in each package shows each
  key assertion can fail.
- **Integration**: `bench` runs sub-second configs in-process. It checks the files, that every request ends in
  exactly one final attempt (even under overload), cancellation, and that kept databases are reused.
- **System**: `cmd/bench/sqlbench` drives `realMain` for every exit code and checks that a failure's log ID is in the
  log with its chain. `-tags e2e` builds and runs the real binary.
