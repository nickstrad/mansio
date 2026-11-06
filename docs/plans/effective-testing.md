# Plan: apply _Effective Software Testing_ (Aniche) to mansio

Status: proposal, not started · Author: Claude Opus 5.5 (with nickstrad) · Date: 2026-11-04 · Baseline: `main`
at `fac3e3e`.

Source material, outside the repo: `~/AI/effective-software-testing/` holds the research write-up
(`writeup.md`, a copy of `~/AI/writeups/aniche_effective_software_testing.md`) and a clone of the book's code
(`book-code/`, `github.com/effective-software-testing/code`). Chapter references below (ch2, ch3, ch5, ch9)
are to the book; "write-up §n" is the research note.

## 0. Summary

mansio already tests a lot: 60 test files (~29 k lines), fast-check properties with fixed seeds and replay, a
Storage conformance suite that checks the in-memory double against real SQLite, test layers for every service,
and a seven-scenario e2e suite with ephemeral Postgres. What it lacks, measured against the book:

| Book practice                                    | mansio today                                                                                                          | Gap                                                                                                                     |
| ------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| Specification-based (ch2): partitions, on/off    | Many example tests; no written partition tables; some limits have on/off tests (`WRITER_QUEUE_CAP`), most don't       | No spec catalogue; untested boundaries (§1.1)                                                                           |
| Structural (ch3): coverage to find missing tests | No coverage run anywhere; no mutation testing                                                                         | No coverage report, no structural pass, no mutation score                                                               |
| Property-based (ch5) + fuzzing                   | 21 fast-check files on core, framing, FFI, storage commit                                                             | No stateful (command-sequence) properties; no fuzz campaigns; no saved regression corpus                                |
| Integration (ch9): one adapter, real infra       | Storage conformance, S3 object store (optional), FFI against SQLite                                                   | Postgres adapters (tenant store, key cache, session auth) have no integration tests outside e2e                         |
| System (ch9): riskiest user flows, few, stable   | `deno task e2e` (7 scenarios), `deno task integration`                                                                | Riskiest flows missing: backup in tenancy mode, crash/restart, overload (503), deadline (504), revocation               |
| Contracts (ch4): pre/post/invariants             | Typed errors at boundaries (good); defects → 500 with trace id (good); 11 ad hoc `Effect.die` sites; FFI usage checks | No contract helper, no stated pre/post per module, no `invariant()` on stateful components, no contract-violation event |

The plan adds a spec catalogue, boundary tests, a coverage-driven structural pass, mutation testing on
`core/`, **first-class contracts (preconditions, postconditions, invariants) in `core/` and in the stateful
`platform/` components**, stateful properties and fuzz campaigns that use those contracts as oracles, Postgres
adapter integration tests, and risk-ranked system scenarios. Contracts are a deliberate product change (they
halt on a bug); everything else changes no product behavior, except where a test exposes a bug, which becomes
its own fix item.

## 1. Findings that shape the plan

### 1.1 Boundaries without on/off-point tests

From a grep of `tests/` against the constants in `core/`, `platform/` (2026-11-04). "On" = exactly at the
limit, "off" = one past it (write-up §1).

| Limit                                            | Value       | Where                             | Tests today                                   |
| ------------------------------------------------ | ----------- | --------------------------------- | --------------------------------------------- |
| Writer queue `WRITER_QUEUE_CAP`                  | 1000        | `platform/sqlite.ts:147`          | On and off (`tests/unit/storage_test.ts:193`) |
| Request body `maxRequestBytes`                   | 65 536      | `platform/config.ts`              | Off only ("over the 64 KiB cap → 413")        |
| Reader admission `DEFAULT_READER_CAP`            | 1000        | `platform/document_reader.ts:422` | None                                          |
| Read concurrency `DOCUMENT_READ_CONCURRENCY_CAP` | 32          | `platform/document_reader.ts:77`  | On only                                       |
| Tenant handle cache capacity / idle              | 64 / 60 s   | `platform/sqlite.ts:1021-1022`    | Not found                                     |
| API-key cache capacity / TTL                     | 1024 / 30 s | `platform/http/auth.ts:97-139`    | TTL config only                               |
| Event-log `MAX_LINE_BYTES`                       | 1 MiB       | `platform/eventlog/ingest.ts`     | None                                          |
| Log queue `EVENT_QUEUE_CAPACITY`                 | 10 000      | `platform/logging/logger.ts:326`  | None                                          |
| Log file `LOG_MAX_FILE_BYTES`                    | 64 MiB      | `platform/logging/file_sink.ts`   | None                                          |
| IPC outbox                                       | 1000        | `platform/ipc.ts:226`, `:477`     | Not checked                                   |

### 1.2 Risky flows with no system test

From the contention survey in
[`docs/architecture/scale-out-direct-actors.md`](../architecture/scale-out-direct-actors.md) §5.1:

- **Backup in tenancy mode.** The snapshot path appears to run without a `TenantContext`
  (`platform/sqlite.ts:1078-1080`), so tenant databases may not be backed up. Inferred from code, not run.
  This is the first test TS-09 writes, and it is worth checking before the plan starts.
- A cold tenant open holds the global handle-cache lock; request 65 waits with no timeout.
- Mutations have no request deadline (`platform/http/handlers.ts:175`).
- A long ingest blocks `/health` on the single reader child.

### 1.3 Tooling available on Deno

| Need                     | Tool                                                                                               | Notes                                                                                                                             |
| ------------------------ | -------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| Line and branch coverage | `deno test --coverage=<dir>` then `deno coverage --lcov` / `--html`                                | Built in; lcov includes branch records. Equivalent of the book's JaCoCo (`book-code/ch3/coverage.sh`).                            |
| Mutation testing         | StrykerJS with its generic `command` test runner running `deno test`                               | Equivalent of the book's PIT. No native Deno runner, so every mutant reruns the chosen test files: scope to `core/`. Spike first. |
| Properties               | fast-check 4.10.2 (already pinned)                                                                 | jqwik equivalent; `fc.commands` + `fc.modelRun` / `fc.asyncModelRun` cover the book's stateful `BasketPBTest` (ch5).              |
| Fuzzing                  | fast-check in time-boxed campaigns (`interruptAfterTimeLimit`, many seeds); byte-level arbitraries | Deno has no coverage-guided fuzzer. Jazzer.js (Node-only) could fuzz `core/`, which is Deno-free by rule: spike only.             |
| Integration DB           | The e2e harness's ephemeral Docker Postgres (`tests/e2e/platform/postgres.ts`)                     | Reuse per test file with a reset per test (book `ch9/sql/SqlIntegrationTestBase.java`: clean state before each test).             |

### 1.4 Contracts today (ch4)

The repo already follows ch4's split between **validation** and **contracts** (write-up §5, 4.5.2–4.5.3)
without naming it:

- **Validation at trust boundaries** returns typed errors through the Effect error channel: schema validation,
  HTTP auth and body limits, config decoding. `tests/unit/boundaries_test.ts:322` forbids `throw new Error` in
  `core/` and `platform/`. These are tested, as ch4 recommends.
- **Bugs are defects.** `Effect.die` appears at 11 sites (`platform/sqlite.ts`, `document_reader.ts`,
  `duckdb.ts`, `http/auth.ts`, `sqlite_ffi/effect.ts`), for example invalid cache limits at
  `platform/http/auth.ts:104`. The router turns any defect into a 500 that names the trace id and logs the
  full cause (`platform/http/router.ts:28`, `:219`). That is ch4's "elegant error page, not a stack trace".
- **The FFI layer is library-style.** `assertIdle` and `assertNoNul` (`platform/sqlite_ffi/database.ts:356`,
  `:364`) raise typed usage errors, which matches ch4's "libraries favor exceptions".

What is missing: a shared way to state a precondition, postcondition or invariant; any `invariant()` on the
stateful components where the contention survey found shared state (handle cache, key cache, writer queue,
reader permits, IPC outbox); postconditions on the compiler and decoder; and a way to tell a contract
violation apart from other defects in logs. The suspected tenancy backup bug (§1.2) is the kind of fault a
precondition ("a snapshot in tenancy mode has a `TenantContext`") turns from silent into loud.

### 1.5 The `deno.json` freeze

The main plan (§2.2) froze `deno.json` after item 01. New entry points (coverage, mutation, fuzz, Postgres
integration) need either new tasks or scripts. **Default in this plan: scripts under `work-scripts/bash/`, no
`deno.json` edit.** New test files go under existing globs (`tests/unit/**`, `tests/property/**`) so
`deno
task test` and `test:prop` pick them up. If Nick prefers tasks, TS-03 also owns a single `deno.json`
edit adding `coverage`, `test:fuzz`, `test:mutation` and `test:pg` (decision D1).

## 2. Test taxonomy (the book's, mapped to mansio)

| Book level    | mansio meaning                                                                                                      | Where                                                   | Gate                                                 |
| ------------- | ------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------- | ---------------------------------------------------- |
| Unit          | `core/` pure logic, and services over test layers                                                                   | `tests/unit/`, `tests/core_test.ts`                     | `deno task ci`                                       |
| Specification | Not a level: a way to choose tests at every level, from a written partition/boundary table                          | `docs/testing/spec/*.md` → tests tagged with spec ids   | `ci` (traceability check)                            |
| Structural    | Coverage and mutation reports used to _find_ missing tests; no percentage target                                    | `work-scripts/bash/coverage.sh`, `mutation.sh`          | Report only                                          |
| Property      | Generated inputs with invariants, plus stateful command-sequence models                                             | `tests/property/`                                       | `ci` (200 runs, fixed seed)                          |
| Fuzz          | Long property campaigns over malformed input; failures saved as regressions                                         | `tests/property/fuzz/` + `work-scripts/bash/fuzz.sh`    | Corpus in `ci`; campaigns manual/nightly             |
| Integration   | One adapter against its real infrastructure: SQLite FFI, Postgres, DuckDB, object store, IPC child                  | `tests/unit/*` (existing), `tests/pg/` (new)            | `ci` where infra is local; Postgres suite via script |
| System        | The whole server through HTTP and the SDK, as a user                                                                | `tests/e2e/platform/scenarios/`, `tests/integration.ts` | `deno task e2e` before landing risky items           |
| Contracts     | Not a level: pre/post/invariant checks in product code that halt on a bug, and act as oracles for every level above | `core/contract.ts`; call sites in `core/`, `platform/`  | Always on; every test run is also a contract run     |

**Validation or contract?** (ch4 4.5.2–4.5.3, applied.) Input that crosses a trust boundary (HTTP, SDK,
config, files, IPC frames, Postgres rows) is **validated**: typed error, tested by spec tests. Data inside the
safe zone, already validated, is guarded by **contracts**: a violation is a bug, so it is a defect with a
`ContractViolation` cause, not a typed error, and it is rarely tested directly. A check is repeated on both
sides only if the contract is "very important and should never be broken" (ch4): in mansio that means tenant
isolation (a resolved file path stays inside `TENANTS_DIR`) and data loss (a backup's manifest checksum
matches the bytes stored).

The book's ordering is the work order for each module: spec tests first, then structural additions, then
properties, then a few larger tests on top (write-up §"chapter order").

## 3. Batch table

| Batch | Items                      | Scope                                                                           | Gate                                                                                                                                 |
| ----- | -------------------------- | ------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| TS-A  | TS-01, TS-03, TS-11, SQ-M  | Spec catalogue; coverage tooling; contract helper and policy; mutation spike    | concurrent (disjoint files)                                                                                                          |
| TS-B  | TS-02, TS-04, TS-12, TS-13 | Boundary tests; structural pass; contracts in `core/`; contracts in `platform/` | after TS-A; TS-12 ∥ TS-13 (disjoint files); TS-04 reads but does not edit the files TS-13 owns, and reruns its log after TS-13 lands |
| TS-C  | TS-05, TS-06, TS-07        | Mutation adoption; stateful properties; fuzz campaigns (contracts as oracles)   | after TS-12 and TS-13; TS-05 after SQ-M verdict; TS-06 ∥ TS-07                                                                       |
| TS-D  | TS-08                      | Postgres adapter integration tests                                              | after TS-A                                                                                                                           |
| TS-E  | TS-09                      | Risk-ranked system scenarios                                                    | **SYNC**, alone; after TS-08 (reuses its Postgres reset)                                                                             |
| TS-F  | TS-10                      | Docs, traceability check, gate update                                           | last                                                                                                                                 |

Model policy as in `AGENTS.md`: Opus for items that decide oracles around concurrency, resource lifetimes and
security; Sonnet for mechanical items with checkable acceptance; Haiku only for the side quest. Every item
gets the Opus review. Any product bug a test exposes is reported, not fixed in the test item: the coordinator
opens a fix item (`TS-FIX-nn`) with its own owner.

---

#### TS-01 — Specification catalogue (ch2)

- **Model:** **Opus / astra.** Choosing partitions decides what the suite can ever catch.
- **Owns:** `docs/testing/spec/README.md`, `docs/testing/spec/http.md`, `docs/testing/spec/sdk-documents.md`,
  `docs/testing/spec/schema.md`, `docs/testing/spec/limits.md`, `docs/testing/spec/tenancy-auth.md`.
- **Reads:** `docs.md`, `platform/http/*.ts`, `sdk/*.ts`, `core/schema.ts`, `core/compile/*.ts`,
  `platform/config.ts`, §1.1 above, `~/AI/effective-software-testing/book-code/ch2/`.
- **Work:** For each public surface, follow the book's seven steps (write-up §1) and record:
  1. Inputs and outputs, from `docs.md` and the code's types.
  2. Partitions per input, per input combination, and per output (the three passes), each with a stable id
     (`HTTP-AUTH-P3`, `LIM-BODY-B1`).
  3. Boundaries with on, off, in and out points; equality conditions get three points.
  4. The existing test(s) covering each partition (file:line), or "none".
  5. Cases added from experience (step 7): NUL bytes, lone surrogates, `-0`, huge integers, empty collections,
     duplicate ids, unknown fields, reserved names.
- **Acceptance criteria:**
  - Every limit in §1.1 has a row with on/off values and current coverage.
  - Every HTTP route in `platform/http/router.ts` has a partition table covering auth mode × outcome status.
  - Each partition id is unique across the catalogue (checked by TS-10).

#### TS-02 — Boundary and partition tests (ch2)

- **Model:** **Sonnet / sol.** Interfaces and values are fixed by TS-01.
- **Owns:** new files `tests/unit/spec/*_test.ts` only.
- **Reads:** `docs/testing/spec/*.md`, `tests/layers/`, existing tests named in the catalogue.
- **Work:** For every catalogue row marked "none" or "on only"/"off only", add the missing points. Each test
  name starts with its spec id (`LIM-READER-B1 on: request 1000 is admitted`). Use test layers and `TestClock`
  for TTLs and idle expiry; no sleeps. Priorities: request body 65 536 / 65 537; reader admission 1000 / 1001;
  read concurrency 32 / 33; handle cache 64 / 65 (record the unbounded wait as a finding); key cache 1024 /
  1025 and TTL at `exp − 1 ms` / `exp`; `MAX_LINE_BYTES`; log queue drop at 10 000 / 10 001; log rotation at
  64 MiB.
- **Acceptance criteria:**
  - `deno task ci` green. Every catalogue row has at least one test id, or a written reason it is untestable
    at this level.
  - Any boundary that fails is reported as a `TS-FIX` candidate with the failing test marked `ignore` and a
    link, never deleted or weakened.

#### TS-03 — Coverage tooling and baseline (ch3)

- **Model:** **Sonnet / sol.**
- **Owns:** `work-scripts/bash/coverage.sh`, `work-scripts/INDEX.md` (one row), `docs/testing/coverage.md`,
  `.gitignore` (add `coverage/`). If D1 = tasks: `deno.json` (one edit, all four tasks).
- **Reads:** `book-code/ch3/coverage.sh`, `deno.json`.
- **Work:** Script runs `deno task test` and `test:prop` with `--coverage`, writes lcov and HTML, and prints a
  per-file table (lines, branches) sorted by uncovered branches, with an option to restrict to files changed
  against `main`. Record the baseline per directory in `docs/testing/coverage.md`. State the rule from ch3:
  the number is not a target, and there is no threshold gate.
- **Acceptance criteria:** the script runs from a clean checkout; the baseline table lists `core/`,
  `platform/`, `sdk/`; HTML opens; lcov contains branch records.

#### TS-04 — Structural pass on high-risk modules (ch3)

- **Model:** **Opus / astra.** Touches locking, scopes and auth paths.
- **Owns:** new files `tests/unit/structural/*_test.ts`; new rows in `docs/testing/coverage.md` (structural
  log section only).
- **Reads:** coverage output from TS-03; `platform/sqlite.ts`, `platform/http/auth.ts`,
  `platform/auth/service.ts`, `platform/document_reader.ts`, `platform/backup.ts`, `platform/ipc.ts`,
  `core/compile/*.ts`.
- **Work:** For each module, list every uncovered branch and decide: test it, or record why not (unreachable,
  covered by a system test, defensive). Loops get zero, one and many iterations. Compound conditions in
  `core/compile` get MC/DC-style cases (each condition flips the outcome alone). The log entry per module is
  "uncovered → decision → test id".
- **Acceptance criteria:** every uncovered branch in the listed files has a decision row; the after-baseline
  is recorded; new tests pass in `deno task ci`.

#### TS-11 — Contract helper and policy (ch4)

- **Model:** **Opus / astra.** Public helper API and the error taxonomy: expensive to change later.
- **Owns:** `core/contract.ts` (new), `core/errors.ts` (additive: `ContractViolation` defect type),
  `platform/http/router.ts` and `platform/http/handlers.ts` (additive only: tag a `ContractViolation` defect
  in the log event as `errorTag: "ContractViolation"`; response stays the existing 500 with trace id),
  `tests/unit/contract_test.ts`, `tests/unit/boundaries_test.ts` (one additive rule),
  `docs/testing/contracts.md`.
- **Reads:** write-up §5 (ch4), `book-code/ch4/`, `core/errors.ts`, `platform/http/router.ts:28-260`,
  `platform/config.ts`.
- **Work:**
  1. `core/contract.ts`, pure (no `Deno.*`, per the core rule), two forms:
     - sync, for pure code: `require(cond, what, values?)`, `ensure(cond, what, values?)`,
       `invariant(cond, what, values?)`, each throwing a `ContractViolation` (kind, module, message, a small
       values record, never document contents or secrets);
     - Effect, for services: `Contract.require/ensure/invariant` returning `Effect.die(ContractViolation)`.
     - A `checkInvariant(name, fn)` pattern for stateful components: one function per component, called at the
       end of every public operation (ch4 4.2), allowed to be false in the middle of an operation.
     - Postconditions that need old state capture it in a local before the change (ch4 4.2).
  2. **Levels.** `cheap` (O(1) checks, always on, including production) and `full` (O(n) invariants such as
     scanning a cache). The level is passed in from `platform/config.ts` (`CONTRACTS=cheap|full`, default
     `cheap`; tests run `full`). Contracts are never switched off entirely (ch4 4.1.1: you would lose the
     check where it matters). Adding the option is an additive config row owned here.
  3. **Policy doc** `docs/testing/contracts.md`: the validation-or-contract rule from §2, the two repeated
     contracts, how to write each kind, the levels, the log event, and ch4 4.5.6: contracts are not tested one
     by one, but each `checkInvariant` gets one fault-injection test proving it can fire (the write-up's
     jumphash lesson: an assertion never seen to fail proves nothing).
  4. **Boundary rule.** `boundaries_test.ts` gains a rule: `ContractViolation` is raised only through
     `core/contract.ts` (no hand-built ones), and `core/contract.ts` itself must not import from `platform/`.
  5. **Convert** the existing ad hoc `Effect.die` sites that are bug checks to the helper, so every one of
     them is tagged; leave typed-error paths alone.
- **Acceptance criteria:**
  - Unit tests: each helper fires with the right kind and message, and passes silently when true; a
    `ContractViolation` raised inside a handler yields the existing 500 body with trace id and a log event
    tagged `ContractViolation`, with no stack in the response.
  - `deno task ci` green; existing 500 tests unchanged.

#### TS-12 — Contracts in `core/` (ch4)

- **Model:** **Sonnet / sol** with **Opus** review. Mechanical once TS-11 fixes the helper; each contract is
  stated in the work list.
- **Owns:** `core/compile/*.ts`, `core/schema.ts`, `core/eventlog/offsets.ts`, `core/logging/rotation.ts`
  (contract lines and doc comments only, no behavior change), `docs/testing/contracts.md` (catalogue section,
  `core/` rows only).
- **Reads:** TS-11's helper and policy, TS-01 catalogue, `tests/property/*`.
- **Work:** Add contracts where data is already validated (the safe zone), and document each in the function's
  doc comment as `@pre`, `@post`, `@invariant` (ch4: write the contract in the docs too). Starting list:
  - **Compiler** (`core/compile/`): pre: the plan has been validated against its schema; post: the placeholder
    count equals the bind count; post: every identifier in the SQL is quoted and comes from the schema; post
    (`write.ts`): an insert or replace writes exactly the schema's columns.
  - **Classifier**: post: `bounded ⇒ _id eq filter present and no aggregate` (today a property test only).
  - **Decode** (`decode.ts`): post: the decoded document validates against its schema.
  - **Schema** (`schema.ts`): post: normalization is idempotent (`normalize(normalize(x)) = normalize(x)`;
    `full` level only).
  - **Offsets** (`eventlog/offsets.ts`): invariant: committed offsets are monotonic per file and never past
    the file size.
  - **Rotation** (`logging/rotation.ts`): post: after a rotation decision, the active file is under
    `LOG_MAX_FILE_BYTES`; after a retention decision, the kept total is under the cap.
- **Acceptance criteria:** every contract is listed in the catalogue with kind, level and file:line;
  `deno task ci` and `test:prop` green with `CONTRACTS=full` (the property suites now exercise them); no
  `core/` behavior change: existing tests pass unmodified.

#### TS-13 — Invariants in `platform/` stateful components (ch4)

- **Model:** **Opus / astra.** Locks, scopes and lifetimes; a wrong invariant here halts the server.
- **Owns:** `platform/sqlite.ts`, `platform/document_reader.ts`, `platform/http/auth.ts`, `platform/ipc.ts`,
  `platform/backup.ts`, `platform/tenants/routing.ts` (contract lines, `checkInvariant` functions and doc
  comments only), new `tests/unit/contracts/*_test.ts`, `docs/testing/contracts.md` (catalogue section,
  `platform/` rows only).
- **Reads:** TS-11 helper and policy, `docs/architecture/scale-out-direct-actors.md` §5.1 (the contention
  survey: these are the components it named).
- **Work:** One `checkInvariant` per stateful component, called at the end of each public operation, plus
  pre/postconditions where listed:
  - **Tenant handle cache** (`sqlite.ts:1015-1145`): invariant: entries ≤ capacity; every entry's leases ≥ 0;
    `idleSince` is set if and only if leases = 0; evicted entries are not in the map.
  - **Writer queue** (`sqlite.ts:769-787`): invariant: running + waiting ≤ `WRITER_QUEUE_CAP`; at most one
    running writer per file.
  - **Document reader** (`document_reader.ts`): invariant: active ≤ `DOCUMENT_READ_CONCURRENCY_CAP`, pending ≤
    `DEFAULT_READER_CAP`.
  - **API-key cache** (`http/auth.ts`): invariant: size ≤ capacity; post on a hit: the entry's expiry is after
    now.
  - **IPC** (`ipc.ts`): invariant: outbox ≤ its bound; in-flight request ids are unique; post: a response
    matches a pending request id.
  - **Backup** (`backup.ts`): pre: in tenancy mode a snapshot has a `TenantContext` (§1.2); post: the
    manifest's SHA-256 equals the hash of the bytes uploaded (repeated contract: data loss).
  - **Tenant routing** (`tenants/routing.ts`): post: the resolved path is inside `TENANTS_DIR` after
    normalization (repeated contract: tenant isolation; validation already rejects bad ids).
  - O(n) checks (scanning cache entries) run at `full` only; O(1) counter checks at `cheap`.
  - One fault-injection test per `checkInvariant` (corrupt the state, call the check, expect a
    `ContractViolation`), and one wiring test per component showing a public operation calls it.
- **Acceptance criteria:**
  - `deno task ci`, `test:prop`, `deno task integration` and `deno task e2e` green with `CONTRACTS=full`.
  - k6 smoke (`deno task perf:smoke`) at `cheap` shows no p50 regression beyond noise, recorded in the report.
  - If the backup precondition fires in e2e, that confirms §1.2: report it as `TS-FIX-01`; do not weaken the
    contract.

#### SQ-M — Side quest: mutation testing feasibility (ch3)

- **Model:** **Haiku** (≥ 5.5), side-quest protocol; Opus review; Nick approves before anything lands.
- **Owns:** `docs/haiku-experiments/findings/sq-mutation.md`; scratch only.
- **Question:** can StrykerJS's `command` runner drive `deno test` over `core/compile/` and `core/schema.ts`
  with only their unit and property test files, and what are the run time, mutant count and score?
- **Done when:** the findings give a runnable config, wall time on the VM, score per file, and the 10 most
  instructive surviving mutants. If Stryker cannot run, record why and evaluate a minimal custom mutator
  (operator swaps on the TypeScript AST) as the fallback.

#### TS-05 — Mutation testing on `core/` (ch3)

- **Model:** **Sonnet / sol.** Config follows SQ-M.
- **Owns:** `work-scripts/bash/mutation.sh`, `stryker.config.json` (or the fallback script),
  `work-scripts/INDEX.md` (one row), new tests `tests/unit/mutation/*_test.ts`, `docs/testing/mutation.md`.
- **Work:** Adopt the SQ-M config. Run on `core/`; for each surviving mutant, add a test that kills it or mark
  it equivalent with a reason. Manual or nightly, never in `ci` (too slow).
- **Acceptance criteria:** baseline and after-scores recorded per file; every survivor has a disposition.

#### TS-06 — Stateful properties (ch5)

- **Model:** **Opus / astra.** The oracles encode concurrency and lifetime rules.
- **Owns:** new files `tests/property/stateful/*_test.ts`.
- **Reads:** `tests/property/arbitraries.ts`, `tests/core_test.ts` (conformance), `platform/sqlite.ts`,
  `platform/http/auth.ts`, `book-code/ch5/.../BasketPBTest.java`.
- **Work:** Use `fc.commands` with `fc.asyncModelRun`, the seed and run count from `arbitraries.ts`:
  1. **Storage vs model.** Random insert/replace/delete/query/transaction-abort sequences against
     `StorageLive` (FFI) with a plain `Map` model; after every command the query results match the model.
  2. **Tenant handle cache.** Acquire/release/idle-advance (`TestClock`)/evict across many tenants;
     invariants: entries ≤ capacity, leases ≥ 0, no evicted entry is in use, a released handle is reusable.
  3. **API-key cache.** Resolve/issue/revoke/advance-clock against a model with TTL; invariant: a revoked key
     never resolves after `revocation + TTL`.
  4. **Writer queue.** Concurrent submit/complete; invariant: in flight + waiting ≤ 1000, and job 1001 gets
     `QueueFullError`. Run with `CONTRACTS=full`: the `checkInvariant` functions from TS-13 are a second
     oracle after every command, so a model run fails on a broken invariant even where the model's own
     comparison would not notice.
- **Acceptance criteria:** each model runs 200 sequences in `test:prop` within the suite's current runtime
  budget (+25 % at most); a deliberately injected bug (documented in the PR, then reverted) is caught and
  shrunk to a short sequence.

#### TS-07 — Fuzz campaigns and regression corpus (ch5, write-up §5 "contracts as fuzz oracles")

- **Model:** **Opus / astra.** Decides what counts as a failure at trust boundaries.
- **Owns:** new `tests/property/fuzz/*_test.ts`, `tests/property/fuzz/corpus/*.json`,
  `work-scripts/bash/fuzz.sh`, `work-scripts/INDEX.md` (one row), `docs/testing/fuzzing.md`.
- **Reads:** `core/errors.ts`, `tests/errors_exhaustiveness.ts`, `platform/ipc.ts`, framing and JSON number
  code, `platform/http/router.ts`, `core/compile/*.ts`.
- **Work:**
  1. **Targets and oracle.** The oracle is: no untyped throw, no crash, no hang, every error is a member of
     the `core/errors.ts` taxonomy, **no `ContractViolation`** (ch4 4.4: "assertions serve as oracles during
     fuzzing"; a fuzzed input that passes validation and then trips a contract is a bug in validation or in
     the code), and the process stays healthy afterwards. Campaigns run at `CONTRACTS=full`. Targets:
     - IPC frame decoder and JSONL line reader (raw bytes, truncation, oversize, invalid UTF-8).
     - JSON number parsing and document decode (deep nesting, huge numbers, `__proto__` keys, NUL,
       surrogates).
     - HTTP: mutated document requests against an in-process server (headers, methods, bodies); every response
       status is in the route's allowed set from TS-01, never 500.
     - Compiler: random schemas and plans; generated SQL either runs on SQLite or the compiler returns a typed
       error, and DuckDB agrees with SQLite where both run (extends `sqlite_oracle_test.ts`).
  2. **Two modes.** In `ci`: 200 runs at the fixed seed, plus every corpus entry replayed. In `fuzz.sh`: a
     time box (`FUZZ_SECONDS`, default 600) across random seeds, logging each seed; any failure writes its
     shrunk counterexample to `corpus/` so it becomes a permanent `ci` case.
  3. **Optional.** A Jazzer.js run over `core/` under Node (coverage-guided), only if a 1-hour spike shows it
     runs; otherwise a note.
- **Acceptance criteria:** `ci` time grows by under 30 s; a 10-minute campaign runs clean or yields corpus
  entries plus `TS-FIX` reports; the docs explain replay.

#### TS-08 — Postgres adapter integration tests (ch9)

- **Model:** **Sonnet / sol** with **Opus** review. The harness exists; the semantics are fixed.
- **Owns:** new `tests/pg/*_test.ts`, `tests/pg/harness.ts`, `work-scripts/bash/test-pg.sh`,
  `work-scripts/INDEX.md` (one row).
- **Reads:** `tests/e2e/platform/postgres.ts`, `platform/tenants/store.ts`, `platform/http/auth.ts`,
  `platform/auth/service.ts`, `tests/layers/tenant_registry_test_layer.ts`,
  `book-code/ch9/src/.../sql/SqlIntegrationTestBase.java`.
- **Work:** One ephemeral Postgres per run, migrated once, with tables truncated before each test (the book's
  base-class pattern). Contract suite: the same cases run against `TenantRegistryTest` and the Postgres store,
  as `tests/core_test.ts` does for Storage. The contract suite checks both sides of substitution (ch4 4.3):
  postconditions, and preconditions, so an adapter that rejects an input the interface accepts (a stronger
  precondition) fails. Cover key issue/revoke/resolve, deactivation with `FOR UPDATE`, concurrent key issue,
  session login → membership → tenant, revocation visibility within the TTL, and the pool under 50 concurrent
  resolves (records the global-lock behaviour from §1.2 as data, not a fix).
- **Acceptance criteria:** the script runs from clean and leaves no container; both registry implementations
  pass the same contract; runtime under 2 min.

#### TS-09 — Risk-ranked system scenarios (ch9)

- **Model:** **Opus / astra.**
- **Scoping:** **SYNC**, alone.
- **Owns:** new `tests/e2e/platform/scenarios/*.ts` files and their registration in
  `tests/e2e/platform/main.ts` (additive).
- **Reads:** existing scenarios, `tests/e2e/platform/harness.ts`, §1.2.
- **Work:** Add scenarios in risk order, through HTTP and the SDK only:
  1. Backup and restore in tenancy mode (tests the suspected bug in §1.2 first).
  2. Server killed mid-mutation, restarted: committed writes survive, the uncommitted one is absent, and a
     client retry is applied exactly once.
  3. Overload: writer queue full → typed 503-class response; the server recovers without restart.
  4. Statement deadline → 504; the next request on the same tenant succeeds.
  5. API key revoked → rejected within `tenantKeyCacheTtlMs`.
  6. Reader child killed → supervised restart; `/health` reports it; queries resume.

  Anti-flake rules (ch9: system tests are slow and flaky): fixed seeds, ephemeral ports, polling with
  deadlines instead of sleeps, no retries inside assertions; each new scenario runs 20 times in a row clean
  before landing.
- **Acceptance criteria:** `deno task e2e` green with the new scenarios and skipped=none; a 20× loop log is
  attached to the report; total runtime stays within the suite's bound, or the bound change is justified.

#### TS-10 — Documentation, traceability and gate

- **Model:** **Sonnet / sol.**
- **Owns:** `docs/testing.md`, `tests/unit/spec_traceability_test.ts`, `docs/knowledge/decisions.md` (one
  dated entry).
- **Work:** Rewrite the tier table in `docs/testing.md` to §2's taxonomy and link the new docs. Add a test
  that parses `docs/testing/spec/*.md` and fails if a spec id is unreferenced by any test name or has no
  "untestable" reason, and if a test references an unknown id. Record the "coverage is a report, not a gate"
  decision.
- **Acceptance criteria:** `deno task ci` green including the traceability test; docs link every script.

## 4. Decisions needed

- **D1.** Scripts under `work-scripts/bash/` (default, keeps the `deno.json` freeze) or new `deno.json` tasks
  owned by TS-03.
- **D2.** Whether `fuzz.sh` and `mutation.sh` get a scheduled nightly run (needs a cron or CI runner; none
  exists today) or stay manual.
- **D3.** Whether TS-09's backup check runs first as a standalone quick check, before the plan starts.
- **D4.** Contract level in production: `cheap` (default proposed: O(1) checks always on) or `full`. Contracts
  halt the request with a 500 on a bug; that is the point (ch4: "halting is often much better than continuing
  with an incorrect value"), but it changes production behavior and needs your sign-off.

## 5. Out of scope

- Test doubles, testability and TDD (ch6–ch8) and test code quality (ch10).
- Performance tests (k6, `perf/`) and deterministic simulation testing (the main plan's stated future goal).
- Fixing any bug these tests find (each becomes a `TS-FIX` item).
