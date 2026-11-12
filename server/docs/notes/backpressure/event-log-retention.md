# Event-log retention in the writer

How agentd's event-log writer bounds its own files on disk by bytes and age, and counts what it drops.

The offline ingest and sweep that older text here refers to were removed
([ADR 0014](../../../../docs/adr/0014-request-log-without-an-offline-tool.md)): the writer's limits are the only
retention, and a log collector is expected to ship files before they reach them.

Related work not implemented: a warm-set default (requiring `cache_bytes` or `max_active` in durable mode),
which waits on the open LiteFS / write-back decision; exporting the owner-slot count (the slots are never
removed); and rotating the operational log (one append-only file).

Before this, the log rolled files by hour and at 64 MiB and nothing in the server deleted them, and records
still queued when the close budget ended were lost without being counted.

Now (`internal/eventlog/retention.go`, `log.go`; agentd `event_log.max_total_bytes`, `event_log.max_age`):

- When the writer opens a new file it deletes the oldest files of its role until the rest total at most
  `max_total_bytes` (default 4 GiB) and none is older than `max_age` (default 45 days).
- Never deleted: the open file, a file written in the last 10 minutes, another role's files, anything not
  named like an event file. So the ceiling is the limit plus one file plus the last 10 minutes of writing.
- `planRetention` is the pure decision (table test with on and off points); `TestRetentionBoundsTheTree` is
  the scenario: 101 records in 1.5 KB files with a 10 000-byte limit ends with 9 936 bytes on disk, 29 files
  deleted, the old days gone, an operator's file and another role's file untouched.
- A file deleted before ingest is lost telemetry, which is acceptable for telemetry as long as the loss is
  counted: `mansio_eventlog_retired_total{what="files|bytes"}`, `mansio_eventlog_bytes`, log event
  `EVENTLOG.RETIRED`.
- `Close` past its deadline now counts the records it abandons in `Stats.Lost`
  (`mansio_eventlog_records_total{result="lost"}`) and names how many were queued in its error
  (`EVENTLOG.CLOSE_FAILED`); `TestCloseCountsWhatItAbandons`.
