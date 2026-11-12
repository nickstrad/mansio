# 2026-11-05 — TestS3Compat on MinIO and on multi-node SeaweedFS (CP-8, OS X)

Author: CP-8 agent. Docker Desktop on macOS, Apple M5 Max. Client minio-go v7 with `MaxRetries: 1` (every
answer is the server's first). Single-node SeaweedFS 4.47 results:
[2026-11-05-s3-compat-seaweedfs.md](2026-11-05-s3-compat-seaweedfs.md).

## MinIO — all pass, X12 included

- **Image:** `bitnamilegacy/minio:latest` → `minio version DEVELOPMENT.2025-05-24T17-08-30Z` (go1.24.2,
  linux/arm64). `minio/minio` and `quay.io/minio/minio` refused anonymous pulls on 2026-11-05 (MinIO stopped
  publishing community images); Bitnami's frozen legacy image is the MinIO server binary. Single node, one
  drive.
- **Rig:** `docker run -d --name mansio-minio -p 9010:9000 -e MINIO_ROOT_USER=coldload
  -e MINIO_ROOT_PASSWORD=coldload-secret bitnamilegacy/minio:latest` (left running).
- **Scoped identity for X12:** `mc admin user add local mansio-agent mansio-agent-secret` with a policy allowing
  only `s3:GetObject` and `s3:PutObject` on `arn:aws:s3:::coldload/*`.
- **Command:** `COLDLOAD_S3=localhost:9010 S3_BACKEND=minio-2025-05-24 go test -count=1 -v -run TestS3Compat
  ./internal/objstore` (X12 run with `S3_AGENT_ACCESS_KEY=mansio-agent S3_AGENT_SECRET_KEY=mansio-agent-secret`).

| # | Result | Detail |
|---|---|---|
| X1 | pass | second create → 412 |
| X2 | pass | current ETag → 200, stale ETag → 412 |
| X3 | pass | 200 rounds × 8 If-Match writers: exactly 1 winner each, stored bytes = winner's (15.4 s; SeaweedFS: 0.4 s) |
| X4 | pass | 50 overwrites: HEAD and GET right after each PUT see its ETag and bytes |
| X5 | pass | 20 distinct overwrites → 20 ETags; identical bytes repeat the ETag (MD5) |
| X6 | pass | wrong MD5 → BadDigest (permanent), nothing stored; right MD5 → 200 |
| X7 | pass | sha256, epoch, generation, key on HEAD and GET |
| X8 | pass | Code PreconditionFailed, status 412, Classify Precondition |
| X9 | pass | 5 keys right after PUT, pages of 2, lexicographic order |
| X10 | pass | RemoveObjects of 2 keys + 1 missing: no errors, prefix empty |
| X11 | pass | second create-only multipart completion → 412, first upload kept |
| X12 | pass | agent identity: LIST and DELETE denied |

Also passing on MinIO: every S3-gated test (`COLDLOAD_S3=localhost:9010 go test -run S3 ./internal/objstore
./internal/vactor`), including the durable store contract and transport faults.

Note: X3 on MinIO is ~40× slower than on SeaweedFS (≈ 77 ms per round of 8 concurrent conditional PUTs plus a GET). Not
investigated; for us a publish is one PUT per write-back, so it matters only for latency, not correctness.

## SeaweedFS 4.47, multi-node — all pass, conditional PUT atomic across gateways

- **Rig:** `deploy/seaweedfs-cluster/docker-compose.yml` (new): 1 master, 3 volume servers
  (`-defaultReplication=001`), **2 filers sharing one Postgres 17 filer store** (`postgres2`, C collation), one
  S3 gateway per filer: `localhost:8343` and `localhost:8344`. Same `s3.json` identity as single-node.
  Left running (`docker compose -f deploy/seaweedfs-cluster/docker-compose.yml down` removes it).
- **New:** `S3_COMPAT_PEER` (additive env var): X3's odd writers and its read-back go through the second
  gateway, so the If-Match race is across filers; the detail reports how many rounds the peer won.
- **Command:** `COLDLOAD_S3=localhost:8343 S3_COMPAT_PEER=localhost:8344 S3_BACKEND=seaweedfs-4.47-cluster
  go test -count=1 -v -run TestS3Compat ./internal/objstore`.

| # | Result | Detail |
|---|---|---|
| X1–X2, X4–X11 | pass | as single-node (all through gateway 1) |
| X3 | pass | 200 rounds × 8 If-Match writers, 4 per gateway: exactly 1 winner each, stored bytes = winner's, read back through gateway 2; **gateway 2 won 106 of 200**. A second run with 1000 rounds also passed (3.3 s) |
| X12 | skip | no scoped identity in `s3.json` (CP-4 adds one) |

S3-gated objstore and vactor tests also pass against gateway 1.

What this does and does not show:

- Cross-gateway conditional PUT on a shared Postgres filer store is atomic in over 1200 contended rounds. It is
  evidence, not proof; the safety argument's assumption "conditional publish is atomic" now has a multi-node
  data point (§5 of the control-plane plan).
- Not covered: filers with **separate** stores (leveldb per filer, async metadata sync) — conditional PUTs
  there would race on unsynchronized metadata and are expected to fail X3; do not deploy that topology. Not
  covered either: master failover, volume-server loss during a publish, and network partitions between
  filers and Postgres.
- Startup quirk: both filers create the `filemeta` table at once and the loser exits
  (`duplicate key … pg_type_typname_nsp_index`); the compose file sets `restart: on-failure`.
- Postgres with the default `en_US.utf8` collation makes SeaweedFS warn that LIST order is not
  byte-lexicographic (and fall back to a slower sort); the compose file initializes the store with
  `--locale=C`.
