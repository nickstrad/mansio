# coldload

Owner: coldload agent (state: `../../docs/state_files/STATE-coldload.md, archived`).

How quickly one Go process can fetch a SQLite file from SeaweedFS (S3 API), open it as an actor
(`internal/actor`, via `internal/vactor`), and answer its first query, by file size and activation concurrency.
Scope: object storage → ready actor only. Running actors (how many per process, per-database cost, request
load) belong to `../sqlbench/`.

## Findings (newest first)

- 2026-11-04 [sizes/](2026-11-04-sizes/README.md) (corrected rerun, one GET per fetch): download is 69–99% of
  time-to-ready; open + first query < 1 ms at any size; ready ≈ 1.2 ms + size ÷ ~250 MB/s (c = 1). Total
  bandwidth caps at ~250–265 MB/s, so concurrency past ~4 only queues for files ≥ 1 MiB; 64 KiB peaks at
  ~3,100 activations/s (c = 16). 500 callers on one cold db → 1 fetch. 1 GiB ~40 MB/s (Docker VM RAM).

How to run and clean up: `make help` (coldload section), `make coldload-bench CONFIG=sizes`.
Testing approach used here: `../testing/`; error handling and logging: `../concurrency-in-go/`.
