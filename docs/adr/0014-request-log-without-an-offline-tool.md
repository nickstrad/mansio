# ADR 0014 — The request log is shipped, not analysed by an offline tool

- **Status:** accepted
- **Date:** 2026-11-12
- **Deciders:** Nick; the plan at [`docs/plans/go-only-platform.md`](../plans/go-only-platform.md) phase 1
- **Related:** supersedes [ADR 0004](0004-eventlog-duckdb-in-reader-only.md),
  [ADR 0007](0007-ingest-before-retention.md), the event-log database parts of
  [ADR 0006](0006-event-identity.md) and decision 4 of [ADR 0013](0013-go-server-replaces-deno-platform.md)
- **Implements:** deletes `eventlog/`, `scripts/logs.ts`, `scripts/duckdb/`, `core/events.ts`,
  `core/eventlog/`, `core/logging/`, `perf/ingest_k6.ts` and their tests; `server/internal/eventlog` is the
  only owner of the record format

## Context

agentd writes one JSON line per `/v1/ops` request, handler run, transaction and SQL statement, all carrying
the request's trace id (`server/internal/eventlog`, configured by agentd's `event_log` section). Until this
ADR a TypeScript tool read those files: `deno task logs:ingest` loaded them exactly once into a local DuckDB
file (ADR 0004, ADR 0006), `logs:query` ran a catalog of named, parameterised queries over it, `logs:sweep`
gzipped and deleted files only after they were ingested (ADR 0007), and `perf:ingest` loaded k6's raw points
so one query could join client and server latencies for the same trace.

That tool was a private log pipeline for one machine. With its tests it was about 16 000 lines, it kept a
second implementation of the record format byte-identical to Go's through shared goldens, it pulled a native
npm module (`@duckdb/node-api`) into the TypeScript gate, and its suites were documented to hang on macOS. A
deployed platform already has the parts it imitated: a log collector and store for "what happened to this
request", and Prometheus metrics, which agentd exposes, for health, throughput, error rates and overload.
Porting the tool to Go would build a component to retire.

## Decision

1. **Delete the offline tool and do not port it.** No ingest, catalog, sweep or k6 loader exists in Go or
   TypeScript.
2. **What stays is the request log as agentd writes it.** JSON lines under
   `<dir>/server/<date>/events-<HH>-<pid>-<seq>.jsonl`, written through a bounded queue that never blocks a
   request (drops and write failures are counted and exported as metrics), rolled by hour and size, and kept
   under a size and an age limit by the writer itself (`max_total_bytes`, `max_age`). The format does not
   change.
3. **The format has one owner.** `server/internal/eventlog/record.go` defines it; its tests pin the bytes of
   every kind against `server/internal/eventlog/testdata/goldens.jsonl`, which used to be written by the
   TypeScript side and is now frozen Go test data. A change to the format is a change to that file.
4. **Reading the log is the deployment's job.** A log collector ships the files as they are; for ad hoc
   reading, `jq` and the `duckdb` CLI read them in place ([docs/observability.md](../observability.md) has the
   worked queries). The `duckdb` CLI is optional and nothing in the repository depends on it for this.
5. **The e2e suite checks the files directly**: after a traced query and mutation through the SDK, the request
   log holds their `http`, `handler`, `tx` and `sql` records under the trace id, with the documented keys.

## Consequences

- What is lost, plainly:
  - **The named queries** (`trace`, `slowest-sql`, `error-rate-by-handler`, `handler-throughput`,
    `mutation-vs-query-latency`, `plan-shapes`, `queue-depth`, `tx-outcomes`, `backup-history`,
    `deadline-hits-per-handler-hour`, `k6-vs-platform`, …) as a tested, parameterised catalog. The worked
    one-liners in `docs/observability.md` replace the common ones; they are documentation, not tested code.
  - **The exactly-once local database**: there is no derived, append-only DuckDB copy with per-file offsets,
    rejected-line quarantine and schema versions (ADR 0004, ADR 0006 §4). A query reads the JSON files each
    time; a log store indexes them instead.
  - **Retention that waits for ingest** (ADR 0007): the writer deletes the oldest files at its limits whether
    or not anything shipped them. A collector that falls behind by more than the limits loses records; the
    deletions are counted in `mansio_eventlog_retired_total`.
  - **The k6 join**: client latencies from k6 and server records are no longer loaded into one table. k6's own
    summary (`summary.json`) reports the client side, and k6's trace ids (`<vu>-<iter>-<run-id>`) still find
    its requests in the request log.
- The TypeScript gate no longer needs DuckDB and no longer hangs on macOS (one property test still fails there
  on a SQLite rounding difference, so the gate runs in a Linux container: `docs/testing.md`).
- The writer's limits, counters and alert (`MansioUnexpectedDrops`) are unchanged; they were already the
  writer's own and did not depend on the tool.
- Log shipping, a log store, request tracing as spans and dashboards remain future work for the control plane
  hardening; none is part of the platform today.
- ADR 0006's record-level decisions stay in force through the Go format: every record has an `eventId`, the
  record's kind is `kind` and a handler's kind is `handlerKind`, and a backup record names its `emitter` and
  `runId`. Its table, view and schema-version decisions describe the deleted DuckDB copy.

## Alternatives considered

- **Port the tool to Go.** Keeps the catalog and the exactly-once database. Rejected: a second log pipeline
  beside the one every deployment has, to be maintained and then retired.
- **Keep the tool in TypeScript.** Rejected: it is the main reason TypeScript outside the SDK exists, it keeps
  a second implementation of the format, and its tests made the gate Linux-only.
- **Emit to a log store directly from agentd.** Deferred: it ties agentd to one store's protocol and failure
  modes; plain files that any collector reads keep the request path independent of the store.
