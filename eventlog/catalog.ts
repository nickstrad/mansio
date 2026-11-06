/**
 * The named analysis query catalog and `AnalysisLive` (plan item 23, §4.3).
 *
 * Callers choose a query by **name** and supply **scalar parameters**; they never supply SQL. Every
 * catalog entry is a `SELECT`/`WITH` over the v2 event-log tables in `core/eventlog/ddl.ts`, with every
 * caller-influenced value bound as a named parameter (`$since`, `$traceId`, …) and never interpolated.
 * `AnalysisLive` additionally opens the event-log file `READ_ONLY`, so even a catalog bug could not
 * write to it.
 *
 * The catalog is a plain record and is meant to be extended: a new entry is one more key, and nothing
 * here enumerates the names except `ANALYSIS_QUERY_NAMES`, which is derived from the record.
 * `docs/observability.md` documents each entry; `tests/unit/analysis_catalog_test.ts` asserts the doc
 * and this record list the same names.
 *
 * ## Parameters
 *
 * Each entry declares its parameters (`ParamSpec`): a type (`string`, `number`, `timestamp`), whether it
 * is required, and an optional default. `resolveParams` rejects an unknown or missing parameter and
 * coerces CLI strings (`--param limit=20`) to the declared type. Every declared parameter is bound on
 * every run — an absent optional one as `NULL` — so each SQL string references a fixed parameter set.
 * Time windows are `since` (inclusive) / `until` (exclusive), both optional.
 *
 * ## Schema notes the SQL depends on
 *
 * - `queue_events`, `backup_events` and `lifecycle_events` carry no `trace_id`; `queue_events` has no
 *   `app_id`. `trace` therefore unions only the tables that have the column.
 * - `http_events` has no `outcome`: a request is an error when `status >= 400` or `error_tag` is set.
 * - `platform_events.event_kind = 'handler'` rows carry the routed handler's `handler_kind`
 *   (`query` = reader path, `mutation` = writer path) and `outcome`.
 * - `k6_points` is filled by `perf:ingest` (item 25); when empty, `k6-vs-platform` returns zero rows.
 */
import { Effect, Layer, type Scope } from "effect";
import { type DuckDBConnection, DuckDBInstance } from "@duckdb/node-api";
import { AnalysisError, UnknownAnalysisQuery } from "../core/errors.ts";
import { Analysis, type AnalysisService, Config, type Row } from "./services.ts";

// ---------------------------------------------------------------------------
// The catalog
// ---------------------------------------------------------------------------

export type ParamType = "string" | "number" | "timestamp";

export interface ParamSpec {
  readonly type: ParamType;
  readonly required?: boolean;
  /** Applied when the caller omits the parameter. Without one, an optional parameter binds `NULL`. */
  readonly default?: string | number;
  readonly description: string;
}

export interface CatalogEntry {
  /** One `SELECT`/`WITH` statement. Parameters are `$name` placeholders. */
  readonly sql: string;
  readonly params: Readonly<Record<string, ParamSpec>>;
  /** The question the query answers (also the line `logs:list` prints). */
  readonly description: string;
}

const SINCE: ParamSpec = {
  type: "timestamp",
  description: "Inclusive lower bound on `ts` (ISO-8601, UTC when no offset). Default: unbounded.",
};
const UNTIL: ParamSpec = {
  type: "timestamp",
  description: "Exclusive upper bound on `ts` (ISO-8601, UTC when no offset). Default: unbounded.",
};
const APP_ID: ParamSpec = {
  type: "string",
  description: "Restrict to one app id. Default: every app.",
};
const limit = (value: number): ParamSpec => ({
  type: "number",
  default: value,
  description: `Maximum rows returned. Default ${value}.`,
});

/** `since`/`until` predicate on a column, `NULL`-tolerant so an absent bound is "unbounded". */
const window = (column: string) =>
  `(CAST($since AS TIMESTAMP) IS NULL OR ${column} >= CAST($since AS TIMESTAMP))
    AND (CAST($until AS TIMESTAMP) IS NULL OR ${column} < CAST($until AS TIMESTAMP))`;

export const CATALOG = {
  "deadline-hits-per-handler-hour": {
    description: "statement deadline hits per app, handler and UTC hour, with execution path",
    params: { since: SINCE, until: UNTIL, appId: APP_ID },
    sql: `SELECT date_trunc('hour', s.ts) AS hour, s.app_id, h.handler_kind, h.name,
  s.path, count(*) AS deadline_hits
FROM sql_events s
LEFT JOIN LATERAL (
  SELECT handler_kind, name FROM http_events request
  WHERE request.trace_id = s.trace_id AND request.app_id = s.app_id
    AND s.ts <= request.ts + INTERVAL '1 millisecond'
    AND s.ts >= request.ts - (request.duration_ms + 1) * INTERVAL '1 millisecond'
  ORDER BY request.ts, request.event_id
  LIMIT 1
) h ON TRUE
WHERE s.error_tag = 'StatementDeadlineError'
  AND ${window("s.ts")}
  AND (CAST($appId AS VARCHAR) IS NULL OR s.app_id = $appId)
GROUP BY 1, 2, 3, 4, 5
ORDER BY hour, s.app_id, h.name, s.path`,
  },

  "slowest-sql": {
    description: "p50/p95/p99 duration_ms by engine, statement kind and table",
    params: { since: SINCE, until: UNTIL, appId: APP_ID, limit: limit(100) },
    sql: `SELECT
  engine,
  statement_kind,
  table_name,
  count(*) AS calls,
  count(*) FILTER (WHERE outcome = 'error') AS errors,
  round(quantile_cont(duration_ms, 0.50), 3) AS p50_ms,
  round(quantile_cont(duration_ms, 0.95), 3) AS p95_ms,
  round(quantile_cont(duration_ms, 0.99), 3) AS p99_ms,
  round(max(duration_ms), 3) AS max_ms
FROM sql_events
WHERE ${window("ts")}
  AND (CAST($appId AS VARCHAR) IS NULL OR app_id = $appId)
GROUP BY engine, statement_kind, table_name
ORDER BY p95_ms DESC
LIMIT CAST($limit AS BIGINT)`,
  },

  "plan-shapes": {
    description:
      "distinct compiled SQL texts with counts and latency: which plans actually run, and whether declared indexes help",
    params: { since: SINCE, until: UNTIL, appId: APP_ID, limit: limit(50) },
    sql: `SELECT
  engine,
  statement_kind,
  table_name,
  sql_text,
  count(*) AS calls,
  round(avg(duration_ms), 3) AS avg_ms,
  round(quantile_cont(duration_ms, 0.95), 3) AS p95_ms,
  round(max(duration_ms), 3) AS max_ms,
  CAST(sum(row_count) AS BIGINT) AS total_rows,
  round(avg(row_count), 2) AS avg_rows,
  min(bind_count) AS min_binds,
  max(bind_count) AS max_binds
FROM sql_events
WHERE ${window("ts")}
  AND (CAST($appId AS VARCHAR) IS NULL OR app_id = $appId)
GROUP BY engine, statement_kind, table_name, sql_text
ORDER BY calls DESC, p95_ms DESC
LIMIT CAST($limit AS BIGINT)`,
  },

  "error-rate-by-handler": {
    description: "request count, error rate and error tag mix per app and handler",
    params: { since: SINCE, until: UNTIL, appId: APP_ID, limit: limit(100) },
    sql: `WITH requests AS (
  SELECT
    app_id,
    handler_kind,
    name,
    error_tag,
    (status >= 400 OR error_tag IS NOT NULL) AS is_error
  FROM http_events
  WHERE ${window("ts")}
    AND (CAST($appId AS VARCHAR) IS NULL OR app_id = $appId)
),
tag_counts AS (
  SELECT app_id, handler_kind, name, error_tag, count(*) AS n
  FROM requests
  WHERE error_tag IS NOT NULL
  GROUP BY app_id, handler_kind, name, error_tag
),
tag_mix AS (
  SELECT app_id, handler_kind, name,
         string_agg(error_tag || ':' || n, ', ' ORDER BY n DESC, error_tag) AS error_tags
  FROM tag_counts
  GROUP BY app_id, handler_kind, name
)
SELECT
  r.app_id,
  r.handler_kind,
  r.name,
  count(*) AS requests,
  count(*) FILTER (WHERE r.is_error) AS errors,
  round(count(*) FILTER (WHERE r.is_error) * 1.0 / count(*), 4) AS error_rate,
  any_value(m.error_tags) AS error_tags
FROM requests r
LEFT JOIN tag_mix m
  ON m.app_id IS NOT DISTINCT FROM r.app_id
 AND m.handler_kind IS NOT DISTINCT FROM r.handler_kind
 AND m.name IS NOT DISTINCT FROM r.name
GROUP BY r.app_id, r.handler_kind, r.name
ORDER BY error_rate DESC, requests DESC
LIMIT CAST($limit AS BIGINT)`,
  },

  "mutation-vs-query-latency": {
    description: "writer path (mutation) vs reader path (query) latency distributions over time",
    params: {
      since: SINCE,
      until: UNTIL,
      appId: APP_ID,
      bucketSeconds: {
        type: "number",
        default: 60,
        description: "Bucket width in seconds. Default 60.",
      },
      limit: limit(500),
    },
    sql: `SELECT
  to_timestamp(floor(epoch(ts) / CAST($bucketSeconds AS DOUBLE)) * CAST($bucketSeconds AS DOUBLE))
    AT TIME ZONE 'UTC' AS bucket,
  handler_kind,
  CASE handler_kind WHEN 'mutation' THEN 'writer' WHEN 'query' THEN 'reader' ELSE handler_kind END AS path,
  count(*) AS handlers,
  count(*) FILTER (WHERE outcome = 'error') AS errors,
  round(quantile_cont(duration_ms, 0.50), 3) AS p50_ms,
  round(quantile_cont(duration_ms, 0.95), 3) AS p95_ms,
  round(quantile_cont(duration_ms, 0.99), 3) AS p99_ms,
  round(max(duration_ms), 3) AS max_ms
FROM platform_events
WHERE event_kind = 'handler'
  AND ${window("ts")}
  AND (CAST($appId AS VARCHAR) IS NULL OR app_id = $appId)
GROUP BY bucket, handler_kind
ORDER BY bucket, handler_kind
LIMIT CAST($limit AS BIGINT)`,
  },

  "handler-throughput": {
    description: "requests per minute per handler, with status breakdown",
    params: { since: SINCE, until: UNTIL, appId: APP_ID, limit: limit(500) },
    sql: `SELECT
  date_trunc('minute', ts) AS minute,
  app_id,
  handler_kind,
  name,
  count(*) AS requests,
  count(*) FILTER (WHERE status BETWEEN 200 AND 299) AS status_2xx,
  count(*) FILTER (WHERE status BETWEEN 300 AND 399) AS status_3xx,
  count(*) FILTER (WHERE status BETWEEN 400 AND 499) AS status_4xx,
  count(*) FILTER (WHERE status >= 500) AS status_5xx,
  round(quantile_cont(duration_ms, 0.95), 3) AS p95_ms
FROM http_events
WHERE ${window("ts")}
  AND (CAST($appId AS VARCHAR) IS NULL OR app_id = $appId)
GROUP BY minute, app_id, handler_kind, name
ORDER BY minute, requests DESC
LIMIT CAST($limit AS BIGINT)`,
  },

  "queue-depth": {
    description: "writer/reader queue high-water marks, rejections and dropped events over time",
    params: {
      since: SINCE,
      until: UNTIL,
      bucketSeconds: {
        type: "number",
        default: 60,
        description: "Bucket width in seconds. Default 60.",
      },
      limit: limit(500),
    },
    sql: `SELECT
  to_timestamp(floor(epoch(ts) / CAST($bucketSeconds AS DOUBLE)) * CAST($bucketSeconds AS DOUBLE))
    AT TIME ZONE 'UTC' AS bucket,
  queue,
  max(depth) AS max_depth,
  max(cap) AS cap,
  count(*) FILTER (WHERE event = 'high-water') AS high_water_events,
  count(*) FILTER (WHERE event = 'rejected') AS rejected_events,
  count(*) FILTER (WHERE event = 'dropped') AS dropped_events,
  coalesce(sum(dropped) FILTER (WHERE event = 'dropped'), 0)::BIGINT AS dropped_total
FROM queue_events
WHERE ${window("ts")}
GROUP BY bucket, queue
ORDER BY bucket, queue
LIMIT CAST($limit AS BIGINT)`,
  },

  "tx-outcomes": {
    description: "commit/rollback/interrupted counts and queue wait times",
    params: { since: SINCE, until: UNTIL, appId: APP_ID, limit: limit(100) },
    sql: `SELECT
  app_id,
  engine,
  outcome,
  count(*) AS transactions,
  CAST(sum(operations) AS BIGINT) AS operations,
  round(quantile_cont(duration_ms, 0.95), 3) AS p95_ms,
  round(avg(queue_wait_ms), 3) AS avg_queue_wait_ms,
  round(quantile_cont(queue_wait_ms, 0.95), 3) AS p95_queue_wait_ms,
  round(max(queue_wait_ms), 3) AS max_queue_wait_ms
FROM tx_events
WHERE ${window("ts")}
  AND (CAST($appId AS VARCHAR) IS NULL OR app_id = $appId)
GROUP BY app_id, engine, outcome
ORDER BY app_id, engine, outcome
LIMIT CAST($limit AS BIGINT)`,
  },

  "backup-history": {
    description: "per-app backup phases, durations, bytes, failures, and gaps between successes",
    params: { since: SINCE, until: UNTIL, appId: APP_ID, limit: limit(200) },
    sql: `WITH phases AS (
  SELECT *
  FROM backup_events
  WHERE emitter = 'backups'
    AND ${window("ts")}
    AND (CAST($appId AS VARCHAR) IS NULL OR app_id = $appId)
),
successes AS (
  -- A run succeeded when its final phase (publish-latest) did.
  SELECT app_id, run_id, ts AS finished_at,
         ts - lag(ts) OVER (PARTITION BY app_id ORDER BY ts) AS gap_since_previous_success
  FROM phases
  WHERE phase = 'publish-latest' AND outcome = 'ok'
)
SELECT
  p.app_id,
  p.run_id,
  p.ts,
  p.phase,
  p.outcome,
  p.error_tag,
  round(p.duration_ms, 3) AS duration_ms,
  p.bytes,
  p.key,
  s.gap_since_previous_success
FROM phases p
LEFT JOIN successes s
  ON s.app_id IS NOT DISTINCT FROM p.app_id
 AND s.run_id IS NOT DISTINCT FROM p.run_id
 AND p.phase = 'publish-latest'
 AND p.outcome = 'ok'
ORDER BY p.app_id, p.ts
LIMIT CAST($limit AS BIGINT)`,
  },

  "trace": {
    description: "the complete ordered event timeline for one trace_id across both processes (and k6)",
    params: {
      traceId: { type: "string", required: true, description: "The trace id to follow." },
      limit: limit(1000),
    },
    // Only tables that carry trace_id. `platform_events` holds one row per typed event too (same
    // event_id), so those rows are listed once, from the typed table, which is then joined back to
    // `platform_events` on event_id for the role and span identity the typed tables lack.
    sql: `WITH timeline AS (
  SELECT ts, 'platform' AS source, role, event_kind AS kind, name, outcome, error_tag, duration_ms,
         app_id, span_id, parent_span_id, CAST(attrs AS VARCHAR) AS detail, event_id
  FROM platform_events
  WHERE trace_id = $traceId
    AND event_kind NOT IN ('sql', 'tx', 'http')
  UNION ALL
  SELECT t.ts, 'sql', p.role, t.statement_kind, t.table_name, t.outcome, t.error_tag, t.duration_ms,
         t.app_id, p.span_id, p.parent_span_id, t.sql_text, t.event_id
  FROM sql_events t LEFT JOIN platform_events p ON p.event_id = t.event_id
  WHERE t.trace_id = $traceId
  UNION ALL
  SELECT t.ts, 'tx', p.role, 'tx', t.engine, t.outcome, t.error_tag, t.duration_ms,
         t.app_id, p.span_id, p.parent_span_id,
         'operations=' || t.operations || ' queue_wait_ms=' || t.queue_wait_ms, t.event_id
  FROM tx_events t LEFT JOIN platform_events p ON p.event_id = t.event_id
  WHERE t.trace_id = $traceId
  UNION ALL
  SELECT t.ts, 'http', p.role, t.handler_kind, t.name,
         CASE WHEN t.status >= 400 OR t.error_tag IS NOT NULL THEN 'error' ELSE 'ok' END, t.error_tag,
         t.duration_ms, t.app_id, p.span_id, p.parent_span_id,
         t.method || ' ' || t.path || ' -> ' || t.status, t.event_id
  FROM http_events t LEFT JOIN platform_events p ON p.event_id = t.event_id
  WHERE t.trace_id = $traceId
  UNION ALL
  SELECT ts, 'k6', NULL, metric, scenario, status, NULL, value,
         NULL, NULL, NULL, CAST(tags AS VARCHAR), run_id
  FROM k6_points
  WHERE trace_id = $traceId
)
SELECT * FROM timeline
ORDER BY ts, source
LIMIT CAST($limit AS BIGINT)`,
  },

  "k6-vs-platform": {
    description: "k6 client p95 joined to server handler p95 per 10-second bucket per scenario",
    params: {
      runId: { type: "string", description: "Restrict to one k6 run id. Default: every run." },
      limit: limit(500),
    },
    sql: `WITH client AS (
  SELECT
    run_id,
    scenario,
    to_timestamp(floor(epoch(ts) / 10) * 10) AT TIME ZONE 'UTC' AS bucket,
    count(*) AS client_requests,
    quantile_cont(value, 0.95) AS client_p95_ms
  FROM k6_points
  WHERE metric = 'http_req_duration'
    AND (CAST($runId AS VARCHAR) IS NULL OR run_id = $runId)
  GROUP BY run_id, scenario, bucket
),
server AS (
  SELECT
    to_timestamp(floor(epoch(ts) / 10) * 10) AT TIME ZONE 'UTC' AS bucket,
    count(*) AS server_requests,
    quantile_cont(duration_ms, 0.95) AS server_p95_ms
  FROM http_events
  GROUP BY bucket
)
SELECT
  c.run_id,
  c.scenario,
  c.bucket,
  c.client_requests,
  round(c.client_p95_ms, 3) AS client_p95_ms,
  s.server_requests,
  round(s.server_p95_ms, 3) AS server_p95_ms,
  round(c.client_p95_ms - s.server_p95_ms, 3) AS transport_overhead_p95_ms
FROM client c
JOIN server s ON s.bucket = c.bucket
ORDER BY c.run_id, c.scenario, c.bucket
LIMIT CAST($limit AS BIGINT)`,
  },
} as const satisfies Record<string, CatalogEntry>;

export type AnalysisQueryName = keyof typeof CATALOG;

/** The catalog's names, derived from the record so adding an entry needs no second edit. */
export const ANALYSIS_QUERY_NAMES: readonly string[] = Object.keys(CATALOG);

const entries: Readonly<Record<string, CatalogEntry>> = CATALOG;

// ---------------------------------------------------------------------------
// Parameter validation
// ---------------------------------------------------------------------------

type Bound = string | number | null;

function invalid(name: string, detail: string): AnalysisError {
  return new AnalysisError({ name, detail });
}

/**
 * Checks `given` against `entry.params` and returns the full binding: every declared parameter, an
 * absent optional one as its default or `null`. Pure; exported for tests.
 */
export function resolveParams(
  name: string,
  entry: CatalogEntry,
  given: Readonly<Record<string, unknown>>,
): Effect.Effect<Readonly<Record<string, Bound>>, AnalysisError> {
  return Effect.suspend(() => {
    const unknown = Object.keys(given).filter((key) => !(key in entry.params));
    if (unknown.length > 0) {
      return Effect.fail(invalid(
        name,
        `unknown parameter(s): ${unknown.join(", ")}; accepted: ${Object.keys(entry.params).join(", ")}`,
      ));
    }
    const bound: Record<string, Bound> = {};
    for (const [key, spec] of Object.entries(entry.params)) {
      const raw = given[key];
      if (raw === undefined || raw === null || raw === "") {
        if (spec.required) return Effect.fail(invalid(name, `missing required parameter: ${key}`));
        bound[key] = spec.default ?? null;
        continue;
      }
      if (typeof raw !== "string" && typeof raw !== "number") {
        return Effect.fail(invalid(name, `parameter ${key} must be a string or number`));
      }
      if (spec.type === "number") {
        const value = typeof raw === "number" ? raw : Number(raw);
        if (!Number.isFinite(value)) {
          return Effect.fail(invalid(name, `parameter ${key} must be a number, got ${JSON.stringify(raw)}`));
        }
        bound[key] = value;
      } else if (spec.type === "timestamp") {
        const text = String(raw);
        if (Number.isNaN(Date.parse(text))) {
          return Effect.fail(
            invalid(name, `parameter ${key} must be an ISO-8601 timestamp, got ${JSON.stringify(raw)}`),
          );
        }
        // An offset-less value is UTC; a value with an offset is converted to UTC.
        const hasZone = /(Z|[+-]\d\d:?\d\d)$/i.test(text);
        bound[key] = new Date(hasZone ? text : `${text.replace(" ", "T")}Z`).toISOString().replace("Z", "");
      } else {
        bound[key] = String(raw);
      }
    }
    return Effect.succeed(bound);
  });
}

// ---------------------------------------------------------------------------
// Result rows
// ---------------------------------------------------------------------------

/** 64-bit and wider integers come back from `getRowObjectsJson` as strings; counts should be numbers. */
const INTEGER_TYPES = new Set([
  "BIGINT",
  "UBIGINT",
  "HUGEINT",
  "UHUGEINT",
  "INTEGER",
  "UINTEGER",
  "SMALLINT",
  "TINYINT",
  "USMALLINT",
  "UTINYINT",
]);

function numeric(value: unknown): unknown {
  if (typeof value !== "string") return value;
  const n = Number(value);
  return Number.isSafeInteger(n) ? n : value;
}

// ---------------------------------------------------------------------------
// AnalysisLive
// ---------------------------------------------------------------------------

export interface AnalysisOptions {
  /** Test seam: how the event-log file is opened. Default: `READ_ONLY` DuckDB. */
  readonly open?: (path: string) => Promise<DuckDBInstance>;
}

const openReadOnly = (path: string) =>
  DuckDBInstance.create(path, { access_mode: "READ_ONLY", threads: "2", memory_limit: "256MB" });

/**
 * Builds the service in the current scope. The file opens on the first `run`, so `names` (and an
 * unknown-name failure) work without one; a file that cannot be opened fails that `run` with
 * `AnalysisError` and is retried on the next.
 */
export function makeAnalysis(
  options: AnalysisOptions = {},
): Effect.Effect<AnalysisService, never, Scope.Scope | Config> {
  return Effect.gen(function* () {
    const config = yield* Config;
    const path = config.eventLog.duckdbPath;
    const open = options.open ?? openReadOnly;
    let handle: { instance: DuckDBInstance; connection: DuckDBConnection } | undefined;

    yield* Effect.addFinalizer(() =>
      Effect.sync(() => {
        handle?.connection.closeSync();
        handle?.instance.closeSync();
        handle = undefined;
      })
    );

    const names = Effect.succeed(ANALYSIS_QUERY_NAMES);

    const connection = (name: string) =>
      Effect.tryPromise({
        try: async () => {
          if (handle === undefined) {
            const instance = await open(path);
            try {
              const connection = await instance.connect();
              await connection.run("SET TimeZone = 'UTC'");
              handle = { instance, connection };
            } catch (error) {
              instance.closeSync();
              throw error;
            }
          }
          return handle.connection;
        },
        catch: (cause) =>
          new AnalysisError({
            name,
            detail: `cannot open the event-log file ${path} read-only: ${
              cause instanceof Error ? cause.message : String(cause)
            }`,
            cause,
          }),
      });

    const run = (
      name: string,
      params: Readonly<Record<string, unknown>>,
    ): Effect.Effect<readonly Row[], AnalysisError | UnknownAnalysisQuery> =>
      Effect.gen(function* () {
        // Own-property lookup: "toString" or "__proto__" is not a catalog name.
        const entry = Object.hasOwn(entries, name) ? entries[name] : undefined;
        if (entry === undefined) {
          return yield* Effect.fail(new UnknownAnalysisQuery({ name, known: ANALYSIS_QUERY_NAMES }));
        }
        const bound = yield* resolveParams(name, entry, params);
        const conn = yield* connection(name);
        return yield* Effect.tryPromise({
          try: async () => {
            const result = await conn.runAndReadAll(entry.sql, { ...bound });
            const types = result.columnTypes();
            const names = result.columnNames();
            return result.getRowObjectsJson().map((row) => {
              const out: Record<string, unknown> = {};
              names.forEach((column, i) => {
                out[column] = INTEGER_TYPES.has(types[i]!.toString()) ? numeric(row[column]) : row[column];
              });
              return out;
            });
          },
          catch: (cause) =>
            new AnalysisError({
              name,
              detail: cause instanceof Error ? cause.message : String(cause),
              cause,
            }),
        });
      });

    return { run, names };
  });
}

/** Live: the read-only analysis service over `Config.eventLog.duckdbPath`. Reader process / CLI only. */
export function analysisLayer(options: AnalysisOptions = {}): Layer.Layer<Analysis, never, Config> {
  return Layer.scoped(Analysis, makeAnalysis(options));
}

export const AnalysisLive: Layer.Layer<Analysis, never, Config> = analysisLayer();
