# FFI-01 — production FFI SQLite binding (brief)

Status: built, awaiting review · Branch: `item/ffi-binding` · Worktree: `/root/Software/mansio-work/ffi-binding`
· Base: `main` at `dedef4f` · Builder: Claude Opus 5.5 (`claude-ffi-binding`) · Date: 2026-10-13 · Task: Nick,
`/root/ffi-build-task.md`.

## Why

Fable's language-switch study (`docs/architecture/lang-switch-fable.md` on `research/fable-lang-switch`, §3
and §8) measured that Deno FFI to the system `libsqlite3` gives what `node:sqlite` lacks: a `nonblocking`
`sqlite3_step` that runs on a pool thread while the event loop stays live, and `sqlite3_interrupt` from the
main thread that stops it in under a millisecond. Its recommendation step 1 is an own binding of about 200
lines. This item is that binding, production quality, and nothing that uses it yet.

## Scope

- `platform/sqlite_ffi/`: load and validate `libsqlite3` (version ≥ 3.37, threadsafe); open/close; exec;
  prepare (exactly one statement); bind positional and named parameters; synchronous `run`/`get`/`all`;
  nonblocking `runAsync`/`getAsync`/`allAsync` with a per-execution deadline (`timeoutMs`) and `AbortSignal`,
  both enforced by `sqlite3_interrupt`; `closeAsync` that cancels and waits; typed errors.
- Tests proving: (a) a never-ending query is cancelled within a few ms of its deadline while the event loop
  stays responsive; (b) point-read p50 parity with `node:sqlite`; (c) ordinary CRUD correctness.

Out of scope (follow-up items): moving `platform/sqlite.ts` / `platform/document_reader.ts` onto the binding;
mapping the binding's tags into `core/errors.ts`'s `PlatformError`; a bounded statement pool with per-tenant
caps; the statement classifier; lanes. `deno.json` is untouched (item-01 rule): the tests sit under
`tests/unit/` and run in `deno task test`.

## Acceptance criteria

1. `deno task lint`, `check`, `test`, `test:prop` green; `deno fmt --check` clean for every file this item
   owns. (`deno task fmt:check` is red on `main` at `dedef4f` from three files outside this item: `README.md`,
   `docs/knowledge/decisions.md`, `docs/architecture/README.md`.)
2. Cancellation: `tests/unit/sqlite_ffi_cancel_test.ts` — an unbounded recursive CTE under `timeoutMs` rejects
   `SqliteCancelled({ reason: "deadline" })`, each cancel latency < 25 ms and the median of 5 < 5 ms; a 2 ms
   timer chain keeps ticking and point reads on another connection keep running throughout; same for
   `AbortSignal`; an interrupted write inside `BEGIN` rolls back; four concurrent connections all meet their
   deadlines; the connection is reusable afterwards (no lingering interrupt).
3. Parity: `tests/unit/sqlite_ffi_perf_test.ts` — FFI sync point-read p50 ≤ 1.3 × `node:sqlite` p50 + 1 µs
   over 24,000 interleaved reads per path, same results from both.
4. CRUD: `tests/unit/sqlite_ffi_crud_test.ts` — insert/select/update/delete sync and async, transactions,
   every storage class round-trips (including text with U+0000, empty text, empty blob, int64 extremes under
   `readBigInts`), constraint failures carry extended codes, and each contract breach is a typed error.
5. Resource safety: no call can free a connection or statement while a pool thread uses it (`"busy"`), use
   after close/finalize is a typed error, and the library is never `dlclose`d.

## Design decisions for the reviewer

- **One execution per connection.** While an async execution is in flight every other call on that connection
  fails `SqliteUsageError("busy")`. Alternatives were blocking on SQLite's FULLMUTEX (stalls the event loop,
  defeating the point) or queueing (hides back-pressure; belongs in the pool item).
- **Interrupt only while inside a step.** `cancel` calls `sqlite3_interrupt` only when a pool thread is in
  `sqlite3_step_nb`; between steps it sets a flag the loop checks. With every execution running to completion
  and resetting, no other statement is active on the connection, so a racing interrupt is cleared by SQLite
  when the next statement starts.
- **Errors are binding-local tags** (`platform/sqlite_ffi/errors.ts`) with structured fields, like
  `core/errors.ts`; joining them to `PlatformError` is the migration item's call.
- **Sync calls are unbounded by design**, as the study prescribes: the thread that would interrupt is the one
  running the statement. A progress-handler watchdog was rejected: the same connection also steps on pool
  threads, where a JS callback would be called off its isolate thread.
- **Point-read cost** comes from cached column names (invalidated by `SQLITE_STMTSTATUS_REPREPARE`), a
  `getCString` fast path for ASCII text, int32 binds and `column_double` integer reads (no bigint), and
  skipping `clear_bindings` unless SQLite holds a text/blob copy.

## Measured (vm-1, 2026-10-13, load 0.8–2.0)

Cancel latency 0.12–0.5 ms (p99 1.7 ms over 20 runs); event loop max gap 3–12 ms during a 150–200 ms
statement. Point read p50 µs: `node:sqlite` 7.8–8.6, FFI sync 7.5–8.6, FFI nonblocking 46–93 (p99 0.3–1.5 ms).
Reproduce with `work-scripts/deno/ffi-binding-bench.ts`.

## Repair (2026-10-13)

Astra's review of `acdef43` (`agent-work/items/ffi-binding/review-astra.md` on `review/ffi-01-astra`) returned
FAIL with three blocking defects. The repair agent was Claude Opus 5.5 (`opus-ffi-01-fix`, worktree
`/root/Software/mansio-work/ffi01-fix`). It made these fixes in `platform/sqlite_ffi/database.ts`:

1. **Cancel race in the dispatch-to-entry window.** A cancelled async execution with a step outstanding calls
   `sqlite3_interrupt` at once and again every 1 ms until that step's promise settles. The timer is cleared
   synchronously in the continuation, so no interrupt reaches the connection's next execution (the invariant
   is in the comment on `Execution`). Test: `race:` in `sqlite_ffi_cancel_test.ts`, a subprocess under
   `timeout` with these cases:
   - 100 immediate aborts
   - 50 runs with `timeoutMs: 0`
   - 50 immediate `closeAsync` calls
   - 16 connections aborted in one tick
   - reuse of each connection afterwards
2. **Reentrant getters.** Sync and async executions reserve the connection (`Connection.active`; sync uses
   `SYNC_EXECUTION`) before they evaluate caller code: parameter and option getters, array accessors and the
   signal's methods. Nested `finalize`, `close`, `prepare`, `exec` or execution then fails
   `SqliteUsageError("busy")`. `exec` and `prepare` encode their SQL before the lifetime check. Tests:
   `reentrancy:` in `sqlite_ffi_crud_test.ts`. They run the review's two probes plus a 6 × 11 matrix of entry
   shapes and nested calls, each in a subprocess under `timeout` with `ulimit -c 0`.
3. **Ignored reset errors.** The result code of `sqlite3_reset` is checked on otherwise successful executions
   and fails `SqliteError({ op: "reset" })`, with `"reset"` added to `SqliteOp`. An earlier step or
   cancellation error is kept, and the statement is still reset and the connection released. Test:
   `reset errors:` (sync and async `get` of `INSERT ... RETURNING` under a reader's lock gives `SQLITE_BUSY`,
   no lost row, then reuse).

`Database.interrupt()` remains a single unconditional call. Its doc comment now states SQLite's semantics: the
call can miss a step that is still being dispatched, and it cannot carry over to the next execution.

### Second repair (2026-10-13)

Astra's re-review of `9eec83b` (`agent-work/items/ffi-binding/rereview-astra.md` on
`review/ffi-01-astra-rereview`) returned FAIL with two defects in async signal cleanup. The second repair was
made by Claude Opus 5.5 (`opus-ffi-01-fix2`, worktree `/root/Software/mansio-work/ffi01-fix2`), commit
`e45b2ae`:

1. 2026-10-13: **Native reads after caller cleanup.** Async cleanup now resets the statement and builds the
   pending reset error (or `SqliteCancelled`) while it still owns the open connection. It then clears the
   timer and bound copies, releases the connection and settles waiters in a `finally`. Only after that does it
   call `signal.removeEventListener`. The sync path also reads its reset error before release. Test:
   `cleanup: signal detachment that closes or reuses the connection ...` in `sqlite_ffi_crud_test.ts`
   (subprocess under `timeout`, `ulimit -c 0`; the hook is a method or getter that closes, runs `closeAsync`,
   runs a sync call, runs a failing sync call, or starts an async call).
2. 2026-10-13: **A throwing cleanup replaced the original error.** Signal detachment runs in its own
   `try`/`catch`. A pending typed error wins, with the cleanup exception as a non-enumerable `cause`. A
   caller's own exception is rethrown untouched. After a success, the exception becomes
   `SqliteUsageError({ reason: "signal-cleanup", cause })`, a new additive `UsageFailure`, and the statement's
   changes stand. Tests: `cleanup: a throwing signal detachment (method|getter) ...` in
   `sqlite_ffi_cancel_test.ts`.

### Third repair (2026-10-13)

2026-10-13: Astra's second re-review of `ca348ea` (`rereview2-astra.md` on `review/ffi-01-astra-rereview2`)
found that cleanup attached its `cause` by error class, so it could change a caller-owned binding error or
replace it with a `TypeError`. Claude Opus 5.5 (`opus-ffi-01-fix3`) changed `database.ts` to decide ownership
by identity (`Own`), so that only the error object the execution created gets a `cause`. The attach step
swallows its own failures, and anything thrown from caller code is rethrown without being inspected. Tests:
`cleanup: a throwing signal detachment (method|getter) never touches an exception the caller threw` in
`sqlite_ffi_cancel_test.ts`.
