# Document DB MVP — TypeScript document API, SQLite storage, DuckDB analytics

**Application authors write schemas and document operations, not SQL.** The platform captures document
operations as a small intermediate representation and compiles document reads to SQLite SQL. DuckDB remains
the engine for event-log ingest and named analysis. Regular TypeScript control flow (the tic-tac-toe rules)
still runs as TypeScript.

## Verification status

Item 27 audit on 2026-10-11, based on landed main `643b706`, on this 8 GB VM with Deno 2.9.5 and
`@duckdb/node-api` 1.5.6-r.1. These are dated observations, not standing guarantees.

| Command                                                | Evidence in this environment                                                                                                                 |
| ------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `deno task ci`                                         | **PASS on first attempt**: formatting, lint, type checks; 597 unit/service passed, 1 optional S3 case ignored; 79 property passed; 0 failed. |
| `deno task integration`                                | PASS: both layouts, read-after-write, aggregation, app isolation, backup/restore, HTTP auth, trace JSONL, ingest and analysis.               |
| `deno task e2e`                                        | **PASS locally**: 7 passed, 0 failed, seed 20261010, skipped=none; 101.75 s including setup/cleanup. MT-10 also landed green at `643b706`.   |
| `PROP_SEED=20261010 deno task test:prop`               | PASS: 79 passed, 0 failed; explicit default-seed replay (2m20s).                                                                             |
| `deno task explain > GENERATED_SQL.txt`                | Regenerated; deterministic output checked after committing.                                                                                  |
| `python3 docs/architecture/diagrams/render.py --check` | Passed for all four diagram pairs (both formats).                                                                                            |

The older Node run in `tests/CORE_TEST_RESULTS.txt` is historical. Earlier full unit runs sometimes exited 139
in a native DuckDB worker; item 27 records its actual results below rather than borrowing those counts. Real
S3 cases are not executed: no endpoint/credentials were supplied; the optional backup test is ignored.

Operational examples in this README and [SKIM.md](SKIM.md) (serve, demo, backup curl, restore and alternate
server configurations) are **not executed individually in item 27**: they need a managed, long-lived server
and operator-selected files/tokens. The integration and E2E harnesses exercise their underlying paths in
isolated scratch environments. Performance tasks are **not executed here**: latency thresholds depend on host
capacity and this VM is shared with other builders. See [perf/README.md](perf/README.md).

Command audit for the read-only [observability guide](docs/observability.md): `logs:ingest`, `logs:list`,
`logs:sweep` (including `--dry-run`) and its concrete `logs:query` examples are executed against an empty
scratch event log in item 27 (all exit 0; successful execution with no rows is not evidence of real traffic).
The placeholder query syntax (`<name>`, `[--param ...]`) is a usage template, not an executable command. Its
`TRACE_SQL=true bash ... perf:mixed my-run` example is **not executed** because it runs shared-host load
thresholds; its curl and jq trace pipeline are **not executed** because `$BASE_URL` and a managed
authenticated server are operator inputs. Its `perf:ingest` example is **not executed** because no fresh k6
raw-output artifact is produced by this audit. Integration/E2E verify trace and analysis paths; the k6 join
also has fixture coverage. No live load-performance claim follows from that coverage.

## Start here to skim

| File                          | What it demonstrates                                                                               |
| ----------------------------- | -------------------------------------------------------------------------------------------------- |
| `app/schema.ts`               | The developer's schema builder: typed fields and indexes.                                          |
| `app/functions.ts`            | All application code: create/move/delete mutations, reads, grouped statistics. No SQL.             |
| `sdk/db.ts`                   | Document operations become an engine-neutral query plan.                                           |
| `core/compile/`               | Schema to DDL; query plan to dialect-specific SQL; document writes to SQL; row decoding.           |
| `platform/sqlite.ts`          | Per-app mutation serialization and SQLite transactions, including mutation-internal reads.         |
| `platform/document_reader.ts` | In-process read-only SQLite connections for document queries, with a concurrency cap.              |
| `platform/sqlite_ffi/`        | FFI binding over the system `libsqlite3`: blocking point reads and writes, deadline-bounded reads. |
| `platform/server.ts`          | Server entrypoint: scoped listener, backup and ingest-then-sweep timers, signal shutdown.          |
| `platform/layers.ts`          | `PlatformLive`: the server's ordered layer chain (config, logging, per-app storage, reader).       |
| `platform/http/`              | Router, auth, handlers, and the `X-Trace-Id` / span contract (`trace.ts`).                         |
| `platform/reader.ts`          | Reader child entrypoint; its layer root is `platform/reader_layers.ts`.                            |
| `platform/backup.ts`          | Consistent SQLite snapshot, checksum, immutable object, latest manifest.                           |

`GENERATED_SQL.txt` shows actual compiler output for the two layouts and two dialects.
`deno task explain > GENERATED_SQL.txt` regenerates it without running either database engine.

## Effect services and layers

`core/` holds pure validation, plans, compilation/decoding, tagged errors and event policy. SDK terminal
operations return `Effect.Effect<_, DbError>`; application handlers compose them with `yield*`.
`platform/services.ts` declares service tags. `PlatformLive` in `platform/layers.ts` acquires Config, logging,
IdGen, ObjectStore/AppRegistry, scope-owned TenantRegistry, per-app Storage/Backups, then the ReaderClient
child. TenantRegistry connects lazily; off mode retains the existing app tokens/files. `Layer.provideMerge`
orders resource acquisition and finalization. Shutdown drains HTTP, stops timers, closes the reader and SQLite
handles, then emits the final lifecycle record and drains logging.

The reader has a separate root in `platform/reader_layers.ts`: Config, reader logging, IdGen and
EventLogIngest. `makeAnalysis` shares the event-log instance for live analysis after ingest. App query readers
are scoped independently. Writer and reader retain separate native module graphs.

With tenancy enabled, credentials resolve request-local `TenantContext`; client tenant selectors are ignored.
`TenantStorageRouter` uses `LocalFolderRouter` to prepare validated per-tenant SQLite locations. Each app
builds a router over the configured tenant directory; trusted tenant id/path travels in reader IPC. Each
tenant has one schema-guarded file, so incompatible layouts need distinct tenants (as in E2E), or separate
deployments with separate TENANTS_DIR values. Tenant-mode readers are scoped per request; legacy mode uses the
app reader pool. Postgres holds tenants, hashed API keys, users, sessions and memberships, not application
documents. See [platform/auth/README.md](platform/auth/README.md).

## Tasks

Task names below are invoked as `deno task NAME`. Standalone steps of `ci` are executed by that task.
Operational tasks marked not executed have the reasons in Verification status above.

| Tasks                                                                | Purpose / item-27 execution                                                                               |
| -------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `fmt:check`, `lint`, `check`, `test`, `test:prop`, `ci`              | Format, lint, types, unit/service, property, aggregate gate; executed.                                    |
| `e2e`                                                                | Seven real-platform scenarios (package.json task); executed separately from ci.                           |
| `integration`                                                        | Both layouts and real server/reader/backup/log paths; executed.                                           |
| `explain`                                                            | Compiler artifact; executed, redirect stdout to GENERATED_SQL.txt.                                        |
| `serve`, `demo`, `restore`                                           | Operator server/client/restore examples; not executed individually.                                       |
| `logs:ingest`, `logs:list`, `logs:query`, `logs:sweep`               | Offline ingest, catalog, analysis, retention; executed on scratch files.                                  |
| `perf:smoke`, `perf:load`, `perf:query`, `perf:mixed`, `perf:ingest` | k6 load and point import; not executed.                                                                   |
| `db:migrate`                                                         | Journaled Postgres migrations; exercised by e2e harness, not separately executed against a persistent DB. |

## Errors and HTTP statuses

`core/errors.ts:toHttpStatus` is exhaustive. Startup `ConfigError`/`SchemaDriftError` stop layer construction;
CLI and client errors are not served by this table.

| Status | Tags                                                                                                                                                                                              |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 400    | MalformedTenantId, SchemaError, CompileError, RuleError, HandlerKindMismatch                                                                                                                      |
| 401    | AuthError (also tenant credential refusals, deliberately normalized to auth failure)                                                                                                              |
| 403    | TenantDeactivated (direct mapping; authentication normalizes deactivated credentials to 401)                                                                                                      |
| 404    | UnknownTenant, RouteNotFound, UnknownAppError, UnknownHandlerError, NotFoundError, UnknownAnalysisQuery                                                                                           |
| 409    | ConflictError, BackupAlreadyRunning                                                                                                                                                               |
| 413    | RequestTooLarge                                                                                                                                                                                   |
| 500    | DecodeError, ContextExpiredError, NonSerializableResultError, StorageError, ReaderError, QueryWriteAttemptError, InvalidKeyError, BackupError, SnapshotIntegrityError, IngestError, AnalysisError |
| 502    | IpcError, ReaderExitedError, ObjectStoreError                                                                                                                                                     |
| 503    | QueueFullError                                                                                                                                                                                    |

The Effect port deliberately changes stale-version conflicts from blanket 400 to **409**, and queue saturation
to **503**. An overlapping backup is also 409; oversized bodies are 413. A mutation client should refresh
state after a conflict; retries after load shedding still need application-level idempotency.

## Minimum process model

```text
TypeScript client / browser
     |
     | named handler + JSON args (never raw SQL)
     v
Deno process 1: HTTP API + SQLite document reads/writes + backup and ingest timers
     |                  |
     | libsqlite3 (FFI) | JSON-lines RPC over stdin/stdout (ping, ingest, analysis)
     v                  v
app.sqlite          Deno process 2: DuckDB event-log ingest + named analysis
     |                  |
     | consistent       | JSONL logs -> event-log DuckDB file (derived; reader only)
     | VACUUM INTO      v
     v              <DATA_DIR>/eventlog.duckdb
local snapshot -> S3-compatible object store
```

Both long-lived processes run on **the same machine with the same local filesystem**. The server starts the
reader automatically. The two demo app IDs share these processes but have independent files. App data has no
second copy: no CDC pipeline, no replica-copy refresh loop, and no extra backup daemon. The only persistent
DuckDB file is the **derived** event-log file written by the reader's ingest from the JSONL logs
(`EVENTLOG_DUCKDB`, default `<DATA_DIR>/eventlog.duckdb`); the server never opens it. See "Observability and
the event log" below.

The server opens every application SQLite file through the FFI binding in `platform/sqlite_ffi/`, which calls
the system `libsqlite3`; the server does not load `node:sqlite`. The separate reader child remains responsible
for DuckDB event-log ingest and named analysis. Keeping DuckDB in its own process also keeps DuckDB's SQLite
extension out of the API process, so the server never opens one database file through two SQLite copies (ADR
0008). Deno Workers/threads are not substituted for that OS-process boundary.

## Run

Use a recent Deno 2 runtime (a current Deno 2 release is the intended target). The server's FFI binding needs
a system `libsqlite3` of 3.37.0 or newer; set `SQLITE_LIBRARY` to its path when the default search order picks
a different copy (vm-1 has two). The native DuckDB npm module needs local `node_modules` plus FFI permission.
`nodeModulesDir: auto` and the local demo's `-A` cover those requirements. Dependencies and the DuckDB
`sqlite` extension need network access on first use. This is native Deno, not a browser-WASM example or an
edge-runtime deployment.

From this directory:

```sh
# Terminal 1. Also starts the DuckDB reader process.
TRACE_SQL=1 deno task serve

# Terminal 2. Runs a complete game using the typed HTTP client.
deno task demo

# Same application API and handlers, independent app, all-JSON physical layout.
deno task demo tic-tac-toe-json

# Inspect compiler output without database drivers.
deno task explain > GENERATED_SQL.txt

# Core tests, API type checks, then the real multi-process integration test.
deno task test
deno task check
deno task integration
```

The service binds to `127.0.0.1:8000`. Default app/admin tokens are `dev-only`, deliberately unsuitable for
deployment. Set `PORT`, `DATA_DIR`, `COLUMN_APP_TOKEN`, `JSON_APP_TOKEN`, and `ADMIN_TOKEN` as needed. Do not
expose this service to untrusted users. `TRACE_SQL=1` logs every compiled SQL statement at `info`; bound
application values are included only when `LOG_INCLUDE_BIND_VALUES` is also set.

## App-facing client

Since IR-3 the handlers run **in the caller's process**: the client takes the functions object and the schema
as values, runs a query or mutation handler locally, and sends its database work to the backend as IR requests
(`POST /v1/ops`, [`ir/spec/v1.md`](ir/spec/v1.md)).

```ts
import { Effect } from "effect";
import { createClient } from "./sdk/client.ts";
import { functions } from "./app/functions.ts";
import { schema } from "./app/schema.ts";

// Single node: the Deno server's shim (app token), or agentd with a ticket (`token` may be a function).
const client = createClient({
  functions,
  schema, // in the database's layout: withStorage(schema, "json") for tic-tac-toe-json
  tenant: "local",
  app: "tic-tac-toe",
  endpoint: "http://127.0.0.1:8000/apps/tic-tac-toe", // agentd: "http://127.0.0.1:7070"
  token: "dev-only",
  // Multi-node instead of endpoint/token: locator: { url: "http://controld:7080", apiKey: "mansio_…" },
});
const { gameId } = await Effect.runPromise(
  client.mutation("createGame", { xPlayer: "alice", oPlayer: "bob" }),
);
const game = await Effect.runPromise(client.query("getGame", { gameId }));
await Effect.runPromise(
  client.mutation("makeMove", { gameId, playerId: "alice", cell: 0, expectedVersion: game!._version }),
);
```

Call sites keep the shape `client.query(name, args)` / `client.mutation(name, args)`; names, argument and
result types are inferred from the functions object. `createPromiseClient` takes the same options and returns
promises.

How a mutation runs: reads go out at once as query requests; writes are buffered (inserts get client-side
UUIDv7 ids; later reads in the same handler see the buffered writes); when the handler returns, one atomic
mutation request carries the writes plus **preconditions derived from the reads** (`get` → the version it saw,
or `exists: false`; a `find` is not guarded in v1). If another writer changed something the handler read, the
server answers `ConflictError` on a derived precondition and the client **re-runs the handler** (bounded, with
jitter). So **handlers must have no side effects beyond `db`**: no email, no external calls — they may run
more than once. One commit holds at most 64 operations (derived asserts + writes), else the call fails locally
with `RuleError`. App-raised errors (`makeMove`'s own `ConflictError` on a stale `expectedVersion`, a
`RuleError`) fail the handler locally and reach the caller as `RemoteError` with the status and tag the server
would have used; transport trouble that outlasts the retry policy and budget is `Unavailable`. Details:
`sdk/client.ts`, `sdk/recording.ts`, `sdk/transport.ts`.

Every call carries one `X-Trace-Id` on all its requests: the `traceId` argument if given, otherwise a
generated UUID. See "Trace propagation" below for what the server does with it.

`createLegacyClient(baseURL, appId, token)` is the pre-IR client (handlers run on the Deno server,
`/apps/:app/:kind/:name`); it stays until the Deno platform is removed (IR-6).

## Query language and semantics

Supported: `get`, `find` with top-level scalar equality, chained `where`, numeric `gt`, `sort`, `limit`,
`collect`, `first`, `count`, `countBy`; and `insert`, `patch`, `delete` within mutations.

Inside a handler with `db` in scope:

```ts
Effect.gen(function* () {
  const games = yield* db.find("games", { status: "playing" })
    .sort("_creationTime", "desc").limit(20).collect();
  const wins = yield* db.find("games", { status: "won" }).countBy("winner");
  return { games, wins };
});
```

The first operation produces a plan such as:

```json
{
  "kind": "select",
  "table": "games",
  "filters": [{ "field": "status", "op": "eq", "value": "playing" }],
  "order": { "field": "_creationTime", "direction": "desc" },
  "limit": 20
}
```

Only platform code converts it to SQL. Identifiers are schema-validated, values use bound parameters, and
array predicates/unknown fields fail explicitly. This is not a compiler for arbitrary JavaScript closures,
MongoDB's entire query language, or Convex's full API. No hidden fetch-everything-and-filter-in-JavaScript
fallback is provided.

All fields are required unless explicitly nullable; there is no separate missing/undefined field state. Arrays
are validated recursively and stored as JSON text. Optional fields, nested-object schemas, joins, array
predicates, MongoDB operators, projection, cursors, and search are not implemented. Collection reads default
to 100 results and are capped at 1000. `count` counts all matching documents regardless of limit; `countBy`
groups all matches and caps returned groups. Sorting adds `_id` as a stable tie-breaker and spells out null
ordering.

`_id`, `_creationTime`, and `_version` are platform-owned metadata; each patch increments `_version`. Typed
clients cannot patch metadata. Arguments and writes also receive runtime validation.

### Number and text contracts

- **Numbers.** A `number` field is a JS double. In the columns layout it is a REAL column. In the json layout
  the document is stored as `JSON.stringify` text, which writes an integral double at or beyond 2^53 as
  integer text; SQLite's `json_extract` would read that back as an exact INTEGER rather than the double it
  denotes (item 30). The SQLite compiler therefore wraps a json-layout number read in `CAST(... AS REAL)`, so
  filters, ordering, `countBy` keys and index expressions agree with the columns layout and with DuckDB's
  DOUBLE cast. Integer, boolean, text and metadata reads are unchanged. Known gaps, pinned by
  `tests/unit/number_dialects_test.ts`: a database created before item 30 keeps its old bare `json_extract`
  index on a json-layout number field (results stay correct; drop and recreate the `idx_*` to use it again),
  and the DuckDB bind path throws for integral numbers with `|x| >= 2^63` in either layout.
- **Embedded NUL.** Before FFI-04, `node:sqlite` truncated TEXT at U+0000 on read, so an accepted string could
  commit and read back as a different value; the FFI binding returns the whole string, but the rule stays.
  Item 31 rejects it before commit (ADR 0005): a string or enum field value (at any array depth), a
  `get`/`patch`/`delete` id or a filter value containing U+0000 fails with
  `SchemaError { reason: "type-mismatch", expected: "text without U+0000" }`, and an `IdGen` id containing it
  fails as a `StorageError`. Nothing runs, so nothing is written. Reading a row that already holds NUL text
  (written before the contract) fails with a `StorageError` instead of returning a truncated value. Both
  layouts and both `Storage` implementations apply the same rules.

## Two physical layouts, same logical schema

`tic-tac-toe` uses columns. Strings/enums become TEXT, integers/booleans INTEGER, numbers REAL, and board
arrays JSON-encoded TEXT. Generated tables use SQLite STRICT mode.

`tic-tac-toe-json` uses `_id`, `_creationTime`, `_version`, and one `doc TEXT` JSON document per row. The same
schema validates the JSON. Index declarations compile to SQLite JSON expression indexes. This is JSON text,
not SQLite's binary JSONB encoding, so SQLite's document-read compiler can extract fields from it.

In columns mode, `status = "playing"` targets the `status` column. In JSON mode the SQLite compiler uses
`json_extract(doc, '$.status')`. The result decoder returns the same TypeScript document shape in either
layout and normalizes safe-integer BIGINT results to JS numbers.

Schema descriptors are stored in each file. Changing a schema or storage mode on an existing file fails
explicitly. There is no automatic migration or live schema reload.

## Transaction and consistency contract

A mutation gets one per-app queue slot and one `BEGIN IMMEDIATE ... COMMIT` transaction. Its `db.get` and
`db.find` calls execute through SQLite **inside the same transaction**. The board update and inserted move are
atomic. If validation or game logic throws, both roll back. Concurrent move attempts are serialized, and
`expectedVersion` rejects a stale client attempt.

Public `query` handlers execute in the server process with a read-only SQLite connection to the app's file (or
the authenticated tenant's file). Each handler has its own connection and read transaction, without taking the
Storage writer semaphore. A separate semaphore caps concurrent document reads at 32. Handlers have independent
snapshots; connections close on success, failure, or interruption. A `count`, `countBy` or any `select` that
is not a primary-key lookup runs under `STATEMENT_DEADLINE_MS`: past the deadline the statement is interrupted
and the request fails with HTTP 504 (`StatementDeadlineError`). Primary-key reads and writes are never
interrupted. Ingest, analysis, and ping still use the supervised DuckDB reader child over IPC.

The mutation response returns the locally committed game immediately. DuckDB is never used to make
transactional write decisions. There is no second data copy that needs to catch up. This does not establish a
remotely replicated/linearizable database service; concurrent readers may legitimately see the state before an
overlapping write.

## Observability and the event log

Every process writes structured JSONL under `LOG_DIR/<role>/<date>/`. The reader owns ingest, retention and
live named analysis; the server reaches these through RPC and never imports DuckDB. The standalone `logs:*`
CLI opens the same derived file between reader writes. DuckDB permits one writer, so use the live admin
analysis route while the reader owns it. [docs/observability.md](docs/observability.md) describes the schema,
catalog, parameters and worked examples; [perf/README.md](perf/README.md) describes k6 scenarios and the
loader into `k6_points`.

`LOG_LEVEL=info` is the default; `TRACE_SQL=true` or debug level enables SQL events. Bind values stay hidden
unless `LOG_INCLUDE_BIND_VALUES=true`. Logging uses a dropping queue (`EVENT_QUEUE_CAPACITY=10000`) and
batched flushes (`EVENT_FLUSH_MS=250`); shutdown drains the sink. Retention defaults to gzip after 7 days,
delete after 30 days, 1 GiB total JSONL, and 64 MiB rotation. `LOG_GZIP_AFTER_DAYS`, `LOG_RETENTION_DAYS`,
`LOG_MAX_TOTAL_BYTES` and `LOG_MAX_FILE_BYTES` control those limits. Only closed, settled, fully ingested
files with matching committed watermarks may be swept; today's files and pinned tails may exceed the cap.
Retention does not prune the derived DuckDB file. The hourly ingest-then-sweep timer and admin ingest RPC use
the same coordinator; dry-run plans from already committed state without writing.

### Trace propagation (item 34)

- **Ingress.** The server accepts the caller's `X-Trace-Id` when it is 1-128 characters of `[A-Za-z0-9._:-]`.
  Anything else (absent, empty, too long, a space, a repeated header) is not an error: the request runs under
  a generated id. The id the request actually ran under is echoed in the response `X-Trace-Id` on every
  status.
- **Server fibers.** Each handler runs its whole dispatched Effect as a child span. The `http` event, the
  `handler` event (app invocations only: outcome `ok`, a typed failure, a defect or an interruption), the
  SQLite writer's `sql` and `tx` events (including rollback), backup phases and log records carry the trace
  id; handler-scoped ones also carry the handler's `spanId`.
- **Reader child.** `ReaderClient` captures the caller's trace when a call is submitted and writes it into
  that call's frame (`traceId`, a fresh `spanId`, the handler span as `parentSpanId`). The reader restores it
  as annotations, so its `sql`/`tx` events and logs share the trace id. Calls made outside any request (the
  startup ping, the ingest timer) send no trace properties and run under the reader's fallback
  `reader-<pid>-<requestId>`. A late reply to a timed-out call is logged under that call's trace.
- **k6 joins.** `perf/ingest_k6.ts` loads raw points into `k6_points`; `k6-vs-platform` compares overlapping
  time buckets with HTTP events. This is a bucket-level latency comparison, not a one-to-one request join.
- Tested end to end by `tests/unit/trace_e2e_test.ts` (real SDK, server and reader child).

### Runtime ingest and retention (items 32, 33, 35, 36)

- **Ingest.** The reader's `ingest` request runs the ingest-then-sweep coordinator against the persistent
  event-log file. The server never loads DuckDB: it sends the RPC every hour (first run one interval after
  startup) and on `POST /admin/logs/ingest` (admin token). The handle opens on the first ingest; a file that
  cannot be opened or has an incompatible schema fails that one RPC with `IngestError` and does not affect
  queries. The call shares `READER_CALL_TIMEOUT_MS`; a longer run is logged by the server as
  `ingest-sweep-failed` while the reader still commits.
- **Event schema v2.** Every typed event row carries `event_id` and joins exactly one `platform_events` row;
  `event_kind` is always the record's kind and `handler_kind` is the explicit query/mutation mapping; backup
  records carry `emitter`, `run_id` and `attempts` (ADR 0006). Ingest refuses an unversioned (v1) or foreign
  file; `upgradeEventLog` (`platform/eventlog/tables.ts`) rebuilds v1 from retained JSONL and is an explicit
  operator step with no CLI yet.
- **Retention follows ingest.** A JSONL file is gzipped or deleted only when it is closed (dated before today,
  not open, settled) and committed ingest accounts for all of it (ADR 0007). A file that fails the gate is
  kept, still counts toward `LOG_MAX_TOTAL_BYTES`, and is reported as blocked. Nothing in the server or reader
  starts a retention sweep on its own; it runs only inside the ingest RPC.
- **Limits.** `LOG_MAX_TOTAL_BYTES` bounds the JSONL tree only, and even there today's files and blocked files
  can exceed it. The derived DuckDB file is never pruned, so it grows without bound; the reply reports both
  budgets separately. Gzipped files are never re-ingested, so rebuilding the event log reconstructs only
  events whose JSONL is still retained. The DuckDB file is a derived cache: the JSONL is the source of truth.
  Server and reader must run in one timezone (they do, by inheriting one environment).

## Backups and restore

Manual backup, using the local object-store stand-in by default:

```sh
curl -X POST http://127.0.0.1:8000/admin/backup/tic-tac-toe \
  -H 'Authorization: Bearer dev-only'
```

For actual off-host backups, configure an S3-compatible bucket base URL and credentials on the server:

```sh
export S3_BASE_URL='https://YOUR_BUCKET.s3.us-east-1.amazonaws.com'
export AWS_REGION='us-east-1'
export AWS_ACCESS_KEY_ID='...'
export AWS_SECRET_ACCESS_KEY='...'
# AWS_SESSION_TOKEN is supported for temporary credentials.
# For a compatible path-style service, base URL includes /bucket-name.

BACKUP_INTERVAL_MS=60000 deno task serve
```

The credentials need object GET/PUT permissions. They are server-side only. The signer does not implement a
cloud SDK credential-provider chain or automatic refresh of environment-supplied temporary credentials. Supply
renewed credentials/restart as appropriate for this demo.

The flow is
`VACUUM INTO new-file -> PRAGMA quick_check -> SHA-256 -> immutable snapshot PUT -> immutable manifest PUT -> latest manifest PUT`.
It does not copy the live SQLite main file while ignoring the WAL. Snapshot creation serializes with local
mutations; upload runs after releasing the mutation queue. There is one backup in progress per app and no
overlapping periodic backup sweep.

Snapshots live under `apps/<appId>/<timestamp>-<uuid>.sqlite`, with a manifest alongside each snapshot.
`apps/<appId>/latest.json` is published after successful snapshot upload. A failed upload does not advance the
latest manifest. Each snapshot contains an entire app, not one row or one table.

To restore, **stop the service and restore to a new path**, without an existing database/WAL/SHM there. The
command reads `latest.json`, verifies the hash and integrity, and creates the destination without overwriting
an existing database. Run it with the same object-store environment as the server:

```sh
deno task restore tic-tac-toe ./restored-data/tic-tac-toe.sqlite
# Restore the other app separately if needed, then start with:
DATA_DIR=./restored-data deno task serve
```

The restore publishes a complete temporary file with a same-filesystem hard link. It is an offline operation,
not an automatic live-database replacement. Server startup does not automatically restore absent files; absent
app files are initialized empty.

**This is asynchronous backup, not high availability.** A successful mutation acknowledges local SQLite
commit, not S3 durability. A host loss can lose commits newer than the last completed snapshot. Backup
intervals do not guarantee a fixed RPO during upload delays or failures. There is no leader election,
replicated WAL, failover, incremental upload, snapshot retention policy, or live restore. (Log retention is
separate; see "Observability and the event log".) Full snapshots read the entire database into memory before
upload; replace that path before using large databases. Local `object-store/` is not off-host redundancy.

## Configuration

The mechanically mirrored options table still carries MT-02’s historical “reserved” description; `api-key` and
`better-auth` are now implemented as described above. `DATABASE_URL` may also be used by admin tenant
operations in off mode because the registry is acquired lazily.

`platform/config.ts` is the single env boundary: `ConfigLive` reads every option below once, at layer
construction, and fails with a typed `ConfigError` rather than at first use. Every option is declared exactly
once, in `platform/config.ts`'s options table — this section is checked against that table mechanically
(`tests/unit/config_test.ts`), so it always lists the current platform version's options. Empty-string env
values are treated the same as unset. `ConfigTest(overrides)` ignores the environment entirely and is for
tests.

| Option                      | Env var                        | Type                            | Default                                          | Description                                                                                                                                                                                                                                                                                 |
| --------------------------- | ------------------------------ | ------------------------------- | ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `port`                      | `PORT`                         | number                          | `8000`                                           | HTTP server port.                                                                                                                                                                                                                                                                           |
| `bindHost`                  | `BIND_HOST`                    | string                          | `127.0.0.1`                                      | HTTP server bind address.                                                                                                                                                                                                                                                                   |
| `maxRequestBytes`           | `MAX_REQUEST_BYTES`            | number                          | `65536`                                          | Request body cap in bytes; larger bodies are rejected before parsing.                                                                                                                                                                                                                       |
| `dataDir`                   | `DATA_DIR`                     | string                          | `./data`                                         | Root directory for per-app SQLite files, resolved to an absolute path.                                                                                                                                                                                                                      |
| `adminToken`                | `ADMIN_TOKEN`                  | string                          | `dev-only`                                       | Bearer token for the /admin/* routes.                                                                                                                                                                                                                                                       |
| `columnAppToken`            | `COLUMN_APP_TOKEN`             | string                          | `dev-only`                                       | Bearer token for the tic-tac-toe app (columns storage).                                                                                                                                                                                                                                     |
| `jsonAppToken`              | `JSON_APP_TOKEN`               | string                          | `dev-only`                                       | Bearer token for the tic-tac-toe-json app (json storage).                                                                                                                                                                                                                                   |
| `backupIntervalMs`          | `BACKUP_INTERVAL_MS`           | number                          | `0`                                              | Backup timer period in milliseconds. 0 disables the timer; otherwise must be finite and >= 1000.                                                                                                                                                                                            |
| `logDir`                    | `LOG_DIR`                      | string                          | `./logs`                                         | Root of the logs/<role>/<date>/ tree.                                                                                                                                                                                                                                                       |
| `logLevel`                  | `LOG_LEVEL`                    | enum(debug\|info\|warn\|error)  | `info`                                           | Minimum event level written to the log.                                                                                                                                                                                                                                                     |
| `logRetentionDays`          | `LOG_RETENTION_DAYS`           | number                          | `30`                                             | Days a log file is kept before deletion.                                                                                                                                                                                                                                                    |
| `logGzipAfterDays`          | `LOG_GZIP_AFTER_DAYS`          | number                          | `7`                                              | Days a log file is kept uncompressed before gzip.                                                                                                                                                                                                                                           |
| `logMaxTotalBytes`          | `LOG_MAX_TOTAL_BYTES`          | number                          | `1073741824`                                     | Total bytes the log tree may occupy before the oldest files are swept.                                                                                                                                                                                                                      |
| `logMaxFileBytes`           | `LOG_MAX_FILE_BYTES`           | number                          | `67108864`                                       | Bytes a single log file may reach before it rolls over.                                                                                                                                                                                                                                     |
| `logIncludeBindValues`      | `LOG_INCLUDE_BIND_VALUES`      | boolean                         | `false`                                          | Include SQL bind values in sql events. Off by default: bind values are application data. Accepts true/false, 1/0, yes/no.                                                                                                                                                                   |
| `traceSql`                  | `TRACE_SQL`                    | boolean                         | `false`                                          | Emit an event for every compiled SQL statement. Accepts true/false, 1/0, yes/no.                                                                                                                                                                                                            |
| `eventQueueCapacity`        | `EVENT_QUEUE_CAPACITY`         | number                          | `10000`                                          | Depth of the in-process dropping queue that buffers events before they are flushed to disk.                                                                                                                                                                                                 |
| `eventFlushMs`              | `EVENT_FLUSH_MS`               | number                          | `250`                                            | Interval in milliseconds between batched event-queue flushes.                                                                                                                                                                                                                               |
| `s3BaseUrl`                 | `S3_BASE_URL`                  | optional-string                 | `(unset)`                                        | Base URL of the S3 (or S3-compatible) bucket for backups. Absent selects the local object-store stand-in.                                                                                                                                                                                   |
| `awsRegion`                 | `AWS_REGION`                   | string                          | `us-east-1`                                      | AWS region used to sign S3 requests.                                                                                                                                                                                                                                                        |
| `awsAccessKeyId`            | `AWS_ACCESS_KEY_ID`            | optional-string                 | `(unset)`                                        | AWS access key id. Required when S3_BASE_URL is set.                                                                                                                                                                                                                                        |
| `awsSecretAccessKey`        | `AWS_SECRET_ACCESS_KEY`        | optional-string                 | `(unset)`                                        | AWS secret access key. Required when S3_BASE_URL is set.                                                                                                                                                                                                                                    |
| `awsSessionToken`           | `AWS_SESSION_TOKEN`            | optional-string                 | `(unset)`                                        | AWS session token, for temporary credentials.                                                                                                                                                                                                                                               |
| `localObjectDir`            | `LOCAL_OBJECT_DIR`             | string                          | `./object-store`                                 | Local object-store directory used when S3_BASE_URL is unset, resolved to an absolute path.                                                                                                                                                                                                  |
| `eventlogDuckdbPath`        | `EVENTLOG_DUCKDB`              | string                          | `<DATA_DIR>/eventlog.duckdb`                     | Persistent DuckDB file backing the ingested event log, resolved to an absolute path. Defaults to eventlog.duckdb inside DATA_DIR.                                                                                                                                                           |
| `readerCallTimeoutMs`       | `READER_CALL_TIMEOUT_MS`       | number                          | `30000`                                          | Per-call deadline in milliseconds for a request to the reader process; a call with no reply by then fails with HTTP 502. Must be finite and > 0.                                                                                                                                            |
| `sqliteLibrary`             | `SQLITE_LIBRARY`               | optional-string                 | `(unset)`                                        | Path or soname of the system libsqlite3 loaded by the FFI binding; must be libsqlite3 >= 3.37.0. vm-1 has two copies: 3.45.1 under /usr/lib/x86_64-linux-gnu/libsqlite3.so.0 and 3.53.4 under /usr/local/lib. Unset, the binding's default list applies, which tries the distro path first. |
| `statementDeadlineMs`       | `STATEMENT_DEADLINE_MS`        | number                          | `5000`                                           | Deadline in milliseconds after which an unbounded statement (any select that is not a primary-key lookup, `count`, `countBy`) is interrupted via sqlite3_interrupt and fails with a typed deadline error (HTTP 504 once FFI-05 lands). Must be finite, > 0 and < READER_CALL_TIMEOUT_MS.    |
| `statementInterruptGraceMs` | `STATEMENT_INTERRUPT_GRACE_MS` | number                          | `1000`                                           | Milliseconds to wait after the interrupt for the statement to return; if it has not, an error-level lifecycle event is emitted and the connection is poisoned. Must be finite and > 0.                                                                                                      |
| `tenancyMode`               | `TENANCY`                      | enum(off\|api-key\|better-auth) | `off`                                            | Multitenancy mode. `off` preserves the pre-tenancy single-database, app-token behavior; `api-key` and `better-auth` are reserved for later multitenancy items.                                                                                                                              |
| `databaseUrl`               | `DATABASE_URL`                 | string                          | `postgres://mansio:mansio@localhost:5432/mansio` | Postgres connection URL for the multitenancy central database (tenant registry). Unused while tenancy mode is `off`.                                                                                                                                                                        |
| `tenantsDir`                | `TENANTS_DIR`                  | string                          | `<DATA_DIR>/tenants`                             | Root directory for per-tenant SQLite files, resolved to an absolute path. Defaults to tenants inside DATA_DIR.                                                                                                                                                                              |
| `tenantRouter`              | `TENANT_ROUTER`                | enum(local\|control-plane)      | `local`                                          | Router kind selecting tenant storage locations. Only `local` exists today; `control-plane` is reserved for the future.                                                                                                                                                                      |
| `betterAuthSecret`          | `BETTER_AUTH_SECRET`           | optional-string                 | `(unset)`                                        | Secret used to verify Better Auth session tokens. No default; required when TENANCY is `better-auth`.                                                                                                                                                                                       |
| `betterAuthUrl`             | `BETTER_AUTH_URL`              | string                          | `http://localhost:3000`                          | Base URL of the Better Auth instance issuing session tokens.                                                                                                                                                                                                                                |

## Deliberate limits

This is a local, trusted-application prototype. Two database files are not a hostile-tenant security sandbox.
Handlers are statically registered modules; uploaded code execution, per-tenant process isolation, deployment
tooling, row-level authorization, and secret isolation are not implemented. Better Auth provides headless
email/password sessions and single-tenant membership resolution. `playerId` is supplied by the demo caller,
not verified identity. App-level shared bearer tokens are only a basic routing/access check. Client mutations
have no automatic retry or idempotency key, so a network timeout can leave an unknown commit outcome.

Legacy mode uses one writer connection per app; tenancy leases writers from a bounded per-app handle cache.
The reader child remains shared for event-log ingest and named analysis. This demonstrates the design without
establishing capacity. Primary-key reads, writes and snapshot creation run synchronously on the API event loop
and cannot be interrupted; every other document read runs under `STATEMENT_DEADLINE_MS` (default 5 s) and is
interrupted at the deadline. There are queue-size and result-size caps but no overload, restart, or
resource-governance policy. Avoid network I/O or long-running work inside mutation handlers. All long-lived
processes must remain on one local filesystem; object storage is a backup target, not a mounted live SQLite
volume.

Reactive subscriptions, automatic dependency tracking, optimistic UI, durable actions/jobs, automatic
application-schema migrations, a complete production authentication policy, and a full query optimizer are not
included. Document queries use in-process SQLite; DuckDB remains responsible for the event log and named
analysis. This split reuses SQLite SQL for ordinary document reads and keeps the supervised reader child
load-bearing for ingest and analysis. Benchmark actual workloads rather than assuming every declared SQLite
index accelerates every query.

## Architecture diagrams site

[`pages/`](pages/README.md) is an interactive C4 viewer of this repo's architecture, for designing features: N
architectures in a dropdown, the current one by default. The current one is what runs on `main` today
(converted from the audited Markdown diagrams in [`docs/architecture/`](docs/architecture/README.md)); the
others are design proposals (today the statement lanes and tenant placement scale-out; the FFI execution layer
has landed and is drawn in the current one). One architecture is shown at a time, with the dropdown and the
diagram navigation in a bar pinned to the top of the page. It is a separate npm project (Node 22.23.2); run it
with `cd pages && npm ci && npm run dev`, and see its README for checks and dataset authoring. It renders
through the published [`c4-sdk`](https://www.npmjs.com/package/c4-sdk) npm package. It will be served at
<https://nickstrad.github.io/mansio/> once GitHub Pages is enabled for the repository.

## Primary references

- Deno SQLite API: https://docs.deno.com/api/node/sqlite/
- Deno native npm/Node-API compatibility: https://docs.deno.com/runtime/fundamentals/node/
- DuckDB SQLite extension (native-library isolation rationale):
  https://duckdb.org/docs/current/core_extensions/sqlite
- DuckDB Node Neo API: https://duckdb.org/docs/current/clients/node_neo/overview
- DuckDB JSON functions: https://duckdb.org/docs/current/data/json/json_functions
- SQLite VACUUM INTO guarantees: https://www.sqlite.org/lang_vacuum.html
- aws4fetch signer: https://github.com/mhart/aws4fetch
