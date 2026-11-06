# FFI-05 — Classifier and deadline-bounded execution (builder brief)

Status: dispatched · Branch: `item/ffi-05-deadline` · Worktree: `/root/Software/mansio-work/ffi05` · Base:
`main` at `702c354` (FFI-04b merge) · Builder: Claude Opus 5.5 (actor `claude-opus-ffi-05`) · Reviewer: Astra
(`gpt-6-astra`), fresh unit · Date: 2026-10-13 · Authorization: Nick via `/root/fable-ffi04-go.md` ("proceed
per plan — FFI-04b next, then FFI-05").

The plan lives on branch `plan/ffi-implementation`:
`git show origin/plan/ffi-implementation:docs/plans/ffi-implementation.md`. Read §1.2, §1.3, §2 (especially
the rows on `Effect.timeout`, `Effect.die`, bind params, `prepareCached`), §3, §5 and the FFI-05 item in §4 in
full before writing code. Also read `platform/sqlite_ffi/README.md` (FFI-03's contract; the plan's
`connection.ts` is `effect.ts`), `platform/sqlite_ffi/effect.ts`, `docs/knowledge/index.md`,
`docs/knowledge/gotchas.md` (every FFI entry), `docs/plans/effect-port-and-observability.md` §0–§1 (CLAUDE.md
requires it before touching `core/` or `platform/`), `AGENTS.md`, and the live event log per
`agent-work/event-log.md`.

## Why

FFI-01..04b put the server on the FFI binding with every statement on the blocking path. This item is the
payoff: long statements run on the nonblocking path under a deadline and are stopped with `sqlite3_interrupt`,
surfacing as a typed `StatementDeadlineError` (HTTP 504), while point reads and all writes stay synchronous
and fast.

## Owns (plan)

`platform/sqlite_ffi/classify.ts` (new), `platform/document_reader.ts` and `platform/sqlite.ts` (path
selection and deadline wiring), `core/errors.ts` (additive `StatementDeadlineError`),
`platform/http/router.ts` (`toHttpStatus` row; if `toHttpStatus` lives in `core/errors.ts`, the row goes there
— same Owns), `tests/errors_exhaustiveness.ts`, `core/events.ts` (additive optional
`path: "blocking" | "nonblocking"` on `SqlPayload`), `docs/adr/0002-error-taxonomy.md` (one row),
`tests/unit/ffi_classify_test.ts` (new), `tests/unit/document_reader_test.ts` and `tests/unit/storage_test.ts`
(additive cases), `agent-work/items/ffi-05-deadline/*` (this brief; `git add -f`).

Coordinator additions: `platform/services.ts` only if a type there must widen for the new error/event field
(say so in the commit body); `tests/unit/http_test.ts` additive case for the 504 mapping if the router test
lives there.

Not owned: `platform/sqlite_ffi/{mod,library,database,errors,effect}.ts` — if the binding or Effect layer
lacks something you need (for example a way to observe "step did not return after interrupt" for the grace
period), stop and log `blocker` with the exact gap; the coordinator will decide whether to extend your Owns.
`deno.json` frozen. `sdk/*`, `app/*`, `core/compile/*` read-only. No other worktree.

## Reads

`sdk/db.ts` (`SelectPlan`, `Operation`), `core/compile/select.ts` and `ddl.ts` (`_id` is the primary key, so
`_id = ?` is the indexed single-row lookup), `platform/config.ts` (FFI-02: `sqlite.statementDeadlineMs`
default 5000, must be `< reader.callTimeoutMs`; `sqlite.statementInterruptGraceMs` default 1000),
`platform/sqlite_ffi/*`.

## Work (plan FFI-05, with findings from FFI-03's build and review)

1. `classify(operation): "bounded" | "unbounded"` — pure function on the IR, never on SQL text. Bounded:
   `insert`, `patch`, `delete`, and a `select` whose `filters` contain `{ field: "_id", op: "eq" }` with no
   `aggregate`. Unbounded: every other `select`, `count`, `countBy`. Exhaustive unit tests plus a property
   over generated `SelectPlan`s: `bounded ⇔ (_id eq filter present ∧ no aggregate)`.
2. Document reader: bounded → `all` (blocking, unchanged); unbounded → `allAsync` with the binding's own
   `timeoutMs = statementDeadlineMs`. **Do not use `Effect.timeout`/`timeoutFail` for the deadline**: in
   effect 3.22.2 it always fails `TimeoutException` and cannot carry the typed error; the binding's
   `timeoutMs` interrupts via `sqlite3_interrupt` and fails `SqliteCancelled({ reason: "deadline" })`, which
   you map to `StatementDeadlineError { deadlineMs, elapsedMs, table, statementKind }`. The `tx` event records
   `outcome: "rollback"` with that tag as for any failure. The outer `Effect.timeoutFail(callTimeoutMs)` stays
   as a last-resort guard; by FFI-02's cross-field check it cannot fire first.
3. Writer: same rule inside a mutation's transaction. Bounded statements run sync as today; an unbounded
   **read** inside a mutation (`find`/`count` in a handler) runs `allAsync` under the same deadline. **Writes
   stay sync** (an interruption landing after an async write committed would leave the commit standing while
   the fiber reports interruption). On `StatementDeadlineError` the transaction's release half rolls back. The
   writer connection still runs one statement at a time under `Semaphore(1)`; while an async read is in flight
   the binding refuses other calls on that connection with `"busy"`, so never issue a second statement on it
   until the async one settles. A `prepareCached` statement is only safe to use immediately — fetch it in the
   same step you run it.
4. `StatementDeadlineError` → HTTP **504**; `errorMessage` names the deadline; `PlatformError` union and
   `tests/errors_exhaustiveness.ts` updated; ADR 0002 row. `IpcError{timeout}` → 502 unchanged.
5. **Grace period.** If the interrupted step has not returned `statementInterruptGraceMs` after interrupt,
   emit an `error`-level `lifecycle` event (`"statement did not return after interrupt"`) and poison the
   connection: no further statements; close it when the step finally returns (`closeAsync` cancels and waits).
   If the binding gives you no way to observe the step's return separately from the deadline rejection, log
   `blocker` (see Not owned).
6. `SqlPayload.path` set on every `sql` event (`"blocking"`/`"nonblocking"`). The DuckDB projection column is
   FFI-06's job; do not touch `platform/duckdb.ts`.
7. Error provenance lessons (gotchas): classify by identity, not class; `Effect.die(x)` inspects/swaps its
   argument inside spans, use `Effect.failCause(Cause.die(x))`; never run caller hooks before reading the
   native error; a throwing cleanup must not mask the original error. Instrument the surrounding Effect, not
   Statement instances.

## Acceptance evidence (plan, adjusted to the host)

- `tests/unit/ffi_classify_test.ts`: every `Operation` shape classified; the property above.
- Document reader test: a seeded 200 000-row table, a `count` with `statementDeadlineMs = 50` →
  `StatementDeadlineError` within 50 ms + 20 ms wall clock (PoC interrupt 0.4 ms, outliers to 9 ms; if the
  shared VM makes 20 ms flaky, use a ratio with the load average recorded, as risk 11 prescribes, and say so);
  the `sql` event shows `path: "nonblocking"`, `outcome: "error"`, `errorTag: "StatementDeadlineError"`; the
  `tx` event shows rollback. A concurrent point read on another file completes in < 5 ms during the long
  statement. Remember FFI-02's cross-field rule: a test lowering `reader.callTimeoutMs` must also lower
  `sqlite.statementDeadlineMs` if it goes through `Config` parsing (`ConfigTest` skips the check).
- Writer test: a mutation whose handler runs an unbounded `find` past the deadline rolls back (no row
  committed), fails 504, and the next mutation on the same connection succeeds.
- Point reads stay blocking: `sql` events for get-by-id show `path: "blocking"`; p50 unchanged from FFI-04's
  run (`deno task perf:smoke` via `work-scripts/bash/k6-baseline.sh`, 3 runs each side interleaved with load
  recorded, as FFI-04 did; if k6 is unavailable say so).
- `deno task lint`, `check`, `test`, `test:prop` green; `deno fmt --check` on every owned file
  (`deno task
  fmt:check` is red on `main` from the unowned `docs/knowledge/decisions.md`);
  `deno task integration` green; e2e via `npm run e2e` (Docker + Postgres) if available, else say so;
  `grep -c StatementDeadlineError
  tests/errors_exhaustiveness.ts` ≥ 1. Known timing flakes under load, not
  blockers: `tests/unit/ipc_test.ts:663`, `number_dialects_test.ts:243`, `sqlite_ffi_cancel_test.ts:76`. Never
  run suites concurrently with another suite (`tests/unit/tenants_test.ts` shares the dev Postgres).

## Protocol for this unit

- Fresh agent, own worktree, actor `claude-opus-ffi-05`. Log `task_started` first and `task_completed` last
  with
  `work-scripts/bash/log-event.sh --actor claude-opus-ffi-05 --branch item/ffi-05-deadline --type … --summary
  "FFI-05 …" --detail "…"`;
  log `blocker` immediately if the binding API blocks a step.
- **One implementation commit**: `platform: run unbounded statements nonblocking under a statement deadline`.
  Body: the classifier rule, what runs where, the 504 mapping, perf numbers with load, grace-period behaviour.
  Separate `agent-work: add FFI-05 brief` commit (`git add -f`). Knowledge/scripts in further separate commits
  if any.
- Do not merge, do not push to `main`, do not edit any other worktree. Push the branch to origin when done.
- Close with: commit SHA(s), validation output per task, perf comparison, learning assessment (what went to
  `docs/knowledge/`, what is handed to the coordinator because it lies outside Owns), and what the reviewer
  should look at first.

## Repair 1 after Astra review

Astra review (FAIL, `review/ffi-05-astra`, `agent-work/items/ffi-05-deadline/review-astra.md`) on `6246f9d`;
repair by `claude-opus-ffi-05-repair1` in `/root/Software/mansio-work/ffi05-rep1`, the one repair round before
arbitration. Repair commit `076b327`.

- Defect 1 (P2), fixed: the reader's query context is rechecked where each select starts, after any gate wait
  (`admit`: the `gate.blocking` start callback and the `gate.nonblocking` guard), and `COMMIT`'s gate callback
  expires the context in the same synchronous step, as the writer does. A refusal for an expired context emits
  no `sql` event and is not counted as an operation. Regression in `tests/unit/document_reader_test.ts`
  (blocking and nonblocking late read × immediate and yielding sink); all four variants fail against
  `6246f9d`.
- Defect 2 (P3), fixed: the gate fixtures in `tests/unit/storage_test.ts` do their native call inside `start`;
  a new competing-claim regression shows the deferred shape failing `"busy"`.
- Reviewer's reproduction, scratch copy, both modes: before, `delay-sink` late `Success` after `COMMIT` and
  the `tx` event, immediate late `ReaderError` "prepare on a closed connection"; after, both
  `ContextExpiredError` with no native call or `sql` event after `COMMIT`.
- Validation: lint, check, test (686 passed, 0 failed, 1 ignored), test:prop (79 passed), integration PASS,
  `deno fmt --check` on touched files. Knowledge: `docs/knowledge/gotchas.md`, "FFI-05 repair".
