# 2026-11-05 — SeaweedFS conditional PUT: verified (coordinator note)

Before this, the writeback and fencing reports marked S3 conditional-PUT atomicity _unverified_, because
Docker was down. Now verified against a real SeaweedFS.

- **Rig:** `make coldload-up` (single SeaweedFS container, `chrislusf/seaweedfs:latest` = SeaweedFS 4.47, Docker 29.4.1, Docker
  Desktop on macOS). Apple M5 Max. `main` at `699d7e5`.
- **Full contract suite:**
  `COLDLOAD_S3=localhost:8333 go test -count=1 -race -v -run 'S3|Objstore|Durable' ./internal/objstore ./cmd/bench/coldload`.
  All passed:
  - `TestS3DurableStoreContract`, all 15 cases, including create-only generations (`If-None-Match: *`),
    publish against a moved version giving `ErrStale`, a lower epoch giving `ErrStale`, and corrupt-fetch
    detection;
  - `TestS3FetcherContract`, `TestS3StoreDownIsWrapped`.
- **Atomicity stress:**
  `go test -count=200 -run 'TestS3DurableStoreContract/concurrent_publishes' ./internal/objstore`. That is 200
  rounds of 8 concurrent `If-Match` publishes on one manifest version. Each round had **exactly one winner and
  7 `ErrStale`**. Run under the bench lock.

What this does and doesn't show:

- It shows a single-node SeaweedFS honours `If-Match` / `If-None-Match` atomically under concurrency, which is
  what the fencing safety argument's storage backstop needs (`docs/notes/fencing/2026-11-05-safety-argument.md`).
- It doesn't cover a multi-node SeaweedFS cluster (the docs say the check is atomic cluster-wide; not tested),
  AWS S3, or failover during a publish.
