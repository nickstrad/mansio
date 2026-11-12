# 2026-11-05 — WAL shipping: amendments before any code (object-storage plan OS-13)

By the p4-gc agent. Docs only. Amends [the WAL-shipping design note](2026-11-05-wal-shipping.md); where they
disagree, this note wins. The rules it applies are in [object-storage.md](../../object-storage.md).

## 1. Segment names carry the epoch (K4, I2, GC6)

```
dbs/<h2>/<esc key>/gen/<G 20d>-e<E 20d>.db                              base snapshot (layout v2)
dbs/<h2>/<esc key>/gen/<G 20d>-e<E 20d>/wal/<seq 20d>-e<W 20d>.wal      segment seq after base G, written at epoch W
```

- Segments live under the base generation's full object name (`GenObject` without `.db`), so dropping or
  GC'ing a generation reaches its segments by prefix, and two owners' bases never share a segment prefix.
- `W` is the epoch of the owner that uploaded the segment (not the base's epoch: a stamp raises the
  manifest's epoch without renaming the base). Two owners can never collide on a segment name.
- Create-only (`If-None-Match: *`), Content-MD5, user metadata `sha256, key, generation, epoch, seq,
  format=sqlite-wal-v1`, like generations (I1, I4, V2). Never reuse a tried `seq`: an ambiguous upload is
  resolved by HEAD and the sha256 metadata (R3), as `PutGeneration` is.
- `durable.ParseGenObject` and the sweeper's `PlanGC` already keep every name under `gen/` they cannot parse
  as a generation, so segments are safe from today's GC until it learns them (§5).

## 2. Segment format: header and footer (V3)

- Header (fixed size): magic `TSDBWAL1`, format version, key (length-prefixed), base generation, base epoch,
  writer epoch `W`, start sequence (first frame's index in the WAL since the base), the SQLite WAL salt-1 and
  salt-2 and page size.
- Body: whole WAL frames, as copied from `-wal`.
- Footer: end sequence (exclusive), frame count, CRC32C over header + body.
- A reader rejects a segment whose magic, key, base, epoch or sequence disagree with its name or with the
  previous segment, whose CRC fails, or whose salt is not the salt the manifest records for that base.

## 3. Restore never adopts a stranger's segment (GC6, safety argument S2)

- History is only what the manifest names: base `G` plus segments `1..through`. Restore applies exactly
  those, each verified (§2), and stops at the first failure (corruption is a page, not a skip).
- Segments past `through` are **not** history. The book's "adopt valid orphans" recovery (Ch 5) is allowed
  in one case only: the same process (same writer epoch, same in-memory WAL incarnation) after a restart of
  its shipper, before it reconciles with the manifest, and only if continuity (start = previous end) and CRC
  pass. A segment with any other epoch is never adopted, even if valid: a zombie may have written it after
  the new owner's stamp.
- A new owner stamps first (fencing), then publishes from what the manifest names; it starts its own
  segments at `through + 1` with its own epoch in the name.

## 4. Shipping trigger and discovery

- Ship a segment on whichever comes first: a size trigger (4–16 MiB for slow streams, Ch 11) or a maximum
  ship interval (the RPO target, e.g. 1 s). Publish `through` on the same triggers or every N segments; the
  trade is RPO against one conditional manifest PUT per publish (G7 cost: ~2 PUTs per segment).
- Find segments past `through` (the same process after a restart, §3) with a computed-key HEAD probe:
  `seq+1`, `seq+2`, … until a 404. No LIST on any request or restore path (K2). LIST stays for GC and dbtool.

## 5. GC and retention for segments (GC1–GC5)

- A compaction (today's write-back) publishes base `G+1` with `through = 0`. The segments under the old
  base are then predecessors: kept while their base is among the K retained, deleted with it. The sweeper
  deletes a base's segment prefix only when it would delete the base itself, and orphan segments past
  `through` by the GC3 rule (uploaded more than grace before the manifest's last rewrite, and more than grace
  ago).
- Owner-side retention deletes the segment prefix of the base it drops (one LIST of that prefix, GC-only; or
  the shipper's own record of the sequences it wrote, to stay LIST-free).
- `dbtool verify` checks the segments `1..through` too; `inspect` lists them per base.

## 6. Contract and compatibility

- A layout change: a contract-suite case per rule above (segment create-only, metadata round trip, HEAD probe
  stops at 404, restore refuses a foreign-epoch segment), and the S3 compat checks X1, X4, X7 re-run for the
  segment PUT path (K5).
