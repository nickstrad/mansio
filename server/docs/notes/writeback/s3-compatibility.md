# S3 compatibility: SeaweedFS and MinIO

Which S3 backends pass the checks the durable store relies on (`TestS3Compat` X1–X12 and the durable store
contract suite), with the rigs and commands, and what the results do and do not show.

The fencing [safety argument](../fencing/safety-argument.md) assumes the conditional publish (`If-Match` /
`If-None-Match`) is atomic; these runs are its evidence. All runs 2026-11-05, Docker Desktop on macOS, Apple
M5 Max. Client minio-go v7 (v7.4.1) with `MaxRetries: 1` (no SDK retries, so every answer is the server's
first).

## The checks

| #   | What is checked                                                                                                                                                                                    |
| --- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| X1  | `If-None-Match: *`: the second create → 412                                                                                                                                                        |
| X2  | `If-Match` on the current ETag → 200; on a stale ETag → 412                                                                                                                                        |
| X3  | 200 rounds × 8 concurrent `If-Match` writers on one ETag: exactly 1 winner each round, stored bytes = winner's                                                                                     |
| X4  | 50 overwrites: HEAD and GET right after each PUT see its ETag and bytes                                                                                                                            |
| X5  | 20 distinct overwrites → 20 distinct ETags; identical bytes repeat the ETag (MD5 of the body)                                                                                                      |
| X6  | wrong `Content-MD5` → `BadDigest` (400, `Permanent`), nothing stored; right MD5 → 200                                                                                                              |
| X7  | `x-amz-meta-{sha256,epoch,generation,key}` round-trip on HEAD and GET                                                                                                                              |
| X8  | through minio-go: `Code` `PreconditionFailed`, status 412, `objstore.Classify` → `Precondition`                                                                                                    |
| X9  | 5 keys listed right after PUT, pages of 2, lexicographic order                                                                                                                                     |
| X10 | `RemoveObjects` of 2 keys + 1 missing key: no errors, prefix empty afterwards                                                                                                                      |
| X11 | `If-None-Match: *` on `CompleteMultipartUpload`: the second completion → 412, first upload kept                                                                                                    |
| X12 | a scoped agent identity is refused what it must not do: LIST on SeaweedFS (and bucket creation, PUT outside its buckets); LIST and DELETE on MinIO (see [S3 identities](s3-identities-and-tls.md)) |

X1–X5 are the blocking checks: a backend that fails one of them must not be used.

## Results

| Backend                              | X1–X11                    | X12                                                                        |
| ------------------------------------ | ------------------------- | -------------------------------------------------------------------------- |
| SeaweedFS 4.47, single node          | pass (X3: 0.38 s)         | pass with the `agent` identity ([S3 identities](s3-identities-and-tls.md)) |
| MinIO 2025-05-24, single node        | pass (X3: 15.4 s)         | pass                                                                       |
| SeaweedFS 4.47, 2 filers on Postgres | pass (X3 across gateways) | skip (no scoped identity in that rig's `s3.json`)                          |

Every S3-gated test (`COLDLOAD_S3=<endpoint> go test -run S3 ./internal/objstore ./internal/vactor`),
including the durable store contract and transport faults, also passes on each backend.

## SeaweedFS 4.47, single node

- **Rig:** the `coldload-seaweedfs` container from `deploy/seaweedfs/docker-compose.yml`
  (`chrislusf/seaweedfs:latest`, `weed version` → `30GB 4.47 c50733600 linux arm64`; also `make coldload-up`),
  one process (master + volume + filer + S3). Credentials: the `coldload` identity in
  `deploy/seaweedfs/s3.json` (Admin, Read, List, Tagging, Write).
- **Compat command:**
  `COLDLOAD_S3=localhost:8333 S3_BACKEND=seaweedfs-4.47-single go test -count=1 -v -run TestS3Compat ./internal/objstore`
  (also passes under `-race`).
- **Contract suite:**
  `COLDLOAD_S3=localhost:8333 go test -count=1 -race -v -run 'S3|Objstore|Durable' ./internal/objstore ./cmd/bench/coldload`.
  All passed: `TestS3DurableStoreContract` (all 15 cases, including create-only generations
  (`If-None-Match: *`), publish against a moved version giving `ErrStale`, a lower epoch giving `ErrStale`,
  and corrupt-fetch detection), `TestS3FetcherContract`, `TestS3StoreDownIsWrapped`.
- **Atomicity stress:**
  `go test -count=200 -run 'TestS3DurableStoreContract/concurrent_publishes' ./internal/objstore`: 200 rounds
  of 8 concurrent `If-Match` publishes on one manifest version, each round **exactly one winner and 7
  `ErrStale`**. Run under the bench lock.

What the single-node results mean:

- X5: an overwrite with identical bytes keeps the ETag, so an ETag alone cannot tell "rewritten with the same
  bytes" from "not rewritten" (the ABA case: a version must change on every publish). Our manifests always
  change bytes on publish (`WrittenAt`, and epoch, generation or seal rise), so `If-Match` stays sound; a
  writer that republished identical bytes would not move the version.
- X6: SeaweedFS enforces `Content-MD5`, so the server-side upload check works on this backend. A byte flipped
  in flight inside a streaming-signed (aws-chunked) upload is also rejected and nothing is stored, but the
  answer is **500 InternalError**, not `BadDigest` (`TestS3FlippedUploadByteIsRejected`, through
  `faultnet.FlipByteAt`): `Classify` says `Transient`. Harmless for us (PUTs are never re-sent; write-back
  retries with a new generation), but a 500 here does not mean the store is unhealthy.
- X11: create-only multipart completion works here, so generations over 5 GiB could use multipart on this
  backend. Not used yet; other backends need their own run.

## MinIO

- **Image:** `bitnamilegacy/minio:latest` → `minio version DEVELOPMENT.2025-05-24T17-08-30Z` (go1.24.2,
  linux/arm64). `minio/minio` and `quay.io/minio/minio` refused anonymous pulls on 2026-11-05 (MinIO stopped
  publishing community images); Bitnami's frozen legacy image is the MinIO server binary. Single node, one
  drive.
- **Rig:**
  `docker run -d --name mansio-minio -p 9010:9000 -e MINIO_ROOT_USER=coldload -e MINIO_ROOT_PASSWORD=coldload-secret bitnamilegacy/minio:latest`.
- **Scoped identity for X12:** `mc admin user add local mansio-agent mansio-agent-secret` with a policy
  allowing only `s3:GetObject` and `s3:PutObject` on `arn:aws:s3:::coldload/*`.
- **Command:**
  `COLDLOAD_S3=localhost:9010 S3_BACKEND=minio-2025-05-24 go test -count=1 -v -run TestS3Compat ./internal/objstore`
  (X12 with `S3_AGENT_ACCESS_KEY=mansio-agent S3_AGENT_SECRET_KEY=mansio-agent-secret`).

X3 on MinIO is ~40× slower than on SeaweedFS (≈ 77 ms per round of 8 concurrent conditional PUTs plus a GET).
Not investigated; a publish is one PUT per write-back, so it matters only for latency, not correctness.

## SeaweedFS 4.47, multi-node

- **Rig:** `deploy/seaweedfs-cluster/docker-compose.yml`: 1 master, 3 volume servers
  (`-defaultReplication=001`), **2 filers sharing one Postgres 17 filer store** (`postgres2`, C collation),
  one S3 gateway per filer: `localhost:8343` and `localhost:8344`. Same `s3.json` identity as single-node.
  `docker compose -f deploy/seaweedfs-cluster/docker-compose.yml down` removes it.
- `S3_COMPAT_PEER`: X3's odd writers and its read-back go through the second gateway, so the `If-Match` race
  is across filers; the detail reports how many rounds the peer won.
- **Command:**
  `COLDLOAD_S3=localhost:8343 S3_COMPAT_PEER=localhost:8344 S3_BACKEND=seaweedfs-4.47-cluster go test -count=1 -v -run TestS3Compat ./internal/objstore`.
- **X3:** 200 rounds × 8 `If-Match` writers, 4 per gateway: exactly 1 winner each, stored bytes = winner's,
  read back through gateway 2; **gateway 2 won 106 of 200**. A second run with 1000 rounds also passed (3.3
  s). The other checks ran through gateway 1.

What this does and does not show:

- Cross-gateway conditional PUT on a shared Postgres filer store is atomic in over 1200 contended rounds. It
  is evidence, not proof.
- Not covered: filers with **separate** stores (leveldb per filer, async metadata sync) — conditional PUTs
  there would race on unsynchronized metadata and are expected to fail X3; do not deploy that topology. Not
  covered either: master failover, volume-server loss during a publish, network partitions between filers and
  Postgres.
- Startup quirk: both filers create the `filemeta` table at once and the loser exits
  (`duplicate key … pg_type_typname_nsp_index`); the compose file sets `restart: on-failure`.
- Postgres with the default `en_US.utf8` collation makes SeaweedFS warn that LIST order is not
  byte-lexicographic (and fall back to a slower sort); the compose file initializes the store with
  `--locale=C`.

## Not covered on any backend

AWS S3, and failover during a publish.

## minio-go does not send a SHA-256 checksum on a streaming PUT

minio-go `AutoChecksum: ChecksumSHA256` does **not** add `x-amz-checksum-sha256` to a single streaming-signed
PUT (seen with the in-process fake S3). So the store does not verify the hash on upload; the host verifies
size by HEAD and re-hashes every fetch (`ErrCorrupt`).
