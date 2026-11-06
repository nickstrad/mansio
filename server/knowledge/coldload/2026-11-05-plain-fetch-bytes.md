# 2026-11-05 — plain fetches bounded by bytes (follow-up to the RI-02 bulkhead note)

By the p4-gc agent. Follows [the RI-02 bulkhead note](2026-11-05-ri02-bulkhead.md), where 16 plain 128 MiB
fetches all overran objstore's per-transfer deadline (OpTimeout + size/MinThroughput) because a plain fetch
counted 0 bytes in the bulkhead.

Fix: coldload's `s3Fetcher` is now a `vactor.Sizer` (one HEAD, `objstore.Store.Size`). Plain mode then
bounds fetches by bytes as durable mode does (`MaxFetchBytes` 1 GiB ⇒ 8 × 128 MiB in flight, ~4.1 s at
~260 MB/s, inside the 5 s OpTimeout alone) and grows the activation deadline by size/MinThroughput.

Rerun, same rig, defaults (`max_fetches` 16), objects already seeded in the `coldload` bucket:

| class | c=16 | c=64 |
|---|---|---|
| 128 MiB (n=16) | 16/16 ok, 1.7 act/s (238 MB/s) | 16/16 ok, 1.6 act/s (225 MB/s) |

No `S3.REQUEST_RETRIED` in the log. Cost: one HEAD per plain activation.
