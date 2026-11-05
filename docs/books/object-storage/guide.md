# Object storage patterns and rules for ts-db-go

Status: adopted rules · Date: 2026-11-05 · Background and refactor items: [plan.md](plan.md) (`OS-NN`)

These are the rules for any code that touches S3-compatible storage. Today that means `internal/objstore`,
`internal/durable`, `internal/vactor` (write-back, passivation and activation), and `internal/owner` where it
calls Stamp, Publish and Head. Future WAL shipping, GC and tools follow the same rules.

Each rule gives the rule, why, and an example from this codebase. "Ch N" is _Object Storage as
Infrastructure_. Use the book for its ideas only: several of its listings break these rules (plan.md, header).
Where this guide and the book disagree, this guide wins.

Related rules:
- [knowledge/testing/README.md](../../../knowledge/testing/README.md): how to test.
- `~/AI/writeups/go-coding-style.md`: errors (`errs.Wrap`, one log line per failure with its log ID) and
  contracts (`internal/assert`).
- [fencing safety argument](../../../knowledge/fencing/2026-11-05-safety-argument.md): the ownership
  invariants these storage rules protect.

## 1. Key layout

**K1. One database owns one prefix, ended by `/`. No database's prefix may contain another's.**
- Why: GC, drop and inventory work by prefix. A nested key would delete or count another database's
  objects (Ch 2, Ch 16).
- Example: build every name from `durable.KeyPrefix(key)`, with the key escaped (OS-04). Never use
  `"dbs/" + key` directly.
  ```go
  durable.KeyPrefix("t1/a") // "dbs/bb/t1%2Fa/"   never "dbs/t1/a/"
  ```

**K2. Compute keys; do not LIST on a request path.**
- Why: LIST is slow, costs money, and pages through 1,000 entries at a time (Ch 1, Ch 2). Every object on
  the hot path is named from data we already have: the manifest names the generation, and a generation
  number names its object.
- Example: `fetchDurable` reads `manifest.json` and then `m.Object`. Nothing lists. LIST is allowed only in
  the GC sweeper, `cmd/dbtool` and coldload setup.

**K3. Put a hash shard first, then the readable key.**
- Why: the shard gives S3 prefix partitions to scale on (about 3,500 PUT/s and 5,500 GET/s per prefix,
  Ch 2), and gives GC 256 bounded slices to work through (Ch 4). The readable key keeps listings
  debuggable.
- Example: `dbs/<sha256(key)[:2] hex>/<escaped key>/…`.

**K4. Name each kind of object so its role, number and writer can be read from the name.**
- Why: recovery and GC must classify an object without opening it (Ch 16, naming conventions).
- Example layout (OS-05, OS-13):
  ```
  dbs/<h2>/<key>/manifest.json                         the only mutable object
  dbs/<h2>/<key>/gen/<G 20d>-e<E 20d>.db               immutable snapshot, written at epoch E
  dbs/<h2>/<key>/gen/<G>-e<E>/wal/<seq 20d>-e<E>.wal   immutable WAL segment (future)
  ```
  Numbers are zero-padded so that lexical order equals numeric order. Never rely on LIST order anyway:
  sort on the client side (Ch 3).

**K5. Write a layout change into the contract.**
- Why: the layout is a cross-agent contract (contract 1 in `shared_state.md`).
- Example: a new layout version gets a contract-suite case, and a dated note in `knowledge/writeback/`.
  Readers always go through `Manifest.Object` and never rebuild the name themselves, so old and new names
  can coexist.

## 2. Immutability

**I1. Every object except the manifest is create-only and is never overwritten.**
- Why: immutable objects cannot tear, need no cache invalidation, and make recovery a scan rather than a
  repair (Ch 2, Ch 4, Ch 5).
- Example (`objstore/durable.go:91`):
  ```go
  opts.SetMatchETagExcept("*") // If-None-Match: * — a 412 is durable.ErrExists, never an overwrite
  ```

**I2. Never reuse a number you tried.**
- Why: an upload whose outcome is unknown may exist. Reusing its number would either collide or name
  different bytes.
- Example: `e.lastGen = gen // never reuse a number we tried` (`vactor/writeback.go:210`). Since OS-05, the
  epoch in the name also rules out collisions between owners.

**I3. Write data first, publish it second, and trust only the published pointer.**
- Why: S3 has no transactions across objects. Only a conditional write to one object makes a set of
  objects visible all at once (Ch 2, Ch 5, Ch 7).
- Example: `upload` runs PutGeneration, then `publish`. `fetchDurable` never looks at a generation the
  manifest does not name. An object no manifest names is an orphan: invisible and harmless until GC.

**I4. Each object carries enough metadata to rebuild its pointer.**
- Why: a manifest that is lost or corrupted must be recoverable from the objects it names (Ch 9 reflog,
  Ch 12 runbooks).
- Example (OS-05): `PutGeneration` sets user metadata `sha256`, `epoch`, `generation`, `key` and
  `format`.

## 3. Conditional writes (the mutable pointer)

**W1. Every write to a mutable object is conditional.**
- Why: an unconditional read-modify-write loses updates. That is the book's own `objqueue`/`objkv` bug
  (Ch 6, Ch 7).
- Example: `PublishManifest` sends `If-Match: <etag read>`, or `If-None-Match: *` when no manifest exists
  (`objstore/durable.go:153-158`). No other mutable objects exist. If you add one, it follows this rule.

**W2. Every conditional publish also carries the writer's fencing epoch, and storage refuses a lower one.**
- Why: an ETag condition stops lost updates but not zombies. The epoch stops zombies (Ch 5, Ch 10; safety
  argument, step 6).
- Example:
  ```go
  if err := durable.CheckPublish(prev, next); err != nil { return durable.NoVersion, err } // epoch + generation
  v, err := h.opts.Store.PublishManifest(ctx, next, e.ver)                                    // atomic If-Match
  ```
  A new owner `Stamp`s its epoch before it admits anything.

**W3. Every publish must change the bytes, with a strictly increasing `(epoch, generation)`.**
- Why: on many backends the ETag is the MD5 of the body. A byte-identical rewrite produces the same ETag
  again (ABA), and an old `If-Match` would then succeed (Ch 1, ETag).
- Example: `CheckPublish` requires `gen > stored.gen`, and `CheckStamp` raises the epoch or does nothing
  (`durable/durable.go:197-228`). Never publish "the same manifest again" to refresh it.

**W4. Treat 412 and 409 `ConditionalRequestConflict` the same: they mean stale.**
- Why: AWS returns 409 for concurrent conditional conflicts.
- Example: `preconditionFailed` (`objstore/durable.go:50-53`).

**W5. Do not use S3 for coordination that needs low latency, watches, or atomicity with other data.**
- Why: Ch 10's own rule: do it in the database that holds the data. Placement and epochs live in Postgres
  (fencing's Postgres note). S3 carries only the storage backstop.

**W6. No clocks in correctness.**
- Why: leases and TTLs do not make a lock safe; fencing does (Ch 10, Kleppmann). Our ownership has no
  clocks at all.
- Example: object timestamps are used only for GC grace windows (GC3), never for who may write.

## 4. Retries and idempotency

**R1. Classify every error before deciding.**

| Kind | Examples | Action |
|---|---|---|
| `Transient` | 5xx, 429, 503 SlowDown, network error, timeout | retry |
| `Precondition` | 412, 409 | do not retry blindly; re-read (R3) |
| `NotFound` | 404 | do not retry |
| `Permanent` | other 4xx, e.g. AccessDenied | do not retry |
| `Canceled` | context cancelled or expired | do not retry |

- Why: retrying a 4xx hides bugs, and not retrying a 503 turns throttling into an outage (Ch 2, Ch 12).
- Example: `objstore.Classify(err)` (OS-01). Callers branch on `errors.Is` and the kind, never on message
  text.

**R2. Retry only idempotent requests automatically: GET, HEAD, LIST, DELETE.**
- How: jittered exponential backoff (100 ms, ×2, 5 s cap, at most 5 attempts), inside
  `objstore.retryIdempotent`.
- Why: a PUT whose reply was lost may have applied (Ch 12).
- Example: the durable client sets `minio.Options.MaxRetries` so minio-go does not retry on its own.
  minio-go v7 would otherwise re-send seekable PUT bodies up to 10 times (OS-01).

**R3. After an ambiguous write (an error, or a 412 that our own retry could have caused), re-read before
anything else.**
- Why: the outcome is unknown. Only the stored state can say (Ch 2, Ch 12).
- Example (`vactor/writeback.go:264-274`):
  ```go
  stored, sv, rerr := store.ReadManifest(ctx, next.Key)
  if rerr == nil && stored.Same(next) { return sv, nil } // it applied; the reply was lost
  ```
  Stamp follows the same pattern (OS-02). For create-only uploads, compare the HEAD's `sha256` metadata
  with our digest.

**R4. Set a sticky "stale" or "superseded" state only after a re-read shows another writer.**
- Why: a false "stale" leaves the real owner unable to become durable.
- Example: set `e.stale.Store(true)` only when the re-read manifest is not ours.

**R5. Every error message says what changed and whether a retry is safe.**
- Why: callers and operators need this to act. It is already the repo's style.
- Example:
  ```go
  errs.Wrap(module, err, "cannot upload %s; no manifest names it, safe to retry with a new generation", name)
  ```

## 5. Integrity

**V1. Verify size and SHA-256 on every read of a generation, before use. Leave no file on failure.**
- Why: bit rot, truncated writes and storage bugs happen, and corruption that goes undetected spreads
  (Ch 4, Ch 12).
- Example: `FetchGeneration` hashes while it writes `.part`, compares the result with the manifest, and
  renames only on a match (`objstore/durable.go:121-133`). A mismatch is `durable.ErrCorrupt`.

**V2. Have the server check every upload.**
- Why: a corrupt upload that gets published is found only after the warm copy is gone.
- Example: `SendContentMd5: true` on PUTs (OS-07), plus the HEAD size check after the PUT
  (`durable.go:98-105`). If a backend ignores `Content-MD5`, record that in its compat note.

**V3. Formats that hold records carry a header and a footer checksum.**
- Why: a reader rejects a non-log or truncated object without trusting its name (Ch 5).
- Example (WAL segments, OS-13): the header holds magic, key, generation, epoch and start sequence; the
  footer holds end sequence and CRC32C.

## 6. GC and retention

**GC1. Never delete the object the current manifest names, the pending (ambiguous) one, or the K
retained predecessors.**
- Why: an activation or a move fetches the generation the manifest names, and K gives point-in-time
  rollback (Ch 7, Ch 14).
- Example: `durable.PlanGC` (OS-06) is pure and spec-tested at the boundary at K. Default K = 2. The
  sweeper cannot tell a published predecessor from an orphan, so it keeps the K highest-numbered generations
  below the current one; owner-side retention (`durable.PlanRetention`) keeps the K it published.

**GC2. Owner-side retention runs under the same serialization as publish.**
- Why: GC must never race the publisher it is trimming behind.
- Example: delete G−K−1 right after a successful publish, while `e.wb` is held, so Fence drains it like a
  publish. Use computed names, with no LIST (`vactor.Host.retain`). A failed delete never fails the
  write-back; the sweeper gets the object later.

**GC3. Delete an orphan only after the manifest has been rewritten since the orphan was uploaded, and
only after a grace window.**
- Why: every publish naming the orphan was conditional on a manifest version that no longer exists, and
  can never return (W3). The grace window bounds delayed requests. This is stronger than the book's
  "reachability manifest" (Ch 9), which works only if writers check that same object.
- Example: `o.LastModified + grace < manifest.LastModified && now − o.LastModified > grace`, with
  grace = 2 × `WriteBackTimeout`. `durable.Sweep` takes the manifest's LastModified from the listing and
  skips the key if the listed version is not the one `ReadManifest` returns (it moved in between).

**GC4. GC never writes the manifest, and never deletes from more than one key's prefix per decision.**
- Why: a GC bug must not be able to fork a history or reach another database (K1).

**GC5. Use batch deletes, measure them, and keep up.**
- Why: GC that falls behind looks like a cost leak (Ch 14).
- Example: `DeleteGenerations` deletes in `RemoveObjects` batches of up to 1000, and listings go one page at
  a time (`objstore.ListPage`, `EachPage`). `durable.SweepResult` reports objects and bytes listed and
  deleted per key; `Host.Stats` counts `GenerationsDeleted` and `RetentionFailures`. Alert when bytes grow
  faster than (K+1) × size.

**GC5a. Dropping a database: tombstone in placement first, then delete its prefix.**
- Why: if the prefix goes first, a concurrent activation could recreate the database.
- Example: the placement tombstone comes first, so no new owner can be placed. Then `DeletePrefix(KeyPrefix(key))`
  runs, and is retried until the prefix is empty.

**GC6. Never adopt an orphan across epochs.**
- Why: a zombie's uploads after the stamp must never enter the history (safety argument, S2). The book's
  "adopt valid orphans" recovery (Ch 5) applies only within one epoch, on the same process.

## 7. Compatibility checks per S3 implementation

Run `TestS3Compat` (OS-03) for every backend and version before using it. Record the results in
`knowledge/writeback/<date>-s3-compat-<backend>.md`. A failing check in X1–X5 blocks the backend.

| # | Check | Why we need it |
|---|---|---|
| X1 | `If-None-Match: *` on PUT: the second create gets 412 | I1, I2 |
| X2 | `If-Match: <etag>` on PUT: a stale ETag gets 412 or 409 | W1 |
| X3 | Atomic under concurrency: N concurrent `If-Match` writers on one version produce exactly 1 winner, over 200 rounds; multi-node too | safety argument S2 |
| X4 | Strong read-after-write: a GET or HEAD right after a PUT or overwrite sees the new bytes and ETag | R3 re-reads, activation |
| X5 | The ETag changes on every overwrite; record whether identical bytes repeat the ETag | W3 |
| X6 | `Content-MD5` mismatch is rejected (BadDigest) | V2 |
| X7 | User metadata round-trips on HEAD and GET | I4 |
| X8 | The 412 is distinguishable from 5xx through minio-go (`ToErrorResponse().Code`) | R1 |
| X9 | LIST right after PUT includes the key; note any page-order differences | GC sweeper |
| X10 | `RemoveObjects` batch delete; deleting a missing key is not an error | GC5 |
| X11 | `If-None-Match` on `CompleteMultipartUpload` | required before any generation over 5 GiB |
| X12 | Scoped identity: the agent credential cannot LIST or delete outside its permissions | §9 |

Known results: single-node SeaweedFS 4.47 passes X1–X11; X12 is not run (no scoped identity yet)
([compat note](../../../knowledge/writeback/2026-11-05-s3-compat-seaweedfs.md)). Identical bytes repeat the
ETag there (X5). 2026-11-05 (CP-8): MinIO (2025-05-24 build) passes X1–X12; a SeaweedFS 4.47 cluster with two
filers on a shared Postgres store passes X1–X11 with X3 raced across both gateways (`S3_COMPAT_PEER`,
`deploy/seaweedfs-cluster/`) ([note](../../../knowledge/writeback/2026-11-05-s3-compat-minio-and-cluster.md)).
Filers with separate stores, failover during a publish, and AWS are unverified.

## 8. Caching

**CA1. Cache immutable objects by content hash, and trust the cache only when the manifest names that
hash.**
- Why: immutable data never needs invalidation; only the pointer can change (Ch 7, Ch 11).
- Example (OS-10): `<cache>/gen/<sha256>.db`. Activation reads the manifest (1 GET), and on a hash match
  it links the cached file instead of downloading. Never cache `manifest.json` across activations.

**CA2. One long-lived client per process, with its connection pool sized to the fetch concurrency.**
- Why: setting up a connection costs 50–200 ms (Ch 11).
- Example: `objstore.New` with `MaxConns` (`objstore.go:58-71`). Never build a client per request.

## 9. Security

**S1. One credential per component, scoped to the bucket and to the actions it needs.**
- Why: a leaked credential should only be able to do its own job (Ch 13).

| Credential | May do |
|---|---|
| agent | Read, Write |
| gc | Read, List, Write |
| bench/dev | Admin, local only |

- Example: `deploy/seaweedfs/s3.json` (OS-11).

**S2. TLS outside a developer laptop. Credentials never go in code, configs or logs.**
- Example: `objstore.Config.Secure` (OS-11). agentd refuses plain HTTP without `-dev`.

**S3. Tenant isolation comes from the key (K1), not from a check in the caller.**
- Why: if tenants share a bucket, the tenant must be part of the database key, so the prefix is the
  isolation unit (Ch 13, prefix per tenant). Per-tenant buckets or keys can come later.

## 10. Observability and operations

**O1. Measure every S3 request: op, kind, bytes, duration.**
- How: count every request. Log each failure once with key, status and kind (`logx`).
- Why: request cost and 503s show up here first (Ch 11, Ch 14).
- Example: `objstore.Store.Metrics()` (OS-08): attempts by op and `Kind`, retries, bytes, latency histogram;
  retried attempts log `S3.REQUEST_RETRIED`. It counts HTTP requests, so retries and the HEAD after an upload show.

**O2. Report requests, and dollars, per logical operation in benchmarks.**
- Why: "2× faster but 10× the requests" can be a loss (Ch 11).
- Example: wbbench prints requests per write-back. Today a write-back is 4 requests and an activation is
  2.

**O3. Every storage failure mode has a runbook entry and a `cmd/dbtool` command.**
- Failure modes: corrupt generation, lost manifest, orphan growth, cost spike, SlowDown storm.
- Why: recovery paths that run only during incidents do not work (Ch 12, Ch 14).
- Example: OS-12.

## 11. Where this guide sits

- It lives here, `docs/books/object-storage/guide.md`.
- `docs/coding-style.md` should summarize it in one section that links back here.
- `knowledge/writeback/README.md` should link to it from "Design in one screen".
- Testing additions belong in `knowledge/testing/README.md` "Notes by others", not here:
  - the X-checks;
  - the fault switches `ApplyThenStale` and `LoseUploadReply`;
  - the GC invariant in `FuzzDurableHostModel`.
