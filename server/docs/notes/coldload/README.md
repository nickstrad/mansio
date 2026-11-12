# Cold load

How quickly one Go process can fetch a SQLite file from SeaweedFS (S3 API), open it as an actor
(`internal/actor`, via `internal/vactor`), and answer its first query, by file size and activation
concurrency. Scope: object storage → ready actor only. Running actors (how many per process, per-database
cost, request load) belong to [sqlbench](../sqlbench/README.md). Code: `cmd/bench/coldload`,
`internal/coldload`, `internal/vactor`, `internal/objstore`; configs in `configs/coldload/`.

- [SQLite file in SeaweedFS → ready virtual actor, by size and concurrency](fetch-by-size/README.md): ready
  time and throughput per file size and concurrency, with the run's report and CSV.
- [Fetch bulkhead on SeaweedFS](fetch-bulkhead.md): why the fetch bulkhead must bound bytes in flight to stay
  inside objstore's per-transfer deadline, and the plain-mode fix.

How to run and clean up: `make help` (coldload section), `make coldload-bench CONFIG=sizes`. Testing approach
used here: [testing](../testing/README.md); error handling and logging:
[concurrency in Go](../concurrency-in-go/README.md).

## Key numbers

Measured 2026-11-04 on an 18-core Apple Silicon Mac, single-node SeaweedFS in Docker Desktop (one GET per
fetch):

- Download is 69–99% of time-to-ready; open + first query < 1 ms at any size; ready ≈ 1.2 ms + size ÷ ~250
  MB/s (c = 1).
- Total bandwidth caps at ~250–265 MB/s, so concurrency past ~4 only queues for files ≥ 1 MiB; 64 KiB peaks at
  ~3,100 activations/s (c = 16).
- 500 callers on one cold database → 1 fetch.
- 1 GiB at ~40 MB/s is a limit of the Docker VM's RAM, not of the design.
