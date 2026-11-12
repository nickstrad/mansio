# ADR 0006 — Event identity, kind taxonomy and backup accounting in the event log (schema v2)

- **Status:** accepted for the record format (`eventId`, `kind`/`handlerKind`, a backup's `emitter` and
  `runId`, now `server/internal/eventlog`); its DuckDB tables and schema versions are superseded by
  [ADR 0014](0014-request-log-without-an-offline-tool.md)
- **Date:** 2026-10-11
- **Deciders:** the plan at `docs/plans/completed/effect-port-and-observability.md` item 35, from the accepted
  `reflection/astra` P2 finding; recorded by item 35
- **Related:** [ADR 0004 — the event log's DuckDB file](0004-eventlog-duckdb-in-reader-only.md)
- **Implements:** `core/eventlog/ddl.ts`, `core/events.ts`, `platform/eventlog/tables.ts`,
  `platform/backup.ts`, `platform/object_store.ts`

## Context

Item 21 built the DuckDB event log as §4.1 specified it. The reflection pass ran real queries against it and
found three ways the schema answers questions wrongly or not at all:

1. **Typed rows have no identity.** `sql_events`, `tx_events` and the other typed tables have no `event_id`.
   Joining a typed row back to its `platform_events` row (for `span_id`, `attrs`, `source_file`) can only use
   `ts` and `trace_id`, and many events share both. The reflection's `trace` query had to drop the typed
   tables to avoid duplicates.
2. **One column, two meanings.** `platform_events.kind` held the record's kind (`http`, `sql`, …) except for
   `handler` records, where it held the handler's `query` or `mutation`. A query for "handler events" had to
   know that a handler event never has `kind = 'handler'`. `http_events.kind` meant handler kind only.
3. **Backups double-count.** `Backups` wrote one `backup` record per phase. `ObjectStore` also wrote a
   `backup` record (`phase: put-object`) for every put, including the manifest and `latest.json` puts, which
   already had their own phase records. One backup therefore produced three `put-object` rows. Nothing named
   the run, so two backups under one caller trace could not be told apart. An interrupted upload was recorded
   by both producers in a race. S3 retries were invisible.

Fixing any of these changes persisted DDL. The event-log file is a derived cache, but by now it can hold
history that the retained JSONL no longer does: retention gzips old files, and `.gz` files are never ingested.

## Decision

### 1. `event_id` is the one identity, in every table that projects an event

Every typed event table gains `event_id` as its first column, holding the record's `eventId`. Ingest fills the
canonical and typed rows from the same staged rows in one transaction, so every typed row has exactly one
`platform_events` row with its `event_id`, of the matching kind, and every canonical row of a typed kind has
exactly one typed row. `ts` and `trace_id` are not join keys.

Duplicates follow the offset model (ADR 0004):

- An unchanged re-run reads nothing.
- A rewritten file is re-read with dedupe. A line lands only if its `eventId` is not already in
  `platform_events` and it is the batch's first copy, so a re-read old line and a repeated line each land
  once.
- Duplicate _production_ stays out of model: an outside process copying lines into a file while keeping its
  ingested prefix intact. Those lines would land twice in both tables, and
  `SELECT event_id FROM platform_events GROUP BY 1 HAVING count(*) > 1` finds them.

`k6_points` is not a projection of an event record. Item 25 fills it from k6's own output, so it gets no
`event_id`. **Item 25 handoff:** if `perf:ingest` creates the event-log file itself, holding only `k6_points`,
it must stamp it by running `EVENTLOG_DDL`, which writes the `(2, …, 'create')` row. An unstamped file with
event-log tables looks like v1 to `UNVERSIONED_GUARD`, so the next ingest would refuse it.

A record whose `kind` is unknown (say `metrics`) or absent is not rejected. It lands in `platform_events` with
`event_kind` set to that kind (or `NULL`) and no per-kind `name`, and no typed table gets a row.

### 2. `event_kind` and `handler_kind` replace the overloaded `kind`

| column                         | value                                                                                                                                               |
| ------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `platform_events.event_kind`   | the record's `kind`, always: `http`, `handler`, `sql`, `tx`, `queue`, `backup`, `lifecycle`, `ingest`                                               |
| `platform_events.handler_kind` | for `http` and `handler` records: `handlerKind` mapped `query` → `query`, `mutation` → `mutation`, anything else `NULL`; for all other kinds `NULL` |
| `http_events.handler_kind`     | the same explicit mapping (was `http_events.kind`)                                                                                                  |

The JSONL format does not change: records still carry `kind` and `handlerKind`, so every existing line ingests
under the new mapping. A line shaped after §4.1's original field list, a handler record whose `kind` is itself
`query` or `mutation`, maps to `event_kind = 'handler'` with that `handler_kind`.

`platform_events.outcome` keeps each kind's own vocabulary, so item 23 can tell failures apart rather than
fold them into one value:

| `event_kind`                            | `outcome`                                                    |
| --------------------------------------- | ------------------------------------------------------------ |
| `handler`, `sql`, `backup`              | `ok` / `error`                                               |
| `tx`                                    | `commit` / `rollback` / `interrupted`                        |
| `http` (no outcome field in the record) | `error` when `errorTag` is set or `status` is 5xx, else `ok` |
| `queue`                                 | the signal: `high-water` / `rejected` / `dropped`            |
| `ingest`                                | `status`                                                     |
| `lifecycle`                             | `NULL`                                                       |

For item 23's catalog, a failure is `outcome IN ('error', 'rollback', 'interrupted')`; `error_tag` names it.

### 3. Backup records name their emitter and their run

`BackupPayload` gains `emitter` (required), `runId` and `attempts`. `backup_events` gains `emitter`, `run_id`
and `attempts`.

- **`emitter: "backups"`** — orchestration. `Backups.take` mints a `runId` (from `IdGen`, separate from the
  trace, because one caller trace may span several backups). It writes exactly one record per phase the run
  reaches: `snapshot`, `verify`, `hash`, `put-object`, `put-manifest`, `publish-latest`. These are written on
  success, failure and interruption alike.
- **`emitter: "object-store"`** — transfers. `ObjectStore` writes one record per PUT, whoever asked for it,
  with `phase: put-object`. `runId` comes from the `backupRunId` log annotation that `take` sets, and is
  absent outside a run. `attempts` counts the requests sent: 1 plus retries, or 0 when the key was rejected
  first. Gets stay `lifecycle`/`object-get` records (`BackupPhase` has no read phase) and gain `attempts` in
  `detail`.

A transfer links to the phase that issued it by `(run_id, key)`. Accounting therefore needs no interpretation:

| question                  | query over `backup_events`                                            |
| ------------------------- | --------------------------------------------------------------------- |
| backups started           | `count(DISTINCT run_id)`                                              |
| backups completed         | `emitter = 'backups' AND phase = 'publish-latest' AND outcome = 'ok'` |
| phase durations, failures | `emitter = 'backups'`, grouped by `phase`                             |
| transfers                 | `emitter = 'object-store'`                                            |
| retries                   | `sum(attempts - 1)` over `emitter = 'object-store'`                   |

The interrupted-upload race is gone. `Backups` always records its `put-object` phase, with no `errorDetail`.
The store's transfer record is the one that speaks for the object. An earlier draft of this item claimed the
store already wrote that record on interruption. It did not: the record was emitted after the PUT returned, so
an interrupted transfer left none, and nothing said whether the object might exist. The store now emits its
record from a finalizer, so an interrupted PUT still writes one `object-store` record
(`errorTag:
Interrupted`, `attempts` so far, `errorDetail: { written: "unknown" }`). The interrupt can land
before the request, during it or after the backend stored the bytes, so `unknown` is the honest answer. The
marker moved from `Backups`' phase record to the store's transfer record.

Records written before this change have no `emitter`. Ingest infers it from what each producer wrote then:

- `put-object` came from the object store, except `Backups`' record of an interrupted upload
  (`errorTag: Interrupted`, `errorDetail.written: unknown`).
- Every other phase came from `Backups`.

An `emitter` value this build does not know maps to `NULL`, not to a guess. Pre-change records have no
`run_id`.

### 4. The schema is versioned; v1 is rebuilt from JSONL, never reused

**Versioning.** A new `_eventlog_schema (version, applied_at, method, detail)` table records the version. Rows
are only ever inserted. This schema is **v2**; item 21's unversioned schema is **v1**. `EVENTLOG_DDL`, which
ingest runs on every connection, now begins with two guards. Both only read DuckDB's catalog, and they run
before any statement can create or change anything:

- `UNVERSIONED_GUARD` fails if event-log tables exist without `_eventlog_schema`, i.e. a v1 file.
- `VERSION_GUARD` fails if the file is stamped with any version but this build's.

A fresh file gets the v2 tables and one `(2, …, 'create')` row. A v1 file makes ingest fail at connect with
`IngestError({ phase: "inventory" })`, naming the cause and the remedy. The file is left byte-for-byte
unchanged: a clean v1 file opened and refused this way keeps its exact bytes. A v1 file with a pending WAL has
the WAL folded in, which changes bytes but not content. No v2 mapping ever writes into a v1 table.

**Why rebuild, not migrate in place.** v1's typed rows carry neither `event_id` nor a source offset. No SQL
can give them back their identity: that is the very join this item exists to make exact. `platform_events`'
handler kind for `http` rows is likewise unrecoverable from v1 alone. The JSONL tree is the source of truth
(ADR 0001), and the real ingest is the one mapping this build trusts. So the upgrade re-runs it.

**`upgradeEventLog`** (`platform/eventlog/tables.ts`) is explicit, and each step must succeed before the next:

1. **Refuse an unsafe start.** If `<file>.wal` exists, the file was not closed cleanly, and the upgrade
   refuses rather than copy a half-checkpointed file. Opening it once, even via a refused ingest, checkpoints
   it. The schema is read through a `READ_ONLY` attach, which also fails explicitly if another process holds
   the file.
2. **Preserve the original.** The upgrade copies the v1 file byte-for-byte to `<file>.v1-pending.duckdb` and
   fsyncs it. The copy takes its kept name, `<file>.v1-<instant>.duckdb`, only after step 5's switch. So an
   attempt killed earlier (OOM, power loss) leaves no timestamped orphan. Its pending copy is a copy of a v1
   file still in place, and the next attempt discards it along with the side files.
3. **Rebuild through the existing ingest entry.** `makeEventLogIngest` ingests the whole retained JSONL tree,
   from offset 0, into `<file>.v2-rebuild.duckdb`.
4. **Prove completeness against the copy.** Every v1 `platform_events` `event_id` must be in the rebuild.
   Every v1 typed table must have exactly as many rows as v1 `platform_events` has of its kind (v1 inserted
   them pairwise), and each of those events must have a row, by `event_id`, in the rebuilt typed table. Values
   may differ, since that is the new mapping, but no row may be missing. v1 `k6_points` rows have no JSONL
   source and an unchanged DDL, so they are copied over verbatim. The rebuild records its evidence in a
   `(2, …, 'rebuild-from-v1', detail)` row.
5. **Switch** by fsyncing the rebuilt file and renaming it over the original. The rename is atomic, so the
   path always names a whole v1 file or a whole v2 file. The copy is then renamed to its kept name, and the
   directory is fsynced so that both renames survive a crash. A process killed between the two renames leaves
   a v2 file beside `<file>.v1-pending.duckdb`, which then holds the only v1 original. The upgrade never
   removes a pending copy while the live file is v2. The operator renames it.

If step 3 or 4 fails, the upgrade fails with `IngestError` (`phase: "plan"` when sources are missing). It
removes the side file and the copy and emits a `lifecycle` `eventlog-schema-upgrade-refused` event. The v1
file and its `_ingest_state` offsets are exactly as they were, and ingest keeps refusing it. The usual cause
is a source that retention gzipped or deleted after v1 ingested it. The error names up to five such sources.
The remedy is ADR 0004's: gunzip the `.jsonl.gz` in place (or restore the file) and retry. If those sources
are gone for good, the only way forward is a human decision to move the v1 file aside. The platform never
discards the v1 rows that only that file still holds.

The normal ingest event tables stay append-only. The upgrade creates a new file rather than change one.

## Consequences

- **Item 36** owns ingest startup. The guard needs no wiring, because it is in `EVENTLOG_DDL`. Whether ingest
  startup should call `upgradeEventLog` automatically on a v1 file, or leave it as an operator step, is item
  36's call. `inspectEventLogSchema` reports `absent` / `current` / `v1` without opening the file for writing.
  **Item 36 handoff:** the `READ_ONLY` attach in step 1 refuses a file another process holds, but DuckDB's
  file lock is a per-process POSIX lock. It may not protect if `upgradeEventLog` is called from inside the
  reader while the reader's own ingest connection has the file open. Wiring it into startup must close that
  connection first, or serialize the call with ingest.
- **Item 23** writes its catalog against v2: `event_kind` / `handler_kind`, `event_id` joins, the outcome
  vocabularies above, and `emitter` / `run_id` / `attempts` for `backup-history`. Its `trace` query can now
  join typed tables exactly instead of dropping them.
- **Item 32** composes producers. Any new `backup` producer must set `emitter`, which the type system
  requires. A new typed table needs a `TYPED_EVENT_TABLES` entry, and the typed mapping in `tables.ts` is
  keyed by the same names, so neither can be added without the other.
- **`scripts/duckdb/*.sql`** (outside this item) still use v1's `platform_events.kind` and `http_events.kind`:
  `event-counts-by-type.sql`, `recent-errors.sql`, `trace-timeline.sql`. These fail with a binder error
  against a v2 file until item 23 replaces them or item 37 reconciles them.
- **`backup-durations.sql` is worse: against v2 it runs and gives wrong answers.** Every column it reads still
  exists, so nothing fails. It has no `emitter` filter, so its `put-object` row mixes `Backups`' one phase
  record per run with the store's one transfer record per PUT (snapshot, manifest and `latest.json`). That
  inflates `phase_count`, and the `failure_count`, durations and `total_bytes` of the two record kinds get
  summed together. Its stated assumptions (no `trace_id`, no run) are now answered by `run_id`. Until item 23
  or 37 replaces it, do not trust its `put-object` row.
- **A rebuild re-quarantines** every historical rejected line: like any first ingest, it appends a second copy
  to `rejected.jsonl`. That is a duplicate in a diagnostic file, which is ingest's documented safe failure.
- **A rebuild re-reads** every retained `.jsonl`. That is the same cost as a first ingest, paid once.
- **Deviation from §4.1:** the column changes above, plus `_eventlog_schema`. §4.1 remains the plan's
  historical text; `core/eventlog/ddl.ts` is the schema of record.

## Alternatives considered

- **`ALTER TABLE … ADD COLUMN event_id` in place.** Old typed rows would keep a `NULL` `event_id` forever,
  joinable only by `ts`/`trace_id`. That is the defect, made permanent and mixed with correct rows. It also
  reuses v1 tables under v2 mappings, which the item forbids. Rejected.
- **Copy v1 rows into the v2 tables with best-effort mappings.** This is lossy for typed rows (no identity)
  and for `http` handler kinds. Rejected in favour of a provable rebuild that fails explicitly.
- **New table names (`platform_events_v2`, …) in the same file.** Every query, and ingest's `_ingest_state`
  statements (owned by item 36), would carry version suffixes forever. Rejected; the version table plus a file
  swap keeps canonical names.
- **Make `upgradeEventLog` automatic in ingest.** That needs ingest initialization changes, which item 36
  owns. It would also turn a startup into a full re-ingest without an operator seeing why. Deferred to
  item 36.
- **A separate `operation_id` for transfers.** A store operation writes exactly one record, so its `event_id`
  already identifies it. `(run_id, key)` links it to its phase.
- **Keep the store's transfer records out of `backup_events`** (e.g. as `lifecycle`). Transfers are part of a
  backup's cost, and item 23's `backup-history` needs their bytes and durations. With `emitter`, keeping them
  is unambiguous.
