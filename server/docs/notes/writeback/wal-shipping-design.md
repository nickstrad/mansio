# WAL shipping on top of generations (design, not built)

A design for shipping SQLite WAL segments to S3 between full write-backs, so the crash loss shrinks to seconds
and the cost to the bytes written. Not implemented: nothing here is built or measured.

## Why

Generations bound the crash loss (RPO) by `Policy.WriteBackEvery` / `WriteBackCommits` plus one tick, and each
write-back copies the whole database (~0.8–0.9 GB/s, see [write-back cost](writeback-cost.md)). WAL shipping
(Litestream's idea) would cut the RPO to seconds and the cost to the bytes written.

## Layout

Additive to the [durable layout](layout.md):

```
dbs/<h2>/<esc key>/gen/<G 20d>-e<E 20d>.db                              base snapshot (as today)
dbs/<h2>/<esc key>/gen/<G 20d>-e<E 20d>/wal/<seq 20d>-e<W 20d>.wal      segment seq after base G, written at epoch W
dbs/<h2>/<esc key>/manifest.json                                        + "wal": {"generation": G, "through": <seq>, "salt": ...}
```

- Segments live under the base generation's full object name (`GenObject` without `.db`), so dropping or
  GC'ing a generation reaches its segments by prefix, and two owners' bases never share a segment prefix.
- `W` is the epoch of the owner that uploaded the segment (not the base's epoch: a stamp raises the manifest's
  epoch without renaming the base). Two owners can never collide on a segment name.
- A segment is a run of whole WAL frames copied from `-wal` between two checkpoints of our own. Segments are
  create-only (`If-None-Match: *`) with Content-MD5 and user metadata
  `sha256, key, generation, epoch, seq,
  format=sqlite-wal-v1`, like generations. A tried `seq` is never
  reused: an ambiguous upload is resolved by HEAD and the sha256 metadata, as `PutGeneration` is.
- The manifest names the base generation, the last segment that is _durable_ (`through`), and the WAL salt for
  that base. Restore = fetch base + segments `1..through`, apply in order (write them as the WAL and let
  SQLite recover), open.
- `durable.ParseGenObject` and the sweeper's `PlanGC` already keep every name under `gen/` they cannot parse
  as a generation, so segments are safe from today's GC until it learns them.

## Segment format

- Header (fixed size): magic `TSDBWAL1`, format version, key (length-prefixed), base generation, base epoch,
  writer epoch `W`, start sequence (first frame's index in the WAL since the base), the SQLite WAL salt-1 and
  salt-2 and page size.
- Body: whole WAL frames, as copied from `-wal`.
- Footer: end sequence (exclusive), frame count, CRC32C over header + body.
- A reader rejects a segment whose magic, key, base, epoch or sequence disagree with its name or with the
  previous segment, whose CRC fails, or whose salt is not the salt the manifest records for that base.

## Ownership stays in the manifest

- Segments are uploaded at will, but they count only once a manifest publish (conditional, epoch-checked, same
  `ErrStale` rules) moves `through`. A zombie can upload segments under a generation; it can never make them
  part of the history. Fencing's `Stamp` works unchanged.
- History is only what the manifest names: base `G` plus segments `1..through`. Restore applies exactly those,
  each verified, and stops at the first failure (corruption is a page, not a skip).
- Segments past `through` are **not** history. Adopting valid orphan segments is allowed in one case only: the
  same process (same writer epoch, same in-memory WAL incarnation) after a restart of its shipper, before it
  reconciles with the manifest, and only if continuity (start = previous end) and CRC pass. A segment with any
  other epoch is never adopted, even if valid: a zombie may have written it after the new owner's stamp.
- A new owner stamps first (fencing), then publishes from what the manifest names; it starts its own segments
  at `through + 1` with its own epoch in the name.

## Who copies frames

The actor goroutine already serializes commits. A hook (additive, like `Commits`) would disable SQLite's
automatic checkpoint (`wal_autocheckpoint=0`), and after each group commit report the WAL size; a shipper
goroutine copies the new frames `[old size, new size)` from `-wal` (readers do not change the WAL).
Checkpoints happen only when the shipper says the frames are uploaded, so no frame is dropped from the WAL
before it is safe elsewhere.

## Shipping trigger and discovery

- Ship a segment on whichever comes first: a size trigger (4–16 MiB) or a maximum ship interval (the RPO
  target, e.g. 1 s). Publish `through` on the same triggers or every N segments; the trade is RPO against one
  conditional manifest PUT per publish (~2 PUTs per segment).
- Find segments past `through` (the same process after a restart, above) with a computed-key HEAD probe:
  `seq+1`, `seq+2`, … until a 404. No LIST on any request or restore path; LIST stays for GC and dbtool.

## Compaction and GC

- **Compaction = today's write-back.** When the segments under G pass a size or count, write back a new
  generation G+1 (snapshot) and publish a manifest with `wal: {generation: G+1, through: 0}`. The segments
  under the old base are then predecessors: kept while their base is among the K retained, deleted with it.
- The sweeper deletes a base's segment prefix only when it would delete the base itself, and orphan segments
  past `through` by the usual orphan rule (uploaded more than the grace before the manifest's last rewrite,
  and more than the grace ago).
- Owner-side retention deletes the segment prefix of the base it drops (one LIST of that prefix, GC-only; or
  the shipper's own record of the sequences it wrote, to stay LIST-free).
- `dbtool verify` checks the segments `1..through` too; `inspect` lists them per base.

## Acknowledgement modes

(a) Ack on local commit, RPO = shipping lag (Litestream). (b) Ack after the segment holding the commit is
uploaded and the manifest moved (synchronous; one PUT per batch: group commit amortizes it). (b) needs the
actor to hold replies until the shipper confirms: a bigger change, and it puts an S3 PUT on the write-ack
path. What to do under an open S3 breaker (refuse writes, or ack locally with a larger RPO) is not decided.

## Contract and compatibility

A layout change needs a contract-suite case per rule above (segment create-only, metadata round trip, HEAD
probe stops at 404, restore refuses a foreign-epoch segment), and the S3 compat checks X1, X4, X7 re-run for
the segment PUT path ([S3 compatibility](s3-compatibility.md)).

## Risks

- Manifest publish per segment is one conditional PUT per batch: a write rate cap per database. Publishing
  `through` every N segments trades RPO for PUTs.
- Restore time grows with segment count until compaction; bound it by bytes.
