# ADR 0002 — Error taxonomy: tagged classes, structured payloads, one status mapping

- **Status:** accepted
- **Date:** 2026-10-10
- **Deciders:** the plan at `docs/plans/completed/effect-port-and-observability.md` §1.3; recorded by item 02
- **Related:** [ADR 0001 — Effect port](0001-effect-port.md)
- **Implements:** `core/errors.ts`, asserted by `tests/errors_exhaustiveness.ts`

## Context

Every failure in the repo today is `throw new Error("some message")` — 51 `throw` sites plus 5
`new Error(...)` values constructed for rejections — and `platform/server.ts:64-67` catches all of them and
answers HTTP 400 with the message string. Three consequences:

- **Statuses are wrong.** A stale `_version`, a full writer queue, an oversize body and a misspelled enum are
  all 400. A client cannot tell "you sent nonsense" from "retry in a moment".
- **Handling is string matching.** `tests/core_test.ts` already asserts on seven message regexes
  (`/Game is over/`, `/Unknown field/`, `/Not your turn/`, `/Invalid move/`, `/expected integer/`,
  `/refuses automatic migration/`, `/Array predicates/`). Any production recovery logic would have to do the
  same.
- **Nothing is analysable.** The event log in workstreams 4 and 5 wants a stable `error_tag` column to group
  by. A formatted message with interpolated ids is the opposite of that: unbounded cardinality and no
  structure.

## Decisions

### 1. `Data.TaggedError`, one class per failure mode

`core/errors.ts` defines 32 classes with `Data.TaggedError`. Each is a real `Error` subclass, so it keeps a
stack trace when it escapes as a defect, and each is `yield*`-able inside `Effect.gen`:

```text
if (game._version !== expectedVersion) {
  return yield* new ConflictError({ expected: expectedVersion, actual: game._version });
}
```

### 2. Structured fields, never a formatted message

No class defines a `message`. They carry the data that _would have been_ interpolated:
`SchemaError({ reason, path, expected, actual })`, not `` `${path}: expected ${kind}` ``.

Rendering a human string is the consumer's job, in exactly three places:

- `sdk/schema.ts`'s `validateOrThrow` / `parseObjectOrThrow` wrappers (item 04), which reproduce today's exact
  strings so the seven asserted regexes keep matching.
- the HTTP body `{ error: { tag, message } }` (item 22), which keeps the `error` key `sdk/client.ts:14`
  already reads.
- `core/events.ts`'s `fromPlatformError(e) -> { errorTag, errorDetail }` (item 07), which is what gives the
  event log its stable, low-cardinality `error_tag` column.

Later items that must preserve an asserted message string find it in the mapping table below.

### 3. Families share a tag and discriminate on `reason`

Minting a tag per message would produce ~50 tags and a `toHttpStatus` nobody reads. Instead, related failures
share a tag and carry a `reason` literal: `SchemaError` has nine reasons, `CompileError` seven, `IpcError`
seven. Analysis groups by `error_tag` and drills into `attrs.reason`. The reason vocabularies are exported
types (`SchemaFailure`, `CompileFailure`, `DecodeFailure`, `IpcFailure`, `ConfigFailure`, `KeyFailure`,
`RestoreFailure`), so a later item cannot invent a reason string.

### 4. Payloads never carry secrets

`AuthError` records _that_ a token was missing, malformed or wrong, and for which scope — never the presented
token. `ConfigError` records the env var's name and why it was rejected, never its value. `SchemaError.actual`
and `DecodeError.actual` do carry rejected values, because that is the point of them; redaction of those is
`core/events.ts`'s unconditional job for bearer tokens, `authorization` headers and S3 credentials, and is
policy-gated (`logging.includeBindValues`) for SQL bind values.

### 5. One status mapping, total by construction

`toHttpStatus(e: PlatformError): number` is a `switch` over `_tag` whose `default` branch narrows to `never`
via Effect's `Function.absurd`. Adding a member to `PlatformError` without adding a case is a type error,
verified at `deno task check` time.

`tests/errors_exhaustiveness.ts` is a compile-only file in the `check` entry points that restates the mapping
as a `Record<PlatformError["_tag"], number>` (exhaustive by construction), freezes the tag inventory, and
asserts with `@ts-expect-error` that a tag outside the union is rejected. All three guards were verified to
fail the build when a tag is added without a case.

### 6. Errors that are never served are kept out of `PlatformError`

Per the plan's `ConfigError -> fails at startup, never served`, the union `toHttpStatus` ranges over contains
only what can reach an HTTP response. Four classes are deliberately excluded, so no status has to be invented
for them:

| Union          | Members                           | Where it surfaces                                                    |
| -------------- | --------------------------------- | -------------------------------------------------------------------- |
| `StartupError` | `ConfigError`, `SchemaDriftError` | fails `ConfigLive` / `StorageLive` acquire; the process never serves |
| `CliError`     | `RestoreError`                    | `scripts/restore.ts`: stderr plus a non-zero exit                    |
| `ClientError`  | `RemoteError`                     | raised in the _caller's_ process by `sdk/client.ts`                  |

`AnyError = PlatformError | StartupError | CliError | ClientError` is the full set, for `core/events.ts`'s
`error_tag` column.

## The tag union and its payloads

### Request and plan errors (pure, raised from `core/`)

| Tag            | Payload                                               | Status |
| -------------- | ----------------------------------------------------- | ------ |
| `SchemaError`  | `{ reason: SchemaFailure; path; expected?; actual? }` | 400    |
| `CompileError` | `{ reason: CompileFailure; field?; table?; detail? }` | 400    |
| `DecodeError`  | `{ reason: DecodeFailure; table?; field?; actual? }`  | 500    |
| `RuleError`    | `{ rule; detail?; fields? }`                          | 400    |

`SchemaFailure` = `type-mismatch`, `not-an-object`, `unknown-field`, `invalid-identifier`, `reserved-field`,
`reserved-table`, `empty-index`, `unindexable-field`, `duplicate-index`.

`CompileFailure` = `unknown-collection`, `unknown-field`, `array-predicate`, `unsupported-operator`,
`gt-requires-numeric`, `invalid-limit`, `invalid-ordering`.

`DecodeFailure` = `unsafe-integer`, `malformed-json`, `missing-column`.

### Routing and authentication (raised from `platform/http/`)

| Tag                   | Payload                                                                               | Status |
| --------------------- | ------------------------------------------------------------------------------------- | ------ |
| `RouteNotFound`       | `{ method; path }`                                                                    | 404    |
| `UnknownAppError`     | `{ appId }`                                                                           | 404    |
| `UnknownHandlerError` | `{ appId; name }`                                                                     | 404    |
| `HandlerKindMismatch` | `{ appId?; name?; expected: HandlerKind; actual: HandlerKind }`                       | 400    |
| `AuthError`           | `{ scope: "app" \| "admin"; appId?; reason: "missing" \| "malformed" \| "mismatch" }` | 401    |
| `RequestTooLarge`     | `{ limit; observed }`                                                                 | 413    |

### Document semantics

| Tag             | Payload                             | Status |
| --------------- | ----------------------------------- | ------ |
| `NotFoundError` | `{ table; id }`                     | 404    |
| `ConflictError` | `{ expected; actual; table?; id? }` | 409    |

`table` and `id` are optional on `ConflictError` so an app handler can fail with just the two versions,
matching the plan's §1.4 example verbatim.

### Write path (`Storage`, SQLite)

| Tag                          | Payload                                                   | Status       |
| ---------------------------- | --------------------------------------------------------- | ------------ |
| `StorageError`               | `{ operation; table?; detail?; cause? }`                  | 500          |
| `SchemaDriftError`           | `{ file; detail? }`                                       | never served |
| `ContextExpiredError`        | `{ context: "mutation" \| "query" }`                      | 500          |
| `NonSerializableResultError` | `{ appId?; name?; detail? }`                              | 500          |
| `StatementDeadlineError`     | `{ deadlineMs; elapsedMs; table; statementKind; cause? }` | 504          |

`StorageError.operation` is one of `open`, `ddl`, `select`, `insert`, `update`, `delete`, `begin`, `commit`,
`rollback`, `snapshot`, `close`, `pragma` — enough to tell a failed `COMMIT` from a failed `VACUUM INTO` in an
analysis query without parsing a message.

### Read path (`Reader`, DuckDB)

| Tag                      | Payload                                                  | Status |
| ------------------------ | -------------------------------------------------------- | ------ |
| `ReaderError`            | `{ operation; table?; detail?; cause? }`                 | 500    |
| `QueryWriteAttemptError` | `{ operation: "insert" \| "patch" \| "delete"; table? }` | 500    |

### Queues, IPC, object store, backups, event log

| Tag                      | Payload                                                        | Status |
| ------------------------ | -------------------------------------------------------------- | ------ |
| `QueueFullError`         | `{ queue: "writer" \| "reader" \| "events"; depth; cap }`      | 503    |
| `IpcError`               | `{ reason: IpcFailure; detail?; cause? }`                      | 502    |
| `ReaderExitedError`      | `{ code; signal? }`                                            | 502    |
| `ObjectStoreError`       | `{ operation: "put" \| "get"; key; status?; detail?; cause? }` | 502    |
| `InvalidKeyError`        | `{ key; reason: KeyFailure }`                                  | 500    |
| `BackupError`            | `{ appId; phase: BackupPhase; detail?; cause? }`               | 500    |
| `BackupAlreadyRunning`   | `{ appId }`                                                    | 409    |
| `SnapshotIntegrityError` | `{ file; detail? }`                                            | 500    |
| `IngestError`            | `{ phase: IngestPhase; sourceFile?; detail?; cause? }`         | 500    |
| `AnalysisError`          | `{ name; detail?; cause? }`                                    | 500    |
| `UnknownAnalysisQuery`   | `{ name; known }`                                              | 404    |

`IpcFailure` = `message-too-large`, `truncated-message`, `malformed-frame`, `unknown-reply-id`,
`write-failed`, `stream-failure`, `timeout`. `BackupPhase` = `snapshot`, `verify`, `hash`, `put-object`,
`put-manifest`, `publish-latest` — the order `Backups.take` performs them, so a stalled backup is visible in
`backup-history`.

### Never served

| Tag            | Payload                                   |
| -------------- | ----------------------------------------- |
| `ConfigError`  | `{ key; reason: ConfigFailure; detail? }` |
| `RestoreError` | `{ reason: RestoreFailure; detail? }`     |
| `RemoteError`  | `{ status; remoteTag?; message }`         |

## Union aliases

```ts
MutationError = SchemaError | CompileError | DecodeError | RuleError | NotFoundError
              | ConflictError | ContextExpiredError | NonSerializableResultError
              | StorageError | QueueFullError

QueryError    = SchemaError | CompileError | DecodeError | RuleError | NotFoundError
              | ContextExpiredError | QueryWriteAttemptError | ReaderError | QueueFullError

DbError       = MutationError | QueryError          // the sdk `Execute` channel, plan §1.4
PlatformError = DbError | <routing, auth, ipc, object store, backup, event log>
```

Two aliases that are not in the plan's list but that the service signatures need:

- `RemoteReaderError` — what the reader process can return inside a `ReaderReply` error frame (`QueryError`
  plus the routing errors the reader itself checks, plus the ingest and analysis errors). `ReaderClient.call`
  includes it in its channel rather than collapsing every remote failure into `IpcError`, so a query-side
  `SchemaError` still answers **400** and not the 502 a blanket wrap would produce. The wire shape is
  `ReaderErrorFrame` in `platform/services.ts`: `{ _tag, message, fields }`, which item 17 rehydrates into the
  class.
- `LayerError = StartupError | PlatformError` — the error channel of a `Layer.scoped` / `Layer.effect`,
  declared in `platform/services.ts` so items 08–23 agree on it.

## Two renames and four status changes

### `QueryError` is the read-path union; the engine failure is `ReaderError`

The plan asks item 02 for both a union alias named `QueryError` (§1.2:
`Reader.query(def, args): Effect<unknown, QueryError>`) and a tag named `QueryError` (§1.3:
`QueryError -> 500`). TypeScript cannot have both: a class declaration already introduces a type of the same
name. The union alias wins, because the `Reader` signature needs it and because item 02's own work list names
the four aliases explicitly. The 500-level DuckDB engine failure is therefore **`ReaderError`**, which also
reads better next to `StorageError` on the write side.

### `RouteNotFound` is separate from `NotFoundError`

`NotFoundError({ table, id })` is the document-level failure the plan's §1.4 example constructs. An unmatched
HTTP route has no table and no id, so it gets its own tag. Both answer 404.

### Status changes against today's blanket 400

| Error                  | Today                                 | After   | Note                                            |
| ---------------------- | ------------------------------------- | ------- | ----------------------------------------------- |
| `ConflictError`        | 400 (string match on `Stale version`) | **409** | called out in the plan; item 22 and item 27     |
| `QueueFullError`       | 400                                   | **503** | called out in the plan; item 22 and item 27     |
| `RequestTooLarge`      | 400                                   | **413** | specified in §1.3; item 22 keeps the 64 KiB cap |
| `BackupAlreadyRunning` | 400                                   | **409** | **not** in §1.3's list — see below              |

`BackupAlreadyRunning` is a fourth client-visible change, decided here rather than in the plan. Its reasoning
is identical to `ConflictError`'s: the request is well-formed and conflicts with current state, which is what
409 means, and `POST /admin/backup/:appId` is the one endpoint that can return it. Item 22's router must map
it and item 27's README must list it alongside the other three.

## Legacy throw sites → tagged errors

All 56 `new Error(...)` sites in the repo as of commit `962f675`. "Asserted" marks a message string
`tests/core_test.ts` matches on today, which the owning item must keep reproducible.

### `sdk/schema.ts` — validation and schema declarations (item 04)

| Site  | Today's message                                                           | Tagged error                                                                 |
| ----- | ------------------------------------------------------------------------- | ---------------------------------------------------------------------------- |
| `:35` | `An index needs at least one field`                                       | `SchemaError({ reason: "empty-index", path: indexName })`                    |
| `:38` | `Cannot index ${field}`                                                   | `SchemaError({ reason: "unindexable-field", path: field })`                  |
| `:41` | `Duplicate index ${name}`                                                 | `SchemaError({ reason: "duplicate-index", path: name })`                     |
| `:55` | `Invalid identifier: ${name}`                                             | `SchemaError({ reason: "invalid-identifier", path: name })`                  |
| `:61` | `Reserved field: ${name}`                                                 | `SchemaError({ reason: "reserved-field", path: name })`                      |
| `:69` | `Reserved table: ${name}`                                                 | `SchemaError({ reason: "reserved-table", path: name })`                      |
| `:92` | `${path}: expected ${kind}[ or null]` — **asserted** `/expected integer/` | `SchemaError({ reason: "type-mismatch", path, expected, actual })`           |
| `:96` | `Expected an object`                                                      | `SchemaError({ reason: "not-an-object", path, expected: "object", actual })` |
| `:99` | `Unknown field: ${key}` — **asserted** `/Unknown field/`                  | `SchemaError({ reason: "unknown-field", path: key })`                        |

`Table.index`, `defineTable` and `defineSchema` run at module load in `app/`, so item 04 keeps them throwing
through the `*OrThrow` wrappers; the `Either`-returning forms are the ones `core/` exposes.

### `sdk/db.ts`, `sdk/client.ts` — the SDK seam (item 10)

| Site           | Today's message                | Tagged error                                        |
| -------------- | ------------------------------ | --------------------------------------------------- |
| `db.ts:34`     | `Limit must be 1..1000`        | `CompileError({ reason: "invalid-limit", detail })` |
| `client.ts:14` | `body.error ?? HTTP ${status}` | `RemoteError({ status, remoteTag, message })`       |

### `platform/compiler.ts` — plan compilation (item 09)

| Site   | Today's message                                                                            | Tagged error                                                  |
| ------ | ------------------------------------------------------------------------------------------ | ------------------------------------------------------------- |
| `:14`  | `Unknown collection: ${name}`                                                              | `CompileError({ reason: "unknown-collection", table: name })` |
| `:19`  | `Unknown field: ${name}` — **asserted** `/Unknown field/`                                  | `CompileError({ reason: "unknown-field", field: name })`      |
| `:24`  | `Array predicates/ordering are not supported: ${name}` — **asserted** `/Array predicates/` | `CompileError({ reason: "array-predicate", field: name })`    |
| `:78`  | `Invalid limit`                                                                            | `CompileError({ reason: "invalid-limit", detail })`           |
| `:86`  | `Unsupported operator`                                                                     | `CompileError({ reason: "unsupported-operator", field })`     |
| `:88`  | `gt requires a non-null numeric field and value`                                           | `CompileError({ reason: "gt-requires-numeric", field })`      |
| `:103` | `Invalid ordering`                                                                         | `CompileError({ reason: "invalid-ordering", field })`         |
| `:147` | `Integer result exceeds JavaScript safe range`                                             | `DecodeError({ reason: "unsafe-integer", field, actual })`    |

`:147` is the one site in this file that is **not** a 400. It fires on the way _out_ of the engine, on data
already stored, so it is a server-side decode fault — hence `DecodeError` → 500 rather than `CompileError`
→ 400.

### `platform/sqlite.ts` — the writer (item 15)

| Site  | Today's message                                                                                             | Tagged error                                                     |
| ----- | ----------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| `:27` | `Schema/storage changed. MVP refuses automatic migration; …` — **asserted** `/refuses automatic migration/` | `SchemaDriftError({ file })`                                     |
| `:39` | `Writer queue full` (rejection)                                                                             | `QueueFullError({ queue: "writer", depth, cap: 1000 })`          |
| `:69` | `Document not found`                                                                                        | `NotFoundError({ table, id })`                                   |
| `:80` | `Expected a mutation`                                                                                       | `HandlerKindMismatch({ expected: "mutation", actual: "query" })` |
| `:85` | `Transaction context has expired`                                                                           | `ContextExpiredError({ context: "mutation" })`                   |

Item 02's work list specifies `QueueFullError({ queue, depth, cap })` with all three fields; item 15's
illustrative snippet omits `depth`. The three-field payload wins — `depth` is what makes the `queue-depth`
analysis query possible. Driver-level failures around these sites (`:21` pragmas, `:29` DDL, `:82`/`:92`/`:95`
transaction control, `:104` `VACUUM INTO`) become `StorageError({ operation, cause })`; today they propagate
as raw `node:sqlite` errors via the bare `throw e` at `:35` and `:96`.

### `platform/duckdb.ts`, `platform/reader.ts` — the reader (item 16)

| Site           | Today's message             | Tagged error                                                                 |
| -------------- | --------------------------- | ---------------------------------------------------------------------------- |
| `duckdb.ts:29` | `Expected a query`          | `HandlerKindMismatch({ expected: "query", actual: "mutation" })`             |
| `duckdb.ts:33` | `Query context has expired` | `ContextExpiredError({ context: "query" })`                                  |
| `duckdb.ts:34` | `Queries cannot write`      | `QueryWriteAttemptError({ operation, table })`                               |
| `reader.ts:17` | `Unknown app or handler`    | split: `UnknownAppError({ appId })` / `UnknownHandlerError({ appId, name })` |

> Note: `HandlerKindMismatch` is raised by the router (item 22), not inside `Storage.mutation` /
> `Reader.query` — those services' frozen error channels (`MutationError`, `QueryError`) do not include it.
> The mappings above describe the semantic site, not the raising layer. | `reader.ts:19` |
> `Reader accepts queries only` | `HandlerKindMismatch({ appId, name, expected: "query", actual })` |

`reader.ts:27` currently flattens whatever was thrown to `e.message` on the reply. It becomes a
`ReaderErrorFrame`, so the parent can rebuild the tag and keep the status. `DuckReader.open`'s
`INSTALL`/`LOAD`/`ATTACH` failures (`duckdb.ts:21-25`) become
`ReaderError({ operation: "attach" | "open", cause })`.

### `platform/ipc.ts` — framing and the parent client (items 05 and 17)

| Site  | Today's message                           | Tagged error                                               |
| ----- | ----------------------------------------- | ---------------------------------------------------------- |
| `:12` | `IPC message too large`                   | `IpcError({ reason: "message-too-large", detail })`        |
| `:14` | `Truncated IPC message`                   | `IpcError({ reason: "truncated-message" })`                |
| `:44` | `new Error(reply.error)` (remote failure) | rehydrated `RemoteReaderError` from the `ReaderErrorFrame` |
| `:46` | `Reader exited: ${code}`                  | `ReaderExitedError({ code, signal })`                      |
| `:47` | `new Error(String(e))` (stream failure)   | `IpcError({ reason: "stream-failure", cause })`            |
| `:51` | `Reader queue full` (rejection)           | `QueueFullError({ queue: "reader", depth, cap: 1000 })`    |

Item 05's pure decoder returns `Either<IpcError, string[]>`; the plan's `MessageTooLarge` and
`TruncatedMessage` are the first two `IpcFailure` reasons rather than separate tags. Item 17's new per-call
timeout is `IpcError({ reason: "timeout" })`, and a reply for an id nobody is waiting on is
`IpcError({ reason: "unknown-reply-id" })` logged as an anomaly rather than silently dropped.

### `platform/object_store.ts` — object storage (item 18)

| Site  | Today's message                      | Tagged error                                                   |
| ----- | ------------------------------------ | -------------------------------------------------------------- |
| `:13` | `Invalid key`                        | `InvalidKeyError({ key, reason })`                             |
| `:30` | `S3 credentials are required`        | `ConfigError({ key: "AWS_ACCESS_KEY_ID", reason: "missing" })` |
| `:44` | `S3 PUT failed (${status}): ${body}` | `ObjectStoreError({ operation: "put", key, status, detail })`  |
| `:49` | `S3 GET failed (${status})`          | `ObjectStoreError({ operation: "get", key, status })`          |

`:13`'s three rejection branches become the three `KeyFailure` reasons (`illegal-character`,
`parent-traversal`, `absolute-path`). `InvalidKeyError` is 500, not 400: object keys are generated by
`Backups.take`, never supplied by an HTTP caller, so a bad key is this server's bug.

### `platform/backup.ts` — backups (item 19)

| Site  | Today's message                       | Tagged error                               |
| ----- | ------------------------------------- | ------------------------------------------ |
| `:12` | `Snapshot failed integrity check`     | `SnapshotIntegrityError({ file, detail })` |
| `:19` | `Backup already running for this app` | `BackupAlreadyRunning({ appId })`          |

Every other failure inside `take` is wrapped as `BackupError({ appId, phase, cause })`, which is why
`BackupsService.take`'s channel stays narrow.

### `platform/server.ts` — startup and the HTTP surface (item 22)

| Site         | Today                                            | Tagged error                                                         |
| ------------ | ------------------------------------------------ | -------------------------------------------------------------------- |
| `:27`        | `throw "Request exceeds 64 KiB"` → 400           | `RequestTooLarge({ limit: 65536, observed })` → **413**              |
| `:72`        | `throw "Invalid backup interval"`                | `ConfigError({ key: "BACKUP_INTERVAL_MS", reason: "out-of-range" })` |
| `:42`, `:51` | `reply({ error: "Not found" }, 404)`             | `RouteNotFound({ method, path })`                                    |
| `:45`, `:55` | `reply({ error: "Unauthorized" }, 401)`          | `AuthError({ scope, appId?, reason })`                               |
| `:47`, `:54` | `reply({ error: "Unknown app" }, 404)`           | `UnknownAppError({ appId })`                                         |
| `:56`        | `reply({ error: "Unknown handler" }, 404)`       | `UnknownHandlerError({ appId, name })`                               |
| `:58`        | `reply({ error: "Handler kind mismatch" }, 400)` | `HandlerKindMismatch({ appId, name, expected, actual })`             |
| `:64-67`     | blanket `catch` → 400 with `e.message`           | `toHttpStatus(e)` with `{ error: { tag, message } }`                 |

### `app/functions.ts` — application rules (item 10)

| Site         | Today's message                                  | Tagged error                                                        |
| ------------ | ------------------------------------------------ | ------------------------------------------------------------------- |
| `:16`        | `Provide two distinct players`                   | `RuleError({ rule: "distinct-players" })`                           |
| `:31`, `:82` | `Game not found`                                 | `NotFoundError({ table: "games", id: gameId })`                     |
| `:32`        | `Stale version; reload the game`                 | `ConflictError({ table: "games", id, expected, actual })` → **409** |
| `:33`        | `Game is over` — **asserted** `/Game is over/`   | `RuleError({ rule: "game-over" })`                                  |
| `:35`        | `Not your turn` — **asserted** `/Not your turn/` | `RuleError({ rule: "not-your-turn" })`                              |
| `:36`        | `Invalid move` — **asserted** `/Invalid move/`   | `RuleError({ rule: "invalid-move", fields: { cell } })`             |

The four `RuleError`s move into the pure `applyMove` in `app/rules.ts`, which is the second testability win in
the plan's §1.4: the game rules become unit- and property-testable with no database at all. Item 10 must keep
`rule` values that render to the asserted strings.

### `scripts/restore.ts` — the restore CLI

| Site  | Today's message                                           | Tagged error                                                |
| ----- | --------------------------------------------------------- | ----------------------------------------------------------- |
| `:6`  | `Usage: deno task restore <appId> <NEW database path>; …` | `RestoreError({ reason: "usage" })`                         |
| `:10` | `Refusing to overwrite ${file}`                           | `RestoreError({ reason: "would-overwrite", detail: file })` |
| `:15` | `Invalid manifest`                                        | `RestoreError({ reason: "invalid-manifest" })`              |
| `:17` | `Backup checksum mismatch`                                | `RestoreError({ reason: "checksum-mismatch" })`             |

### Test scaffolding — not part of the taxonomy

`tests/integration.ts:28` (`Server exited before becoming ready`) and `tests/core_test.ts:91`
(`Injected failure after both writes`, a deliberately thrown fixture) stay plain `throw`s. The second one is
load-bearing as a _defect_: it proves the transaction rolls back when a handler blows up in a way the taxonomy
does not describe. Items 20 and 25 own those files.

## Consequences

**Good.** Statuses are correct, and correct for a reason that is written down once. Recovery logic switches on
`_tag` instead of matching message text. The event log gets a bounded-cardinality `error_tag` column plus a
structured `attrs` payload, which is what makes `error-rate-by-handler` and `trace` answerable. `toHttpStatus`
cannot silently fall out of date.

**Costs.** 32 classes and seven reason vocabularies are more to read than `throw new Error`. Four status
changes are client-visible. Each later item has to construct the right payload rather than a convenient
string, and items 04 and 10 carry the extra burden of reproducing the seven asserted message strings on top of
the structured error.

**Deferred.** No `Schema`-based (de)serialization of errors: `ReaderErrorFrame` is hand-rolled
`{ _tag, message, fields }` and item 17 owns the rehydration. If `@effect/schema` enters the dependency set
later, that frame is the natural first thing to derive instead of hand-write.

## Alternatives considered

- **One `PlatformError` with a `code` field.** Fewer declarations, and a single class to catch. Rejected: a
  single payload type that covers `{ path, expected, actual }` and `{ queue, depth, cap }` and
  `{ operation, key, status }` is either `Record<string, unknown>` — which loses the typing that is the point
  — or a union of optional fields, which no longer tells a reader which fields are present when.
- **A tag per message (~50 tags).** Maximum precision. Rejected: `toHttpStatus` becomes unreadable, and the
  distinctions are mostly not ones any caller acts on. Reason literals keep the precision for analysis without
  spending it on the status mapping.
- **Keep `Error` and add a `status` property.** Minimal diff. Rejected: it does not type the error channel, so
  `Effect`'s `E` parameter stays `unknown` and the compiler stops helping — which is most of why this port is
  happening.
- **Put statuses on the error classes themselves** (a `status` field on each). Rejected: it mixes a transport
  concern into `core/`, which must not know that HTTP exists, and it loses the single total function that
  `tests/errors_exhaustiveness.ts` can check.
