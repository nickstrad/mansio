# ADR 0007 — Retention follows committed ingest

- **Status:** accepted
- **Date:** 2026-10-11
- **Deciders:** the plan at `docs/plans/completed/effect-port-and-observability.md` item 36, from the accepted
  `reflection/astra` P1 finding; recorded by item 36
- **Related:** [ADR 0004 — the event log's DuckDB file](0004-eventlog-duckdb-in-reader-only.md),
  [ADR 0006 — event identity and schema v2](0006-event-identity.md)
- **Implements:** `core/logging/rotation.ts`, `platform/logging/retention.ts`, `platform/eventlog/ingest.ts`,
  `platform/logging/file_sink.ts`

## Context

The JSONL tree under `LOG_DIR` is the event log's source of truth. The DuckDB event tables are derived from it
by ingest, which runs in the reader process (ADR 0004). Item 24's retention sweeper gzipped and deleted JSONL
files by age and by a total-bytes cap. Nothing tied that to ingest:

- Ingest reads only `*.jsonl`. A file gzipped before its tail was ingested never reached DuckDB.
- A file deleted before ingest read it was simply gone.
- The only protection was an age gap (`LOG_GZIP_AFTER_DAYS`). A stopped reader, a failing ingest, or a small
  `LOG_MAX_TOTAL_BYTES` (which deletes regardless of age) defeated it.
- A rejected line counted as "handled" once quarantined, but the quarantine write was not durable before the
  offset commit that consumed the line.
- A torn last line, left by a crash or a failed write, is never completed. Ingest never consumes it, so its
  file could never be proven fully ingested.

The sweeper also runs in processes that cannot see `_ingest_state`: the server, which must never load DuckDB,
and the CLI. Retention therefore needs facts that only the reader has.

## Decision

### 1. A file may be gzipped or deleted only when committed ingest covers all of it

`rotation.retentionBlock` is a pure gate. `rotation.plan` gained a `pinned` set: a file the gate refuses is
pinned. The sweeper evaluates the gate on every event file that the plan wants to gzip or delete. The checks
run in this order, and the first one that fails is reported:

| check                | passes when                                                                           |
| -------------------- | ------------------------------------------------------------------------------------- |
| `open`               | no sink in the sweeping process holds the file                                        |
| `unsettled`          | its mtime is at least `settleMs` (default 10 min) before now, and not in the future   |
| `changed`            | size, mtime and inode match the inventory before and after the full read              |
| `unreadable`         | the content, decompressed for a `.gz`, can be read                                    |
| `no-watermark`       | `_ingest_state` has a committed row for the source                                    |
| `unread-tail`        | `bytes_ingested` is at least the content length                                       |
| `watermark-mismatch` | `bytes_ingested` equals the content length and `prefix_sha256` is the content's hash  |
| `unreconciled`       | unless the source is `dedupe`, accepted rows ∪ quarantine records equal record starts |

"Closed" combines the date rule (only directories dated before today are ever planned), `open` and
`unsettled`. It relies on the sink's contract, now written down in `file_sink.ts`:

- a sink appends only while `now` is inside its file's hour bucket;
- every roll opens a fresh `seq`, and a file it rolled away from is never reopened;
- a failed write abandons the handle instead of finishing the torn line.

Under that contract, a file dated before today can receive no more bytes once its last in-flight write has
settled.

"Today" is the local date (`rotation.dateBucketOf`), so the contract assumes that every process writing to or
sweeping a `LOG_DIR` uses the same timezone. A reader whose local date is ahead of the server's would treat
the server's current directory as yesterday, and an idle server could later append to a file the reader had
already sealed or removed. The server and its reader child inherit one environment, so they agree. A process
deployed separately must be given the same `TZ`.

The watermark check is a full identity check. The file's whole content must hash to the prefix hash that
ingest committed. An append, a rewrite or a stale inventory therefore blocks destructive action.
Reconciliation is exact: it compares sets of offsets, not counts. Every complete line, plus a trailing partial
line if there is one, must appear in the union of the source's `platform_events.source_offset`s and the
`sourceOffset`s its directory's `rejected.jsonl` records for it. Nothing else may appear in either set. The
gate checks only that union; it does not assert that the two sets are disjoint. A `dedupe` (rewritten) source
drops lines whose `eventId` already exists under another source, so the watermark alone is its evidence.

A pinned file keeps counting toward `LOG_MAX_TOTAL_BYTES`, so the cap moves on to newer directories. The
pinning loop repeats until no plan names a file that has not been judged. Pinned files are reported as
`blocked`, with a reason and a byte count: this is the unsafe-to-delete backlog. A `rejected.jsonl` is pinned
(`quarantine-evidence`) while any event file in its directory stays. It is deleted last, after a fresh check
that no event file remains beside it.

`applyPlan` re-checks the gate's result independently. It refuses (`unverified`) an event file in `gzip` or
`delete` that the gate did not pass. Right before each removal it checks the inode, size, mtime and parent
directory again. A gzip also re-hashes the source against the gate's hash.

### 2. Rejected lines count as consumed only with durable evidence

Ingest now fsyncs each quarantine write, first the file and then its directory, before the `COMMIT` that moves
the offset past it. A committed watermark therefore never covers a reject whose record could still be lost in
a crash.

A torn tail is consumed by **sealing** it. `sealTornTails` runs right after a successful ingest. It picks
sources that meet all of these conditions:

- dated before today, not open, and settled;
- the committed offset still matches the file's prefix;
- everything after the offset is one newline-free tail.

For each such source, one transaction quarantines the tail as a `torn-tail` reject, durably, and moves the
offset to the end of the file. That offset now ends a partial line rather than sitting just past a `\n`. A
byte appended later would fall outside the writers' model, and ingest would read it, and quarantine it, as a
line of its own.

The same transaction also covers an empty source. Ingest skips a 0-byte file, so it never gets a row; without
one it would stay `no-watermark` and pin its directory and `rejected.jsonl` forever. A closed, settled, empty
source with no row therefore gets an empty-prefix row (offset 0, the hash of no bytes), which the gate accepts
as complete.

Like a `line-too-large` reject, the quarantine record keeps only the tail's first 64 KiB, decoded lossily. A
torn tail can never parse, so the record proves the tail was consumed and shows its start. Once the source
ages out, the rest of a longer tail is gone with it.

### 3. Ingest, then sweep, in the reader, through the existing RPC

The sweeper never opens DuckDB. It takes an `IngestLedger` (committed watermarks and accepted offsets), and
there is no sweep without one. The only live supplier is `makeEventLogIngest(...).ingestThenSweep`. Under the
ingest lock it runs, in order:

1. an ingest run; if it fails, the call fails and nothing is swept;
2. torn-tail sealing;
3. the sweep, using the committed state as its ledger;
4. a budget measurement.

`eventLogIngestThenSweepLayer` / `EventLogIngestThenSweepLive` provide the **existing** `EventLogIngest` tag,
with `ingest` bound to that call. That layer is the whole hook:

- **Item 33** composes it in the reader and answers the existing `ingest` RPC with it.
- **Item 32** triggers it through `ReaderClient`.
- The reply is an `IngestThenSweepReport`, a superset of `IngestReport`.
- No RPC kind is added, and no item-34 service changes.

A sweep failure is reported in the reply and in its own lifecycle event. It does not fail an ingest that has
already committed.

`startRetentionSweeper` remains, but needs a ledger like any sweep. In practice the schedule is item 32's
ingest RPC. A sweep run without one would be a mistake: the server cannot see `_ingest_state`.

**Schema compatibility (item 35).** Ingest startup already refuses an incompatible file. Every connection runs
`EVENTLOG_DDL`, whose guards fail before any statement touches a v1 or foreign file, so the layer cannot build
and no sweep runs. The JSONL that an upgrade rebuild needs is therefore kept. The coordinator does not call
`upgradeEventLog`: it would have to close its own connection first (item 35's N3 warning), so the upgrade
stays an explicit operator step.

### 4. Crash and retry

Every step is idempotent, and the next sweep repairs a crash at any point:

- **gzip:** write `<file>.gz.<pid>.tmp`, fsync, rename to `.gz`, verify that it decompresses to the gate's
  hash, fsync the directory, re-check, then unlink the source.
  - A crash before the rename leaves a `.tmp`, which is deleted once it is an hour old.
  - A crash after the rename leaves a `.gz` beside its source. While both exist, ingest sees only the plain
    source, which is fully read. The next sweep verifies the `.gz` against the gate's hash and removes the
    source. A `.gz` that does not match is left with its source and reported.
- **rename:** the `.tmp` → `.gz` rename is the only one, and it is atomic.
- **delete:** a delete either happened or is retried. Directories are fsynced at the end.
- **quarantine files** are deleted last, and only from a directory with no event file left.
- **ingest and seal** are single DuckDB transactions. Quarantine records are durable before `COMMIT`, so a
  crash between the two leaves at most a duplicate quarantine record. Reconciliation uses sets, so a duplicate
  is harmless. A quarantine write torn by a full disk leaves an unterminated fragment. The retry starts on a
  fresh line, so its records are not glued to the fragment, and the fragment itself proves nothing.

**Byte offsets survive compression**, because only a fully ingested source is compressed and the `.gz`
decompresses to exactly those bytes. `_ingest_state` rows are **never pruned**. A row is a tombstone: a source
gunzipped in place, or restored, is skipped, and a recreated path is read as a rewrite with dedupe. This
answers item 24's open question about `_ingest_state` pruning. The cost is one row per source file, reported
as part of the derived growth.

### 5. Two budgets, reported separately

`IngestThenSweepReport.budgets` keeps the two stores apart:

- **`jsonl`:** retained bytes and files, today's bytes, the blocked backlog and `LOG_MAX_TOTAL_BYTES`.
  `withinCap` may be false while today's files or the blocked backlog are over the cap. The cap holds only as
  far as closed-file safety allows.
- **`derived`:** the DuckDB file and its WAL in bytes, `platform_events` rows, and `_ingest_state` rows.
  `boundedByLogMaxTotalBytes` is always `false`. Event tables are never pruned (still out of scope), so the
  derived file grows without bound. It is measured, not capped.
- **`logger.dropsRecorded`:** the sinks' own drop reports (`queue_events`, `event = 'dropped'`), as ingested.
  These are counted apart from what reached disk.

Emitted records equal flushed records plus dropped records, plus the I/O losses that `logger.ts` reports only
on stderr. Flushed records equal ingested records plus rejected records.

**Item 23 must carry this contract into its observability guide and its `logs:sweep` CLI.** The CLI calls this
same coordinator (`ingestThenSweep({ dryRun })`) and does not run a sweep of its own. It reports the two
budgets separately and never says that `LOG_MAX_TOTAL_BYTES` bounds the DuckDB file.

## Consequences

- **No un-ingested byte becomes inaccessible.** `tests/property/retention_ingest_test.ts` drives seeded
  interleavings with a real filesystem and a real DuckDB file:
  - writer protocol: append, rotate, torn write, day change, and late append (also racing inside a run);
  - coordinator: ingest-then-sweep, injected commit failure, interruption, restart, and gzip-crash leftovers.
  - It asserts that every accepted event is ingested exactly once, that every line whose file is gone was
    ingested or durably quarantined, and that a final run leaves no backlog.
- A source that is never fully ingested is kept forever and stays visible as `blocked`. Examples are a reader
  that stays down, and a legacy `.gz` from item 24 that was gzipped early. Losing it silently is no longer
  possible.
- Retention costs more I/O. Every candidate is read in full and hashed, and one query fetches its accepted
  offsets. That cost is bounded by what the sweep would remove anyway.
- The sweep now emits a lifecycle event only when it did something, skipped something or was blocked, so a
  quiet tree stays quiet after its own events are ingested.
- Retention without the reader does nothing destructive. Running the CLI while the reader holds the DuckDB
  file is item 23's to arrange: DuckDB's file lock allows one opener.
- One residual race remains, documented rather than closed. A writer suspended for longer than `settleMs`
  between capturing `now` and writing could append to a file that has just been removed. The platform's sinks
  capture `now` immediately before the write.

## Alternatives considered

- **Compress a copy that keeps offsets, and keep ingesting `.gz`.** Ingest would need to stream-decompress
  every retained `.gz` on every run just to re-hash its prefix. "Never compress before the whole source is
  durably ingested" gives the same guarantee for less work.
- **Prune `_ingest_state` rows when their source leaves the tree.** That makes a restored or gunzipped source
  look new, and its events would land twice. Tombstones cost one row per file.
- **Gate on the watermark alone, without reconciling offsets.** A watermark only says that ingest consumed the
  bytes. Reconciling the offsets also proves that every record landed somewhere it can still be found.
- **Let the cap ignore pinned bytes.** The cap would then report success over the real disk use. Counting
  pinned bytes, pruning other files, and reporting the remainder is the honest version.
- **A new `sweep` RPC kind.** The plan forbids it. Binding `EventLogIngest.ingest` to ingest-then-sweep gives
  callers the same capability with no protocol change.
