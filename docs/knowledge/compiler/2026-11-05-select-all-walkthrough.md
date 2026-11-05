# Select-all walkthrough: `db.find("games")` to SQL to process

- **Date:** 2026-11-05
- **Question:** How does a plain `SELECT * FROM <table>` request travel from app code to valid SQL, and which
  process runs it?
- **Read at:** `422ac91` (branch `docs/scale-out-and-testing`)
- **Evidence:** Steps 2–4 and 8 are real output from
  `deno run --config deno.json work-scripts/deno/compile-plan-probe.ts walk`. Steps 1 and 5–7 come from
  reading the cited lines.
- **Deeper:** steps 3 and 4 in detail:
  [2026-11-05-table-lookup-and-compile-select.md](2026-11-05-table-lookup-and-compile-select.md).

## Overview

```
app handler          server process (Deno)                                   reader child (DuckDB)
───────────          ──────────────────────────────────────────────────────  ─────────────────────
db.find("games") ─►  ① SelectPlan
  .collect()         ② normalise        (core/plan.ts)
                     ③ tableFor         (core/compile/select.ts)
                     ④ compileSelect ──► { text, values }  dialect = "sqlite"
                     ⑤ HTTP route: query → ReaderClient.call({kind:"query"})
                     ⑥ DocumentReaderClient intercepts "query" ──X──────────►  (only ping / ingest /
                     ⑦ read-only SQLite conn, BEGIN, prepare, run, ROLLBACK      analysis reach it)
                     ⑧ decode rows → documents
```

## Step 1: app code builds a plan, with no SQL yet

```ts
// a query handler (compare app/functions.ts:57)
handler: (({ db }) => db.find("games").collect());
```

`Documents.find` (`sdk/db.ts:107`) only builds an object. The default limit of 100 is applied here, so there
is no unbounded `SELECT *`:

```json
{ "kind": "select", "table": "games", "limit": 100, "filters": [] }
```

`.collect()` (`sdk/db.ts:73`) wraps `execute(plan)` in an `Effect`. Nothing runs until the runtime that
supplied `execute` runs it.

## Step 2: normalise checks the limit and drops order on aggregates

`core/plan.ts:57`. The limit is an integer in 1..1000. This plan has no aggregate, so it passes through
unchanged:

```
Right({ kind:"select", table:"games", limit:100, filters:[] })
```

A plan with `limit: 5000` would be `Left(CompileError{ reason:"invalid-limit", detail:"1..1000, got 5000" })`.

## Step 3: look up the table in the schema

`tableFor` (`core/compile/select.ts:31`). `games` is a `columns`-layout table (`app/schema.ts`), so each field
is a real SQLite column. A misspelled name fails before any SQL exists:

```
compileSelect(schema, {...plan, table:"gamez"}, "sqlite")
→ Left(CompileError{ reason:"unknown-collection", table:"gamez" })
```

## Step 4: compileSelect builds the statement

`core/compile/select.ts:149`, one piece at a time:

| Piece      | Rule                                               | Result                   |
| ---------- | -------------------------------------------------- | ------------------------ |
| WHERE      | `filters` is empty → no WHERE                      | (none)                   |
| target     | `quote("games")` checks `^[A-Za-z_][A-Za-z0-9_]*$` | `"games"`                |
| projection | no aggregate → `*`                                 | `SELECT *`               |
| ORDER BY   | no `order` → fixed fallback on the primary key     | `ORDER BY "_id" ASC`     |
| LIMIT      | always bound, never inlined                        | `LIMIT ?`, values: [100] |

Real output for each dialect:

```
sqlite → { text: 'SELECT * FROM "games" ORDER BY "_id" ASC LIMIT ?',          values: [100] }
duckdb → { text: 'SELECT * FROM "app"."games" ORDER BY "_id" ASC LIMIT $p1',  values: [100] }
```

The DuckDB form reads the SQLite file through `ATTACH … AS "app" (READ_ONLY)` (`platform/duckdb.ts`). It uses
named `$pN` placeholders. For `json`-layout tables or filtered fields the two dialects also differ in how they
read a field (`expression`, `select.ts:104`), but a select-all has none of that.

## Step 5: HTTP routes the request by handler kind

`POST /apps/:appId/query/listGames` → `platform/http/router.ts:68` → `invoke`
(`platform/http/handlers.ts:147`):

```
kind === "mutation"  → stack.storage.mutation(...)         SQLite writer, server process
kind === "query"     → ReaderClient.call({ kind:"query" }) (+ tenant file when tenancy is on)
```

## Step 6: the query stays in the server process

`ReaderClient` sounds like the DuckDB child, but `makePlatformLive` (`platform/layers.ts:449`) wraps it in
`DocumentReaderClientLive`. That wrapper intercepts `kind: "query"` (`platform/document_reader.ts:425`) and
passes only `ping`, `ingest` and `analysis` on to the child over IPC:

```
call(request):
  request.kind !== "query"  → ipc.call(request)       → DuckDB reader child
  request.kind === "query"  → querySqlite(app, ...)   → this process
```

**Correction to the thread:** an earlier answer in the same session said HTTP queries run on DuckDB. They
don't. The DuckDB path (`platform/reader.ts:157` → `duckdb.ts:297`) still exists, but app queries from the
live server don't reach it.

## Step 7: run it on a read-only SQLite connection

`querySqlite` (`platform/document_reader.ts:144`). Per handler call:

```
open read-only conn (FFI, readBigInts) ─► BEGIN TRANSACTION
  handler runs: db = new Documents(executeIn(ctx))        (:363)
    executeIn: refuse writes (QueryWriteAttemptError), refuse after ctx expires
    select: compileSelect(schema, plan, "sqlite")         (:269)  ← Step 4
            classify(plan): no _id lookup → "unbounded"
            → allAsync on Deno's blocking pool, under statementDeadlineMs (504 on timeout)
  ROLLBACK / end ─► sql + tx events (when TRACE_SQL is on, or on failure)
```

A primary-key `db.get` would be `"bounded"` and run on the main thread instead
(`platform/sqlite_ffi/classify.ts:42`).

## Step 8: decode rows back into documents

`decode` (`core/compile/decode.ts:66`). Input is a raw SQLite row, output is the document after a schema
re-check. Real output:

```
row in:  { _id:"g1", _creationTime:1730800000000n, _version:1n, board:'[null,…,"X",…]',
           turn:"O", status:"playing", winner:null, xPlayer:"alice", oPlayer:"bob" }
doc out: { xPlayer:"alice", oPlayer:"bob", board:[null,null,null,null,"X",null,null,null,null],
           turn:"O", status:"playing", winner:null, _id:"g1", _creationTime:1730800000000, _version:1 }
```

- bigint becomes a safe number. Anything outside the safe-integer range is a `DecodeError`, not a lossy
  number.
- the array column is JSON-parsed. Corrupt JSON is a `DecodeError`, not a thrown `SyntaxError`.
- the body is checked against the schema again (`parseObject`), so a row that no longer matches the schema
  fails loudly.

The documents go back up the `Effect`, out through the HTTP response.
