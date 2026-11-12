/**
 * What does DuckDB's JSON reader do with large integers, integral doubles, and timestamps with an
 * offset? Prints one line per probe. Backs the "Large integers" note in `eventlog/tables.ts`
 * (item 21; the concern came from the item-13 review of SQLite's `json_extract`).
 *
 * Opens an in-memory DuckDB only: no filesystem, no network.
 *
 *   deno run -A work-scripts/deno/duckdb-json-bigint-probe.ts
 */
import { DuckDBInstance } from "@duckdb/node-api";

const instance = await DuckDBInstance.create(":memory:");
const connection = await instance.connect();

const literals = [
  "9007199254740993",
  "9007199254740993.0",
  "1e16",
  "1e+21",
  "18446744073709551617",
  '"123"',
  "12.5",
];

console.log("literal | json_type | ->> text | TRY_CAST(text AS BIGINT)");
for (const literal of literals) {
  const json = `{"a":${literal}}`.replaceAll("'", "''");
  const reader = await connection.runAndReadAll(
    `SELECT json_type('${json}', '$.a') AS t, ('${json}' ->> '$.a') AS text,
       TRY_CAST(('${json}' ->> '$.a') AS BIGINT) AS big`,
  );
  const [row] = reader.getRowObjects();
  console.log(`${literal} | ${row.t} | ${row.text} | ${row.big ?? "NULL"}`);
}

const ts = await connection.runAndReadAll(
  `SELECT CAST(TRY_CAST('2026-10-10T12:00:00+02:00' AS TIMESTAMP) AS VARCHAR) AS plain,
     CAST(TRY_CAST('2026-10-10T12:00:00+02:00' AS TIMESTAMPTZ) AT TIME ZONE 'UTC' AS VARCHAR) AS utc`,
);
const [row] = ts.getRowObjects();
console.log(`'2026-10-10T12:00:00+02:00' AS TIMESTAMP -> ${row.plain}; AS TIMESTAMPTZ at UTC -> ${row.utc}`);

connection.closeSync();
instance.closeSync();
