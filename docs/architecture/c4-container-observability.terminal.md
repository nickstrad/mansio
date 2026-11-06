# Container — observability path

Redrawn for IR-6 (ADR 0013) on 2026-11-06 on `ir/06-cutover` from main `7cf4ba6`: the Deno platform is gone
and the server is the Go server in `server/`. Both formats are generated from
`diagrams/c4-container-observability.py`; numbered relationships are identical. All boxes describe landed
code.

<!-- c4-render: diagrams/c4-container-observability.py -->

```text
╭╌ Mansio observability ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╮
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ Agent                        │   │ Structured event sources     │ ╎
╎  │ [Go process]                 │   │ [Filesystem]                 │ ╎
╎  │ server/cmd/agentd;           │   │ event_log.dir:               │ ╎
╎  │ internal/eventlog            │   │ role/date/events-*.jsonl     │ ╎
╎  │ event_log: http, handler,    │   │ agentd JSONL (role server);  │ ╎
╎  │ sql, tx, backup and          │   │ the CLI's own (role reader); │ ╎
╎  │ lifecycle records            │   │ quarantine beside source     │ ╎
╎  └──────────────────────────────┘   └──────────────────────────────┘ ╎
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ Derived event log            │   │ Logs CLI                     │ ╎
╎  │ [DuckDB file]                │   │ [Deno process]               │ ╎
╎  │ EVENTLOG_DUCKDB              │   │ scripts/logs.ts; eventlog/   │ ╎
╎  │ Schema v2; typed events      │   │ Ingest/list/query/sweep;     │ ╎
╎  │ (sql_events.path),           │   │ named catalog                │ ╎
╎  │ watermarks, k6_points        │   └──────────────────────────────┘ ╎
╎  └──────────────────────────────┘                                    ╎
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ k6 point loader              │   │ k6 artifacts                 │ ╎
╎  │ [Deno process]               │   │ [Filesystem]                 │ ╎
╎  │ perf/ingest_k6.ts            │   │ logs/k6/run-id/              │ ╎
╎  │ Loads raw points into        │   │ raw.json and summary.json    │ ╎
╎  │ k6_points                    │   └──────────────────────────────┘ ╎
╎  └──────────────────────────────┘                                    ╎
╎                                                                      ╎
╰╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╯

   ┌──────────────────────────────┐
   │ k6 load generator            │
   │ [External process]           │
   │ perf/*.js                    │
   │ IR load over /v1/ops; JSON   │
   │ metric output                │
   └──────────────────────────────┘
```

## Relationships

| # | From              | To                       | Relationship                                                                      |
| - | ----------------- | ------------------------ | --------------------------------------------------------------------------------- |
| 1 | Agent             | Structured event sources | Append JSONL in core/events.ts format through a dropping queue                    |
| 2 | Logs CLI          | Structured event sources | Offline ingest-then-sweep; seal tails, gzip/delete safe files; dry-run plans only |
| 3 | Logs CLI          | Derived event log        | Transactional ingest (adds sql_events.path if missing); read-only catalog SELECT  |
| 4 | k6 load generator | k6 artifacts             | Raw points and summary JSON                                                       |
| 5 | k6 artifacts      | k6 point loader          | Read raw.json for selected run id                                                 |
| 6 | k6 point loader   | Derived event log        | Insert k6_points; catalog compares overlapping HTTP buckets                       |

## Runtime notes

agentd writes the event log when its config has event_log (dir, level, trace_sql); the records are
byte-identical to core/events.ts toJsonLine (goldens shared with Go, IR-4c). Nothing in the server opens
DuckDB: ingest and analysis are offline, in the logs CLI (eventlog/), and the DuckDB file has a single writer,
so a query fails while an ingest holds it. The CLI writes its own records under the reader role. Retention
bounds JSONL only, requires committed watermarks and matching hashes, and never touches today or the CLI's
open file. Derived tables are never pruned. Gzipped retained sources are not re-ingested. Until IR-6 the Deno
server and its reader child process did all of this in-process (ADR 0004).
