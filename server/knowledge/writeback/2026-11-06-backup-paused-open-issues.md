# 2026-11-06 — S3 backup (write-back): paused, open issues

Status: **paused by the user.** The code stays as it is and works (tests, e2e, and the cluster sim pass).
No more work goes into it for now. The user plans to look at LiteFS or a similar tool to replace the
per-database full-file copies. These are the problems to deal with if we keep this design.

## How often a database is backed up today

- On the 1 s maintenance tick (`maintenance.interval`), a database with new writes is backed up once
  `maintenance.write_back_every` (default **10 s**) has passed since its last backup.
- `write_back_commits` (back up after N commits) is off by default.
- A database is also backed up when it goes idle (`idle_after`, default 5 min) and is passivated, and when
  it moves (the fence's final write-back). A graceful move loses nothing.
- Defaults are in `cmd/agentd/config.go`; the shipped durable configs (`configs/agentd/durable-*.json`,
  `configs/soak/agentd.json`) use 10 s. The tests and e2e use 200 ms.
- The local-disk mode (no `s3` section) does no backups. A kill -9 there loses nothing: the SQLite WAL is
  on the same disk. But databases cannot move or be restored on another machine.

## Open issues

1. **Data lost on kill -9 (RPO about 11 s).** After a kill -9, the agent serves each database from its
   last published generation, not from its local file. Writes acknowledged after that backup are lost:
   up to `write_back_every` + one tick + the time the backup takes, and longer while S3 is down.
   Plan §9 (`docs/plans/ir-protocol.md`) earlier quoted "about 170 ms". That figure came from the Jepsen
   test, which sets backups to every 200 ms. It is not the production default.
2. **`_version` reuse after a rollback (data corruption, not only loss).** After the rollback, version
   numbers count up again from the restored generation. A write that checked `expect: {version}` before
   the crash can then pass against a different document.
   - The cluster sim found it: swarm seed 2761, bank total 398 instead of 400.
   - Saved vector: `internal/sim/testdata/open/version-reuse-after-rollback.json`.
   - Replay with `MANSIO_SIM_OPEN=1 go test ./internal/sim -run TestClusterOpenFindings -v`.
   - Possible fix: a version floor per incarnation, e.g. `max(prev+1, seal<<32)`.
   - The bug cannot happen if no acknowledged write is ever rolled back (RPO 0).
   - **Fixed 2026-11-10** (added by the version-fixes run): [the version floor](2026-11-10-version-floor.md).
     The vector moved to `internal/sim/testdata/cluster/`. The commit sequence has the same flaw, still open
     (same note).
3. **Every backup copies the whole file.** A snapshot copies at about 0.8 GB/s, and each backup is a
   full PUT. A 1 GB database backed up every 30 s under steady writes is about 2.8 TB of uploads a day
   (`docs/books/object-storage/plan.md`). So a shorter window costs a lot on large databases, and
   "acknowledge only after the backup is published" cannot work with full copies.
4. **Not built: incremental (WAL) shipping.** It is designed only, in
   [wal-shipping](2026-11-05-wal-shipping.md) and its [amendments](2026-11-05-wal-shipping-amendments.md).
   Litestream and LiteFS do this work.
5. **Restart throws the local file away.** Even when the disk survives a process crash, a restart
   restores from S3. Reusing the local file when it is safe (same VM, same epoch, no newer owner) would
   remove most kill -9 loss without changing S3. This needs fencing checks.
6. **Sim gaps** (`server/knowledge/determinism/2026-11-06-dst2-dst3-cluster-sim.md`):
   - Storms still excuse RPO losses on every database: 2990 excused losses in 115 of 2000 swarm runs.
   - Not exercised: group commit, replica lag, controld crashes, clock skew.
   - No CI job runs the nightly swarm.

## If we switch to LiteFS (or similar)

What may carry over:
- epochs and fencing (`internal/owner`): one writer per database;
- placement and controld;
- the edge and locator routing;
- the sim and its checker, which needs only the RPO parameter and an S3 fake to change.

What it would replace:
- `internal/durable`;
- the vactor write-back;
- generations, GC and `dbtool`;
- most of this folder.

Check first:
- LiteFS's own failover rules against our epochs. Two fencing systems must not disagree.
- Its RPO on kill -9.
- Whether it runs under one SQLite file per database at our scale (thousands of databases per VM).
