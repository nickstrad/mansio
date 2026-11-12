# Write-back

Warm → cold durability for virtual actors: an agent serves each database from a local SQLite file and writes
it back to S3 as immutable generations named by a conditionally published manifest. Code: `internal/durable`
(manifest, layout, `Store` port, fake + contract suite, GC sweeper), `internal/vactor` (`writeback.go`,
`maintain.go`, `plan.go`), `internal/vactor/actordb`, `internal/objstore/durable.go`, `cmd/dbtool`,
`cmd/bench/wbbench`; hooks `actor.(*Actor).Commits` / `Snapshot`.

- [Durable layout](layout.md): `dbs/<h2>/<escaped key>/`, `gen/<G>-e<E>.db`, self-describing metadata; stamps
  and uploads re-read after ambiguous errors.
- [Snapshot with the backup API](backup-api-snapshot.md): the snapshot runs on a reader, so the write stall
  during a write-back drops from the copy time to ~1 ms; a write-back is 4 S3 requests.
- [Write-back cost](writeback-cost.md): snapshot, hash, upload, passivation and tick cost by database size
  (`wbbench`, `VACUUM INTO` era).
- [Generation GC and the generation cache](gc-and-generation-cache.md): owner-side retention (K = 2) and an
  orphan sweeper keep the bucket flat; a cached re-warm of 256 MiB takes ~2 ms instead of ~1 s.
- [Publishes in doubt](publishes-in-doubt.md): a publish whose outcome was never seen is kept until one of
  ours applies or the grace window passes (`ErrInDoubt`); uploads skip generation numbers orphans took.
- [The version and sequence floor](version-and-sequence-floor.md): `_version` and `seq` are not reissued after
  a rollback (`max(prev+1, seal<<32)`; the seal survives a new generation).
- [S3 compatibility](s3-compatibility.md): `TestS3Compat` and the contract suite on SeaweedFS (single node and
  a 2-filer cluster) and MinIO; conditional PUT is atomic, also across gateways.
- [S3 identities and TLS](s3-identities-and-tls.md): scoped `agent`, `gc` and `bench` identities; agentd needs
  only Read and Write; plain-HTTP S3 needs `-dev`.
- [Storage runbook](storage-runbook.md): `cmd/dbtool` steps for a corrupt generation, a lost or unreadable
  manifest, orphan growth, a cost spike, a SlowDown storm.
- [Backup open issues](backup-open-issues.md): the default 10 s backup interval (kill -9 RPO ~11 s), full-file
  copies, and what would carry over to LiteFS or a similar tool.
- [WAL shipping design](wal-shipping-design.md): segments under a generation, made part of history only by the
  conditional manifest publish. Not built.

## Design in one screen

- **Snapshot: the SQLite online backup API on a reader** (`actor.Snapshot`) when the database is in WAL mode
  with a read pool; otherwise on the writer between batches. It pins one read transaction, so the copy is one
  committed state. `VACUUM INTO` (the earlier choice) fails on a `query_only` connection ("attempt to write a
  readonly database", modernc v1.61.1 / SQLite 3.54.1), so it had to run on the writer and stalled writes for
  the whole copy.
- **Dirty = `actor.Commits() != commits in the published generation`.** The snapshot returns the commit count
  read before it ran, so it holds at least that many: conservative (a spurious extra upload, never a missed
  one).
- **Passivate = close, then upload the closed file if dirty, then delete.** Closing first stops writes even
  through handles callers still hold; the file is then complete (WAL checkpointed; if a WAL is left, reopen
  and snapshot). Failure → a fresh entry over the reopened file (warm again, forced dirty); if the reopen
  fails too, the entry is _stranded_: Gets fail, the file stays, a later Passivate retries. Never deletes the
  only up-to-date copy. `Close` keeps the cache dir if any write-back failed.
- **Publish safety.** Generations are create-only; a number that was tried is never reused (orphans from a
  crash between upload and publish are skipped). The manifest publish is conditional on the version read and
  refused if the stored epoch is higher (`durable.ErrStale`, also the outcome of `CheckPublish`). An ambiguous
  publish (error after it applied) is reconciled by re-reading; a lost reply is adopted on the next
  write-back. Activation below the stored epoch fails (`ErrStale`). Once stale, a dirty database is never
  planned again; `Discard` drops it (fencing decides).
- **RPO**: `Policy.WriteBackEvery` and/or `WriteBackCommits`, run by `Host.Tick` /
  `RunMaintenance(ctx, every, report)`. Crash loss ≤ the trigger + one tick interval + one write-back.
- **Eviction**: pure `PlanTick(now, candidates, policy)`: idle (`now - lastUsed >= IdleAfter`), then LRU until
  `MaxActive` / `MaxBytes` hold; passivations write back first; dirty stale dbs are never planned.
- **Upload integrity**: minio-go does not send a SHA-256 checksum on a streaming-signed PUT, so the host
  verifies size by HEAD and re-hashes every fetch (`ErrCorrupt`).

## Open risks

- Big databases: whole-file uploads on every write-back (see [backup open issues](backup-open-issues.md)).
- The backup-API copy is not compacted: after heavy deletes every generation carries the freelist pages.
- A Tick passivation can race a Get that arrived after planning (the Get then waits and reactivates).
- `Commits` counts only writes made through the actor's write transactions; a write sent as a read
  (`Write: false`) on the write connection (ReadConns = 0) is not counted. `actordb` classifies with
  `actor.IsWrite`.

## Tests

Spec tables (durable, PlanTick, host), fakes with fault switches (`durabletest.Faults`: Down, FailRead,
FailUpload, FailFetch, FailPublish, LosePublishReply, ApplyThenStale, LoseUploadReply, LandLate, Delay;
`vactortest.Opener.FailSnapshot`), contract suite `durabletest.RunStoreSuite` (fake, in-process S3, SeaweedFS
when `COLDLOAD_S3`), `FuzzDurableHostModel`
(Get/Write/Passivate/Crash/Tick/WriteBack/faults/clock/Bump/Discard), `FuzzPlanTick`, -race
`TestConcurrentWriteBackAndPassivateLoseNoAcknowledgedWrite`, SQLite integration in `actordb`.
