# Plan: Effect port, testability, DuckDB event logging, k6 perf

Status: proposed · Author: nickstrad (with Claude Code) · Date: 2026-10-10 · Target branch: feature branches
off `main`, one commit per item.

This plan evolves the document-db MVP in this repo (`platform/`, `sdk/`, `app/`, `scripts/`, `tests/`) along
six workstreams:

1. Port the codebase to Effect-TS: services, layers, typed error channels, dependency injection replacing the
   ad-hoc wiring in `platform/` and `sdk/`.
2. Refactor for testability as part of the port: pure core logic separated from effects, interfaces swappable
   with test layers.
3. Testing: unit tests on core logic **plus** property-based tests with fast-check on the schema/compiler/plan
   logic.
4. Event logging throughout: append-only structured event log, materialised into a DuckDB file.
5. Observability: DuckDB analysis queries over platform behaviour, plus folder-based log file rotation with a
   retention policy.
6. Perf: basic k6 load tests runnable against the server, joined against the DuckDB analysis logs.

Deterministic simulation testing (DST) is **explicitly not in scope** for this phase. It is the stated future
goal; see [Future work: deterministic simulation testing](#future-work-deterministic-simulation-testing).

---

## 0. Where the code is today

Verified by reading every file in the repo on 2026-10-10. Sizes matter here because they set the slice sizes
below.

| File                                | Lines  | What it does now                                                                                                                            | Why the port touches it                                                         |
| ----------------------------------- | ------ | ------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| `sdk/schema.ts`                     | 107    | `v.*` field builders, `Table`, `validate`, `parseObject`                                                                                    | Pure already, but signals failure by `throw new Error`                          |
| `sdk/db.ts`                         | 82     | `SelectPlan`/`Operation` IR, `Find`, `Documents`, `Execute = (op) => Promise<unknown>`                                                      | `Execute` is the central seam; becomes an `Effect`                              |
| `sdk/functions.ts`                  | 39     | `defineFunctions`, `Definition.invoke(db, input): Promise`                                                                                  | Handler return type becomes `Effect`                                            |
| `sdk/client.ts`                     | 21     | `fetch` wrapper, throws on non-2xx                                                                                                          | Becomes Effect + trace-id propagation                                           |
| `platform/compiler.ts`              | 173    | Schema→DDL, plan→SQL per dialect, insert/replace/delete, row decode                                                                         | Pure logic that throws; the prime property-test target                          |
| `platform/sqlite.ts`                | 108    | `SQLiteWriter`: opens DB in constructor, promise-chain mutex (`tail`/`pending`), `BEGIN IMMEDIATE`, `VACUUM INTO`                           | Becomes a scoped `Storage` service with a `Semaphore`                           |
| `platform/duckdb.ts`                | 52     | `DuckReader.open` static async ctor, per-query transaction                                                                                  | Becomes a scoped `Reader` service                                               |
| `platform/ipc.ts`                   | 62     | `lines()` async-generator framing; `ReaderRPC` spawns a child in its constructor, hand-rolled pending map                                   | Framing becomes pure; RPC becomes a scoped service with `Deferred`              |
| `platform/registry.ts`              | 26     | `loadApps()` reads `Deno.env` directly; `trace()` reads `TRACE_SQL` and `console.error`s                                                    | Becomes `AppRegistry` + `Config` services; `trace` is replaced by the event log |
| `platform/server.ts`                | 100    | Top-level-await script: writers map, child spawn, `Deno.serve`, regex routing, `setInterval` backup timer, signal handlers, `console.error` | Becomes a layer composition + Effect HTTP handler + scheduled fiber             |
| `platform/backup.ts`                | 42     | `VACUUM INTO` → `quick_check` → SHA-256 → immutable PUT → latest manifest                                                                   | Becomes a `Backups` service over `Storage` + `ObjectStore`                      |
| `platform/object_store.ts`          | 57     | `objectStore()` env-sniffing factory, local stand-in or signed S3                                                                           | Becomes two layers selected by `Config`                                         |
| `app/schema.ts`, `app/functions.ts` | 21, 90 | Tic-tac-toe schema and handlers; the only app code                                                                                          | Handlers move to `Effect.gen`; this is the API-ergonomics proof                 |
| `tests/core_test.ts`                | 212    | 20 tests via `node:test` + real `node:sqlite`                                                                                               | Stays as the regression net; gains an Effect runner                             |
| `tests/types.ts`                    | 24     | Compile-only `@ts-expect-error` assertions                                                                                                  | Must keep passing; guards the typed API                                         |
| `tests/integration.ts`              | 85     | Spawns the real server, real DuckDB, backup + restore                                                                                       | Stays end-to-end, ported last                                                   |
| `scripts/explain.ts`                | 15     | Regenerates `GENERATED_SQL.txt` with no DB driver                                                                                           | Cheap canary for compiler refactors                                             |

**Baseline verified on this droplet, 2026-10-10:**

```sh
export PATH="$HOME/.deno/bin:$PATH"   # deno 2.9.5 is NOT on PATH in agent shells
deno test -A tests/core_test.ts       # ok | 20 passed | 0 failed (715ms)
deno check scripts/explain.ts         # ok
```

Environment facts that constrain the plan:

- Deno 2.9.5 lives at `/root/.deno/bin/deno` and is not on `PATH`; every command below assumes the
  `export PATH` line above.
- There is **no `deno.lock`** and no `node_modules/` yet. Deno 2.9.5 enforces a 24-hour minimum dependency
  age, so the first dependency resolution must run `deno install --minimum-dependency-age=0` once and commit
  `deno.lock`.
- **k6 is not installed** (`/usr/local/bin` has no `k6`). Workstream 6 starts with an install slice.
- There is no `duckdb` CLI; all DuckDB access goes through `npm:@duckdb/node-api@1.5.6-r.1`, which is already
  pinned in `platform/duckdb.ts`.
- `data/`, `object-store/` and `node_modules/` are gitignored; `deno.lock` is deliberately tracked. A new
  `logs/` entry must be added to `.gitignore` (item 01).

### The hard architectural constraint to respect

`platform/duckdb.ts` carries a comment that is load-bearing: the reader process _never_ imports `node:sqlite`,
because DuckDB's `sqlite` extension and Node's SQLite are two copies of the SQLite library in one address
space. The whole two-process split exists for that reason.

Therefore **the writer process must never import `@duckdb/node-api`**, which rules out "mutations append
straight into a DuckDB file". The event-log design below works with that constraint rather than against it:
rotating JSONL files are the durable write path in every process, and the DuckDB event tables are built by
ingesting those files from the reader process, which already owns DuckDB. That single decision satisfies
workstreams 4 and 5 with one write path and no new native-library risk.

---

## 1. Target architecture

### 1.1 Directory layout after the port

```text
core/                 # PURE. No Deno.*, no I/O, no npm natives. Property-test target.
  errors.ts           #   Data.TaggedError taxonomy (shared by every layer)
  schema.ts           #   total validation: Either<SchemaError, T>
  compile/            #   ddl.ts select.ts write.ts decode.ts dialect.ts  (all Either-returning)
  plan.ts             #   plan normalisation + limit clamping
  events.ts           #   event record types, constructors, redaction rules
  eventlog/ddl.ts     #   append-only DuckDB DDL + ingest-offset maths
  ipc/framing.ts      #   incremental JSON-lines decoder (pure state machine)
  logging/rotation.ts #   dated path computation + retention decisions (pure)
platform/             # EFFECTFUL. Services + layers only.
  services.ts         #   Context.Tag declarations + interface types (no implementations)
  config.ts           #   Config service; the ONLY place that reads Deno.env
  layers.ts           #   PlatformLive / ReaderLive composition roots
  logging/            #   logger.ts (Effect Logger) file_sink.ts retention.ts
  eventlog/           #   ingest.ts (JSONL -> DuckDB), tables.ts
  analysis/           #   catalog.ts (named analysis queries, no caller-supplied SQL)
  http/               #   router.ts auth.ts handlers.ts
  sqlite.ts duckdb.ts ipc.ts object_store.ts backup.ts registry.ts reader.ts server.ts
sdk/                  # App-facing. Effect-typed.
app/                  # Unchanged responsibility: tic-tac-toe rules, now in Effect.gen
perf/                 # k6 scripts (JS, run by the k6 binary, not by Deno)
scripts/              # demo.ts explain.ts restore.ts logs.ts
tests/
  unit/               #   per-module unit tests on core/
  property/           #   fast-check properties on core/
  layers/             #   test layers (StorageTest, ObjectStoreTest, EventLogTest, ...)
  core_test.ts        #   existing 20-test regression net, Effect-ified
  types.ts            #   compile-only type assertions
  integration.ts      #   real multi-process end-to-end
docs/
  plans/              #   this file
  adr/                #   decision records produced by the port
```

**The rule that makes this testable:** nothing in `core/` may import from `platform/`, `sdk/`, `node:*`,
`npm:*`, or touch `Deno.*`. Item 26 adds a test that greps for violations so the boundary cannot rot.

### 1.2 Services and tags

Declared once in `platform/services.ts` (item 02) as `Context.Tag`s with interface types only. Implementations
land in later items; test doubles land in item 20.

| Tag              | Interface (abridged)                                                                              | Live layer                                | Test layer                               |
| ---------------- | ------------------------------------------------------------------------------------------------- | ----------------------------------------- | ---------------------------------------- |
| `Config`         | `readonly server: {...}; apps: ...; backup: ...; logging: ...`                                    | `ConfigLive` (env)                        | `ConfigTest(overrides)`                  |
| `IdGen`          | `next: Effect<string>`                                                                            | `IdGenLive` (`crypto.randomUUID`)         | `IdGenTest` (`id-1`, `id-2`, …)          |
| `Clock`          | Effect built-in                                                                                   | default                                   | `TestClock`                              |
| `EventSink`      | `emit(e: EventRecord): Effect<void>`                                                              | `EventSinkLive` (queue → JSONL)           | `EventSinkTest` (`Ref<EventRecord[]>`)   |
| `AppRegistry`    | `get(id): Effect<App, UnknownAppError>; all: Effect<App[]>`                                       | `AppRegistryLive`                         | `AppRegistryTest`                        |
| `Storage`        | `mutation(def, args): Effect<unknown, MutationError>; snapshot(path): Effect<void, StorageError>` | `StorageLive` (scoped `node:sqlite`)      | `StorageTest` (pure in-memory doc store) |
| `Reader`         | `query(def, args): Effect<unknown, QueryError>`                                                   | `ReaderLive` (scoped DuckDB)              | `ReaderTest`                             |
| `ReaderClient`   | `call(req): Effect<unknown, IpcError>`                                                            | `ReaderClientLive` (scoped child process) | `ReaderClientTest` (in-process `Reader`) |
| `ObjectStore`    | `put(key, bytes): Effect<void, ObjectStoreError>; get(key)`                                       | `ObjectStoreLocal` \| `ObjectStoreS3`     | `ObjectStoreTest` (`Ref<Map>`)           |
| `Backups`        | `take(appId): Effect<Manifest, BackupError>`                                                      | `BackupsLive`                             | reuse live over test deps                |
| `EventLogIngest` | `ingest: Effect<IngestReport, IngestError>`                                                       | `EventLogIngestLive` (DuckDB)             | `EventLogIngestTest`                     |
| `Analysis`       | `run(name, params): Effect<Row[], AnalysisError>`                                                 | `AnalysisLive`                            | n/a                                      |

`ReaderClientTest` is the interesting one: substituting an in-process `Reader` for the child process lets the
whole HTTP surface be tested in one process with no subprocess and no DuckDB.

### 1.3 Error channel

Today every failure is `throw new Error(string)`, and `platform/server.ts` turns _all_ of them into HTTP 400.
After the port, failures are `Data.TaggedError` classes in `core/errors.ts`, and HTTP status mapping is a
total function over the tag union:

```text
SchemaError       -> 400   (validation, unknown field, bad enum)
CompileError      -> 400   (unsupported operator, array predicate, bad limit)
UnknownAppError   -> 404
UnknownHandlerError -> 404
HandlerKindMismatch -> 400
AuthError         -> 401
RequestTooLarge   -> 413
ConflictError     -> 409   (stale _version; today a 400 string match)
NotFoundError     -> 404   ("Document not found", "Game not found")
QueueFullError    -> 503   (writer/reader queue caps)
StorageError      -> 500
QueryError        -> 500
IpcError          -> 502
ObjectStoreError  -> 502
BackupError       -> 500
ConfigError       -> fails at startup, never served
```

`ConflictError -> 409` and `QueueFullError -> 503` are behaviour changes, not just refactors. They are called
out in the acceptance criteria of item 22 and in `README.md` (item 27).

### 1.4 The SDK seam

```ts
// before
export type Execute = (operation: Operation) => Promise<unknown>;

// after
export type Execute = (operation: Operation) => Effect.Effect<unknown, DbError>;
```

`Find`/`Documents` keep their shape; their methods return `Effect` instead of `Promise`. Handlers written by
app authors become:

```ts
makeMove: mutation({
  args: { gameId: v.string(), playerId: v.string(), cell: v.integer(), expectedVersion: v.integer() },
  handler: ({ db }, { gameId, playerId, cell, expectedVersion }) =>
    Effect.gen(function* () {
      const game = yield* db.get("games", gameId);
      if (!game) return yield* new NotFoundError({ table: "games", id: gameId });
      if (game._version !== expectedVersion) {
        return yield* new ConflictError({ expected: expectedVersion, actual: game._version });
      }
      const next = applyMove(game, playerId, cell); // pure, from app/rules.ts
      yield* db.patch("games", gameId, next.game);
      yield* db.insert("moves", next.move);
      return yield* db.get("games", gameId);
    }),
});
```

Note the second testability win: the tic-tac-toe rules move into a pure `applyMove` in `app/rules.ts`, so the
game logic is unit- and property-testable without a database at all.

### 1.5 Dependency pins (versions checked against the npm registry on 2026-10-10)

```jsonc
// deno.json "imports"
{
  "effect": "npm:effect@3.22.2",
  "fast-check": "npm:fast-check@4.10.2",
  "@duckdb/node-api": "npm:@duckdb/node-api@1.5.6-r.1", // already used, now mapped
  "aws4fetch": "npm:aws4fetch@1.0.20" // already used, now mapped
}
```

Decisions, with reasons:

- **Pin `effect@3.22.2`, not `4.0.2`.** Effect 4.0.0 shipped on 2026-10-01 and 4.0.2 on 2026-10-07; the 3.x
  line's last release is 3.22.2 (2026-09-09) and the surrounding ecosystem still peers on `effect@^3.22.2`
  (`@effect/platform@0.97.2`). A nine-day-old major is the wrong thing to port a codebase onto. Revisit after
  this phase; the service/layer concepts carry over.
- **No `@effect/platform` / `@effect/platform-node`.** The HTTP surface is 25 lines of routing over
  `Deno.serve`. Wrapping `Deno.serve` in `Effect.acquireRelease` keeps the dependency surface at two packages
  and avoids pulling a Node-API platform implementation into a Deno process. If the HTTP layer later needs
  OpenAPI/`HttpApi`, adopt `@effect/platform@0.97.2` then.
- **`fast-check@4.10.2`** for property tests; `deno test` is the runner, so no vitest and no `@effect/vitest`.

---

## 2. Conventions for executing this plan

### 2.1 Model assignment policy

Each item names the Anthropic model that should execute it. The rule:

- **Opus** — the item decides semantics that are expensive to get wrong later: SQL generation, transaction and
  concurrency boundaries, resource lifetimes (scopes, child processes, DB handles), public type-level API, or
  the error taxonomy. Map to the repo's `implementer-opus-high` agent.
- **Sonnet** — the item is bounded and mechanical: the interface is already fixed by an earlier item, the file
  list is short, and the acceptance criteria are checkable by running a command. Map to
  `implementer-sonnet-high`.

Every item, regardless of model, gets a `reviewer-opus-high` pass against its acceptance criteria before it
lands. Reviews are not separate items.

### 2.2 Batch, concurrency, and file-ownership rules

- A **batch** is a set of items that may run **CONCURRENTLY**: no shared files, no ordering dependency between
  them.
- An item marked **SYNC** must complete and land before the next batch starts. Where a whole batch is a
  prerequisite, that is stated on the batch.
- Every item lists **Owns** (files it may create or edit) and **Reads** (files it may import but not edit).
  Two items in the same batch must have disjoint `Owns` sets. A reviewer should reject a diff that edits a
  file outside the item's `Owns` set.
- `deno.json` is the one contended file in the repo. Item 01 therefore writes the **complete** task table and
  import map up front, including tasks that point at files which do not exist yet (a `deno task` only fails
  when it is run). After item 01, no item edits `deno.json`.
- One item = one commit. Commit subject: `<area>: <imperative summary>` (e.g.
  `core: extract total schema validation`). Each commit must leave `deno task ci` green, which is why
  compatibility wrappers are explicit deliverables in items 04, 09 and 10 and are removed in item 26.

### 2.3 Commands the acceptance criteria use

All of these are added by item 01.

```sh
export PATH="$HOME/.deno/bin:$PATH"

deno task fmt:check     # deno fmt --check
deno task lint          # deno lint
deno task check         # deno check over every entry point
deno task test          # unit + regression tests (tests/unit/, tests/core_test.ts)
deno task test:prop     # fast-check property tests (tests/property/)
deno task ci            # fmt:check + lint + check + test + test:prop
deno task explain       # regenerate compiler output, no DB driver
deno task integration   # real server + real DuckDB + backup + restore
deno task serve         # run the service
deno task logs:ingest   # JSONL -> DuckDB append-only tables
deno task logs:query    # named analysis query
deno task logs:sweep    # apply the retention policy
deno task perf:smoke    # k6 smoke
deno task perf:load     # k6 mutation load
deno task perf:query    # k6 query load
deno task perf:mixed    # k6 mixed read/write
deno task perf:ingest   # k6 JSON output -> DuckDB k6_points
```

---

## 3. Batches and items

37 items in 12 batches (0–11). Batch 11 is appended below but executes immediately after 22 and the item-24
prerequisite, before 23 and every MT item. Summary first; full item specs follow.

| Batch         | Items              | Concurrency                                           | Gate to next batch                                 |
| ------------- | ------------------ | ----------------------------------------------------- | -------------------------------------------------- |
| 0             | 01                 | single item, **SYNC**                                 | lockfile + tasks exist                             |
| 1             | 02, 03             | 2 concurrent; **both SYNC prerequisites** for batch 2 | error taxonomy + service tags frozen; k6 installed |
| 2             | 04, 05, 06, 07, 08 | 5 concurrent                                          | pure core modules exist                            |
| 3             | 09, 10             | 2 concurrent; **both SYNC prerequisites** for batch 4 | compiler + SDK Effect types frozen                 |
| 4             | 11, 12, 13, 14     | 4 concurrent                                          | logger + property suites + k6 scenarios            |
| 5             | 15, 16, 17, 18     | 4 concurrent                                          | all four adapters are Effect services              |
| 6             | 19, 20, 21         | 3 concurrent                                          | backups, test layers, DuckDB ingest                |
| 7             | 22                 | single item, **SYNC**                                 | server runs entirely on layers                     |
| 8 (early)     | 24                 | prerequisite implementation, **SYNC**                 | retention base for 36                              |
| 11            | 30–37              | staged SYNC gate; see batch 11                        | correctness fixes before 23 and MT                 |
| 8 (remaining) | 23, 25             | 2 concurrent, after batch 11                          | analysis CLI and integration                       |
| 9             | 26, 27             | 2 concurrent                                          | wrappers removed, docs true                        |
| 10            | 28, 29             | 2 concurrent; starts only after batch 9 lands         | runnable demos, platform user guide                |

**Non-reorderable build order (overrides historical batch-8 concurrency):**
`22 → 24 → batch 11: (30+35) → 31 → 36 → 32 → 33 → 34 → 37 →
(23+25+MT-01+MT-02) → (26+MT-03) → (MT-04+MT-05) → (MT-07+MT-09) → (MT-06+MT-08) → MT-10 → 27 → (28+29)`.
Item 24 moves out of the old batch-8 concurrent slot; its implementation is a prerequisite for 36. Its CLI
acceptance needs item 23's `scripts/logs.ts`: record that check as pending until 23, while requiring its
direct retention unit tests before 36. Never claim the missing CLI passed. Batch 11 must fully pass before
item 23 or any MT item starts; these fixes must not queue behind multitenancy. The MT continuation and its
existing gates remain as recorded in `multitenancy.md` §3.

---

### Batch 0 — bootstrap

#### Item 01 — Dependency pins, import map, complete task table, lockfile

- **Scoping:** SYNC. Single item in its batch. Everything else depends on the lockfile and tasks.
- **Model:** **Sonnet / sol.** Mechanical configuration with an exactly specified target and a
  command-checkable outcome.
- **Owns:** `deno.json`, `deno.lock` (new), `.gitignore`.
- **Reads:** nothing.
- **Work:**
  1. Add the `imports` map from §1.5. Keep `nodeModulesDir: auto`, `compilerOptions.strict`, and
     `fmt.lineWidth: 110`. Add `"lint": { "rules": { "tags": ["recommended"] } }`.
  2. Replace `tasks` with the complete table from §2.3. Tasks that reference files created later are expected
     to fail until their item lands; that is acceptable and must be noted in the commit message.
  3. Run `deno install --minimum-dependency-age=0` once (Deno 2.9.5 refuses npm versions younger than 24 h
     otherwise) and commit `deno.lock`.
  4. Add `logs/` and `perf/results/` to `.gitignore`.
  5. Keep type checking on in every test task (no `--no-check`); the compile-only assertions in
     `tests/types.ts` are part of the test signal.
- **Acceptance criteria:**
  - `deno.lock` is committed and contains `effect@3.22.2` and `fast-check@4.10.2`.
  - `deno task check` passes (unchanged source still type-checks with the new import map).
  - `deno task test` passes: 20 tests, matching the recorded baseline.
  - `deno eval 'import * as E from "effect"; console.log(typeof E.Effect.succeed)'` prints `function` — proves
    the import map resolves from the lock without the age flag.
  - `deno task fmt:check` and `deno task lint` pass on the untouched tree (fix formatting drift in this commit
    if any exists).

---

### Batch 1 — architectural spine + perf bootstrap

Items 02 and 03 run **concurrently** (one touches only TypeScript, the other only `perf/` and the host).
**Both are SYNC prerequisites for batch 2**: every batch-2 item imports `core/errors.ts`.

#### Item 02 — Error taxonomy and service tags (declarations only)

- **Scoping:** CONCURRENT with 03. SYNC prerequisite for batches 2–9.
- **Model:** **Opus / astra.** This is the contract every other item codes against; the tag union, the error
  payload shapes and the service interface signatures are the one thing in this plan that is expensive to
  change later.
- **Owns:** `core/errors.ts`, `platform/services.ts`, `docs/adr/0001-effect-port.md`,
  `docs/adr/0002-error-taxonomy.md`.
- **Reads:** all existing sources (to enumerate every current `throw` site).
- **Work:**
  1. Enumerate every `throw new Error(...)` in the repo (there are ~40) and map each to a tagged error class.
     `core/errors.ts` defines them with `Data.TaggedError`, each carrying structured fields rather than a
     formatted string (e.g. `SchemaError({ path, expected, actual })`, `CompileError({ reason, field? })`,
     `ConflictError({ expected, actual })`, `QueueFullError({ queue, depth, cap })`).
  2. Define union aliases: `DbError`, `MutationError`, `QueryError`, `PlatformError`.
  3. Define `toHttpStatus(e: PlatformError): number` as a total switch over `_tag` (used by item 22, tested
     there).
  4. `platform/services.ts`: `Context.Tag` declarations and interface types for every row of the §1.2 table.
     **No implementations, no layers.** Also freeze the reader IPC protocol type
     (`ReaderRequest`/`ReaderReply`) here, so items 16 and 17 can be built concurrently against it.
  5. ADR 0001 records: Effect 3.22.2 over 4.x, no `@effect/platform`, `core/` purity rule, JSONL as the
     durable log write path with DuckDB as a derived analysis sink, and why the writer process must not import
     DuckDB.
- **Acceptance criteria:**
  - `deno task check` passes; `core/errors.ts` and `platform/services.ts` are included in the `check` entry
    points.
  - `deno task test` still passes 20 tests (no behaviour touched).
  - A compile-only assertion file proves `toHttpStatus` is exhaustive: adding a tag to the union without a
    case is a type error (use a `never`-returning default branch).
  - `grep -rE "from \"(\.\./)*platform/" core/` returns nothing.
  - ADR 0001 names each of the six workstreams and the item numbers that deliver them.

#### Item 03 — k6 install and smoke scenario against the current server

- **Scoping:** CONCURRENT with 02. Touches no TypeScript.
- **Model:** **Sonnet / sol.** Install a binary, write one k6 script against an HTTP API that already exists
  and is documented in `README.md`.
- **Owns:** `perf/smoke.js`, `perf/lib/api.js`, `perf/README.md`, `docs/adr/0003-k6-perf.md`.
- **Reads:** `README.md`, `platform/server.ts` (routes, auth header), `scripts/demo.ts` (call order).
- **Work:**
  1. Install k6 to `/usr/local/bin/k6` (official Grafana Linux amd64 tarball; record the exact version and URL
     in `perf/README.md`). Verify with `k6 version`. Record the install recipe in the knowledge store via the
     `update-knowledge-store` skill — k6 was absent on this droplet, so that is a durable finding.
  2. `perf/lib/api.js`: shared helpers — base URL, app id, bearer token, `POST /apps/:app/:kind/:name`
     wrapper, `X-Trace-Id` header per iteration (`${__VU}-${__ITER}-${runId}`), JSON body parse.
  3. `perf/smoke.js`: 1 VU / 30 s. `createGame` → `getGame` → 5× `makeMove` → `winsByMark`. Thresholds:
     `http_req_failed < 0.01`, `http_req_duration p(95) < 250`, plus `checks` rate 1.0.
  4. `perf/README.md`: how to start the server (`deno task serve` with a scratch `DATA_DIR`), run the scripts,
     where output lands (`logs/k6/<run-id>/`), and the explicit caveat from the repo README that this
     prototype's capacity is not a benchmark claim.
- **Acceptance criteria:**
  - `k6 version` prints the pinned version.
  - With `DATA_DIR=$(mktemp -d) deno task serve` running: `deno task perf:smoke` exits 0 and all thresholds
    pass.
  - `k6 run perf/smoke.js` with the server **down** exits non-zero (the script fails loudly rather than
    reporting a vacuous pass).
  - `logs/k6/<run-id>/raw.json` and `summary.json` exist after a run.
  - A `kb search` for k6 on this droplet now returns the install entry.

---

### Batch 2 — pure core extraction (5 concurrent)

All five depend on item 02 and on nothing else. File ownership is disjoint. Each keeps a throwing
compatibility wrapper so the existing 20 tests stay green within the same commit.

#### Item 04 — `core/schema.ts`: total validation

- **Scoping:** CONCURRENT with 05–08.
- **Model:** **Opus / astra.** Converting `validate`/`parseObject` to total `Either`-returning functions while
  preserving exact semantics (nullable handling, `Number.isSafeInteger` for integers, fixed array length,
  recursive array validation, unknown-field rejection, partial mode for patches) is a semantics-preserving
  refactor where a subtle change silently weakens validation.
- **Owns:** `core/schema.ts`, `sdk/schema.ts`, `tests/unit/schema_test.ts`.
- **Reads:** `core/errors.ts`.
- **Work:**
  1. Move `Node`, `Field`, `Fields`, `Infer`, `Storage`, `Table`, `identifier`, `defineTable`, `defineSchema`,
     `withStorage`, `validate`, `parseObject` into `core/schema.ts`.
  2. New signatures: `validate(node, value, path): Either<SchemaError, unknown>`,
     `parseObject(fields, input, partial?): Either<SchemaError, Infer<F>>`,
     `identifier(name): Either<SchemaError, string>`. Accumulate the failing path; do not stop at a formatted
     message.
  3. `sdk/schema.ts` becomes the app-facing re-export plus `validateOrThrow`/`parseObjectOrThrow` wrappers
     that `Either.getOrThrowWith` into the old `Error` messages, so `platform/compiler.ts`,
     `platform/sqlite.ts` and `sdk/functions.ts` compile untouched. `Table.index` and `defineTable` keep
     throwing for now (they run at module load in `app/`).
  4. `tests/unit/schema_test.ts`: one unit test per node kind, per nullable combination, per failure mode,
     plus the `_`-prefixed reserved-name and `sqlite_` reserved-table rules.
- **Acceptance criteria:**
  - `deno task test` passes: 20 existing tests plus the new unit tests, zero regressions.
  - `deno task check` passes, including `tests/types.ts` (the `@ts-expect-error` assertions must still be
    errors — the inferred app types are unchanged).
  - `deno task explain` output is byte-identical to the committed `GENERATED_SQL.txt`.
  - Every error message string asserted by `tests/core_test.ts` (`/Unknown field/`, `/expected integer/`)
    still matches through the wrapper.
  - `grep -n "throw" core/schema.ts` returns nothing.

#### Item 05 — `core/ipc/framing.ts`: pure JSON-lines decoder

- **Scoping:** CONCURRENT with 04, 06, 07, 08.
- **Model:** **Sonnet / sol.** Small, self-contained state machine with an obvious contract; the current
  `lines()` implementation is 14 lines and already correct.
- **Owns:** `core/ipc/framing.ts`, `tests/unit/framing_test.ts`, `platform/ipc.ts`.
- **Reads:** `core/errors.ts`.
- **Work:**
  1. Extract the buffering logic into a pure incremental decoder:
     `decoder(maxBytes): { push(chunk: string): Either<IpcError, string[]>; finish(): Either<IpcError, void> }`.
     Preserve today's rules exactly: empty lines are skipped, a buffer over 2 MiB fails with
     `MessageTooLarge`, and a non-empty trailing buffer at end-of-stream fails with `TruncatedMessage`.
  2. `platform/ipc.ts`: `lines()` delegates to the pure decoder. The `ReaderRPC` class is otherwise untouched
     in this item (item 17 ports it).
  3. Unit tests: single message, multiple messages in one chunk, message split across chunks, split
     mid-multibyte-character (decode at the stream boundary, not inside the decoder), empty lines, oversize,
     truncated tail.
- **Acceptance criteria:**
  - `deno task test` passes with no regressions.
  - `deno task integration` still passes (the real child-process transport is unchanged) — or, if DuckDB
    cannot be fetched in the executing environment, record the skip explicitly in the commit message rather
    than claiming a pass.
  - Unit tests cover all seven cases above.

#### Item 06 — `core/logging/rotation.ts`: pure rotation and retention policy

- **Scoping:** CONCURRENT with 04, 05, 07, 08.
- **Model:** **Sonnet / sol.** Pure date/path arithmetic against a specified policy; the risky part (actually
  deleting files) is deliberately not in this item.
- **Owns:** `core/logging/rotation.ts`, `tests/unit/rotation_test.ts`.
- **Reads:** `core/errors.ts`.
- **Work:**
  1. Define the on-disk layout as data: `logs/<role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl`, where
     `role ∈ {server, reader,
     restore, cli}`.
  2. `currentPath(policy, now, role, pid, seq): string` and
     `shouldRoll(state, now, bytesWritten, policy): "no" | "size" | "date" | "hour"`.
  3. `plan(entries: FileEntry[], now, policy): RetentionPlan` where
     `FileEntry = { path, dir, bytes,
     mtime }` and
     `RetentionPlan = { gzip: string[]; delete: string[]; keep: string[] }`. Policy:
     `{ keepDays, gzipAfterDays, maxTotalBytes, maxFileBytes }` with defaults
     `{ keepDays: 14, gzipAfterDays: 2, maxTotalBytes: 2 GiB, maxFileBytes: 64 MiB }`.
  4. Invariants the function must guarantee (asserted again as properties in item 12): today's directory is
     never gzipped or deleted; the currently open file is never in `gzip` or `delete`; `gzip ∩ delete = ∅`;
     `gzip ∪ delete ∪ keep` is exactly the input set; when `maxTotalBytes` is exceeded, oldest dated
     directories are pruned first until the cap is met or only today remains.
  5. Unit tests with a frozen `now` across day, hour, size and cap boundaries.
- **Acceptance criteria:**
  - `deno task test` passes; unit tests cover each invariant with an explicit case.
  - `grep -nE "Deno\.|node:" core/logging/rotation.ts` returns nothing.
  - `plan` is idempotent: applying it to the post-plan file set yields empty `gzip` and `delete`.

#### Item 07 — `core/events.ts`: event taxonomy, constructors, redaction

- **Scoping:** CONCURRENT with 04, 05, 06, 08.
- **Model:** **Sonnet / sol.** Type and constructor definitions against the schema fixed in §4.1 of this plan;
  no I/O.
- **Owns:** `core/events.ts`, `tests/unit/events_test.ts`.
- **Reads:** `core/errors.ts`.
- **Work:**
  1. `EventRecord` discriminated union over `kind`: `http` | `handler` | `sql` | `tx` | `queue` | `backup` |
     `lifecycle` | `ingest`, with the shared envelope
     `{ eventId, ts, level, role, pid, traceId, spanId?, parentSpanId?, appId? }`.
  2. Constructors (`httpEvent`, `sqlEvent`, …) that take structured input and return a record;
     `toJsonLine(record): string` producing one canonical, key-ordered JSON line.
  3. Redaction: `sql` events carry `sqlText` and `bindCount` but **never bind values** unless
     `logging.includeBindValues` is true, mirroring today's `TRACE_SQL=1` caveat that it logs application
     values. Bearer tokens, `authorization` headers and S3 credentials are redacted unconditionally; player
     ids and document ids are not (they are the data we want to analyse).
  4. `fromPlatformError(e): { errorTag, errorDetail }` so every tagged error has a stable analysable column.
- **Acceptance criteria:**
  - `deno task test` passes; unit tests assert one line per event kind against a golden fixture.
  - A test asserts that a record constructed with `includeBindValues: false` and a bind value of
    `"O'Reilly'); DROP TABLE games; --"` does not contain that substring in `toJsonLine` output.
  - `toJsonLine` output contains no newline characters (so one record is always one line).
  - Key order is stable across runs (golden comparison, not `deepEqual`).

#### Item 08 — `platform/config.ts`: the single env boundary

- **Scoping:** CONCURRENT with 04–07.
- **Model:** **Sonnet / sol.** Mechanical consolidation of nine `Deno.env.get` call sites behind one service,
  with validation rules that already exist in the code.
- **Owns:** `platform/config.ts`, `tests/unit/config_test.ts`, plus the "Configuration" section of `README.md`
  (nothing else in that file).
- **Reads:** `core/errors.ts`, `platform/services.ts`, and every current `Deno.env` site (`registry.ts`,
  `server.ts`, `object_store.ts`).
- **Work:**
  1. One typed record covering `PORT`, `DATA_DIR`, `COLUMN_APP_TOKEN`, `JSON_APP_TOKEN`, `ADMIN_TOKEN`,
     `BACKUP_INTERVAL_MS`, `TRACE_SQL`, `S3_BASE_URL`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`,
     `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `LOCAL_OBJECT_DIR`, plus new `LOG_DIR`, `LOG_LEVEL`,
     `LOG_RETENTION_DAYS`, `LOG_GZIP_AFTER_DAYS`, `LOG_MAX_TOTAL_BYTES`, `LOG_INCLUDE_BIND_VALUES`,
     `EVENTLOG_DUCKDB`, `EVENT_QUEUE_CAPACITY`, `EVENT_FLUSH_MS`.
  2. `ConfigLive` reads the environment once at layer construction and fails with `ConfigError` rather than at
     first use. Port today's validation: `BACKUP_INTERVAL_MS` must be `0` or `>= 1000` and finite; S3
     credentials are required when `S3_BASE_URL` is set.
  3. `ConfigTest(partial)` merges over documented defaults for tests.
  4. Keep the dev-token defaults (`dev-only`) **and** emit a `lifecycle` warn-level event when a default token
     is in use, replacing today's unconditional `console.error` banner.
  5. Design the module for extension over time: every option is declared exactly once in a single options
     table (option name, env var, type, default, human description). Adding a new option later is one row in
     that table — no new plumbing, no new call sites.
  6. Enforce the schema with Effect Schema (`effect/Schema`), which is already a dependency. Zod is the
     fallback only if Effect Schema cannot express a rule — if used, document in a code comment exactly which
     rule forced the fallback. Do not add Zod speculatively.
  7. `README.md` gains a "Configuration" section that documents every option: name, env var, default, and what
     it does, so the README explains how to configure the current platform version. The builder keeps this
     section in sync with the options table — they must match exactly.
- **Acceptance criteria:**
  - `deno task test` passes; unit tests cover each validation rule and each default.
  - `grep -rn "Deno.env" platform/ sdk/ core/ | grep -v platform/config.ts` returns nothing after items 15–22
    land; **in this item** it returns only the not-yet-ported call sites, and the commit message lists them
    with their owning item numbers.
  - A test proves `ConfigLive` fails with `ConfigError` for `BACKUP_INTERVAL_MS=500` and for `S3_BASE_URL` set
    without credentials.
  - The reviewer verifies the README "Configuration" section lists exactly the options in the options table
    (same names, env vars, defaults).

---

### Batch 3 — compiler and SDK Effect core (2 concurrent, both SYNC gates)

Items 09 and 10 are the two deepest cuts. They are concurrent because their file sets are disjoint, but **both
must land before batch 4** because items 11–14 and all of batch 5 compile against them.

#### Item 09 — `core/compile/*`: pure, total compiler

- **Scoping:** CONCURRENT with 10. SYNC prerequisite for batch 4.
- **Model:** **Opus / astra.** This is the SQL-generation core: dialect-specific JSON extraction and casts,
  bind-parameter numbering (`?` vs `$pN`), `NULLS FIRST/LAST` ordering with the `_id` tie-breaker, `count` vs
  `countBy` limit semantics, STRICT-mode DDL with enum/boolean/array CHECKs. A subtle change here is an
  injection bug or a silent query-semantics change.
- **Owns:** `core/compile/dialect.ts`, `core/compile/ddl.ts`, `core/compile/select.ts`,
  `core/compile/write.ts`, `core/compile/decode.ts`, `core/plan.ts`, `platform/compiler.ts`,
  `tests/unit/compile_test.ts`.
- **Reads:** `core/schema.ts`, `core/errors.ts`, `sdk/db.ts` (IR types only).
- **Work:**
  1. Move `quote`, `literal`, `tableFor`, `fieldNode`, `scalar`, `expression`, `encode`, `columnType`,
     `compileDDL`, `compileSelect`, `compileInsert`, `compileReplace`, `compileDelete`, `decode`,
     `safeNumber`, `decodeValue` into `core/compile/*`, all returning `Either<CompileError | SchemaError, …>`.
  2. `core/plan.ts`: `normalise(plan): Either<CompileError, SelectPlan>` centralising the limit rule
     (`1..1000`, integer) that is currently duplicated in `sdk/db.ts:34` and `platform/compiler.ts:78`, plus
     the `aggregate` ⇒ `order: undefined` normalisation currently done ad hoc in `Find.count`/`Find.countBy`.
  3. `platform/compiler.ts` becomes a thin re-export plus `*OrThrow` wrappers preserving today's exact
     messages, so `platform/sqlite.ts`, `platform/duckdb.ts` and `tests/core_test.ts` compile unchanged.
  4. Unit tests: every branch of `expression` (columns vs json × sqlite vs duckdb × each node kind), every
     `CompileError` reason, both aggregate forms, order with and without direction.
- **Acceptance criteria:**
  - `deno task test` passes: 20 existing tests unchanged (including the four compiler tests that assert exact
    SQL regexes) plus new unit tests.
  - `deno task explain` regenerates `GENERATED_SQL.txt` **byte-identically** to the committed file — this is
    the primary safety net for this item; run `git diff --exit-code GENERATED_SQL.txt`.
  - `grep -n "throw" core/compile/ core/plan.ts` returns nothing.
  - `deno task check` passes.

#### Item 10 — SDK Effect core: `Execute`, `Documents`, `defineFunctions`, app handlers, client

- **Scoping:** CONCURRENT with 09. SYNC prerequisite for batch 4.
- **Model:** **Opus / astra.** This changes the public, type-inferred app API: `ScalarKey`/`NumericKey`
  inference, `Definition` variance, and the `Names`/`ArgsOf`/`ResultOf` client inference all have to keep
  working, and `tests/types.ts` must keep failing in exactly the same six places. Also the item that decides
  how app authors experience Effect.
- **Owns:** `sdk/db.ts`, `sdk/functions.ts`, `sdk/client.ts`, `app/functions.ts`, `app/rules.ts` (new),
  `tests/types.ts`, and the bridging edits in `platform/sqlite.ts` / `platform/duckdb.ts`.
- **Reads:** `core/errors.ts`, `core/schema.ts`, `platform/services.ts`.
- **Work:**
  1. `Execute = (op: Operation) => Effect<unknown, DbError>`. `Find` methods and `Documents` methods return
     `Effect`. Keep the IR types exactly as they are — the IR is the stable contract that both engines and
     `scripts/explain.ts` depend on.
  2. `defineFunctions`: `handler` returns `Effect<R, E, never>`; `invoke(db, input)` returns
     `Effect<R, E | SchemaError>` and does argument parsing with `core/schema.parseObject` in the error
     channel instead of throwing.
  3. Extract tic-tac-toe rules from `app/functions.ts` into **pure** `app/rules.ts`:
     `applyMove(game, playerId, cell): Either<MoveError, { game: GameBody; move: MoveBody }>` and
     `winner(board): Mark | null`. Handlers become thin `Effect.gen` bodies. This is the testability payoff
     for app code and the target of a property test in item 12.
  4. `sdk/client.ts`: `call` returns `Effect<R, ClientError>`; adds `X-Trace-Id` (caller-supplied or
     generated) so k6 runs and server events can be joined. Keep the 30 s `AbortSignal.timeout` and map
     non-2xx bodies to tagged errors by status.
  5. Bridge, so the tree still compiles and the 20 tests still pass in this commit: `platform/sqlite.ts` and
     `platform/duckdb.ts` wrap handler invocation in `Effect.runPromise(...)` and adapt their existing
     promise-returning `Execute` with `Effect.promise`. These bridges are explicitly temporary and are deleted
     by items 15 and 16.
- **Acceptance criteria:**
  - `deno task test` passes: all 20 existing tests, unmodified assertions.
  - `deno task check` passes, and `tests/types.ts` still has exactly six `@ts-expect-error` lines, each still
    a genuine error (remove one `@ts-expect-error` locally and confirm `check` fails).
  - New unit tests for `app/rules.ts`: win on each of the 8 lines, draw, occupied cell, out-of-range cell,
    wrong player, finished game.
  - `deno task explain` still produces an identical `GENERATED_SQL.txt` (the IR is unchanged).
  - `grep -n "await\b" app/functions.ts` returns nothing — handlers are Effects, not async functions.

---

### Batch 4 — logger, property suites, k6 scenarios (4 concurrent)

#### Item 11 — Logger service and rotating JSONL sink

- **Scoping:** CONCURRENT with 12, 13, 14.
- **Model:** **Opus / astra.** A real logging service with a bounded queue, a batching background fiber,
  scope-ordered flush-on-shutdown, and the guarantee that logging never blocks or fails a mutation. Getting
  the backpressure policy and the finalizer ordering wrong loses events or hangs shutdown.
- **Owns:** `platform/logging/logger.ts`, `platform/logging/file_sink.ts`, `tests/unit/logger_test.ts`.
- **Reads:** `core/events.ts`, `core/logging/rotation.ts`, `platform/config.ts`, `platform/services.ts`,
  `core/errors.ts`.
- **Work:**
  1. A real `Logger` in the Effect sense: `Logger.replace(Logger.defaultLogger, jsonlLogger)` so that
     `Effect.log*`, `Effect.annotateLogs`, `Effect.withLogSpan` and fiber/span metadata all flow into the same
     JSONL records. `console.log`/`console.error` are not used anywhere in `platform/` after item 26.
  2. `EventSinkLive`: `Queue.dropping(config.eventQueueCapacity)` plus a `forkScoped` consumer that batches by
     `max(256 records, config.eventFlushMs)`, appends to the current file via a single `Deno.FsFile` opened in
     append mode, rolls files using `core/logging/rotation.shouldRoll`, and flushes + closes in an
     `Effect.addFinalizer`. Dropped-event counts are themselves logged as a `queue` event so loss is visible
     rather than silent.
  3. `emit` is `Effect<void, never>`: a logging failure must degrade (increment a dropped counter, write to
     stderr once per minute) and must never surface in a handler's error channel.
  4. Separate files per role (`server`, `reader`), because the two processes write concurrently; the pid and
     `seq` in the filename keep them from interleaving into one file.
- **Acceptance criteria:**
  - `deno task test` passes. Unit tests use a temp `LOG_DIR` and assert: records land as one JSON object per
    line; a date boundary (driven by `TestClock`) opens a new dated directory; exceeding `maxFileBytes` opens
    `-002`; the scope finalizer flushes buffered records before the test's temp dir is removed.
  - A test fills the queue beyond capacity and asserts that (a) `emit` never fails, (b) a `queue` event
    records the drop count, and (c) the number of lines on disk plus the drop count equals the number emitted.
  - `grep -rn "console\." platform/logging/` returns only the documented stderr degradation path.
  - Appending 10 000 events in a test completes in under 2 s on this droplet (sanity, not a benchmark).

#### Item 12 — Property tests, wave 1: schema, rules, framing, rotation

- **Scoping:** CONCURRENT with 11, 13, 14.
- **Model:** **Sonnet / sol.** The properties are enumerated below and the modules under test are already pure
  with fixed signatures; writing arbitraries and running them is bounded work.
- **Owns:** `tests/property/arbitraries.ts`, `tests/property/schema_test.ts`, `tests/property/rules_test.ts`,
  `tests/property/framing_test.ts`, `tests/property/rotation_test.ts`.
- **Reads:** `core/schema.ts`, `app/rules.ts`, `core/ipc/framing.ts`, `core/logging/rotation.ts`.
- **Work:**
  1. `arbitraries.ts`: generators for `Node` (all kinds × nullable × array nesting depth ≤ 2), values matching
     a given node, values _not_ matching a given node, identifiers (valid and invalid), `Fields`, `Table`, and
     `Schema`. Fix the seed via `fc.configureGlobal({ seed: ... })` and record it, so a failure is
     reproducible; `numRuns: 200` by default.
  2. Schema properties:
     - a generated value matching a node always validates;
     - a generated non-matching value always fails with a `SchemaError` whose `path` points at the offending
       position;
     - `parseObject` is total: it returns `Left` or `Right`, never throws, for arbitrary JSON input;
     - partial mode accepts exactly the subsets of a valid object;
     - validation is idempotent: `validate(node, validate(node, x))` equals the first result.
  3. Rules properties (on `app/rules.ts`): from any reachable board, a legal move increases the filled-cell
     count by exactly one; a won game stays won; `winner` is non-null iff some line is uniform and non-null; a
     full board is `draw` or `won`, never `playing`; replaying the same move sequence is deterministic.
  4. Framing properties: for arbitrary message lists and arbitrary chunk splits of their concatenation, the
     decoder yields exactly the original non-empty messages (chunk-split invariance); `push` never throws;
     total bytes exceeding the cap always yields `MessageTooLarge`.
  5. Rotation properties: the five invariants listed in item 06, over arbitrary file sets and arbitrary `now`.
- **Acceptance criteria:**
  - `deno task test:prop` passes with `numRuns >= 200` per property.
  - Each property file prints the configured seed in its test name or output.
  - Deliberately breaking one invariant (e.g. letting rotation delete today's dir) makes the suite fail and
    print a minimal counterexample — demonstrate this in the commit message.
  - `deno task test:prop` completes in under 60 s on this droplet.

#### Item 13 — Property tests, wave 2: compiler and plan invariants

- **Scoping:** CONCURRENT with 11, 12, 14.
- **Model:** **Opus / astra.** The valuable properties here are security and semantics properties over
  generated schemas and plans (no unbound literal reaches the SQL string; placeholder count equals bind count;
  dialects agree on shape). Choosing properties that actually catch injection and dialect-drift bugs, rather
  than restating the implementation, is the hard part.
- **Owns:** `tests/property/compiler_test.ts`, `tests/property/plan_test.ts`, `tests/property/decode_test.ts`,
  `tests/property/sqlite_oracle_test.ts`.
- **Reads:** `core/compile/*`, `core/plan.ts`, `tests/property/arbitraries.ts` (read-only; item 12 owns it —
  if a new generator is needed, add it in a `compiler_arbitraries.ts` this item owns).
- **Work:**
  1. Plan generators over generated schemas: valid plans (fields that exist, operators the field kind
     supports) and invalid plans (array predicates, unknown fields, bad limits, `gt` on strings).
  2. Compiler properties:
     - **Bind parity:** for every successfully compiled select, the number of `?` (sqlite) or `$pN` (duckdb)
       placeholders equals `values.length`, and the `$pN` indices are exactly `1..n` in order.
     - **No value leakage:** for a filter value containing SQL metacharacters (`'`, `"`, `;`, `--`, `/*`), the
       compiled `text` never contains the value — it is always bound. Generate adversarial strings explicitly.
     - **Identifier safety:** every identifier appearing in `text` is double-quoted and matches
       `/^[A-Za-z_][A-Za-z0-9_]*$/`; any schema containing an invalid identifier fails to compile rather than
       emitting it.
     - **Totality:** compile never throws for arbitrary (schema, plan) pairs; it returns `Left(CompileError)`.
     - **Dialect agreement:** sqlite and duckdb compilations of the same plan produce the same `values` array
       and the same filter/order/limit structure (compare a normalised token skeleton, not the raw text).
     - **Storage agreement:** `columns` and `json` layouts of the same schema produce the same `values`.
     - **Limit semantics:** `count` plans never emit `LIMIT`; `countBy` plans always do; `order` is dropped
       for aggregates.
  3. Decode properties: `decode(encode(doc))` round-trips for every generated document and both storage
     layouts, including `BIGINT` inputs (`1n`) for metadata columns, boolean `0/1/true` forms, and `null` in
     nullable arrays; integers outside the safe range always fail with `SafeIntegerError` rather than silently
     truncating.
  4. **SQLite oracle property** (the one that catches real bugs): for a generated schema and document set,
     create a temp database with `compileDDL`, insert via `compileInsert`, then assert that executing the
     compiled select returns the same documents as applying the plan's filters, order and limit to the
     in-memory array with a pure JS reference implementation. Runs against real `node:sqlite`, which is
     already a verified dependency; `numRuns: 50` to keep it fast.
- **Acceptance criteria:**
  - `deno task test:prop` passes, including the oracle property.
  - The no-value-leakage property explicitly includes the exact string already used in
    `tests/core_test.ts:115` (`O'Reilly'); DROP TABLE games; --`).
  - Mutating `core/compile/select.ts` to interpolate a filter value instead of binding it makes the suite fail
    with a counterexample.
  - `deno task test:prop` total runtime stays under 90 s.

#### Item 14 — k6 load scenarios and output capture

- **Scoping:** CONCURRENT with 11, 12, 13.
- **Model:** **Sonnet / sol.** Three more k6 scripts following the pattern established in item 03, with
  thresholds and output paths specified here.
- **Owns:** `perf/mutation_load.js`, `perf/query_load.js`, `perf/mixed.js`, `perf/lib/scenarios.js`, and the
  scenario section of `perf/README.md`.
- **Reads:** `perf/lib/api.js`, `sdk/client.ts` (trace-id header name).
- **Work:**
  1. `mutation_load.js`: `ramping-vus` 1→20 over 1 min, hold 1 min. Each VU creates its own game and makes
     moves with `expectedVersion`. **Conflicts are expected** — a 409 (post item 22) or a stale-version error
     is a _success_ for the scenario and must be counted in a custom `conflicts` counter, not in
     `http_req_failed`. Thresholds: `checks` rate > 0.99,
     `http_req_duration{expected_response:true} p(95) < 400`.
  2. `query_load.js`: `constant-arrival-rate` 50 rps for 2 min over a pre-seeded dataset (`getGame`,
     `listGames`, `moveHistory`, `winsByMark` weighted 4:2:2:1). Thresholds: `http_req_failed < 0.01`,
     `p(95) < 500`. This is the scenario that exercises the serialized single reader process, which is the
     known capacity limit documented in `README.md`.
  3. `mixed.js`: 80 % read / 20 % write for 3 min; the default scenario for regression comparison.
  4. `perf/lib/scenarios.js`: shared seeding helper, run id, `--out json=logs/k6/<run-id>/raw.json` wiring,
     and `handleSummary` writing `summary.json`.
  5. Document in `perf/README.md` that these measure this prototype's documented design (one writer
     connection, one serial reader), and that numbers are for regression detection, not capacity claims.
- **Acceptance criteria:**
  - Against a freshly seeded scratch `DATA_DIR`: `deno task perf:load`, `deno task perf:query` and
    `deno task perf:mixed` each exit 0 with thresholds met.
  - `logs/k6/<run-id>/{raw.json,summary.json}` exist for each run and `raw.json` is newline-delimited JSON
    that `duckdb`'s `read_json_auto` can read (verified in item 25).
  - Every request carries a non-empty `X-Trace-Id`, verified by grepping the server's JSONL log for one of the
    ids emitted by the run.
  - A deliberately low threshold (`p(95) < 1`) makes the task exit non-zero — proof the thresholds are wired.

---

### Batch 5 — adapters become Effect services (4 concurrent)

Four disjoint files, four independent ports. Each deletes the temporary bridge that item 10 added to its file
and emits events through `EventSink`, replacing `platform/registry.ts:trace`.

#### Item 15 — `Storage` service (SQLite writer)

- **Scoping:** CONCURRENT with 16, 17, 18.
- **Model:** **Opus / astra.** Transaction boundaries, `BEGIN IMMEDIATE`/`COMMIT`/`ROLLBACK` ordering, the
  mutation-context expiry flag, the exclusive queue, and the `VACUUM INTO` interaction with that queue are the
  correctness heart of the system. The existing tests for competing moves and rollback are precisely the
  behaviour that must not drift.
- **Owns:** `platform/sqlite.ts`, `tests/unit/storage_test.ts`.
- **Reads:** `core/compile/*`, `core/schema.ts`, `core/errors.ts`, `core/events.ts`, `platform/config.ts`,
  `platform/services.ts`, `sdk/db.ts`, `sdk/functions.ts`.
- **Work:**
  1. `StorageLive`: `Layer.scoped` over `Effect.acquireRelease` — acquire opens `DatabaseSync`, sets the three
     pragmas, runs the schema-fingerprint check and DDL in one `BEGIN IMMEDIATE` transaction; release closes
     the handle after the in-flight queue drains. The current constructor-throws-and-closes path becomes a
     scoped failure.
  2. Replace the `tail`/`pending` promise chain with `Effect.Semaphore(1)` plus an explicit depth check that
     fails `QueueFullError({ queue: "writer", cap: 1000 })`. Keep the cap at 1000 to preserve observable
     behaviour.
  3. `mutation`: `Effect.acquireUseRelease` around the SQL transaction so a _defect_ or an _interruption_, not
     just a failure, still rolls back — a strict improvement over the current try/catch, which does not handle
     cancellation at all. Keep the `JSON.parse(JSON.stringify(result))` serializability check before `COMMIT`,
     now as a typed `NonSerializableResultError`.
  4. `Clock` and `IdGen` come from services instead of constructor parameters, so `tests/core_test.ts`'s fixed
     clock/id injection becomes a test layer.
  5. Emit `sql`, `tx` and `queue` events: per statement (kind, table, duration, row count, bind count, and SQL
     text subject to the redaction policy), per transaction (commit/rollback, duration, operation count), per
     queue-full rejection.
  6. Delete the item-10 `Effect.runPromise` bridge in this file.
- **Acceptance criteria:**
  - `deno task test` passes with **all 20 original tests semantically intact**, including
    `two competing moves cannot both commit`, `rollback restores board, version, and move history`,
    `standalone backup includes uncheckpointed WAL commits`, and
    `schema drift fails rather than silently migrating`.
  - A new test interrupts a mutation fiber mid-handler and asserts the transaction rolled back and the
    connection is still usable — behaviour the current implementation does not have.
  - A new test asserts `QueueFullError` at depth > 1000 rather than an untyped rejection.
  - `grep -n "Promise" platform/sqlite.ts` returns nothing outside the `node:sqlite` type surface.
  - `deno task check` passes.

#### Item 16 — `Reader` service (DuckDB) and reader process entry

- **Scoping:** CONCURRENT with 15, 17, 18.
- **Model:** **Opus / astra.** Scoped native resources (instance + connection must close in the right order on
  every path), per-query transaction lifecycle, the query-context expiry flag, and the hard rule that this
  process must never import `node:sqlite`.
- **Owns:** `platform/duckdb.ts`, `platform/reader.ts`, `tests/unit/reader_test.ts`.
- **Reads:** `core/compile/*`, `core/errors.ts`, `core/events.ts`, `platform/config.ts`,
  `platform/services.ts`, `platform/logging/*`, `core/ipc/framing.ts`.
- **Work:**
  1. `ReaderLive`: `Layer.scoped` acquiring the `DuckDBInstance` + `DuckDBConnection`, running
     `INSTALL sqlite` / `LOAD sqlite` / `ATTACH ... READ_ONLY`, and releasing connection-then-instance in a
     finalizer (replacing today's manual `catch { closeSync(); closeSync(); throw }`).
  2. `query`: `Effect.acquireUseRelease` around `BEGIN TRANSACTION` / `COMMIT` / `ROLLBACK`; write operations
     in a query context fail `QueryWriteAttemptError`; context expiry becomes a typed error. Emit `sql` and
     `tx` events with the duckdb dialect.
  3. `platform/reader.ts` becomes an Effect program: a scoped stdin consumer using `core/ipc/framing`, a
     per-app `Reader` cache built with `Effect.cachedFunction`, serialized query execution (one at a time, as
     today), and JSONL replies on stdout. Its logger writes to `logs/reader/...`, keeping **stdout reserved
     for the RPC protocol** — the reason `platform/registry.ts:trace` uses stderr today.
  4. Delete the item-10 bridge in `platform/duckdb.ts`.
- **Acceptance criteria:**
  - `deno task check` passes.
  - `grep -n "node:sqlite" platform/duckdb.ts platform/reader.ts` returns nothing (the architectural
    invariant; also asserted by the boundary test in item 26).
  - `deno task integration` passes, including the read-after-write assertion at `tests/integration.ts:47` on
    the real extension. If DuckDB cannot be downloaded in the executing environment, say so explicitly in the
    commit message instead of claiming a pass.
  - Unit test: a failed `ATTACH` (nonexistent file) fails the layer with `QueryError` and leaks no handle
    (assert via a finalizer-ordering probe).
  - Reader stdout contains only RPC JSON lines during a run; all log records land under `logs/reader/`.

#### Item 17 — `ReaderClient` service (IPC, parent side)

- **Scoping:** CONCURRENT with 15, 16, 18.
- **Model:** **Opus / astra.** A child process lifecycle plus a correlation map is a classic source of leaked
  fibers, lost replies and hangs on shutdown. The current implementation mutates a `pending` map, chains
  writes through `this.writes`, and swallows failures into `fail()`.
- **Owns:** `platform/ipc.ts`, `tests/unit/ipc_test.ts`.
- **Reads:** `core/ipc/framing.ts`, `core/errors.ts`, `core/events.ts`, `platform/services.ts`,
  `platform/config.ts`.
- **Work:**
  1. `ReaderClientLive`: `Layer.scoped` spawning the child with `Effect.acquireRelease`; the finalizer closes
     stdin, awaits exit with a timeout, then kills — a graceful shutdown path the current `close()` only
     partially has.
  2. Replace the pending map with `Ref<HashMap<number, Deferred<unknown, IpcError>>>` and the write chain with
     a `Queue` drained by one `forkScoped` writer fiber. A reply to an unknown id is logged as an anomaly
     event instead of being silently dropped.
  3. On child exit or stream failure, fail every outstanding `Deferred` with `ReaderExitedError({ code })`,
     and fail subsequent calls immediately — preserving today's semantics with types.
  4. Keep the 1000-call cap as `QueueFullError({ queue: "reader" })`. Add a per-call timeout from `Config`
     (new behaviour: today a lost reply hangs forever) and make interruption remove the pending entry.
  5. Emit `queue` events for depth high-water marks and rejections.
- **Acceptance criteria:**
  - `deno task check` passes; `deno task test` passes.
  - Unit tests against a **stub child process** (a tiny Deno script that echoes, delays, exits early, or emits
    garbage): correlated replies resolve the right callers; early child exit fails all pending calls with
    `ReaderExitedError`; a malformed line fails the connection and is logged; an interrupted call removes its
    pending entry; the scope finalizer terminates the child (assert no stray process via `child.status`).
  - A test asserts the per-call timeout fires and does not leave a leaked `Deferred`.
  - `deno task integration` passes (real reader process).

#### Item 18 — `ObjectStore` service (local and S3 layers)

- **Scoping:** CONCURRENT with 15, 16, 17.
- **Model:** **Sonnet / sol.** Two straightforward adapters behind an interface fixed in item 02; the key path
  validation and the atomic temp-file-rename already exist and just move.
- **Owns:** `platform/object_store.ts`, `tests/unit/object_store_test.ts`.
- **Reads:** `core/errors.ts`, `core/events.ts`, `platform/config.ts`, `platform/services.ts`.
- **Work:**
  1. `ObjectStoreLocal` (`Layer.effect`) and `ObjectStoreS3` (`Layer.effect`, dynamic `aws4fetch` import
     inside the layer so the dependency stays lazy), with `ObjectStoreLive = Layer.unwrapEffect` selecting on
     `Config.objectStore.s3BaseUrl`. This replaces the env-sniffing `objectStore()` factory.
  2. Keep the local key validation (`/^[A-Za-z0-9_./-]+$/`, no `..`, no leading `/`) as `InvalidKeyError`, and
     keep the temp-file + `rename` publish.
  3. Map HTTP failures to `ObjectStoreError({ operation, key, status, body })`; keep the 60 s timeouts; add
     `Effect.retry` with an exponential schedule (3 attempts) for S3 `put`/`get`, which today relies on
     `aws4fetch`'s own `retries: 3`.
  4. Emit `backup`-channel events for each object operation (key, bytes, duration, outcome).
  5. Keep `sha256` where `scripts/restore.ts` and `tests/integration.ts` import it from, or update both call
     sites in this commit.
- **Acceptance criteria:**
  - `deno task test` passes; unit tests cover local round-trip, each invalid-key form, atomicity (no partial
    file visible at the final path on a failed write), and S3 error mapping against a stub `fetch`.
  - `deno task check` passes including `scripts/restore.ts` and `tests/integration.ts`.
  - `deno task integration` passes (local object-store stand-in path).
  - `grep -n "Deno.env" platform/object_store.ts` returns nothing.

---

### Batch 6 — backups, test layers, DuckDB event log (3 concurrent)

#### Item 19 — `Backups` service

- **Scoping:** CONCURRENT with 20, 21.
- **Model:** **Opus / astra.** The ordering guarantee is the point of this module: snapshot → `quick_check` →
  hash → immutable object PUT → immutable manifest PUT → _only then_ `latest.json`. The temp-directory
  finalizer and the per-app "already running" guard have to survive interruption.
- **Owns:** `platform/backup.ts`, `tests/unit/backup_test.ts`.
- **Reads:** `platform/services.ts`, `platform/config.ts`, `core/errors.ts`, `core/events.ts`.
- **Work:**
  1. `BackupsLive` over `Storage` + `ObjectStore` + `Clock` + `IdGen`. The per-app guard becomes a
     `Ref<HashSet<string>>` with `Effect.acquireRelease`, so interruption releases it (today a rejected
     promise could leave it set if the finally path is skipped — make the new version provably safe with a
     test).
  2. Temp directory via `Effect.acquireRelease(makeTempDir, removeRecursive)`; `integrityCheck` becomes an
     Effect returning `SnapshotIntegrityError`.
  3. Emit one `backup` event per phase (`snapshot`, `verify`, `hash`, `put-object`, `put-manifest`,
     `publish-latest`) with durations, so a stalled backup is visible in the analysis logs. This is the
     replacement for the `console.error(JSON.stringify({ backup: ... }))` in `platform/server.ts:78`.
  4. Preserve the documented invariant that a failed upload never advances `latest.json`, and the note that
     snapshot creation serializes with mutations while upload does not.
- **Acceptance criteria:**
  - `deno task test` passes. Unit tests over `StorageTest` + `ObjectStoreTest` (item 20 provides them;
    coordinate by building against the item-02 interfaces and landing after 20 if needed — otherwise use a
    local inline stub, which this item owns).
  - A test injects a `put` failure on the snapshot object and asserts `latest.json` is unchanged.
  - A test interrupts `take` mid-upload and asserts the per-app guard is released and the temp directory is
    removed.
  - A test asserts two concurrent `take` calls for the same app produce one `BackupAlreadyRunning`.
  - `deno task integration` passes (backup + restore round-trip with real files).

#### Item 20 — Test layers and Effect-ified regression suite

- **Scoping:** CONCURRENT with 19, 21.
- **Model:** **Opus / astra.** `StorageTest` is a second implementation of the document-store semantics
  (versioning, metadata, filter/order/limit, transactional rollback) and must agree with the real SQLite
  implementation or every test built on it is a lie. This is the single highest-leverage item for testability
  and for the future DST work.
- **Owns:** `tests/layers/storage_test_layer.ts`, `tests/layers/object_store_test_layer.ts`,
  `tests/layers/event_sink_test_layer.ts`, `tests/layers/reader_test_layer.ts`,
  `tests/layers/id_gen_test_layer.ts`, `tests/layers/index.ts`, `tests/core_test.ts`, `tests/helpers/run.ts`.
- **Reads:** everything in `core/` and `platform/services.ts`.
- **Work:**
  1. `StorageTest`: a pure in-memory document store built on `Ref<HashMap<table, Document[]>>`, implementing
     the `Storage` interface including `_id`/`_creationTime`/`_version` metadata, patch-merge semantics, the
     plan's filter/order/limit/aggregate evaluation (reusing the reference evaluator from item 13 if
     practical), and transactional rollback by snapshotting the `Ref` and restoring on failure or
     interruption. No SQLite, no filesystem.
  2. `ReaderTest` delegates reads to the same in-memory store, so the whole HTTP surface can be exercised in
     one process with no child process and no DuckDB.
  3. `ObjectStoreTest` (`Ref<HashMap<string, Uint8Array>>` with optional injected failures), `EventSinkTest`
     (`Ref<EventRecord[]>` with query helpers like `eventsOfKind("sql")`), `IdGenTest` (`id-1`, `id-2`, …
     matching today's `tests/core_test.ts:37` fixture), and `TestClock` wiring.
  4. `tests/helpers/run.ts`: `runTest(name, effect)` that provides a default test layer stack and runs via
     `Effect.runPromise` under `node:test`, keeping the existing runner.
  5. Port `tests/core_test.ts` to the Effect runner. Keep **all 20 tests** and their assertions; the six
     storage-dependent ones keep running against the real `StorageLive` + real `node:sqlite` (they are the
     integration value), while argument-validation and compiler tests move to the fast test layers.
  6. Add a **conformance suite** run twice — once against `StorageLive`, once against `StorageTest` —
     asserting identical observable results for a scripted operation sequence. This is what keeps the double
     honest.
- **Acceptance criteria:**
  - `deno task test` passes: 20 original tests plus the conformance suite.
  - The conformance suite runs the same ~30-step script against both layers and asserts deep equality of every
    result and of the final store contents.
  - `deno task test` with only the test layers (`deno test -A tests/unit/ tests/layers/`) runs in under 5 s
    and spawns no subprocess and creates no files (assert with a temp-dir watcher or by running with
    `--deny-write`).
  - A deliberate divergence in `StorageTest` (e.g. not incrementing `_version`) makes the conformance suite
    fail.

#### Item 21 — DuckDB append-only event log and idempotent ingest

- **Scoping:** CONCURRENT with 19, 20.
- **Model:** **Opus / astra.** Idempotent ingestion with byte-offset state, append-only tables, and the
  process-placement constraint (DuckDB only in the reader process) is a design problem, not a transcription
  problem; a wrong offset model silently duplicates or loses events.
- **Owns:** `core/eventlog/ddl.ts`, `core/eventlog/offsets.ts`, `platform/eventlog/tables.ts`,
  `platform/eventlog/ingest.ts`, `tests/unit/eventlog_test.ts`, `tests/property/offsets_test.ts`.
- **Reads:** `core/events.ts`, `core/errors.ts`, `platform/config.ts`, `platform/services.ts`.
- **Work:**
  1. `core/eventlog/ddl.ts`: the DDL for the append-only tables in §4.1, as pure strings, plus the
     `_ingest_state` table. Tables are `CREATE TABLE IF NOT EXISTS` in a persistent DuckDB file
     (`logs/duckdb/events.duckdb`), separate from the per-app `:memory:` read instances.
  2. `core/eventlog/offsets.ts`: pure ingest planning — given the on-disk JSONL inventory and the
     `_ingest_state` rows, decide which files to read and from which byte offset, detect truncated or
     rewritten files (compare size and a prefix hash), and never re-ingest a complete prefix. Property-tested:
     ingest is idempotent (running twice inserts nothing the second time) and complete (every line is ingested
     exactly once across arbitrary interleavings of append + ingest).
  3. `platform/eventlog/ingest.ts`: `EventLogIngestLive` reading JSONL with DuckDB's `read_json_auto` over an
     explicit file list and offset-trimmed temp views, inserting into the typed tables inside one DuckDB
     transaction per batch, and updating `_ingest_state` in the same transaction. Only `INSERT` is ever issued
     against event tables; `_ingest_state` is the one mutable table. Lives in the reader process, which
     already owns DuckDB.
  4. Record in ADR 0004 why the writer process never opens DuckDB.
- **Acceptance criteria:**
  - `deno task test` and `deno task test:prop` pass.
  - Unit test: ingest a fixture directory, assert row counts per table; run ingest again, assert **zero** new
    rows and unchanged `_ingest_state`.
  - Unit test: append more lines to the same file, re-ingest, assert only the new lines appear.
  - Unit test: a malformed line is quarantined to `logs/<role>/<date>/rejected.jsonl` with a `ingest` event,
    and ingestion of the rest of the batch still succeeds.
  - Property test asserts exactly-once ingestion across arbitrary append/ingest interleavings.
  - `grep -n "UPDATE\|DELETE" platform/eventlog/ingest.ts` matches only `_ingest_state`.

---

### Batch 7 — server composition

#### Item 22 — Server on layers: HTTP, auth, scheduling, shutdown, admin endpoints

- **Scoping:** **SYNC. Single item in its batch.** It is the composition root and touches the one file every
  previous batch feeds into.
- **Model:** **Opus / astra.** This is where the port either pays off or does not: layer composition and
  memoization, graceful shutdown ordering (HTTP drain → backup fiber → reader child → DB handles → log flush),
  signal handling, request-body limits, the error-to-status mapping, and the two behaviour changes (409, 503).
  Getting shutdown ordering wrong loses events and corrupts nothing but looks fine in tests.
- **Owns:** `platform/server.ts`, `platform/layers.ts`, `platform/registry.ts`, `platform/http/router.ts`,
  `platform/http/auth.ts`, `platform/http/handlers.ts`, `tests/unit/http_test.ts`.
- **Reads:** everything.
- **Work:**
  1. `platform/registry.ts` → `AppRegistryLive` over `Config` (no `Deno.env`), with `trace()` deleted — the
     event log replaces it.
  2. `platform/http/router.ts`: the existing routes, expressed as an Effect handler — `GET /health`,
     `POST /apps/:appId/:kind/:name`, `POST /admin/backup/:appId`, plus new `POST /admin/logs/ingest` and
     `POST /admin/logs/analysis/:name` (named queries only; no caller-supplied SQL, consistent with the repo's
     "never raw SQL from callers" posture). Keep the regex route shapes and the 64 KiB body cap (now
     `RequestTooLarge` → 413).
  3. `platform/http/auth.ts`: per-app bearer token and admin token checks as an Effect returning `AuthError`;
     keep the documented dev-token behaviour and the item-08 warning event.
  4. Error mapping via `toHttpStatus` from item 02 — replacing the blanket 400. Response bodies get
     `{ error: { tag, message } }` while keeping the `error` key the existing client reads
     (`sdk/client.ts:14`) so the demo and integration test keep working.
  5. Per-request tracing: accept or generate `X-Trace-Id`, `Effect.annotateLogs` it onto every event in the
     request's fiber, echo it in the response header, and emit one `http` event per request with method, path,
     app, handler, status, duration, bytes in/out.
  6. Backup timer: `Effect.schedule(Schedule.spaced(interval))` in a `forkScoped` fiber replacing
     `setInterval` + the `backupTick` guard; keep the "one sweep at a time" property, now via the fiber's own
     sequencing.
  7. `platform/layers.ts`:
     `PlatformLive = Layer.mergeAll(ConfigLive, LoggerLive, EventSinkLive,
     AppRegistryLive, IdGenLive, StorageLive, ReaderClientLive, ObjectStoreLive, BackupsLive)`
     with explicit dependency wiring and `Layer.memoize` where shared. `ReaderLive`/`EventLogIngestLive`
     compose into the reader process's own root.
  8. Shutdown: one `Scope`, signal handlers that interrupt the root fiber, and finalizers ordered so the log
     sink flushes last. Replaces the hand-rolled `stopping` flag and signal-listener removal.
- **Acceptance criteria:**
  - `deno task check` and `deno task test` pass.
  - `tests/unit/http_test.ts` exercises the full router in-process over `StorageTest` + `ReaderTest` +
    `ObjectStoreTest` with **no subprocess and no DuckDB**, covering: health, query, mutation, unknown app →
    404, unknown handler → 404, kind mismatch → 400, missing/wrong token → 401, oversize body → 413, stale
    version → **409**, writer queue full → **503**, validation error → 400. This test is the clearest
    demonstration that the port achieved testability.
  - `deno task integration` passes end-to-end with the real reader process, real DuckDB, backup and restore;
    it must additionally assert the 401 case it already checks (`tests/integration.ts:70`).
  - `deno task demo` and `deno task demo tic-tac-toe-json` both complete against a running server.
  - SIGTERM during an in-flight mutation: the mutation completes or rolls back, the child process exits, and
    the final log file ends with a `lifecycle` shutdown event — assert in a test that spawns the server and
    signals it.
  - `grep -rn "console\.\|Deno\.env" platform/ | grep -v platform/config.ts` returns nothing.

---

### Batch 8 — analysis, retention, end-to-end (3 concurrent)

#### Item 23 — Analysis query catalog and `logs` CLI

- **Scoping:** CONCURRENT with 24, 25.
- **Model:** **Sonnet / sol.** A fixed list of named SQL queries over item 35's v2 DDL and taxonomy
  (`event_kind`/`handler_kind`), plus a small CLI. The queries are specified below.
- **Owns:** `platform/analysis/catalog.ts`, `scripts/logs.ts`, `docs/observability.md`,
  `scripts/duckdb/event-counts-by-type.sql`, `scripts/duckdb/recent-errors.sql`,
  `scripts/duckdb/trace-timeline.sql` (the three starters broken by the v1→v2 `kind` drift — they get fixed
  here so they don't stay ownerless).
- **Reads:** `core/eventlog/ddl.ts` (item 35's v2 DDL and taxonomy: `event_kind`/`handler_kind` — §4.1 is
  updated to match; never the item-21-frozen `kind VARCHAR`), `platform/eventlog/*`, `platform/services.ts`.
- **Work:**
  1. `catalog.ts`: a `Record<name, { sql, params, description }>` of the named queries in §4.3, all
     parameterized, all read-only. `AnalysisLive.run(name, params)` validates the name against the catalog —
     callers never supply SQL. Fix the v1→v2 drift per the corrected punch list in
     `docs/haiku-experiments/findings/sq-12.md`: resolve `kind` in all 7 queries (`event-counts-by-type`,
     `recent-errors`, `trace-timeline`, `error-rate-by-handler`, `handler-throughput`, `trace`, and
     `k6-vs-platform`); cast `k6_points.tags` to `VARCHAR` in the trace-timeline UNION (the `path` cast is
     redundant); `date_bin`→`time_bucket` (bucket semantics unverified); add a non-zero-row check or seeded
     data guaranteeing bucket overlap, since item 23's acceptance requires queries to return rows.
  2. `scripts/logs.ts`: `ingest`, `query <name> [--param k=v] [--format table|json|csv]`, `list`, `sweep`.
     Runs as its own Effect program with the reader-side layer stack.
  3. `docs/observability.md`: the log directory layout, the DuckDB table schemas, each named query with the
     question it answers, the retention policy, and worked examples (including "show me everything that
     happened for trace id X").
- **Acceptance criteria:**
  - After `deno task perf:mixed` and `deno task logs:ingest`: `deno task logs:query slowest-sql`,
    `error-rate-by-handler`, `handler-throughput`, `queue-depth`, `plan-shapes`, `backup-history` and
    `trace --param traceId=<id>` each return rows and exit 0.
  - `deno task logs:query not-a-query` exits non-zero with `UnknownAnalysisQuery` and lists valid names.
  - `docs/observability.md` documents every table column and every catalog entry; a test asserts the catalog
    names in the doc match `catalog.ts` exactly (so the doc cannot drift).
  - Every catalog SQL string is a `SELECT`/`WITH` — asserted by a test.

#### Item 24 — Retention sweeper

- **Scoping:** CONCURRENT with 23, 25.
- **Model:** **Sonnet / sol.** The policy decisions are already pure and property-tested in items 06 and 12;
  this item only executes a plan. It is kept separate from item 06 precisely because deleting files is the
  risky half.
- **Owns:** `platform/logging/retention.ts`, `tests/unit/retention_test.ts`.
- **Reads:** `core/logging/rotation.ts`, `platform/config.ts`, `platform/logging/file_sink.ts`.
- **Work:**
  1. Inventory `logs/<role>/` dated directories, feed them to `core/logging/rotation.plan`, then apply: gzip
     eligible files, delete eligible files, remove emptied dated directories.
  2. Run on startup and then on `Schedule.cron`-style daily spacing in a `forkScoped` fiber from the server's
     root; also exposed as `deno task logs:sweep`.
  3. Safety rails: never touch the currently open file (ask the sink), never touch today's directory, refuse
     to operate outside the configured `LOG_DIR` (resolve and prefix-check), and emit a `lifecycle` event
     listing exactly what was gzipped and deleted. A `--dry-run` flag prints the plan without applying it.
  4. DuckDB event-table pruning is **out of scope** for this phase and must be stated in
     `docs/observability.md`: the DuckDB file grows until an explicit future `logs:compact`.
- **Acceptance criteria:**
  - `deno task test` passes; unit tests build a synthetic `logs/` tree spanning 30 days and assert exactly
    which files are gzipped, deleted and kept, and that today's files are untouched.
  - A test with `LOG_DIR` pointing at a temp dir and a crafted path outside it asserts the prefix-check
    refuses and emits an error event.
  - `deno task logs:sweep --dry-run` changes nothing on disk (assert via a before/after inventory).
  - Gzipped files remain ingestible, or the ingest planner skips `.gz` with a documented note — whichever item
    21's implementation supports; state which in the commit message.

#### Item 25 — End-to-end: integration test port plus k6→DuckDB join

- **Scoping:** CONCURRENT with 23, 24.
- **Model:** **Opus / astra.** It is the item that proves the whole phase: a real multi-process run, real
  DuckDB, real backup/restore, plus correlating k6 client-side latency with server-side event durations: k6
  mints per-request trace IDs (`<vu>-<iter>-<runId>`, sent as `X-Trace-Id`) that scope the server side to the
  run's requests, and the `k6-vs-platform` join correlates the two sides by 10-second time bucket (client p95
  > = server p95 per bucket; the difference is transport overhead). Diagnosing a failure here requires
  > reasoning across every layer.
- **Owns:** `tests/integration.ts`, `perf/ingest_k6.ts`, `tests/e2e/observability_test.ts`.
- **Reads:** everything.
- **Work:**
  1. Port `tests/integration.ts` to an Effect program, keeping every existing assertion (read-after-write
     visibility on the real extension, app-file isolation, 401, aggregation, backup checksum, restore
     checksum) and adding: the response `X-Trace-Id` appears in the server's JSONL log; `/admin/logs/ingest`
     populates the DuckDB tables; a named analysis query returns the expected handler names.
  2. `perf/ingest_k6.ts`: load `logs/k6/<run-id>/raw.json` into `k6_points` via DuckDB `read_json_auto`, with
     the run id as a column.
  3. `tests/e2e/observability_test.ts`: run the smoke scenario, ingest both sources, then assert the
     `k6-vs-platform` join returns rows where client-side p95 per 10-second bucket is `>=` the server-side
     handler duration p95 for the same bucket (client latency includes transport, so this is a real invariant,
     not a tautology).
- **Acceptance criteria:**
  - `deno task integration` exits 0 and prints the existing PASS line extended with the new observability
    assertions.
  - `deno task perf:smoke && deno task perf:ingest && deno task logs:query k6-vs-platform` returns rows and
    exits 0.
  - k6 gate (decide the host at run time): `p(95)<160` on the shared VM — host load is recorded per run in
    `baseline.json` (`work-scripts/bash/k6-baseline.sh` warns on stderr when a run's 1-minute load exceeds the
    CPU count); `p(95)<60` on a quiet host. Never gate p99 (on a 1-VU 30 s run it rests on ~2 samples).
    Throughput-at-fixed-latency (open-model k6, read/write split, per the lx-04 review) is the prescribed
    methodology for dedicated performance investigations; it is informational, not a merge gate, until a
    quiet-host CI runner exists.
  - The e2e observability test fails if `EventSinkLive` is swapped for a no-op sink — proving it actually
    asserts on logged data.
  - If DuckDB or k6 cannot be installed in the executing environment, the commit message states exactly which
    assertions were not executed; a skipped run is never reported as a pass.

---

### Batch 9 — cleanup and documentation (2 concurrent)

#### Item 26 — Remove compatibility wrappers and enforce the boundaries

- **Scoping:** CONCURRENT with 27.
- **Model:** **Sonnet / sol.** Deletion guided by the compiler plus a set of grep-based boundary tests whose
  rules are already specified.
- **Owns:** `platform/compiler.ts` (deleted or reduced to re-exports), the `*OrThrow` wrappers in
  `sdk/schema.ts`, `tests/unit/boundaries_test.ts`.
- **Reads:** everything.
- **Work:**
  1. Delete the `*OrThrow` wrappers from items 04 and 09 and update the remaining call sites to the
     `Either`/`Effect` forms. Delete `platform/compiler.ts` if nothing imports it, keeping `core/compile` as
     the only compiler.
  2. `tests/unit/boundaries_test.ts` asserts, by scanning source text:
     - no file in `core/` imports `platform/`, `sdk/`, `node:*` or `npm:*`, or references `Deno.`;
     - no `console.*` outside `platform/logging/` (documented degradation path) and `scripts/`;
     - no `Deno.env` outside `platform/config.ts`;
     - `platform/duckdb.ts` and `platform/reader.ts` never mention `node:sqlite`;
     - `platform/sqlite.ts` never mentions `@duckdb/node-api`;
     - no `throw new Error` in `core/` or `platform/` (errors go through the tagged channel).
  3. Run `deno lint` with no per-file ignores remaining.
- **Acceptance criteria:**
  - `deno task ci` passes.
  - `deno task integration` passes.
  - `tests/unit/boundaries_test.ts` fails if any single rule is violated — demonstrate by temporarily adding a
    `console.log` to `platform/server.ts`.
  - `git grep -c "OrThrow"` returns 0.

#### Item 27 — Documentation and regenerated artifacts

- **Scoping:** CONCURRENT with 26.
- **Model:** **Sonnet / sol.** Prose and regenerated output against a now-settled codebase.
- **Owns:** `README.md`, `SKIM.md`, `GENERATED_SQL.txt`, `docs/testing.md`, `docs/adr/README.md`.
- **Reads:** everything.
- **Work:**
  1. `README.md`: the Effect service/layer architecture, the new task table, the error-to-status table
     including the **409** and **503** behaviour changes, the logging and retention configuration, pointers to
     `docs/observability.md` and `perf/README.md`, and an honest restatement of the verification status — what
     was actually executed on this machine versus what was not (the current README is scrupulous about this
     and must stay that way).
  2. `SKIM.md`: refresh the excerpts, which currently show pre-port code (`Execute` returning `Promise`, the
     `mutation()` try/catch, the `DuckReader.open` static constructor).
  3. Regenerate `GENERATED_SQL.txt` via `deno task explain`.
  4. `docs/testing.md`: the four test tiers (unit on `core/`, property on `core/`, layer-based service tests,
     real-process integration), how to run each, how to reproduce a property failure from its seed, and the
     storage-conformance suite's role.
  5. `docs/adr/README.md`: index of ADRs 0001–0004.
- **Acceptance criteria:**
  - `git diff --exit-code GENERATED_SQL.txt` is clean after `deno task explain`.
  - Every command quoted in `README.md`, `SKIM.md`, `docs/testing.md` and `docs/observability.md` is executed
    once and exits 0 (or is explicitly marked as not executed in this environment, with the reason).
  - No code excerpt in `SKIM.md` contradicts the committed source — verified by re-reading each excerpt
    against its file.
  - `deno task ci` passes.

---

### Batch 10 — demos and user guide (2 concurrent)

Items 28 and 29 run **concurrently** and are **independent of each other**: disjoint `Owns` sets, and neither
reads the other's output (`docs.md` does not link to `demos/`, and `demos/README.md` does not link to
`docs.md`; cross-linking is a follow-up once both have landed). The whole batch runs **after batch 9 has
landed**, because both items describe the settled platform: the wrapper-free API from item 26, and the
`README.md`, `docs/observability.md` and `docs/testing.md` that item 27 makes true.

Neither item edits `deno.json` (§2.2): item 01 wrote it once. Item 28's demos therefore run via documented
`deno run` commands, not new `demo:*` tasks, and item 29's parity test is picked up by the existing
`tests/unit/**/*.ts` glob in `deno task test`.

#### Item 28 — `demos/`: self-contained runnable demos

- **Scoping:** CONCURRENT with 29; the two items are independent of each other. Starts only after batch 9 has
  landed (both 26 and 27).
- **Model:** **Sonnet / sol.** Bounded and mechanical: every interface the demos touch (the Effect client from
  item 10, the HTTP and admin routes from item 22, the analysis catalog from item 23, the restore script) is
  fixed by earlier items, the file list is short, and each demo is checked by running one command.
- **Owns:** `demos/README.md`, and everything under `demos/documents/`, `demos/backup-restore/` and
  `demos/observability/` (each holds a `main.ts` plus any helper files private to that demo). Nothing outside
  `demos/`; in particular not `deno.json`, and no `demos/deno.json`.
- **Reads:** `README.md` (Configuration, Run, Backups and restore, Deliberate limits), `platform/config.ts`
  (`OPTIONS`), `sdk/client.ts`, `sdk/functions.ts`, `app/schema.ts`, `app/functions.ts`, `core/errors.ts`,
  `platform/http/router.ts` (route shapes, admin endpoints, request-body shape for named analysis queries),
  `platform/analysis/catalog.ts`, `scripts/restore.ts`, `scripts/demo.ts` (call order),
  `docs/observability.md`, `deno.json` (the `serve` and `restore` task definitions).
- **Why the demos use the registered apps rather than new schemas:** the deployment registry is static.
  `platform/config.ts` lists exactly `tic-tac-toe` (columns) and `tic-tac-toe-json` (json), and both the
  server and the reader child load their schemas and handlers from `app/` via `platform/registry.ts`. A notes
  or task-tracker demo with its own schema would have to edit files owned by items 08, 16 and 22, so each demo
  instead shows one platform capability end to end over the registered apps. Revisit if a later item adds an
  app-registration seam.
- **Work:**
  1. Each demo is one Effect program at `demos/<name>/main.ts`, run from the repo root with
     `deno run -A demos/<name>/main.ts`, so the root import map and `deno.lock` apply. Do not add
     `demos/deno.json`: a nested config would need its own import map and lockfile, duplicating the §1.5 pins.
  2. Self-containment: a demo imports only from `sdk/`, `app/` (types and schema), `core/errors.ts` and
     `effect`. It never imports from `platform/`, and never from another demo directory. Each demo carries its
     own copy of the small server-lifecycle helper (about 30 lines). That duplication is deliberate, so any
     single demo directory can be copied out as a starting point.
  3. Server lifecycle, in every demo: create one temp root (`Deno.makeTempDir`); pick a free port; spawn the
     command the `serve` task runs (`deno run -A platform/server.ts`) with `PORT`, `DATA_DIR`, `LOG_DIR` and
     `LOCAL_OBJECT_DIR` pointing into the temp root (only option names from the item-08 table); poll
     `GET /health` with a bounded timeout; and release it all in `Effect.acquireRelease`: SIGTERM, await exit,
     remove the temp root. A demo leaves no `./data`, `./logs` or `./object-store` in the repo and no stray
     server or reader process, even when it fails.
  4. `demos/documents/main.ts` covers documents and queries on both storage layouts. Using the typed Effect
     `createClient`, run the same script against `tic-tac-toe` and `tic-tac-toe-json`: `createGame`,
     `getGame`, a sequence of `makeMove`s, `listGames` filtered by status, `moveHistory`, and `winsByMark`
     (`countBy`). Then make a stale `expectedVersion` move and handle the 409 as a typed `ConflictError` with
     `Effect.catchTag`; make an argument-validation failure and handle the 400 the same way. Assert that both
     layouts return the same document shapes and aggregate results (ignoring `_id` and `_creationTime`).
  5. `demos/backup-restore/main.ts` covers backups. Create and play a game, then take a backup with
     `POST /admin/backup/tic-tac-toe` (admin token). Read `apps/tic-tac-toe/latest.json` from
     `LOCAL_OBJECT_DIR` and print its key and SHA-256. Make one more move after the backup. Stop the server,
     run the restore script (`deno run -A scripts/restore.ts tic-tac-toe <tmp>/restored/tic-tac-toe.sqlite`)
     with the same `LOCAL_OBJECT_DIR`, then start a second server with `DATA_DIR=<tmp>/restored`. Assert that
     the restored game has the backed-up `_version`, not the post-backup one. That assertion is the README's
     "asynchronous backup, not high availability" point, made visible.
  6. `demos/observability/main.ts` covers logs and the DuckDB analysis queries. Run a small workload with a
     caller-supplied `X-Trace-Id` on one request, and include one deliberate 409 and one unknown-handler 404.
     Call `POST /admin/logs/ingest`, retrying with a bounded timeout of at most 10 s until the trace id is
     queryable, since the event sink flushes asynchronously. Then run the named queries `handler-throughput`,
     `error-rate-by-handler` (it must show the 409 and the 404), `slowest-sql` and `trace` (with the trace id
     as its parameter) through `POST /admin/logs/analysis/:name`, and print each result as a table. Finally,
     print the `LOG_DIR` tree to show the `logs/<role>/<date>/` layout from §4.2.
  7. Each demo prints `DEMO OK: <name>` as its last line only after its assertions pass. On any failed
     assertion it exits non-zero with a message naming the assertion.
  8. `demos/README.md`: one section per demo giving the run command, what it demonstrates, which platform
     features and analysis queries it touches, and the expected tail of its output. Add a note that demos run
     via `deno run` because `deno.json` is frozen after item 01.
- **Acceptance criteria:**
  - With `export PATH="$HOME/.deno/bin:$PATH"`, each of `deno run -A demos/documents/main.ts`,
    `deno run -A demos/backup-restore/main.ts` and `deno run -A demos/observability/main.ts` exits 0 and
    prints `DEMO OK: <name>` as its last line.
  - All three run concurrently (`… & … & … & wait`) and all exit 0, which shows that ports and temp dirs are
    independent.
  - After the runs, `git status --porcelain` is unchanged; `./data`, `./logs` and `./object-store` were not
    created by the demos; and `pgrep -f "platform/(server|reader).ts"` prints nothing.
  - Demos assert, not just print. The reviewer flips one expected value locally (for example the restored
    `_version`) and confirms that the demo exits non-zero without printing `DEMO OK`.
  - `grep -rnE 'from "[^"]*platform/' demos/` returns nothing, and no file under `demos/<a>/` imports from
    `demos/<b>/`.
  - `deno check demos/*/main.ts`, `deno task fmt:check` and `deno task lint` pass.
  - `git diff --name-only <base>..HEAD` lists only paths under `demos/`, and `deno.json` is untouched.

#### Item 29 — `docs.md`: platform user guide

- **Scoping:** CONCURRENT with 28; the two items are independent of each other. Starts only after batch 9 has
  landed (both 26 and 27).
- **Model:** **Sonnet / sol.** This one is a judgment call. The guide needs editorial choices about what a
  newcomer must learn first and how to explain Effect-typed handlers. But every fact it states is fixed by
  landed items and existing docs, the outline is prescribed below, and acceptance is mechanical: a parity test
  against the item-08 options table and the item-23 catalog, plus running every snippet. That matches item 27,
  which is also prose over a settled codebase. The `reviewer-opus-high` pass covers the judgment that is left:
  whether the guide's statements about consistency, backup and error guarantees are accurate.
- **Owns:** `docs.md` (new, repo root), `tests/unit/docs_guide_test.ts`.
- **Reads:** `README.md` (the Configuration section is the source of truth for options; also Run, Query
  language, Transaction contract, Backups and restore, Deliberate limits), `platform/config.ts` (`OPTIONS`),
  `sdk/schema.ts`, `sdk/db.ts`, `sdk/functions.ts`, `sdk/client.ts`, `app/schema.ts`, `app/functions.ts`,
  `app/rules.ts`, `core/errors.ts` (`toHttpStatus`), `platform/registry.ts`, `platform/http/router.ts`,
  `platform/analysis/catalog.ts`, `scripts/logs.ts`, `scripts/restore.ts`, `docs/observability.md`,
  `docs/testing.md`, `perf/README.md`, `docs/adr/*`, `deno.json`.
- **Work:**
  1. Write `docs.md` for a developer new to the platform, with exactly these H2 sections in this order:
     `## Installation`, `## Configuration`, `## Core concepts`, `## SDK walkthrough`, `## Running the server`,
     `## Backups and restore`, `## Observability`, `## Troubleshooting`. Above them, a short introduction with
     the process diagram and a pointer to README "Deliberate limits".
  2. Installation: Deno 2.9.5 and the `PATH` line, the committed `deno.lock`, and the first-run network
     requirement for npm packages and the DuckDB `sqlite` extension.
  3. Configuration: link to [README → Configuration](README.md#configuration) as the single source of truth
     and **do not copy the options table**. Explain only the handful of options a newcomer touches first
     (`PORT`, `DATA_DIR`, the three tokens, `BACKUP_INTERVAL_MS`, `LOG_DIR`), and write any default as
     `` `ENV_VAR` (default `value`) `` so the parity test can check it. Explain fail-fast `ConfigError` at
     startup.
  4. Core concepts: documents (fields, platform-owned `_id`, `_creationTime` and `_version`), collections
     (state once that the code calls them tables, via `defineTable` and `db.find("games", …)`, then use one
     term consistently), the two storage layouts, functions (query vs mutation, where each runs, the
     transaction contract), and the static deployment registry. Say plainly that adding an app means editing
     `platform/registry.ts` and `platform/config.ts`, because there is no plugin loading.
  5. SDK walkthrough: a schema, one query, one mutation written with `Effect.gen` (the pattern from §1.4), the
     typed Effect client, and handling a typed error with `Effect.catchTag`. Every TypeScript block is a
     complete module with imports: no `…` elisions and no fragments.
  6. Running the server: `deno task serve`, the health check, the two registered apps, the dev-only token
     warning, and the error-to-HTTP-status table from §1.3, including the 409 and 503 behaviour changes.
  7. Backups and restore: the manual backup endpoint, the timer, the local stand-in versus S3, offline
     restore, and the "asynchronous backup, not HA" caveat. Mark any S3 block
     `# not run: needs S3
     credentials` on its first line.
  8. Observability: the `logs/<role>/<date>/` layout, `logs:ingest`, `logs:query` and `logs:sweep`, the admin
     ingest and analysis endpoints, each named query in one line, and the trace-id workflow. Link to
     `docs/observability.md` for table schemas rather than repeating them.
  9. Troubleshooting: one entry per symptom, as symptom → cause → fix. Cover at least: `deno` not found;
     `ConfigError` at startup (`BACKUP_INTERVAL_MS=500`, or `S3_BASE_URL` without credentials); DuckDB
     extension download failure; schema drift on an existing data file; 401, 404, 409, 413 and 503 responses;
     reader exited / 502; and port already in use.
  10. `tests/unit/docs_guide_test.ts` reads `docs.md` and asserts: (a) every env-var-shaped token
      (`[A-Z][A-Z0-9]*_[A-Z0-9_]+`, plus `PORT`) is an `OPTIONS` `envVar`, apart from a small, commented
      allowlist of non-config names; (b) every `` `ENV_VAR` (default `value`) `` matches that option's
      `defaultLabel` exactly; (c) every backticked analysis query name is a key of the item-23 catalog; (d)
      every `deno task <name>` it mentions exists in `deno.json`'s `tasks`; (e) it links to
      `README.md#configuration` and contains no Markdown table with an `Env var` column; (f) the eight H2
      headings from step 1 are present, in order.
- **Acceptance criteria:**
  - `deno task test` passes, including `tests/unit/docs_guide_test.ts`.
  - Config parity is enforced. The reviewer adds a fake `FOO_BAR` mention, or changes one stated default to a
    wrong value, and confirms that the test fails. Every config option docs.md mentions matches the item-08
    options table exactly.
  - Every snippet is copy-paste runnable. The reviewer runs every `sh` block in order from a fresh clone with
    a temp `DATA_DIR`; each exits 0, except blocks whose first line is `# not run: <reason>`. The reviewer
    also writes each `ts` block to a temp file at the repo root and runs `deno check` on it, then runs the
    client snippets against the server that the guide's own commands started.
  - `grep -c '^## ' docs.md` shows the eight required sections, and `deno task fmt:check` passes.
  - `git diff --name-only <base>..HEAD` lists exactly `docs.md` and `tests/unit/docs_guide_test.ts`.

---

## 4. Observability data model (reference for items 07, 11, 21, 23, 24)

### 4.1 DuckDB append-only tables — `logs/duckdb/events.duckdb`

```sql
CREATE TABLE IF NOT EXISTS platform_events (
  event_id   VARCHAR, ts TIMESTAMP, level VARCHAR, role VARCHAR, pid INTEGER,
  trace_id   VARCHAR, span_id VARCHAR, parent_span_id VARCHAR,
  app_id     VARCHAR, event_kind VARCHAR, handler_kind VARCHAR, name VARCHAR,
  outcome    VARCHAR, error_tag VARCHAR, duration_ms DOUBLE,
  attrs      JSON, source_file VARCHAR, source_offset BIGINT
);
CREATE TABLE IF NOT EXISTS sql_events (
  ts TIMESTAMP, trace_id VARCHAR, app_id VARCHAR,
  engine VARCHAR,          -- sqlite | duckdb
  dialect VARCHAR, statement_kind VARCHAR,   -- select | insert | update | delete | ddl | pragma
  table_name VARCHAR, sql_text VARCHAR, bind_count INTEGER,
  duration_ms DOUBLE, row_count BIGINT, outcome VARCHAR, error_tag VARCHAR
);
CREATE TABLE IF NOT EXISTS tx_events (
  ts TIMESTAMP, trace_id VARCHAR, app_id VARCHAR, engine VARCHAR,
  outcome VARCHAR,         -- commit | rollback | interrupted
  operations INTEGER, duration_ms DOUBLE, queue_wait_ms DOUBLE, error_tag VARCHAR
);
CREATE TABLE IF NOT EXISTS http_events (
  ts TIMESTAMP, trace_id VARCHAR, method VARCHAR, path VARCHAR,
  app_id VARCHAR, handler_kind VARCHAR, name VARCHAR, status INTEGER,
  duration_ms DOUBLE, bytes_in BIGINT, bytes_out BIGINT, error_tag VARCHAR
);
CREATE TABLE IF NOT EXISTS queue_events (
  ts TIMESTAMP, queue VARCHAR, depth INTEGER, cap INTEGER,
  event VARCHAR            -- high-water | rejected | dropped
, dropped BIGINT);
CREATE TABLE IF NOT EXISTS backup_events (
  ts TIMESTAMP, app_id VARCHAR, phase VARCHAR, key VARCHAR,
  bytes BIGINT, sha256 VARCHAR, duration_ms DOUBLE, outcome VARCHAR, error_tag VARCHAR
);
CREATE TABLE IF NOT EXISTS lifecycle_events (
  ts TIMESTAMP, role VARCHAR, pid INTEGER, phase VARCHAR, detail JSON
);
CREATE TABLE IF NOT EXISTS k6_points (
  ts TIMESTAMP, run_id VARCHAR, metric VARCHAR, value DOUBLE,
  scenario VARCHAR, status VARCHAR, trace_id VARCHAR, tags JSON
);
CREATE TABLE IF NOT EXISTS _ingest_state (
  source_file VARCHAR PRIMARY KEY, bytes_ingested BIGINT,
  prefix_sha256 VARCHAR, last_ingested_at TIMESTAMP
);
```

Only `INSERT` is ever issued against the event tables. `_ingest_state` is the single mutable table.

### 4.2 Log directory layout and retention

```text
logs/
  server/2026-10-10/events-14-31887-001.jsonl
  reader/2026-10-10/events-14-31902-001.jsonl
  server/2026-10-08/events-09-31010-001.jsonl.gz     # gzipAfterDays = 2
  k6/<run-id>/{raw.json,summary.json}
  duckdb/events.duckdb
```

Defaults: `keepDays 14`, `gzipAfterDays 2`, `maxTotalBytes 2 GiB`, `maxFileBytes 64 MiB`. Roll on date change,
hour change, or size. Today's directory and the open file are never swept.

### 4.3 Named analysis queries (item 23)

| Name                        | Question it answers                                                                                               |
| --------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `slowest-sql`               | p50/p95/p99 `duration_ms` by engine, statement kind and table                                                     |
| `plan-shapes`               | distinct compiled SQL texts with counts and latency — which plans actually run, and whether declared indexes help |
| `error-rate-by-handler`     | request count, error rate and error tag mix per app and handler                                                   |
| `mutation-vs-query-latency` | writer path vs reader path latency distributions over time                                                        |
| `handler-throughput`        | requests per minute per handler, with status breakdown                                                            |
| `queue-depth`               | writer/reader queue high-water marks, rejections and dropped events over time                                     |
| `tx-outcomes`               | commit/rollback/interrupted counts and queue wait times                                                           |
| `backup-history`            | per-app backup phases, durations, bytes, failures, and gaps between successes                                     |
| `trace`                     | the complete ordered event timeline for one `trace_id` across both processes                                      |
| `k6-vs-platform`            | k6 client p95 joined to server handler p95 per 10-second bucket per scenario                                      |

---

## 5. Workstream-to-item traceability

| Workstream                                                       | Items                                      |
| ---------------------------------------------------------------- | ------------------------------------------ |
| 1 — Effect port (services, layers, errors, DI)                   | 01, 02, 08, 10, 15, 16, 17, 18, 19, 22, 26 |
| 2 — Testability (pure core, swappable interfaces)                | 04, 05, 06, 07, 09, 10, 20                 |
| 3 — Unit + fast-check property tests                             | 04, 05, 06, 07, 12, 13, 20, 21             |
| 4 — Event logging (append-only DuckDB, real Logger)              | 07, 11, 21                                 |
| 5 — Observability (DuckDB analysis, folder rotation + retention) | 06, 11, 21, 23, 24                         |
| 6 — Perf (k6 + DuckDB analysis)                                  | 03, 14, 25                                 |
| Docs and boundary enforcement                                    | 02, 23, 26, 27                             |
| Onboarding (runnable demos, platform user guide)                 | 28, 29                                     |

---

## 6. Risks and how this plan handles them

| Risk                                                                                       | Handling                                                                                                                                                    |
| ------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Effect's typed error channel makes everything `Effect<A, E, R>` and the app API gets worse | Item 10 is Opus and is judged on `app/functions.ts` readability and on `tests/types.ts` still inferring identically                                         |
| The 20 existing tests quietly weaken during the port                                       | Every item's acceptance criteria require them green with unmodified assertions; items 04, 09 and 10 ship compat wrappers so no commit breaks the tree       |
| Compiler refactor changes generated SQL                                                    | `deno task explain` + `git diff --exit-code GENERATED_SQL.txt` is a required check in items 04, 09, 10 and 27                                               |
| `StorageTest` diverges from real SQLite, invalidating fast tests                           | Item 20's conformance suite runs the same script against both implementations                                                                               |
| Logging on the hot path slows or fails mutations                                           | Item 11: `emit` is `Effect<void, never>`, dropping queue, batched background fiber, drop counts logged                                                      |
| DuckDB in the writer process reintroduces the dual-SQLite hazard                           | JSONL is the write path; DuckDB ingest runs only in the reader process; enforced by the boundary test in item 26                                            |
| Retention deletes the wrong files                                                          | Policy is pure and property-tested (06, 12) before any deletion code exists (24); `--dry-run`, `LOG_DIR` prefix check, open-file and today's-dir exclusions |
| k6 or DuckDB unavailable in the executing environment                                      | Items 03, 16, 25 require the commit message to state exactly which assertions were not executed; a skip is never reported as a pass                         |
| Effect 3 → 4 migration debt                                                                | ADR 0001 records the pin and the reason; revisit after this phase                                                                                           |
| User guide and demos drift from the platform they describe                                 | Item 29's parity test checks `docs.md` against `OPTIONS`, the analysis catalog and `deno.json`; item 28's demos assert their own results and exit non-zero  |
| Batch concurrency causes merge conflicts                                                   | `Owns`/`Reads` sets are disjoint per batch, `deno.json` is written once in item 01, and reviewers reject diffs outside an item's `Owns` set                 |

---

## Future work: deterministic simulation testing

**Not in scope for this phase.** Nothing in items 01–37 attempts deterministic simulation testing (DST), and
no item's acceptance criteria depend on it. It is the stated next goal, and this phase is deliberately shaped
to make it reachable rather than to deliver it.

What DST would mean for this system: run the whole platform — HTTP requests, the mutation queue, the reader
process, backups, the log sink, the retention sweeper — inside a single-threaded simulated world driven by a
seed, where time, concurrency interleaving, I/O latency and faults are all decided by the seed. A failing run
is then reproducible from that one number, and a schedule that exposes a race can be shrunk and replayed
indefinitely.

Why this phase is the right foundation:

- Every dependency is already behind a `Context.Tag` with a live layer and a test layer, which is exactly the
  substitution point a simulator needs.
- `TestClock` already removes wall-clock time from the backup schedule, the log rotation boundaries and the
  IPC timeouts.
- `StorageTest` (item 20) is already a pure, `Ref`-based implementation of the document store with a
  conformance suite proving it matches real SQLite — the hard prerequisite for a deterministic storage engine.
- The pure core (`core/compile`, `core/schema`, `core/plan`, `core/ipc/framing`, `core/logging/rotation`,
  `core/eventlog/offsets`) has no nondeterminism left to simulate.
- The event log gives the simulator a ready-made trace to assert invariants against, and the property-test
  arbitraries from items 12 and 13 are reusable as workload generators.

What a DST phase would still need to build:

1. A seeded PRNG service, and a rule that no code may use `Math.random` or `crypto.randomUUID` outside
   `IdGen`.
2. A deterministic fiber scheduler: `Effect`'s runtime is cooperative and single-threaded, but choosing a
   reproducible interleaving requires a custom `Scheduler` and a ban on raw `Promise`/`setTimeout` anywhere in
   the tree (item 26's boundary tests already move toward this).
3. A simulated filesystem and object store with seeded latency, partial writes, `fsync` reordering and ENOSPC
   injection.
4. An in-process, seeded replacement for the reader IPC channel (`ReaderClientTest` is the beginning of this)
   with message reordering, duplication, delay and drop.
5. A fault-injection vocabulary tied to the real failure modes this repo documents: crash between
   `VACUUM INTO` and the object PUT; crash after the immutable PUT but before `latest.json`; reader process
   death mid-query; SIGKILL during `COMMIT`.
6. Invariant checkers run after every simulated step: `_version` is monotonic per document, a rolled-back
   mutation leaves no trace, `latest.json` always points at a verifiable snapshot, the ingested event log is a
   permutation-free prefix of what was emitted, no document is visible to a reader before its commit.
7. A workload generator producing concurrent client sessions (the tic-tac-toe rules make a good oracle: the
   pure `applyMove` from item 10 is the reference model for a linearizability check).
8. A seed corpus and a shrinker, plus CI that runs N seeds per commit and pins any seed that ever failed.

The known blockers, worth stating now: `node:sqlite` and `@duckdb/node-api` are native and not virtualizable,
so DST runs would use `StorageTest` and an analogous pure reader rather than the real engines — which means
DST complements, and does not replace, the real-process integration test in item 25. Deciding how much
confidence a simulated-storage DST run actually buys is the first design question of that phase, not an
implementation detail.

---

### Batch 11 — accepted reflection correctness follow-ups (mandatory before 23 and MT)

Source: accepted `origin/reflection/astra:docs/architecture/reflection-astra.md`, P1 suggestions 1–5 and P2
suggestions 6–8. Numbering continues after 29; placement at the end of this document does not imply execution
after batch 10. The §3 order is authoritative. All eight items start after item 22 lands and finish before
item 23; none waits for MT.

**Status evidence, 2026-10-11:** `git ls-remote origin refs/heads/main` verified that the live remote matches
local `origin/main` at `a030ed6ee1488d6aa4bca8dd72d3f719ec76c28d`. The requested fetch could not write the
session's read-only Git metadata; the read-only remote check establishes this snapshot instead.
`git log origin/main --oneline --grep='item 22'` matched historical commit bodies, not a landed item-22
implementation. The ref still has facade-based `platform/server.ts`, no `platform/layers.ts` or
`platform/http/`, and no `platform/logging/retention.ts`. Thus neither 22 nor 24 has landed in the verified
snapshot; re-fetch and inspect their actual landed diffs before dispatch. Never touch item 22's in-flight
branch or worktree.

**Scheduling exception to §2.2:** batch 11 is a staged release gate rather than one concurrent wave. Every new
item's exact `Owns` set is disjoint, but dependencies still require `(30+35) → 31 → 36 → 32 → 33 → 34 → 37`.
Only 30 and 35 are independent and concurrent. The existing item 24 lands before this gate; 36 strengthens its
implementation. Item 24's previously recorded concurrency with 23/25 does not authorize bypassing this order.

**Ownership handoffs:** 32 extends landed item 22's server/registry/layers/router/auth; 34 alone extends its
HTTP handlers. 33 owns the reader entry and reader-only layer composition. It also prepares optional trace
context reception for 34; 34 owns the public wire declaration and send path. 35 freezes the event schema
before 36 changes ingest ordering and before 23 writes its catalog. None owns item 23's catalog, CLI or
`docs/observability.md`, item 25's integration files, or `deno.json`. Item 36 takes over 24's retention files
only after 24 lands. Later MT-02 extends 37's README; MT-04 extends 31's SQLite storage; MT-03/04 extend 34's
services; MT-05/07/08 extend 32/34's HTTP, layers and SDK. These are sequential handoffs, not concurrent
ownership. MT-01/06/09/10 have no overlapping new-item writes. Diagram maintenance agents run between landing
barriers, not alongside 37's diagram verification.

**Model labels:** `Opus / astra`, `Sonnet / sol`, `Haiku (≥5.5, never ≤4.5) / luna (high effort)` use
AGENTS.md's Model duality table: `gpt-6.1-sol`, `gpt-6-luna`, `gpt-6-astra`, respectively. The machine's cache
verifies these IDs, not this policy's intelligence/price ordering. Every new item gets an independent top-tier
review and must leave `deno task ci` green. Commands below are future builder/reviewer acceptance, not tests
run by this planning session.

#### Item 30 — SQLite JSON number semantics

- **Scoping:** After 22 lands, before 23 and all MT. CONCURRENT only with 35; their exact file sets and inputs
  are independent. Land before 31, which owns promotion of the shared oracle regressions.
- **Model:** **Opus / astra.** Numeric coercion affects filtering, grouping and ordering across both engines.
- **Owns:** `core/compile/select.ts`, `tests/unit/compile_test.ts`, `tests/property/json_number_test.ts`,
  `tests/unit/number_dialects_test.ts`, `GENERATED_SQL.txt`.
- **Reads:** `core/schema.ts`, `core/compile/ddl.ts`, `core/compile/write.ts`, `core/compile/decode.ts`,
  `core/plan.ts`, `tests/property/sqlite_oracle_test.ts`, `tests/property/arbitraries.ts`,
  `platform/sqlite.ts`, `platform/duckdb.ts`, `scripts/explain.ts`.
- **Work:**
  1. Give schema-declared `number` expressions consistent SQLite REAL semantics in JSON layout; preserve
     integer, boolean, text, metadata and NULL behavior. Use the same expression for predicates, ordering and
     aggregate/group keys. Do not fix this by coercing every JSON scalar or weakening safe-integer checks.
  2. Add seeded regression/property coverage for 2^53, adjacent representable doubles, the reported
     1693240866440448800 case, negative and fractional values, NULL and safe integers. Compare equality,
     inequalities, ordering and countBy with a JS reference and both storage layouts.
  3. Compare SQLite and DuckDB results using separate child processes; never import both native libraries into
     one address space. Regenerate the SQL artifact and review the intentional expression changes.
- **Acceptance criteria:**
  - `ORACLE_KNOWN_ISSUES=1 deno test -A --filter 'oracle known issue 1' tests/property/sqlite_oracle_test.ts`
    passes both reported numeric regressions without skips.
  - `deno test -A tests/property/json_number_test.ts tests/unit/number_dialects_test.ts` passes the boundary
    matrix; property output records the repo seed, and neither engine leg is silently skipped.
  - `deno task explain && git diff --exit-code GENERATED_SQL.txt` passes against this item's committed
    artifact.
  - `deno task ci` passes; 31 subsequently removes the old oracle opt-in gate for the resolved cases.

#### Item 31 — Embedded-NUL text storage contract

- **Scoping:** After 22 and 30 land, before 23 and all MT. Serialized; this item alone owns the existing
  oracle file shared by the reflection's three regressions. Its SQLite changes precede MT-04.
- **Model:** **Opus / astra.** Accepted strings and identifiers must never silently change across a commit.
- **Owns:** `core/schema.ts`, `platform/sqlite.ts`, `tests/unit/schema_test.ts`, `tests/unit/storage_test.ts`,
  `tests/property/sqlite_oracle_test.ts`, `tests/layers/storage_test_layer.ts`,
  `tests/unit/nul_contract_test.ts`, `docs/adr/0005-text-storage-contract.md`.
- **Reads:** `core/errors.ts`, `core/compile/write.ts`, `core/compile/select.ts`, `core/compile/decode.ts`,
  `sdk/db.ts`, `sdk/functions.ts`, `platform/services.ts`, `tests/property/storage_commit_test.ts`,
  `tests/core_test.ts`.
- **Work:**
  1. Choose and record either lossless driver reads or typed rejection of embedded U+0000 before commit.
     Define the same public contract for columns and JSON layouts, scalar/enum/array values and document
     identifiers, including generated IDs. Keep schema/table-name validation explicit. Existing stored NUL
     values must be decoded losslessly or fail explicitly; never return a truncated success.
  2. Enforce the choice at the real Storage boundary for insert, patch, lookup/delete IDs and handler-return
     paths as applicable. For rejection, use existing SchemaError/StorageError payloads and prove rollback of
     prior writes in the same mutation. Keep StorageTest consistent with the chosen contract.
  3. Replace the NUL oracle's bypass of storage validation with a real-storage regression that asserts the
     chosen behavior. Promote both item-30 numeric regressions and this NUL regression into the default suite;
     remove the obsolete known-issue opt-in gate without deleting coverage.
- **Acceptance criteria:**
  - `deno test -A tests/unit/nul_contract_test.ts tests/unit/storage_test.ts` covers both layouts, leading,
    interior and trailing NUL, IDs, patch and preexisting data; every accepted value round-trips unchanged.
  - `deno test -A tests/property/sqlite_oracle_test.ts` runs all three reflection regressions with no opt-in
    and no skips. `deno task test:prop` verifies the commit/rollback properties.
  - `deno task test` passes the real/test storage conformance checks; `deno task ci` passes.

#### Item 32 — Scoped server composition follow-up after item 22

- **Scoping:** FOLLOW-UP only, after 22 lands and its diff is re-read; before 23 and all MT. The inspected
  local origin/main still lacks 22, so this is the not-yet-landed case, not permission to replace its build.
  Starts after 31, 35 and 36. Owns no HTTP handlers or reader entry; 33/34 follow this composition.
- **Model:** **Opus / astra.** Partial startup and shutdown require correct ownership of every scoped
  resource.
- **Owns:** `platform/server.ts`, `platform/registry.ts`, `platform/layers.ts`, `platform/http/router.ts`,
  `platform/http/auth.ts`, `tests/unit/http_test.ts`, `tests/unit/server_scope_test.ts`.
- **Reads:** `platform/http/handlers.ts`, `platform/services.ts`, `platform/config.ts`, `platform/sqlite.ts`,
  `platform/ipc.ts`, `platform/backup.ts`, `platform/logging/logger.ts`, `platform/logging/retention.ts`,
  `core/errors.ts`, `tests/layers/index.ts`.
- **Work:**
  1. Compare item 22's landed implementation with the reflection concerns. Preserve completed work; fix
     remaining composition gaps and add missing failure-path evidence. Mount actual logging and typed HTTP
     error mapping, centralize environment reads through Config, and share layers without duplicate handles.
  2. Scope acquisition from the first writer/child onward. Inject failures after each acquisition, including a
     later app open and HTTP bind failure; release every earlier resource even when serving never starts.
  3. Verify HTTP drain, backup/sweep fiber shutdown, reader exit, writer close and final log flush ordering.
     Trigger 36's ingest-then-sweep coordinator with the existing ingest RPC through ReaderClient; the server
     never opens the event DuckDB file. Leave reader-only composition to 33, and handler trace changes to 34.
- **Acceptance criteria:**
  - `deno test -A tests/unit/server_scope_test.ts tests/unit/http_test.ts` passes partial-startup, occupied
    port, in-flight SIGTERM, child-exit and finalizer-order probes, with bounded termination and no live
    child/handle remaining. HTTP cases assert 400/401/404/409/413/500/502/503 and server JSONL emission.
  - `deno task integration` passes against the real server and child, including backup/restore.
  - `deno task check` and `deno task ci` pass. The scope tests also inspect the server import graph and fail
    if DuckDB is transitively reachable or server/registry bypass Config for environment reads.

#### Item 33 — Connect reader RPC to EventLogIngestLive

- **Scoping:** After 22, 32, 35 and 36 land, before 23 and all MT. Serialized; extends the landed reader entry
  without editing 32's server layer root. Prepares the reader trace receiver for item 34.
- **Model:** **Opus / astra.** Persistent DuckDB scope ownership and serialized RPC access are correctness
  boundaries.
- **Owns:** `platform/reader.ts`, `platform/reader_layers.ts`, `platform/reader_context.ts`,
  `tests/unit/reader_ingest_test.ts`, `tests/unit/reader_context_test.ts`.
- **Reads:** `platform/eventlog/ingest.ts`, `platform/eventlog/tables.ts`, `platform/services.ts`,
  `platform/config.ts`, `platform/duckdb.ts`, `platform/registry.ts`, `platform/logging/logger.ts`,
  `platform/ipc.ts`, `core/ipc/framing.ts`.
- **Work:**
  1. Replace the unavailable-ingest response with the real EventLogIngest service. Compose its persistent
     handle, Config, IdGen and logging in a reader-only root; retain connection-before-instance release and
     serialized query/ingest access. Keep stdout exclusively RPC frames. Leave named analysis to item 23.
  2. Connect 36's committed-offset/retention coordination in this process. No writer-side DuckDB import, even
     transitively; entrypoint imports must make event-log modules reachable by `deno task check`.
  3. Factor request context reception into `reader_context.ts`: accept validated optional `traceId`, `spanId`
     and `parentSpanId` wire properties, annotate the request Effect, and preserve the reader-local fallback
     for old senders. Validate these as optional properties of unknown JSON before narrowing to the existing
     ReaderRequestFrame; do not require item 34's later type extension. Do not replace a supplied valid caller
     trace. This prepares, but does not claim completion of, item 34's end-to-end contract.
- **Acceptance criteria:**
  - `deno test -A tests/unit/reader_ingest_test.ts tests/unit/reader_context_test.ts` starts a real reader,
    ingests a fixed closed JSONL source via RPC, repeats with zero new rows for that source, restarts and
    proves committed offsets survive. Append a tail and assert only its new records appear. Account separately
    for new ingest/lifecycle records emitted by the operation itself.
  - The same command injects ingest failure and interruption, verifies typed replies, pure protocol stdout,
    continued query usability where recoverable, and scope closure without leaked handles.
  - `deno task check` and `deno task ci` pass; the reader test checks the actual import graph excludes
    `node:sqlite` and includes eventlog through the reader entrypoint.

#### Item 34 — End-to-end caller trace context

- **Scoping:** After 22, 32 and 33 land, before 23 and all MT. Serialized. Consumes 33's reader context
  receiver and 32's HTTP root; owns neither entrypoint. Must precede MT-03/04 services and MT-07 client edits.
- **Model:** **Opus / astra.** Cross-process context propagation must isolate concurrent requests and preserve
  causality.
- **Owns:** `sdk/client.ts`, `platform/services.ts`, `platform/ipc.ts`, `platform/http/handlers.ts`,
  `platform/http/trace.ts`, `tests/unit/trace_test.ts`, `tests/unit/ipc_test.ts`,
  `tests/unit/trace_e2e_test.ts`.
- **Reads:** `platform/server.ts`, `platform/http/router.ts`, `platform/reader.ts`,
  `platform/reader_context.ts`, `platform/sqlite.ts`, `platform/duckdb.ts`, `core/events.ts`,
  `platform/logging/logger.ts`, `tests/types.ts`, `perf/lib/api.js`.
- **Work:**
  1. Preserve the SDK's existing caller-supplied trace argument and generated fallback. Specify bounded
     validation, missing/invalid-header behavior and response echo consistently at HTTP ingress.
  2. Extend ReaderRequest/ReaderRequestFrame with optional trace/span context matching 33's receiver. Capture
     context at call submission, not in the shared IPC writer fiber. Restore it per request in the child using
     the landed receiver; keep correlation IDs separate from trace IDs and support old frames.
  3. Apply the same context to HTTP, handler, SQL and transaction events on success and failure, including
     mutation rollback and reader errors. Ensure handler entry wraps the whole dispatched Effect, not only
     console/log calls. Verify the existing storage/reader event emitters consume annotations correctly. Do
     not claim k6 correlation until its producer lands in 25.
- **Acceptance criteria:**
  - `deno test -A tests/unit/trace_test.ts tests/unit/ipc_test.ts tests/unit/trace_e2e_test.ts` passes. The
    end-to-end test uses the real SDK, HTTP server and reader child with isolated directories; one explicit
    trace is echoed and identifies HTTP/handler/SQL/tx JSONL events in both processes across a query and
    mutation. It also covers generated trace IDs, invalid input, rollback and error responses.
  - The command overlaps two callers with distinct traces and asserts no cross-assignment, including queued
    IPC work, cancellation and late replies. Missing-context legacy frames retain a nonempty fallback.
  - `deno task check` preserves SDK inference; `deno task integration` and `deno task ci` pass.

#### Item 35 — Event identity and taxonomy for exact joins

- **Scoping:** After 22 lands, before 23 and all MT. CONCURRENT only with 30. Lands before 36 changes ingest
  coordination, 32 composes producers and 23 writes queries against this schema.
- **Model:** **Opus / astra.** Event identity, compatible schema evolution and backup accounting require
  semantic decisions.
- **Owns:** `core/eventlog/ddl.ts`, `core/events.ts`, `platform/eventlog/tables.ts`, `platform/backup.ts`,
  `platform/object_store.ts`, `tests/unit/eventlog_test.ts`, `tests/unit/events_test.ts`,
  `tests/unit/backup_test.ts`, `tests/unit/object_store_test.ts`, `docs/adr/0006-event-identity.md`.
- **Reads:** `platform/eventlog/ingest.ts`, `core/eventlog/offsets.ts`, `platform/services.ts`,
  `scripts/duckdb/trace-timeline.sql`, `scripts/duckdb/backup-durations.sql`,
  `docs/plans/completed/effect-port-and-observability.md` §4.
- **Work:**
  1. Add stable event_id to typed projections, with exactly one canonical identity per source record. Define
     duplicate/rewrite identity rules and test exact joins; timestamps/trace IDs alone are not join keys.
  2. Separate canonical event_kind from handler_kind without losing legacy JSONL compatibility. Use explicit
     mappings for query/mutation handlers and preserve rollback/error outcomes for item 23's catalog.
  3. Distinguish backup orchestration phases from object-store transfers by emitter and backup-run/operation
     identity. Propagate the run through both producers; count retries and transfers without double-counting
     one phase as two backups. Update constructor and table mappings together.
  4. Record a versioned migration or explicit rebuild strategy before changing persisted DDL. Never silently
     reuse old tables with new mappings. Preserve originals and offsets on failure; prove old records remain
     queryable or rebuilding from retained JSONL is complete before switching. If retained sources cannot
     support a lossless rebuild, fail explicitly rather than discarding historical rows. Keep the normal
     ingest event tables append-only. Item 36 owns any necessary ingest initialization call-site changes.
- **Acceptance criteria:**
  - `deno test -A tests/unit/eventlog_test.ts tests/unit/events_test.ts tests/unit/backup_test.ts tests/unit/object_store_test.ts`
    passes exact typed-to-canonical joins, handler taxonomy, mixed old/new records, duplicate ingestion and
    one-backup/multiple-transfer accounting. Schema tests exercise a pre-change database and failed upgrade.
  - `deno task test:prop` preserves offset/idempotency properties; `deno task ci` passes. Tests use the
    existing ingestion entry so an incompatible DDL/mapping change cannot land as a library-only pass.

#### Item 36 — Retention follows committed ingest

- **Scoping:** After 22, 24, 31 and 35 land, before 23 and all MT. The inspected snapshot has no item-24
  implementation. Chosen resolution: gate this follow-up after 24 and move 24 forward in §3, rather than alter
  its existing item text beyond Model. Serialized before 32/33 wire the safe runtime path.
- **Model:** **Opus / astra.** Destructive retention must never race an uncommitted offset or hide an unread
  tail.
- **Owns:** `platform/logging/retention.ts`, `tests/unit/retention_test.ts`, `platform/eventlog/ingest.ts`,
  `core/logging/rotation.ts`, `platform/logging/file_sink.ts`, `tests/unit/retention_ingest_test.ts`,
  `tests/property/retention_ingest_test.ts`, `docs/adr/0007-ingest-before-retention.md`.
- **Reads:** `core/eventlog/offsets.ts`, `core/eventlog/ddl.ts`, `platform/eventlog/tables.ts`,
  `platform/config.ts`, `platform/services.ts`, `platform/logging/logger.ts`,
  `tests/property/offsets_test.ts`, `tests/property/arbitraries.ts`.
- **Work:**
  1. Extend 24's implementation so gzip/delete eligibility requires a closed stable source and a committed
     ingest watermark covering its entire tail. Coordinate rotation, ingest and sweep; revalidate source
     identity/size before applying a plan. An append, partial line, failed ingest or stale inventory blocks
     destructive action. Rejected lines count as consumed only with durable quarantine evidence.
  2. Expose a safe ingest-then-sweep coordinator triggered by 32 through the existing ingest RPC and hosted by
     33 in the reader; successful ingest runs gate the sweep, without adding a new RPC kind. Keep DuckDB
     access in that process. Publish the smallest module-level hooks possible; do not change services owned
     by 34. Wire 35's schema compatibility check into ingest startup if required.
  3. Retain today's/open-file and path-containment exclusions and dry-run behavior. Define crash/retry
     behavior for gzip, rename and delete. Preserve byte-offset identities across compression or never
     compress until the complete source has been durably ingested.
  4. Record the distinct budgets: retained JSONL versus the derived DuckDB file. Pruning the DuckDB event
     tables remains out of scope; measure and report unbounded derived growth rather than promising that
     LOG_MAX_TOTAL_BYTES caps it. Account for logger drops separately from emitted/flushed/ingested counts.
     Item 23 must carry this contract into its observability guide and CLI.
- **Acceptance criteria:**
  - `deno test -A tests/unit/retention_test.ts tests/unit/retention_ingest_test.ts` passes real-filesystem
    cases for append-during-sweep, unread partial tails, failed commit, restart, gzip retry, active file,
    outside-root path and dry-run. No un-ingested bytes become inaccessible; durable rejected lines are
    explicitly reconciled. Direct invocation needs no not-yet-created item-23 CLI.
  - `deno test -A tests/property/retention_ingest_test.ts` passes seeded append/rotate/ingest/sweep/crash
    interleavings with exactly-once accepted-event assertions.
  - The unit integration command includes a bounded sustained-write run over multiple rotations and sweeps,
    prints input/ingested/rejected/dropped counts plus JSONL and DuckDB bytes, and asserts JSONL limits only
    where closed-file safety permits them. Unsafe-to-delete backlog is reported explicitly.
  - `deno task ci` passes. After 23 lands, `deno task logs:sweep --dry-run` verifies its CLI uses this same
    coordinator; that later CLI check is not a prerequisite for this pre-23 item.

#### Item 37 — Reconcile architecture and runtime documentation

- **Scoping:** After 22 and 30–36 land, before 23 and all MT. Final serialized gate; incorporates each landed
  diff and its required diagram follow-up. Item 23 and retention CLI work remain explicitly future.
- **Model:** **Sonnet / sol.** Bounded documentation reconciliation against landed code and recorded evidence.
- **Owns:** `README.md`, `docs/architecture/README.md`, `docs/architecture/AGENTS.md`,
  `docs/architecture/c4-context-document-db.github.md`,
  `docs/architecture/c4-context-document-db.terminal.md`, `docs/architecture/c4-container-platform.github.md`,
  `docs/architecture/c4-container-platform.terminal.md`,
  `docs/architecture/c4-container-observability.github.md`,
  `docs/architecture/c4-container-observability.terminal.md`,
  `docs/architecture/c4-component-server.github.md`, `docs/architecture/c4-component-server.terminal.md`,
  `docs/architecture/diagrams/c4-context-document-db.py`,
  `docs/architecture/diagrams/c4-container-platform.py`,
  `docs/architecture/diagrams/c4-container-observability.py`,
  `docs/architecture/diagrams/c4-component-server.py`.
- **Reads:** `platform/server.ts`, `platform/layers.ts`, `platform/reader.ts`, `platform/reader_layers.ts`,
  `platform/eventlog/ingest.ts`, `core/eventlog/ddl.ts`, `platform/logging/retention.ts`, `sdk/client.ts`,
  `docs/adr/0005-text-storage-contract.md`, `docs/adr/0006-event-identity.md`,
  `docs/adr/0007-ingest-before-retention.md`, `docs/architecture/diagrams/render.py`, accepted reflection
  report and landed diffs for 22, 24 and 30–36.
- **Work:**
  1. Replace the architecture index's item-21-planned claim and root README's obsolete environment caveat with
     precise dated evidence. Distinguish available libraries, connected runtime, tested behavior and remaining
     targets; do not present a reflection snapshot as a current full-suite pass.
  2. Align architecture instructions with current-state diagrams and explicitly labeled target views.
     Reconcile all four Mermaid/terminal pairs with the actual landed dependency graphs and event schema.
     Explain reader composition in the existing container/observability pair; a new reader-component diagram
     is deferred to keep the exact ownership set bounded. Regenerate terminal embeds from owned sources.
  3. Document number/NUL behavior, runtime ingest, trace propagation and retention/derived-DB limits. Preserve
     the README Configuration source of truth for later MT-02. Describe item 23's catalog/CLI and item 25's k6
     producer as future, even though ingest RPC and safe retention are now connected.
- **Acceptance criteria:**
  - `python3 docs/architecture/diagrams/render.py --check` passes after regeneration, and `git diff --check`
    plus `deno fmt --check README.md docs/architecture/` pass.
  - Reviewer runs `git show --stat` and `git diff <pre-22-base>..HEAD -- platform/ core/ sdk/` and checks each
    diagram relationship/status claim against those landed files; record that evidence, not just a successful
    render check. Mermaid and terminal relationship keys agree.
  - `deno test -A tests/unit/trace_e2e_test.ts tests/unit/reader_ingest_test.ts tests/unit/retention_ingest_test.ts`
    passes on the complete batch, proving actual composition rather than isolated helper tests.
  - `deno task ci` and `deno task integration` pass; README labels any unexecuted external checks with their
    reason. Item 23 and the MT stack may start only after this final batch gate passes.
