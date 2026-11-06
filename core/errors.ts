/**
 * The error taxonomy. Every failure in this repo is one of these tagged classes.
 *
 * This module is the contract the rest of the Effect port codes against (plan item 02). It is PURE:
 * it imports `effect` and nothing else — no platform or SDK modules, no Node built-ins, no native
 * npm packages, no Deno globals. See `docs/adr/0001-effect-port.md` for the `core/` purity rule and
 * `docs/adr/0002-error-taxonomy.md` for the per-tag payload rationale and the mapping from every
 * legacy throw site to a tagged class.
 *
 * Three rules for everything built on top of this file:
 *
 * 1. Errors carry **structured fields**, never a pre-formatted message. Rendering a human string is
 *    the caller's job (`sdk/schema.ts`'s `formatSchemaError`, the HTTP body in item 22,
 *    `core/events.ts`'s `fromPlatformError` in item 07).
 * 2. A tag's HTTP status is decided once, in `toHttpStatus`. Adding a member to `PlatformError`
 *    without adding a case there is a compile error.
 * 3. `Data.TaggedError` extends `Error`, so these are `yield*`-able inside `Effect.gen` and still
 *    produce a usable stack trace when they escape as defects.
 */
import { Data, Function as EffectFunction } from "effect";

// ---------------------------------------------------------------------------
// Reason vocabularies
//
// A handful of tags cover a family of related failures. Rather than minting a tag per message, the
// family shares a tag and discriminates on a `reason` literal, so analysis queries can group by
// `error_tag` and still drill into `attrs.reason`.
// ---------------------------------------------------------------------------

/** Why a value failed schema validation or a schema declaration was rejected. */
export type SchemaFailure =
  | "type-mismatch"
  | "not-an-object"
  | "unknown-field"
  | "invalid-identifier"
  | "reserved-field"
  | "reserved-table"
  | "empty-index"
  | "unindexable-field"
  | "duplicate-index";

/** Why a logical plan could not be compiled to SQL. */
export type CompileFailure =
  | "unknown-collection"
  | "unknown-field"
  | "array-predicate"
  | "unsupported-operator"
  | "gt-requires-numeric"
  | "invalid-limit"
  | "invalid-ordering"
  | "empty-enum";

/** Why a stored value could not be decoded back into a document. */
export type DecodeFailure = "unsafe-integer" | "malformed-json" | "missing-column";

/** Which side of the IPC transport failed, and how. */
export type IpcFailure =
  | "message-too-large"
  | "truncated-message"
  | "malformed-frame"
  | "unknown-reply-id"
  | "write-failed"
  | "stream-failure"
  | "timeout";

/** Why a configuration value was rejected at startup. */
export type ConfigFailure = "missing" | "malformed" | "out-of-range" | "inconsistent";

/** Why an object-store key was rejected. Keys are platform-generated, never caller-supplied. */
export type KeyFailure = "illegal-character" | "parent-traversal" | "absolute-path";

/** Why a restore run refused to continue. */
export type RestoreFailure = "usage" | "would-overwrite" | "invalid-manifest" | "checksum-mismatch";

/** The named queues with an enforced depth cap. */
export type QueueName = "writer" | "reader" | "events";

/** The two kinds of handler the platform routes to. Mirrors `sdk/functions.ts`'s `Kind` without importing it. */
export type HandlerKind = "query" | "mutation";

/** The backup phases, in the order `Backups.take` performs them (item 19). */
export type BackupPhase =
  | "snapshot"
  | "verify"
  | "hash"
  | "put-object"
  | "put-manifest"
  | "publish-latest";

/** The phases of a JSONL-to-DuckDB ingest run (item 21). */
export type IngestPhase = "inventory" | "plan" | "read" | "insert" | "commit" | "state-update";

// ---------------------------------------------------------------------------
// Request and plan errors (pure; raised from `core/`)
// ---------------------------------------------------------------------------

/**
 * A value did not match its schema node, or a schema declaration is illegal.
 * Covers every `sdk/schema.ts` throw site. `path` is the dotted/indexed field path
 * (`"board[3]"`), or the offending identifier for declaration-time failures.
 */
export class SchemaError extends Data.TaggedError("SchemaError")<{
  readonly reason: SchemaFailure;
  readonly path: string;
  /** The node kind or shape that was required, e.g. `"integer"`, `"object"`, `"integer or null"`. */
  readonly expected?: string;
  /** The rejected value. Carried structurally; redaction is `core/events.ts`'s job. */
  readonly actual?: unknown;
}> {}

/** A logical plan could not be compiled into SQL for the target dialect. */
export class CompileError extends Data.TaggedError("CompileError")<{
  readonly reason: CompileFailure;
  readonly field?: string;
  readonly table?: string;
  readonly detail?: string;
}> {}

/** A row came back from the engine in a shape the decoder cannot represent as a document. */
export class DecodeError extends Data.TaggedError("DecodeError")<{
  readonly reason: DecodeFailure;
  readonly table?: string;
  readonly field?: string;
  readonly actual?: unknown;
}> {}

/**
 * An application invariant was violated — the app-domain failures that today read
 * `throw new Error("Not your turn")`. `rule` is an app-defined identifier; the platform only
 * transports it. Item 10 moves these into `app/rules.ts`.
 */
export class RuleError extends Data.TaggedError("RuleError")<{
  readonly rule: string;
  readonly detail?: string;
  readonly fields?: Readonly<Record<string, unknown>>;
}> {}

// ---------------------------------------------------------------------------
// Routing and authentication errors (raised by the HTTP surface; the Deno platform's until IR-6)
// ---------------------------------------------------------------------------

/** The request path matched no route. */
export class RouteNotFound extends Data.TaggedError("RouteNotFound")<{
  readonly method: string;
  readonly path: string;
}> {}

/** No app is registered under this id. */
export class UnknownAppError extends Data.TaggedError("UnknownAppError")<{
  readonly appId: string;
}> {}

/** The app exists but exports no handler by this name. */
export class UnknownHandlerError extends Data.TaggedError("UnknownHandlerError")<{
  readonly appId: string;
  readonly name: string;
}> {}

/** A query was routed to a mutation handler, or vice versa. */
export class HandlerKindMismatch extends Data.TaggedError("HandlerKindMismatch")<{
  readonly appId?: string;
  readonly name?: string;
  readonly expected: HandlerKind;
  readonly actual: HandlerKind;
}> {}

/** A bearer token was missing or wrong. Never carries the presented token. */
export class AuthError extends Data.TaggedError("AuthError")<{
  readonly scope: "app" | "admin";
  readonly appId?: string;
  readonly reason: "missing" | "malformed" | "mismatch";
}> {}

/** The request body exceeded the configured cap before it was fully read. */
export class RequestTooLarge extends Data.TaggedError("RequestTooLarge")<{
  readonly limit: number;
  /** Bytes read when the cap was crossed; the body is not drained to find the true size. */
  readonly observed: number;
}> {}

// ---------------------------------------------------------------------------
// Document semantics
// ---------------------------------------------------------------------------

/** The addressed document does not exist. */
export class NotFoundError extends Data.TaggedError("NotFoundError")<{
  readonly table: string;
  readonly id: string;
}> {}

/**
 * Optimistic-concurrency failure: the caller's `_version` is not the stored one.
 * `table` and `id` are optional so app handlers can fail with just the two versions
 * (see the plan's §1.4 example).
 */
export class ConflictError extends Data.TaggedError("ConflictError")<{
  readonly expected: number;
  readonly actual: number;
  readonly table?: string;
  readonly id?: string;
}> {}

// ---------------------------------------------------------------------------
// Write path (`Storage`, SQLite)
// ---------------------------------------------------------------------------

/** The SQLite writer failed: a driver error, a failed statement, or a transaction that would not commit. */
export class StorageError extends Data.TaggedError("StorageError")<{
  readonly operation:
    | "open"
    | "ddl"
    | "select"
    | "insert"
    | "update"
    | "delete"
    | "begin"
    | "commit"
    | "rollback"
    | "snapshot"
    | "close"
    | "pragma";
  readonly table?: string;
  readonly detail?: string;
  readonly cause?: unknown;
}> {}

/**
 * The stored schema fingerprint does not match the deployed schema. The MVP refuses automatic
 * migration, so this fails `StorageLive`'s acquire and the process never starts serving.
 */
export class SchemaDriftError extends Data.TaggedError("SchemaDriftError")<{
  readonly file: string;
  readonly detail?: string;
}> {}

/**
 * A `Documents` handle was used after its transaction or query context closed — today's
 * `active` flag, now typed.
 */
export class ContextExpiredError extends Data.TaggedError("ContextExpiredError")<{
  readonly context: "mutation" | "query";
}> {}

/** A handler returned something `JSON.stringify` cannot transport, caught before `COMMIT`. */
export class NonSerializableResultError extends Data.TaggedError("NonSerializableResultError")<{
  readonly appId?: string;
  readonly name?: string;
  readonly detail?: string;
}> {}

// ---------------------------------------------------------------------------
// Statement deadline (both SQLite paths, FFI-05)
// ---------------------------------------------------------------------------

/** The unbounded statement kinds a deadline can stop: a plain select, `count` or `countBy`. */
export type DeadlineStatementKind = "select" | "count" | "countBy";

/**
 * An unbounded statement (any select that is not a primary-key lookup, `count`, `countBy`) ran past
 * `Config.sqlite.statementDeadlineMs`. It was stopped by `sqlite3_interrupt`, not abandoned: when
 * this error exists the statement is no longer running. Raised by the writer (`Storage`) and the
 * in-process document reader; the DuckDB reader process never raises it, so it stays out of
 * `QueryError` and `RemoteReaderError` and joins the unions at `DbError`. HTTP 504.
 */
export class StatementDeadlineError extends Data.TaggedError("StatementDeadlineError")<{
  /** The deadline the statement ran under, in ms. */
  readonly deadlineMs: number;
  /** Wall time from the statement's start to its interrupted return, in ms. */
  readonly elapsedMs: number;
  readonly table: string;
  readonly statementKind: DeadlineStatementKind;
  /** The binding's own cancellation record (`SqliteCancelled`). */
  readonly cause?: unknown;
}> {}

// ---------------------------------------------------------------------------
// Read path (`Reader`, DuckDB)
// ---------------------------------------------------------------------------

/**
 * The DuckDB reader failed: `INSTALL`/`LOAD`/`ATTACH`, a failed statement, or a transaction that
 * would not commit. Named `ReaderError`, not `QueryError`, because `QueryError` is the read-path
 * union alias — see ADR 0002.
 */
export class ReaderError extends Data.TaggedError("ReaderError")<{
  readonly operation: "open" | "attach" | "select" | "begin" | "commit" | "rollback" | "close";
  readonly table?: string;
  readonly detail?: string;
  readonly cause?: unknown;
}> {}

/** A query handler attempted a write. A server-side invariant violation, not caller input. */
export class QueryWriteAttemptError extends Data.TaggedError("QueryWriteAttemptError")<{
  readonly operation: "insert" | "patch" | "delete";
  readonly table?: string;
}> {}

// ---------------------------------------------------------------------------
// Queues
// ---------------------------------------------------------------------------

/** A bounded queue rejected work at its cap. Shed load, not a server fault. */
export class QueueFullError extends Data.TaggedError("QueueFullError")<{
  readonly queue: QueueName;
  readonly depth: number;
  readonly cap: number;
}> {}

// ---------------------------------------------------------------------------
// Reader IPC (parent side)
// ---------------------------------------------------------------------------

/** The JSON-lines transport to the reader process failed. */
export class IpcError extends Data.TaggedError("IpcError")<{
  readonly reason: IpcFailure;
  readonly detail?: string;
  readonly cause?: unknown;
}> {}

/** The reader child process exited; every outstanding call fails with this. */
export class ReaderExitedError extends Data.TaggedError("ReaderExitedError")<{
  readonly code: number;
  readonly signal?: string;
}> {}

// ---------------------------------------------------------------------------
// Object store and backups
// ---------------------------------------------------------------------------

/** A local or S3 object operation failed. `status` is set only for HTTP transports. */
export class ObjectStoreError extends Data.TaggedError("ObjectStoreError")<{
  readonly operation: "put" | "get";
  readonly key: string;
  readonly status?: number;
  readonly detail?: string;
  readonly cause?: unknown;
}> {}

/** An object key failed validation. Keys are platform-generated, so this is an internal fault. */
export class InvalidKeyError extends Data.TaggedError("InvalidKeyError")<{
  readonly key: string;
  readonly reason: KeyFailure;
}> {}

/** A backup run failed at a named phase. */
export class BackupError extends Data.TaggedError("BackupError")<{
  readonly appId: string;
  readonly phase: BackupPhase;
  readonly detail?: string;
  readonly cause?: unknown;
}> {}

/** A backup for this app is already in flight; the per-app guard rejected a second one. */
export class BackupAlreadyRunning extends Data.TaggedError("BackupAlreadyRunning")<{
  readonly appId: string;
}> {}

/** `PRAGMA quick_check` on a fresh snapshot returned anything other than `ok`. */
export class SnapshotIntegrityError extends Data.TaggedError("SnapshotIntegrityError")<{
  readonly file: string;
  readonly detail?: string;
}> {}

/** A restore run refused to continue (the Deno platform's restore script; unused since IR-6). CLI-only. */
export class RestoreError extends Data.TaggedError("RestoreError")<{
  readonly reason: RestoreFailure;
  readonly detail?: string;
}> {}

// ---------------------------------------------------------------------------
// Event log and analysis
// ---------------------------------------------------------------------------

/** JSONL-to-DuckDB ingestion failed at a named phase. */
export class IngestError extends Data.TaggedError("IngestError")<{
  readonly phase: IngestPhase;
  readonly sourceFile?: string;
  readonly detail?: string;
  readonly cause?: unknown;
}> {}

/** A named analysis query failed to execute. */
export class AnalysisError extends Data.TaggedError("AnalysisError")<{
  readonly name: string;
  readonly detail?: string;
  readonly cause?: unknown;
}> {}

/** The requested analysis query is not in the catalog. Callers never supply SQL. */
export class UnknownAnalysisQuery extends Data.TaggedError("UnknownAnalysisQuery")<{
  readonly name: string;
  readonly known: readonly string[];
}> {}

// ---------------------------------------------------------------------------
// Tenancy (MT-03)
// ---------------------------------------------------------------------------

/**
 * A tenant id that is not a strictly-parsed canonical-lowercase UUID. Raised by
 * the Deno platform's `parseTenantId` (and `parseApiKeyId`; removed at IR-6) on anything else:
 * uppercase, `urn:uuid:` forms, NUL, whitespace, percent-encoded forms, separators.
 * The brand on `TenantId` guarantees the parse already happened, so this never escapes
 * a parsed id.
 */
export class MalformedTenantId extends Data.TaggedError("MalformedTenantId")<{
  /** The offending input, verbatim. Ids are UUIDs, never secrets. */
  readonly input: string;
}> {}

/**
 * No tenant is addressed by this id — or no usable API key hashes to one.
 * `resolveTenantByKeyHash` fails this for unknown keys, revoked keys, and inactive
 * tenants alike, so callers cannot distinguish the three (no key-guessing oracle).
 */
export class UnknownTenant extends Data.TaggedError("UnknownTenant")<{
  /** The tenant id that missed, when one was supplied (absent for key-hash lookups). */
  readonly tenantId?: string;
}> {}

/** The tenant exists but is deactivated. Its keys stay revoked even if it is reactivated. */
export class TenantDeactivated extends Data.TaggedError("TenantDeactivated")<{
  readonly tenantId: string;
}> {}

/** Tenant domain failures, served by the admin HTTP surface (MT-05). */
export type TenantError = MalformedTenantId | UnknownTenant | TenantDeactivated;

// ---------------------------------------------------------------------------
// Startup and client
// ---------------------------------------------------------------------------

/** A configuration value was missing or invalid. Fails `ConfigLive`; never served. */
export class ConfigError extends Data.TaggedError("ConfigError")<{
  readonly key: string;
  readonly reason: ConfigFailure;
  readonly detail?: string;
}> {}

/**
 * A call through `sdk/client.ts` ended with a definite, tagged failure. Raised in the *caller's*
 * process; never served. Since IR-3 the tag is decided either by the server (a non-2xx `/v1/ops`
 * answer) or by the handler, which now runs in the caller's process: a handler failure such as
 * `makeMove`'s `ConflictError`, or a `SchemaError` from argument parsing, is reported with the status
 * the server would have answered (`toHttpStatus`), so callers see the same `status`/`remoteTag` on
 * every backend. `cause` holds the handler's own error object for a local failure.
 */
export class RemoteError extends Data.TaggedError("RemoteError")<{
  readonly status: number;
  /** The `error.tag` the server reported (or the handler's failure tag), when there is one. */
  readonly remoteTag?: string;
  readonly message: string;
  /** The index of the operation the server blamed (`/v1/ops` `error.op`). */
  readonly op?: number;
  /** The structured payload of a 4xx answer (`/v1/ops` `error.fields`). */
  readonly fields?: Readonly<Record<string, unknown>>;
  /** The handler's own failure, when the tag was decided in this process. */
  readonly cause?: unknown;
}> {}

/**
 * A call through `sdk/client.ts` got no definite answer (IR-3): the server never answered, kept
 * answering a retryable code (`shed`, `unavailable`, `aborted`, `expired`, `wrong_owner`, `fenced`)
 * until the retry policy, the retry budget or the call's deadline ran out, or the locator could not
 * route it. `code` is the last code seen (`transport` for no answer, `deadline` for the call's own
 * deadline, `resolve:<code>` for a locator answer). `mayHaveApplied` is true for a mutation whose
 * commit may have landed (no answer, or an ambiguous `expired`): repeating the call re-runs the
 * handler under a new request id, so callers that need exactly-once must check before repeating.
 */
export class Unavailable extends Data.TaggedError("Unavailable")<{
  readonly code: string;
  readonly message: string;
  readonly status?: number;
  readonly mayHaveApplied: boolean;
}> {}

// ---------------------------------------------------------------------------
// Union aliases
// ---------------------------------------------------------------------------

/**
 * The write path: `Storage.mutation`'s error channel, and the `Execute` channel inside a mutation,
 * together with `StatementDeadlineError` (see `DbError`). Excludes `SchemaDriftError`, which fails
 * layer construction rather than a request.
 */
export type MutationError =
  | SchemaError
  | CompileError
  | DecodeError
  | RuleError
  | NotFoundError
  | ConflictError
  | ContextExpiredError
  | NonSerializableResultError
  | StorageError
  | QueueFullError;

/**
 * The read path: `Reader.query`'s error channel, and the `Execute` channel inside a query (the
 * in-process document reader adds `StatementDeadlineError`, see `DbError`). The 500-level engine
 * failure inside this union is `ReaderError`, not `QueryError`.
 */
export type QueryError =
  | SchemaError
  | CompileError
  | DecodeError
  | RuleError
  | NotFoundError
  | ContextExpiredError
  | QueryWriteAttemptError
  | ReaderError
  | QueueFullError;

/**
 * The SDK seam: `Execute = (operation) => Effect<unknown, DbError>` (plan §1.4). Both SQLite paths
 * can also stop an unbounded statement at its deadline (`StatementDeadlineError`, FFI-05). That tag
 * joins here rather than in `MutationError` / `QueryError`, whose members cross the reader IPC and
 * are listed exhaustively by the DuckDB reader and the test doubles, none of which can raise it.
 */
export type DbError = MutationError | QueryError | StatementDeadlineError;

/**
 * What the reader process can send back inside a `ReaderReply` error frame. The parent rehydrates
 * these (item 17) instead of collapsing them into `IpcError`, so a query-side validation failure
 * still answers 400 rather than 502.
 */
export type RemoteReaderError =
  | QueryError
  | UnknownAppError
  | UnknownHandlerError
  | HandlerKindMismatch
  | IngestError
  | AnalysisError
  | UnknownAnalysisQuery;

/** Everything that can reach an HTTP response, and therefore needs a status. */
export type PlatformError =
  | TenantError
  | DbError
  | RouteNotFound
  | UnknownAppError
  | UnknownHandlerError
  | HandlerKindMismatch
  | AuthError
  | RequestTooLarge
  | IpcError
  | ReaderExitedError
  | ObjectStoreError
  | InvalidKeyError
  | BackupError
  | BackupAlreadyRunning
  | SnapshotIntegrityError
  | IngestError
  | AnalysisError
  | UnknownAnalysisQuery;

/** Fails layer construction. The process does not start, so no status is ever needed. */
export type StartupError = ConfigError | SchemaDriftError;

/** Raised by `scripts/`. Reported on stderr with a non-zero exit code, never served. */
export type CliError = RestoreError;

/** Raised inside an SDK consumer's process by `sdk/client.ts`. */
export type ClientError = RemoteError | Unavailable;

/** Every error this repo defines. Useful for `core/events.ts`'s `error_tag` column. */
export type AnyError = PlatformError | StartupError | CliError | ClientError;

// ---------------------------------------------------------------------------
// HTTP status mapping
// ---------------------------------------------------------------------------

/**
 * The single place a tag becomes a status (plan §1.3). Total by construction: the `default` branch
 * narrows `error` to `never`, so adding a member to `PlatformError` without a case here is a type
 * error. `tests/errors_exhaustiveness.ts` locks that property in the `check` entry points.
 *
 * Used by item 22 and tested there; two mappings are deliberate behaviour changes against today's
 * blanket 400 — `ConflictError` → 409 and `QueueFullError` → 503 — plus `BackupAlreadyRunning` →
 * 409 and `RequestTooLarge` → 413. ADR 0002 records all four, and FFI-05's
 * `StatementDeadlineError` → 504.
 */
export function toHttpStatus(error: PlatformError): number {
  switch (error._tag) {
    // 400 — the request itself is wrong.
    case "MalformedTenantId":
    case "SchemaError":
    case "CompileError":
    case "RuleError":
    case "HandlerKindMismatch":
      return 400;

    // 401 — authentication.
    case "AuthError":
      return 401;

    case "TenantDeactivated":
      return 403;

    // 404 — nothing is addressed by this request.
    case "UnknownTenant":
    case "RouteNotFound":
    case "UnknownAppError":
    case "UnknownHandlerError":
    case "NotFoundError":
    case "UnknownAnalysisQuery":
      return 404;

    // 409 — the request is well-formed but conflicts with current state.
    case "ConflictError":
    case "BackupAlreadyRunning":
      return 409;

    // 413 — body cap.
    case "RequestTooLarge":
      return 413;

    // 500 — this server is at fault.
    case "DecodeError":
    case "ContextExpiredError":
    case "NonSerializableResultError":
    case "StorageError":
    case "ReaderError":
    case "QueryWriteAttemptError":
    case "InvalidKeyError":
    case "BackupError":
    case "SnapshotIntegrityError":
    case "IngestError":
    case "AnalysisError":
      return 500;

    // 502 — a dependency this server talks to is at fault.
    case "IpcError":
    case "ReaderExitedError":
    case "ObjectStoreError":
      return 502;

    // 503 — load shedding; the caller may retry.
    case "QueueFullError":
      return 503;

    // 504 — a statement ran past its deadline and was interrupted (FFI-05).
    case "StatementDeadlineError":
      return 504;

    default:
      return EffectFunction.absurd<number>(error);
  }
}
