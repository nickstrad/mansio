# How mansio uses object storage

The durable copy of every database lives in an S3-compatible bucket. This page says how the objects are laid
out, which writes are conditional and why, how ambiguous outcomes and garbage are handled, and what a backend
must support. It applies to `internal/objstore` (the S3 adapter), `internal/durable` (the port, the pure
decisions and GC), `internal/vactor` (activation, write-back, passivation), `internal/owner` where it stamps,
and `cmd/dbtool`. The ownership invariants these rules protect are in the
[fencing safety argument](notes/fencing/safety-argument.md).

The model in one paragraph: each database is a series of **immutable generations** (whole SQLite files) plus
one small **mutable manifest** that names the current one. A write-back uploads a new generation, then moves
the manifest with a conditional PUT that also carries the writer's fencing epoch. Readers trust only the
manifest.

## Key layout

```
dbs/<h2>/<escaped key>/manifest.json                      the only mutable object
dbs/<h2>/<escaped key>/gen/<G 20 digits>-e<E 20 digits>.db  immutable generation G, written at epoch E
```

- **One database owns one prefix, ended by `/`, and no prefix contains another.** Build every name with
  `durable.KeyPrefix(key)`, never `"dbs/" + key`. The key is path-escaped (`/` becomes `%2F`), so keys `t1`
  and `t1/gen/x` cannot nest. GC, drop and inventory work by prefix; a nested key would let them delete or
  count another database's objects.
- **`<h2>` is the first two hex digits of sha256(key).** It spreads load over S3 prefix partitions (each has
  its own request-rate limit) and gives GC 256 bounded shards. The escaped key keeps listings readable.
- **Names are computed, never listed, on a request path.** The manifest names the generation; the generation
  number and epoch name its object. LIST is slow, paged and billed; only the GC sweeper, `cmd/dbtool` and
  setup list.
- **Numbers are zero-padded** so lexical order is numeric order. Code still sorts client-side and never relies
  on LIST order.
- **The layout is a cross-agent contract** (contract 1 in `agent-work/server/shared_state.md`). Readers go
  through `Manifest.Object` and never rebuild a name, so old and new layouts can coexist. A layout change
  needs a contract-suite case and a dated note under `notes/writeback/`.

## Immutability and publishing

- **Everything except the manifest is create-only** (`If-None-Match: *`; a 412 is `durable.ErrExists`, never
  an overwrite). Immutable objects cannot tear, never need cache invalidation, and make recovery a scan
  instead of a repair.
- **Never reuse a generation number that was tried.** An upload whose outcome is unknown may exist. The epoch
  in the name rules out collisions between owners. Within one owner, an earlier incarnation may have tried
  numbers this one does not know: on `ErrExists` for an object that is not ours, the host HEADs forward at
  doubling strides (no LIST). On AWS a HEAD of a missing object answers 403 unless the identity has
  `s3:ListBucket`, so grant `s3:ListBucket` on the prefix (no LIST call is made); without it the host probes
  with full uploads, which is correct but slow.
- **Data first, pointer second, trust only the pointer.** S3 has no multi-object transactions; a conditional
  write to one object is the only way to make a set of objects visible at once. An object no manifest names is
  an orphan: invisible, harmless, and GC's job.
- **Every generation carries the metadata to rebuild its manifest**: `sha256`, `epoch`, `generation`, `key`,
  `format`. A lost or corrupt manifest can then be rebuilt from the objects (`dbtool rebuild-manifest`).

## Conditional writes on the manifest

- **Every write to a mutable object is conditional**: `If-Match: <etag read>`, or `If-None-Match: *` when no
  manifest exists. An unconditional read-modify-write loses updates. The manifest is the only mutable object;
  any new one follows the same rule.
- **Every publish also carries the writer's fencing epoch, and a lower epoch is refused**
  (`durable.CheckPublish`, `CheckStamp`). An ETag condition stops lost updates but not a zombie owner; the
  epoch does. A new owner stamps its epoch on the manifest before it admits any request.
- **Every publish changes the bytes; `(epoch, generation, seal)` only rises.** On many backends the ETag is
  the MD5 of the body, so a byte-identical rewrite repeats the ETag (an ABA), and an old `If-Match` would
  succeed again. `CheckPublish` requires a higher generation; a stamp raises the epoch or, at the same epoch,
  the `Seal`. Never "re-publish the same manifest" to refresh it.
- **412 and 409 (`ConditionalRequestConflict`) both mean stale.** AWS answers 409 to concurrent conditional
  writes.
- **S3 is not a coordination service.** Placement and epochs live in Postgres ([postgres.md](postgres.md)),
  which gives primary reads, single-statement CAS and transactions. S3 carries only the storage backstop.
- **No clocks in correctness.** Leases and TTLs do not make a lock safe; fencing does. Object timestamps are
  used only for GC grace windows, never to decide who may write.

## Errors, retries and ambiguous outcomes

`objstore.Classify(err)` gives every error one kind; callers branch on the kind or `errors.Is`, never on
message text.

| Kind           | Examples                                       | Action                                |
| -------------- | ---------------------------------------------- | ------------------------------------- |
| `Transient`    | 5xx, 429, 503 SlowDown, network error, timeout | retry if idempotent                   |
| `Precondition` | 412, 409                                       | never retry blindly; re-read          |
| `NotFound`     | 404                                            | an answer                             |
| `Permanent`    | other 4xx (AccessDenied, BadDigest)            | do not retry; a bug or a config error |
| `Canceled`     | ctx cancelled or expired                       | do not retry                          |

- **`objstore` is the one retry layer for S3.** minio-go is built with `MaxRetries: 1`; its default re-sends a
  seekable PUT body up to ten times, and a re-sent conditional PUT whose first try applied comes back as a 412
  that looks like "another writer".
- **Only idempotent requests retry automatically** (GET, HEAD, LIST, DELETE): 100 ms ×2, full jitter, 5 s cap,
  5 attempts, never past the caller's deadline. PUTs are sent once.
- **After an ambiguous write, re-read before deciding.** An error or a 412 on a conditional write means
  "unknown". Read the manifest back: if it is `Same` as ours, the write applied and only the reply was lost.
  For a create-only upload, compare the HEAD's `sha256` metadata with our digest. A re-read that still shows
  the old version means "not applied yet": a request in flight may still land, so the host keeps it as pending
  until one of ours applies or the GC grace window passes.
- **Set the sticky `stale`/`superseded` state only when the re-read shows another writer.** A false stale
  leaves the real owner unable to become durable.
- **Error messages say what changed and whether a retry is safe**, e.g. "cannot upload X; no manifest names
  it, safe to retry with a new generation".

## Integrity

- **Verify size and SHA-256 on every generation read, before use, and leave no file on failure.** The fetch
  hashes while writing a `.part` file and renames only on a match; a mismatch is `durable.ErrCorrupt`.
- **The server checks every upload**: every PUT carries `Content-MD5` (computed in the same pass as the
  SHA-256), and a HEAD checks the size afterwards. A corrupt upload that got published would otherwise be
  found only after passivation had deleted the warm copy.
- **Record formats carry a header and a footer checksum** (the planned WAL segments: magic, key, generation,
  epoch and start sequence in the header; end sequence and CRC32C in the footer), so a reader rejects a
  truncated or foreign object without trusting its name.

## Garbage collection and retention

Two mechanisms, both in `internal/durable/gc.go`:

- **Owner-side retention** (`PlanRetention`, run by `vactor` right after a successful publish, under the same
  serialization as the publish, so a fence drains it like a publish). It deletes by computed name the
  generation that fell out of the retained window. No LIST. A failed delete never fails the write-back; the
  sweeper gets the object later.
- **The orphan sweeper** (`PlanGC`, `Sweep`; run as `dbtool gc`, one key at a time). Rules:
  - Never delete the generation the manifest names, the pending (ambiguous) one, or the K retained
    predecessors (default K = 2). The sweeper cannot tell a published predecessor from an orphan, so it keeps
    the K highest numbers below the current one.
  - Delete an orphan only when the manifest was rewritten more than the grace after the orphan's upload, and
    the upload is older than the grace (default 10 min, `durable.DefaultGCPolicy`). Every publish that could
    name the orphan was conditional on a manifest version that no longer exists and can never come back
    (versions only rise), and the grace covers requests delayed in flight. Writers give up on a request after
    `OpTimeout` plus transfer time, and on a write-back after `WriteBackTimeout` (60 s in agentd). That S3
    applies nothing more than the grace after it was sent is an assumption about the server.
    `dbtool gc -grace` must not be shorter than the default.
  - The sweeper skips a key whose listed manifest version differs from the one `ReadManifest` returns (it
    moved in between).
- **GC never writes the manifest and never deletes outside one key's prefix per decision**, so a GC bug cannot
  fork a history or reach another database.
- **Never adopt an orphan across epochs.** A zombie's uploads after the stamp must never enter the history.
- **Dropping a database: placement tombstone first, then delete the prefix** (retried until empty). The other
  order lets a concurrent activation recreate the database.
- **Deletes are batched** (`RemoveObjects`, up to 1000) and listings go one page at a time (`ListPage`,
  `EachPage`). `SweepResult` and `Host.Stats` report what was listed and deleted. Storage per database should
  stay near (K + 1) × its size; alert when bytes grow faster.

## What a backend must support

`TestS3Compat` (`internal/objstore/compat_test.go`, gated on `COLDLOAD_S3`) checks every behaviour above. Run
it for each backend and version before use, and record the result in
`notes/writeback/<date>-s3-compat-<backend>.md`. A failure in the first five blocks the backend.

| Check                                                                                | Needed for                           |
| ------------------------------------------------------------------------------------ | ------------------------------------ |
| X1 `If-None-Match: *` on PUT: a second create gets 412                               | create-only generations              |
| X2 `If-Match` with a stale ETag gets 412 or 409                                      | conditional manifest                 |
| X3 N concurrent `If-Match` writers on one version: exactly one wins, over 200 rounds | fencing                              |
| X4 strong read-after-write for GET and HEAD                                          | re-reads after ambiguity, activation |
| X5 the ETag changes on every overwrite (records whether identical bytes repeat it)   | no ABA                               |
| X6 a `Content-MD5` mismatch is rejected                                              | upload integrity                     |
| X7 user metadata round-trips on HEAD and GET                                         | rebuilding a manifest                |
| X8 a 412 is distinguishable from a 5xx through minio-go                              | error classes                        |
| X9 LIST right after PUT includes the key                                             | the sweeper                          |
| X10 batch delete; deleting a missing key is not an error                             | GC                                   |
| X11 `If-None-Match` on `CompleteMultipartUpload`                                     | any generation over 5 GiB            |
| X12 a scoped identity cannot act outside its permissions                             | credentials                          |

Known results: single-node SeaweedFS 4.47 and MinIO (2025-05-24 build) pass all twelve; a SeaweedFS 4.47
cluster with two filers on a shared Postgres store passes X1–X11 with X3 raced across both gateways
([S3 compatibility results](notes/writeback/s3-compatibility.md)). Identical bytes repeat the ETag on
SeaweedFS, which is why every publish must change the bytes.

## Client, cache and credentials

- **One long-lived `objstore.Store` per process**, its connection pool (`MaxConns`) at least the fetch
  concurrency. A new connection costs tens to hundreds of milliseconds.
- **Per-request budget**: `OpTimeout` (5 s) for small requests and the response headers; transfers get
  `OpTimeout + size / MinThroughput` (32 MiB/s); a transfer that moves no byte for `StallTimeout` (10 s)
  fails. A response-header timeout alone does not protect a body that stops halfway.
- **The local generation cache keys files by content hash** (`<cache>/gen/<sha256>.db`, `vactor`
  `Options.GenCacheBytes`, off by default). Activation still reads the manifest and uses the cached file only
  when the manifest names that hash. Never cache `manifest.json` across activations.
- **One credential per component, scoped to the bucket** (`deploy/seaweedfs/s3.json`): `agent` has Read and
  Write (Write includes DELETE, for owner-side retention; no LIST, no bucket creation); `gc` has Read, List
  and Write; `bench` is an admin identity for local runs only. A leaked agent credential can then only do an
  agent's job.
- **TLS outside a developer laptop**: agentd and controld refuse plain HTTP to S3 without `-dev`. Credentials
  live in the 0600 secrets file, never in configs, code or logs.
- **Tenant isolation comes from the key prefix**, not from a check in the caller: with a shared bucket the
  tenant is part of the database key.

## Observability and operations

- `objstore.Store.Metrics()` counts every HTTP request by operation and kind, with bytes, a latency histogram
  and retries; a retried request logs `S3.REQUEST_RETRIED`. Failures are logged once with key, status and
  kind.
- Benchmarks report requests (and dollars at list price) per logical operation, not only latency: "2× faster
  with 10× the requests" can be a loss. Today a write-back is 4 requests (PUT generation, HEAD, GET manifest,
  PUT manifest) and an activation is 2. The manifest pre-read in a write-back is kept on purpose: a GET costs
  about a tenth of a PUT, and it checks the stored epoch as a second line of defence.
- Every storage failure mode has a `cmd/dbtool` command and a runbook entry
  ([storage runbook](notes/writeback/storage-runbook.md)): corrupt generation (`verify`), lost manifest
  (`rebuild-manifest`, with the epoch from the placement row, published only with `If-None-Match` or
  `If-Match` on the manifest the operator saw), orphan growth (`orphans`, `gc`), a cost spike, a 503 SlowDown
  storm.

## Not adopted

- **Bucket versioning and Object Lock.** Generations already are application-level versions and K of them are
  kept; backend support is not verified and nothing depends on it.
- **Bucket policies that "require conditional writes".** SeaweedFS has none; `TestS3Compat` and the adapter
  enforce conditional writes instead.
- **Leases stored in S3.** If leases are ever added they live in Postgres placement (see the
  [leases options note](notes/fencing/leases-options.md)).

## Open questions

- **Multipart uploads.** Generations over 5 GiB are refused. Multipart needs X11 on the backend in use, and
  then a lifecycle rule that aborts incomplete uploads after a day.
- **AWS S3, filers with separate stores, and failover during a publish** are unverified.
- **WAL shipping** is designed, not built ([design](notes/writeback/wal-shipping-design.md)): create-only
  segments named `gen/<G>-e<E>/wal/<seq>-e<E>.wal`, found by computed-key HEAD probes, not LIST; never adopted
  across epochs. With an output gate it puts an S3 PUT on the write-ack path, so write availability becomes at
  most S3 PUT availability. Before any WAL code, decide the behaviour under an open S3 breaker: refuse writes,
  or acknowledge locally with a larger RPO.
