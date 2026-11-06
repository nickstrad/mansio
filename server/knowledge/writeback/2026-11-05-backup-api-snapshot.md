# 2026-11-05 — snapshot with the SQLite backup API: the write stall is gone (CP-8)

Author: CP-8 agent. Machine: Apple M5 Max, macOS 27.0, Go 1.26.4, modernc.org/sqlite v1.61.1 (SQLite 3.54.1),
GOMAXPROCS=18, load average ~3. Store = in-memory fake (`durabletest.Store`).

## What changed

`actor.Snapshot` used `VACUUM INTO` on the write connection (it fails on a `query_only` reader), so every write
to that database queued for the whole copy. It now uses the online backup API (`sqlite3_backup_*`, reached
through `sql.Conn.Raw` → modernc's `conn.NewBackup`):

- **Where:** on a read-pool connection when `JournalMode = WAL` and `ReadConns > 0`; otherwise on the writer,
  between batches, as before. A rollback journal is excluded on purpose: a long read transaction there would
  block the writer's COMMIT (SHARED lock), turning the stall into `SQLITE_BUSY` after `busy_timeout`.
- **Consistency:** the copy pins one read transaction (`BEGIN; SELECT … FROM sqlite_schema`) across all
  steps, so it is one committed state and never restarts under concurrent commits. `ctx` is checked between
  steps of 4096 pages (16 MiB at 4 KiB pages).
- **"path must not exist"** is kept: the destination is created `O_EXCL` first (an empty file is a valid empty
  database for the backup to fill).
- **Trade-off: the copy is not compacted.** VACUUM INTO wrote only live pages; the backup copies the
  freelist too, so after heavy deletes every generation (hash, upload, cache) carries the dead pages. If that
  shows up in bucket bytes, run an occasional `VACUUM` (or `auto_vacuum=INCREMENTAL`) when `freelist_count`
  is a large share of `page_count`. Not built.
- The destination is opened with `synchronous(OFF)`, `journal_mode(OFF)`: it is a temp file made durable by
  its upload.
- With a read pool the snapshot is a read: it can be shed (`ErrMailboxFull`) if every reader is busy and
  their queue is full. The write-back retries at the next tick.

## Before / after (same machine, same session, median of 3, ms)

Command (under `scripts/lock.sh results/.bench-lock`), base = `e538e31`, after = this branch:

```sh
wbbench -sizes 16M,128M,512M -reps 3 -tick-dbs 100
```

| size | snapshot before | snapshot after | max write stall before | max write stall after |
|---:|---:|---:|---:|---:|
| 16 MiB | 19.8 | 19.3 | 19.5 | 0.19 |
| 128 MiB | 148.9 | 140.5 | 148.7 | 0.88 |
| 512 MiB | 589.5 | 558.1 | 589.3 | 1.17 |

Raw: [before](2026-11-05-wbbench-backup-before.txt), [after](2026-11-05-wbbench-backup-after.txt),
[after CSV](2026-11-05-wbbench-backup-after.csv). "Max write stall" is the slowest write of a writer that runs
every 200 µs during the warm write-back.

- **The stall equalled the snapshot; now it is ~1 ms at any size** (the writer's own commit time plus
  scheduling noise). Writes and the copy run side by side in WAL mode.
- The copy itself is the same speed or ~5 % faster (~0.9 GB/s).
- Expected (SQLite docs, not measured here): the WAL grows while the copy holds its read transaction, since
  checkpoints cannot pass it; a later auto-checkpoint catches up.
- Hash and fake-upload are ~200 ms at 128 MiB here vs ~47 ms in the first wbbench run
  ([2026-11-05-wbbench.md](2026-11-05-wbbench.md)); same code, so likely machine state (other agents' load).
  Not investigated.

## Request accounting (OS-08 leftovers, same run)

wbbench now prints `reqs` and `$/M` per warm write-back: **4 requests** (`put_gen` + `head` + `get_manifest` +
`publish`), **$10.80 per million write-backs** at S3 Standard list prices (2 class A at $0.005/1000 + 2 class B
at $0.0004/1000). The fake counts in the S3 adapter's request shape; `objstore` counts real attempts, retries
included. `Host.Stats` has `Requests` (by op) and `RequestCostUSD`. agentd already exports
`mansio_s3_requests_total`; cost is not exported as a metric (derive it from the counts).

## Tests

- `TestSnapshotOnAReaderDoesNotBlockWrites` (fake executors): a write commits while the backup is blocked.
- `TestSnapshotRunsOnTheWriterWithoutWALOrReadPool`.
- `TestSnapshotSQLiteIsConsistentUnderConcurrentWrites` (real SQLite): one page per step, 4 writers committing
  pairs `(+x, −x)`; the snapshot passes `integrity_check`, `sum(v) = 0` (one committed state), holds at least
  the reported commits, and no acknowledged write is lost. A second snapshot over the file fails.
