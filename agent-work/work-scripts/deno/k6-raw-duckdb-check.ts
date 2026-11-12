// k6-raw-duckdb-check.ts — prove a k6 `--out json=` file is readable by DuckDB's read_json_auto and
// print row counts by line type plus per-metric point counts (sq-04: Metric lines must be filtered
// out with type = 'Point'). In-memory DuckDB; reads the one file given.
// Usage: deno run -A work-scripts/deno/k6-raw-duckdb-check.ts logs/k6/<run-id>/raw.json
import { DuckDBInstance } from "@duckdb/node-api";

const path = Deno.args[0];
if (!path) throw new Error("usage: k6-raw-duckdb-check.ts <raw.json>");
const literal = `'${path.replaceAll("'", "''")}'`;

const instance = await DuckDBInstance.create(":memory:");
const conn = await instance.connect();
const rows = async (sql: string) => (await conn.runAndReadAll(sql)).getRowObjectsJson();

console.log(
  "lines by type:",
  await rows(`SELECT type, count(*) AS n FROM read_json_auto(${literal}) GROUP BY 1`),
);
console.log(
  "points by metric:",
  await rows(
    `SELECT metric, count(*) AS n FROM read_json_auto(${literal}) WHERE type = 'Point' GROUP BY 1 ORDER BY n DESC LIMIT 8`,
  ),
);
conn.closeSync();
instance.closeSync();
