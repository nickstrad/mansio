# writeback — warm → cold durability for virtual actors

Owner: writeback agent (state: `STATE-writeback.md`, archived to `docs/state_files/` when done).
Code: `internal/durable` (manifest, layout, `Store` port, fake + contract suite), `internal/vactor`
(`writeback.go`, `maintain.go`, `plan.go`), `internal/vactor/actordb`, `internal/objstore/durable.go`,
`cmd/wbbench`; hooks `actor.(*Actor).Commits` / `Snapshot`.

## Findings (newest first)

- 2026-11-05 (CP-8): [snapshot with the backup API](2026-11-05-backup-api-snapshot.md): on a reader in WAL
  mode, the write stall during a write-back drops from the snapshot time (149 ms at 128 MiB, 589 ms at 512 MiB)
  to ~1 ms; wbbench prints requests (4) and $10.80 per million write-backs (OS-08 leftovers).
- 2026-11-05 (CP-8): [TestS3Compat on MinIO and multi-node SeaweedFS](2026-11-05-s3-compat-minio-and-cluster.md):
  MinIO passes X1–X12; a 2-filer SeaweedFS cluster on a shared Postgres store passes, conditional PUT atomic
  across gateways (1200+ contended rounds, the peer gateway won about half).
- 2026-11-05 (books p4-gc): [storage runbook](2026-11-05-storage-runbook.md) with `cmd/dbtool` (inspect, verify, orphans, gc, rebuild-manifest) for corrupt generation, lost/unreadable manifest, orphan growth, cost spike, SlowDown storm (OS-12).
- 2026-11-05 (books p4-gc): [generation GC and the generation cache](2026-11-05-gc-and-gen-cache.md): owner-side retention (K = 2) + orphan sweeper keep the bucket flat on SeaweedFS; a cached re-warm of 256 MiB takes ~2 ms instead of ~1 s; md5 is ~3/4 of upload hashing (OS-06, OS-10).
- 2026-11-05 (books p2-storage): [layout v2 + ambiguity resolution](2026-11-05-layout-v2.md): `dbs/<h2>/<escaped key>/`, `gen/<G>-e<E>.db`, self-describing metadata; stamps and uploads re-read after ambiguous errors (OS-02, OS-04, OS-05).
- 2026-11-05 (coordinator): [S3 conditional PUT verified on SeaweedFS](2026-11-05-s3-conditional-put-verified.md): full contract suite passes; 200 × 8 concurrent `If-Match` publishes → exactly one winner each round. Single node only.
- 2026-11-05 [wbbench](2026-11-05-wbbench.md): VACUUM INTO snapshot ~0.8 GB/s and it **blocks that
  database's writes for its whole duration** (512 MiB: 640 ms stall); passivation uploads the closed file,
  ~2× cheaper; sha256 ~2.9 GB/s; dirty check 0.28 ns; a tick ~2.4 µs per warm db. Fake store (Docker down).
- 2026-11-05 [WAL shipping design note](2026-11-05-wal-shipping.md): segments under a generation, made part
  of history only by the conditional manifest publish; compaction = today's write-back. Not built.
- 2026-11-05 **SeaweedFS conditional PUT: documented, unverified.** Its wiki ("S3 Conditional Operations")
  lists `If-Match` / `If-None-Match` on PUT and says "the conditional check and the write are atomic —
  cluster-wide" (no version given; our compose file uses `chrislusf/seaweedfs:latest`). Not run: Docker was
  down. `TestS3DurableStoreContract` (incl. a concurrent-publish test) is the check:
  `COLDLOAD_S3=localhost:8333 go test ./internal/objstore -run S3`.
- 2026-11-05 minio-go `AutoChecksum: ChecksumSHA256` does **not** add `x-amz-checksum-sha256` to a single
  streaming-signed PUT (seen with the in-process fake S3). So the store does not verify the hash on upload;
  the host verifies size by HEAD and re-hashes every fetch (`ErrCorrupt`).
- 2026-11-05 `VACUUM INTO` fails on a `query_only` connection ("attempt to write a readonly database"),
  so the snapshot must run on the actor's writer (modernc v1.61.1 / SQLite 3.54.1).

## Design in one screen

- **Superseded 2026-11-05 (CP-8):** the snapshot now uses the backup API on a reader ([note](2026-11-05-backup-api-snapshot.md)).
- **Snapshot choice: `VACUUM INTO` on the actor goroutine** (`actor.Snapshot`). Plain SQL, no driver
  internals; it is a consistent read transaction by construction, and because the actor serializes writes
  it sits between two batches: no write is lost, queued writes just wait. Rejected for now:
  `wal_checkpoint(TRUNCATE)` + file copy (blocks the writer for the copy, and TRUNCATE waits for readers);
  backup API (faster and could run on a reader, but needs the raw driver conn: a bigger actor hook; it is
  the next step for big databases).
- **Dirty = `actor.Commits() != commits in the published generation`.** The snapshot returns the commit
  count read before it ran, so it holds at least that many: conservative (a spurious extra upload, never a
  missed one).
- **Passivate = close, then upload the closed file if dirty, then delete.** Closing first stops writes even
  through handles callers still hold; the file is then complete (WAL checkpointed; if a WAL is left, reopen
  and snapshot). Failure → a fresh entry over the reopened file (warm again, forced dirty); if the reopen
  fails too, the entry is *stranded*: Gets fail, the file stays, a later Passivate retries. Never deletes
  the only up-to-date copy. `Close` keeps the cache dir if any write-back failed.
- **Publish safety.** Generations are create-only; a number that was tried is never reused (orphans from a
  crash between upload and publish are skipped). The manifest publish is conditional on the version read
  and refused if the stored epoch is higher (`durable.ErrStale`, also the outcome of `CheckPublish`). An
  ambiguous publish (error after it applied) is reconciled by re-reading; a lost reply is adopted on the
  next write-back. Activation below the stored epoch fails (`ErrStale`). Once stale, a dirty database is
  never planned again; `Discard` drops it (fencing decides).
- **RPO**: `Policy.WriteBackEvery` and/or `WriteBackCommits`, run by `Host.Tick` /
  `RunMaintenance(ctx, every, report)`. Crash loss ≤ the trigger + one tick interval + one write-back.
- **Eviction**: pure `PlanTick(now, candidates, policy)`: idle (`now - lastUsed >= IdleAfter`), then LRU
  until `MaxActive` / `MaxBytes` hold; passivations write back first; dirty stale dbs are never planned.

## Open risks

- No GC: superseded generations and orphans accumulate in the bucket.
- Big databases: the VACUUM stall (above) and whole-file uploads; see the next steps in wbbench.
- A Tick passivation can race a Get that arrived after planning (the Get then waits and reactivates).
- SeaweedFS conditional-put atomicity unverified (above).
- `Commits` counts only writes made through the actor's write transactions; a write sent as a read
  (`Write: false`) on the write connection (ReadConns = 0) is not counted. `actordb` classifies with
  `actor.IsWrite`.

## Tests

Spec tables (durable, PlanTick, host), fakes with fault switches (`durabletest.Faults`: Down, FailRead,
FailUpload, FailFetch, FailPublish, LosePublishReply, Delay; `vactortest.Opener.FailSnapshot`), contract suite
`durabletest.RunStoreSuite` (fake, in-process S3, SeaweedFS when `COLDLOAD_S3`), `FuzzDurableHostModel`
(Get/Write/Passivate/Crash/Tick/WriteBack/faults/clock/Bump/Discard), `FuzzPlanTick`, -race
`TestConcurrentWriteBackAndPassivateLoseNoAcknowledgedWrite`, SQLite integration in `actordb`.
