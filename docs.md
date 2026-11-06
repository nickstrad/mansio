# mansio platform user guide

Write TypeScript schemas and named document handlers; the platform compiles document operations to SQL. This
guide uses the default app-token mode (`TENANCY=off`) and the two shipped tic-tac-toe apps. Read the README's
[Deliberate limits](README.md#deliberate-limits) before choosing it for a deployment.

```text
Typed client / browser
       | named handler + JSON arguments
       v
Deno server: HTTP + in-process SQLite document reads/writes + backups
       |                        |
       | libsqlite3 (FFI)      | JSON-lines RPC over stdin/stdout
       v                        v
Per-app SQLite files      Deno reader: DuckDB ingest + named analysis
       |                        |
       | consistent snapshot    | ingest JSONL logs
       v                        v
Local stand-in or S3         Derived event-log DuckDB file
```

The server starts the reader. Both processes share one machine and local filesystem; queries read the same
SQLite files that mutations write. Follow the shell blocks in order, in one Bash session from the root of a
fresh clone. They use temporary data, logs and local backup objects. The TypeScript examples are complete
modules; save them at the repo root if you want to check them with `deno check`.

## Installation

Use Deno **2.9.5**, the version verified for this repository. Put its executable on your shell's path:

```sh
export PATH="$HOME/.deno/bin:$PATH"
deno --version
export DATA_DIR="$(mktemp -d)"
export LOG_DIR="$DATA_DIR/logs"
export LOCAL_OBJECT_DIR="$DATA_DIR/objects"
export PORT=18029
export TENANCY=off
export ADMIN_TOKEN=dev-only
export COLUMN_APP_TOKEN=dev-only
export JSON_APP_TOKEN=dev-only
export BACKUP_INTERVAL_MS=0
unset S3_BASE_URL AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
```

Keep the committed `deno.lock`: it pins dependency resolution. `deno.json` enables automatic local npm
modules, including the native DuckDB package. The tasks supply the permissions needed by the engines. The
first run needs network access for npm packages and DuckDB's `sqlite` extension; a cached run is not proof
that a new host can download them. This is a native Deno deployment, not a browser runtime.

## Configuration

[README → Configuration](README.md#configuration) is the single source of truth for all options. For a first
run, understand these:

- `PORT` (default `8000`) selects the HTTP port; the examples use 18029 to keep their server separate.
- `DATA_DIR` (default `./data`) holds per-app SQLite files. Paths are resolved to absolute paths.
- `COLUMN_APP_TOKEN` (default `dev-only`) authenticates the columns-layout app.
- `JSON_APP_TOKEN` (default `dev-only`) authenticates the JSON-layout app.
- `ADMIN_TOKEN` (default `dev-only`) authenticates admin endpoints.
- `BACKUP_INTERVAL_MS` (default `0`) disables the backup timer at zero. A nonzero period must be finite and at
  least 1000 milliseconds.
- `LOG_DIR` (default `./logs`) selects the structured log tree.
- `SQLITE_LIBRARY` (unset) names the system `libsqlite3` the FFI binding loads. Unset, the binding tries its
  default search order. It must be SQLite 3.37.0 or newer, and a startup `sqlite-library` lifecycle event
  names the path and version it loaded.
- `STATEMENT_DEADLINE_MS` (default `5000`) is the deadline for an unbounded read (a `select` that is not a
  primary-key lookup, `count` or `countBy`). It must be below `READER_CALL_TIMEOUT_MS`.
- `STATEMENT_INTERRUPT_GRACE_MS` (default `1000`) is how long an interrupted statement may take to return. If
  it does not, the server logs an error-level `lifecycle` event and stops using that connection.

Configuration is decoded once during layer construction. Invalid values or inconsistent combinations fail
startup with a typed `ConfigError`, before serving requests. Empty strings are treated as unset. Set real,
distinct tokens before deployment; `dev-only` is for local development.

## Core concepts

A document has schema-defined fields plus platform-owned `_id`, `_creationTime` and `_version`. A patch
increments `_version`; application writes cannot set these metadata fields. Fields are required unless
explicitly nullable. Arrays are recursively validated; there is no separate missing-field state.

Documents belong to **collections**. The code calls these tables: `defineTable` declares one and
`db.find("games", { status: "playing" })` selects from it. This guide uses “collection” throughout.

The `columns` layout stores scalar fields in typed SQLite STRICT columns and arrays as JSON text. The `json`
layout stores metadata columns and one `doc TEXT` JSON body per row, with JSON expression indexes. Both use
the same schema and return the same TypeScript document shape. See
[the layout reference](README.md#two-physical-layouts-same-logical-schema).

A **query** runs in the server process against the app's SQLite file (or the authenticated tenant's file),
using a fresh read-only connection and read transaction. A concurrency semaphore caps active document reads at
32. An unbounded read that runs past `STATEMENT_DEADLINE_MS` is interrupted and the request returns HTTP 504
with a `Statement deadline exceeded` message; primary-key reads and writes are not interrupted. A **mutation**
also runs in the server process, serialized per app, in one SQLite `BEGIN IMMEDIATE` transaction. Its reads
use SQLite inside that transaction; its writes commit together or roll back on failure/interruption. Do not
retain a handler's database context after the handler finishes.

A successful mutation response follows local commit. Queries read the live file rather than an asynchronously
refreshed replica; a query overlapping a write may see earlier state. Event-log ingest and named analysis
continue in the supervised DuckDB reader child over IPC. See the
[transaction contract](README.md#transaction-and-consistency-contract) for the precise scope.

The deployment registry is static. Adding an app means editing `platform/registry.ts` and `platform/config.ts`
to register its schema, handlers, layout and token: there is no plugin loading. HTTP callers choose registered
app IDs and named handlers, never database paths or raw SQL.

## SDK walkthrough

This schema mirrors the shipped app's two collections. Index declarations are part of the schema; changing a
schema or layout on an existing data file causes startup schema drift, not migration.

```ts
import { defineSchema, defineTable, v } from "./sdk/schema.ts";

const mark = v.enum("X", "O");
export const schema = defineSchema({
  games: defineTable({
    xPlayer: v.string(),
    oPlayer: v.string(),
    board: v.array(v.nullable(mark), 9),
    turn: mark,
    status: v.enum("playing", "won", "draw"),
    winner: v.nullable(mark),
  }).index("by_status", ["status"]),
  moves: defineTable({
    gameId: v.string(),
    playerId: v.string(),
    mark,
    cell: v.integer(),
    moveNumber: v.integer(),
  }).index("by_game", ["gameId", "moveNumber"]),
});
```

Here is a complete handler module using the landed `app/schema.ts`. `Effect.gen` describes the work; `yield*`
composes database operations and typed failures. The platform runs the Effect in the appropriate process and
transaction. `validatePlayers` returns an Either with `RuleError`, which can also be yielded. These
declarations illustrate authoring; they do not install handlers into a running server.

```ts
import { Effect } from "effect";
import { defineFunctions } from "./sdk/functions.ts";
import { v } from "./sdk/schema.ts";
import { schema } from "./app/schema.ts";
import { validatePlayers } from "./app/rules.ts";

const { query, mutation } = defineFunctions(schema);
export const functions = {
  listGames: query({
    args: { status: v.enum("playing", "won", "draw") },
    handler: ({ db }, { status }) =>
      Effect.gen(function* () {
        return yield* db.find("games", { status })
          .sort("_creationTime", "desc").limit(20).collect();
      }),
  }),
  createGame: mutation({
    args: { xPlayer: v.string(), oPlayer: v.string() },
    handler: ({ db }, { xPlayer, oPlayer }) =>
      Effect.gen(function* () {
        yield* validatePlayers(xPlayer, oPlayer);
        const gameId = yield* db.insert("games", {
          xPlayer,
          oPlayer,
          board: Array(9).fill(null),
          turn: "X",
          status: "playing",
          winner: null,
        });
        return { gameId };
      }),
  }),
};
```

The typed client infers names, argument types and results from a type-only handler import. Calls return
Effects; run them at your program's boundary. Run this module with `deno run --allow-net --allow-env` after
the next section starts the server. It creates and reads a game, then deliberately exercises a rule failure.
`Effect.catchTag` handles the client's typed `RemoteError`; inspect `remoteTag` for the server error tag.
Network failures and the client's 30-second timeout are defects, not `RemoteError`.

```ts
import { Effect } from "effect";
import { createClient } from "./sdk/client.ts";
import type { functions } from "./app/functions.ts";

const client = createClient<typeof functions>(
  `http://127.0.0.1:${Deno.env.get("PORT") ?? "8000"}`,
  "tic-tac-toe",
  Deno.env.get("COLUMN_APP_TOKEN") || "dev-only",
);
const program = Effect.gen(function* () {
  const { gameId } = yield* client.mutation("createGame", {
    xPlayer: "alice",
    oPlayer: "bob",
  }, "guide-create");
  const game = yield* client.query("getGame", { gameId }, "guide-read");
  console.log(game);
  yield* client.mutation("createGame", { xPlayer: "alice", oPlayer: "alice" }).pipe(
    Effect.catchTag("RemoteError", (error) =>
      Effect.sync(() => {
        if (error.remoteTag !== "RuleError") throw error;
        console.log(`Expected rejection: ${error.status} ${error.remoteTag}`);
      })),
  );
});
await Effect.runPromise(program);
```

## Running the server

The two registered apps are `tic-tac-toe` (columns) and `tic-tac-toe-json` (JSON). In default app-token mode
they have independent SQLite files beneath `DATA_DIR`, with the same handlers. The server binds loopback by
default and emits a configuration warning when any token remains `dev-only`.

Start it in the background and wait for the health endpoint, which pings the reader and lists apps:

```sh
deno task serve >"$DATA_DIR/server-output.txt" 2>&1 &
guide_server_pid=$!
guide_ready=false
for guide_attempt in $(seq 1 120); do
  if curl -fsS "http://127.0.0.1:$PORT/health" >"$DATA_DIR/health.json"; then
    guide_ready=true
    break
  fi
  if ! kill -0 "$guide_server_pid" 2>/dev/null; then break; fi
  sleep 1
done
if [ "$guide_ready" != true ]; then
  cat "$DATA_DIR/server-output.txt"
  exit 1
fi
cat "$DATA_DIR/health.json"
BASE_URL="http://127.0.0.1:$PORT" TOKEN="$COLUMN_APP_TOKEN" deno task demo
BASE_URL="http://127.0.0.1:$PORT" TOKEN="$JSON_APP_TOKEN" deno task demo tic-tac-toe-json
```

Successful HTTP responses wrap the value as `{ result }`; failures use `{ error: { tag, message } }`. The
exhaustive mapping in `core/errors.ts:toHttpStatus` is:

| Status | Error tags                                                                                                                                                                                        |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 400    | MalformedTenantId, SchemaError, CompileError, RuleError, HandlerKindMismatch                                                                                                                      |
| 401    | AuthError                                                                                                                                                                                         |
| 403    | TenantDeactivated                                                                                                                                                                                 |
| 404    | UnknownTenant, RouteNotFound, UnknownAppError, UnknownHandlerError, NotFoundError, UnknownAnalysisQuery                                                                                           |
| 409    | ConflictError, BackupAlreadyRunning                                                                                                                                                               |
| 413    | RequestTooLarge                                                                                                                                                                                   |
| 500    | DecodeError, ContextExpiredError, NonSerializableResultError, StorageError, ReaderError, QueryWriteAttemptError, InvalidKeyError, BackupError, SnapshotIntegrityError, IngestError, AnalysisError |
| 502    | IpcError, ReaderExitedError, ObjectStoreError                                                                                                                                                     |
| 503    | QueueFullError                                                                                                                                                                                    |

Tenant authentication normalizes refused credentials, including deactivated credentials, to 401. Startup
`ConfigError` and `SchemaDriftError` stop startup rather than producing HTTP responses. Defects produce 500
with tag `Defect`. For 5xx failures, the response hides internal details and points to a trace ID in the
server log.

The Effect port changed stale-version conflicts from blanket 400 to **409**, and queue saturation to **503**.
On a conflict, refresh state before another move; retries under load need application-level idempotency. An
already-running backup is also 409; a body exceeding the configured cap is 413.

## Backups and restore

Request a whole-app snapshot with the admin token:

```sh
curl -fsS -X POST "http://127.0.0.1:$PORT/admin/backup/tic-tac-toe" \
  -H "Authorization: Bearer ${ADMIN_TOKEN:-dev-only}"
```

The backup timer is disabled at zero; setting `BACKUP_INTERVAL_MS=60000` before starting the server requests a
sweep every minute. Snapshots serialize with mutations; upload happens after releasing the mutation queue.
There is one backup in progress per app and no overlapping periodic sweep.

The flow is `VACUUM INTO` a new file, integrity check, SHA-256, immutable snapshot upload, immutable manifest
upload, then publication of `apps/<appId>/latest.json`. Restore uses that latest manifest. An upload failure
does not advance it. The default local stand-in uses `LOCAL_OBJECT_DIR`; the setup above places it in the
temporary directory. It is not off-host redundancy.

For S3, set `S3_BASE_URL`, `AWS_REGION`, `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` before starting or
restarting the server. Temporary credentials can include `AWS_SESSION_TOKEN`. The bucket credentials need
object GET/PUT permissions; there is no credential-provider chain or automatic credential refresh. See
[the backup reference](README.md#backups-and-restore) for endpoint conventions.

**This is asynchronous backup, not HA.** A mutation acknowledges local SQLite commit, not S3 durability. Host
loss can lose writes newer than the last completed snapshot. Upload delays/failures prevent a fixed
recovery-point guarantee. There is no replicated WAL, failover, incremental backup or snapshot retention
policy. Full snapshots are read into memory before upload.

Restore is offline: stop the server and restore to a new path with no existing database, WAL or SHM. The
command checks the manifest, byte count, checksum and SQLite integrity and refuses overwrite. Leave the server
stopped for the remaining offline log commands; run the client module above before this step.

```sh
kill -TERM "$guide_server_pid"
wait "$guide_server_pid"
deno task restore tic-tac-toe "$DATA_DIR/restored/tic-tac-toe.sqlite"
```

To serve restored data, set `DATA_DIR` to that `restored` directory on the next startup. Restore the other app
separately if needed: absent app files initialize empty; startup does not restore automatically. Do not
replace a live database with a restored file.

## Observability

Structured JSONL lives under `logs/<role>/<date>/`, rooted at `LOG_DIR`, with hourly/size-rolled files. Server
and reader write separate files. JSONL is the source of truth; the reader ingests it into the derived
`EVENTLOG_DUCKDB` file. See [docs/observability.md](docs/observability.md) for table schemas, parameters and
retention rules, and [docs/testing.md](docs/testing.md) and [perf/README.md](perf/README.md) for correctness
checks and k6 scenarios.

While the server runs, `POST /admin/logs/ingest` with the admin token ingests then sweeps, and
`POST /admin/logs/analysis/:name` runs a named query with a flat JSON object of scalar parameters. For
example, the path `/admin/logs/analysis/trace` takes `{ "traceId": "guide-read" }`. The server also requests
ingest then sweep hourly, first one hour after startup. The reader owns the DuckDB file; use these admin
endpoints while it holds the file, rather than competing offline CLI opens.

After stopping the server as above, these commands ingest its final events and query the catalog:

```sh
deno task logs:ingest
deno task logs:query handler-throughput --format json
deno task logs:query trace --param traceId=guide-read --format json
deno task logs:sweep --dry-run
```

`logs:sweep` without `--dry-run` ingests and seals torn tails before retention. Only closed, settled, fully
ingested files with matching committed watermarks can be gzipped or deleted. Today's files and blocked files
can exceed the JSONL cap. The derived DuckDB file is never pruned and grows independently.

The named analysis queries are:

- `deadline-hits-per-handler-hour`: statement deadline hits by app, handler, UTC hour and execution path.
- `slowest-sql`: SQL p50/p95/p99 by engine, statement kind and collection.
- `plan-shapes`: compiled SQL texts, frequency and latency for investigating plans and indexes.
- `error-rate-by-handler`: request counts, error rates and error tags by app and handler.
- `mutation-vs-query-latency`: writer versus reader latency distributions over time.
- `handler-throughput`: requests per minute per handler, with status breakdown.
- `queue-depth`: queue high-water marks, rejections and dropped events over time.
- `tx-outcomes`: commits, rollbacks, interruptions and queue wait times.
- `backup-history`: backup phases, duration, bytes, failures and gaps between successes.
- `trace`: an ordered timeline for one trace ID across both processes and k6.
- `k6-vs-platform`: k6 client p95 versus server handler p95 per ten-second scenario bucket.

To investigate a failure, keep the response's `X-Trace-Id`, ingest logs and run the `trace` query with that
ID. The client can supply an explicit ID as its third call argument. The server accepts 1–128 characters from
`[A-Za-z0-9._:-]`, otherwise replaces the ID, and echoes the chosen ID on responses. Reader RPC carries the
trace and child-span relationship across processes. Enable `TRACE_SQL=true` before startup to see compiled
statements; bind values remain hidden unless `LOG_INCLUDE_BIND_VALUES` is enabled. `k6-vs-platform` is a
bucket-level comparison, not a one-to-one request join.

## Troubleshooting

Each entry follows symptom → cause → fix:

- **504 `Statement deadline exceeded`** → an unbounded read (a non-primary-key select, `count` or `countBy`)
  ran past `STATEMENT_DEADLINE_MS` and was stopped → find the trace in the response's `X-Trace-Id`, then run
  `deno task logs:ingest` and `deno task logs:query trace --param traceId=<id>`;
  `deno task logs:query deadline-hits-per-handler-hour` lists the handlers that hit it. Narrow the read, or
  raise the deadline if the work is legitimate (it must stay below `READER_CALL_TIMEOUT_MS`).
- **Startup fails with a `StorageError` (operation `open`) that names libsqlite3** → no usable `libsqlite3`
  was found, or the one found is older than 3.37.0 or was built without threads → install a current
  libsqlite3, set `SQLITE_LIBRARY` to its path, and confirm the server runs with FFI permission (the repo's
  tasks already pass `-A`).
- **`deno` not found** → its bin directory is missing from the shell path → use the Installation path export
  and confirm `deno --version`; install Deno 2.9.5 if absent.
- **`ConfigError` at startup with `BACKUP_INTERVAL_MS=500`** → a nonzero interval is below 1000 ms → use zero
  to disable it or a finite value at least 1000.
- **`ConfigError` with `S3_BASE_URL`** → credentials are missing → supply both `AWS_ACCESS_KEY_ID` and
  `AWS_SECRET_ACCESS_KEY`, or unset the base URL to select the local stand-in.
- **DuckDB extension download failure** → first-use network access or extension installation failed → allow
  the host to download the `sqlite` extension, check the reader log and retry startup, ingest or analysis.
- **Schema drift on an existing data file** → stored schema/layout differs from the declarations → stop the
  service and use a fresh data directory for a disposable run, or deliberately migrate data offline. There is
  no automatic migration; preserve existing data before changing it.
- **401** → bearer token is absent or wrong → use the selected app's token for app routes and the admin token
  for admin routes; tenant modes require their own credentials.
- **404** → route, app, handler, document or analysis name is unknown → verify the exact registered name,
  endpoint method and document ID; check whether a handler returned `NotFoundError`.
- **409** → stale expected version or an overlapping backup → re-read state before a new move, or wait for the
  current backup to finish before requesting another.
- **413** → body exceeds `MAX_REQUEST_BYTES` → reduce the payload or deliberately raise the cap in
  configuration and restart.
- **503** → a bounded writer/reader queue is full → reduce concurrency and retry with backoff and
  application-level idempotency; inspect queue-depth analysis.
- **Reader exited / 502** → reader process or IPC failed (object-store failures also map to 502) → inspect the
  trace and lifecycle logs. A reader exit makes the server shut down and exit 1; correct the cause and restart
  the server, or let your supervisor restart both processes together.
- **Port already in use / `ListenError`** → another listener owns the address → choose an unused `PORT` or
  stop the conflicting listener you own, then restart.
- **Offline logs command fails with a lock error** → the live reader owns the derived DuckDB file → use admin
  ingest/analysis while serving, or stop the server before running offline commands.
