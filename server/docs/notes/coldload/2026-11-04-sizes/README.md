# 2026-11-04 — SQLite file in SeaweedFS → ready virtual actor, by size and concurrency

Run: `configs/coldload/sizes.json` (warm-up = full scan of `items`), rerun 2026-11-04 ~20:10 after the
/simplify refactor (one GET per fetch, real copy buffer). `make coldload-bench CONFIG=sizes` reproduces it.
Artifacts in this folder: `report-sizes.txt` (DuckDB report), `levels-sizes.csv`, `sizes.json`.
Older run with the first fetch path (HEAD + GET, see "Correction" below): `*-v1-head-get.*`, `sizes-nowarm.json`.
Raw `activations.csv` was in `results/coldload/sizes/` (kept locally, ~0.5 MB).

Machine: Apple Silicon, 18 cores, 128 GB, macOS. SeaweedFS `chrislusf/seaweedfs:latest`, one container
(master + volume + filer + S3) in Docker Desktop, VM limit **3.8 GiB RAM**. Go 1.26.4, `modernc.org/sqlite`,
actor from `internal/actor` (one goroutine per database, WAL, `read_conns` 0). Objects are byte-identical copies
of one template per class; actual sizes 64 KiB, 1.18 MiB, 19.7 MiB, 132 MiB, 1 GiB.

"Ready" = S3 GET → local file → `actor.Start` → first point query answered → warm-up scan.

## Results

Ready time per activation at c = 1 (p50, ms), with the share spent downloading:

| file | TTFB | fetch | open | first query | full-scan warm | ready | fetch % |
|---|---|---|---|---|---|---|---|
| 64 KiB | 0.51 | 0.84 | 0.30 | 0.026 | 0.04 | **1.21** | 69 |
| 1.2 MiB | 1.0 | 5.5 | 0.32 | 0.027 | 0.35 | **6.2** | 89 |
| 19.7 MiB | 2.6 | 80 | 0.47 | 0.047 | 5.4 | **86** | 93 |
| 132 MiB | 3.0 | 511 | 0.48 | 0.049 | 29 | **541** | 95 |
| 1 GiB | 8.7 | 23,455 | 0.51 | 0.053 | 215 | **23,672** | 99 |

Throughput by concurrency (activations/s, total MB/s in brackets):

| file | c=1 | c=4 | c=16 | c=64 | c=256 |
|---|---|---|---|---|---|
| 64 KiB | 791/s | 2,769/s | **3,142/s** | 2,972/s | 1,129/s |
| 1.2 MiB | 160/s (200) | **206/s (257)** | 198/s (247) | 200/s (249) | 189/s (236) |
| 19.7 MiB | 11.8/s (243) | 12.8/s (263) | **12.9/s (265)** | 11.6/s (239) | 10.2/s (211) |
| 132 MiB | **1.83/s (253)** | 1.65/s (227) | 1.18/s (163) | 0.97/s (134) | 1.03/s (142) |
| 1 GiB | **0.04/s (46)** | 0.04/s (38) | — | — | — |

p50 / p99 ready under concurrency: 1.2 MiB 6 / 7 ms (c=1) → 23 / 585 ms (c=16) → 1.2 / 2.1 s (c=256);
19.7 MiB 86 / 92 ms (c=1) → 9.4 / 9.8 s (c=256). 4,609 activations, 0 errors, 0 records dropped.

## Findings

1. **Download time is almost all of the ready time** (69% for 64 KiB, ≥ 93% from 16 MiB up). Opening the database
   (actor goroutine + connection, WAL pragma) costs 0.3–0.5 ms and the first point query 0.03–0.05 ms at every
   size. At c = 1, ready ≈ 1.2 ms + size ÷ ~250 MB/s.
2. **Warm-up (full scan) is cheap right after a download**: ~0.21–0.27 ms per MiB (the file is in the OS page
   cache because we just wrote it); ≤ 6% of ready time.
3. **This rig tops out at ~250–265 MB/s in total.** For files ≥ 1 MiB, concurrency past ~4 adds no throughput; it
   only queues (p99 for 1.2 MiB: 7 ms at c=1, 585 ms at c=16). For 132 MiB, c ≥ 16 drops total throughput to
   ~135–165 MB/s.
4. **Small files are latency-bound**: 64 KiB peaks at ~3,100 activations/s at c = 16 and falls to ~1,100/s at
   c = 256 (p99 144 ms).
5. **1 GiB files are slow here (~40–46 MB/s) because of the Docker VM**: 3.8 GiB RAM cannot cache 1 GiB volume
   files, so SeaweedFS reads from the VM's disk image (container at ~1700% CPU). A test-rig limit.
6. **One activation per key works**: 500 concurrent callers for one cold 1 GiB database → 1 fetch; all 500 got
   the database after 26.1 s.

## Correction (first run, HEAD + GET)

The first version called minio's `GetObject` then `Stat()`, which sends a HEAD before the GET; the "TTFB" column
measured the HEAD, and each fetch paid two round trips. Its copy buffer was also bypassed (`*os.File.ReadFrom`
copies 32 KiB at a time). Fixed by `minio.Core.GetObject` (one GET, headers → TTFB) and a plain read/write loop.
Effect: 64 KiB ready 1.82 → 1.21 ms at c=1 and 539 → 791 activations/s; peak 2,963 → 3,142/s. Files ≥ 1 MiB:
unchanged within noise (bandwidth-bound). The no-warm-up comparison (`*-v1-head-get.*`) was not rerun; its
conclusion (warm-up ≤ 6% of ready) does not depend on the fetch path.

## What this means for a design

- Bound concurrent downloads by **bytes in flight**, not by count: ~4 large fetches saturate the link; dozens of
  small ones are fine. Without a bound, a burst of cold large tenants makes every cold tenant wait.
- For big databases avoid the full download before the first query (a local disk cache that survives
  passivation — `keep_local` — or a page-level lazy-fetch VFS). Not yet measured.
- Open + first query are not worth optimizing; fetch is.

## Limits of this test

- Single-node SeaweedFS in Docker Desktop on macOS, loopback network: real S3 / a SeaweedFS cluster has higher
  TTFB (tens of ms) but scales total bandwidth with connections. Treat absolute MB/s as this rig's numbers.
- Objects in a class are identical copies; SeaweedFS and the OS may cache them (no cache drop between levels).
- Ready excludes any sustained query load after activation (that is sqlbench's area).
