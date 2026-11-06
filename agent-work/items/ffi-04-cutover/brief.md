# FFI-04 — Cutover of the statement path to the FFI binding (brief)

Status: dispatched · Branch: `item/ffi-04-cutover` · Worktree: `/root/Software/mansio-work/ffi04` · Base: `main`
at `182e85d` (FFI-03 merge) · Builder: Claude Opus 5.5 (actor `claude-opus-ffi-04`) · Reviewer: Astra
(`gpt-6-astra`), fresh unit · Date: 2026-10-13 · Authorization: Nick via `/root/fable-ffi03-go.md` (land
FFI-03, then dispatch FFI-04 per `docs/plans/ffi-implementation.md`).

The plan lives on branch `plan/ffi-implementation` (not yet on `main`); read it with
`git show origin/plan/ffi-implementation:docs/plans/ffi-implementation.md`. Read §1.2, §1.3, §2, §3 and the
FFI-04 item in §4 in full before writing code. Also read `platform/sqlite_ffi/README.md` (FFI-03's contract),
`docs/knowledge/index.md`, `docs/knowledge/gotchas.md` (the FFI-01 lessons), `AGENTS.md`, and the live event
log per `agent-work/event-log.md`.

## Why

FFI-01 (binding, `platform/sqlite_ffi/{mod,library,database,errors}.ts`) and FFI-03 (Effect layer,
`platform/sqlite_ffi/effect.ts`) are on `main` and nothing imports them. This item moves every live-file
SQLite open in the API process from `node:sqlite` to the binding so that FFI-05 can add deadline-bounded
execution on top. Semantics do not change in this item.

## The one rule (plan §1.2)

**Never open the same database file through `node:sqlite` and the FFI binding in one process.** The two SQLite
copies keep separate POSIX advisory-lock tables; a close through one silently drops the other's locks. So the
writer, the document reader, the router's `prepare`, the backup verifier, and every test that opens a live
file switch together in this one commit. No flag, no dual-driver period. Rollback is `git revert`.

## Owns

Plan set: `platform/sqlite.ts`, `platform/document_reader.ts`, `platform/tenants/routing.ts`,
`platform/backup.ts`, `platform/layers.ts`, `platform/server.ts` (wire `Config.sqlite.library` into the open
calls and emit the one-time lifecycle event), `tests/unit/boundaries_test.ts` (new rules),
`tests/helpers/sqlite_inspect.ts` (new), `tests/core_test.ts`, `tests/unit/storage_test.ts`,
`tests/unit/backup_test.ts`, `tests/unit/document_reader_test.ts`, `tests/unit/http_test.ts`,
`tests/unit/server_process_test.ts`, `tests/unit/server_scope_test.ts`, `tests/unit/reader_context_test.ts`,
`tests/unit/nul_contract_test.ts`, `tests/unit/number_dialects_test.ts`,
`tests/property/tenant_routing_props.ts`, `tests/property/storage_commit_test.ts`,
`tests/layers/storage_test_layer.ts` (comment-only).

Coordinator additions (these also import `node:sqlite` today and were missed by the plan):
`tests/unit/reader_test.ts` (fixture writer, dynamic import), `tests/unit/reader_ingest_test.ts` (same),
`tests/unit/tenant_routing_test.ts` (type import only). Plus `agent-work/items/ffi-04-cutover/*` for this
brief and your claims/notes (`agent-work/` is gitignored: `git add -f`).

Not owned, do not edit: `deno.json` (frozen), `platform/sqlite_ffi/*` (if the FFI-03 API is missing something
you need, stop and log `blocker` with the exact gap; do not patch the binding in this item), `core/*`,
`sdk/*`, `app/*`, `tests/property/sqlite_oracle_test.ts`, `tests/property/json_number_test.ts`,
`tests/unit/sqlite_ffi_*_test.ts`, `docs/plans/*`, any other worktree under `/root/Software/mansio-work/`.

## Deviations accepted by the coordinator

Recorded at repair 1 (actor `claude-opus-ffi-04-repair1`, worktree `/root/Software/mansio-work/ffi04-rep1`)
after the Astra review FAIL on `73f882f`:

- **Owns extension: `tests/property/sqlite_oracle_test.ts`.** Review defect 1: regression 2 held `Storage`
  (FFI binding) on a live file and opened the same file through `node:sqlite` to write beneath it, the §1.2
  two-copies hazard. The coordinator added this file to the repair's Owns so the bypass write could move to
  the binding (`tests/helpers/sqlite_inspect.ts`, read-write). `node:sqlite` stays in the file only for
  `:memory:` databases. `tests/property/json_number_test.ts` remains not owned and untouched.
- **Executable edit in `tests/layers/storage_test_layer.ts`.** Review defect 2: the file was listed as
  comment-only, but the build changed the U+0000 probe's `StorageError.detail` string (line ~471) so the test
  double matches the live layer. Accepted as correct; not reverted.
- **Comment-only edits in `platform/services.ts`, `platform/registry.ts`, `platform/reader_layers.ts`.**
  Listed under Reads above ("leave them unless they become false"); the cutover made their `node:sqlite`
  comments false, so the build reworded them. Accepted.

## Reads

`platform/sqlite_ffi/*`, `platform/config.ts` (FFI-02:
`Config.sqlite = { library?, statementDeadlineMs,
statementInterruptGraceMs }`), `core/compile/*`,
`core/errors.ts` (no new errors in this item), `platform/services.ts`, `platform/registry.ts`,
`platform/reader_layers.ts` (comments mention `node:sqlite`; leave them unless they become false).

## Work (plan FFI-04, with FFI-03's real API names)

Check the exported names in `platform/sqlite_ffi/mod.ts` and `effect.ts` first; the plan was written before
FFI-03 landed and uses approximate names (`openConnection` is `openDatabase`, etc.).

1. `Storage` (`platform/sqlite.ts`): `new DatabaseSync(file)` → the binding's open (read-write);
   `database.prepare(sql.text)` → `prepareCached(db, sql.text)` **used in the same synchronous step** (a
   cached statement is only safe to use immediately; an evicted one fails typed `"finalized"`); binds are an
   **array** (`run(stmt, sql.values)`), never variadic; `isTransaction` → `inTransaction`; the three pragmas
   as three `exec` calls; `VACUUM INTO ?` unchanged in SQL, through `run`. **Semantics unchanged**: same
   `Semaphore(1)`, same `WRITER_QUEUE_CAP`, same `acquireUseRelease` transaction, same `nulProbe` contract,
   same `sql`/`tx` events, same error mapping into `PlatformError`. Every statement still takes the **blocking
   (sync)** path in this item; no `runAsync`/`allAsync`, no `timeoutMs`.
2. Document reader (`platform/document_reader.ts`): per-call
   `openDatabase(file, { readOnly: true, readBigInts:
   true })` (the `setReadBigInts(true)` contract,
   unchanged); `statement.all` through the binding; throws mapped to `ReaderError` as today. The
   `Effect.timeoutFail` at `callTimeoutMs` stays (FFI-05 replaces it).
3. Router (`platform/tenants/routing.ts`): `StorageLocation.open` returns the FFI connection synchronously,
   inside the same `check` step (the containment and symlink re-check keeps its position; update the type of
   `open`'s return). `prepare(location)` runs its DDL transaction on a short-lived connection and closes it.
   The header comment's `node:sqlite cannot open a prevalidated fd` sentence must be reworded or removed.
4. Backup (`platform/backup.ts`): `verifySnapshot` and `integrityCheck` open the snapshot copy read-only
   through the binding.
5. Lifecycle: one `lifecycle` event at startup with the resolved library path and `sqlite3_libversion()`
   (through `EventSink`, same shape as existing lifecycle events). Library path comes from `Config.sqlite`.
6. Boundary test (`tests/unit/boundaries_test.ts`): `platform/**` never mentions `node:sqlite` (replacing the
   two narrower rules; `platform/sqlite_ffi/*` comments/README that explain the contrast with `node:sqlite`
   may be exempted by exact path, or reworded — your call, say which in the commit body); `tests/**` never
   mentions `node:sqlite` except an explicit allow-list: `tests/property/sqlite_oracle_test.ts`,
   `tests/property/json_number_test.ts` (in-memory oracles), and the binding's own tests
   `tests/unit/sqlite_ffi_perf_test.ts`, `tests/unit/sqlite_ffi_effect_test.ts` (coordinator ruling: they use
   `node:sqlite` as a parity/behaviour oracle on their own temp files and never on a file the server has
   open). The rule must list the allowed files by exact path so a new exemption is a visible diff.
7. Tests: add `tests/helpers/sqlite_inspect.ts` (`inspect(path)` opens read-only through the binding and
   exposes the small surface the tests use: `get`, `all`, `exec`, `close`; prefer one shape). Replace every
   `new DatabaseSync(liveFile)` and every `await import("node:sqlite")` in the owned tests with it. Where a
   test writes a fixture file (reader_test, reader_ingest_test) use the binding read-write for that too. Where
   a test only asserts file contents after the server closed, keep the assertion and switch the opener. Tests
   that lower `readerCallTimeoutMs` must also lower `sqlite.statementDeadlineMs` to keep the FFI-02
   cross-field rule (`statementDeadlineMs < readerCallTimeoutMs`) if they go through `Config` parsing
   (`ConfigTest` skips cross-field checks; check which path each test takes).

## Acceptance evidence (plan, adjusted to the host)

- `deno task lint`, `deno task check`, `deno task test`, `deno task test:prop` green; `deno fmt --check` on
  every owned file. (`deno task fmt:check` is red on `main` from one unowned file,
  `docs/knowledge/decisions.md`; `deno task ci` therefore cannot be green — run its parts.)
- `deno task integration` green. End-to-end: `npm run e2e` (= `deno run -A tests/e2e/platform/main.ts`, needs
  Docker + Postgres); run it if `docker` works on this host, otherwise state plainly that it was not run.
- `grep -rn "node:sqlite" platform/` lists only exempted `sqlite_ffi` prose (or nothing);
  `tests/unit/boundaries_test.ts` fails if a non-exempt file mentions it.
- `work-scripts/bash/segfault-stress.sh` (30 runs of `deno task test`): no new crash signature beyond the
  known DuckDB teardown flake (gotchas "Exit-139"). Record the run count and any failure names in the commit
  body. Known timing flakes under load: `tests/unit/ipc_test.ts:663`, `tests/unit/number_dialects_test.ts:243`
  (both pass alone; neither is in your Owns).
- `deno task perf:smoke` (k6, see `work-scripts/bash/k6-baseline.sh` for how the server is started) p50/p99
  within 10 % of a `main` run on the same host and load. Record both runs and the load averages in the commit
  body. Point-read latency is expected to drop; a rise fails the gate. If k6 or the server harness is
  unavailable, say so and give the binding-level parity numbers from `sqlite_ffi_perf_test.ts` instead.
- Snapshot round trip: `backup_test.ts` takes a snapshot, verifies it (`quick_check` through the binding),
  restores it; unchanged assertions pass.
- Reviewer will confirm no code path opens a file through both drivers (`grep -rn DatabaseSync`); make that
  grep easy to read.

## Protocol for this unit

- Fresh agent, own worktree, actor `claude-opus-ffi-04`. Log `task_started` first and `task_completed` last
  with
  `work-scripts/bash/log-event.sh --actor claude-opus-ffi-04 --branch item/ffi-04-cutover --type … --summary
  "FFI-04 …" --detail "…"`;
  the completion names validation and the build SHA. Log `blocker` immediately if the FFI-03 API blocks a
  step.
- **One implementation commit**: `platform: move the statement path from node:sqlite to the FFI binding`.
  Body: what moved, the exemption list, perf numbers, stress-loop result. Commit this brief plus any claims as
  a separate `agent-work: add FFI-04 brief` commit (`git add -f`). Knowledge (`docs/knowledge/`) and reusable
  scripts (`work-scripts/`) go in further separate commits, if any.
- Do not merge, do not push to `main`, do not edit any other worktree. Push the branch to origin when done.
- Close with: commit SHA(s), validation output (pass/fail counts per task), the perf comparison, the learning
  assessment (what went to `docs/knowledge/`, what was handed to the coordinator because it lies outside
  Owns), and anything the reviewer should look at first.
- Lessons from FFI-01/03 reviews (gotchas.md): error provenance must be by identity, not class;
  `Effect.die(x)` swaps its argument inside spans (use `Effect.failCause(Cause.die(x))` if you ever need it);
  never run caller hooks before reading the native error; a throwing cleanup must not mask the original error.
