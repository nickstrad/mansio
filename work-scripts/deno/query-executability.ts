/**
 * query-executability.ts: per-query executability check for 18 analysis queries against the
 * event-log tables. Run from the repository root:
 *   deno run -A work-scripts/deno/query-executability.ts
 *
 * Schema: the tables come from `core/eventlog/ddl.ts` (`EVENTLOG_DDL`, the item-21/35 v2 schema).
 * The `--schema` flag accepts only `item21` (the default); a stray first argument is not treated
 * as a schema name.
 *
 * Source of the SQL texts: the 8 starter queries are `scripts/duckdb/*.sql` (comment headers removed),
 * embedded below; the 10 catalog-draft queries are lx-02's drafts, copied from
 * `origin/luna/lx-02-query-bench:work-scripts/deno/lx-02-query-bench.ts` — they are lx-02's
 * interpretations of plan §4.3's name/question table (which has no SQL), not plan SQL. Both are
 * embedded so the check runs the same text lx-02 measured. This is a work-script, not the item-23
 * catalog.
 *
 * Data: `ROWS_PER_TABLE` deterministic rows per event table from a fixed-seed PRNG, plus a JSONL file
 * for the log-level query. Rows cover every column of every table (a missing column is a thrown error,
 * not a NULL) and the two lx-02 edge shapes: a plain-text `http_events.path` next to JSON
 * `k6_points.tags` for the trace UNION, and the k6 bucket join's time inputs.
 *
 * Output: a PASS/FAIL row per query with the DuckDB error line on failure. No timings, no paths, so
 * two runs print identical bytes. Exit code 0 only if all 18 pass. PASS proves the query binds and
 * runs; it does not prove the query returns rows — a PASS with 0 rows happens (e.g. the k6-vs-platform
 * bucket join, whose random 10-second buckets rarely overlap).
 *
 * Adopted 2026-10-11 from the sq-12 side quest as a standing harness (Opus 5.5 reviewed). The
 * `--schema lx02` reproduction mode from the quest was dropped on adoption; item 23 drops it when
 * adapting this harness pattern to `eventlog/catalog.ts`.
 */
import { DuckDBInstance } from "@duckdb/node-api";
import { EVENT_TABLES, EVENTLOG_DDL } from "../../core/eventlog/ddl.ts";
import type { EventTable } from "../../core/eventlog/ddl.ts";

const SEED = 0x5912;
const ROWS_PER_TABLE = 300;
const TRACES = 12; // rows with i % TRACES === 7 carry "trace-7", the id the starter and catalog queries use
const WINDOW = { start: "2026-09-01 00:00:00", end: "2026-10-01 00:00:00", trace: "trace-7", limit: 100 };
const LOG_DIR = "/tmp/query-executability";
const LOG_PATH = `${LOG_DIR}/events.jsonl`;

const schemaFlagIndex = Deno.args.indexOf("--schema");
const schemaArg = schemaFlagIndex === -1 ? "item21" : Deno.args[schemaFlagIndex + 1] ?? "item21";
if (schemaArg !== "item21") {
  throw new Error(`--schema must be item21, got ${schemaArg}`);
}

type Row = Record<string, string | number | null>;

// ---- deterministic data -----------------------------------------------------------------------------

/** mulberry32: a tiny seeded PRNG, so every run generates the same rows in the same order. */
function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const EPOCH_MS = Date.UTC(2026, 8, 1); // 2026-09-01 UTC, inside the query window
const timestampAt = (r: () => number) =>
  new Date(EPOCH_MS + Math.floor(r() * 30 * 86400) * 1000).toISOString().replace("T", " ").replace("Z", "");
const traceAt = (i: number) => `trace-${i % TRACES}`;
const EVENT_KINDS = ["http", "handler", "sql", "tx", "queue", "backup", "lifecycle", "ingest"];

/** One generator per table. It returns every column name the table might use; the insert keeps only the ones the table has. */
const GENERATORS: Record<EventTable, (i: number, r: () => number) => Row> = {
  platform_events: (i, r) => {
    const eventKind = EVENT_KINDS[i % EVENT_KINDS.length];
    const failed = i % 9 === 0;
    return {
      event_id: `evt-${i}`,
      ts: timestampAt(r),
      level: failed ? "error" : "info",
      role: i % 2 ? "server" : "reader",
      pid: 1000 + (i % 4),
      trace_id: traceAt(i),
      span_id: `span-${i}`,
      parent_span_id: null,
      app_id: `app-${i % 3}`,
      event_kind: eventKind,
      handler_kind: eventKind === "handler" ? (i % 2 ? "query" : "mutation") : null,
      name: eventKind === "handler" ? `handler-${i % 5}` : `${eventKind}-name`,
      outcome: failed ? "error" : "ok",
      error_tag: failed ? "ValidationError" : null,
      duration_ms: Math.round(r() * 5000) / 10,
      attrs: JSON.stringify({ sample: i % 16 }),
      source_file: "synthetic.jsonl",
      source_offset: i,
    };
  },
  sql_events: (i, r) => ({
    event_id: `sql-${i}`,
    ts: timestampAt(r),
    trace_id: traceAt(i),
    app_id: `app-${i % 3}`,
    engine: i % 2 ? "duckdb" : "sqlite",
    dialect: "sqlite",
    statement_kind: i % 4 ? "insert" : "select",
    table_name: `table-${i % 4}`,
    sql_text: `SELECT * FROM table_${i % 7}`,
    bind_count: i % 8,
    duration_ms: Math.round(r() * 2000) / 10,
    row_count: i % 100,
    outcome: i % 50 === 0 ? "error" : "ok",
    error_tag: i % 50 === 0 ? "ConstraintError" : null,
  }),
  tx_events: (i, r) => ({
    event_id: `tx-${i}`,
    ts: timestampAt(r),
    trace_id: traceAt(i),
    app_id: `app-${i % 3}`,
    engine: i % 2 ? "duckdb" : "sqlite",
    outcome: i % 25 === 0 ? "rollback" : "commit",
    operations: i % 10,
    duration_ms: Math.round(r() * 400) / 10,
    queue_wait_ms: Math.round(r() * 100) / 10,
    error_tag: i % 25 === 0 ? "ConstraintError" : null,
  }),
  http_events: (i, r) => {
    const handlerKind = i % 2 ? "query" : "mutation";
    return {
      event_id: `http-${i}`,
      ts: timestampAt(r),
      trace_id: traceAt(i),
      method: "GET",
      // Plain text, not JSON: the UNION arm that collides with k6_points.tags in trace-timeline.
      path: `/api/${i % 10}`,
      app_id: `app-${i % 3}`,
      handler_kind: handlerKind,
      name: `handler-${i % 5}`,
      status: i % 100 === 0 ? 500 : 200,
      duration_ms: Math.round(r() * 500) / 10,
      bytes_in: 128,
      bytes_out: 512,
      error_tag: i % 100 === 0 ? "InternalError" : null,
    };
  },
  queue_events: (i, r) => ({
    event_id: `queue-${i}`,
    ts: timestampAt(r),
    queue: i % 2 ? "writer" : "reader",
    depth: i % 100,
    cap: 256,
    event: i % 100 === 0 ? "rejected" : i % 10 === 0 ? "high-water" : "accepted",
    dropped: i % 3,
  }),
  backup_events: (i, r) => ({
    event_id: `backup-${i}`,
    ts: timestampAt(r),
    app_id: `app-${i % 3}`,
    phase: i % 6 === 0 ? "publish-latest" : "snapshot",
    emitter: "server",
    run_id: `run-${i % 20}`,
    key: `backups/${i}`,
    bytes: i * 17,
    sha256: `sha-${i}`,
    attempts: 1 + (i % 3),
    duration_ms: Math.round(r() * 10000) / 10,
    outcome: i % 100 === 0 ? "error" : "ok",
    error_tag: i % 100 === 0 ? "S3Error" : null,
  }),
  lifecycle_events: (i, r) => ({
    event_id: `life-${i}`,
    ts: timestampAt(r),
    role: i % 2 ? "server" : "reader",
    pid: i % 8,
    phase: i % 2 ? "startup" : "shutdown",
    detail: JSON.stringify({ sequence: i % 16 }),
  }),
  k6_points: (i, r) => ({
    ts: timestampAt(r),
    run_id: "run-1",
    metric: i % 2 ? "http_req_duration" : "http_req_failed",
    value: Math.round(r() * 1000) / 10,
    scenario: "mixed",
    status: i % 100 === 0 ? "error" : "ok",
    trace_id: traceAt(i),
    tags: JSON.stringify({ status: i % 5 }),
  }),
};

/** SQL literal for one value. Strings are quoted and escaped; DuckDB casts them to the column type on INSERT. */
function literal(value: string | number | null): string {
  if (value === null) return "NULL";
  if (typeof value === "number") return String(value);
  return `'${value.replaceAll("'", "''")}'`;
}

// ---- SQL texts ----------------------------------------------------------------------------------------

/** lx-02's 8 starter queries: scripts/duckdb/*.sql, comment headers removed. $-placeholders are substituted below. */
const STARTER: { name: string; sql: string }[] = [
  {
    name: "starter:backup-durations.sql",
    sql: `WITH windowed AS (
  SELECT *
  FROM backup_events
  WHERE ts >= $start_ts
    AND ts <  $end_ts
),
per_phase AS (
  SELECT
    app_id,
    phase,
    count(*) AS phase_count,
    count(*) FILTER (WHERE outcome = 'error') AS failure_count,
    round(avg(duration_ms), 3) AS avg_duration_ms,
    round(max(duration_ms), 3) AS max_duration_ms,
    sum(bytes) AS total_bytes
  FROM windowed
  GROUP BY app_id, phase
),
successes AS (
  SELECT
    app_id,
    ts,
    ts - lag(ts) OVER (PARTITION BY app_id ORDER BY ts) AS gap_since_prior_success
  FROM windowed
  WHERE phase = 'publish-latest'
    AND outcome = 'ok'
)
SELECT
  p.app_id,
  p.phase,
  p.phase_count,
  p.failure_count,
  p.avg_duration_ms,
  p.max_duration_ms,
  p.total_bytes,
  (SELECT round(avg(epoch(gap_since_prior_success)), 1)
     FROM successes s WHERE s.app_id = p.app_id) AS avg_gap_between_successes_sec
FROM per_phase p
ORDER BY p.app_id, p.phase;`,
  },
  {
    name: "starter:error-breakdown.sql",
    sql: `WITH windowed AS (
  SELECT *
  FROM platform_events
  WHERE ts >= $start_ts
    AND ts <  $end_ts
),
totals AS (
  SELECT
    count(*) AS total_events,
    count(*) FILTER (WHERE outcome = 'error') AS total_errors
  FROM windowed
)
SELECT
  w.error_tag,
  count(*) AS error_count,
  round(100.0 * count(*) / t.total_errors, 2) AS pct_of_errors,
  round(100.0 * count(*) / t.total_events, 2) AS pct_of_all_events
FROM windowed w, totals t
WHERE w.outcome = 'error'
GROUP BY w.error_tag, t.total_errors, t.total_events
ORDER BY error_count DESC;`,
  },
  {
    name: "starter:event-counts-by-type.sql",
    sql: `SELECT
  event_kind,
  handler_kind,
  name,
  outcome,
  count(*) AS event_count
FROM platform_events
WHERE ts >= $start_ts
  AND ts <  $end_ts
GROUP BY event_kind, handler_kind, name, outcome
ORDER BY event_count DESC;`,
  },
  {
    name: "starter:lifecycle-events.sql",
    sql: `SELECT
  role,
  pid,
  ts,
  phase,
  detail,
  ts - lag(ts) OVER (PARTITION BY role, pid ORDER BY ts) AS time_since_prior_phase
FROM lifecycle_events
WHERE ts >= $start_ts
  AND ts <  $end_ts
ORDER BY role, pid, ts;`,
  },
  {
    name: "starter:log-level-distribution.sql",
    sql: `SELECT
  level,
  count(*) AS event_count,
  round(100.0 * count(*) / sum(count(*)) OVER (), 2) AS pct_of_total
FROM read_json($log_glob, format = 'newline_delimited')
GROUP BY level
ORDER BY event_count DESC;`,
  },
  {
    name: "starter:recent-errors.sql",
    sql: `SELECT
  ts,
  trace_id,
  role,
  app_id,
  event_kind,
  handler_kind,
  name,
  error_tag,
  attrs
FROM platform_events
WHERE outcome = 'error'
ORDER BY ts DESC
LIMIT $limit;`,
  },
  {
    name: "starter:slowest-sql.sql",
    sql: `SELECT
  engine,
  statement_kind,
  table_name,
  count(*) AS call_count,
  round(quantile_cont(duration_ms, 0.50), 3) AS p50_ms,
  round(quantile_cont(duration_ms, 0.95), 3) AS p95_ms,
  round(quantile_cont(duration_ms, 0.99), 3) AS p99_ms,
  round(max(duration_ms), 3) AS max_ms
FROM sql_events
WHERE ts >= $start_ts
  AND ts <  $end_ts
  AND outcome = 'ok'
GROUP BY engine, statement_kind, table_name
ORDER BY p95_ms DESC;`,
  },
  {
    name: "starter:trace-timeline.sql",
    sql:
      `SELECT ts, 'platform' AS source, event_kind AS kind, name, outcome, error_tag, duration_ms, NULL AS extra
FROM platform_events
WHERE trace_id = $trace_id

UNION ALL

SELECT ts, 'sql' AS source, statement_kind AS kind, table_name AS name, outcome, error_tag,
       duration_ms, sql_text AS extra
FROM sql_events
WHERE trace_id = $trace_id

UNION ALL

SELECT ts, 'tx' AS source, 'tx' AS kind, outcome AS name, outcome, error_tag, duration_ms,
       NULL AS extra
FROM tx_events
WHERE trace_id = $trace_id

UNION ALL

SELECT ts, 'http' AS source, handler_kind AS kind, name, CASE WHEN error_tag IS NULL THEN 'ok' ELSE 'error' END,
       error_tag, duration_ms, path AS extra
FROM http_events
WHERE trace_id = $trace_id

UNION ALL

SELECT ts, 'k6' AS source, metric AS kind, scenario AS name, status AS outcome,
       NULL AS error_tag, value AS duration_ms, CAST(tags AS VARCHAR) AS extra
FROM k6_points
WHERE trace_id = $trace_id

ORDER BY ts;`,
  },
];

/** lx-02's 10 catalog-draft queries, copied from lx-02's catalog(). Window and trace values are interpolated. */
function catalog(start: string, end: string, trace: string): { name: string; sql: string }[] {
  return [
    {
      name: "catalog:slowest-sql",
      sql: `SELECT engine, statement_kind, table_name, count(*) AS call_count,
      round(quantile_cont(duration_ms, 0.50), 3) AS p50_ms,
      round(quantile_cont(duration_ms, 0.95), 3) AS p95_ms,
      round(quantile_cont(duration_ms, 0.99), 3) AS p99_ms, round(max(duration_ms), 3) AS max_ms
      FROM sql_events WHERE ts >= TIMESTAMP '${start}' AND ts < TIMESTAMP '${end}' AND outcome = 'ok'
      GROUP BY engine, statement_kind, table_name ORDER BY p95_ms DESC`,
    },
    {
      name: "catalog:plan-shapes",
      sql: `SELECT sql_text, count(*) AS call_count, round(avg(duration_ms), 3) AS avg_ms,
      round(quantile_cont(duration_ms, 0.95), 3) AS p95_ms FROM sql_events
      WHERE ts >= TIMESTAMP '${start}' AND ts < TIMESTAMP '${end}'
      GROUP BY sql_text ORDER BY call_count DESC`,
    },
    {
      name: "catalog:error-rate-by-handler",
      sql: `SELECT app_id, name AS handler, count(*) AS request_count,
      count(*) FILTER (WHERE outcome = 'error') AS error_count,
      round(100.0 * count(*) FILTER (WHERE outcome = 'error') / count(*), 2) AS error_pct,
      list(DISTINCT error_tag) FILTER (WHERE outcome = 'error') AS error_tags
      FROM platform_events WHERE kind = 'handler' AND ts >= TIMESTAMP '${start}' AND ts < TIMESTAMP '${end}'
      GROUP BY app_id, name ORDER BY error_pct DESC`,
    },
    {
      name: "catalog:mutation-vs-query-latency",
      sql: `SELECT date_trunc('hour', ts) AS hour, role AS path,
      round(quantile_cont(duration_ms, 0.50), 3) AS p50_ms,
      round(quantile_cont(duration_ms, 0.95), 3) AS p95_ms,
      round(quantile_cont(duration_ms, 0.99), 3) AS p99_ms
      FROM platform_events WHERE ts >= TIMESTAMP '${start}' AND ts < TIMESTAMP '${end}'
      GROUP BY hour, path ORDER BY hour, path`,
    },
    {
      name: "catalog:handler-throughput",
      sql: `SELECT date_trunc('minute', ts) AS minute, app_id, name AS handler, outcome,
      count(*) AS request_count FROM platform_events WHERE kind = 'handler'
      AND ts >= TIMESTAMP '${start}' AND ts < TIMESTAMP '${end}'
      GROUP BY minute, app_id, handler, outcome ORDER BY minute, app_id, handler`,
    },
    {
      name: "catalog:queue-depth",
      sql: `SELECT date_trunc('minute', ts) AS minute, queue,
      max(depth) FILTER (WHERE event = 'high-water') AS high_water,
      count(*) FILTER (WHERE event = 'rejected') AS rejections,
      sum(dropped) AS dropped FROM queue_events WHERE ts >= TIMESTAMP '${start}' AND ts < TIMESTAMP '${end}'
      GROUP BY minute, queue ORDER BY minute, queue`,
    },
    {
      name: "catalog:tx-outcomes",
      sql: `SELECT date_trunc('hour', ts) AS hour, engine, outcome, count(*) AS tx_count,
      round(avg(queue_wait_ms), 3) AS avg_queue_wait_ms,
      round(quantile_cont(queue_wait_ms, 0.95), 3) AS p95_queue_wait_ms
      FROM tx_events WHERE ts >= TIMESTAMP '${start}' AND ts < TIMESTAMP '${end}'
      GROUP BY hour, engine, outcome ORDER BY hour, engine, outcome`,
    },
    {
      name: "catalog:backup-history",
      sql:
        `WITH windowed AS (SELECT * FROM backup_events WHERE ts >= TIMESTAMP '${start}' AND ts < TIMESTAMP '${end}'),
      phases AS (SELECT app_id, phase, count(*) AS phase_count,
        count(*) FILTER (WHERE outcome = 'error') AS failures,
        round(avg(duration_ms), 3) AS avg_duration_ms, sum(bytes) AS total_bytes
        FROM windowed GROUP BY app_id, phase),
      successes AS (SELECT app_id, ts, ts - lag(ts) OVER (PARTITION BY app_id ORDER BY ts) AS gap
        FROM windowed WHERE phase = 'publish-latest' AND outcome = 'ok')
      SELECT p.*, (SELECT round(avg(epoch(gap)), 1) FROM successes s WHERE s.app_id = p.app_id) AS avg_gap_sec
      FROM phases p ORDER BY app_id, phase`,
    },
    {
      name: "catalog:trace",
      sql: `SELECT ts, 'platform' AS source, kind, name, outcome, error_tag, duration_ms, NULL AS extra
      FROM platform_events WHERE trace_id = '${trace}'
      UNION ALL SELECT ts, 'sql', statement_kind, table_name, outcome, error_tag, duration_ms, sql_text
      FROM sql_events WHERE trace_id = '${trace}'
      UNION ALL SELECT ts, 'tx', 'tx', outcome, outcome, error_tag, duration_ms, NULL
      FROM tx_events WHERE trace_id = '${trace}'
      UNION ALL SELECT ts, 'http', kind, name, CASE WHEN error_tag IS NULL THEN 'ok' ELSE 'error' END,
        error_tag, duration_ms, path FROM http_events WHERE trace_id = '${trace}'
      UNION ALL SELECT ts, 'k6', metric, scenario, status, NULL, value, CAST(tags AS VARCHAR)
        FROM k6_points WHERE trace_id = '${trace}' ORDER BY ts`,
    },
    {
      name: "catalog:k6-vs-platform",
      sql: `WITH k AS (SELECT date_bin(INTERVAL '10 seconds', ts, TIMESTAMP '2000-01-01') AS bucket,
      scenario, quantile_cont(value, 0.95) AS client_p95 FROM k6_points
      WHERE metric = 'http_req_duration' AND ts >= TIMESTAMP '${start}' AND ts < TIMESTAMP '${end}'
      GROUP BY bucket, scenario),
      p AS (SELECT date_bin(INTERVAL '10 seconds', ts, TIMESTAMP '2000-01-01') AS bucket,
      name, quantile_cont(duration_ms, 0.95) AS handler_p95 FROM platform_events
      WHERE kind = 'handler' AND ts >= TIMESTAMP '${start}' AND ts < TIMESTAMP '${end}'
      GROUP BY bucket, name)
      SELECT k.bucket, k.scenario, k.client_p95, p.name AS handler, p.handler_p95
      FROM k JOIN p USING (bucket) ORDER BY k.bucket, k.scenario, handler`,
    },
  ];
}

/** Substitutes the starter $-placeholders the way lx-02 did: numbers bare, strings quoted and escaped. */
function substitute(sql: string, params: Record<string, string | number>): string {
  return sql.replace(/\$(start_ts|end_ts|trace_id|limit|log_glob)\b/g, (placeholder) => {
    const value = params[placeholder.slice(1)];
    if (value === undefined) throw new Error(`No value for ${placeholder}`);
    return typeof value === "number" ? String(value) : `'${value.replaceAll("'", "''")}'`;
  });
}

const STARTER_PARAMS: Record<string, Record<string, string | number>> = {
  "starter:backup-durations.sql": { start_ts: WINDOW.start, end_ts: WINDOW.end },
  "starter:error-breakdown.sql": { start_ts: WINDOW.start, end_ts: WINDOW.end },
  "starter:event-counts-by-type.sql": { start_ts: WINDOW.start, end_ts: WINDOW.end },
  "starter:lifecycle-events.sql": { start_ts: WINDOW.start, end_ts: WINDOW.end },
  "starter:log-level-distribution.sql": { log_glob: LOG_PATH },
  "starter:recent-errors.sql": { limit: WINDOW.limit },
  "starter:slowest-sql.sql": { start_ts: WINDOW.start, end_ts: WINDOW.end },
  "starter:trace-timeline.sql": { trace_id: WINDOW.trace },
};

// ---- run ----------------------------------------------------------------------------------------------

const queries: { name: string; sql: string }[] = [
  ...STARTER.map((q) => ({ name: q.name, sql: substitute(q.sql, STARTER_PARAMS[q.name]) })),
  ...catalog(WINDOW.start, WINDOW.end, WINDOW.trace),
];
if (queries.length !== 18) throw new Error(`expected 18 queries, built ${queries.length}`);

const instance = await DuckDBInstance.create(":memory:", { threads: "1", memory_limit: "1GB" });
const connection = await instance.connect();
const run = async (sql: string) => await connection.run(sql);
const query = async (sql: string) => (await connection.runAndReadAll(sql)).getRowObjects();

try {
  const [{ version }] = await query("SELECT version() AS version");
  console.log(
    `duckdb ${version}; schema ${schemaArg}; seed 0x${SEED.toString(16)}; ${ROWS_PER_TABLE} rows/table`,
  );

  // Schema: the item-21/35 v2 DDL from core/eventlog/ddl.ts runs as a whole, guards included.
  for (const statement of EVENTLOG_DDL) await run(statement);

  // Data: one batched INSERT per table, columns limited to the ones the table has. A table column with no
  // generated value is an error, so every column the queries touch gets a value.
  const random = mulberry32(SEED);
  const logLevels: string[] = [];
  for (const table of EVENT_TABLES) {
    const columns = (
      await query(
        `SELECT column_name FROM duckdb_columns() WHERE database_name = current_database() AND table_name = '${table}' ORDER BY column_index`,
      )
    ).map((row) => String(row.column_name));
    const rows = Array.from({ length: ROWS_PER_TABLE }, (_, i) => GENERATORS[table](i, random));
    const values = rows.map((row) => {
      for (const column of columns) {
        if (!(column in row)) throw new Error(`${table}.${column} has no generated value`);
      }
      return `(${columns.map((column) => literal(row[column])).join(", ")})`;
    });
    for (let offset = 0; offset < values.length; offset += 100) {
      await run(
        `INSERT INTO ${table} (${columns.join(", ")}) VALUES ${
          values.slice(offset, offset + 100).join(", ")
        }`,
      );
    }
  }
  for (let i = 0; i < ROWS_PER_TABLE; i++) {
    logLevels.push(i % 100 === 0 ? "error" : i % 10 === 0 ? "warn" : "info");
  }
  await Deno.mkdir(LOG_DIR, { recursive: true });
  await Deno.writeTextFile(LOG_PATH, logLevels.map((level) => JSON.stringify({ level })).join("\n") + "\n");

  const counts: string[] = [];
  for (const table of EVENT_TABLES) {
    counts.push(`${table}=${(await query(`SELECT count(*) AS n FROM ${table}`))[0].n}`);
  }
  console.log(`rows: ${counts.join(" ")}`);
  console.log("");

  const results: { name: string; ok: boolean; detail: string }[] = [];
  for (const { name, sql } of queries) {
    try {
      const rows = await query(sql);
      results.push({ name, ok: true, detail: `${rows.length} rows` });
    } catch (error) {
      const message = error instanceof Error ? error.message.split("\n")[0] : String(error);
      results.push({ name, ok: false, detail: message });
    }
  }

  const width = Math.max(...results.map((r) => r.name.length));
  for (const r of results) {
    console.log(`${r.ok ? "PASS" : "FAIL"}  ${r.name.padEnd(width)}  ${r.detail}`);
  }
  const failed = results.filter((r) => !r.ok).length;
  console.log("");
  console.log(`${results.length - failed}/${results.length} passed`);
  Deno.exitCode = failed === 0 ? 0 : 1;
} finally {
  connection.closeSync();
  instance.closeSync();
  await Deno.remove(LOG_DIR, { recursive: true }).catch(() => undefined);
}
