/**
 * The analysis query catalog (plan item 23): `eventlog/catalog.ts`, `AnalysisLive`, and the
 * `scripts/logs.ts` command line over it.
 *
 * - **Catalog hygiene** — every SQL string is a single `SELECT`/`WITH` with no write or DDL keyword,
 *   and every `$placeholder` is a declared parameter and vice versa.
 * - **Doc cannot drift** — `docs/observability.md` documents exactly the catalog's names and every
 *   column of every table `EVENTLOG_DDL` creates.
 * - **Behavior** — over a real DuckDB file built from `EVENTLOG_DDL` and a small fixture, each query
 *   runs and the ones with an arithmetic story return the numbers that story predicts; unknown names,
 *   unknown/missing parameters and an unreadable file are typed failures.
 * - **CLI** — argument parsing, the three output formats, and the exit codes.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DuckDBInstance } from "@duckdb/node-api";
import { Effect, Either, Layer } from "effect";
import { AnalysisError, UnknownAnalysisQuery } from "../../core/errors.ts";
import { EVENT_TABLE_DDL, EVENT_TABLES, EVENTLOG_DDL } from "../../core/eventlog/ddl.ts";
import {
  ANALYSIS_QUERY_NAMES,
  analysisLayer,
  CATALOG,
  type CatalogEntry,
  resolveParams,
} from "../../eventlog/catalog.ts";
import { ConfigTest } from "../../eventlog/config.ts";
import { Analysis } from "../../eventlog/services.ts";
import { formatRows, parseQueryArgs, run, UsageError } from "../../scripts/logs.ts";

const entries = Object.entries(CATALOG) as [string, CatalogEntry][];
const DOC = await Deno.readTextFile(new URL("../../docs/observability.md", import.meta.url));

// ---------------------------------------------------------------------------
// Catalog hygiene
// ---------------------------------------------------------------------------

test("catalog: every entry is one SELECT/WITH statement with no write or DDL keyword", () => {
  const forbidden =
    /\b(insert|update|delete|drop|create|alter|attach|detach|copy|pragma|install|load|call|set|export|import|vacuum|checkpoint)\b/i;
  assert.ok(entries.length >= 10, "the ten §4.3 queries");
  for (const [name, entry] of entries) {
    const sql = entry.sql.trim();
    assert.match(sql, /^(SELECT|WITH)\b/i, `${name} must start with SELECT or WITH`);
    assert.ok(!sql.includes(";"), `${name} must be a single statement`);
    // Quoted literals ('commit', 'high-water', …) are data, not statements.
    const code = sql.replace(/'(?:[^']|'')*'/g, "''");
    assert.doesNotMatch(code, forbidden, `${name} contains a write/DDL keyword`);
  }
});

test("catalog: placeholders and declared parameters match exactly", () => {
  for (const [name, entry] of entries) {
    const used = new Set([...entry.sql.matchAll(/\$([A-Za-z_]\w*)/g)].map((m) => m[1]!));
    assert.deepEqual([...used].sort(), Object.keys(entry.params).sort(), `${name} parameters`);
    assert.ok(entry.description.length > 0, `${name} has a description`);
  }
});

test("catalog: names are derived from the record", () => {
  assert.deepEqual([...ANALYSIS_QUERY_NAMES], Object.keys(CATALOG));
  assert.deepEqual([...ANALYSIS_QUERY_NAMES].sort(), [
    "backup-history",
    "deadline-hits-per-handler-hour",
    "error-rate-by-handler",
    "handler-throughput",
    "k6-vs-platform",
    "mutation-vs-query-latency",
    "plan-shapes",
    "queue-depth",
    "slowest-sql",
    "trace",
    "tx-outcomes",
  ]);
});

// ---------------------------------------------------------------------------
// docs/observability.md cannot drift
// ---------------------------------------------------------------------------

/** The text under `## <heading>` up to the next `## ` heading. */
function section(heading: string): string {
  const start = DOC.indexOf(`\n## ${heading}\n`);
  assert.notEqual(start, -1, `docs/observability.md has a "## ${heading}" section`);
  const rest = DOC.slice(start + 1);
  const end = rest.indexOf("\n## ", 1);
  return end === -1 ? rest : rest.slice(0, end);
}

test("doc: the named queries section lists exactly the catalog names", () => {
  const documented = [...section("Named queries").matchAll(/^### `([^`]+)`/gm)].map((m) => m[1]!);
  assert.deepEqual([...documented].sort(), [...ANALYSIS_QUERY_NAMES].sort());
});

test("doc: every parameter of every catalog entry is documented under its query", () => {
  const queries = section("Named queries");
  for (const [name, entry] of entries) {
    const start = queries.indexOf(`### \`${name}\``);
    const next = queries.indexOf("\n### ", start + 1);
    const body = queries.slice(start, next === -1 ? undefined : next);
    for (const param of Object.keys(entry.params)) {
      assert.ok(body.includes(`\`${param}\``), `${name}: parameter ${param} is not documented`);
    }
  }
});

test("doc: every column of every event-log table is documented", async () => {
  const dir = await mkdtemp(join(tmpdir(), "analysis-doc-"));
  const instance = await DuckDBInstance.create(join(dir, "events.duckdb"));
  const connection = await instance.connect();
  try {
    for (const ddl of EVENTLOG_DDL) await connection.run(ddl);
    const schema = await connection.runAndReadAll(
      "SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = 'main' " +
        "ORDER BY table_name, ordinal_position",
    );
    const tables = section("Tables");
    const byTable = new Map<string, string[]>();
    for (const row of schema.getRowObjects()) {
      const table = String(row.table_name);
      byTable.set(table, [...(byTable.get(table) ?? []), String(row.column_name)]);
    }
    for (const table of [...EVENT_TABLES, "_ingest_state", "_eventlog_schema"]) {
      const columns = byTable.get(table);
      assert.ok(columns !== undefined && columns.length > 0, `${table} exists in the DDL`);
      const start = tables.indexOf(`### \`${table}\``);
      assert.notEqual(start, -1, `${table} has a heading in "## Tables"`);
      const next = tables.indexOf("\n### ", start + 1);
      const body = tables.slice(start, next === -1 ? undefined : next);
      for (const column of columns!) {
        assert.match(body, new RegExp(`^\\| \`${column}\`\\s+\\|`, "m"), `${table}.${column} is documented`);
      }
    }
    assert.ok(Object.keys(EVENT_TABLE_DDL).length === EVENT_TABLES.length);
  } finally {
    connection.closeSync();
    instance.closeSync();
    await rm(dir, { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------
// resolveParams
// ---------------------------------------------------------------------------

test("resolveParams: defaults, coercion, required, unknown, and bad values", async () => {
  const entry = CATALOG["slowest-sql"] as CatalogEntry;
  const ok = await Effect.runPromise(resolveParams("slowest-sql", entry, {
    since: "2026-10-10 12:00:00",
    limit: "5",
  }));
  assert.deepEqual(ok, { since: "2026-10-10T12:00:00.000", until: null, appId: null, limit: 5 });
  const offset = await Effect.runPromise(
    resolveParams("slowest-sql", entry, { until: "2026-10-10T12:00:00+02:00" }),
  );
  assert.equal(offset.until, "2026-10-10T10:00:00.000", "an offset is converted to UTC");
  assert.equal((await Effect.runPromise(resolveParams("slowest-sql", entry, {}))).limit, 100);

  const fails = (given: Record<string, unknown>, e: CatalogEntry = entry) =>
    Effect.runPromise(Effect.flip(resolveParams("q", e, given)));
  assert.match(String((await fails({ bogus: 1 })).detail), /unknown parameter.*bogus/);
  assert.match(String((await fails({ limit: "many" })).detail), /number/);
  assert.match(String((await fails({ since: "yesterday-ish" })).detail), /ISO-8601/);
  assert.match(String((await fails({}, CATALOG.trace)).detail), /missing required parameter: traceId/);
  assert.match(String((await fails({ appId: { sql: "1" } })).detail), /string or number/);
});

// ---------------------------------------------------------------------------
// AnalysisLive over a real file
// ---------------------------------------------------------------------------

async function withFixture<A>(
  populate: ((sql: (text: string) => Promise<unknown>) => Promise<void>) | undefined,
  body: (dbPath: string) => Promise<A>,
): Promise<A> {
  const dir = await mkdtemp(join(tmpdir(), "analysis-catalog-"));
  const dbPath = join(dir, "events.duckdb");
  try {
    if (populate !== undefined) {
      const instance = await DuckDBInstance.create(dbPath);
      const connection = await instance.connect();
      try {
        await connection.run("SET TimeZone = 'UTC'");
        for (const ddl of EVENTLOG_DDL) await connection.run(ddl);
        await populate((text) => connection.run(text));
      } finally {
        connection.closeSync();
        instance.closeSync();
      }
    }
    return await body(dbPath);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

const analysis = (dbPath: string) =>
  analysisLayer().pipe(Layer.provide(ConfigTest({ eventLog: { duckdbPath: dbPath } })));

const runQuery = <A, E>(dbPath: string, f: (a: typeof Analysis.Service) => Effect.Effect<A, E>) =>
  Effect.runPromise(
    Effect.scoped(Effect.flatMap(Analysis, f)).pipe(Effect.provide(analysis(dbPath))),
  );

const TRACE = "trace-aaaa";

async function fixture(sql: (text: string) => Promise<unknown>): Promise<void> {
  // Two handlers: `play` is a mutation (writer path), `get` a query (reader path).
  await sql(`INSERT INTO http_events VALUES
    ('p-http', '2026-10-10 12:00:05', '${TRACE}', 'POST', '/rpc/play', 'app1', 'mutation', 'play', 200, 10.0, 10, 20, NULL),
    ('h2', '2026-10-10 12:00:20', 't2', 'POST', '/rpc/play', 'app1', 'mutation', 'play', 409, 30.0, 10, 20, 'MutationError'),
    ('h3', '2026-10-10 12:00:25', 't3', 'POST', '/rpc/get', 'app1', 'query', 'get', 200, 2.0, 10, 20, NULL),
    ('h4', '2026-10-10 12:01:05', 't4', 'POST', '/rpc/get', 'app1', 'query', 'get', 200, 4.0, 10, 20, NULL)`);
  await sql(`INSERT INTO platform_events (event_id, ts, level, role, pid, trace_id, span_id, parent_span_id,
      app_id, event_kind, handler_kind, name, outcome, error_tag, duration_ms, attrs) VALUES
    ('p-http', '2026-10-10 12:00:05', 'info', 'server', 1, '${TRACE}', 's1', NULL, 'app1', 'http', 'mutation', 'play', NULL, NULL, 10.0, NULL),
    ('p-handler', '2026-10-10 12:00:05', 'info', 'server', 1, '${TRACE}', 's2', 's1', 'app1', 'handler', 'mutation', 'play', 'ok', NULL, 9.0, NULL),
    ('p-sql', '2026-10-10 12:00:05', 'info', 'server', 1, '${TRACE}', 's3', 's2', 'app1', 'sql', NULL, NULL, 'ok', NULL, 1.0, NULL),
    ('p-tx', '2026-10-10 12:00:05', 'info', 'server', 1, '${TRACE}', 's7', 's2', 'app1', 'tx', NULL, NULL, 'commit', NULL, 2.0, NULL),
    ('p-reader', '2026-10-10 12:00:06', 'info', 'reader', 2, '${TRACE}', 's4', 's2', 'app1', 'handler', 'query', 'get', 'ok', NULL, 1.0, NULL),
    ('p-other', '2026-10-10 12:00:07', 'info', 'server', 1, 'other', 's5', NULL, 'app1', 'handler', 'query', 'get', 'ok', NULL, 1.0, NULL),
    ('p-slow', '2026-10-10 12:00:30', 'info', 'server', 1, 't2', 's6', NULL, 'app1', 'handler', 'mutation', 'play', 'error', 'MutationError', 30.0, NULL)`);
  await sql(`INSERT INTO sql_events (event_id, ts, trace_id, app_id, engine, dialect, statement_kind,
    table_name, sql_text, bind_count, duration_ms, row_count, outcome, error_tag) VALUES
    ('p-sql', '2026-10-10 12:00:05', '${TRACE}', 'app1', 'sqlite', 'sqlite', 'insert', 'games', 'INSERT INTO games VALUES (?)', 1, 1.0, 1, 'ok', NULL),
    ('s-b', '2026-10-10 12:00:10', 't9', 'app1', 'sqlite', 'sqlite', 'insert', 'games', 'INSERT INTO games VALUES (?)', 1, 3.0, 1, 'ok', NULL),
    ('s-c', '2026-10-10 12:00:11', 't9', 'app1', 'duckdb', 'duckdb', 'select', 'games', 'SELECT * FROM games', 0, 7.0, 4, 'ok', NULL)`);
  await sql(`INSERT INTO tx_events VALUES
    ('p-tx', '2026-10-10 12:00:05', '${TRACE}', 'app1', 'sqlite', 'commit', 1, 2.0, 0.5, NULL),
    ('x2', '2026-10-10 12:00:20', 't2', 'app1', 'sqlite', 'rollback', 1, 3.0, 1.5, 'MutationError')`);
  await sql(`INSERT INTO queue_events VALUES
    ('q1', '2026-10-10 12:00:10', 'writer', 3, 64, 'high-water', 0),
    ('q2', '2026-10-10 12:00:20', 'writer', 9, 64, 'high-water', 0),
    ('q3', '2026-10-10 12:00:30', 'writer', 64, 64, 'rejected', 0),
    ('q4', '2026-10-10 12:00:40', 'logger', 100, 100, 'dropped', 7)`);
  await sql(`INSERT INTO backup_events VALUES
    ('b1', '2026-10-10 12:00:00', 'app1', 'snapshot', 'backups', 'r1', NULL, NULL, NULL, NULL, 5.0, 'ok', NULL),
    ('b2', '2026-10-10 12:00:01', 'app1', 'publish-latest', 'backups', 'r1', 'apps/app1/latest.json', NULL, NULL, NULL, 1.0, 'ok', NULL),
    ('b3', '2026-10-10 12:10:00', 'app1', 'snapshot', 'backups', 'r2', NULL, NULL, NULL, NULL, 5.0, 'error', 'BackupError'),
    ('b4', '2026-10-10 12:20:01', 'app1', 'publish-latest', 'backups', 'r3', 'apps/app1/latest.json', NULL, NULL, NULL, 1.0, 'ok', NULL),
    ('b5', '2026-10-10 12:00:01', 'app1', 'put-object', 'object-store', 'r1', 'apps/app1/x', 10, 'ab', 1, 2.0, 'ok', NULL)`);
  await sql(`INSERT INTO k6_points VALUES
    ('2026-10-10 12:00:05', 'run1', 'http_req_duration', 15.0, 'mixed', '200', '${TRACE}', NULL),
    ('2026-10-10 12:00:06', 'run1', 'http_req_duration', 25.0, 'mixed', '200', 't2', NULL)`);
}

test("AnalysisLive: every catalog query runs against an empty file and against the fixture", async () => {
  await withFixture(async () => {}, async (dbPath) => {
    for (const name of ANALYSIS_QUERY_NAMES) {
      const params = name === "trace" ? { traceId: "none" } : {};
      assert.deepEqual(await runQuery(dbPath, (a) => a.run(name, params)), [], `${name} on an empty file`);
    }
  });
  await withFixture(async (sql) => {
    await fixture(sql);
    // This query only returns deadline events; the shared fixture has none.
    await sql(`INSERT INTO sql_events (event_id, ts, trace_id, app_id, error_tag, path) VALUES
      ('s-deadline', '2026-10-10 12:00:19.990', 't2', 'app1', 'StatementDeadlineError', 'nonblocking')`);
  }, async (dbPath) => {
    for (const name of ANALYSIS_QUERY_NAMES) {
      const params = name === "trace" ? { traceId: TRACE } : {};
      const rows = await runQuery(dbPath, (a) => a.run(name, params));
      assert.ok(rows.length > 0, `${name} returns rows over the fixture`);
    }
  });
});

test("AnalysisLive: query results", async () => {
  await withFixture(fixture, async (dbPath) => {
    const q = (name: string, params: Record<string, unknown> = {}) =>
      runQuery(dbPath, (a) => a.run(name, params));

    const slow = await q("slowest-sql");
    assert.equal(slow[0]!.engine, "duckdb", "ordered by p95 descending");
    const sqliteInsert = slow.find((r) => r.engine === "sqlite")!;
    assert.equal(sqliteInsert.calls, 2);
    assert.equal(sqliteInsert.p50_ms, 2);
    assert.equal(typeof sqliteInsert.calls, "number", "counts are numbers, not strings");

    const shapes = await q("plan-shapes");
    assert.equal(shapes.find((r) => r.sql_text === "INSERT INTO games VALUES (?)")!.calls, 2);

    const errors = await q("error-rate-by-handler");
    const play = errors.find((r) => r.name === "play")!;
    assert.equal(play.requests, 2);
    assert.equal(play.errors, 1);
    assert.equal(play.error_rate, 0.5);
    assert.equal(play.error_tags, "MutationError:1");
    assert.equal(errors.find((r) => r.name === "get")!.error_rate, 0);

    const throughput = await q("handler-throughput", { appId: "app1" });
    assert.equal(throughput.length, 3, "play@12:00, get@12:00, get@12:01");
    assert.equal(throughput.find((r) => r.name === "play")!.status_4xx, 1);

    const kinds = await q("mutation-vs-query-latency");
    assert.deepEqual(
      kinds.filter((r) => String(r.bucket).startsWith("2026-10-10 12:00")).map((
        r,
      ) => [r.handler_kind, r.path, r.handlers]),
      [["mutation", "writer", 2], ["query", "reader", 2]],
    );

    const queue = await q("queue-depth");
    const writer = queue.find((r) => r.queue === "writer")!;
    assert.equal(writer.max_depth, 64);
    assert.equal(writer.high_water_events, 2);
    assert.equal(writer.rejected_events, 1);
    assert.equal(queue.find((r) => r.queue === "logger")!.dropped_total, 7);

    const tx = await q("tx-outcomes");
    assert.deepEqual(tx.map((r) => [r.outcome, r.transactions]), [["commit", 1], ["rollback", 1]]);

    const backups = await q("backup-history");
    assert.equal(backups.length, 4, "object-store transfer rows are not phases");
    const finished = backups.filter((r) => r.phase === "publish-latest");
    assert.equal(finished[0]!.gap_since_previous_success, null);
    assert.ok(finished[1]!.gap_since_previous_success !== null, "second success carries the gap");

    const joined = await q("k6-vs-platform");
    assert.equal(joined.length, 1);
    assert.equal(joined[0]!.client_p95_ms, 24.5);
    assert.equal(joined[0]!.server_p95_ms, 10, "only the 12:00:05 request is in the 12:00:00 bucket");
    assert.equal(joined[0]!.transport_overhead_p95_ms, 14.5);

    const since = await q("slowest-sql", { since: "2026-10-10T12:00:10" });
    assert.equal(since.reduce((n, r) => n + Number(r.calls), 0), 2, "since is inclusive");
    const limited = await q("slowest-sql", { limit: 1 });
    assert.equal(limited.length, 1);
  });
});

test("AnalysisLive: deadline-hits-per-handler-hour pins its counting, attribution and bucketing rules", async () => {
  // `http_events.ts` is the request's completion time; the request spans [ts - duration_ms, ts]. A deadline
  // event matches a request of the same trace and app when it falls in that span widened by 1 ms on each
  // side. Ties go to the earliest-completing request.
  const http = (id: string, ts: string, trace: string, kind: string, name: string, ms: number) =>
    `('${id}', '${ts}', '${trace}', 'POST', '/rpc/${name}', 'app1', '${kind}', '${name}', 504, ${ms}, 10, 20, 'StatementDeadlineError')`;
  const deadline = (id: string, ts: string, trace: string, app: string, path: string | null) =>
    `('${id}', '${ts}', '${trace}', '${app}', 'StatementDeadlineError', ${
      path === null ? "NULL" : `'${path}'`
    })`;
  await withFixture(async (sql) => {
    await sql(`INSERT INTO http_events VALUES
      ${http("r-play", "2026-10-11 12:00:05", "dl-a", "mutation", "play", 10)},
      ${http("r-edge", "2026-10-11 12:59:59.999", "dl-h", "query", "edge", 5)},
      ${http("r-x1", "2026-10-11 14:00:10.000", "dl-s", "mutation", "x1", 5)},
      ${http("r-y1", "2026-10-11 14:00:10.050", "dl-s", "query", "y1", 55)},
      ${http("r-a1", "2026-10-11 15:00:00.020", "dl-b", "mutation", "a1", 10)},
      ${http("r-b1", "2026-10-11 15:00:00.030", "dl-b", "query", "b1", 9)},
      ${http("r-w1", "2026-10-11 16:00:10.000", "dl-w", "mutation", "w1", 100)}`);
    await sql(`INSERT INTO sql_events (event_id, ts, trace_id, app_id, error_tag, path) VALUES
      ${deadline("d1", "2026-10-11 12:00:04.995", "dl-a", "app1", "nonblocking")},
      ${deadline("d2", "2026-10-11 12:00:04.995", "dl-a", "app1", "nonblocking")},
      ${deadline("d3", "2026-10-11 12:00:05.001", "dl-a", "app1", "blocking")},
      ${deadline("d4", "2026-10-11 12:00:05.002", "dl-a", "app1", "nonblocking")},
      ${deadline("d5", "2026-10-11 12:00:04.995", "dl-a", "app2", "nonblocking")},
      ${deadline("d6", "2026-10-11 12:00:05.000", "dl-a", "app1", null)},
      ${deadline("d7", "2026-10-11 13:00:00.000", "dl-h", "app1", "blocking")},
      ${deadline("d8", "2026-10-11 14:00:10.000", "dl-s", "app1", "nonblocking")},
      ${deadline("d9", "2026-10-11 15:00:00.021", "dl-b", "app1", "nonblocking")},
      ${deadline("d10", "2026-10-11 15:00:00.025", "dl-b", "app1", "nonblocking")},
      ${deadline("d11", "2026-10-11 15:00:00.015", "dl-b", "app1", "nonblocking")},
      ${deadline("d12", "2026-10-11 16:00:09.899", "dl-w", "app1", "nonblocking")},
      ${deadline("d13", "2026-10-11 16:00:09.898", "dl-w", "app1", "blocking")},
      ${deadline("d14", "2026-10-11 16:00:09.950", "dl-other", "app1", null)},
      ('not-deadline', '2026-10-11 12:00:05.000', 'dl-a', 'app1', 'SqliteError', 'nonblocking'),
      ('no-error', '2026-10-11 12:00:05.000', 'dl-a', 'app1', NULL, 'nonblocking')`);
  }, async (dbPath) => {
    const rows = await runQuery(dbPath, (a) => a.run("deadline-hits-per-handler-hour", {}));
    const hits = (app: string, name: string | null, path: string | null) =>
      rows.filter((r) => r.app_id === app && r.name === name && r.path === path)
        .reduce((n, r) => n + Number(r.deadline_hits), 0);

    // One hit per deadline-tagged sql event: 14 events, no double count, other error tags excluded.
    assert.equal(rows.reduce((n, r) => n + Number(r.deadline_hits), 0), 14);
    assert.equal(typeof rows[0]!.deadline_hits, "number", "counts are numbers, not strings");

    // Attribution to the completing request by trace and app, with the path column carried through.
    assert.equal(hits("app1", "play", "nonblocking"), 2, "d1 and d2 share a timestamp and both count once");
    assert.equal(hits("app1", "play", "blocking"), 1, "d3 sits exactly on the +1 ms upper tolerance");
    assert.equal(hits("app1", "play", null), 1, "a pre-FFI-06 row has a NULL path and still counts");
    const play = rows.filter((r) => r.name === "play");
    assert.ok(play.every((r) => r.handler_kind === "mutation"));
    assert.ok(play.every((r) => String(r.hour).startsWith("2026-10-11 12:00:00")));

    // Outside the window (d4, +2 ms) and in another app (d5): unattributed rows, still counted.
    assert.equal(hits("app1", null, "nonblocking"), 1, "d4 is 2 ms past the request");
    assert.equal(hits("app2", null, "nonblocking"), 1, "same trace id in another app never matches");

    // The hour bucket comes from the sql event, not the request: d7 is in 13:00 though its request ended 12:59.
    const edge = rows.filter((r) => r.name === "edge");
    assert.equal(edge.length, 1);
    assert.equal(edge[0]!.handler_kind, "query");
    assert.ok(String(edge[0]!.hour).startsWith("2026-10-11 13:00:00"));

    // Documented limit: simultaneous requests sharing a trace -> the earliest-completing request wins.
    assert.equal(hits("app1", "x1", "nonblocking"), 1);
    assert.equal(hits("app1", "y1", "nonblocking"), 0);

    // Documented limit: back-to-back trace reuse -> the 1 ms after the earlier request ends (d9) goes to it
    // even though the later request has already started; later events fall to the later request.
    assert.equal(hits("app1", "a1", "nonblocking"), 2, "d11 plus the d9 tail");
    assert.equal(hits("app1", "b1", "nonblocking"), 1, "d10 only");

    // Trace and time vary independently. r-w1 completes at 16:00:10.000 after 100 ms, so it started at
    // 16:00:09.900 and its widened start is 09.899. d12 is exactly 1 ms before the start: still attributed.
    // d13 is 2 ms before it: below the lower bound, even though the trace matches. d14 sits inside the
    // request's span in the same app but carries another trace: never attributed by time alone.
    const hour16 = rows.filter((r) => String(r.hour).startsWith("2026-10-11 16:00:00"))
      .map((r) => `${r.app_id}/${r.name ?? "unattributed"}/${r.path}=${r.deadline_hits}`).sort();
    assert.deepEqual(hour16, [
      "app1/unattributed/blocking=1",
      "app1/unattributed/null=1",
      "app1/w1/nonblocking=1",
    ]);
  });
});

test("AnalysisLive: trace is the ordered cross-process timeline without double-listing typed events", async () => {
  await withFixture(fixture, async (dbPath) => {
    const rows = await runQuery(dbPath, (a) => a.run("trace", { traceId: TRACE }));
    const ids = rows.map((r) => r.event_id);
    // platform_events rows of kind http/sql/tx are shown once, via their typed table.
    assert.deepEqual(new Set(ids).size, ids.length);
    assert.deepEqual(rows.map((r) => r.source).sort(), ["http", "k6", "platform", "platform", "sql", "tx"]);
    const timestamps = rows.map((r) => String(r.ts));
    assert.deepEqual(timestamps, [...timestamps].sort());
    const roles = new Set(rows.map((r) => r.role).filter((r) => r !== null));
    assert.deepEqual([...roles].sort(), ["reader", "server"], "both processes appear");
    const sql = rows.find((r) => r.source === "sql")!;
    assert.equal(sql.role, "server", "a typed row takes its role and span from platform_events");
    assert.equal(sql.span_id, "s3");
    assert.equal(sql.parent_span_id, "s2");
    assert.deepEqual(await runQuery(dbPath, (a) => a.run("trace", { traceId: "nope" })), []);
  });
});

test("AnalysisLive: typed failures", async () => {
  await withFixture(fixture, async (dbPath) => {
    const unknown = await runQuery(dbPath, (a) => Effect.flip(a.run("not-a-query", {})));
    assert.ok(unknown instanceof UnknownAnalysisQuery);
    assert.equal(unknown.name, "not-a-query");
    assert.deepEqual([...unknown.known], [...ANALYSIS_QUERY_NAMES]);
    const proto = await runQuery(dbPath, (a) => Effect.flip(a.run("toString", {})));
    assert.ok(proto instanceof UnknownAnalysisQuery, "inherited properties are not catalog names");

    const missing = await runQuery(dbPath, (a) => Effect.flip(a.run("trace", {})));
    assert.ok(missing instanceof AnalysisError);
    const stray = await runQuery(
      dbPath,
      (a) => Effect.flip(a.run("slowest-sql", { sql: "DROP TABLE sql_events" })),
    );
    assert.ok(stray instanceof AnalysisError);
    assert.match(String(stray.detail), /unknown parameter/);
    assert.equal((await runQuery(dbPath, (a) => a.names)).length, ANALYSIS_QUERY_NAMES.length);
  });
  await withFixture(undefined, async (dbPath) => {
    const absent = await Effect.runPromise(
      Effect.scoped(Effect.flatMap(Analysis, (a) => Effect.either(a.run("slowest-sql", {})))).pipe(
        Effect.provide(analysis(dbPath)),
      ),
    );
    assert.ok(Either.isLeft(absent) && absent.left instanceof AnalysisError);
    assert.match(String((absent.left as AnalysisError).detail), /read-only/);
    // The failed open created nothing.
    await assert.rejects(Deno.stat(dbPath));
  });
});

// ---------------------------------------------------------------------------
// scripts/logs.ts
// ---------------------------------------------------------------------------

test("logs: argument parsing", () => {
  assert.deepEqual(parseQueryArgs(["trace", "--param", "traceId=abc", "--format=csv"]), {
    name: "trace",
    params: { traceId: "abc" },
    format: "csv",
  });
  assert.deepEqual(parseQueryArgs(["--param=since=2026-10-10 12:00:00", "slowest-sql"]).params, {
    since: "2026-10-10 12:00:00",
  });
  assert.throws(() => parseQueryArgs([]), UsageError);
  assert.throws(() => parseQueryArgs(["a", "b"]), UsageError);
  assert.throws(() => parseQueryArgs(["a", "--format", "xml"]), UsageError);
  assert.throws(() => parseQueryArgs(["a", "--param", "novalue"]), UsageError);
  assert.throws(() => parseQueryArgs(["a", "--bogus"]), UsageError);
});

test("logs: table, json and csv output", () => {
  const rows = [{ name: "play", n: 2, note: 'say "hi", ok' }, { name: "get", n: 10, note: null }];
  assert.equal(
    formatRows(rows, "table"),
    ["name  n   note", "----  --  ------------", 'play  2   say "hi", ok', "get   10", "(2 rows)"].join("\n"),
  );
  assert.equal(formatRows(rows, "csv"), 'name,n,note\nplay,2,"say ""hi"", ok"\nget,10,');
  assert.deepEqual(JSON.parse(formatRows(rows, "json")), rows);
  assert.equal(formatRows([], "table"), "(0 rows)");
  assert.equal(formatRows([], "json"), "[]");
});

test("logs: list, query and exit codes", async () => {
  const listed = await run(["list"]);
  assert.equal(listed.code, 0);
  for (const name of ANALYSIS_QUERY_NAMES) assert.match(listed.stdout!, new RegExp(`^${name}\\s`, "m"));

  const unknown = await run(["query", "not-a-query"]);
  assert.equal(unknown.code, 2);
  assert.match(unknown.stderr!, /UnknownAnalysisQuery/);
  for (const name of ANALYSIS_QUERY_NAMES) assert.ok(unknown.stderr!.includes(name));

  assert.equal((await run(["query"])).code, 1);
  assert.equal((await run(["nonsense"])).code, 1);
  assert.equal((await run(["sweep", "--bogus"])).code, 1);
});
