/**
 * The typed event tables (plan items 21 and 35, §4.1): how one ingested JSONL record becomes a row in
 * each DuckDB table, plus the one-off v1 → v2 schema upgrade. The ingest statements are statements
 * only — `eventlog/ingest.ts` decides when to run them, inside the one transaction per batch
 * that also moves `_ingest_state`.
 *
 * Every statement here against an event table is an `INSERT ... SELECT` from the batch's staging
 * relations (or, in `upgradeEventLog`, from a rebuilt copy). Nothing in this module rewrites or
 * removes a row.
 *
 * ## Staging
 *
 * `ingest.ts` writes each source file's new complete lines to an offset-trimmed temp file, one
 * wrapper object per line: `{"source_file", "source_offset", "dedupe", "line"}`, where `line` is the
 * original line *as a JSON string*. `read_json_auto` over that explicit file list therefore never
 * parses a record itself, so one malformed record cannot fail the read, and DuckDB's JSON functions
 * are the single judge of what is well-formed:
 *
 * - `ingest_stage`: every staged line plus a `reject_reason` (`NULL` when the line is ingestible).
 * - `ingest_rows`: the ingestible lines as `JSON`, minus — for a file that is or was rewritten only —
 *   lines whose `eventId` is already in `platform_events` (see `core/eventlog/offsets.ts`).
 *
 * A line is ingestible iff it is valid JSON, an object, has a non-empty string `eventId`, and has a
 * `ts` DuckDB can read as a timestamp. A wrong-typed *field* is not a reject: that column is `NULL`.
 *
 * ## Column mapping (schema v2, ADR 0006)
 *
 * Every ingestible record lands in `platform_events`; records of a typed kind also land in that
 * kind's table (`TYPED_EVENT_TABLES`). `handler` and `ingest` records have no typed table. As
 * `core/events.ts` documents:
 *
 * - **Identity.** `event_id` is the record's `eventId`, in `platform_events` and in its typed table.
 *   Both inserts read the same `ingest_rows`, in one transaction, so every typed row has exactly one
 *   `platform_events` row with its `event_id`, and that row's `event_kind` is the typed table's kind.
 *   Duplicates follow the offset model's rules (see "Identity under duplicates" below).
 * - **Taxonomy.** `platform_events.event_kind` is the record's `kind`, always. `handler_kind` is
 *   `handlerKind` mapped explicitly — `query` → `query`, `mutation` → `mutation`, anything else
 *   `NULL` — for `http` and `handler` records, and `NULL` for every other kind. `http_events.handler_kind`
 *   is the same mapping. Legacy JSONL shaped after §4.1's original field list, a handler record whose
 *   `kind` is itself `query` or `mutation`, maps to `event_kind = 'handler'` with that `handler_kind`.
 * - `platform_events.attrs` is `errorDetail` (for `lifecycle` records, `detail`).
 * - `platform_events.name` is the record's most specific label: `name` (http, handler),
 *   `statementKind` (sql), `queue` (queue), `phase` (backup, lifecycle), `sourceFile` (ingest).
 * - `platform_events.outcome` keeps each kind's own vocabulary, so item 23 can tell failures apart:
 *   `outcome` (`ok`/`error`; for `tx`, `commit`/`rollback`/`interrupted`), `event` (queue), `status`
 *   (ingest), and for `http` — which has no outcome field — `error` when `errorTag` is set or `status`
 *   is 5xx, else `ok` (a 5xx answered without a tagged error is still a server failure).
 * - **Backups.** `backup_events.emitter` is `emitter` (`backups` or `object-store`), `run_id` is
 *   `runId`, `attempts` is `attempts`. A record written before item 35 has no `emitter`; it is inferred
 *   from what each producer wrote then: `put-object` came from the object store, except the record
 *   `Backups` wrote for an interrupted upload (`errorTag` `Interrupted`, `errorDetail.written`
 *   `unknown`); every other phase came from `Backups`. Such records have no `run_id`.
 * - `ts` is read as `TIMESTAMPTZ` and stored as its UTC wall time, so a `+02:00` offset is honoured
 *   rather than dropped (a plain `CAST(... AS TIMESTAMP)` silently ignores the offset).
 *
 * ## Identity under duplicates
 *
 * The same `eventId` can reach ingest twice only through a rewritten source (offsets.ts): a re-read
 * generation, or two copies inside one batch. Lines of a rewritten file land only if their `eventId`
 * is not already in `platform_events` and is the batch's first copy, so the canonical row and its typed
 * row exist once. Re-running ingest over an unchanged tree reads nothing. Duplicate *production* — an
 * outside process copying lines into a file whose ingested prefix it keeps intact — is outside the
 * offset model (ADR 0004): it would land twice, in both tables, and `SELECT event_id FROM
 * platform_events GROUP BY 1 HAVING count(*) > 1` finds it. The platform's own writers never do it.
 *
 * ## Large integers
 *
 * The item-13 review found SQLite's `json_extract` mishandles integral doubles ≥ 2^53. DuckDB
 * 1.5.6's JSON reader (yyjson), probed with `agent-work/work-scripts/deno/duckdb-json-bigint-probe.ts`:
 *
 * - An integer literal (`9007199254740993`) is typed `BIGINT`/`UBIGINT` and extracts exactly.
 * - A literal with a fraction or exponent (`9007199254740993.0`, `1e16`) is typed `DOUBLE` and has
 *   already been rounded to the nearest double (`...992`) by the time SQL sees it.
 * - `TRY_CAST('12.5' AS BIGINT)` *rounds* to 13, and out-of-range values become `NULL`.
 *
 * So `intOf` never casts a `DOUBLE` it cannot prove exact: an integral `DOUBLE` with magnitude
 * ≤ 2^53 - 1 is accepted, any other `DOUBLE` becomes `NULL`, a string is accepted only if it is all
 * digits (that is how `core/events.ts` serialises a `bigint`), and anything outside the column's
 * range is `NULL`. A `NULL` is an honest "unknown"; a silently rounded count is not. Note the JSONL
 * producers are JavaScript, so a `number` past 2^53 has lost precision *before* it is written —
 * DuckDB stores exactly what the line says, which is the most this side can promise.
 */
import { dirname } from "node:path";
import { Clock, Effect, Exit } from "effect";
import { IngestError } from "../core/errors.ts";
import { lifecycleEvent, type Role } from "../core/events.ts";
import {
  EVENTLOG_DDL,
  EVENTLOG_SCHEMA_VERSION,
  INGEST_STATE_TABLE,
  SCHEMA_TABLE,
  TYPED_EVENT_TABLES,
} from "../core/eventlog/ddl.ts";
import { Config, EventSink, type IngestReport } from "./services.ts";
// Type-only: erased at runtime, so there is no import cycle. The values are imported lazily below.
import type { EventLogConnection, EventLogDriver } from "./ingest.ts";

export { EVENTLOG_DDL };

/** The staging relations. Temp tables: connection-local, never persisted to the event-log file. */
export const STAGE_TABLE = "ingest_stage";
export const ROWS_TABLE = "ingest_rows";

/** Why a staged line was quarantined. The first two are decided in `ingest.ts` before staging. */
export type RejectReason =
  | "invalid-utf8"
  | "line-too-large"
  | "invalid-json"
  | "not-an-object"
  | "missing-event-id"
  | "invalid-ts";

/** The per-line wrapper `ingest.ts` writes to the temp files `read_json_auto` reads. */
export interface StagedLine {
  readonly source_file: string;
  readonly source_offset: number;
  readonly dedupe: boolean;
  readonly line: string;
}

/**
 * Upper bound on one wrapper object, in bytes. `ingest.ts` rejects source lines over
 * `MAX_LINE_BYTES` before staging; JSON-string escaping grows a line by at most 6x (`\u0000`), so
 * this leaves room for every line that gets through.
 */
export const MAX_LINE_BYTES = 1024 * 1024;
const MAX_OBJECT_BYTES = 8 * MAX_LINE_BYTES;

/**
 * Replaces each `\\uXXXX` escape of an unpaired UTF-16 surrogate with `\\ufffd`.
 *
 * `core/events.ts` serialises with `JSON.stringify`, which writes a lone surrogate (a client can put
 * one in any string that reaches `errorDetail`) as an escape. `JSON.parse` accepts that, but
 * DuckDB's `json_valid` rejects the whole line, so without this a client could keep its own error
 * events out of the tables. The replacement is the character DuckDB would need to substitute anyway.
 * Escaped backslashes are skipped, so `\\\\ud800` (a literal backslash, then `ud800`) is untouched.
 * Valid UTF-8 cannot hold a raw lone surrogate, so escapes are the only form to repair.
 */
export function repairLoneSurrogates(line: string): string {
  if (!line.includes("\\u")) return line;
  const hex4 = /^[0-9a-fA-F]{4}$/;
  let out = "";
  let i = 0;
  while (i < line.length) {
    if (line[i] !== "\\") {
      out += line[i];
      i += 1;
      continue;
    }
    if (line[i + 1] !== "u" || !hex4.test(line.slice(i + 2, i + 6))) {
      out += line.slice(i, i + 2); // any other escape, including an escaped backslash
      i += 2;
      continue;
    }
    const code = parseInt(line.slice(i + 2, i + 6), 16);
    const isHigh = code >= 0xd800 && code <= 0xdbff;
    const isLow = code >= 0xdc00 && code <= 0xdfff;
    if (isHigh && line[i + 6] === "\\" && line[i + 7] === "u" && hex4.test(line.slice(i + 8, i + 12))) {
      const next = parseInt(line.slice(i + 8, i + 12), 16);
      if (next >= 0xdc00 && next <= 0xdfff) {
        out += line.slice(i, i + 12); // a well-formed pair
        i += 12;
        continue;
      }
    }
    out += isHigh || isLow ? "\\ufffd" : line.slice(i, i + 6);
    i += 6;
  }
  return out;
}

/** A single-quoted SQL string literal. */
export function sqlString(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

/** Stages the batch's temp files. Not called with an empty list: `read_json_auto([])` does not bind. */
export function stageSql(files: readonly string[]): string {
  const list = `[${files.map(sqlString).join(", ")}]`;
  return `CREATE OR REPLACE TEMP TABLE ${STAGE_TABLE} AS
SELECT source_file, source_offset, dedupe, line,
  CASE
    WHEN NOT json_valid(line) THEN 'invalid-json'
    WHEN json_type(line) <> 'OBJECT' THEN 'not-an-object'
    WHEN json_type(line, '$.eventId') IS DISTINCT FROM 'VARCHAR' OR (line ->> '$.eventId') = ''
      THEN 'missing-event-id'
    WHEN json_type(line, '$.ts') IS DISTINCT FROM 'VARCHAR'
      OR TRY_CAST((line ->> '$.ts') AS TIMESTAMPTZ) IS NULL THEN 'invalid-ts'
  END AS reject_reason
FROM read_json_auto(${list},
  format = 'newline_delimited',
  columns = {source_file: 'VARCHAR', source_offset: 'BIGINT', dedupe: 'BOOLEAN', line: 'VARCHAR'},
  maximum_object_size = ${MAX_OBJECT_BYTES})`;
}

/**
 * The ingestible rows. A line flagged `dedupe` lands only if its `eventId` is not already in
 * `platform_events` and is the first ingestible line with that id in this batch (file, then offset
 * order), so two flagged copies in one batch cannot both land. Unflagged lines are trusted: they come
 * from past a verified prefix of a file that was never rewritten.
 *
 * `dedupe` says whether any staged line is flagged. Without one, the anti-join is left out of the SQL
 * entirely: DuckDB plans `NOT r.dedupe OR (… NOT EXISTS …)` as a MARK delim-join that scans all of
 * `platform_events` even when no row is flagged, which makes every batch cost O(history).
 */
export function rowsSql(dedupe: boolean): string {
  if (!dedupe) {
    return `CREATE OR REPLACE TEMP TABLE ${ROWS_TABLE} AS
SELECT source_file, source_offset, line::JSON AS j
FROM ${STAGE_TABLE}
WHERE reject_reason IS NULL`;
  }
  return `CREATE OR REPLACE TEMP TABLE ${ROWS_TABLE} AS
SELECT source_file, source_offset, j FROM (
  SELECT s.source_file, s.source_offset, s.dedupe, s.line::JSON AS j, (s.line ->> '$.eventId') AS event_id,
    row_number() OVER (PARTITION BY (s.line ->> '$.eventId') ORDER BY s.source_file, s.source_offset) AS occurrence
  FROM ${STAGE_TABLE} s
  WHERE s.reject_reason IS NULL
) r
WHERE NOT r.dedupe OR (
  r.occurrence = 1 AND NOT EXISTS (SELECT 1 FROM platform_events p WHERE p.event_id = r.event_id)
)`;
}

/** Lines to quarantine, in file order. */
export const rejectedSql = `SELECT source_file, source_offset, reject_reason, line
FROM ${STAGE_TABLE} WHERE reject_reason IS NOT NULL
ORDER BY source_file, source_offset`;

/** Per source file: ingestible lines staged, and rows actually inserted (they differ only by dedupe). */
export const countsSql = `SELECT s.source_file,
  count(*) FILTER (WHERE s.reject_reason IS NULL) AS valid,
  (SELECT count(*) FROM ${ROWS_TABLE} r WHERE r.source_file = s.source_file) AS inserted
FROM ${STAGE_TABLE} s GROUP BY s.source_file`;

export const dropStagingSql = [
  `DROP TABLE IF EXISTS ${ROWS_TABLE}`,
  `DROP TABLE IF EXISTS ${STAGE_TABLE}`,
] as const;

// ---------------------------------------------------------------------------
// Field extraction from `ingest_rows.j`
// ---------------------------------------------------------------------------

const MAX_SAFE_INTEGER = 9007199254740991;

function path(field: string): string {
  return sqlString(`$.${field}`);
}

/** A scalar as text; `NULL` for a missing key or JSON `null`. */
function textOf(field: string): string {
  return `(j ->> ${path(field)})`;
}

/**
 * A finite number as `DOUBLE`; `NULL` for anything else. DuckDB's JSON parser accepts `Infinity`,
 * `NaN` and overflowing literals (`1e999`), none of which is a duration.
 */
function doubleOf(field: string): string {
  const value = `TRY_CAST(${textOf(field)} AS DOUBLE)`;
  return `CASE WHEN json_type(j, ${path(field)}) IN ('BIGINT', 'UBIGINT', 'DOUBLE') AND isfinite(${value}) ` +
    `THEN ${value} END`;
}

/** An exact integer of `type`, or `NULL` — see "Large integers" above. */
function intOf(field: string, type: "INTEGER" | "BIGINT"): string {
  const text = textOf(field);
  const asDouble = `TRY_CAST(${text} AS DOUBLE)`;
  const big = `CASE json_type(j, ${path(field)})
      WHEN 'BIGINT' THEN TRY_CAST(${text} AS BIGINT)
      WHEN 'UBIGINT' THEN TRY_CAST(${text} AS BIGINT)
      WHEN 'DOUBLE' THEN CASE WHEN abs(${asDouble}) <= ${MAX_SAFE_INTEGER} AND ${asDouble} = floor(${asDouble})
        THEN CAST(${asDouble} AS BIGINT) END
      WHEN 'VARCHAR' THEN CASE WHEN regexp_full_match(${text}, '-?[0-9]{1,19}') THEN TRY_CAST(${text} AS BIGINT) END
    END`;
  return type === "BIGINT" ? `(${big})` : `TRY_CAST((${big}) AS INTEGER)`;
}

/** A JSON value; `NULL` for a missing key or JSON `null`. */
function jsonOf(field: string): string {
  return `CASE WHEN coalesce(json_type(j, ${path(field)}), 'NULL') <> 'NULL' THEN (j -> ${path(field)}) END`;
}

const TS = `(TRY_CAST(${textOf("ts")} AS TIMESTAMPTZ) AT TIME ZONE 'UTC')`;
/** The record's `kind` as written: the union discriminant, or `query`/`mutation` in legacy handler records. */
const KIND = textOf("kind");

/** The canonical event kind: the discriminant, with legacy `query`/`mutation` handler records mapped to `handler`. */
const EVENT_KIND = `CASE WHEN ${KIND} IN ('query', 'mutation') THEN 'handler' ELSE ${KIND} END`;

/** The explicit `handlerKind` mapping. Anything but `query` or `mutation` is `NULL`. */
const HANDLER_KIND_OF_FIELD = `CASE ${
  textOf("handlerKind")
} WHEN 'query' THEN 'query' WHEN 'mutation' THEN 'mutation' END`;

/** `handler_kind` for any record: from `handlerKind` for http/handler records, from legacy `kind`, else `NULL`. */
const HANDLER_KIND = `CASE WHEN ${KIND} IN ('http', 'handler') THEN ${HANDLER_KIND_OF_FIELD} ` +
  `WHEN ${KIND} IN ('query', 'mutation') THEN ${KIND} END`;

/** `true` when `field` is present and not JSON `null`. */
function hasValue(field: string): string {
  return `coalesce(json_type(j, ${path(field)}), 'NULL') <> 'NULL'`;
}

/** `backup_events.emitter`: explicit when written, inferred for pre-item-35 records (see the module doc). */
const EMITTER = `CASE
    WHEN ${hasValue("emitter")} THEN CASE ${textOf("emitter")} WHEN 'backups' THEN 'backups' ` +
  `WHEN 'object-store' THEN 'object-store' END
    WHEN ${textOf("phase")} = 'put-object' AND ${textOf("errorTag")} = 'Interrupted' ` +
  `AND (j ->> '$.errorDetail.written') = 'unknown' THEN 'backups'
    WHEN ${textOf("phase")} = 'put-object' THEN 'object-store'
    WHEN ${textOf("phase")} IN ('snapshot', 'verify', 'hash', 'put-manifest', 'publish-latest') THEN 'backups'
  END`;

const EVENT_ID: readonly [string, string] = ["event_id", textOf("eventId")];

/** One event table's insert: target columns, the expression for each, and which records qualify. */
interface TableInsert {
  readonly table: string;
  readonly columns: readonly (readonly [column: string, expression: string])[];
  /** `undefined`: every ingestible record. */
  readonly kind?: string;
}

function insertSql({ table, columns, kind }: TableInsert): string {
  const where = kind === undefined ? "" : `\nWHERE ${KIND} = ${sqlString(kind)}`;
  return `INSERT INTO ${table} (${columns.map(([column]) => column).join(", ")})
SELECT ${columns.map(([column, expression]) => `${expression} AS ${column}`).join(",\n  ")}
FROM ${ROWS_TABLE}${where}
ORDER BY source_file, source_offset`;
}

const PLATFORM_EVENTS: TableInsert = {
  table: "platform_events",
  columns: [
    EVENT_ID,
    ["ts", TS],
    ["level", textOf("level")],
    ["role", textOf("role")],
    ["pid", intOf("pid", "INTEGER")],
    ["trace_id", textOf("traceId")],
    ["span_id", textOf("spanId")],
    ["parent_span_id", textOf("parentSpanId")],
    ["app_id", textOf("appId")],
    ["event_kind", EVENT_KIND],
    ["handler_kind", HANDLER_KIND],
    [
      "name",
      `CASE ${EVENT_KIND} WHEN 'http' THEN ${textOf("name")} WHEN 'handler' THEN ${textOf("name")} ` +
      `WHEN 'sql' THEN ${textOf("statementKind")} WHEN 'queue' THEN ${textOf("queue")} ` +
      `WHEN 'backup' THEN ${textOf("phase")} WHEN 'lifecycle' THEN ${textOf("phase")} ` +
      `WHEN 'ingest' THEN ${textOf("sourceFile")} END`,
    ],
    [
      "outcome",
      `CASE ${EVENT_KIND} WHEN 'queue' THEN ${textOf("event")} WHEN 'ingest' THEN ${textOf("status")} ` +
      `WHEN 'http' THEN CASE WHEN ${hasValue("errorTag")} OR ${intOf("status", "INTEGER")} >= 500 ` +
      `THEN 'error' ELSE 'ok' END ` +
      `ELSE ${textOf("outcome")} END`,
    ],
    ["error_tag", textOf("errorTag")],
    ["duration_ms", doubleOf("durationMs")],
    [
      "attrs",
      `CASE WHEN ${EVENT_KIND} = 'lifecycle' THEN ${jsonOf("detail")} ELSE ${jsonOf("errorDetail")} END`,
    ],
    ["source_file", "source_file"],
    ["source_offset", "source_offset"],
  ],
};

/**
 * Each typed table's columns after `event_id`. Keyed like `TYPED_EVENT_TABLES`, which supplies the kind
 * each one projects, so a typed table cannot be added to the DDL without a mapping here, or vice versa.
 */
const TYPED_COLUMNS: Readonly<Record<keyof typeof TYPED_EVENT_TABLES, TableInsert["columns"]>> = {
  sql_events: [
    ["ts", TS],
    ["trace_id", textOf("traceId")],
    ["app_id", textOf("appId")],
    ["engine", textOf("engine")],
    ["dialect", textOf("dialect")],
    ["statement_kind", textOf("statementKind")],
    ["path", textOf("path")],
    ["table_name", textOf("tableName")],
    ["sql_text", textOf("sqlText")],
    ["bind_count", intOf("bindCount", "INTEGER")],
    ["duration_ms", doubleOf("durationMs")],
    ["row_count", intOf("rowCount", "BIGINT")],
    ["outcome", textOf("outcome")],
    ["error_tag", textOf("errorTag")],
  ],
  tx_events: [
    ["ts", TS],
    ["trace_id", textOf("traceId")],
    ["app_id", textOf("appId")],
    ["engine", textOf("engine")],
    ["outcome", textOf("outcome")],
    ["operations", intOf("operations", "INTEGER")],
    ["duration_ms", doubleOf("durationMs")],
    ["queue_wait_ms", doubleOf("queueWaitMs")],
    ["error_tag", textOf("errorTag")],
  ],
  http_events: [
    ["ts", TS],
    ["trace_id", textOf("traceId")],
    ["method", textOf("method")],
    ["path", textOf("path")],
    ["app_id", textOf("appId")],
    ["handler_kind", HANDLER_KIND_OF_FIELD],
    ["name", textOf("name")],
    ["status", intOf("status", "INTEGER")],
    ["duration_ms", doubleOf("durationMs")],
    ["bytes_in", intOf("bytesIn", "BIGINT")],
    ["bytes_out", intOf("bytesOut", "BIGINT")],
    ["error_tag", textOf("errorTag")],
  ],
  queue_events: [
    ["ts", TS],
    ["queue", textOf("queue")],
    ["depth", intOf("depth", "INTEGER")],
    ["cap", intOf("cap", "INTEGER")],
    ["event", textOf("event")],
    ["dropped", intOf("dropped", "BIGINT")],
  ],
  backup_events: [
    ["ts", TS],
    ["app_id", textOf("appId")],
    ["phase", textOf("phase")],
    ["emitter", EMITTER],
    ["run_id", textOf("runId")],
    ["key", textOf("key")],
    ["bytes", intOf("bytes", "BIGINT")],
    ["sha256", textOf("sha256")],
    ["attempts", intOf("attempts", "INTEGER")],
    ["duration_ms", doubleOf("durationMs")],
    ["outcome", textOf("outcome")],
    ["error_tag", textOf("errorTag")],
  ],
  lifecycle_events: [
    ["ts", TS],
    ["role", textOf("role")],
    ["pid", intOf("pid", "INTEGER")],
    ["phase", textOf("phase")],
    ["detail", jsonOf("detail")],
  ],
};

const TYPED_TABLES: readonly TableInsert[] = (Object.keys(TYPED_COLUMNS) as (keyof typeof TYPED_COLUMNS)[])
  .map((table) => ({ table, kind: TYPED_EVENT_TABLES[table], columns: [EVENT_ID, ...TYPED_COLUMNS[table]] }));

/** The batch's inserts, `platform_events` first. Run in this order inside the batch transaction. */
export const insertStatements: readonly { readonly table: string; readonly sql: string }[] = [
  PLATFORM_EVENTS,
  ...TYPED_TABLES,
].map((spec) => ({ table: spec.table, sql: insertSql(spec) }));

// ---------------------------------------------------------------------------
// Schema upgrade: v1 → v2, by rebuild from the retained JSONL (ADR 0006)
// ---------------------------------------------------------------------------

/*
 * Why a rebuild and not an in-place schema change: v1's typed rows carry no `event_id` and no source
 * offset, so no statement can give them their identity back — `ts` and `trace_id` are not join keys.
 * The JSONL is the source of truth, and the platform's own ingest is the only mapping this build
 * trusts, so the upgrade re-runs that ingest into a side file and switches only once the side file
 * provably holds every v1 event.
 */

export interface EventLogUpgradeOptions {
  /** How DuckDB files are opened, for the rebuild and the checks. Default: ingest's `eventLogDriver`. */
  readonly driver?: EventLogDriver;
  /** The `role` on emitted events. Default `"reader"`, the process that owns the event log. */
  readonly role?: Role;
}

/** What `Config.eventLog.duckdbPath` holds: no event log yet, this build's schema, or v1. */
export type EventLogSchemaStatus = "absent" | "current" | "v1";

export type EventLogUpgradeReport =
  | { readonly status: "absent" | "current"; readonly version: number }
  | {
    readonly status: "upgraded";
    readonly fromVersion: 1;
    readonly version: number;
    /** A byte-for-byte copy of the v1 file, kept beside the new one. */
    readonly backupPath: string;
    /** v1 `platform_events` rows proven present, by `event_id`, in the rebuilt tables. */
    readonly eventsVerified: number;
    /** `k6_points` rows carried over verbatim: their DDL did not change and they have no JSONL source. */
    readonly k6PointsCopied: number;
    /** The rebuild's own ingest run, over every retained source. */
    readonly rebuild: IngestReport;
  };

type Phase = IngestError["phase"];

function failure(phase: Phase, detail: string, cause?: unknown): IngestError {
  return new IngestError({ phase, detail, cause });
}

function attemptIo<A>(phase: Phase, run: () => Promise<A>): Effect.Effect<A, IngestError> {
  return Effect.tryPromise({
    try: run,
    catch: (cause) => failure(phase, cause instanceof Error ? cause.message : String(cause), cause),
  });
}

async function exists(path: string): Promise<boolean> {
  try {
    await Deno.stat(path);
    return true;
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return false;
    throw cause;
  }
}

/** Flushes `path` — a file's bytes, or a directory's entries — to stable storage. */
async function fsync(path: string): Promise<void> {
  const handle = await Deno.open(path, { read: true });
  try {
    await handle.sync();
  } finally {
    handle.close();
  }
}

/** Removes a scratch file of the upgrade's own making; a file already gone is fine. */
function discard(path: string): Effect.Effect<void> {
  return Effect.promise(() => Deno.remove(path).catch(() => undefined));
}

function closeQuietly(handle: { closeSync(): void }): Effect.Effect<void> {
  return Effect.try(() => handle.closeSync()).pipe(Effect.ignore);
}

/** One connection to `path` for the duration of `use`; connection, then instance, closed after. */
function withConnection<A>(
  driver: EventLogDriver,
  path: string,
  use: (connection: EventLogConnection) => Effect.Effect<A, IngestError>,
): Effect.Effect<A, IngestError> {
  return Effect.acquireUseRelease(
    attemptIo("inventory", () => driver.open(path)),
    (instance) =>
      Effect.acquireUseRelease(
        attemptIo("inventory", () => instance.connect()),
        use,
        closeQuietly,
      ),
    closeQuietly,
  );
}

function rows(
  connection: EventLogConnection,
  phase: Phase,
  sql: string,
): Effect.Effect<Record<string, unknown>[], IngestError> {
  return Effect.map(
    attemptIo(phase, () => connection.runAndReadAll(sql)),
    (reader) => reader.getRowObjects(),
  );
}

function count(value: unknown): number {
  return typeof value === "bigint" ? Number(value) : Number(value ?? 0);
}

/** The `main`-schema tables of the database attached as `alias`. */
function tablesOf(connection: EventLogConnection, alias: string): Effect.Effect<Set<string>, IngestError> {
  return Effect.map(
    rows(
      connection,
      "inventory",
      `SELECT table_name FROM duckdb_tables() WHERE database_name = ${
        sqlString(alias)
      } AND schema_name = 'main'`,
    ),
    (found) => new Set(found.map((row) => String(row.table_name))),
  );
}

/** Every table a v1 file can hold. */
const V1_TABLES: readonly string[] = [
  "platform_events",
  ...Object.keys(TYPED_EVENT_TABLES),
  "k6_points",
  INGEST_STATE_TABLE,
];

/**
 * Reads the schema of the file at `dbPath` without writing it: the file is attached `READ_ONLY` to an
 * in-memory instance. A file another process holds read-write fails here, explicitly.
 */
function inspect(driver: EventLogDriver, dbPath: string): Effect.Effect<EventLogSchemaStatus, IngestError> {
  return withConnection(driver, ":memory:", (connection) =>
    Effect.gen(function* () {
      yield* attemptIo(
        "inventory",
        () => connection.run(`ATTACH ${sqlString(dbPath)} AS target (READ_ONLY)`),
      );
      const tables = yield* tablesOf(connection, "target");
      if (tables.has(SCHEMA_TABLE)) {
        const [row] = yield* rows(
          connection,
          "inventory",
          `SELECT max(version) AS v FROM target.${SCHEMA_TABLE}`,
        );
        // An empty version table is a file this build began creating and has yet to stamp.
        if (
          row.v === null || row.v === undefined || Number(row.v) === EVENTLOG_SCHEMA_VERSION
        ) return "current";
        return yield* Effect.fail(
          failure(
            "inventory",
            `event-log schema v${row.v} at ${dbPath}; this build reads and upgrades to v2 only`,
          ),
        );
      }
      return V1_TABLES.some((table) => tables.has(table)) ? "v1" : "absent";
    }));
}

/**
 * What the file at `Config.eventLog.duckdbPath` holds. `absent` covers a missing file and one with no
 * event-log tables. For item 36's ingest startup: ingest itself refuses a v1 file (`EVENTLOG_DDL`'s
 * guard), and `upgradeEventLog` is the way forward.
 */
export function inspectEventLogSchema(
  options: Pick<EventLogUpgradeOptions, "driver"> = {},
): Effect.Effect<EventLogSchemaStatus, IngestError, Config> {
  return Effect.gen(function* () {
    const dbPath = (yield* Config).eventLog.duckdbPath;
    if (!(yield* attemptIo("inventory", () => exists(dbPath)))) return "absent";
    const driver = options.driver ?? (yield* Effect.promise(() => import("./ingest.ts"))).eventLogDriver;
    return yield* inspect(driver, dbPath);
  });
}

interface Verified {
  readonly eventsVerified: number;
  readonly k6PointsCopied: number;
}

/**
 * Proves the rebuilt file (the connection's own database) holds every event of the v1 copy at
 * `copyPath`, then carries over `k6_points` and records the rebuild in `_eventlog_schema`, naming
 * `backupPath`, where the copy is kept once the switch is made. Fails — having changed only the side
 * file — when any v1 event is missing.
 *
 * "Holds every event" means: every v1 `platform_events` row's `event_id` is in the rebuilt
 * `platform_events`; every v1 typed table has exactly as many rows as v1 `platform_events` has of its
 * kind (v1 inserted them pairwise); and each of those v1 events has a row, by `event_id`, in the
 * rebuilt typed table. Values may differ — that is the v2 mapping — but no row is lost.
 */
function verifyAndCarry(
  connection: EventLogConnection,
  dbPath: string,
  copyPath: string,
  backupPath: string,
  at: string,
): Effect.Effect<Verified, IngestError> {
  return Effect.gen(function* () {
    yield* attemptIo("plan", () => connection.run(`ATTACH ${sqlString(copyPath)} AS v1 (READ_ONLY)`));
    const v1Tables = yield* tablesOf(connection, "v1");
    const problems: string[] = [];
    let eventsVerified = 0;
    if (v1Tables.has("platform_events")) {
      const missingIds = "FROM v1.platform_events o WHERE NOT EXISTS " +
        "(SELECT 1 FROM main.platform_events n WHERE n.event_id = o.event_id)";
      const [totals] = yield* rows(
        connection,
        "plan",
        `SELECT (SELECT count(*) FROM v1.platform_events) AS total, (SELECT count(*) ${missingIds}) AS missing`,
      );
      const total = count(totals.total);
      const missing = count(totals.missing);
      eventsVerified = total - missing;
      if (missing > 0) {
        const sources = yield* rows(
          connection,
          "plan",
          `SELECT DISTINCT o.source_file ${missingIds} ORDER BY 1 LIMIT 6`,
        );
        const named = sources.slice(0, 5).map((row) => String(row.source_file));
        problems.push(
          `${missing} of ${total} v1 platform_events rows have no event_id in the rebuild (sources: ` +
            `${named.join(", ")}${sources.length > 5 ? ", …" : ""})`,
        );
      }
    }
    for (const [table, kind] of Object.entries(TYPED_EVENT_TABLES)) {
      if (!v1Tables.has(table)) continue;
      const canonical = v1Tables.has("platform_events")
        ? `(SELECT count(*) FROM v1.platform_events WHERE kind = ${sqlString(kind)})`
        : "0";
      const unmatched = v1Tables.has("platform_events")
        ? `(SELECT count(*) FROM v1.platform_events o WHERE o.kind = ${sqlString(kind)} AND NOT EXISTS ` +
          `(SELECT 1 FROM main.${table} n WHERE n.event_id = o.event_id))`
        : "0";
      const [row] = yield* rows(
        connection,
        "plan",
        `SELECT (SELECT count(*) FROM v1.${table}) AS typed, ${canonical} AS canonical, ${unmatched} AS missing`,
      );
      const typed = count(row.typed);
      if (typed !== count(row.canonical)) {
        problems.push(
          `v1 ${table} has ${typed} rows but v1 platform_events has ${
            count(row.canonical)
          } '${kind}' rows, ` +
            "so its rows cannot all be vouched for",
        );
      } else if (count(row.missing) > 0) {
        problems.push(`${count(row.missing)} v1 '${kind}' events have no row in the rebuilt ${table}`);
      }
    }
    if (problems.length > 0) {
      return yield* Effect.fail(failure(
        "plan",
        `event-log schema upgrade v1 → v2 refused: the retained JSONL cannot rebuild every v1 event: ` +
          `${problems.join("; ")}. Restore the missing sources under LOG_DIR (gunzip a retention-gzipped ` +
          `.jsonl.gz in place, ADR 0004) and retry. Nothing was switched: ${dbPath} is unchanged.`,
      ));
    }
    let k6PointsCopied = 0;
    if (v1Tables.has("k6_points")) {
      const columns = "ts, run_id, metric, value, scenario, status, trace_id, tags";
      yield* attemptIo(
        "insert",
        () => connection.run(`INSERT INTO main.k6_points (${columns}) SELECT ${columns} FROM v1.k6_points`),
      );
      const [copied] = yield* rows(connection, "insert", "SELECT count(*) AS n FROM v1.k6_points");
      k6PointsCopied = count(copied.n);
    }
    const detail = JSON.stringify({ fromVersion: 1, backupPath, eventsVerified, k6PointsCopied });
    yield* attemptIo("insert", () =>
      connection.run(
        `INSERT INTO main.${SCHEMA_TABLE} (version, applied_at, method, detail) ` +
          `VALUES (${EVENTLOG_SCHEMA_VERSION}, CAST($1 AS TIMESTAMPTZ) AT TIME ZONE 'UTC', 'rebuild-from-v1', CAST($2 AS JSON))`,
        [at, detail],
      ));
    yield* attemptIo("commit", () => connection.run("DETACH v1"));
    yield* attemptIo("commit", () => connection.run("CHECKPOINT"));
    return { eventsVerified, k6PointsCopied };
  });
}

/**
 * Upgrades the event-log file at `Config.eventLog.duckdbPath` from v1 to this build's schema, or does
 * nothing when it is absent or already current. The steps, each of which must succeed before the next:
 *
 * 1. **Refuse an unsafe start.** A `<file>.wal` beside the file means its last writer did not close it;
 *    the upgrade refuses rather than copy a half-checkpointed file. (Opening the file once — even the
 *    refused v2 ingest connection does — checkpoints it.) The schema is read through a `READ_ONLY`
 *    attach, which also fails if another process holds the file.
 * 2. **Preserve the original.** The v1 file is copied byte-for-byte to `<file>.v1-pending.duckdb` and
 *    fsynced. The copy takes its kept name, `<file>.v1-<instant>.duckdb`, only after the switch, so an
 *    attempt killed before then (OOM, power loss) leaves no timestamped orphan: its pending copy is a
 *    copy of a v1 file still in place, and the next attempt discards it with the other scratch files.
 * 3. **Rebuild through the real ingest.** `makeEventLogIngest` — the same entry, DDL and mappings every
 *    run uses — ingests the whole retained JSONL tree into `<file>.v2-rebuild.duckdb`, from offset 0.
 *    Like any first ingest it quarantines rejected lines and emits its anomalies, so a rebuild appends
 *    a second copy of each historical reject to `rejected.jsonl` (a duplicate in a diagnostic file).
 * 4. **Prove it is complete** against the copy (`verifyAndCarry`): no v1 event may be missing. If one
 *    is, fail `IngestError({ phase: "plan" })`: the side file and the copy are removed and the v1 file,
 *    with its `_ingest_state` offsets, is exactly as it was. A source that retention gzipped or deleted
 *    is the usual cause; this never discards the v1 rows that only it still holds.
 * 5. **Switch** by fsyncing the rebuilt file and renaming it over the original — atomic, so the path
 *    always names a whole v1 or a whole v2 file — then rename the copy to its kept name and fsync the
 *    directory so both renames survive a crash. A process killed between the two renames leaves a v2
 *    file and `<file>.v1-pending.duckdb` holding the only v1 original; nothing removes a pending copy
 *    while the live file is v2, and the operator renames it.
 *
 * Run it while nothing else has the event log open (the reader's ingest is refused on a v1 file anyway).
 * Item 36 owns calling it from ingest startup; until then it is an explicit operator step. The `READ_ONLY`
 * attach in step 1 refuses a file another *process* holds, but DuckDB's file lock is a per-process POSIX
 * lock, so it may not refuse a call from inside the reader while the reader's own ingest connection has
 * the file open. Item 36 must close that connection first, or serialize the call with ingest.
 */
export function upgradeEventLog(
  options: EventLogUpgradeOptions = {},
): Effect.Effect<EventLogUpgradeReport, IngestError, Config | EventSink> {
  return Effect.gen(function* () {
    const config = yield* Config;
    const sink = yield* EventSink;
    const ingest = yield* Effect.promise(() => import("./ingest.ts"));
    const driver = options.driver ?? ingest.eventLogDriver;
    const role = options.role ?? "reader";
    const dbPath = config.eventLog.duckdbPath;
    const version = EVENTLOG_SCHEMA_VERSION;

    if (!(yield* attemptIo("inventory", () => exists(dbPath)))) return { status: "absent", version } as const;
    if (yield* attemptIo("inventory", () => exists(`${dbPath}.wal`))) {
      return yield* Effect.fail(failure(
        "inventory",
        `${dbPath}.wal exists: the event log was not closed cleanly. Open it once (a refused ingest ` +
          "connection checkpoints it) and retry; the upgrade copies only a checkpointed file",
      ));
    }
    const found = yield* inspect(driver, dbPath);
    if (found !== "v1") return { status: found, version } as const;

    const at = new Date(yield* Clock.currentTimeMillis).toISOString();
    const backupPath = `${dbPath}.v1-${at.replaceAll(":", "-")}.duckdb`;
    const copyPath = `${dbPath}.v1-pending.duckdb`;
    const rebuildPath = `${dbPath}.v2-rebuild.duckdb`;
    const scratch = [rebuildPath, `${rebuildPath}.wal`, copyPath];

    const emit = (level: "info" | "error", phase: string, detail: unknown) =>
      Effect.flatMap(Clock.currentTimeMillis, (millis) =>
        sink.emit(lifecycleEvent({
          eventId: crypto.randomUUID(),
          ts: new Date(millis).toISOString(),
          level,
          role,
          pid: Deno.pid,
          traceId: `eventlog-upgrade-${at}`,
          phase,
          detail,
        })));

    const upgrade = Effect.gen(function* () {
      // Leftovers of an earlier, failed or killed attempt are derived data of the upgrade's own making:
      // the live file is still v1, so a pending copy is a copy of it.
      yield* Effect.forEach(scratch, discard, { discard: true });
      yield* attemptIo("inventory", async () => {
        await Deno.copyFile(dbPath, copyPath);
        await fsync(copyPath);
      });
      const rebuild = yield* Effect.scoped(
        Effect.flatMap(ingest.makeEventLogIngest({ driver, role }), (service) => service.ingest),
      ).pipe(
        Effect.provideService(Config, {
          ...config,
          eventLog: { ...config.eventLog, duckdbPath: rebuildPath },
        }),
      );
      const verified = yield* withConnection(
        driver,
        rebuildPath,
        (connection) => verifyAndCarry(connection, dbPath, copyPath, backupPath, at),
      );
      if (yield* attemptIo("commit", () => exists(`${rebuildPath}.wal`))) {
        return yield* Effect.fail(failure("commit", `${rebuildPath} did not checkpoint on close`));
      }
      yield* attemptIo("commit", async () => {
        await fsync(rebuildPath);
        await Deno.rename(rebuildPath, dbPath);
      });
      return { status: "upgraded", fromVersion: 1, version, backupPath, ...verified, rebuild } as const;
    });

    const report = yield* upgrade.pipe(
      Effect.onExit((exit) =>
        Exit.isSuccess(exit) ? Effect.void : Effect.forEach(scratch, discard, { discard: true })
      ),
      Effect.tapError((error) =>
        emit("error", "eventlog-schema-upgrade-refused", {
          dbPath,
          fromVersion: 1,
          toVersion: version,
          errorTag: error._tag,
          ingestPhase: error.phase,
          errorDetail: error.detail,
        })
      ),
    );
    // Past the switch: the pending copy is now the only v1 original, so a failure here keeps it.
    yield* attemptIo("commit", async () => {
      await Deno.rename(copyPath, backupPath);
      await fsync(dirname(dbPath));
    });
    yield* emit("info", "eventlog-schema-upgraded", {
      dbPath,
      fromVersion: 1,
      toVersion: version,
      backupPath,
      eventsVerified: report.eventsVerified,
      k6PointsCopied: report.k6PointsCopied,
      linesIngested: report.rebuild.linesIngested,
      linesRejected: report.rebuild.linesRejected,
    });
    return report;
  });
}
