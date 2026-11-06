/**
 * DDL for the append-only DuckDB event log (plan items 21 and 35, §4.1): the typed event tables, the
 * `_ingest_state` offset table and the `_eventlog_schema` version table, as pure strings.
 *
 * This module is PURE: no imports at all. `eventlog/ingest.ts` runs `EVENTLOG_DDL` on every
 * connection to the persistent event-log file (`Config.eventLog.duckdbPath`), which is separate from
 * anything else.
 *
 * Three kinds of table, with different rules:
 *
 * - **Event tables** are append-only. Ingest only ever `INSERT`s into them; nothing in this repo
 *   rewrites or removes an event row. Losing the file loses nothing durable: the JSONL tree is the
 *   source of truth (ADR 0001 decision 4) and re-ingesting it rebuilds every table.
 * - **`_ingest_state`** is the single mutable table: one row per JSONL source file, holding how many
 *   bytes of it are ingested, the SHA-256 of exactly those bytes, and whether it was ever rewritten.
 *   `core/eventlog/offsets.ts` is the pure model of how that row evolves.
 * - **`_eventlog_schema`** records which schema version the file holds, one row per way it got there
 *   (`create`, or `rebuild-from-v1`). Rows are only ever inserted.
 *
 * `k6_points` is declared here so the schema lives in one place, but it is filled by `perf:ingest`
 * (item 25) from k6's own JSON output, not by the event-log ingest. It is not a projection of an event
 * record, so it has no `event_id`.
 *
 * ## Versions (ADR 0006)
 *
 * - **v1** (item 21) had no version table. Its typed tables had no `event_id`, and
 *   `platform_events.kind` was the record's kind except for `handler` records, where it was the
 *   handler's `query`/`mutation`.
 * - **v2** (item 35, this module): `event_id` on every typed event table; `platform_events.event_kind`
 *   (always the record's kind) and `handler_kind` replace `kind`; `http_events.kind` becomes
 *   `handler_kind`; `backup_events` gains `emitter`, `run_id` and `attempts`.
 *
 * A v2 build never reuses a v1 file's tables under the v2 mappings. `EVENTLOG_DDL` starts with two
 * guards that fail the connection — before any statement touches the file — when it holds an
 * unversioned (v1) schema, or a version this build does not know. A v1 file stays exactly as it was
 * until `upgradeEventLog` (`eventlog/tables.ts`) rebuilds it from the retained JSONL and proves
 * the rebuild holds every v1 event.
 */

/** The schema version this build reads and writes. */
export const EVENTLOG_SCHEMA_VERSION = 2;

/** The append-only event tables, in declaration order. */
export const EVENT_TABLES = [
  "platform_events",
  "sql_events",
  "tx_events",
  "http_events",
  "queue_events",
  "backup_events",
  "lifecycle_events",
  "k6_points",
] as const;

export type EventTable = typeof EVENT_TABLES[number];

/** The one mutable table: per-source-file ingest offsets. */
export const INGEST_STATE_TABLE = "_ingest_state";

/** The version table. Its absence beside event tables is what marks a v1 file. */
export const SCHEMA_TABLE = "_eventlog_schema";

/**
 * The typed event tables and the `platform_events` `kind` each one projects. Every record of that kind
 * lands in both, under the same `event_id`.
 */
export const TYPED_EVENT_TABLES = {
  sql_events: "sql",
  tx_events: "tx",
  http_events: "http",
  queue_events: "queue",
  backup_events: "backup",
  lifecycle_events: "lifecycle",
} as const satisfies Partial<Record<EventTable, string>>;

/** v2. §4.1 plus the item-35 identity and taxonomy columns. Keyed by table name so callers can't miss one. */
export const EVENT_TABLE_DDL: Readonly<Record<EventTable, string>> = {
  platform_events: `CREATE TABLE IF NOT EXISTS platform_events (
  event_id VARCHAR, ts TIMESTAMP, level VARCHAR, role VARCHAR, pid INTEGER,
  trace_id VARCHAR, span_id VARCHAR, parent_span_id VARCHAR,
  app_id VARCHAR, event_kind VARCHAR, handler_kind VARCHAR, name VARCHAR,
  outcome VARCHAR, error_tag VARCHAR, duration_ms DOUBLE,
  attrs JSON, source_file VARCHAR, source_offset BIGINT
)`,
  sql_events: `CREATE TABLE IF NOT EXISTS sql_events (
  event_id VARCHAR, ts TIMESTAMP, trace_id VARCHAR, app_id VARCHAR,
  engine VARCHAR,
  dialect VARCHAR, statement_kind VARCHAR,
  table_name VARCHAR, sql_text VARCHAR, bind_count INTEGER,
  duration_ms DOUBLE, row_count BIGINT, outcome VARCHAR, error_tag VARCHAR, path VARCHAR
)`,
  tx_events: `CREATE TABLE IF NOT EXISTS tx_events (
  event_id VARCHAR, ts TIMESTAMP, trace_id VARCHAR, app_id VARCHAR, engine VARCHAR,
  outcome VARCHAR,
  operations INTEGER, duration_ms DOUBLE, queue_wait_ms DOUBLE, error_tag VARCHAR
)`,
  http_events: `CREATE TABLE IF NOT EXISTS http_events (
  event_id VARCHAR, ts TIMESTAMP, trace_id VARCHAR, method VARCHAR, path VARCHAR,
  app_id VARCHAR, handler_kind VARCHAR, name VARCHAR, status INTEGER,
  duration_ms DOUBLE, bytes_in BIGINT, bytes_out BIGINT, error_tag VARCHAR
)`,
  queue_events: `CREATE TABLE IF NOT EXISTS queue_events (
  event_id VARCHAR, ts TIMESTAMP, queue VARCHAR, depth INTEGER, cap INTEGER,
  event VARCHAR, dropped BIGINT
)`,
  backup_events: `CREATE TABLE IF NOT EXISTS backup_events (
  event_id VARCHAR, ts TIMESTAMP, app_id VARCHAR, phase VARCHAR,
  emitter VARCHAR, run_id VARCHAR, key VARCHAR,
  bytes BIGINT, sha256 VARCHAR, attempts INTEGER,
  duration_ms DOUBLE, outcome VARCHAR, error_tag VARCHAR
)`,
  lifecycle_events: `CREATE TABLE IF NOT EXISTS lifecycle_events (
  event_id VARCHAR, ts TIMESTAMP, role VARCHAR, pid INTEGER, phase VARCHAR, detail JSON
)`,
  k6_points: `CREATE TABLE IF NOT EXISTS k6_points (
  ts TIMESTAMP, run_id VARCHAR, metric VARCHAR, value DOUBLE,
  scenario VARCHAR, status VARCHAR, trace_id VARCHAR, tags JSON
)`,
};

/**
 * `source_file` is the path relative to `LOG_DIR` (`server/2026-10-10/events-14-31887-001.jsonl`), so
 * moving the log root does not orphan the offsets. `bytes_ingested` always ends just past a `\n`.
 * `prefix_sha256` is the lowercase hex SHA-256 of bytes `[0, bytes_ingested)` of that file.
 *
 * `dedupe` is not in §4.1: once a rewrite of the file has been detected, every line later read from
 * it is deduplicated on `eventId`, because the offset model can no longer vouch for it. Without it a
 * rewritten file can insert an old line twice — see "Once rewritten, always deduplicated" in
 * `core/eventlog/offsets.ts` and ADR 0004.
 */
export const INGEST_STATE_DDL = `CREATE TABLE IF NOT EXISTS _ingest_state (
  source_file VARCHAR PRIMARY KEY, bytes_ingested BIGINT,
  prefix_sha256 VARCHAR, last_ingested_at TIMESTAMP, dedupe BOOLEAN
)`;

/** One row per way the file reached a version. `detail` is the rebuild's evidence, when there was one. */
export const SCHEMA_TABLE_DDL = `CREATE TABLE IF NOT EXISTS _eventlog_schema (
  version INTEGER NOT NULL, applied_at TIMESTAMP NOT NULL, method VARCHAR NOT NULL, detail JSON
)`;

/** Every table name a v1 file can hold, i.e. every table this module declares apart from the version table. */
const V1_TABLES = [...EVENT_TABLES, INGEST_STATE_TABLE].map((table) => `'${table}'`).join(", ");

/** Tables of the connection's own database and `main` schema only, so an attached file cannot trip a guard. */
const OWN_TABLES = "duckdb_tables() WHERE database_name = current_database() AND schema_name = 'main'";

/**
 * Fails on a v1 file: event-log tables without a version table. Runs first, and only reads the catalog,
 * so a refused file is not touched. DuckDB's `error()` raises `Invalid Input Error: <message>`.
 */
export const UNVERSIONED_GUARD = `SELECT CASE
  WHEN EXISTS (SELECT 1 FROM ${OWN_TABLES} AND table_name IN (${V1_TABLES}))
   AND NOT EXISTS (SELECT 1 FROM ${OWN_TABLES} AND table_name = '${SCHEMA_TABLE}')
  THEN error('event-log schema v1 (unversioned) found; this build reads v${EVENTLOG_SCHEMA_VERSION} and never reuses ' ||
    'v1 tables under v${EVENTLOG_SCHEMA_VERSION} mappings. Run upgradeEventLog (eventlog/tables.ts) to ' ||
    'rebuild it from the retained JSONL; the v1 file is left unchanged (ADR 0006)')
END AS schema_guard`;

/** Fails on a file stamped with any version but this build's. Runs after `SCHEMA_TABLE_DDL`. */
export const VERSION_GUARD = `SELECT CASE
  WHEN (SELECT max(version) FROM ${SCHEMA_TABLE}) <> ${EVENTLOG_SCHEMA_VERSION}
  THEN error('event-log schema v' || (SELECT max(version) FROM ${SCHEMA_TABLE}) ||
    ' found; this build reads v${EVENTLOG_SCHEMA_VERSION} only (ADR 0006)')
END AS version_guard`;

/** Stamps a file this build created. A no-op once any row records this version. */
export const STAMP_VERSION = `INSERT INTO ${SCHEMA_TABLE} (version, applied_at, method)
SELECT ${EVENTLOG_SCHEMA_VERSION}, now() AT TIME ZONE 'UTC', 'create'
WHERE NOT EXISTS (SELECT 1 FROM ${SCHEMA_TABLE} WHERE version = ${EVENTLOG_SCHEMA_VERSION})`;

/**
 * Every statement needed to bring a fresh file up to this schema, or to confirm an existing one is
 * already there, in order. Against a current file every statement is a no-op; against a v1 or foreign
 * version the first guard that applies fails before anything is created.
 */
export const EVENTLOG_DDL: readonly string[] = [
  UNVERSIONED_GUARD,
  SCHEMA_TABLE_DDL,
  VERSION_GUARD,
  ...EVENT_TABLES.map((table) => EVENT_TABLE_DDL[table]),
  "ALTER TABLE sql_events ADD COLUMN IF NOT EXISTS path VARCHAR",
  INGEST_STATE_DDL,
  STAMP_VERSION,
];
