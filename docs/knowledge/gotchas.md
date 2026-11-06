# Gotchas

Traps that have actually bitten agents on this project. Read before running commands on vm-1; append when you
get bitten.

## sshvm.sh consumes stdin

`~/workspace/goals/claude-code-and-codex-sessions-on-vm-1/hidden_files/bin/sshvm.sh` runs a fingerprint SSH
before the real command, and that first SSH eats local stdin — piping file content through
`sshvm.sh 'cat > remote'` silently writes a **0-byte file**. Deliver content as base64 inside the command
string:

```bash
B64=$(base64 -w0 local-file) && sshvm.sh "echo '$B64' | base64 -d > remote/path"
```

(base64 alphabet is shell-safe in single quotes.) Verify with `wc -c` after.

## Quote heredoc delimiters through sshvm.sh

Always `<<'PYEOF'` (quoted). An unquoted delimiter lets the local/remote bash eat backticks and `$(...)` via
command substitution mid-script.

## Fingerprint every SSH session

Some connections land on a **wrong same-hostname backend** (empty `/root`, no repo). Never use raw SSH for
repo work — always go through `sshvm.sh`, which fingerprints and prints `REPO_OK`.

## Shared checkout: use worktrees

`/root/Software/mansio` is shared by concurrent agents. Inspect branch/status before acting, and give every
branch its own worktree (convention: `/root/Software/mansio-work/<slug>`). Never build two branches in one
checkout.

## Pushes can appear flaky

`git push origin main` sometimes fails silently on this VM. Push with an explicit SSH URL or
`git push <url> HEAD:main`, and always verify with `git ls-remote`.

## `claude --model haiku` is Haiku 5.5 — keep it that way

Verified 2026-10-11: the alias resolves to `claude-haiku-5-5`. If it ever drifts to ≤4.5, pin a ≥5.5 model ID
explicitly (`claude --model
claude-haiku-5-5`). Never run Haiku ≤4.5: it drops safety-critical details and
self-rates 5/5 regardless.

## Exit-139 segfault on teardown (pre-existing flake)

Intermittent native crash (`duckdb.node`) after tests print `ok` — roughly 1 in 15 runs of
`tests/property/offsets_test.ts`, also seen mid-suite in `deno task test`. Reproduced on clean main; treated
as a pre-existing DuckDB/Deno teardown flake. Policy: **chase, don't quarantine** — ~25 identical-shape
`tokio-runtime-w` segfaults in dmesg share one stack page offset (one bug site). Reusable stress loop:
`work-scripts/bash/segfault-stress.sh` (adopted from sq-07; until it lands, on `origin/haiku/sq-07`).

## Two SQLite copies in one process must not share a file

Added 2026-10-13 (FFI binding, `item/ffi-binding`). `platform/sqlite_ffi/` loads the system `libsqlite3`
(3.45.1 on vm-1) next to the SQLite bundled inside `node:sqlite`. POSIX advisory locks belong to the process,
not the descriptor, so when one copy closes any descriptor on a database file the kernel drops the locks the
other copy holds on it, and the two copies then corrupt each other silently. While both libraries are loaded
in one process, each database file is opened by exactly one of them. The perf parity test
(`tests/unit/sqlite_ffi_perf_test.ts`) uses two identical files for that reason. The same rule is why the
DuckDB reader is a separate process (plan §0).

Update 2026-10-13 (FFI-04, `item/ffi-04-cutover`): the server process no longer loads `node:sqlite` at all.
Writer, document reader, tenant router and backup verifier open through the binding, all on
`Config.sqlite.library`. Different `libsqlite3` files are separate copies too: vm-1's 3.45.1 and 3.53.4 must
never both open one file in a process, which is why every open passes the same configured path. Tests open
files through `tests/helpers/sqlite_inspect.ts` (`inspect(path)` read-only, `{ write: true }` for fixtures and
side connections). `tests/unit/boundaries_test.ts` forbids `node:sqlite` anywhere under `platform/` and
`tests/` except exact-path allow-lists, and walks both processes' runtime import graphs.

Update 2026-10-13 (FFI-04 repair 1): an allow-list entry proves only that a file _may_ import `node:sqlite`,
not that it keeps the drivers on separate files. The FFI-04 build allow-listed
`tests/property/sqlite_oracle_test.ts` as "in-memory only", but its NUL regression held `Storage` (now the
binding) on a temp file and opened the same file through `new DatabaseSync(file)` to write beneath it. Green
tests did not show it; Astra found it by tracing each open's scope. When a cutover changes which driver a
shared helper (`makeStorage`, a layer) uses, re-audit every `DatabaseSync(` in the allow-listed files: each
must open `:memory:` or a path nothing else in the process opens, for the whole time it is open.

## `Deno.dlopen` and FFI `i64`: bigint in, bigint out

Added 2026-10-13. On Deno 2.9.5 an `i64` FFI parameter's type is `bigint` only (a `number` fails `deno check`)
and `i64` results are `number | bigint`. `platform/sqlite_ffi/` binds int32 values through `sqlite3_bind_int`
and reads INTEGER columns through `sqlite3_column_double` (exact for every safe integer), falling back to
`sqlite3_column_int64` only for `readBigInts` or the unsafe-integer error, which keeps bigint allocation off
the point-read path. A dlopen'd library is not a resource for Deno's test sanitizer, so loading it lazily
inside a test is fine.

## The VM is shared and loaded

Sibling agents run concurrently; load averages of 12+ on 4 vCPUs are normal. Perf numbers (k6 p95/p99)
measured on a loaded host can be 2–3× worse than a quiet host — always record load average with a measurement
and re-check gates on a quiet host before treating them as stable.

## Long commands die on relay timeout

Don't run multi-minute commands inline through the guard. Start a tmux session on vm-1, write output to a
file, poll the file. The VM has 8 GB RAM — keep parallel workloads modest.

## `deno fmt --check` must stay green

A repo-wide formatting baseline landed; CI-equivalent discipline applies to every commit including docs. Run
`deno fmt --check` on touched markdown and `deno lint` where relevant before committing.

## Honesty rules for agent reports

Every factual claim cites pasted command output. Unexecuted checks are labeled _not executed_, never passed.
Haiku 5.5's characteristic failure mode is overstatement of secondary claims (misattributed evidence, dropped
table rows, severity inflation) — headline verdicts held, but every review corrected the details. Verify,
don't trust pastes.

## sshvm.sh fingerprint can't distinguish mansio backends

`sshvm.sh` fingerprints `/root/Software/prototyper/.git`, which exists on BOTH the right and wrong
same-hostname backends — so a "REPO_OK" fingerprint does not prove you are on the mansio backend. If
consecutive calls show missing worktrees or unfamiliar state, drop a sentinel file
(`touch
/root/Software/mansio/.<your-name>`) and retry the connection; bail out rather than working on the
wrong backend. (2026-10-11)

## 2026-10-11 — MT-07 tenant reader and preparation boundary

> **Superseded 2026-10-14 (FFI-08).** The boundary below is no longer current. Original account (2026-10-11):
> tenant queries carried a writer-resolved location over private IPC, and `platform/tenants/routing.ts` loaded
> `node:sqlite`, so it must not be imported in the reader. Current state: `platform/tenants/routing.ts`
> imports the FFI binding (`platform/sqlite_ffi/`), and no file under `platform/` imports `node:sqlite`
> (`tests/unit/boundaries_test.ts`). Document queries are intercepted in-process by `DocumentReaderClientLive`
> (`platform/layers.ts`, `platform/document_reader.ts`): they open a read-only FFI connection per handler in
> the server process and never cross IPC. Ingest and analysis still go to the reader child over IPC. The
> location the reader uses is still the writer-resolved tenant file, validated in the reader (absolute path,
> parseable tenant id); no path or tenant id is taken from HTTP arguments. See ADR 0008 for the current
> two-copies and threading rules.

Original account, kept for history: no path or tenant id is copied from HTTP arguments. The pure UUID parser
lives in `registry.ts`. Tenant readers are scoped to one query for now; a bounded, leased tenant reader pool
is a performance follow-up, not a reason to fall back to the legacy app file.

Writer initialization and query preparation must share the same successful-preparation barrier. Calling
`router.prepare` again on every query takes `BEGIN IMMEDIATE` on a second connection and can block the writer
event loop while a yielding mutation holds a transaction. Regression coverage:
`tests/unit/tenant_auth_test.ts` holds a mutation open while an authenticated query completes.

The required one-file-per-tenant layout means a tenant file has one schema/storage fingerprint. The default
columns and JSON sample apps are alternative layouts: using the other layout on an already-initialized tenant
fails closed on schema mismatch. Supporting incompatible app schemas for one tenant requires an explicit
storage-model decision; do not silently create per-app tenant files.

The key cache stores hashes only, including negative results, with a fixed (non-sliding) deadline. Admin
mutations and authentication must use the same cached registry instance for immediate local invalidation.
Another server instance observes revocation/deactivation within one TTL; there is no cross-instance
invalidation protocol.

MT-07 consumes `Config.tenancy.tenantKeyCacheTtlMs` with the specified 30000 ms default. This base lacks
MT-02's promised `TENANT_KEY_CACHE_TTL_MS` decoder; adding that option, its README row and config tests
remains a configuration-owner follow-up. Do not claim the environment variable is honored until that follow-up
lands. The injected Config field already controls the cache and rejects invalid limits.

## 2026-10-11 — MT-10 tenant fixtures and live analysis

The platform E2E suite must allocate different tenants for the column and JSON sample apps: they are
incompatible layouts of the same tables, and the one-file-per-tenant schema guard intentionally rejects
switching layouts. Better Auth cases also use a fresh server per layout to keep their login workload within
the shared per-path rate limit; credential failures still assert 401.

The reader's analysis RPC must execute the existing catalog, not the pre-item-23 placeholder. Ingest and
analysis use separate connections from the same `DuckDBInstanceCache` in `platform/reader_layers.ts`, with
matching open options. Independent native instances can give analysis stale state after a later ingest;
`tests/unit/reader_ingest_test.ts` covers ingest → analysis → another ingest → analysis. The standalone logs
CLI still opens read-only. The writer process must not import the catalog or DuckDB.

## 2026-10-13 — FFI-01 repair: three native-binding traps

Found by the FFI-01 review (`agent-work/items/ffi-binding/review-astra.md` on `review/ffi-01-astra`), fixed in
`platform/sqlite_ffi/database.ts`:

- **A pending `nonblocking` promise does not mean the native call is running.** `sqlite3_step_nb` returns its
  promise on _submission_ to Deno's blocking pool. A `sqlite3_interrupt` that lands before a pool thread
  enters `sqlite3_step` finds no running statement, and SQLite discards it
  ([docs](https://www.sqlite.org/c3ref/interrupt.html)). One interrupt can therefore be lost and an unbounded
  query never returns (100 immediate aborts hung). The fix repeats the interrupt every 1 ms until the step
  promise settles, then clears the timer synchronously in the continuation so nothing leaks into the next
  statement. Pinned by `race:` in `tests/unit/sqlite_ffi_cancel_test.ts`.
- **Every caller getter is reentrancy.** Named-parameter getters, array index accessors, proxies and option
  getters run JavaScript between the lifetime check and the native calls. A getter that called `finalize()` or
  `close()` made bind/step/reset use a freed pointer (SIGSEGV, exit 139). An execution now reserves its
  connection before it evaluates any caller code, so nested calls fail `SqliteUsageError("busy")`. Crash
  probes run in a subprocess under `timeout` with `ulimit -c 0`, so a regression fails the test and the runner
  keeps going. Pinned by `reentrancy:` in `tests/unit/sqlite_ffi_crud_test.ts`.
- **`sqlite3_reset` can be where a write fails.** `get` stops at the first row, so for `INSERT ... RETURNING`
  in autocommit mode the commit happens inside the reset that follows. If that commit fails (`SQLITE_BUSY`
  against a reader's lock), the insert is rolled back ([docs](https://www.sqlite.org/c3ref/reset.html)).
  Ignoring reset's result code returned a row that does not exist. The binding now checks it and fails
  `SqliteError({ op: "reset" })`, but keeps an earlier step or cancellation error when one exists. Pinned by
  `reset errors:` in the CRUD test.
- **Cleanup has caller code in it too (added 2026-10-13, second repair).** `signal.removeEventListener` is
  caller code: an override or getter on a real `AbortSignal` can close the connection, run another statement,
  or throw. Reading the reset's error (`sqlite3_errmsg`) after it hit a closed handle and reported "bad
  parameter or other API misuse" instead of "database is locked"; a throwing hook replaced the original
  `SqliteError` or `SqliteCancelled`. Rule for async cleanup: do the native work and build the pending error
  first, then reset and clear bindings, clear the timer, release the connection and settle waiters. Run caller
  hooks only after that, inside a `try`/`catch` that cannot replace the pending result. Pinned by the
  `cleanup:` tests in both FFI unit test files.
- **Error class is not ownership (added 2026-10-13, third repair).** To decide "this pending error is mine, so
  I may attach a `cause`", cleanup checked `instanceof` the binding's error classes. A parameter getter can
  throw a binding error the caller caught earlier: an extensible one was changed, a frozen one made
  `defineProperty` throw a `TypeError` that replaced it, and `instanceof` on a thrown `Proxy` ran its
  `getPrototypeOf` trap, which could also replace it. Rule: record the error object your own code creates and
  compare by identity (`===` runs no trap). Treat any other thrown value as opaque, and wrap the annotation in
  a `try`/`catch` so that it cannot change what is thrown. Pinned by the "never touches an exception the
  caller threw" `cleanup:` tests in `tests/unit/sqlite_ffi_cancel_test.ts`.

## 2026-10-13 — FFI-04: the binding is not a drop-in for `node:sqlite`'s defaults

Found while moving the statement path onto `platform/sqlite_ffi/` (FFI-04). Each one is invisible for valid
SQL, which is why the test suite stays green, but they differ:

- **Error text.** The binding's errors are `Data.TaggedError`s with structured fields and an empty `message`.
  Code that builds a `detail` from `e.message` gets `""`. Use `sqliteFailureMessage` in `platform/sqlite.ts`:
  `SqliteError.sqliteMessage` is the `sqlite3_errmsg` text `node:sqlite` put in `message`, and open failures
  append the file as that driver did.
- **Connection defaults (matched by FFI-04b, 2026-10-13).** FFI-04 initially inherited the system library's
  compile-time defaults (`foreign_keys=0`, DQS on), so `SELECT "nope"` returned a string instead of failing.
  Superseded by `Database.open` in `platform/sqlite_ffi/database.ts`: before any SQL it disables DQS for DML
  and DDL with checked `sqlite3_db_config` calls, then runs `PRAGMA foreign_keys=ON`. This restores
  `node:sqlite`'s unknown-column safety net and foreign-key enforcement for every connection, including
  read-only, Effect `openDatabase`, and the inspection helper. Configuration errors are captured before close;
  cleanup cannot replace them. Native error text still lives in `sqliteMessage`, not `message`.
- **Variadic `sqlite3_db_config` and Apple arm64 (2026-10-13, FFI-04b review).** `sqlite3_db_config` is
  variadic in C, but the binding declares it with a fixed `(pointer, i32, i32, pointer)` signature. That is
  correct only where variadic integer/pointer arguments travel like fixed ones (x86_64 on any OS, Linux
  aarch64). Apple arm64 passes variadic arguments on the stack, so the call would read garbage and SQLite
  could write through a garbage `int*`. `loadLibrary` therefore refuses darwin/aarch64 with a
  `SqliteLibraryError` (`unsupportedPlatform` in `library.ts`). Supporting macOS arm64 needs a variadic-aware
  call path (e.g. a small C shim), not a different FFI type list.
- **Number binds.** `node:sqlite` binds every JS `number` as REAL (`typeof(?)` of `1` is `real`) and refuses
  booleans. The binding binds integral safe numbers as INTEGER and booleans as 0/1. STRICT INTEGER/REAL
  columns and numeric comparisons do not see the difference. A TEXT-affinity comparison would (`'1'` against
  `'1.0'`), which the schema validator rules out by typing filter values.

## 2026-10-14 — FFI-05: deadlines, stuck steps and where a new error tag can live

Found while building the statement deadline (`item/ffi-05-deadline`; `platform/sqlite.ts` `ConnectionGate`,
`platform/sqlite_ffi/classify.ts`):

- **`sqlite3_interrupt` does not cut a busy-handler wait.** Measured on vm-1's 3.45.1: a nonblocking read
  queued behind another connection's `BEGIN EXCLUSIVE` (rollback journal, `busyTimeoutMs: 3000`, deadline 50
  ms) returned only after the full busy timeout, at 3047 ms, and with `SqliteError` `SQLITE_BUSY`, not
  `SqliteCancelled`. So a deadline bounds CPU-bound statements, not lock waits; keep busy timeouts on
  deadline-bounded connections short (the document reader's are 0). This is also the reproducible "step did
  not return after interrupt" fixture for the grace period (`tests/unit/storage_test.ts`, "FFI-05 grace
  period").
- **The grace period needs no binding change.** The binding's async promise settles only after its step has
  returned (`database.ts`, `Execution`), so "still unsettled `graceMs` after the deadline" is exactly "the
  step outlived its interrupt". A wall-clock watchdog beside the execution observes it; use real timers, not
  `Effect.sleep`, or a test's `TestClock` can fire it on `adjust` and poison a healthy connection.
- **One statement per connection, also inside one transaction.** While a nonblocking read is in flight the
  binding fails every other call on the connection `"busy"`, including `inTransaction`, `COMMIT` and the
  release's `ROLLBACK`. A handler that forks two reads, or a release that runs while a forked fiber's read is
  in flight, must wait. `ConnectionGate.blocking` does the wait, and its `start` must make the driver call
  synchronously: an `Effect.try` returned from it runs one op later, after a possible fiber switch.
- **A new error tag cannot join `QueryError` / `MutationError` / `RemoteReaderError` lightly.** Those unions
  are listed exhaustively in `platform/duckdb.ts`, `platform/ipc.ts` (`REMOTE_ERRORS`) and the test layers
  (`tests/layers/*`). A tag that only the in-process SQLite paths raise (`StatementDeadlineError`) joins at
  `DbError`. Likewise `ReaderClientService` is the IPC client's own type (`ReaderConnection extends` it), so
  widening its channel breaks `tests/unit/ipc_test.ts`; the `ReaderClient` tag uses the wider
  `ServerReaderClientService` instead.
- **A document query costs about 3 ms before it reads anything.** `querySqlite` for one point read took 2.8 ms
  p50 on vm-1 at load ~1 both before and after FFI-05 (open + `BEGIN` + `COMMIT` + close is 0.25 ms; the rest
  is the Effect pipeline, compile, decode and events). Wall-clock gates of a few ms on a whole call (FFI-05's
  "< 5 ms point read") sit close to that floor; measure several calls and gate the median.
- **Seeding big fixtures.** A recursive CTE (`WITH RECURSIVE n(i) AS (...) INSERT ... SELECT ... FROM n`)
  seeds 200 000 rows in about 1.3 s; one compiled insert per row took 6–10 s. In the `json` layout every
  non-key filter is a `json_extract` per row, which makes a filtered `count` over 200 000 rows take about 100
  ms per extracted field: a reliable "past the deadline" statement.

## 2026-10-14 — FFI-05 repair: a check made before a connection wait is stale after it

Found by the Astra review of FFI-05 (defect 1) and fixed in `item/ffi-05-deadline`
(`platform/document_reader.ts` `admit` and `COMMIT`; regression in `tests/unit/document_reader_test.ts`, "a
forked read queued behind an in-flight read"):

- **Repeat validity checks where the native call starts, after any wait for the connection.** Once a
  connection can be busy (a nonblocking read in flight), every caller may wait at `ConnectionGate`, and
  anything checked before that wait (the query context, poisoning) can change during it. The reader checked
  its context in `executeIn`, before the wait: a handler that forked a long count and a delayed point read,
  then returned, let the point read pass the check, queue behind `COMMIT`, and run after it: outside the
  snapshot, with a `sql` event after the `tx` event, or against the connection the release had closed
  (`ReaderError` "prepare on a closed connection"). Put the check inside the gate's start step: the `start`
  callback of `blocking`, the `guard` of `nonblocking`. An early check before the wait is fine as a fast path,
  never as the only one.
- **Expire the context in the transaction-ending synchronous step.** `context.active = false` belongs in the
  same gate callback as `COMMIT` (the writer's `transaction` already did this), not in the release that runs
  later: between the two there is a yield (an event sink, `closeAsync`), and a waiter woken by the in-flight
  read's settle can take the connection in it. A refusal for an expired context ran no statement, so it emits
  no `sql` event and does not count as an operation.
- **Gate fixtures must follow the gate's contract too.** `gate.blocking(() => Effect.sync(() => native()))`
  passes until something competes for the connection between `start` returning and its Effect running; then
  the native call fails `SqliteUsageError` `"busy"`. Call the driver inside `start` and return
  `Effect.succeed(result)`; `tests/unit/storage_test.ts` ("a blocking start does its native work before
  returning") makes that window deterministic with a hand-off to a competing `nonblocking` claim.

## 2026-10-14 — FFI-07 and FFI-08: binding facts the other driver hides

Found by FFI-07's round-trip property and the FFI-08 docs pass. Each one is pinned by a test or read from the
code.

- **`-0` binds as INTEGER 0.** The int32 fast path excludes `-0`, and the next branch,
  `Number.isSafeInteger(-0)`, binds it as an integer. `SELECT ?` returns integer `0` where `node:sqlite`
  returns real `-0`. Pinned in `tests/property/ffi_roundtrip_test.ts`; the pin moves when `-0` is bound as a
  double (plan risk 18).
- **NaN binds as NULL in both drivers.** `sqlite3_bind_double` turns NaN into NULL, so "NaN rejected" in the
  plan was wrong. The round-trip property pins NULL on both sides (plan risk 22).
- **`getCString` stops at the first NUL.** Text from `sqlite3_column_text` is read with `sqlite3_column_bytes`
  and a full-length copy. `readText` in `platform/sqlite_ffi/database.ts` keeps the `getCString` result only
  when its length equals the byte count; a truncated string can never pass that check.
- **Bare `libsqlite3.so.0` resolves to 3.53.4 on vm-1.** `ldconfig -p` maps the soname to `/usr/local/lib`,
  while `/usr/lib/x86_64-linux-gnu/libsqlite3.so.0` is 3.45.1. `DEFAULT_LIBRARY_PATHS` tries the absolute
  distro path first, so an unset `SQLITE_LIBRARY` loads 3.45.1; setting `SQLITE_LIBRARY` names the copy
  explicitly. Either way there are two copies, and each file must be opened by one of them.
- **The deadline is validated against the reader timeout, not the other way round.** `STATEMENT_DEADLINE_MS`
  must be below `READER_CALL_TIMEOUT_MS`, or the outer call gives up first and the statement is left running
  until the interrupt. Config validation refuses the combination (`platform/config.ts`).

## Deno tooling and the `pages/` npm project (2026-10-15)

`pages/` (PG-01) is an npm project inside the Deno repo. The root tasks still cover it.

- **`deno fmt <path under pages/>` ignores the root config.** With `pages/package.json` present, an explicit
  path below `pages/` resolves its own (empty) config and formats at the default width 80, while
  `deno task fmt:check` (no paths) applies `deno.json`'s 110 and then fails on those files. Format with
  `deno fmt` from the repo root without paths, or pass `--config deno.json`.
- **Explicit paths bypass `.gitignore`.** `deno fmt --check`/`deno lint` without paths skip ignored build
  output such as `pages/dist/`; `deno fmt --check pages/dist` does not.
- **Playwright's `test-results/` is not gitignored** and the root fmt check then fails on `.last-run.json`.
  `pages/playwright.config.ts` sets `outputDir` under `node_modules/`.
- **c4-sdk component views reject sibling containers without the `external` tag.** Tags are per element, so
  such containers also render grey on container views. `mansio-current` tags `reader`, `files`, `registry` and
  `jsonl` and says so in the keys (inventory decision D3); `mansio-lanes-proposal` does the same for `api`,
  `files` and `lane-jsonl` (P12; see the decision of 2026-10-15 on supporting containers).
- **`pkill -f '<pattern>'` can kill the agent's own shell (exit 144).** `-f` matches full command lines, and
  an agent's `bash -c '... pkill -f "vite preview" ...'` contains the pattern itself, so the shell kills
  itself. Stop a preview or dev server by PID (`kill $!` after starting it, or `kill $(lsof -ti :4173)`) or by
  port, never by a pattern that also appears in your own command (PG-02, 2026-10-15).
