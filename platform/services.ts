/**
 * Service tags and interface types — the dependency-injection contract for the whole platform
 * (plan item 02, §1.2). **Declarations only: no implementations and no layers live here.**
 *
 * Live layers land in items 08 and 11–23; test doubles land in item 20. Each tag below names the
 * layers that will provide it, so an item can find its own row.
 *
 * This file also freezes the reader IPC protocol (`ReaderRequest` / `ReaderReply`) so items 16
 * (reader process) and 17 (parent client) can be built concurrently against one wire format.
 *
 * `PLACEHOLDER` marks a type that a later item owns and will move into its own module; the shape
 * here is the minimum needed to type an interface, and the owning item is named. When the owner
 * lands, it re-exports from here or this declaration is deleted in the same commit.
 */
import { Clock as EffectClock, Context, Effect } from "effect";
import type {
  AnalysisError,
  BackupAlreadyRunning,
  BackupError,
  IngestError,
  InvalidKeyError,
  IpcError,
  MutationError,
  ObjectStoreError,
  PlatformError,
  QueryError,
  QueueFullError,
  ReaderExitedError,
  RemoteReaderError,
  SnapshotIntegrityError,
  StartupError,
  StatementDeadlineError,
  StorageError,
  UnknownAnalysisQuery,
  UnknownAppError,
} from "../core/errors.ts";
import type { EventRecord, Level as EventLevel, Role } from "../core/events.ts";
import type { IrFailure } from "../core/ir/errors.ts";
import type { Request as IrRequest, Response as IrResponse } from "../core/ir/types.ts";
import type { Schema } from "../sdk/schema.ts";
import type { AnyDefinition, Functions } from "../sdk/functions.ts";
import type { TenantRegistryService } from "./tenants/registry.ts";

// ===========================================================================
// Event types owned by core/events.ts, re-exported
// ===========================================================================

/**
 * The event vocabulary lives in `core/events.ts`; these are re-exported so `EventSink` and existing
 * importers of this module keep one name for each. `EventRecord` is the canonical discriminated
 * union over `kind`, `Role` the process that writes a record (each gets its own `logs/<role>/`
 * tree, plan §4.2), and `EventLevel` is core's `Level`.
 */
export type { EventLevel, EventRecord, Role };

/**
 * A registered app. Structurally what `platform/registry.ts:loadApps` returns today; item 22 turns
 * that function into `AppRegistryLive` and imports this type instead of inferring it.
 */
export interface App {
  readonly id: string;
  /** Absolute path of the app's SQLite file. */
  readonly file: string;
  readonly schema: Schema;
  readonly functions: Functions;
  /** Bearer token for this app. Redacted by `core/events.ts` before it reaches a log record. */
  readonly token: string;
}

/**
 * A backup manifest. Byte-compatible with `platform/backup.ts:Manifest` and with the `latest.json`
 * documents already written to object storage, so existing snapshots stay restorable. Item 19
 * deletes the local copy in `platform/backup.ts` and imports this one.
 */
export interface Manifest {
  readonly format: 1;
  readonly appId: string;
  /** ISO-8601 instant the snapshot was captured. */
  readonly capturedAt: string;
  readonly key: string;
  readonly bytes: number;
  readonly sha256: string;
}

/** PLACEHOLDER — owned by item 21 (`platform/eventlog/ingest.ts`). One ingest run's outcome. */
export interface IngestReport {
  readonly filesScanned: number;
  readonly filesIngested: number;
  readonly bytesIngested: number;
  readonly linesIngested: number;
  readonly linesRejected: number;
  readonly durationMs: number;
}

/** PLACEHOLDER — owned by item 23 (`platform/analysis/catalog.ts`). One analysis result row. */
export type Row = Readonly<Record<string, unknown>>;

// ===========================================================================
// Reader IPC protocol — FROZEN HERE for items 16, 17, 21 and 23
// ===========================================================================

/** Monotonic per-connection correlation id, assigned by the parent. */
export type ReaderRequestId = number;

/**
 * The caller's trace context, carried beside any request (item 34). Every property is optional, and
 * a frame without them — what every sender before item 34 wrote — is still valid: the reader then
 * runs the request under its local fallback trace `reader-<pid>-<id>`.
 *
 * - `traceId` — the caller's trace. The reader never replaces a valid one.
 * - `spanId` — the span the reader request runs as; the parent assigns it so both processes can name it.
 * - `parentSpanId` — the caller's span that issued the request.
 *
 * Each must be a `TRACE_TOKEN` (1–128 of `[A-Za-z0-9._:-]`, `platform/reader_context.ts`). The
 * reader validates them as unknown JSON *before* narrowing to this type and ignores an invalid one
 * (logged as `ignoredTraceFields`), so on the receiving side these properties are untrusted until
 * `readTraceContext` has read them. `spanId`/`parentSpanId` without a `traceId` are ignored.
 *
 * `ReaderClientLive` (`platform/ipc.ts`) fills absent properties from the calling fiber's trace
 * context when the call is submitted; explicit values here take precedence.
 */
export interface ReaderTraceContext {
  readonly traceId?: string;
  readonly spanId?: string;
  readonly parentSpanId?: string;
}

/**
 * What the parent may ask the reader process to do. The reader owns DuckDB, so both the event-log
 * ingest (item 21) and the analysis catalog (item 23) are reached through this protocol rather than
 * by opening DuckDB in the writer process — see ADR 0001.
 */
export type ReaderRequest =
  & (
    | { readonly kind: "ping" }
    | {
      readonly kind: "query";
      /** Trusted writer-resolved tenant location; never copied from HTTP input. */
      readonly tenant?: { readonly id: string; readonly file: string };
      readonly appId: string;
      readonly name: string;
      readonly args: unknown;
    }
    | { readonly kind: "ingest" }
    | {
      readonly kind: "analysis";
      readonly name: string;
      readonly params: Readonly<Record<string, unknown>>;
    }
  )
  & ReaderTraceContext;

/**
 * A request as it appears on the wire: one JSON object per line, correlation id included. The `id`
 * pairs a reply with its call on one connection; it is a correlation id, never a trace id.
 */
export type ReaderRequestFrame = ReaderRequest & { readonly id: ReaderRequestId };

/**
 * A tagged error, flattened for the wire. The parent rehydrates it into the matching class so the
 * HTTP status still comes from `toHttpStatus(tag)` — a query-side `SchemaError` must answer 400,
 * not the 502 it would get if the parent wrapped every remote failure in `IpcError`.
 */
export interface ReaderErrorFrame {
  readonly _tag: RemoteReaderError["_tag"];
  /** Human-readable rendering, for logs and for the HTTP body's `error.message`. */
  readonly message: string;
  /** The error's structured payload, verbatim, so the parent can reconstruct the class. */
  readonly fields: Readonly<Record<string, unknown>>;
}

/** One reply line. Exactly one of `result` / `error` is present. */
export type ReaderReply =
  | { readonly id: ReaderRequestId; readonly result: unknown }
  | { readonly id: ReaderRequestId; readonly error: ReaderErrorFrame };

// ===========================================================================
// Config — item 08 (`ConfigLive`, `ConfigTest`)
// ===========================================================================

export interface ServerConfig {
  readonly hostname: string;
  readonly port: number;
  /** Request-body cap in bytes. 65 536 today, hard-coded in `platform/server.ts:27`. */
  readonly maxRequestBytes: number;
  /** `ADMIN_TOKEN`. Defaults to `dev-only`, which item 08 reports as a `lifecycle` warning. */
  readonly adminToken: string;
  /** `DATA_DIR`, resolved to an absolute path. */
  readonly dataDir: string;
}

/** One row of the deployment registry. `storage` picks the physical layout for the app's tables. */
export interface AppConfig {
  readonly id: string;
  readonly token: string;
  readonly storage: "columns" | "json";
}

export interface BackupConfig {
  /** `BACKUP_INTERVAL_MS`. `0` disables the timer; otherwise finite and `>= 1000`. */
  readonly intervalMs: number;
}

export interface LoggingConfig {
  /** `LOG_DIR`. Root of the `logs/<role>/<date>/` tree. */
  readonly dir: string;
  readonly level: EventLevel;
  readonly retentionDays: number;
  readonly gzipAfterDays: number;
  readonly maxTotalBytes: number;
  readonly maxFileBytes: number;
  /** `LOG_INCLUDE_BIND_VALUES`. Off by default: bind values are application data. */
  readonly includeBindValues: boolean;
  /** `TRACE_SQL`. Keeps today's opt-in SQL tracing, now as an event level rather than stderr. */
  readonly traceSql: boolean;
  readonly eventQueueCapacity: number;
  readonly eventFlushMs: number;
}

export interface ObjectStoreConfig {
  /** `S3_BASE_URL`. Absent selects `ObjectStoreLocal`; present selects `ObjectStoreS3`. */
  readonly s3BaseUrl?: string;
  readonly region: string;
  readonly accessKeyId?: string;
  readonly secretAccessKey?: string;
  readonly sessionToken?: string;
  /** `LOCAL_OBJECT_DIR`, resolved to an absolute path. */
  readonly localDir: string;
}

export interface EventLogConfig {
  /** `EVENTLOG_DUCKDB`. The persistent DuckDB file, distinct from the per-app `:memory:` readers. */
  readonly duckdbPath: string;
}

/** The server's side of the reader IPC (item 17's `ReaderClientLive`, wired by item 22). */
export interface ReaderConfig {
  /** `READER_CALL_TIMEOUT_MS`. Per-call deadline; a call with no reply by then fails `IpcError("timeout")`. */
  readonly callTimeoutMs: number;
}

/** The SQLite FFI execution layer (FFI-02 options; consumed from FFI-03 on). */
export interface SqliteConfig {
  /** `SQLITE_LIBRARY`. Explicit path or soname of the system libsqlite3; absent = the binding's default list. */
  readonly library?: string;
  /** `STATEMENT_DEADLINE_MS`. Deadline after which an unbounded statement is interrupted. Must be < `reader.callTimeoutMs`. */
  readonly statementDeadlineMs: number;
  /** `STATEMENT_INTERRUPT_GRACE_MS`. Wait after the interrupt before the connection is poisoned. */
  readonly statementInterruptGraceMs: number;
}

export interface TenancyConfig {
  /** Fixed key-cache TTL. Defaults to 30000 until MT-02's environment decoder supplies it. */
  readonly tenantKeyCacheTtlMs?: number;
  /** `TENANCY`. `off` = pre-tenancy behavior (single SQLite file, existing app-token auth). */
  readonly mode: "off" | "api-key" | "better-auth";
  /** `DATABASE_URL`. Postgres central DB for the tenant registry (reserved; MT-01 provisions it). */
  readonly databaseUrl: string;
  /** `TENANTS_DIR`, resolved to an absolute path. */
  readonly tenantsDir: string;
  /** `TENANT_ROUTER`. Only `local` is implemented; `control-plane` is reserved for the future. */
  readonly router: "local" | "control-plane";
  /** `BETTER_AUTH_SECRET`. Required only when mode is `better-auth`. */
  readonly betterAuthSecret?: string;
  /** `BETTER_AUTH_URL`. */
  readonly betterAuthUrl: string;
}

/**
 * The whole configuration, read once at layer construction. `platform/config.ts` is the only module
 * permitted to read the process environment.
 */
export interface ConfigService {
  readonly server: ServerConfig;
  readonly apps: readonly AppConfig[];
  readonly backup: BackupConfig;
  readonly logging: LoggingConfig;
  readonly objectStore: ObjectStoreConfig;
  readonly eventLog: EventLogConfig;
  readonly reader: ReaderConfig;
  readonly sqlite: SqliteConfig;
  readonly tenancy: TenancyConfig;
}

/** Live: `ConfigLive` (env, item 08). Test: `ConfigTest(overrides)`. */
export class Config extends Context.Tag("mansio/Config")<Config, ConfigService>() {}

// ===========================================================================
// IdGen — item 15 (`IdGenLive`), item 20 (`IdGenTest`)
// ===========================================================================

export interface IdGenService {
  /** A fresh opaque id. `crypto.randomUUID()` live; `id-1`, `id-2`, … under `IdGenTest`. */
  readonly next: Effect.Effect<string>;
}

/** Live: `IdGenLive` (`crypto.randomUUID`). Test: `IdGenTest`. */
export class IdGen extends Context.Tag("mansio/IdGen")<IdGen, IdGenService>() {}

// ===========================================================================
// Clock — Effect's built-in
// ===========================================================================

/**
 * Time comes from Effect's own `Clock` service, not a tag of ours: the default layer is already
 * provided by every runtime and `TestClock` already substitutes for it. Re-exported here only so
 * that items read one file to find every service they depend on.
 *
 * Use `yield* Clock` for the service and `Clock.currentTimeMillis` for an epoch-millis `Effect`.
 */
export const Clock = EffectClock.Clock;
export type ClockService = EffectClock.Clock;

// ===========================================================================
// EventSink — item 11 (`EventSinkLive`), item 20 (`EventSinkTest`)
// ===========================================================================

export interface EventSinkService {
  /**
   * Record one event. **Cannot fail** — a logging problem must degrade (drop + count) rather than
   * surface in a handler's error channel (item 11). Dropped counts are themselves emitted as a
   * `queue` event, so loss is visible.
   */
  emit(event: EventRecord): Effect.Effect<void>;
}

/** Live: `EventSinkLive` (bounded queue → rotating JSONL). Test: `EventSinkTest` (`Ref<EventRecord[]>`). */
export class EventSink extends Context.Tag("mansio/EventSink")<EventSink, EventSinkService>() {}

// ===========================================================================
// AppRegistry — item 22 (`AppRegistryLive`), item 20 (`AppRegistryTest`)
// ===========================================================================

export interface AppRegistryService {
  get(id: string): Effect.Effect<App, UnknownAppError>;
  readonly all: Effect.Effect<readonly App[]>;
}

/** Live: `AppRegistryLive` (over `Config`). Test: `AppRegistryTest`. */
export class AppRegistry extends Context.Tag("mansio/AppRegistry")<AppRegistry, AppRegistryService>() {}

// ===========================================================================
// Storage — item 15 (`StorageLive`), item 20 (`StorageTest`)
// ===========================================================================

export interface StorageService {
  /**
   * Run a mutation handler inside one `BEGIN IMMEDIATE` transaction. An unbounded read inside it can
   * also fail `StatementDeadlineError` (FFI-05).
   */
  mutation(
    definition: AnyDefinition,
    args: unknown,
  ): Effect.Effect<unknown, MutationError | StatementDeadlineError>;
  /** `VACUUM INTO` a standalone, WAL-consistent copy. Serializes with mutations. */
  snapshot(path: string): Effect.Effect<void, StorageError>;
  /**
   * Run one decoded IR request (`ir/spec/v1.md`; the `/v1/ops` shim of IR-3) on this writer, in one
   * transaction through the writer queue: the fingerprint check, then `platform/ir/executor.ts`'s
   * `runRequest`. Queries run here too (not on the reader), so they serialize with writes.
   */
  ir(request: IrRequest): Effect.Effect<IrResponse, IrFailure | QueueFullError | StorageError>;
}

/**
 * Live: `StorageLive` (a scoped FFI SQLite connection, `platform/sqlite_ffi/`). Test: `StorageTest`
 * (in-memory doc store).
 *
 * One layer instance per app: the writer owns a single SQLite connection, so item 22 builds the
 * per-app stacks rather than passing an `appId` to every call.
 */
export class Storage extends Context.Tag("mansio/Storage")<Storage, StorageService>() {}

// ===========================================================================
// Reader — item 16 (`ReaderLive`), item 20 (`ReaderTest`)
// ===========================================================================

export interface ReaderService {
  /** Run a query handler against DuckDB inside one read transaction. */
  query(definition: AnyDefinition, args: unknown): Effect.Effect<unknown, QueryError>;
}

/** Live: `ReaderLive` (scoped DuckDB, reader process only). Test: `ReaderTest`. */
export class Reader extends Context.Tag("mansio/Reader")<Reader, ReaderService>() {}

// ===========================================================================
// ReaderClient — item 17 (`ReaderClientLive`), item 20 (`ReaderClientTest`)
// ===========================================================================

/**
 * What a call answered by the reader process can fail with. Wider than the plan's abridged
 * `IpcError`: transport failures are `IpcError` / `ReaderExitedError`, the 1000-call cap is
 * `QueueFullError`, and a `ReaderErrorFrame` rehydrates into the `RemoteReaderError` the reader
 * actually raised. See ADR 0002.
 */
export type ReaderCallError = IpcError | ReaderExitedError | QueueFullError | RemoteReaderError;

/**
 * A reader client. `E` defaults to the reader process's channel (`ReaderCallError`), which the IPC
 * client (`platform/ipc.ts`) implements; the `ReaderClient` service is the wider
 * `ServerReaderClientService`.
 */
export interface ReaderClientService<E = ReaderCallError> {
  /** Send one request to the reader process and await its correlated reply. */
  call(request: ReaderRequest): Effect.Effect<unknown, E>;
}

/**
 * The `ReaderClient` the server's routes call. Document queries are answered in this process
 * (`platform/document_reader.ts`), where an unbounded statement can also stop at its deadline:
 * `StatementDeadlineError` (FFI-05). Every `ReaderClientService` is one.
 */
export type ServerReaderClientService = ReaderClientService<ReaderCallError | StatementDeadlineError>;

/**
 * Live: `ReaderClientLive` (scoped child process). Test: `ReaderClientTest` (in-process `Reader`).
 *
 * Substituting the test layer is what lets the whole HTTP surface be exercised in one process with
 * no subprocess and no DuckDB (item 22's `tests/unit/http_test.ts`).
 */
export class ReaderClient extends Context.Tag("mansio/ReaderClient")<
  ReaderClient,
  ServerReaderClientService
>() {}

// ===========================================================================
// ObjectStore — item 18 (`ObjectStoreLocal` | `ObjectStoreS3`), item 20 (`ObjectStoreTest`)
// ===========================================================================

export interface ObjectStoreService {
  put(key: string, bytes: Uint8Array): Effect.Effect<void, ObjectStoreError | InvalidKeyError>;
  get(key: string): Effect.Effect<Uint8Array, ObjectStoreError | InvalidKeyError>;
}

/** Live: `ObjectStoreLive` selects local or S3 on `Config.objectStore.s3BaseUrl`. Test: `ObjectStoreTest`. */
export class ObjectStore extends Context.Tag("mansio/ObjectStore")<ObjectStore, ObjectStoreService>() {}

// ===========================================================================
// Backups — item 19 (`BackupsLive`)
// ===========================================================================

export interface BackupsService {
  /**
   * snapshot → `quick_check` → SHA-256 → immutable object PUT → immutable manifest PUT → only then
   * `latest.json`. A failed upload never advances `latest.json`.
   *
   * The channel stays narrow on purpose: item 19 wraps whatever `Storage` or `ObjectStore` failed
   * with into `BackupError({ phase, cause })`, so the phase that failed is always recorded.
   * `BackupAlreadyRunning` and `SnapshotIntegrityError` are the two it passes through unwrapped.
   */
  take(
    appId: string,
  ): Effect.Effect<Manifest, BackupError | BackupAlreadyRunning | SnapshotIntegrityError>;
}

/** Live: `BackupsLive` (over `Storage` + `ObjectStore` + `Clock` + `IdGen`). Tests reuse the live layer. */
export class Backups extends Context.Tag("mansio/Backups")<Backups, BackupsService>() {}

// ===========================================================================
// EventLogIngest — item 21 (`EventLogIngestLive`), item 20 (`EventLogIngestTest`)
// ===========================================================================

export interface EventLogIngestService {
  /** Idempotent: a second run over an unchanged log tree inserts nothing. Reader process only. */
  readonly ingest: Effect.Effect<IngestReport, IngestError>;
}

/** Live: `EventLogIngestLive` (DuckDB). Test: `EventLogIngestTest`. */
export class EventLogIngest extends Context.Tag("mansio/EventLogIngest")<
  EventLogIngest,
  EventLogIngestService
>() {}

// ===========================================================================
// Analysis — item 23 (`AnalysisLive`)
// ===========================================================================

export interface AnalysisService {
  /** Run a catalog query by name. Callers never supply SQL. */
  run(
    name: string,
    params: Readonly<Record<string, unknown>>,
  ): Effect.Effect<readonly Row[], AnalysisError | UnknownAnalysisQuery>;
  /** The catalog's names, for the error message `logs:query` prints on a miss. */
  readonly names: Effect.Effect<readonly string[]>;
}

/** Live: `AnalysisLive` (reader process, over the event-log DuckDB file). No test double. */
export class Analysis extends Context.Tag("mansio/Analysis")<Analysis, AnalysisService>() {}

// ===========================================================================
// TenantRegistry — MT-03 (`TenantRegistryLive`), test: ephemeral Postgres
// ===========================================================================

/**
 * Live: `TenantRegistryLive` (Postgres, over `Config.tenancy.databaseUrl`). Test: the live
 * layer against ephemeral Postgres (`make up` + `make db-migrate`); pg-mem is explicitly
 * rejected for registry tests (`docs/haiku-experiments/findings/sq-10.md`).
 */
export class TenantRegistry extends Context.Tag("mansio/TenantRegistry")<
  TenantRegistry,
  TenantRegistryService
>() {}

// ===========================================================================
// Layer-construction error channels
// ===========================================================================

/**
 * What building a layer may fail with. Declared here so items 08–23 agree on the type of their
 * `Layer.scoped` / `Layer.effect` error channels without re-deriving it.
 *
 * `StartupError` (`ConfigError | SchemaDriftError`) is deliberately absent from `PlatformError`:
 * both fail startup, so no HTTP status is ever needed for them (plan §1.3).
 */
export type LayerError = StartupError | PlatformError;
