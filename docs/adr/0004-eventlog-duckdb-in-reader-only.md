# ADR 0004 — The event log's DuckDB file is opened only by the reader process

- **Status:** superseded by [ADR 0014](0014-request-log-without-an-offline-tool.md) (the offline event-log
  tool and its DuckDB file are deleted)
- **Date:** 2026-10-10
- **Deciders:** the plan at `docs/plans/completed/effect-port-and-observability.md` item 21; recorded by item
  21
- **Related:** [ADR 0001 — Effect port](0001-effect-port.md) decisions 4 and 5
- **Implements:** `core/eventlog/ddl.ts`, `core/eventlog/offsets.ts`, `platform/eventlog/tables.ts`,
  `platform/eventlog/ingest.ts`

## Context

Workstreams 4 and 5 want an append-only event log that can be queried with analytical SQL. ADR 0001 settled
the shape: every process writes rotating JSONL files, and DuckDB tables are _derived_ from them. Item 21
builds that derivation and has to decide where it runs: who opens `Config.eventLog.duckdbPath`
(`EVENTLOG_DUCKDB`, default `$DATA_DIR/eventlog.duckdb`), and who may write it.

The server process is the SQLite writer. It hosts `node:sqlite` and every app's database file in read-write
mode. Two facts make it the wrong place for DuckDB, even though "the server already has the events in memory"
makes it look like the natural one:

1. **Two copies of SQLite in one address space.** DuckDB's `sqlite` extension carries its own SQLite.
   Event-log ingest never loads that extension itself, but once `@duckdb/node-api` is in a process, nothing
   stops a later change from loading it — an `ATTACH` for a debugging query, an analysis query that joins an
   app's tables. In the writer that would put two SQLite libraries over the same read-write database files:
   mismatched locking, WAL-index assumptions and page-cache state, with corruption as the failure mode (ADR
   0001 decision 5). Keeping the native module out of the writer's import graph is the only defence a grep can
   check.
2. **DuckDB allows one read-write process per database file.** A persistent DuckDB file is locked by the
   process that opens it. If the server and the reader could both open the event log, one of them would fail
   to start, or an ingest would fail on a lock held by the other. The file needs exactly one owner, and the
   reader process already owns DuckDB.

Two more facts shape the ingest itself:

3. **The writer's hot path must not depend on analysis.** `EventSink.emit` is `Effect<void, never>` (item 11).
   Appending to a JSONL file descriptor cannot fail a mutation. An insert into DuckDB on the request path
   could fail, block on a checkpoint, or contend for that file lock.
4. **Rebuildability.** If the JSONL tree is the only durable write path, the DuckDB file is a cache: deleting
   it loses nothing, and re-ingesting rebuilds it. That only holds if ingest is idempotent and exactly-once.

## Decision

### 1. The writer process never opens DuckDB — for the event log or anything else

`platform/sqlite.ts`, `platform/server.ts` and everything the server imports stay free of `@duckdb/node-api`.
The server's events go to `logs/server/...` as JSONL and nowhere else. When the server wants an ingest run, it
sends the frozen `{ kind: "ingest" }` reader request (`platform/services.ts`) over IPC, exactly as it does for
queries. Item 26's boundary test enforces this at the import-graph level.

### 2. The reader process is the event log's single owner

`EventLogIngestLive` is a scoped layer in the reader process. It opens the persistent event-log file with its
own `DuckDBInstance` (separate from the per-app `:memory:` read instances), creates the tables with
`CREATE TABLE IF NOT EXISTS`, and closes connection-then-instance in its finalizers. Runs are serialized on a
semaphore. A CLI that wants to ingest (`deno task logs:ingest`, item 23) must either ask the reader, or run
while no reader holds the file; it must never open the file alongside a running reader.

### 3. Ingest is exactly-once by construction, and append-only

- **Offsets.** `_ingest_state` holds, per source file, `bytes_ingested` (always just past a `\n`) and the
  SHA-256 of exactly that prefix. Each run re-hashes the prefix. A shorter file is a truncation, a different
  hash is a rewrite, and a match means "read from the offset". Only complete lines are consumed, so a line
  being written is never ingested half-done. The pure planning lives in `core/eventlog/offsets.ts` and is
  property-tested against adversarial histories (`tests/property/offsets_test.ts`).
- **One transaction per batch.** Staging (`read_json_auto` over an explicit list of offset-trimmed temp
  files), every event-table insert, and the `_ingest_state` upsert commit together or not at all. A crash can
  never advance an offset past rows that did not land, or land rows without advancing it.
- **Append-only tables.** Event tables only ever receive `INSERT`. `_ingest_state` is the one mutable table.
- **Rewrites are new generations, deduplicated forever.** A detected rewrite is re-read from byte 0, and from
  then on every line read from that file is inserted only if its `eventId` is not already in `platform_events`
  (the sticky `_ingest_state.dedupe` flag). The property suite found two double-insert paths that offsets
  alone cannot close: a torn `\n` restored by a later append, and a second rewrite bringing back an earlier
  generation's lines. Neither happens with the platform's own writers, which only append. The cost is one
  anti-join per batch, and only for batches that contain a tampered file; other batches never touch history.
  Within a batch, only the first flagged copy of an `eventId` lands. Duplicate _production_ stays out of
  model: an outside append that repeats lines while keeping the ingested prefix intact (`cat f f > f`) reads
  as new events.
- **Rejected lines are quarantined, never silently dropped.** A line that is not UTF-8, is over 1 MiB, or is
  not a JSON object with an `eventId` and a readable `ts` goes to `logs/<role>/<date>/rejected.jsonl` beside
  its source. The run emits an `ingest` event for it, and the rest of the batch proceeds. A line over 1 MiB
  keeps only its first 64 KiB of bytes in quarantine (a UTF-8 sequence cut there becomes U+FFFD); the source
  JSONL still holds it in full.
- **What the platform writes always parses.** `JSON.stringify` escapes a lone UTF-16 surrogate as `\ud800`,
  which DuckDB's `json_valid` rejects. Before staging, ingest replaces unpaired surrogate escapes with
  `\ufffd`, so a client cannot keep its own error events out of the tables. The session time zone is pinned to
  UTC, so a `ts` without an offset is not read in the host's zone.

## Consequences

- **Deviations from the plan.** `_ingest_state` gains a `dedupe BOOLEAN` column that §4.1 does not have. No
  event-log file exists yet (this item creates the first), so no migration is needed. The plan also places the
  file at `logs/duckdb/events.duckdb`, but item 08's `Config` defaults `EVENTLOG_DUCKDB` to
  `$DATA_DIR/eventlog.duckdb`. Ingest uses whatever `Config` says and does not change that default.
- **Analysis lags the write path.** The DuckDB tables are as fresh as the last ingest run, not real time.
  Scheduling runs is item 22's (timer) and item 23's (CLI) concern.
- **Every run reads every `.jsonl` file in full** to re-hash its prefix. That is the price of exact rewrite
  detection; a windowed hash would miss a truncate-and-regrow inside the window. Retention bounds it: dated
  directories are gzipped after `gzipAfterDays` (default 2), and `.gz` files are not ingested.
- **Ingest must run at least once between a file closing and retention gzipping it**, or the file's
  un-ingested tail is not ingested. With the default two days and a periodic ingest this is not a constraint
  in practice. A full rebuild from a tree that contains `.gz` files means gunzipping them in place first:
  offsets are keyed by the `.jsonl` path. **Follow-up (item 24):** nothing enforces this ordering yet;
  retention (item 24) owns ingesting a file's tail before gzipping it, and pruning the `_ingest_state` rows of
  files it gzips or deletes (they are otherwise kept, and read, forever).
- **Large integers.** DuckDB's JSON reader extracts integer literals exactly, but has already rounded a
  literal with a fraction or exponent to the nearest double. The typed columns therefore store `NULL` for any
  integer field they cannot prove exact, rather than a silently rounded value (`platform/eventlog/tables.ts`,
  `agent-work/work-scripts/deno/duckdb-json-bigint-probe.ts`).
- **The reader process gains a second DuckDB instance** and a write workload. It is still single-threaded per
  request, and ingest never touches the app instances or loads the `sqlite` extension.

## Alternatives considered

- **Write DuckDB directly from the server.** Rejected: facts 1–3 above.
- **A third process that owns the event log.** Cleanest isolation, but the reader already owns DuckDB and is
  already reachable over IPC. Another child process means another lifecycle, another protocol client, and
  another failure mode for no gain. ADR 0001 rejected it for the same reason.
- **Dedupe every line on `eventId`, with no offsets.** Simple and exactly-once by identity, but every batch
  would anti-join against the whole of `platform_events`, a cost that grows without bound, to defend against
  rewrites that the platform never performs. Offsets do the common case at O(new bytes); dedupe is reserved
  for files that need it.
- **Trust size alone (no prefix hash).** Misses any rewrite that leaves the file at least as long as before,
  including delete-and-recreate. Rejected.
