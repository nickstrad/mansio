# Fetch bulkhead on SeaweedFS

How the bulkhead that bounds concurrent S3 fetches must agree with objstore's per-transfer deadline, and why
plain fetches are now bounded by bytes.

## The finding: count-only bounds overrun the transfer deadline

Short runs on the [fetch-by-size](fetch-by-size/README.md) rig (2026-11-05), SeaweedFS at `localhost:8333`
possibly shared with other gated tests. Plain mode (coldload's `s3Fetcher`), which at the time bounded fetches
by **count only**: a plain `Fetcher` did not know the size before it fetched.

| class          | bulkhead         | c=16                  | c=64                  |
| -------------- | ---------------- | --------------------- | --------------------- |
| 16 MiB (n=64)  | default 16       | 14.3 act/s (295 MB/s) | 14.4 act/s (296 MB/s) |
| 128 MiB (n=16) | default 16       | **all 16 failed**     | **all 16 failed**     |
| 128 MiB (n=16) | `max_fetches: 4` | 1.9 act/s (267 MB/s)  | 1.8 act/s (254 MB/s)  |

Before the bulkhead (2026-11-04): 132 MiB at c=16 gave 163 MB/s and c=64 134 MB/s.

With 16 large fetches in flight, every 128 MiB GET hit objstore's own transfer deadline
(`OpTimeout + size/MinThroughput` = 5 s + 4.1 s): 16 × 128 MiB share ~260 MB/s, so each transfer gets ~16
MB/s, below the 32 MiB/s floor. objstore retried each GET (idempotent) and failed again (`S3.REQUEST_RETRIED`,
kind transient).

The rule: the per-transfer throughput floor and the bulkhead must agree. In-flight bytes ≤ aggregate bandwidth
× (OpTimeout + size/MinThroughput). Durable mode's byte cap (1 GiB: 8 × 128 MiB, ~4.3 s on this rig) stays
inside it; plain mode needed a smaller `max_fetches` for large files, or a Fetcher that reports the size
first.

Code and tests: `internal/vactor/host.go`, `internal/vactor/bulkhead_test.go`.

## The fix: plain fetches bounded by bytes

coldload's `s3Fetcher` is now a `vactor.Sizer` (one HEAD, `objstore.Store.Size`). Plain mode then bounds
fetches by bytes as durable mode does (`MaxFetchBytes` 1 GiB ⇒ 8 × 128 MiB in flight, ~4.1 s at ~260 MB/s,
inside the 5 s OpTimeout alone) and grows the activation deadline by size/MinThroughput.

Rerun on the same rig, defaults (`max_fetches` 16), objects already seeded in the `coldload` bucket:

| class          | c=16                           | c=64                           |
| -------------- | ------------------------------ | ------------------------------ |
| 128 MiB (n=16) | 16/16 ok, 1.7 act/s (238 MB/s) | 16/16 ok, 1.6 act/s (225 MB/s) |

No `S3.REQUEST_RETRIED` in the log. Cost: one HEAD per plain activation.

## Config

`max_fetches`, `max_fetch_queue`, `activation_timeout` (default 10 m in coldload) in
`configs/coldload/*.json`.
