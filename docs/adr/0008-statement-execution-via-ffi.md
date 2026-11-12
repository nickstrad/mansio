# ADR 0008 — Statement execution through the FFI binding over system libsqlite3

- **Status:** accepted
- **Date:** 2026-10-14 (decision direction 2026-10-13; built by FFI-01 to FFI-07, documented by FFI-08)
- **Deciders:** nickstrad, who approved the plan's direction on 2026-10-13 and its amended parity gate the
  same night (~23:03 CDT); the plan at `docs/plans/completed/ffi-implementation.md`
- **Related:** [ADR 0002 — error taxonomy](0002-error-taxonomy.md) (`StatementDeadlineError` row),
  [ADR 0004 — event-log DuckDB in the reader only](0004-eventlog-duckdb-in-reader-only.md),
  [ADR 0005 — text storage contract](0005-text-storage-contract.md) (U+0000 rejected before commit)
- **Implements:** `platform/sqlite_ffi/` (binding, Effect layer, classifier), `platform/sqlite.ts`,
  `platform/document_reader.ts`, `platform/tenants/routing.ts`, `platform/backup.ts`, `platform/config.ts`
  (`sqliteLibrary`, `statementDeadlineMs`, `statementInterruptGraceMs`)
- **Supersedes in part:** the driver statements in ADR 0004 and ADR 0005 that name `node:sqlite` as the
  server's SQLite driver. The server no longer loads it; those ADRs' decisions stand.

## Context

Every SQLite call in the API process ran through `node:sqlite`'s `DatabaseSync`, synchronously, on the event
loop. Two things followed:

- A statement cannot be stopped. A `count` over a large table kept the event loop, and the process's other
  requests, busy until it finished. The reader timeout (`READER_CALL_TIMEOUT_MS`) failed the HTTP call with
  502 while the statement kept running.
- The driver is bundled inside the Deno binary, so its interrupt surface is not reachable from the platform.

SQLite's C API has `sqlite3_interrupt`, which is documented as safe to call from another thread. Deno's FFI
can call `libsqlite3` directly, and `nonblocking` symbols run on Deno's blocking pool and return a promise.
The PoC on Deno 2.9.5 measured an interrupt at 0.41 ms typical (0.34 to 8.8 ms across runs) and a nonblocking
step at 44 to 69 µs p50 against 6.3 to 6.8 µs for a blocking point read with a reused statement.

## Decision

1. **One binding over the system `libsqlite3`.** `platform/sqlite_ffi/` loads the library with `Deno.dlopen`.
   `sqlite3_step` is declared twice: a blocking symbol for the main thread, and a `nonblocking` symbol that
   runs on the pool and returns a promise. `loadLibrary` checks `sqlite3_threadsafe() != 0`, refuses a library
   older than 3.37.0 (`MIN_VERSION_NUMBER = 3037000`, which `sqlite3_changes64` requires), and caches per
   path. The library is never `dlclose`d.
2. **The statement path moves to the binding; `node:sqlite` leaves `platform/`.** After FFI-04 no file under
   `platform/` imports `node:sqlite`, and `tests/unit/boundaries_test.ts` enforces it. Writer (`Storage`),
   document reader, tenant router and backup verifier switched in one commit (FFI-04), not per file or per
   path, because of §1.2 below. There is no driver flag and no dual-driver period; rollback is `git revert` of
   the cutover.
3. **Connections match `node:sqlite`'s defaults.** Every `Database.open`, read-only and inspection opens
   included, turns off double-quoted string literals for DML and DDL with checked `sqlite3_db_config` calls
   and runs `PRAGMA foreign_keys=ON` before any caller SQL (FFI-04b). `loadLibrary` refuses darwin/aarch64:
   its variadic `sqlite3_db_config` call is only sound where variadic arguments travel like fixed ones.
4. **Two execution paths, chosen once per statement from the plan IR.** `classify(operation)`
   (`platform/sqlite_ffi/classify.ts`, FFI-05) is a pure function of the `Operation`, never of SQL text:
   - _bounded_ (blocking path, main thread, never interrupted): `insert`, `patch`, `delete`, and a `select`
     with an `_id` equality filter and no aggregate. `_id` is every table's primary key, so this is at most
     one row.
   - _unbounded_ (nonblocking path): every other `select`, `count` and `countBy`. They run
     `allAsync`/`getAsync` under `sqlite.statementDeadlineMs`. At the deadline the binding calls
     `sqlite3_interrupt`, the statement stops, and the call fails
     `StatementDeadlineError { deadlineMs, elapsedMs, table, statementKind }`, which `toHttpStatus` maps to
     **HTTP 504**. Writes never take the nonblocking path. An interrupt that landed after an async write had
     committed would leave the commit standing while the caller saw a failure. An unbounded read inside a
     mutation runs nonblocking; on the deadline the transaction's release half rolls it back.
5. **Deadline and grace period.** `sqlite.statementDeadlineMs` (default 5000) must be below
   `reader.callTimeoutMs`, so the interrupt fires before the outer call gives up; config validation enforces
   it. If an interrupted step has not returned after `sqlite.statementInterruptGraceMs` (default 1000), the
   server emits an `error`-level `lifecycle` event (`statement did not return after interrupt`) and poisons
   that connection: no further statements, closed when the step returns.
6. **Every `sql` event carries `path`** (`blocking` or `nonblocking`). Since FFI-06 it is in the JSONL line
   and in the nullable `sql_events.path` column; ingest adds the column to existing logs without a
   schema-version bump, and older rows hold NULL. The named catalog gains `deadline-hits-per-handler-hour`.

### 1.2 The one rule that orders the items: two SQLite copies

**Never open the same database file through two copies of SQLite in one process.** `node:sqlite` is rusqlite's
bundled SQLite inside the Deno binary; the binding is the system `libsqlite3`. Each copy tracks POSIX advisory
locks per inode in its own process-wide table, and POSIX drops every lock a process holds on a file when any
descriptor to that file is closed. A reader closing through one copy can silently release the writer's lock
held through the other.

Consequences that shaped the design: the cutover is one commit; tests that inspect a live file use the
binding's helper (`tests/helpers/sqlite_inspect.ts`); the boundary test forbids `node:sqlite` under
`platform/`; and the DuckDB reader stays a separate process, as ADR 0004 already required. vm-1 has two
`libsqlite3` files, 3.45.1 under `/usr/lib/x86_64-linux-gnu` and 3.53.4 under `/usr/local/lib`. They are two
copies too, so `SQLITE_LIBRARY` is passed to every open and the binding caches one library per path.

### 1.3 Threading model inside the binding

- Connections open with `SQLITE_OPEN_FULLMUTEX` (serialized mode), so `sqlite3_interrupt` from the main thread
  while a pool thread is inside `sqlite3_step` is defined behaviour.
- One execution in flight per connection. While an async execution runs, every other call on that connection
  fails `SqliteUsageError("busy")` rather than waiting on SQLite's mutex from the event loop. The
  `ConnectionGate` (`platform/sqlite.ts`) serializes the callers above the binding, so a forked fiber's
  statement, `COMMIT` and the release's `ROLLBACK` queue instead of failing.
- A nonblocking step holds only SQLite-owned pointers (`sqlite3_stmt*`). No JS-owned buffer is passed to a
  nonblocking call, so the GC cannot move or collect one under the C call.
- No `UnsafeCallback`, no progress or trace callback. Busy handling is `sqlite3_busy_timeout`; cancellation is
  `sqlite3_interrupt`.
- `finalize` and `close` refuse while an execution is in flight. `closeAsync` cancels it, waits for the pool
  thread to leave `sqlite3_step`, then closes. The Effect layer (`openDatabase`) uses `closeAsync` in its
  scope finalizer.
- Text is read with `sqlite3_column_bytes` and a full-length copy. `getCString` is only a fast path, kept when
  its length equals the byte count, because it stops at the first NUL.
- A bounded per-connection statement cache (`prepareCached`, capacity 64, LRU, finalize on eviction) keeps the
  blocking point read at the measured cost. An evicted statement fails typed `"finalized"`, so callers use a
  cached statement in the same step that fetched it.

## Measurements

All numbers are from vm-1 (Deno 2.9.5, the system `libsqlite3` the binding's default search order finds). The
parity gate is a same-run relative bound (`blocking p50 ≤ 1.3 × node p50 + 1 µs`). The other gates are fixed
absolute budgets, not ratios: read 10.2 µs blocking and 100 / 3000 µs nonblocking (`perf/ffi_read_cost.ts`);
interrupt 20 ms p99 and 90 ticks (`perf/ffi_interrupt.ts`); cap 100 ms (`perf/ffi_cap.ts`); HTTP integration a
flat 50 ms p99. Host load (1/5/15-minute) is recorded for every run, for both kinds of gate. Source:
`perf/README.md`, FFI-06 section.

Quiet-host read cost, three runs, 20,000 samples per path each (`deno run -A perf/ffi_read_cost.ts`):

| Run | Load 1/5/15        | node p50 / p99 µs | FFI blocking p50 / p99 µs | FFI nonblocking p50 / p99 µs | Amended gate bound µs |
| --- | ------------------ | ----------------- | ------------------------- | ---------------------------- | --------------------- |
| 1   | 0.50 / 1.35 / 1.76 | 6.93 / 28.00      | 7.35 / 29.97              | 43.85 / 322.94               | 10.01 (PASS)          |
| 2   | 0.70 / 1.38 / 1.77 | 7.21 / 29.24      | 7.64 / 29.66              | 47.56 / 745.04               | 10.37 (PASS)          |
| 3   | 0.70 / 1.38 / 1.77 | 7.10 / 29.19      | 7.57 / 31.80              | 47.80 / 480.43               | 10.23 (PASS)          |

The original strict gate (blocking p50 ≤ node p50) fails all three runs by approximately 6.0 to 6.6 % (6.06,
5.96 and 6.62 % from the rows above). The FFI-06 review traced the gap to the floor of four FFI crossings per
read, against node's single native call. Nonblocking p50 stays under the 100 µs budget and p99 under 3 ms in
all three runs.

Interrupt and cap, quiet host at load 0.58 (review re-run): interrupt p99 4.034 ms with at least 90
ten-millisecond ticks in each window; 32 concurrent unbounded counts all cancelled with reason `deadline`,
slowest 84.5 ms from submission (gate 100 ms each). On a loaded host (load 2.1 to 2.8): interrupt p99 6.114 ms
and 32 of 32 cancels, max 71.05 ms.

End-to-end: the FFI-04 `perf:smoke` run on the same host and load showed p50 about 10.7 % lower than `main`,
with p99 roughly halved (merge `51a5283` message, six runs per side). FFI-06's integration scenario (500,000
JSON games, a 50 ms deadline on `winsByMark`, competing point reads on a second tenant) gave 20 of 20 HTTP 504
and point-read p99 of 16.44, 20.06 and 22.10 ms across three runs at loads from 1.8 to 5.6, against the flat
50 ms budget.

### The parity gate and why it was amended

FFI-06 was first written against "binding blocking p50 ≤ `node:sqlite` p50 in the same run" (FFI-01 already
used the 1.3× bound). The PoC's "6.35 vs 8.66 µs" win had been measured with `node:sqlite` run first and cold,
with no warm-up and no interleaving, at load 3 to 5, so it was biased. Measured fairly, even the PoC's
raw-symbol loop is about 5 % slower per point read than `node:sqlite`. No per-step native call can meet a
strict "not slower" gate.

On 2026-10-13 (~23:03 CDT) Nick approved the amendment that is now the gate: **blocking p50 ≤ 1.3 × node p50 +
1 µs**, the bound FFI-01 already used. The end-to-end HTTP numbers above, which are faster, show the
micro-cost does not reach users. This ADR records that the strict gate was not met, so readers do not take the
amended bound as a stronger claim than the one measured.

## Alternatives considered

- **Keep `node:sqlite` and run statements on a worker thread.** A worker cannot interrupt a synchronous
  `DatabaseSync` call, so the deadline would still be a timeout that gives up waiting. Rejected.
- **Use both drivers behind a per-file or per-path flag.** Two copies could then open one file in one process
  (§1.2). Rejected; the cutover is one commit with `git revert` as rollback.
- **Put each tenant or statement class in a child process (lanes) now.** Stronger isolation, but a separate
  design with its own plan. Deferred: the binding and connection API are written to move into a lane unchanged
  (plan §6).
- **Interrupt writes as well.** An interrupt that lands after an async write committed leaves the commit
  standing while the caller reports a failure. Rejected; writes stay synchronous.

## Consequences

Accepted, with the risks from the plan's register (`docs/plans/completed/ffi-implementation.md` §5, numbers
kept):

- **Blocking tail for wide reads (15).** The nonblocking path pays one pool hop per returned row, about 50 µs.
  That is accepted because it is confined to unbounded reads. A follow-up may batch row steps while keeping
  the cancellation and lifetime invariants.
- **A stuck native step holds its request, and a poisoned writer refuses jobs until restart (16).** By design:
  there is no lane to kill. Operators act on the error-level `lifecycle` event.
- **Event order at COMMIT (17).** A nonblocking read already in flight when a `COMMIT` queues can emit its
  `sql` event after its claim is released, so `sql` may appear after `tx`. The statement ran inside its
  snapshot. A later item may order event emission under the gate.
- **`-0` binds as INTEGER 0 (18).** The int32 fast path excludes `-0`, and `Number.isSafeInteger(-0)` binds it
  as an integer, so the sign is lost in untyped columns. FFI-07's round-trip property pins this. A fix that
  binds `-0` as a double makes the pinned assertion fail, and the value then moves to the agreeing group.
- **Parity gate (19).** Accepted as amended above.
- **Concurrency at the cap (20).** Through the Effect layer at 32 concurrent deadline hits, the largest
  cancellation reached 108 to 140 ms on 4 vCPUs. That is CPU contention delaying the main thread's timers, not
  pool starvation (the raw binding stayed under about 85 ms). Follow-up: measure at the document-reader or
  HTTP boundary, and revisit the per-tenant sub-cap (plan risk 6).
- **Timing gates under load (21).** Several timing assertions are fragile on a shared host. Only the parity
  gate is a same-run ratio; the absolute budgets are fixed. Host load is recorded with each run. Making the
  load-sensitive gates ratios, or otherwise deterministic, is the follow-up and a side-quest item.
- **NaN binds as NULL (22).** `sqlite3_bind_double` turns NaN into NULL in both drivers, so the plan's "NaN
  rejected" premise was wrong. FFI-07 pins NULL on both sides.
- **Unbounded reads can now fail with 504.** A long statement returns `StatementDeadlineError` rather than
  hanging the loop or failing 502 while still running. This is the only client-visible change; the catalog's
  `deadline-hits-per-handler-hour` shows where it happens.
- **Connection defaults are part of the binding.** Future opens must go through `Database.open`, which applies
  them. A raw `Deno.dlopen` of SQLite elsewhere would bypass DQS-off and foreign keys.
- **Known binding limit.** Apple arm64 is refused, because variadic `sqlite3_db_config` needs a C shim there.

Risk 23 (headless Muse approval stalls) is a tooling note for agents, not a property of the architecture, so
it is recorded in the plan and in the knowledge store rather than here.
