# 2026-11-05 — design note: WAL shipping on top of generations (not built)

Amended (OS-13): [WAL-shipping amendments](2026-11-05-wal-shipping-amendments.md) win where they disagree
(epoch-qualified segment names, header/footer, no foreign-epoch adoption, HEAD probe instead of LIST, GC).

Author: writeback agent. Status: design only; out of scope for code in this workstream. Nothing here is
measured.

## Why

Generations bound the crash loss (RPO) by `Policy.WriteBackEvery` / `WriteBackCommits` plus one tick, and
each write-back copies the whole database (VACUUM INTO ~0.8 GB/s, see [wbbench](2026-11-05-wbbench.md)). WAL
shipping (Litestream's idea) would cut the RPO to seconds and the cost to the bytes written.

## Layout (additive to contract 1)

```
dbs/<key>/gen/<G>.db                       base snapshot (as today)
dbs/<key>/gen/<G>/wal/<seq 20 digits>.wal  WAL segments written after generation G, in order
dbs/<key>/manifest.json                    + "wal": {"generation": G, "through": <seq>, "salt": ...}
```

- A segment is a run of whole WAL frames copied from `-wal` between two checkpoints of our own, with the WAL
  header salt so a restore can tell a segment from an older WAL incarnation. Segments are create-only
  (`If-None-Match: *`), like generations.
- The manifest names the base generation and the last segment that is *durable*. Restore = fetch base +
  segments `1..through`, apply in order (write them as the WAL and let SQLite recover), open.

## How it layers on what exists

- **Ownership stays in the manifest.** Segments are uploaded at will, but they count only once a manifest
  publish (conditional, epoch-checked, same `ErrStale` rules) moves `through`. A zombie can upload segments
  under a generation; it can never make them part of the history. Fencing's `Stamp` works unchanged.
- **Who copies frames.** The actor goroutine already serializes commits. A hook (additive, like `Commits`)
  would disable SQLite's automatic checkpoint (`wal_autocheckpoint=0`), and after each group commit report
  the WAL size; a shipper goroutine copies the new frames `[old size, new size)` from `-wal` (readers do not
  change the WAL). Checkpoints happen only when the shipper says the frames are uploaded, so no frame is
  dropped from the WAL before it is safe elsewhere.
- **Compaction = today's write-back.** When the segments under G pass a size or count, write back a new
  generation G+1 (snapshot), publish a manifest with `wal: {generation: G+1, through: 0}`, and garbage-collect
  `gen/<G>/wal/` later.
- **Acknowledgement modes.** (a) ack on local commit, RPO = shipping lag (Litestream); (b) ack after the
  segment holding the commit is uploaded and the manifest moved (synchronous; one PUT per batch: group commit
  amortizes it). (b) needs the actor to hold replies until the shipper confirms: a bigger change.

## Risks to settle before building

- Manifest publish per segment is one conditional PUT per batch: a write rate cap per database. Publishing
  `through` every N segments trades RPO for PUTs; the segments after `through` can be found by LIST on
  restore only if we accept LIST consistency (S3 is strongly consistent since 2020; SeaweedFS: unverified).
- Restore time grows with segment count until compaction; bound it by bytes.
- Garbage collection of superseded generations and segments does not exist yet (also true today: old
  generations and orphans accumulate; see the README's open risks).
