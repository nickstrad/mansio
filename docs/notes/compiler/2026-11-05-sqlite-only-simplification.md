# What a SQLite-only compiler would save

- **Date:** 2026-11-05
- **Question:** How much simpler would the path from SDK to SQL be if the DuckDB side were removed and
  everything targeted SQLite?
- **Read at:** `422ac91`
- **Evidence:** read the cited lines. The counts come from
  `grep -rn 'dialect ===\|Dialect\b\|placeholder(' core sdk`, `grep -rln duckdb tests scripts perf` and
  `wc -l` on the files named. No code was changed or run for this entry. _Unverified:_ the line estimates for
  the deletions. Nobody has made the change.

## Finding: the DuckDB query path is already unreachable from the server

`sdk/` has no SQL and no dialect at all. The SDK would not change.

App queries from the live server never reach DuckDB. `DocumentReaderClientLive` (`platform/layers.ts:449`)
intercepts `kind: "query"` and runs it on read-only SQLite in the server process
(`platform/document_reader.ts:425`). Only `ping`, `ingest` and `analysis` go to the reader child. The child's
`query` case (`platform/reader.ts:157`) and the per-app DuckDB readers (`makeReader`, `ReaderLive` and
`makeReaderPool` in `platform/duckdb.ts`) are reached only by tests.

## Two different "rip out DuckDB" scopes

| Scope                                                                  | What goes                                                                                                                                                                                                                                                                                                                        | What stays                                                                       |
| ---------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------- |
| **A. App queries only** (DuckDB stays for the event log and analytics) | `Dialect` and `placeholder`; the DuckDB branch in `expression`; the `"app".` prefix; the `query` case in the child; app readers in `duckdb.ts` (most of its 510 lines, apart from `emitLifecycle` and `drainDuckDB`); the DuckDB halves of the cross-engine tests; the DuckDB rows of `explain`/`core/compile/GENERATED_SQL.txt` | Reader child, IPC, event-log ingest and analysis catalog (~3.3k lines), ADR 0004 |
| **B. All of DuckDB**                                                   | Everything in A, plus the event-log DuckDB file, `platform/eventlog/*`, `platform/analysis/catalog.ts`, and probably the reader child and much of `platform/ipc.ts`                                                                                                                                                              | Analytics would need a new home (SQLite tables, or plain JSONL files)            |

The compiler gains the same from A or B. The size of B comes from the event-log and analytics features, not
from compiling SQL.

## Compiler diff under either scope

- `core/compile/dialect.ts`: drop `Dialect` and `placeholder`. Every placeholder is `?`.
- `expression` (`select.ts:104`) shrinks to:

  ```ts
  const node = yield * scalar(table, name);
  if (table.storage === "columns" || Object.hasOwn(system, name)) return yield * quote(name);
  const extract = `json_extract("doc", ${literal(`$.${yield * identifier(name)}`)})`;
  return node.kind === "number" ? `CAST(${extract} AS REAL)` : extract;
  ```

  The BIGINT, DOUBLE and BOOLEAN→INTEGER casts go away.
- `compileSelect`: `bind` becomes `values.push(v); return "?"`. The target is always the quoted table name.
  `compileSelect(schema, plan)` loses its third argument, and so does `expression` in `ddl.ts`, `sqlite.ts`,
  `document_reader.ts` and `explain.ts`.
- Optional: `NULLS FIRST` and `NULLS LAST` are SQLite's defaults already (NULL sorts lowest), so they could
  go. Keeping them costs nothing and keeps the intent explicit.

That is about 20–30 lines out of the 624 in `core/plan.ts` + `core/compile/`. The rest of the compiler (limit
rule, schema validation, identifier checks, null handling, ordering tie-breaker, count/countBy, `json` layout,
decode) is SQLite work and stays.

## What actually gets simpler

- **Tests.** The cross-engine tests (`tests/unit/number_dialects_test.ts`,
  `tests/property/json_number_test.ts`, `tests/unit/reader_test.ts`) run separate child processes because
  DuckDB's SQLite extension must never share a process with the writer's SQLite. The SQLite-only versions are
  single-process. The dialect dimension in `tests/property/compiler_test.ts`, `compiler_arbitraries.ts` and
  `tests/unit/compile_test.ts` goes too.
- **One set of semantics.** No parity work between two engines' number, boolean and JSON behaviour (plan item
  30's boundary matrix covered both).
- **Scope B only:** no second SQLite library anywhere, so the "two SQLites in one address space" rule (ADR
  0001 decision 5, ADR 0004) has nothing left to protect against.

## Recommendation recorded in the thread

Scope A is low risk: it deletes code the server cannot reach. Scope B is a product decision about where
event-log analytics live, and it needs its own ADR that supersedes 0004.
