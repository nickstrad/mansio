-- trace-timeline.sql
--
-- Question: the complete ordered event timeline for one trace_id across both processes —
-- mirrors the plan's named `trace` analysis query (§4.3, item 23).
-- Tables: platform_events, sql_events, tx_events, http_events, k6_points (schema v2).
-- The supported form of this query is `deno task logs:query trace --param traceId=<id>`
-- (eventlog/catalog.ts), which also drops the platform_events rows that
-- sql_events/tx_events/http_events already carry. This file lists them from both tables.
-- Variables: $trace_id.
--
-- Assumption: only these five tables carry a trace_id column in §4.1's DDL —
-- queue_events, backup_events and lifecycle_events do not, so they can't be joined into
-- a trace timeline and are intentionally excluded here.

SELECT ts, 'platform' AS source, event_kind AS kind, name, outcome, error_tag, duration_ms, NULL AS extra
FROM platform_events
WHERE trace_id = $trace_id

UNION ALL

SELECT ts, 'sql' AS source, statement_kind AS kind, table_name AS name, outcome, error_tag,
       duration_ms, sql_text AS extra
FROM sql_events
WHERE trace_id = $trace_id

UNION ALL

SELECT ts, 'tx' AS source, 'tx' AS kind, outcome AS name, outcome, error_tag, duration_ms,
       NULL AS extra
FROM tx_events
WHERE trace_id = $trace_id

UNION ALL

SELECT ts, 'http' AS source, handler_kind AS kind, name, CASE WHEN error_tag IS NULL THEN 'ok' ELSE 'error' END,
       error_tag, duration_ms, path AS extra
FROM http_events
WHERE trace_id = $trace_id

UNION ALL

SELECT ts, 'k6' AS source, metric AS kind, scenario AS name, status AS outcome,
       NULL AS error_tag, value AS duration_ms, CAST(tags AS VARCHAR) AS extra
FROM k6_points
WHERE trace_id = $trace_id

ORDER BY ts;
