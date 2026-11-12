# Generation GC and the generation cache

How old generations and orphans are deleted from the bucket (owner-side retention plus an orphan sweeper), and
how a local cache of passivated files makes a re-warm take milliseconds instead of a download.

Measured 2026-11-05 on a laptop (Apple M5 Max) against single-node SeaweedFS 4.47 at `localhost:8333`, short
runs.

## GC

- Owner-side retention (`vactor.Host.retain`, K = 2 by default via `Options.GC`) deletes the generation the
  owner published K+1 publishes ago, right after each publish, under `e.wb`. One extra DELETE (a batch of 1)
  per write-back in steady state: a write-back is now 5 requests (PUT gen, HEAD, GET manifest, PUT manifest,
  DELETE), not 4.
- The orphan sweeper is `durable.Sweep(ctx, store, key, now, policy, dryRun)`: list the key's prefix (paged),
  read the manifest, skip if the listed version differs from the read one, `PlanGC`, batch delete. It keeps
  the current generation and the K highest-numbered ones below it (it cannot tell published predecessors from
  orphans), and deletes only objects uploaded more than `Grace` (default 10 min) before the manifest's last
  rewrite and more than `Grace` ago. `cmd/dbtool gc` runs it.
- Bucket bytes flat: `TestBucketBytesFlatThroughS3` (30 write-backs of a 64 KiB database on SeaweedFS): ≤ 3
  generations and ≤ 3 × 64 KiB + manifest after every write-back; 27 deleted.
- SeaweedFS: the listed ETag of `manifest.json` equals the GET's ETag (the sweeper's version check), and
  LastModified orders writes 1 s apart (the contract suite sleeps 1.1 s before the publish it compares).

## Generation cache

`Options.GenCacheBytes` > 0: a clean or just-uploaded passivated file moves to `<CacheDir>/gen/<sha256>.db`;
an activation whose manifest names that sha256 and size moves it back. 256 MiB generation,
`TestGenCacheRewarmLatencyS3`:

|                                                            | time        |
| ---------------------------------------------------------- | ----------- |
| cold activation (manifest GET + 256 MiB download + verify) | 0.89–1.17 s |
| re-warm from the cache (manifest GET + rename)             | 1.0–4.0 ms  |
| clean passivation (sha256 of the file + rename)            | ~93 ms      |

Finding: hashing sha256 **and** md5 in one pass (`hashFile`, the upload path, which sends Content-MD5) took
~430 ms for 256 MiB; sha256 alone ~93 ms (arm64 has SHA-2 instructions, MD5 is software). The md5 for
Content-MD5 is ~3/4 of the upload-side hashing cost. Real SQLite (actordb) leaves a clean database's bytes
unchanged across open and close, so clean passivations hit (`TestGenCacheRewarmRealSQLite`).

The cache is emptied with the rest of `CacheDir` at `NewHost`, so it never outlives the process.
