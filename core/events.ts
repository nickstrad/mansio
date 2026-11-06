/**
 * The event taxonomy: one discriminated union over every event this platform can emit, total
 * constructors for each kind, and `toJsonLine`, which serializes a record to one canonical,
 * key-ordered JSON line for the rotating JSONL log (ADR 0001 decision 4; §4.1 of the plan).
 *
 * This module is PURE: it imports only `effect`-free types from `./errors.ts` and does no I/O.
 * Constructors never call `crypto.randomUUID()` or `Date.now()` — `eventId` and `ts` are always
 * caller-supplied, which is what makes them deterministic and property-testable.
 *
 * Redaction is load-bearing, not cosmetic (see the plan's item 07 and ADR 0002 decision 4):
 *
 * - `sql` events never carry bind values unless the constructor is given
 *   `{ includeBindValues: true }`, mirroring today's `TRACE_SQL=1` caveat that it logs
 *   application values. As defense in depth, `toJsonLine` withholds `bindValues` from any
 *   record that wasn't built with that opt-in — a hand-built record literal can't leak them.
 * - Bearer <redacted> (`Bearer <token>`), `authorization`/`proxy-authorization` headers (any
 *   casing or separator style), `Cookie`/`Set-Cookie`, S3 credential fields (any casing or
 *   separator style — `AWS_SECRET_ACCESS_KEY`, `awsAccessKeyId`, `aws_secret_access_key`,
 *   …), any key ending in `token` (`COLUMN_APP_TOKEN`, `ADMIN_TOKEN`, …), and presigned-URL
 *   `X-Amz-Credential`/`X-Amz-Signature`/`X-Amz-Security-Token` parameters are redacted
 *   unconditionally, regardless of any flag, wherever they appear in free-form payload data
 *   (`errorDetail`, `lifecycle` detail, bind values).
 * - Player ids and document ids are never redacted: they are the data this log exists to
 *   analyse.
 *
 * `toJsonLine` is total: it never throws. Item 11's `emit` is `Effect<void, never>` and calls
 * this serializer, so bigint values, cycles, hostile nesting depth, and throwing getters are
 * all neutralized; any residual failure falls back to a minimal envelope-only line carrying a
 * `"serialization":"serialization-failed"` marker.
 *
 * Field naming note: the plan's per-kind field lists for `http` and `handler` include a `kind`
 * field (the routed handler's `"query" | "mutation"`). That name collides with this union's own
 * discriminant (`EventRecord["kind"]`, one of the eight event kinds below), so the handler's
 * query/mutation distinction is carried here as `handlerKind` instead.
 *
 * Column mapping for the DuckDB tables (event-log schema v2, `core/eventlog/ddl.ts`; ADR 0006 records
 * why it changed from §4.1). Items 23 and 32 need this stated, not inferred:
 * - `eventId` → `event_id`, in `platform_events` *and* in the record's typed table. It is the record's
 *   one identity: every typed row joins to exactly one `platform_events` row on `event_id`. `ts` and
 *   `traceId` are not join keys — many events share both.
 * - The union discriminant `kind` (`"http" | "handler" | "sql" | …`) → `platform_events.event_kind`,
 *   for every kind, `handler` included.
 * - `handlerKind` → `handler_kind` (in `platform_events` for `http` and `handler` records, and in
 *   `http_events`), through an explicit `query`/`mutation` mapping; any other value is `NULL`.
 *   §4.3's `mutation-vs-query-latency` needs it.
 * - There is no `attrs` field on `EventRecord`, although `platform_events` declares `attrs JSON` and
 *   `core/errors.ts` says analysis queries "drill into `attrs.reason`": `reason` (and the rest of an
 *   error's payload) lands in `errorDetail`, so `errorDetail` → `attrs` (`detail` for `lifecycle`).
 * - `backup` records' `emitter`, `runId` and `attempts` → `backup_events.emitter`, `run_id` and
 *   `attempts`; see `BackupPayload` for what they count.
 */
import type { AnyError, BackupPhase, HandlerKind, QueueName } from "./errors.ts";

// ---------------------------------------------------------------------------
// Shared envelope
// ---------------------------------------------------------------------------

export type Level = "debug" | "info" | "warn" | "error";
export type Role = "server" | "reader" | "restore" | "cli";

/** Fields every event record carries, regardless of kind. */
export interface EventEnvelope {
  readonly eventId: string;
  readonly ts: string;
  readonly level: Level;
  readonly role: Role;
  readonly pid: number;
  readonly traceId: string;
  readonly spanId?: string;
  readonly parentSpanId?: string;
  readonly appId?: string;
}

/** `fromPlatformError`'s output: a stable, low-cardinality `error_tag` plus the structured payload. */
export interface ErrorColumns {
  readonly errorTag: string;
  readonly errorDetail: unknown;
}

// ---------------------------------------------------------------------------
// Per-kind payloads
// ---------------------------------------------------------------------------

export type SqlEngine = "sqlite" | "duckdb";
export type SqlStatementKind = "select" | "insert" | "update" | "delete" | "ddl" | "pragma";
export type TxOutcome = "commit" | "rollback" | "interrupted";
export type QueueSignal = "high-water" | "rejected" | "dropped";
export type Outcome = "ok" | "error";

export interface HttpPayload {
  readonly method: string;
  readonly path: string;
  readonly handlerKind: HandlerKind;
  readonly name: string;
  readonly status: number;
  readonly durationMs: number;
  readonly bytesIn: number;
  readonly bytesOut: number;
  readonly errorTag?: string;
  readonly errorDetail?: unknown;
}

export interface HandlerPayload {
  readonly handlerKind: HandlerKind;
  readonly name: string;
  readonly outcome: Outcome;
  readonly durationMs: number;
  readonly errorTag?: string;
  readonly errorDetail?: unknown;
}

/**
 * Which step path a SQLite statement ran on (FFI-05): `blocking` on the main thread, `nonblocking`
 * on Deno's blocking pool under the statement deadline. Absent on DuckDB statements.
 */
export type SqlPath = "blocking" | "nonblocking";

export interface SqlPayload {
  readonly engine: SqlEngine;
  readonly dialect: string;
  readonly statementKind: SqlStatementKind;
  readonly tableName?: string;
  readonly sqlText: string;
  readonly bindCount: number;
  readonly bindValues?: readonly unknown[];
  readonly durationMs: number;
  readonly rowCount?: number;
  readonly outcome: Outcome;
  readonly errorTag?: string;
  readonly errorDetail?: unknown;
  /**
   * The SQLite step path (FFI-05); set on every event the writer and the document reader emit.
   * Persisted in JSONL and the nullable `sql_events.path` column (FFI-06).
   */
  readonly path?: SqlPath;
}

export interface SqlEventOptions {
  /** Default false: bind values are never logged unless explicitly opted in. */
  readonly includeBindValues?: boolean;
}

export interface TxPayload {
  readonly engine: SqlEngine;
  readonly outcome: TxOutcome;
  readonly operations: number;
  readonly durationMs: number;
  readonly queueWaitMs: number;
  readonly errorTag?: string;
  readonly errorDetail?: unknown;
}

export interface QueuePayload {
  readonly queue: QueueName;
  readonly depth: number;
  readonly cap: number;
  readonly event: QueueSignal;
  readonly dropped: number;
}

/**
 * Which producer wrote a `backup` record. `backups` (agentd's `POST /admin/backup`; the Deno
 * platform's backup orchestrator until IR-6) writes one orchestration record per phase of a run;
 * `object-store` (the Deno platform's object store) wrote one transfer record per object PUT. The two never describe the same thing, so neither double-counts the
 * other.
 */
export type BackupEmitter = "backups" | "object-store";

/**
 * One backup phase (`emitter: "backups"`) or one object transfer (`emitter: "object-store"`).
 *
 * Accounting (ADR 0006): a backup run is one `runId`; its phases are its `backups` records, exactly one
 * per phase reached; its transfers are its `object-store` records, one per PUT, each linked to the phase
 * that issued it by `(runId, key)`. Retries are inside a transfer (`attempts - 1`), never extra records.
 */
export interface BackupPayload {
  readonly phase: BackupPhase;
  readonly emitter: BackupEmitter;
  /** The backup run (one `Backups.take`). Absent on a transfer issued outside any run. */
  readonly runId?: string;
  readonly key?: string;
  readonly bytes?: number;
  readonly sha256?: string;
  /**
   * `object-store` only: requests sent to the backend for this one operation — 1 plus retries, or 0
   * when the key was rejected before anything was sent.
   */
  readonly attempts?: number;
  readonly durationMs: number;
  readonly outcome: Outcome;
  readonly errorTag?: string;
  readonly errorDetail?: unknown;
}

export interface LifecyclePayload {
  readonly phase: string;
  readonly detail?: unknown;
}

export interface IngestPayload {
  readonly sourceFile: string;
  readonly bytesIngested: number;
  readonly runId?: string;
  readonly status?: Outcome;
  readonly errorTag?: string;
  readonly errorDetail?: unknown;
}

// ---------------------------------------------------------------------------
// EventRecord: the discriminated union over `kind`
// ---------------------------------------------------------------------------

/** One variant per event kind; constructors return these so fields typecheck without narrowing. */
export type HttpEventRecord = EventEnvelope & { readonly kind: "http" } & HttpPayload;
export type HandlerEventRecord = EventEnvelope & { readonly kind: "handler" } & HandlerPayload;
export type SqlEventRecord = EventEnvelope & { readonly kind: "sql" } & SqlPayload;
export type TxEventRecord = EventEnvelope & { readonly kind: "tx" } & TxPayload;
export type QueueEventRecord = EventEnvelope & { readonly kind: "queue" } & QueuePayload;
export type BackupEventRecord = EventEnvelope & { readonly kind: "backup" } & BackupPayload;
export type LifecycleEventRecord = EventEnvelope & { readonly kind: "lifecycle" } & LifecyclePayload;
export type IngestEventRecord = EventEnvelope & { readonly kind: "ingest" } & IngestPayload;

export type EventRecord =
  | HttpEventRecord
  | HandlerEventRecord
  | SqlEventRecord
  | TxEventRecord
  | QueueEventRecord
  | BackupEventRecord
  | LifecycleEventRecord
  | IngestEventRecord;

// ---------------------------------------------------------------------------
// Constructors — total, deterministic, one per kind
// ---------------------------------------------------------------------------

export function httpEvent(input: EventEnvelope & HttpPayload): HttpEventRecord {
  return { ...input, kind: "http" };
}

export function handlerEvent(input: EventEnvelope & HandlerPayload): HandlerEventRecord {
  return { ...input, kind: "handler" };
}

/**
 * Opt-in marker set only by `sqlEvent` when the caller passes `{ includeBindValues: true }`.
 * Symbols never appear in `Object.keys`/`JSON.stringify`, so this can't leak into the log;
 * `canonicalize` reads it as the backstop that withholds `bindValues` from hand-built records.
 */
const BIND_VALUES_INCLUDED = Symbol.for("mansio.events.bindValuesIncluded");

export function sqlEvent(
  input: EventEnvelope & SqlPayload,
  opts: SqlEventOptions = {},
): SqlEventRecord {
  const includeBindValues = opts.includeBindValues ?? false;
  const { bindValues, ...rest } = input;
  const record: SqlEventRecord = includeBindValues
    ? { ...rest, bindValues, kind: "sql" }
    : { ...rest, kind: "sql" };
  if (includeBindValues) {
    // Excess-property checks reject a symbol key in the literal above, so mark it afterwards.
    (record as unknown as Record<symbol, unknown>)[BIND_VALUES_INCLUDED] = true;
  }
  return record;
}

export function txEvent(input: EventEnvelope & TxPayload): TxEventRecord {
  return { ...input, kind: "tx" };
}

export function queueEvent(input: EventEnvelope & QueuePayload): QueueEventRecord {
  return { ...input, kind: "queue" };
}

export function backupEvent(input: EventEnvelope & BackupPayload): BackupEventRecord {
  return { ...input, kind: "backup" };
}

export function lifecycleEvent(input: EventEnvelope & LifecyclePayload): LifecycleEventRecord {
  return { ...input, kind: "lifecycle" };
}

export function ingestEvent(input: EventEnvelope & IngestPayload): IngestEventRecord {
  return { ...input, kind: "ingest" };
}

// ---------------------------------------------------------------------------
// fromPlatformError
// ---------------------------------------------------------------------------

/**
 * Splits a repo error into a stable, low-cardinality `errorTag` plus the structured payload
 * (`errorDetail`), which `toJsonLine` redacts like everything else.
 *
 * Accepts any `AnyError` (see `core/errors.ts`: "Every error this repo defines. Useful for
 * `core/events.ts`'s `error_tag` column"), so `ConfigError`, `SchemaDriftError`, `RestoreError`
 * and `RemoteError` need no cast. Non-object inputs (a foreign `throw "boom"`, `null`, …)
 * yield `errorTag: "UnknownError"` instead of throwing — defects reaching item 11's logger are
 * exactly where this gets called.
 *
 * `cause` needs explicit handling: every repo error is a `Data.TaggedError`, and Effect routes
 * `cause` through the `Error` constructor options, where V8 defines it as a NON-enumerable own
 * property — so a plain `{ _tag, ...rest }` spread silently drops it (likewise `stack`;
 * `message` is absent on these classes, and neither leaks into the log). `cause` carries the
 * underlying driver error, the single most useful diagnostic for the classes that declare it
 * (`StorageError`, `ReaderError`, `IpcError`, `ObjectStoreError`, `BackupError`, `IngestError`,
 * `AnalysisError`), so it is read explicitly here.
 */
export function fromPlatformError(e: AnyError): ErrorColumns {
  if (e === null || e === undefined || typeof e !== "object") {
    return { errorTag: "UnknownError", errorDetail: { value: String(e) } };
  }
  const source = e as unknown as Record<string, unknown>;
  const detail: Record<string, unknown> = {};
  for (const key of Object.keys(source)) {
    if (key === "_tag") continue;
    detail[key] = safeRead(source, key);
  }
  if ("cause" in source) detail["cause"] = safeRead(source, "cause");
  const tag = source["_tag"];
  return { errorTag: typeof tag === "string" ? tag : "UnknownError", errorDetail: detail };
}

// ---------------------------------------------------------------------------
// toJsonLine: canonical key order, redaction, newline sanitization
// ---------------------------------------------------------------------------

const ENVELOPE_KEYS = [
  "eventId",
  "ts",
  "level",
  "role",
  "pid",
  "traceId",
  "spanId",
  "parentSpanId",
  "appId",
] as const;

const KIND_KEYS: Record<EventRecord["kind"], readonly string[]> = {
  http: [
    "method",
    "path",
    "handlerKind",
    "name",
    "status",
    "durationMs",
    "bytesIn",
    "bytesOut",
    "errorTag",
    "errorDetail",
  ],
  handler: ["handlerKind", "name", "outcome", "durationMs", "errorTag", "errorDetail"],
  sql: [
    "engine",
    "dialect",
    "statementKind",
    "path",
    "tableName",
    "sqlText",
    "bindCount",
    "bindValues",
    "durationMs",
    "rowCount",
    "outcome",
    "errorTag",
    "errorDetail",
  ],
  tx: ["engine", "outcome", "operations", "durationMs", "queueWaitMs", "errorTag", "errorDetail"],
  queue: ["queue", "depth", "cap", "event", "dropped"],
  backup: [
    "phase",
    "emitter",
    "runId",
    "key",
    "bytes",
    "sha256",
    "attempts",
    "durationMs",
    "outcome",
    "errorTag",
    "errorDetail",
  ],
  lifecycle: ["phase", "detail"],
  ingest: ["sourceFile", "bytesIngested", "runId", "status", "errorTag", "errorDetail"],
};

/** Property read that can't throw: a throwing getter becomes the `"<unreadable>"` marker. */
function safeRead(source: Record<string, unknown>, key: string): unknown {
  try {
    return source[key];
  } catch {
    return "<unreadable>";
  }
}

/** Builds the canonical, fully-populated (absent optionals as `null`) plain object for a record. */
function canonicalize(record: EventRecord): Record<string, unknown> {
  const r = record as unknown as Record<string, unknown>;
  const out: Record<string, unknown> = {};
  for (const key of ENVELOPE_KEYS) out[key] = safeRead(r, key) ?? null;
  const kind = safeRead(r, "kind");
  out["kind"] = kind;
  const kindKeys: readonly string[] | undefined = KIND_KEYS[kind as EventRecord["kind"]];
  for (const key of kindKeys ?? []) {
    const v = safeRead(r, key);
    out[key] = v === undefined ? null : v;
  }
  if (
    kind === "sql" &&
    (record as unknown as Record<symbol, unknown>)[BIND_VALUES_INCLUDED] !== true
  ) {
    // Defense in depth: bind values reach the log only when the `sqlEvent` constructor was
    // explicitly given `{ includeBindValues: true }` (marked via BIND_VALUES_INCLUDED). A
    // hand-built record literal carries no opt-in marker, so its `bindValues` are withheld
    // here — `toJsonLine` never leaks them regardless of how the record was constructed.
    out["bindValues"] = null;
  }
  return out;
}

const BEARER_PATTERN = /Bearer\s+\S+/gi;
const AMZ_PARAM_PATTERN = /X-Amz-(Signature|Credential|Security-Token)=[^&\s"']+/gi;
// Keys are normalized (lower-cased, separators stripped) and matched on SUFFIX, so every
// spelling is caught: `AWS_SECRET_ACCESS_KEY` → `awssecretaccesskey`,
// `Proxy-Authorization` → `proxyauthorization`, `Set-Cookie` → `setcookie`,
// `COLUMN_APP_TOKEN` → `columnapptoken`.
const SECRET_KEY_PATTERN =
  /(authorization|accesskeyid|secretaccesskey|sessiontoken|securitytoken|cookie|token)$/;

function normalizeKey(key: string): string {
  return key.toLowerCase().replace(/[^a-z]/g, "");
}

function redactString(s: string): string {
  return s
    .replace(BEARER_PATTERN, "Bearer <redacted>")
    .replace(AMZ_PARAM_PATTERN, "X-Amz-$1=<redacted>");
}

function sanitizeNewlines(s: string): string {
  return s.replace(/[\r\n\u2028\u2029]+/g, " ");
}

/** Nesting deeper than this is replaced with `"<max-depth>"` — hostile input can't blow the stack. */
const MAX_SERIALIZE_DEPTH = 32;

/**
 * Depth-first redaction (Bearer <redacted>, secret keys, X-Amz URL params) and newline
 * sanitization. Total by construction:
 * - `bigint` coerces to its decimal string (`DecodeError.actual` is a DuckDB BIGINT),
 * - cycles become `"<circular>"` and over-deep structures `"<max-depth>"`,
 * - throwing getters become `"<unreadable>"`,
 * - nested object keys are sorted, so equal payloads serialize identically regardless of
 *   caller insertion order.
 */
function redactAndSanitize(value: unknown, depth: number, seen: Set<object>): unknown {
  if (typeof value === "string") return sanitizeNewlines(redactString(value));
  if (typeof value === "bigint") return value.toString();
  if (value === null || typeof value !== "object") return value;
  if (seen.has(value)) return "<circular>";
  if (depth >= MAX_SERIALIZE_DEPTH) return "<max-depth>";
  seen.add(value);
  try {
    if (Array.isArray(value)) {
      return value.map((item) => redactAndSanitize(item, depth + 1, seen));
    }
    const source = value as Record<string, unknown>;
    const out: Record<string, unknown> = {};
    for (const key of Object.keys(source).sort()) {
      const cleanKey = sanitizeNewlines(key);
      out[cleanKey] = SECRET_KEY_PATTERN.test(normalizeKey(key))
        ? "<redacted>"
        : redactAndSanitize(safeRead(source, key), depth + 1, seen);
    }
    return out;
  } finally {
    seen.delete(value);
  }
}

/**
 * Serializes a record to one canonical, key-ordered, newline-free JSON line.
 *
 * Total: item 11's `emit` is `Effect<void, never>` and calls this serializer, so it must never
 * fail. `bigint` coerces to string, cycles and over-deep structures become markers, and throwing
 * getters become `"<unreadable>"`; any residual failure (a hostile Proxy, …) falls back to a
 * minimal envelope-only line carrying a `"serialization":"serialization-failed"` marker.
 */
export function toJsonLine(record: EventRecord): string {
  try {
    const ordered = canonicalize(record);
    // The top level keeps canonicalize's fixed key order — redact each value in place
    // without re-sorting. Nested objects are still sorted inside redactAndSanitize.
    const seen = new Set<object>();
    const redacted: Record<string, unknown> = {};
    for (const key of Object.keys(ordered)) {
      redacted[key] = redactAndSanitize(ordered[key], 1, seen);
    }
    const line = JSON.stringify(redacted);
    if (typeof line !== "string") return fallbackJsonLine(record);
    return line;
  } catch {
    return fallbackJsonLine(record);
  }
}

/**
 * Last-resort serializer: envelope fields only, each read and stringified defensively, so a log
 * line is always produced and always carries the failure marker.
 */
function fallbackJsonLine(record: EventRecord): string {
  const parts = ['"serialization":"serialization-failed"'];
  const source = record as unknown as Record<string, unknown>;
  for (const key of ENVELOPE_KEYS) {
    parts.push(`${JSON.stringify(key)}:${safeJson(safeRead(source, key) ?? null)}`);
  }
  return `{${parts.join(",")}}`;
}

/** `JSON.stringify` that returns a string no matter what the input does. */
function safeJson(value: unknown): string {
  try {
    const line = JSON.stringify(redactAndSanitize(value, 0, new Set()));
    return typeof line === "string" ? line : "null";
  } catch {
    return '"<unserializable>"';
  }
}
