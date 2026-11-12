# Observability: the request log and the metrics

agentd tells you what it is doing in three ways:

- **The request log**: one JSON line per HTTP request, handler run, transaction and SQL statement on
  `/v1/ops`, all carrying the request's trace id, written to rolled files under `event_log.dir`. It answers
  "what happened to this request" and "which statements are slow". This page is its reference.
- **Metrics**: Prometheus text on `/metrics`, with the health endpoints `/healthz`, `/livez` and `/readyz`.
  They answer "is it healthy" and "is it overloaded". See [Metrics and health](#metrics-and-health).
- **The operational log**: agentd's own JSON log (`log_file`, default stderr): startup, failures with a log
  ID, degradations. It is not described here.

```text
SDK call ──X-Trace-Id──> agentd ──Emit (never blocks)──> bounded queue ──> writer ──> <dir>/server/<date>/*.jsonl
                           │                              full: dropped, counted       │  rolls by hour and size,
                           │                                                           │  deletes the oldest files
                           └── /metrics, /healthz, /livez, /readyz                     └──> jq, duckdb, or a log collector
```

mansio ships no log store, collector or dashboard. The files are plain JSON lines so that whatever the
deployment already runs (Vector, Fluent Bit, the OpenTelemetry Collector, Promtail, …) can ship them as they
are; until then, `jq` and the `duckdb` CLI read them in place ([Reading the log](#reading-the-log)). The
decision is [ADR 0014](adr/0014-request-log-without-an-offline-tool.md).

The writer is `server/internal/eventlog` (`record.go` is the record format, `log.go` the queue and the files,
`retention.go` the limits); agentd's use of it is `server/cmd/agentd/events.go`.

## Enabling it

The request log is off until agentd's config has an `event_log` section (`EventLogConfig` in
`server/cmd/agentd/config.go`):

```json
"event_log": { "dir": "logs", "trace_sql": true }
```

| Field             | Default           | Meaning                                                                                                  |
| ----------------- | ----------------- | -------------------------------------------------------------------------------------------------------- |
| `dir`             | required          | The log's root directory, relative to the config file. agentd writes under `<dir>/server/`.              |
| `level`           | `info`            | `debug`, `info`, `warn` or `error`: records below it are not written.                                    |
| `trace_sql`       | `false`           | One `sql` record per statement. Without it, only failed statements get one.                              |
| `buffer`          | `8192`            | Capacity of the queue between the requests and the writer, in records.                                   |
| `max_file_bytes`  | `67108864`        | Roll to a new file after this many bytes (64 MiB).                                                       |
| `max_total_bytes` | `4294967296`      | Delete the oldest files when the files under `<dir>/server` total more (4 GiB). Negative: no size limit. |
| `max_age`         | `1080h` (45 days) | Delete files older than this (by modification time). Negative: no age limit.                             |

`0` (or leaving a field out) means the default. With `level` at `warn`, only failed requests are logged; at
`error`, only server errors.

## Files

```text
<dir>/server/2026-11-11/events-22-76457-001.jsonl     # <role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl
<dir>/server/2026-11-11/events-22-76457-002.jsonl     # the same hour, after max_file_bytes
<dir>/server/2026-11-11/events-23-76457-001.jsonl     # the next hour
```

- `server` is agentd's role. Give each agent its own `dir`: agents that share one write distinct files (the
  pid differs), but each applies the size and age limits to all the files under `<dir>/server`.
- The date and hour in the path are the host's **local** time when the file was opened; `ts` in the records is
  UTC. A new file starts on a new local hour, a new date, or when the current one would pass `max_file_bytes`.
  `<seq>` starts at 1 and is one past the highest already in the directory, so a restart that reuses a pid
  never appends to an old file (files are created exclusively).
- Each line is one complete JSON object. The writer appends only; it never rewrites or compresses a file. It
  flushes whenever its queue is empty, so a record is in the file within milliseconds of the request's answer;
  under load it writes in 64 KiB blocks, so the last line of the open file can be incomplete until the next
  flush. Read up to the last newline.
- Lines are in the order the writer received the records, not strictly in `ts` order (a request's records are
  queued after its answer is sent). Sort by `ts` when the order matters.

## Records

Every record has the same envelope, then `kind`, then the kind's own keys, always all of them and always in
this order; an absent value is `null`. Strings never contain a line break (replaced by a space).

### The envelope

| Key            | Type          | Meaning                                                                                                                                                                                                                          |
| -------------- | ------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `eventId`      | string        | A random UUID (version 4) for this record.                                                                                                                                                                                       |
| `ts`           | string        | When the operation ended, UTC, to the millisecond: `2026-11-12T04:26:51.198Z`.                                                                                                                                                   |
| `level`        | string        | `debug`, `info`, `warn` or `error`. A request record is `warn` for a 4xx answer and `error` for a 5xx.                                                                                                                           |
| `role`         | string        | `server` (agentd).                                                                                                                                                                                                               |
| `pid`          | number        | agentd's process id.                                                                                                                                                                                                             |
| `traceId`      | string        | The request's `X-Trace-Id` if it is 1 to 128 characters of `A-Za-z0-9._:-`, else one agentd generated (and sent back in `X-Trace-Id`). `lifecycle` for lifecycle and backup records, `event-sink` for the writer's drop reports. |
| `spanId`       | string / null | Shared by the `handler`, `tx` and `sql` records of one request; `null` on `http` and the other kinds.                                                                                                                            |
| `parentSpanId` | null          | Always `null` in agentd.                                                                                                                                                                                                         |
| `appId`        | string / null | The app the request named; `null` when its body did not decode, and on lifecycle and drop records.                                                                                                                               |
| `kind`         | string        | `http`, `handler`, `tx`, `sql`, `queue`, `backup` or `lifecycle`.                                                                                                                                                                |

`durationMs` (and `queueWaitMs`) are numbers of milliseconds with microsecond precision (`0.115`).

### What one request writes

Only `POST /v1/ops` requests are logged; the health, metrics and admin endpoints are not. Each request writes,
under its trace id:

| Kind      | When                                                                    |
| --------- | ----------------------------------------------------------------------- |
| `http`    | Always: one per answer.                                                 |
| `handler` | When the body decoded (it has a kind and an app).                       |
| `tx`      | When its transaction ran on the database.                               |
| `sql`     | Per statement of that transaction with `trace_sql`; failed ones always. |

An SDK call that needs several round trips (a read that depends on an earlier answer, a mutation retried after
a conflict) is several requests with the same trace id.

### `http`

| Key           | Meaning                                                                                                  |
| ------------- | -------------------------------------------------------------------------------------------------------- |
| `method`      | `POST`.                                                                                                  |
| `path`        | `/v1/ops`.                                                                                               |
| `handlerKind` | `query` or `mutation`; `mutation` when the body did not decode.                                          |
| `name`        | The SDK handler, from the `X-Mansio-Handler` header (the SDK sets it); `v1.ops` when absent or invalid.  |
| `status`      | The HTTP status.                                                                                         |
| `durationMs`  | From receiving the request to writing the answer.                                                        |
| `bytesIn`     | Request body bytes.                                                                                      |
| `bytesOut`    | Answer body bytes.                                                                                       |
| `errorTag`    | For status 400 and above: the answer's error tag (`SchemaDriftError`, `ticket_invalid`, …).              |
| `errorDetail` | For status 400 and above: the `error` object the client received (`tag`, `message`, `fields`), redacted. |

### `handler`

| Key           | Meaning                                                                     |
| ------------- | --------------------------------------------------------------------------- |
| `handlerKind` | `query` or `mutation`.                                                      |
| `name`        | As in `http`.                                                               |
| `outcome`     | `ok`, or `error` when the status is 400 or above.                           |
| `durationMs`  | The server's handling of the decoded request, from the start to the answer. |
| `errorTag`    | As in `http`.                                                               |
| `errorDetail` | Always `null` (the detail is on the `http` record).                         |

### `tx`

| Key           | Meaning                                                                                                   |
| ------------- | --------------------------------------------------------------------------------------------------------- |
| `engine`      | `sqlite`.                                                                                                 |
| `outcome`     | `commit`; `rollback` when the request failed; `interrupted` when it hit its statement timeout or expired. |
| `operations`  | Statements the transaction ran.                                                                           |
| `durationMs`  | From the transaction's start on the database to its end.                                                  |
| `queueWaitMs` | Time the request waited for the database (its single writer) before the transaction started.              |
| `errorTag`    | The request's error tag for a rollback or an interruption.                                                |
| `errorDetail` | Always `null`.                                                                                            |

### `sql`

| Key             | Meaning                                                                                                |
| --------------- | ------------------------------------------------------------------------------------------------------ |
| `engine`        | `sqlite`.                                                                                              |
| `dialect`       | `sqlite`.                                                                                              |
| `statementKind` | `select`, `insert`, `update`, `delete`, `pragma` or `ddl`, from the statement's first word.            |
| `path`          | Always `null` in agentd.                                                                               |
| `tableName`     | The first table after `FROM`, `INTO`, `UPDATE` or `TABLE`, or `null`.                                  |
| `sqlText`       | The statement with `?` placeholders.                                                                   |
| `bindCount`     | How many values were bound.                                                                            |
| `bindValues`    | Always `null`: agentd never logs bound values.                                                         |
| `durationMs`    | The statement's execution time.                                                                        |
| `rowCount`      | Rows returned (a read) or changed (a write); `null` when the statement failed.                         |
| `outcome`       | `ok` or `error`.                                                                                       |
| `errorTag`      | `StatementDeadlineError` for a statement stopped at its deadline, else `StorageError`, when it failed. |
| `errorDetail`   | Always `null`.                                                                                         |

A query's transaction also reads `_mansio_changes` (the per-table change counters behind "this table has not
changed since" preconditions), so expect that statement beside your own.

### `queue`

The writer's report of records it dropped because its queue was full ([Limits](#limits-and-what-is-counted)):
`traceId` `event-sink`, level `warn`.

| Key       | Meaning                                     |
| --------- | ------------------------------------------- |
| `queue`   | `events`.                                   |
| `depth`   | Records queued when the report was written. |
| `cap`     | The queue's capacity (`buffer`).            |
| `event`   | `dropped`.                                  |
| `dropped` | Records dropped since the previous report.  |

### `backup`

One record per `POST /admin/backup` in durable mode (`traceId` `lifecycle`; `appId` is the app of the key).

| Key           | Meaning                                                              |
| ------------- | -------------------------------------------------------------------- |
| `phase`       | `publish-latest`.                                                    |
| `emitter`     | `backups`.                                                           |
| `runId`       | A random UUID for this backup.                                       |
| `key`         | The object key of the generation that holds the database.            |
| `bytes`       | Its size; `null` on failure.                                         |
| `sha256`      | Its digest.                                                          |
| `attempts`    | Always `null`.                                                       |
| `durationMs`  | How long the write-back took.                                        |
| `outcome`     | `ok` or `error`.                                                     |
| `errorTag`    | `BackupError`, or the admin answer's code (`fenced`, …), on failure. |
| `errorDetail` | `{"message": …}` on failure.                                         |

### `lifecycle`

`traceId` `lifecycle`: `phase` `startup` with `detail` `{"addr", "vm", "durable"}` once agentd serves, and
`phase` `shutdown` with `detail` `null` when it stops.

### Redaction

Every string value has `Bearer <token>` replaced by `Bearer <redacted>` and presigned-URL credentials
(`X-Amz-Signature`, `X-Amz-Credential`, `X-Amz-Security-Token`) replaced by `<redacted>`. In `errorDetail` and
`detail` objects, keys are sorted, a key whose name ends in `authorization`, `cookie`, `token`, `accesskeyid`,
`secretaccesskey`, `sessiontoken` or `securitytoken` (ignoring case and separators) has its value replaced by
`<redacted>`, and nesting deeper than 32 levels is cut to `<max-depth>`. Bind values are never logged.

## Limits and what is counted

**The request path never waits for the log.** A request hands its records to a bounded queue (`buffer`) and
moves on; one writer goroutine drains it into the file.

- **Queue full**: the record is dropped and counted (`mansio_eventlog_records_total{result="dropped"}`).
  Before its next record, the writer writes a `queue` record saying how many were dropped.
- **Write failure** (disk full, permissions): the records in hand are lost and counted (`result="lost"`), the
  file is abandoned (the next record opens a new one), and the operational log gets an `EVENTLOG.WRITE_FAILED`
  warning at most once a minute.
- **Shutdown**: agentd writes a `lifecycle` `shutdown` record and gives the writer 2 seconds to write what is
  queued; what is left is lost, counted, and reported as `EVENTLOG.CLOSE_FAILED` in the operational log.
- **Disk**: the writer keeps the files under `<dir>/server` within `max_total_bytes` and `max_age`. Each time
  it opens a new file (at most once an hour or once per `max_file_bytes`) it deletes, oldest first, every file
  older than `max_age`, then more of the oldest until the rest total at most `max_total_bytes`. It never
  deletes the file it has open or a file modified in the last 10 minutes, so the total can exceed the limit by
  those. It deletes only `events-*.jsonl` (and `.jsonl.gz`) files in the dated directories of its own role,
  then any dated directory left empty. Each deletion is counted (`mansio_eventlog_retired_total`) and logged
  (`EVENTLOG.RETIRED`).

| Metric                                                           | Meaning                                                                                         |
| ---------------------------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| `mansio_eventlog_records_total{result="written\|dropped\|lost"}` | Records written to the file, dropped at the queue, lost after it.                               |
| `mansio_eventlog_queue_depth`                                    | Records waiting for the writer.                                                                 |
| `mansio_eventlog_bytes`                                          | Bytes of the files under `<dir>/server`, as of the last deletion pass (when a file was opened). |
| `mansio_eventlog_retired_total{what="files\|bytes"}`             | Files and bytes deleted by the limits.                                                          |

The shipped alert `MansioUnexpectedDrops` fires when records are dropped or lost for 5 minutes.

## Shipping the log

A log collector should:

- tail `<dir>/server/*/events-*.jsonl`, discovering new files as they appear (one per hour per agent at
  least), and read only complete lines;
- parse each line as JSON and use `ts` as the event time; `traceId`, `appId`, `kind`, `level` and `name` make
  good indexed fields or labels (not `eventId` or `traceId` as labels in a label-indexed store: they are
  unique per record or request);
- keep its own position per file (path plus offset): files are only appended to, then left alone;
- ship faster than the limits delete. Size `max_total_bytes` and `max_age` to cover the longest outage of the
  collector or the store you accept; a file deleted before it was shipped is lost
  (`mansio_eventlog_retired_total` shows deletions).

The collector may delete or move files it has shipped; the writer tolerates that. Retention in the log store
is the store's own setting.

## Reading the log

Without a log store, read the files in place. The examples run in the directory that holds `<dir>` (the
guide's `$MANSIO_DIR`), against a log with `trace_sql` on; the outputs are real.

### One request by trace id

The SDK sends one `X-Trace-Id` per call (the `traceId` argument of `query` and `mutation`, else a random
UUID); agentd sends it back in the answer's `X-Trace-Id` header.

```sh
jq -c 'select(.traceId == "guide-read")
  | {ts, kind, name, status, outcome, sqlText, rowCount, durationMs} | with_entries(select(.value != null))' \
  logs/server/*/*.jsonl
```

```text
{"ts":"2026-11-12T04:26:51.198Z","kind":"http","name":"getGame","status":200,"durationMs":0.115}
{"ts":"2026-11-12T04:26:51.198Z","kind":"sql","outcome":"ok","sqlText":"SELECT * FROM \"games\" WHERE \"_id\" = ? ORDER BY \"_id\" ASC LIMIT ?","rowCount":1,"durationMs":0.024}
{"ts":"2026-11-12T04:26:51.198Z","kind":"sql","outcome":"ok","sqlText":"SELECT table_name, seq FROM \"_mansio_changes\"","rowCount":2,"durationMs":0.008}
{"ts":"2026-11-12T04:26:51.198Z","kind":"tx","outcome":"commit","durationMs":0.05}
{"ts":"2026-11-12T04:26:51.198Z","kind":"handler","name":"getGame","outcome":"ok","durationMs":0.111}
```

`jq -c 'select(.traceId == "guide-read")' logs/server/*/*.jsonl` prints the full records.

### The slowest SQL statements, with their handler

The [`duckdb` CLI](https://duckdb.org/docs/installation/) reads the JSON lines as a table. It is optional:
nothing in mansio needs it, and it only reads the files. `sql` records carry no handler name; joining on
`spanId` adds it.

```sh
duckdb -c "
  SELECT s.durationMs, h.name AS handler, s.rowCount, left(s.sqlText, 60) AS sql
  FROM read_json_auto('logs/server/**/*.jsonl') s
  JOIN read_json_auto('logs/server/**/*.jsonl') h ON h.kind = 'handler' AND h.spanId = s.spanId
  WHERE s.kind = 'sql'
  ORDER BY s.durationMs DESC LIMIT 5"
```

```text
┌────────────┬────────────┬──────────┬──────────────────────────────────────────────────────────────┐
│ durationMs │  handler   │ rowCount │                             sql                              │
│   double   │  varchar   │  int64   │                           varchar                            │
├────────────┼────────────┼──────────┼──────────────────────────────────────────────────────────────┤
│      0.047 │ createGame │        1 │ INSERT INTO "games" ("_id", "_creationTime", "_version", "xP │
│      0.031 │ makeMove   │        1 │ UPDATE "games" SET "xPlayer" = ?, "oPlayer" = ?, "board" = ? │
│      0.026 │ createGame │        0 │ SELECT * FROM "games" WHERE "_id" = ? ORDER BY "_id" ASC LIM │
│      0.024 │ getGame    │        1 │ SELECT * FROM "games" WHERE "_id" = ? ORDER BY "_id" ASC LIM │
│      0.022 │ makeMove   │        0 │ SELECT * FROM "moves" WHERE "_id" = ? ORDER BY "_id" ASC LIM │
└────────────┴────────────┴──────────┴──────────────────────────────────────────────────────────────┘
```

### Errors by handler

```sh
duckdb -c "
  SELECT name AS handler, status, errorTag, count(*) AS requests
  FROM read_json_auto('logs/server/**/*.jsonl')
  WHERE kind = 'http' AND status >= 400
  GROUP BY ALL ORDER BY requests DESC"
```

```text
┌───────────┬────────┬──────────────────┬──────────┐
│  handler  │ status │     errorTag     │ requests │
│  varchar  │ int64  │     varchar      │  int64   │
├───────────┼────────┼──────────────────┼──────────┤
│ listGames │    409 │ SchemaDriftError │        3 │
│ listGames │    400 │ SchemaError      │        1 │
│ getGame   │    400 │ SchemaError      │        1 │
└───────────┴────────┴──────────────────┴──────────┘
```

The same with `jq`:

```sh
jq -r 'select(.kind == "http" and .status >= 400) | [.name, .status, .errorTag] | @tsv' \
  logs/server/*/*.jsonl | sort | uniq -c | sort -rn
```

```text
3 listGames	409	SchemaDriftError
1 listGames	400	SchemaError
1 getGame	400	SchemaError
```

### Requests and latency per handler

```sh
duckdb -c "
  SELECT name AS handler, handlerKind, count(*) AS requests,
         round(quantile_cont(durationMs, 0.5), 3) AS p50_ms, round(max(durationMs), 3) AS max_ms
  FROM read_json_auto('logs/server/**/*.jsonl')
  WHERE kind = 'http' AND status < 400
  GROUP BY ALL ORDER BY requests DESC"
```

```text
┌────────────┬─────────────┬──────────┬────────┬────────┐
│  handler   │ handlerKind │ requests │ p50_ms │ max_ms │
│  varchar   │   varchar   │  int64   │ double │ double │
├────────────┼─────────────┼──────────┼────────┼────────┤
│ getGame    │ query       │        1 │  0.115 │  0.115 │
│ makeMove   │ mutation    │        1 │   0.29 │   0.29 │
│ createGame │ mutation    │        1 │  1.699 │  1.699 │
└────────────┴─────────────┴──────────┴────────┴────────┘
```

For rates and percentiles over time, use the metrics: the request log is for finding individual requests and
statements.

## Metrics and health

agentd serves these on `health_listen` when it is set, else on the query listener (`listen`):

| Endpoint   | Answers                                                                                                                                                                       |
| ---------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/metrics` | Prometheus text: requests and their latency, queues, admission and shedding, startup, the request log's counters, and in durable mode write-back age, ownership and breakers. |
| `/healthz` | 200 while serving, 503 once draining (for load balancers).                                                                                                                    |
| `/livez`   | 200 whenever the process can answer, also while draining (restarting a draining agent cuts the drain short).                                                                  |
| `/readyz`  | 200 when not draining and the last readiness probe passed (one every `ready_interval`, bounded by `ready_timeout`); else 503 with the reason.                                 |

What each metric means, the bound behind it and its runbook entry are in
[server/docs/capacity.md](../server/docs/capacity.md); the shipped Prometheus alert rules are
[server/deploy/prometheus/backpressure.rules.yml](../server/deploy/prometheus/backpressure.rules.yml). mansio
ships no Prometheus server or dashboards: point the deployment's own scraper at `/metrics`.
