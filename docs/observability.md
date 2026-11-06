# Observability: the event log and how to query it

Every process in mansio writes structured events as JSON lines. A separate step ingests those lines into an
append-only DuckDB file, and a fixed catalog of named, parameterized, read-only queries answers the usual
questions about it. This page is the reference for all three: where the files are, what the tables hold, what
each query answers, how retention works, and worked examples.

> **Since IR-6 (ADR 0013):** the writer of the event log is the Go agent (agentd's `event_log` config: `dir`,
> `level`, `trace_sql`; records byte-identical to `core/events.ts`, role `server`). The Deno server, its
> reader child process and the `/admin/logs/*` routes are gone; ingest, retention and the named catalog run
> only in the offline CLI (`deno task logs:*`, `scripts/logs.ts` over `eventlog/`), whose own records use the
> `reader` role. Where this page names the Deno platform's modules, the code now lives in `eventlog/` under
> the same file names (`ingest.ts`, `tables.ts`, `retention.ts`, `logger.ts`, `catalog.ts`), and its
> configuration is `eventlog/config.ts`. The data model, tables, catalog and retention rules below are
> unchanged.

Plan references: `docs/plans/effect-port-and-observability.md` §4 (data model) and items 21–24, 35, 36.
Decisions: ADR 0004 (DuckDB only in the reader), ADR 0006 (event identity, schema v2), ADR 0007 (ingest before
retention).

```text
server ─┐                                   ┌─> logs:query <name>   (read-only catalog)
        ├─> logs/<role>/<date>/*.jsonl ─ingest─> DuckDB event log ─┤
reader ─┘     (source of truth)                  (derived, rebuildable)  └─> logs:sweep (gzip/delete JSONL)
```

## Log directory layout

`LOG_DIR` (default `./logs`):

```text
logs/
  server/2026-10-10/events-14-31887-001.jsonl       # <role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl
  reader/2026-10-10/events-14-31902-001.jsonl
  server/2026-10-03/events-09-31010-001.jsonl.gz    # gzipped by the sweep after LOG_GZIP_AFTER_DAYS
  server/2026-10-09/rejected.jsonl                  # lines ingest could not accept, kept as evidence
  k6/<run-id>/{raw.json,summary.json}               # k6 output; loaded into k6_points by perf:ingest
```

- One file per role, process (`pid`), hour and size roll (`LOG_MAX_FILE_BYTES`, default 64 MiB; `<seq>` counts
  rolls). Files roll on date change, hour change, or size.
- The JSONL tree is the **source of truth**. The DuckDB file is derived from it and can be rebuilt by
  re-ingesting.
- The DuckDB event log is **not** under `LOG_DIR`: it is `EVENTLOG_DUCKDB`, default
  `<DATA_DIR>/eventlog.duckdb`. (The sweep never touches anything under `LOG_DIR` except event files and
  `rejected.jsonl` in the dated directories.)

JSONL records use camelCase envelope fields (`eventId`, `traceId`, `spanId`, `appId`, `kind`, …); the DuckDB
tables use snake_case. See `core/events.ts` for the record shapes.

## Tables

The schema is **v2** (`core/eventlog/ddl.ts`, ADR 0006). Event tables are append-only: ingest only ever
`INSERT`s into them. `_ingest_state` is the single mutable table. Every typed event table carries `event_id`,
and a record of that kind lands in both `platform_events` (under its `event_kind`) and its typed table under
the **same** `event_id`.

Not every table has every correlation column: `queue_events`, `backup_events` and `lifecycle_events` have no
`trace_id`, and `queue_events` has no `app_id`. Queries that need those columns can only use the tables that
carry them.

### `platform_events`

Every record of every kind, one row each. The place for cross-kind questions and the only table with span
identity.

| Column           | Type      | Meaning                                                                                      |
| ---------------- | --------- | -------------------------------------------------------------------------------------------- |
| `event_id`       | VARCHAR   | Unique id of the record (ADR 0006). Shared with its typed-table row.                         |
| `ts`             | TIMESTAMP | When the event was emitted, UTC.                                                             |
| `level`          | VARCHAR   | `debug`, `info`, `warn` or `error`.                                                          |
| `role`           | VARCHAR   | The process that wrote it: `server` or `reader`.                                             |
| `pid`            | INTEGER   | That process's pid.                                                                          |
| `trace_id`       | VARCHAR   | The request's trace id (the caller's `X-Trace-Id`, or generated). Spans both processes.      |
| `span_id`        | VARCHAR   | This event's span.                                                                           |
| `parent_span_id` | VARCHAR   | The enclosing span; a reader request's span is a child of the server handler's.              |
| `app_id`         | VARCHAR   | The app the event belongs to, when it has one.                                               |
| `event_kind`     | VARCHAR   | The record's kind: `http`, `handler`, `sql`, `tx`, `queue`, `backup`, `lifecycle`, `ingest`. |
| `handler_kind`   | VARCHAR   | For `handler` (and `http`) records: `query` (reader path) or `mutation` (writer path).       |
| `name`           | VARCHAR   | The handler name, for handler and http records.                                              |
| `outcome`        | VARCHAR   | `ok` / `error`, or the transaction outcome for `tx` records.                                 |
| `error_tag`      | VARCHAR   | The failure's `_tag` (see `core/errors.ts`).                                                 |
| `duration_ms`    | DOUBLE    | How long the operation took.                                                                 |
| `attrs`          | JSON      | The kind-specific remainder of the record.                                                   |
| `source_file`    | VARCHAR   | JSONL file the record came from, relative to `LOG_DIR`.                                      |
| `source_offset`  | BIGINT    | Byte offset of the record's line in that file.                                               |

### `sql_events`

One row per SQL statement run by either engine.

| Column           | Type      | Meaning                                                                                                                  |
| ---------------- | --------- | ------------------------------------------------------------------------------------------------------------------------ |
| `event_id`       | VARCHAR   | Record id.                                                                                                               |
| `ts`             | TIMESTAMP | Emission time, UTC.                                                                                                      |
| `trace_id`       | VARCHAR   | The request's trace.                                                                                                     |
| `app_id`         | VARCHAR   | Owning app.                                                                                                              |
| `engine`         | VARCHAR   | `sqlite` (writer) or `duckdb` (reader).                                                                                  |
| `dialect`        | VARCHAR   | The SQL dialect the text was compiled for.                                                                               |
| `statement_kind` | VARCHAR   | `select`, `insert`, `update`, `delete`, `ddl` or `pragma`.                                                               |
| `table_name`     | VARCHAR   | The table the statement targets, when it has one.                                                                        |
| `sql_text`       | VARCHAR   | The compiled statement, with `?`/`$n` placeholders. Bind values are never logged unless `LOG_INCLUDE_BIND_VALUES` is on. |
| `bind_count`     | INTEGER   | Number of bound values.                                                                                                  |
| `duration_ms`    | DOUBLE    | Execution time.                                                                                                          |
| `row_count`      | BIGINT    | Rows returned or changed.                                                                                                |
| `outcome`        | VARCHAR   | `ok` or `error`.                                                                                                         |
| `error_tag`      | VARCHAR   | Failure tag when `outcome = 'error'`.                                                                                    |
| `path`           | VARCHAR   | SQLite execution path: `blocking` or `nonblocking`; NULL in older logs.                                                  |

### `tx_events`

One row per writer transaction.

| Column          | Type      | Meaning                                                   |
| --------------- | --------- | --------------------------------------------------------- |
| `event_id`      | VARCHAR   | Record id.                                                |
| `ts`            | TIMESTAMP | Emission time, UTC.                                       |
| `trace_id`      | VARCHAR   | The request's trace.                                      |
| `app_id`        | VARCHAR   | Owning app.                                               |
| `engine`        | VARCHAR   | `sqlite` or `duckdb`.                                     |
| `outcome`       | VARCHAR   | `commit`, `rollback` or `interrupted`.                    |
| `operations`    | INTEGER   | Statements in the transaction.                            |
| `duration_ms`   | DOUBLE    | Time from begin to outcome.                               |
| `queue_wait_ms` | DOUBLE    | Time spent waiting in the writer queue before it started. |
| `error_tag`     | VARCHAR   | Failure tag for a rollback or interruption.               |

### `http_events`

One row per HTTP request the server answered.

| Column         | Type      | Meaning                                                                 |
| -------------- | --------- | ----------------------------------------------------------------------- |
| `event_id`     | VARCHAR   | Record id.                                                              |
| `ts`           | TIMESTAMP | Emission time, UTC.                                                     |
| `trace_id`     | VARCHAR   | The request's trace (echoed to the client as `X-Trace-Id`).             |
| `method`       | VARCHAR   | HTTP method.                                                            |
| `path`         | VARCHAR   | Request path.                                                           |
| `app_id`       | VARCHAR   | The app the route resolved to, when it did.                             |
| `handler_kind` | VARCHAR   | `query` or `mutation`.                                                  |
| `name`         | VARCHAR   | The handler name.                                                       |
| `status`       | INTEGER   | HTTP status. There is no `outcome` column: `status >= 400` is an error. |
| `duration_ms`  | DOUBLE    | Server-side request time.                                               |
| `bytes_in`     | BIGINT    | Request body bytes.                                                     |
| `bytes_out`    | BIGINT    | Response body bytes.                                                    |
| `error_tag`    | VARCHAR   | Failure tag of a failed request.                                        |

### `queue_events`

A queue's state when something notable happened. No `trace_id`, no `app_id`.

| Column     | Type      | Meaning                                                                     |
| ---------- | --------- | --------------------------------------------------------------------------- |
| `event_id` | VARCHAR   | Record id.                                                                  |
| `ts`       | TIMESTAMP | Emission time, UTC.                                                         |
| `queue`    | VARCHAR   | Which queue (the writer queue, a reader queue, the logger's buffer).        |
| `depth`    | INTEGER   | Queue depth when the event fired.                                           |
| `cap`      | INTEGER   | The queue's capacity.                                                       |
| `event`    | VARCHAR   | `high-water` (new maximum), `rejected` (full, caller refused) or `dropped`. |
| `dropped`  | BIGINT    | Records dropped, for `event = 'dropped'`.                                   |

### `backup_events`

One row per backup phase (`emitter = 'backups'`) or per object transfer (`emitter = 'object-store'`). The two
never describe the same thing. No `trace_id`.

| Column        | Type      | Meaning                                                                           |
| ------------- | --------- | --------------------------------------------------------------------------------- |
| `event_id`    | VARCHAR   | Record id.                                                                        |
| `ts`          | TIMESTAMP | Emission time, UTC.                                                               |
| `app_id`      | VARCHAR   | The app being backed up.                                                          |
| `phase`       | VARCHAR   | `snapshot`, `verify`, `hash`, `put-object`, `put-manifest` or `publish-latest`.   |
| `emitter`     | VARCHAR   | `backups` (run orchestration) or `object-store` (one PUT).                        |
| `run_id`      | VARCHAR   | The backup run; links phases and transfers. Absent on a transfer outside any run. |
| `key`         | VARCHAR   | The object key involved.                                                          |
| `bytes`       | BIGINT    | Object size.                                                                      |
| `sha256`      | VARCHAR   | Object digest.                                                                    |
| `attempts`    | INTEGER   | `object-store` only: requests sent (1 + retries).                                 |
| `duration_ms` | DOUBLE    | Phase or transfer time.                                                           |
| `outcome`     | VARCHAR   | `ok` or `error`.                                                                  |
| `error_tag`   | VARCHAR   | Failure tag.                                                                      |

### `lifecycle_events`

Process and subsystem state changes: startup, shutdown, reader spawn, event-log open/close, retention sweeps.
No `trace_id`.

| Column     | Type      | Meaning                                                       |
| ---------- | --------- | ------------------------------------------------------------- |
| `event_id` | VARCHAR   | Record id.                                                    |
| `ts`       | TIMESTAMP | Emission time, UTC.                                           |
| `role`     | VARCHAR   | `server` or `reader`.                                         |
| `pid`      | INTEGER   | Process id.                                                   |
| `phase`    | VARCHAR   | What happened (`startup`, `shutdown`, `retention-sweep`, …).  |
| `detail`   | JSON      | Phase-specific payload (what a sweep gzipped and deleted, …). |

### `k6_points`

One row per k6 data point. Filled by `deno task perf:ingest` from `logs/k6/<run-id>/raw.json`, **not** by
event-log ingest, and not a projection of an event record (so it has no `event_id`). Empty until a k6 run has
been loaded.

| Column     | Type      | Meaning                                                |
| ---------- | --------- | ------------------------------------------------------ |
| `ts`       | TIMESTAMP | The point's time, UTC.                                 |
| `run_id`   | VARCHAR   | The k6 run id (`logs/k6/<run-id>`).                    |
| `metric`   | VARCHAR   | k6 metric name, e.g. `http_req_duration`.              |
| `value`    | DOUBLE    | The point's value (milliseconds for duration metrics). |
| `scenario` | VARCHAR   | k6 scenario name.                                      |
| `status`   | VARCHAR   | HTTP status tag.                                       |
| `trace_id` | VARCHAR   | The `X-Trace-Id` the VU sent, when tagged.             |
| `tags`     | JSON      | All k6 tags.                                           |

### `_ingest_state`

The ingest watermark: one row per JSONL source file. Retention reads it as its ledger.

| Column             | Type      | Meaning                                                                          |
| ------------------ | --------- | -------------------------------------------------------------------------------- |
| `source_file`      | VARCHAR   | Path relative to `LOG_DIR`. Primary key.                                         |
| `bytes_ingested`   | BIGINT    | Bytes of the file ingested; always ends just past a `\n`.                        |
| `prefix_sha256`    | VARCHAR   | SHA-256 of exactly those bytes. A mismatch means the file was rewritten.         |
| `last_ingested_at` | TIMESTAMP | When this row was last advanced, UTC.                                            |
| `dedupe`           | BOOLEAN   | Set once a rewrite was detected: every later line is deduplicated on `event_id`. |

### `_eventlog_schema`

Which schema version the file holds, one row per way it got there.

| Column       | Type      | Meaning                                     |
| ------------ | --------- | ------------------------------------------- |
| `version`    | INTEGER   | Schema version (2 for this build).          |
| `applied_at` | TIMESTAMP | When it was applied, UTC.                   |
| `method`     | VARCHAR   | `create`, or `rebuild-from-v1`.             |
| `detail`     | JSON      | The rebuild's evidence, when there was one. |

A v2 build refuses a v1 (unversioned) file rather than reuse its tables; `upgradeEventLog`
(`platform/eventlog/tables.ts`) rebuilds it from the retained JSONL.

## Running things

```sh
export PATH="$HOME/.deno/bin:$PATH"
deno task logs:ingest                 # JSONL -> DuckDB, idempotent
deno task logs:list                   # the catalog's names and questions
deno task logs:query <name> [--param k=v]... [--format table|json|csv]
deno task logs:sweep [--dry-run]      # ingest, seal torn tails, then retention
```

- `logs:query` opens the event-log file **read-only**. DuckDB allows one writer: while the reader process or a
  `logs:ingest` holds the file, the query fails with an `AnalysisError` that quotes the lock error. Run
  queries between ingests, or ask the running server (`POST /admin/logs/analysis/:name`).
- `--param` is repeatable and every value is bound as a typed parameter; there is no way to supply SQL. An
  unknown parameter, or a missing required one, is an `AnalysisError` naming the accepted parameters.
- An unknown query name exits **2** with `UnknownAnalysisQuery` and the list of valid names. Any other failure
  exits 1.
- Parameters shared by most queries: `since` (inclusive) and `until` (exclusive), ISO-8601 timestamps (UTC
  when no offset is given); `appId`; `limit`.

## The Go backend (agentd)

agentd writes the same event log (IR-4c, `server/internal/eventlog`): the same envelope, kinds, key order and
redaction as `core/events.ts` (its tests compare lines byte for byte with `toJsonLine`), under
`<event_log.dir>/server/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl` with role `server`. DuckDB stays out of Go
(ADR 0004): ingest and analysis are the offline CLI above, run with `LOG_DIR` set to `event_log.dir`. There is
no `/admin/logs/*` on agentd.

```json
"event_log": { "dir": "/var/log/mansio", "level": "info", "trace_sql": false }
```

```sh
LOG_DIR=/var/log/mansio EVENTLOG_DUCKDB=/var/lib/mansio/eventlog.duckdb deno task logs:ingest
LOG_DIR=/var/log/mansio EVENTLOG_DUCKDB=/var/lib/mansio/eventlog.duckdb deno task logs:query trace --param traceId=…
```

Per `/v1/ops` request, under the request's trace id (`X-Trace-Id` if it is a valid token, else a generated
one; echoed on the response):

| Kind      | When                      | Notes                                                                                  |
| --------- | ------------------------- | -------------------------------------------------------------------------------------- |
| `http`    | every answer              | `name` is the SDK's `X-Mansio-Handler` (else `v1.ops`); `handlerKind` the request kind |
| `handler` | the body decoded          | the pipeline's time: the server's half of one SDK handler run (one read or the commit) |
| `tx`      | the transaction ran       | `commit`, `rollback` (an error answer) or `interrupted` (statement timeout, deadline)  |
| `sql`     | `trace_sql`, or a failure | `engine`/`dialect` `sqlite`, `path` null, bind values never                            |

Plus `lifecycle` `startup` and `shutdown`, and the queue record `queue: "events", event: "dropped"` when the
emitter's bounded queue was full: `Emit` never blocks a request; a drop is counted
(`mansio_eventlog_records_total{result="dropped"}`) and reported before the next batch. Differences from the
Deno server: the SDK runs handlers in the client, so an app error (`makeMove`'s `ConflictError`, a
`RuleError`) never reaches agentd and is not in its log; there is one `handler` record per request, not per
handler run; no `reader` role (agentd has no reader process); a backup is one `backup` record
(`publish-latest`, emitter `backups`) per durable-mode `POST /admin/backup`, with no per-phase records. The
`errorDetail` of a failed request is the error body the client received (the cause stays in agentd's
operational log, with its log id).

## Named queries

The catalog lives in `platform/analysis/catalog.ts` as a plain record of `{ sql, params, description }`. It is
**extensible**: a new query is one more entry (the tenant-aware `error-rate-by-tenant`, for example, will be
added that way). This section and the catalog are kept in step by `tests/unit/analysis_catalog_test.ts`, which
fails if a name or parameter is documented here and absent there, or the reverse. Every entry is a single
`SELECT`/`WITH`, also asserted by that test.

### `deadline-hits-per-handler-hour`

**Question:** how many statements hit their deadline per app, handler and UTC hour? Returns `hour`, `app_id`,
`handler_kind`, `name`, `path` and `deadline_hits`. Joins deadline-tagged `sql_events` to the nearest
completing request by trace, app and request time window (one match per SQL event, including millisecond
timestamp rounding at both window boundaries). Parameters: `since`, `until`, `appId` (all optional). Failed
statements are logged even without `TRACE_SQL` (`deno task integration` runs a second, short pass with
`TRACE_SQL=false` to check it); execution paths in older JSONL are NULL. Ingest adds the nullable `path`
column to existing v2 event logs.

**Upgrade note:** an event log ingested before FFI-06 has no `sql_events.path` column until the next ingest,
and until then this query (only this one) fails with a DuckDB binder error
(`Table "s" does not have a column named
"path"`). Run `deno task logs:ingest` once after upgrading (the
server's periodic ingest sweep or `POST /admin/logs/ingest` heals it the same way). The query is unchanged
because no `path`-free form can still report the execution path.

**Attribution limits** (pinned in `tests/unit/analysis_catalog_test.ts`): the match uses only trace, app and
time. If two requests share a trace and overlap in time, the earliest-completing request gets the hits; if a
trace id is reused back to back, a deadline event in the 1 ms after the earlier request ends goes to that
earlier request even when the later one has already started. An event with no matching request (outside the
window, or in another app) is still counted, with NULL `handler_kind` and `name`.

### `slowest-sql`

**Question:** what are the p50/p95/p99 `duration_ms` by engine, statement kind and table? Ordered by p95,
slowest first. Also counts calls and errors. Source: `sql_events`, which the server only writes when
`TRACE_SQL=true` (or `LOG_LEVEL=debug`); without it this query returns no rows. Parameters: `since`, `until`,
`appId`, `limit` (default 100).

### `plan-shapes`

**Question:** which distinct compiled SQL texts actually run, how often, and how fast — and so whether the
indexes the schema declares are helping? One row per `(engine, statement_kind, table, sql_text)` with calls,
average/p95/max latency and row counts. A shape with a high `avg_rows` and high latency is a candidate for an
index; a shape that never appears is a plan the app never uses. Source: `sql_events`, so it needs
`TRACE_SQL=true` like `slowest-sql`. Parameters: `since`, `until`, `appId`, `limit` (default 50).

### `error-rate-by-handler`

**Question:** per app and handler, how many requests were there, what fraction failed, and which error tags
made up the failures? A request failed when `status >= 400` or it has an `error_tag`. `error_tags` reads like
`MutationError:3, SchemaError:1`. Source: `http_events`. Parameters: `since`, `until`, `appId`, `limit`
(default 100).

### `mutation-vs-query-latency`

**Question:** how do the writer path (`handler_kind = 'mutation'`) and the reader path (`'query'`) compare in
latency over time? One row per time bucket and kind with p50/p95/p99. Source: `platform_events` where
`event_kind = 'handler'`. Parameters: `since`, `until`, `appId`, `bucketSeconds` (default 60), `limit`
(default 500).

### `handler-throughput`

**Question:** how many requests per minute does each handler serve, and what is the status breakdown
(2xx/3xx/4xx/5xx)? Source: `http_events`. Parameters: `since`, `until`, `appId`, `limit` (default 500).

### `queue-depth`

**Question:** how deep did the writer and reader queues get, and were requests rejected or log records
dropped? Per bucket and queue: maximum depth, capacity, counts of `high-water`, `rejected` and `dropped`
events, and records dropped. Source: `queue_events` (no `app_id`, so no `appId` parameter). Parameters:
`since`, `until`, `bucketSeconds` (default 60), `limit` (default 500).

### `tx-outcomes`

**Question:** how many transactions committed, rolled back or were interrupted, and how long did they wait in
the writer queue? Per app, engine and outcome. Source: `tx_events`. Parameters: `since`, `until`, `appId`,
`limit` (default 100).

### `backup-history`

**Question:** per app, which backup phases ran, how long did each take, which failed, and how long was it
between successful backups? Lists the orchestration phases (`emitter = 'backups'`); a run succeeded when its
`publish-latest` phase did, and that row carries `gap_since_previous_success`. Source: `backup_events`.
Parameters: `since`, `until`, `appId`, `limit` (default 200).

### `trace`

**Question:** what is the complete, ordered event timeline for one `trace_id`, across both processes? Rows
from `platform_events`, `sql_events`, `tx_events`, `http_events` and `k6_points` that share the trace id,
ordered by `ts`; a `source` column says which table each came from and `role` says which process wrote
platform events. Typed events (`http`, `sql`, `tx`) are listed once, from their typed table, not twice.
`queue_events`, `backup_events` and `lifecycle_events` have no `trace_id` and so are not part of a trace.
Parameters: `traceId` (**required**), `limit` (default 1000).

### `k6-vs-platform`

**Question:** how does the k6 client's p95 `http_req_duration` compare to the server's handler p95 per
10-second bucket per scenario? The difference is transport and queueing outside the handler. Source:
`k6_points` joined to `http_events`. Returns no rows until `deno task perf:ingest` has loaded a k6 run.
Parameters: `runId`, `limit` (default 500).

## Retention

Retention applies to the **JSONL tree**. It is **ingest-gated**: a source file is gzipped or deleted only
after a _committed_ ingest watermark (`_ingest_state`) covers the file's full tail, with a matching hash. A
file that is open, unsettled, changed since it was inventoried, or not fully accounted for is pinned: kept,
still counted toward the size cap, and reported as `blocked`.

Defaults (environment variables):

| Setting               | Default            | Meaning                                                |
| --------------------- | ------------------ | ------------------------------------------------------ |
| `LOG_GZIP_AFTER_DAYS` | 7                  | Gzip a closed, ingested file after this many days.     |
| `LOG_RETENTION_DAYS`  | 30                 | Delete a closed, ingested file after this many days.   |
| `LOG_MAX_TOTAL_BYTES` | 1073741824 (1 GiB) | Cap on the retained JSONL; oldest safe files go first. |
| `LOG_MAX_FILE_BYTES`  | 67108864 (64 MiB)  | Roll to a new file past this size.                     |

Rules that always hold: today's directory (and anything dated later) and any file a sink holds open are never
touched; every path must resolve strictly inside `LOG_DIR` with the shape
`<role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl[.gz]`; gzip is written to a temp file, verified by
decompress-and-hash, then renamed before the source is removed. A gzipped file stays ingestible: it
decompresses to exactly the bytes ingest committed, and its `_ingest_state` row is kept.

### Ordering: `ingestThenSweep`

`ingestThenSweep` in `platform/eventlog/ingest.ts` is the coordinator that enforces the gate:

1. **Ingest** commits first.
2. **Torn tails are sealed** (a closed file's last, unterminated line is moved to quarantine so the file can
   be accounted for).
3. **The sweep plans** from the committed `_ingest_state` rows as its ledger, then applies.

An ingest failure fails the whole call before anything is swept. A sweep failure is reported in the result
(`retention.status = "failed"`), not as an ingest error.

`deno task logs:sweep` runs `ingestThenSweep()`. `deno task logs:sweep --dry-run` runs
`ingestThenSweep({ dryRun: true })`: it plans the sweep from the **already-committed** state and reports it,
changing nothing — no ingest, no sealing, no file touched (and, when it succeeds, no event emitted). The
reader process runs the same coordinator when `POST /admin/logs/ingest` is called.

### The DuckDB file is not pruned

Retention does not prune the DuckDB event log. The file **grows with every ingested event** until an explicit
future `logs:compact` command exists (out of scope for this phase). Its size is reported next to the JSONL
budget (`budgets.derived.duckdbBytes`) but is not bounded by `LOG_MAX_TOTAL_BYTES`. Because the JSONL is the
source of truth, deleting the file and re-ingesting rebuilds it from whatever JSONL is still retained.

## Worked examples

Generate load and ingest it:

```sh
TRACE_SQL=true bash work-scripts/bash/k6-scenario-run.sh perf:mixed my-run   # throwaway server, k6 -> logs/k6/my-run
deno task logs:ingest
deno task logs:list
```

### "Show me everything that happened for trace id X"

Send a request with your own trace id (or take one from a response's `X-Trace-Id` header or a log line),
ingest, then ask for the trace:

```sh
curl -s -X POST "$BASE_URL/apps/tic-tac-toe/mutation/createGame" \
  -H "authorization: Bearer dev-only" -H 'x-trace-id: demo-trace-1' \
  -d '{"xPlayer":"a","oPlayer":"b"}'
deno task logs:ingest
deno task logs:query trace --param traceId=demo-trace-1
deno task logs:query trace --param traceId=demo-trace-1 --format json | jq '.[] | {ts, source, role, kind, name, outcome}'
```

The result is the whole request in time order: the server's `http` record and handler, the SQLite `sql` and
`tx` events of a mutation, then — for a query — the reader process's own events with `role = reader`, whose
`parent_span_id` is the server handler's `span_id`. A rolled-back mutation shows `outcome = rollback` and its
`error_tag` on the `tx` row.

If it returns no rows, either the event has not been ingested yet (`logs:ingest`) or the id is wrong; ids
longer than 128 characters or containing unsafe characters are replaced at ingress, and the replacement is
what the response echoes.

### Which handlers fail, and how?

```sh
deno task logs:query error-rate-by-handler --param limit=10
```

### Is the writer path slower than the reader path over the last hour?

```sh
deno task logs:query mutation-vs-query-latency --param since=2026-10-10T13:00:00 --param bucketSeconds=300
```

### What did the last backup cost, and how long since the last success?

```sh
deno task logs:query backup-history --param appId=tic-tac-toe
```

### Preview what retention would do

```sh
deno task logs:sweep --dry-run     # plans from committed state; changes nothing
```

### CSV for a spreadsheet

```sh
deno task logs:query slowest-sql --format csv > slowest-sql.csv
```

## Starter SQL for the `duckdb` CLI

`scripts/duckdb/*.sql` are ad-hoc `duckdb`-CLI versions of a few of these queries, written against the same v2
columns (see `scripts/duckdb/README.md`). The catalog is the supported interface; those files are for
exploration.
