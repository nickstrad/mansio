# DuckDB analysis queries (starter set)

Starter `.sql` files for exploring mansio's event logs with the `duckdb` CLI, written against the **v2** event
log schema (`core/eventlog/ddl.ts`). The event log is the JSONL that agentd writes when its config has an
`event_log` section (`<dir>/server/<date>/events-*.jsonl`). These are ad-hoc reference queries. The supported
interface is the named query catalog — `eventlog/catalog.ts` via `deno task logs:query <name>` — documented in
`docs/observability.md`; the files here overlap it where noted and otherwise stay free-form.

## Status

- **Schema:** the DuckDB event log is populated by `deno task logs:ingest` (with `LOG_DIR` set to agentd's
  `event_log.dir`) into `<DATA_DIR>/eventlog.duckdb` (`EVENTLOG_DUCKDB`), so `duckdb <that file>` is where
  these run. The queries were last checked against a v2 file through DuckDB's Node API (all but
  `log-level-distribution.sql`, which reads raw JSONL).
- **`duckdb` CLI:** not installed on this machine (`which duckdb` finds nothing). To get it:
  `curl -fsSL https://install.duckdb.org | sh`, then `export PATH="$HOME/.duckdb/cli/latest:$PATH"`.
- **v2 columns:** `platform_events.kind` is now `event_kind` (the record's kind) plus `handler_kind`
  (`query`/`mutation`); `http_events.kind` is `handler_kind`; `backup_events` gained `emitter`, `run_id` and
  `attempts`; every typed table has `event_id`.

## Schema notes and assumptions

- Table/column names are snake_case, as in `core/eventlog/ddl.ts`. The JSONL log lines (read directly by
  `log-level-distribution.sql`) use the **camelCase** envelope fields from `core/events.ts`'s `toJsonLine`
  (`eventId`, `traceId`, `spanId`, `appId`, `kind`, …). Don't mix the two conventions across queries.
- `platform_events.attrs` is the record's kind-specific JSON payload, including `errorDetail`.
- `backup_events`, `lifecycle_events` and `queue_events` carry no `trace_id`, so `trace-timeline.sql` only
  unions the five tables that do (`platform_events`, `sql_events`, `tx_events`, `http_events`, `k6_points`).
  Typed events appear in both `platform_events` and their typed table, so that file lists them twice; the
  catalog's `trace` query lists them once.
- agentd writes a `sql` record per statement only with `event_log.trace_sql: true` (failed statements are
  always recorded); without it `slowest-sql.sql` returns little or nothing.
- `BackupPhase` (`core/errors.ts`) is `snapshot | verify | hash | put-object | put-manifest | publish-latest`.
  `backup-durations.sql` keeps only `emitter = 'backups'` rows (the phases) and treats a successful
  `publish-latest` as "backup completed". agentd records one `publish-latest` row (emitter `backups`) per
  `POST /admin/backup`, so its backups have no per-phase rows.

## Running a query

Query files use DuckDB CLI **named parameters** (`$start_ts`, `$end_ts`, `$trace_id`, `$limit`, `$log_glob`),
set with the shell's `.parameter set` dot-command before piping the file in:

```sh
duckdb "$EVENTLOG_DUCKDB" \
  -c ".parameter set start_ts '2026-10-01 00:00:00'" \
  -c ".parameter set end_ts   '2026-10-02 00:00:00'" \
  < scripts/duckdb/slowest-sql.sql
```

For `log-level-distribution.sql` (no DuckDB file needed — reads JSONL directly, any in-memory or on-disk
DuckDB connection works):

```sh
duckdb \
  -c ".parameter set log_glob 'logs/server/2026-10-10/events-*.jsonl'" \
  < scripts/duckdb/log-level-distribution.sql
```

Widen `log_glob` to cover every role/date, e.g. `'logs/*/*/events-*.jsonl'`.

## Query index

| File                         | Question it answers                                                                                          | Reads                                                                    | Variables              |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------ | ---------------------- |
| `event-counts-by-type.sql`   | Event counts by kind/name/outcome over a time window.                                                        | `platform_events`                                                        | `$start_ts`, `$end_ts` |
| `error-breakdown.sql`        | Errors grouped by `error_tag`, with counts and share of total errors/events.                                 | `platform_events`                                                        | `$start_ts`, `$end_ts` |
| `log-level-distribution.sql` | Distribution of log levels (`debug`/`info`/`warn`/`error`).                                                  | rotated JSONL files, via `read_json`                                     | `$log_glob`            |
| `slowest-sql.sql`            | p50/p95/p99 `duration_ms` by engine, statement kind and table. Mirrors the plan's named `slowest-sql` query. | `sql_events`                                                             | `$start_ts`, `$end_ts` |
| `backup-durations.sql`       | Per-app backup phases, durations, bytes, failures, and gaps between successful backups.                      | `backup_events`                                                          | `$start_ts`, `$end_ts` |
| `lifecycle-events.sql`       | Startup/shutdown (and other) phases per role/pid, with time between phases.                                  | `lifecycle_events`                                                       | `$start_ts`, `$end_ts` |
| `recent-errors.sql`          | Most recent errors with detail/attrs, newest first.                                                          | `platform_events`                                                        | `$limit`               |
| `trace-timeline.sql`         | Complete ordered event timeline for one `trace_id` across tables. Mirrors the plan's named `trace` query.    | `platform_events`, `sql_events`, `tx_events`, `http_events`, `k6_points` | `$trace_id`            |

All queries are `SELECT`/`WITH` only — none ever `INSERT`/`UPDATE`/`DELETE`, matching the plan's "only
`INSERT` is ever issued against the event tables" invariant (§4.1): these are read paths only.
