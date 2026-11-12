/**
 * `deno task perf:ingest` (plan item 25): loads one k6 run's `--out json=` file into a `k6_points`
 * DuckDB table, so client-side latency can be joined against the server's event tables.
 *
 * ```text
 *   logs/k6/<run-id>/raw.json  ──read_json_auto──>  logs/k6/<run-id>/k6.duckdb : k6_points
 * ```
 *
 * - **Run id.** `K6_RUN_ID`, default `k6-smoke` — the same default as `deno task perf:smoke`, so
 *   `deno task perf:smoke && deno task perf:ingest` needs no arguments.
 * - **Output file.** `logs/k6/<run-id>/k6.duckdb` beside the raw file, or `K6_DUCKDB` when set. Never
 *   the server's event-log file: the reader child holds that one open (and locked) while it runs. A
 *   query that needs both attaches the event log, e.g. `ATTACH '<eventlog.duckdb>' AS events
 *   (READ_ONLY)` from a connection on this file.
 * - **Schema.** `k6_points` exactly as `core/eventlog/ddl.ts` declares it — the one place the schema
 *   lives: `ts TIMESTAMP` (UTC), `run_id`, `metric`, `value DOUBLE`, `scenario`, `status`,
 *   `trace_id`, `tags JSON`. Only `Point` lines are loaded; k6 interleaves them with `Metric` lines
 *   that describe each metric once (`agent-work/work-scripts/deno/k6-raw-duckdb-check.ts`).
 * - **Idempotent per run.** One transaction deletes the run's existing rows and inserts the file's
 *   points, so a re-run replaces that run and leaves any other run in the same file untouched.
 *
 * k6 writes `data.time` as RFC 3339 with the host's offset and nanosecond digits; it is read as
 * `TIMESTAMPTZ` and stored as UTC, the same convention as the event tables (`ts` there is UTC too).
 * `trace_id` is the point's `trace_id` tag when a script sets one; `perf/lib/api.js` sends
 * `X-Trace-Id` as a request header only, so for today's scenarios it is `NULL` and a join correlates
 * by time bucket and by the run id embedded in the server-side trace ids instead.
 *
 * Loads `@duckdb/node-api` and nothing that loads `node:sqlite` (ADR 0001).
 */
import { dirname, resolve } from "node:path";
import { DuckDBInstance } from "@duckdb/node-api";
import { EVENT_TABLE_DDL } from "../core/eventlog/ddl.ts";

/** The default run id, shared with `deno task perf:smoke`. */
export const DEFAULT_RUN_ID = "k6-smoke";

/** A string as a DuckDB SQL literal. */
function sqlString(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

export interface IngestK6Result {
  readonly runId: string;
  readonly rawPath: string;
  readonly dbPath: string;
  /** Rows of this run in `k6_points` after the load. */
  readonly points: number;
  /** Rows of this run the load replaced. */
  readonly replaced: number;
}

/** Loads `rawPath`'s `Point` lines into `dbPath`'s `k6_points` under `runId`, replacing that run. */
export async function ingestK6(runId: string, rawPath: string, dbPath: string): Promise<IngestK6Result> {
  await Deno.stat(rawPath);
  await Deno.mkdir(dirname(dbPath), { recursive: true });
  const instance = await DuckDBInstance.create(dbPath);
  try {
    const connection = await instance.connect();
    try {
      await connection.run("SET TimeZone = 'UTC'");
      await connection.run(EVENT_TABLE_DDL.k6_points);
      const run = sqlString(runId);
      const count = async (): Promise<number> =>
        Number(
          (await connection.runAndReadAll(`SELECT count(*) AS n FROM k6_points WHERE run_id = ${run}`))
            .getRowObjects()[0].n,
        );
      await connection.run("BEGIN TRANSACTION");
      try {
        const replaced = await count();
        await connection.run(`DELETE FROM k6_points WHERE run_id = ${run}`);
        // Explicit columns: `data` stays JSON whatever metric a line carries, so one odd line cannot
        // change the inferred type of every other one.
        await connection.run(
          `INSERT INTO k6_points (ts, run_id, metric, value, scenario, status, trace_id, tags)
SELECT CAST(data ->> 'time' AS TIMESTAMPTZ) AT TIME ZONE 'UTC',
       ${run},
       metric,
       CAST(data ->> 'value' AS DOUBLE),
       data -> 'tags' ->> 'scenario',
       data -> 'tags' ->> 'status',
       data -> 'tags' ->> 'trace_id',
       data -> 'tags'
FROM read_json_auto(${sqlString(rawPath)}, format = 'newline_delimited',
                    columns = {type: 'VARCHAR', metric: 'VARCHAR', data: 'JSON'})
WHERE type = 'Point'`,
        );
        const points = await count();
        await connection.run("COMMIT");
        return { runId, rawPath, dbPath, points, replaced };
      } catch (error) {
        await connection.run("ROLLBACK").catch(() => {});
        throw error;
      }
    } finally {
      connection.closeSync();
    }
  } finally {
    instance.closeSync();
  }
}

if (import.meta.main) {
  const runId = Deno.env.get("K6_RUN_ID") || DEFAULT_RUN_ID;
  const rawPath = resolve("logs", "k6", runId, "raw.json");
  const dbPath = resolve(Deno.env.get("K6_DUCKDB") || resolve("logs", "k6", runId, "k6.duckdb"));
  try {
    const result = await ingestK6(runId, rawPath, dbPath);
    console.log(
      `perf:ingest: ${result.points} k6 points loaded into k6_points for run_id=${runId} -> ${dbPath}` +
        ` (replaced ${result.replaced})`,
    );
  } catch (error) {
    console.error(`perf:ingest: run_id=${runId} raw=${rawPath}: ${error}`);
    Deno.exit(1);
  }
}
