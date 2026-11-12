# 2026-11-05 — S3 compatibility suite (TestS3Compat) on single-node SeaweedFS 4.47

Object-storage guide §7 / plan OS-03. Written by the p1-s3 books agent (branch `books/p1-s3`); the writeback
owner may fold it into this folder's README.

- **Rig:** the `coldload-seaweedfs` container from `deploy/seaweedfs/docker-compose.yml`
  (`chrislusf/seaweedfs:latest`, `weed version` → `30GB 4.47 c50733600 linux arm64`), one process
  (master + volume + filer + S3), Docker Desktop on macOS, Apple M5 Max. Credentials: the single `coldload`
  identity in `deploy/seaweedfs/s3.json` (Admin, Read, List, Tagging, Write).
- **Command:**
  `COLDLOAD_S3=localhost:8333 S3_BACKEND=seaweedfs-4.47-single go test -count=1 -v -run TestS3Compat ./internal/objstore`
  (also passes under `-race`). Client: minio-go v7.4.1, `MaxRetries: 1` (no SDK retries, so every answer
  below is the server's first answer).

| # | Result | What was checked |
|---|---|---|
| X1 | pass | `If-None-Match: *`: the second create → 412 |
| X2 | pass | `If-Match` on the current ETag → 200; on a stale ETag → 412 |
| X3 | pass | 200 rounds × 8 concurrent `If-Match` writers on one ETag: exactly 1 winner each round, and the stored bytes are the winner's (0.38 s) |
| X4 | pass | 50 overwrites: HEAD and GET right after each PUT see its ETag and bytes |
| X5 | pass | 20 distinct overwrites → 20 distinct ETags. **Identical bytes repeat the ETag** (MD5 of the body) |
| X6 | pass | wrong `Content-MD5` → `BadDigest` (400, `Permanent`), nothing stored; right MD5 → 200 |
| X7 | pass | `x-amz-meta-{sha256,epoch,generation,key}` round-trip on HEAD and GET |
| X8 | pass | through minio-go: `Code` `PreconditionFailed`, status 412, `objstore.Classify` → `Precondition` |
| X9 | pass | 5 keys listed right after PUT, pages of 2, lexicographic order |
| X10 | pass | `RemoveObjects` of 2 keys + 1 missing key: no errors, prefix empty afterwards |
| X11 | pass | `If-None-Match: *` on `CompleteMultipartUpload`: the second completion → 412, first upload kept |
| X12 | skip | no scoped identity is configured; `s3.json` has only the admin identity (OS-11 adds one). **Update 2026-11-05: pass** with the `agent` identity ([note](2026-11-05-s3-scoped-identities.md)) |

What this means:

- X1–X5 (the blocking checks) pass: single-node SeaweedFS 4.47 may be used. Same result as the earlier
  [conditional PUT verification](2026-11-05-s3-conditional-put-verified.md), now with the stored bytes checked
  per round.
- X5: an overwrite with identical bytes keeps the ETag, so an ETag alone cannot tell "rewritten with the same
  bytes" from "not rewritten" (the ABA case of guide rule W3). Our manifests always change bytes on publish
  (`WrittenAt`, and epoch or generation rise), so `If-Match` stays sound; a writer that republished identical
  bytes would not move the version.
- X6: SeaweedFS enforces `Content-MD5`, so OS-07's server-side upload check works on this backend. A byte
  flipped in flight inside a streaming-signed (aws-chunked) upload is also rejected and nothing is stored,
  but the answer is **500 InternalError**, not `BadDigest` (`TestS3FlippedUploadByteIsRejected`, through
  `faultnet.FlipByteAt`): `Classify` says `Transient`. Harmless for us (PUTs are never re-sent; write-back
  retries with a new generation), but a 500 here does not mean the store is unhealthy.
- X11: create-only multipart completion works here, so generations over 5 GiB could use multipart on this
  backend. Not used yet; other backends need their own run.
- Not covered: multi-node SeaweedFS (needs `deploy/seaweedfs-cluster/`, owned by coldload/writeback), MinIO,
  AWS S3, failover during a publish, and X12 until a scoped identity exists.
