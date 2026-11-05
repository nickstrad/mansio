# Plan: apply _Object Storage as Infrastructure_ to ts-db-go

Status: plan · Date: 2026-11-05 · Baseline: `main` at `bbf184b` · Item prefix: `OS-`

Sources:
- The book: _Object Storage as Infrastructure_ (Steve Publications, 2026). Summaries are in
  `~/AI/books/object-storage-as-infrastructure/chapters/` and the raw text is in `raw/chapters-text/`.
  "Ch N" refers to the book.
- The project's own verified facts, which win over the book:
  - [S3 conditional PUT verified on SeaweedFS](../../../knowledge/writeback/2026-11-05-s3-conditional-put-verified.md)
  - [the fencing safety argument](../../../knowledge/fencing/2026-11-05-safety-argument.md)
  - [the WAL-shipping note](../../../knowledge/writeback/2026-11-05-wal-shipping.md)
  - [the control-plane plan](../../control-plane-plan.md)
- The rules agents follow from now on are in [guide.md](guide.md). This file holds the findings and the
  refactor items.

**About the book's code.** Several Go listings contradict the book's own prose:
- `objqueue` and `objkv` rewrite their index and manifest with **unconditional PUTs** (Ch 6, Ch 7).
- `objkv` has **no WAL** (Ch 7).
- `objlease` takes over an expired lease with `If-None-Match: *` where it needs `If-Match` (Ch 10).
- The bucket policy that "requires conditional writes" has broken condition logic (Ch 13).
- The Terraform example grants `s3:HeadObject`, which is not an IAM action (Ch 16).

This plan takes the book's design ideas only. Every pattern below was checked against what this repo has
already verified.

## 1. Why this book matters here

ts-db-go's durability model is the book's central pattern:
- immutable objects (generations, `dbs/<key>/gen/<G>.db`);
- one small mutable pointer (`manifest.json`), moved only by a conditional PUT (Ch 2, 5, 7, 9);
- fencing epochs carried into that conditional write (Ch 5, Ch 10).

Each step of the control-plane roadmap leans harder on object storage:
- Wiring agentd to `vactor` in durable mode puts S3 on every activation, write-back, Stamp and Fence.
- Moves (`owner.Mover`) and VM-loss restores use the manifest as the handoff point.
- WAL shipping would make S3 a write path, not just a snapshot store.

The book covers the parts this repo has not built yet:
- garbage collection and retention (Ch 7, 9, 14);
- retries that know which writes are idempotent (Ch 2, 12);
- key and namespace design (Ch 2, 4, 16);
- per-backend compatibility checks (Ch 3);
- integrity on upload (Ch 4, 12);
- caching immutable objects (Ch 7, 11);
- per-component credentials (Ch 13);
- per-request cost and observability (Ch 11, 14).

## 2. What the project already does well

| Book idea | Where it already holds |
|---|---|
| Write data first, then publish it with a CAS on a small manifest (Ch 2, 5, 7) | `vactor.(*Host).upload` uploads, then calls `publish` (`internal/vactor/writeback.go:207-235`). Readers trust only the manifest (`fetchDurable`, `writeback.go:66-105`). |
| Immutable objects are create-only (`If-None-Match: *`) (Ch 1, 4) | `objstore.(*Durable).PutGeneration` uses `SetMatchETagExcept("*")` (`internal/objstore/durable.go:91`). A generation number that was tried is never reused (`writeback.go:209-210`). |
| CAS with `If-Match` maps to 412 → stale (Ch 2, 9) | `PublishManifest` (`durable.go:153-163`). `preconditionFailed` also treats AWS's 409 `ConditionalRequestConflict` as stale (`durable.go:50-53`). |
| An epoch in every conditional publish rejects zombies (Ch 5, 10) | `durable.CheckPublish` and `CheckStamp` (`internal/durable/durable.go:197-228`). `owner` stamps the manifest before it admits work (`internal/owner/table.go:415-440`). This is the safety argument's S2. |
| Correctness locks need fencing, not timers (Ch 10, after Kleppmann) | No leases and no clocks in ownership (safety argument, "No clocks"). Stricter than the book. |
| After an ambiguous write, verify before retrying (Ch 2, 12) | `vactor.(*Host).publish` re-reads and adopts `stored.Same(next)` or `e.pending` (`writeback.go:251-276`). The `LosePublishReply` fault covers it. |
| Verify a checksum on every read (Ch 4, 12) | `FetchGeneration` re-hashes with SHA-256 and checks the size; a mismatch is `ErrCorrupt` and no file is left behind (`durable.go:121-133`). |
| One PUT is all-or-nothing; uploads go to a `.part` file, then rename (Ch 1) | `DisableMultipart` (`durable.go:89`). `GetToFile`/`writeFile` rename only on success (`objstore.go:131-141`). |
| Long-lived client with a sized connection pool (Ch 11) | `objstore.New` builds one `http.Transport` with `MaxConnsPerHost` (`objstore.go:58-71`). |
| Run the contract against the real backend (Ch 3) | `durabletest.RunStoreSuite` runs against the fake, the in-process S3 and SeaweedFS (`COLDLOAD_S3`). It includes 200×8 concurrent `If-Match` publishes. |

## 3. Gaps and risks (ranked by impact)

**G1. No garbage collection; the bucket grows by a full database copy on every write-back.**
- Book: compaction and GC are permanent work. Budget for them from day one, and delete only through
  reference-aware GC (Ch 7 "GC", Ch 14 "GC as scheduled jobs", Ch 15 pitfalls).
- Where: `writeback.go:207-215` writes `gen/<G+1>.db` each time. Nothing ever deletes, as the writeback
  README ("Open risks") and control-plane plan §3.8 both say.
- What goes wrong: a 1 GiB database with `WriteBackEvery = 30s` under steady writes adds about 2.8 TB a day.
  Orphans pile up as well:
  - from uploads whose publish failed (`writeback.go:217-233`);
  - from zombies' uploads after the stamp.
- Evidence: there is no Delete in `durable.Store` (`durable.go:79-109`).

**G2. Hidden SDK retries re-send conditional PUTs; the 412 that results is read as "stale".**
- Book: classify errors. Retry 5xx, timeouts and network errors. Never blindly retry a non-idempotent
  conditional write. After an ambiguous outcome, verify first (Ch 2 "Partial failures", Ch 12).
- Where: minio-go v7.4.1 retries seekable bodies up to `MaxRetry = 10` times (`retry.go:34`,
  `api.go:703-747`). Both of our PUT bodies are seekable (`*os.File` at `durable.go:84`,
  `bytes.Reader` at `durable.go:159`). `objstore.New` leaves `Options.MaxRetries` at its default
  (`objstore.go:68-71`).
- Scenario: a manifest PUT applies on the server, the response is lost, and minio-go re-sends it. The
  re-send gets 412 because the ETag has moved, and the adapter maps that to `ErrStale`.
  - `Host.publish` survives this, because it re-reads on any error.
  - The warm `Host.Stamp` does not: it sets the sticky `e.stale` on `ErrStale` without re-reading
    (`writeback.go:469-474`). A new owner can then refuse to write back forever: it stays available but
    never becomes durable until `Discard`.
  - The cold `durable.Stamp` returns the error without re-reading (`durable.go:262-267`).
    `owner.stamp` re-heads and retries 3 times (`table.go:417-439`), which happens to recover.
  - An `ErrExists` caused by a retry of our own `PutGeneration` makes us upload the whole file again under
    G+1 (`writeback.go:208-215`). That is safe, but it costs a full upload and leaves an orphan.

**G3. A database key can sit inside another key's namespace.**
- Book: design keys around access patterns, keep one owner per prefix, and make listing and deletion safe
  by construction (Ch 2 "Listing and namespace design", Ch 16 "prefix is the unit of security, lifecycle,
  ownership").
- Where: `durable.ValidateKey` allows `/` (`durable.go:113-126`), and `GenObject` and `ManifestObject` use
  the key verbatim (`durable.go:129-132`).
- Scenario: keys `t1` and `t1/gen/x` both exist. `t1/gen/x` then stores its manifest under `t1`'s
  generation prefix (`dbs/t1/gen/x/manifest.json`). Any prefix-based GC, drop or inventory of
  `dbs/t1/` (for example `objstore.DeletePrefix`, `objstore.go:210`) deletes or counts another database's
  objects.
- The limits also disagree: `durable` allows 512 bytes, `owner.MaxKeyLen` is 120 (`internal/owner/ports.go:48`).

**G4. No compatibility gate per backend.**
- Book: treat conditional-write support as a hard per-backend requirement. Write down the exact S3 feature
  set the system needs. Test it on the production backend (Ch 3 "Compatibility gaps").
- Where: verification so far is single-node SeaweedFS 4.47 only (the verified note). The following are
  untested:
  - multi-node SeaweedFS;
  - AWS S3 and MinIO;
  - `Content-MD5` enforcement;
  - conditional `CompleteMultipartUpload`, which databases over 5 GiB will need (`durable.go:81-83`);
  - whether the ETag changes when an overwrite has identical bytes (the ABA question, guide rule W3);
  - LIST read-after-write, which the WAL note depends on.
- Scenario: a backend upgrade or swap that silently ignores `If-Match` turns publishes into
  last-writer-wins. The safety argument says S2 then fails, and nothing above storage can catch it.

**G5. Uploads are not checked by the server.**
- Book: put integrity on the write path too, using `Content-MD5` or a checksum header (Ch 4, Ch 12).
- Where: the comment at `durable.go:33-38` says minio-go's `AutoChecksum` sends no
  `x-amz-checksum-sha256`. After the PUT, only the size is checked with a HEAD (`durable.go:98-105`).
- Scenario: a corrupt upload is published, and the corruption is found only at the next activation. By
  then the warm copy is gone, because passivation deletes it (`writeback.go:308-310`). The `ErrCorrupt`
  check at fetch protects activation but cannot get the data back.

**G6. Passivation throws away a valid, immutable local copy.**
- Book: immutable objects never need cache invalidation. Local SSD is a cache tier, and the cache key is
  the immutable name (Ch 7 "Read path and caching", Ch 11 "Caching tiers").
- Where: `KeepLocal` is refused in durable mode "because a kept copy may be older than the manifest"
  (`internal/vactor/host.go:191-192`).
- What goes wrong: a copy cached by `(generation, sha256)` cannot be stale. It is either the generation
  the manifest names, or unused. Coldload found that fetch time is close to all of the time to ready, at a
  cap of about 250–290 MB/s (`knowledge/coldload/2026-11-04-sizes/`). So every re-warm of an idle
  database pays a full download that it does not need.

**G7. Requests and their cost are not measured per operation.**
- Book: instrument every S3 call by operation, status, size and latency. Alert on cost per operation
  (Ch 11 "Measuring", Ch 14).
- Where: `Host.Stats` counts write-backs, fetches and bytes (`host.go:423-430`), and `WriteBackResult`
  records timings. Nothing counts requests by kind or outcome (412, 404, 5xx, retried).
- Request counts today:

  | Operation | Requests |
  |---|---|
  | Write-back | 4: PUT generation, HEAD, GET manifest (the pre-read at `durable.go:140`), PUT manifest |
  | Stamp | 3: GET in `durable.Stamp`, GET pre-read, PUT |
  | Activation | 2: GET manifest, GET generation |

- Example: 10,000 dirty databases on a 30 s RPO make about 1,300 requests/s. On AWS that is a real cost
  line, and it gets close to per-prefix limits (Ch 2: 3,500 PUT/s).

**G8. One static admin credential, plain HTTP.**
- Book: one least-privilege credential per component, TLS everywhere (Ch 13).
- Where:
  - `deploy/seaweedfs/s3.json` grants a single identity `Admin, Read, List, Tagging, Write`.
  - `objstore.New` uses `credentials.NewStaticV4` with no `Secure` flag, so it speaks HTTP
    (`objstore.go:68-71`).
- What goes wrong: once agentd runs on several VMs, each agent's credential can delete or list every
  database. The agent itself never needs `List` or `Delete`; only GC does (G1).

**G9. No runbook or tool for recovery from storage.**
- Book: write a recovery procedure for each component, with diagnostic commands, and rehearse it (Ch 12
  "Recovery procedures", Ch 14 "Runbooks"). Keep the history of the mutable pointer, as Git's reflog and
  bucket versioning do (Ch 9).
- Where: a manifest that is lost, corrupted or wrongly published cannot be rebuilt today:
  - Generation objects carry only `x-amz-meta-sha256` (`durable.go:90`). They carry no epoch, and no
    written-at time apart from LastModified.
  - No command lists a key's generations, checks them against the manifest, or finds orphans.

**G10. The WAL-shipping design is missing the book's segment safeguards.**
- Book: a segment has a header (magic, start sequence, epoch) and a footer CRC. The flush interval bounds
  how long data waits. Recovery ignores segments that carry a stale epoch (Ch 5, Ch 11).
- Where: `knowledge/writeback/2026-11-05-wal-shipping.md` names segments `gen/<G>/wal/<seq>.wal` with no
  epoch. It also suggests LIST to find segments past `through`.
- What goes wrong: if the book's recovery step "adopt valid orphans" were copied, a new owner would adopt
  segments a zombie uploaded after the stamp. Fencing forbids that (S2).

## 4. Refactor items

Order is top to bottom. "Pre-CP" means before or alongside control-plane step 1 (wiring agentd). "Post-CP"
means after step 1, and before the step-4 gate where a VM is killed and its databases are restored.

| Item | Title | Size | Depends on | When |
|---|---|---|---|---|
| OS-01 | Own the retries: classified errors, no hidden SDK retries | M | — | Pre-CP |
| OS-02 | Reconcile ambiguous stamps and uploads | S | OS-01 | Pre-CP |
| OS-03 | Per-backend S3 compatibility suite | M | — | Pre-CP |
| OS-04 | Key namespace v2: no key nests in another | S | — | Pre-CP (before real keys exist) |
| OS-05 | Epoch-qualified generation names and self-describing metadata | M | OS-04 | Pre-CP |
| OS-06 | Generation GC: owner-side retention plus an orphan sweeper | L | OS-04, OS-05, OS-08 | Post-CP (§3.8) |
| OS-07 | Server-checked upload integrity | S | OS-03 | Pre-CP |
| OS-08 | Per-operation S3 metrics and request accounting | S | — | Pre-CP |
| OS-09 | Drop the redundant manifest pre-read | S | OS-08 | Post-CP, only if OS-08 shows it matters |
| OS-10 | Generation cache on local disk | M | OS-05 | Post-CP |
| OS-11 | Scoped credentials and TLS | S | OS-06 | Post-CP (before more than one VM) |
| OS-12 | Storage recovery tool and runbook | M | OS-05 | Post-CP |
| OS-13 | WAL-shipping design amendments | S (doc) | OS-05 | Before any WAL code |

### OS-01: Own the retries (fixes G2) — **Done** (20c4311)

- **Goal:** every S3 request is sent once per decision our code makes. Retries happen in one place and
  follow guide rules R1–R5.
- **Files:**
  - `internal/objstore/objstore.go` (`Config`, `New`)
  - `internal/objstore/durable.go`
  - a new `internal/objstore/classify.go`
- **Change:**
  - Add `Config.MaxRetries` and pass it to `minio.Options.MaxRetries`.
    - The durable client uses 1. Check that minio-go then does not retry; if it treats 1 as "default",
      set `minio.MaxRetry` for this client, or wrap the transport.
    - Coldload keeps the SDK default.
  - Add `objstore.Classify(err) Kind`, where `Kind` is one of `Transient` (5xx, 429, 503 SlowDown,
    network error, timeout), `Precondition` (412/409), `NotFound`, `Permanent` (other 4xx) or `Canceled`.
  - Wrap with `errs.With("s3_kind", kind)` so callers stop parsing messages.
  - Add `objstore.retryIdempotent(ctx, op, fn)`, with jittered exponential backoff (100 ms base, 5 s cap,
    5 attempts), for GET, HEAD and LIST only. It reuses the `internal/retry` jitter math rather than
    copying it.
  - Conditional PUTs are never retried inside the adapter.
- **Acceptance:**
  - A spec table for `Classify` with on/off points: 408 vs 409, 412, 429, 499, 500, 503, `SlowDown`,
    `AccessDenied`, `NoSuchKey`, `context.Canceled`.
  - An `httptest` fake S3 that returns 503 once and then 200: a GET succeeds; a conditional PUT surfaces
    `Transient` and the fake saw exactly one PUT.

### OS-02: Reconcile ambiguous stamps and uploads (fixes G2) — **Done** (6d768de)

- **Goal:** every conditional write treats "error or 412" as "unknown" until a re-read settles it, as
  `Host.publish` already does.
- **Files:**
  - `internal/vactor/writeback.go` (`Stamp` at 447-479, the upload loop at 207-215)
  - `internal/durable/durable.go` (`Stamp`, 245-270)
  - `internal/durable/durabletest/store.go` (new faults)
- **Change:**
  - In both Stamp paths, re-read the manifest after any error. Return success if `stored.Same(next)`, and
    set `e.stale` only when the re-read shows a different manifest.
  - In the upload loop, on `ErrExists`, HEAD the object. If its `x-amz-meta-sha256` matches `d.SHA256`,
    treat it as our own upload and do not upload again.
  - New faults:
    - `ApplyThenStale`: the publish applies, then returns `ErrStale`. This is the effect of an SDK retry.
    - `LoseUploadReply`: the generation is stored, then `ErrInjected` is returned.
- **Acceptance:**
  - Spec tests: warm and cold Stamp with `ApplyThenStale` both succeed, and `e.stale` stays false.
  - A test that a duplicate upload is detected by its hash.
  - `FuzzDurableHostModel` and `FuzzOwnershipModel` get the new faults as ops, and invariants I1–I3
    still hold.

### OS-03: Per-backend S3 compatibility suite (fixes G4) — **Done** (b6b8ae2; SeaweedFS 4.47 single-node only, MinIO and multi-node not run)

- **Goal:** one env-gated test that records, for each backend and version, every S3 behavior we rely on.
  It must pass before a backend or version is used.
- **Files:**
  - a new `internal/objstore/compat_test.go`
  - a dated note per run: `knowledge/writeback/<date>-s3-compat-<backend>.md` (one note per run, as
    knowledge rules require)
- **Change:** add `TestS3Compat` (gated on `COLDLOAD_S3`, plus `S3_BACKEND` for the label). It checks the
  list in [guide.md §7](guide.md#7-compatibility-checks-per-s3-implementation): X1–X12.
  - Multi-node SeaweedFS needs a compose file with master, volume, filer and S3 as separate services. That
    goes under `deploy/seaweedfs-cluster/` and is owned by the coldload/writeback owner.
- **Acceptance:**
  - The suite passes on single-node SeaweedFS and on MinIO.
  - The multi-node result is recorded, pass or fail.
  - The control-plane plan's §4 assumption "S3 conditional PUT is atomic" links to the note.

### OS-04: Key namespace v2 (fixes G3) — **Done** (3770dff)

- **Goal:** no object name of one database is a prefix of, or lies inside, another database's namespace.
  Key limits agree across packages.
- **Files:**
  - `internal/durable/durable.go` (`ValidateKey`, `GenObject`, `ManifestObject`, plus a new `KeyPrefix`)
  - `internal/owner/ports.go` (`MaxKeyLen`)
  - the contract suite
- **Change:**
  - New layout: `dbs/<h2>/<enc(key)>/…`.
    - `enc` is URL path-segment escaping, so `/` becomes `%2F`. The key stays readable in listings.
    - `h2` is the first 2 hex digits of sha256(key). It gives 256 GC/inventory shards, and spreads load
      across S3 prefixes (Ch 4 "Key layout", Ch 2 "Prefix sharding").
    - The key's namespace ends with the delimiter `/` (`KeyPrefix(key) = "dbs/<h2>/<enc>/"`).
  - A single `MaxKeyLen = 120` lives in `durable` and is used by `owner`.
  - No migration is needed: no production data exists, and the bench and test data are disposable. Say so
    in the commit.
- **Acceptance:**
  - A property test (`FuzzKeyPrefixDisjoint`): for any two distinct valid keys a and b, neither
    `KeyPrefix(a)` nor any object of a starts with `KeyPrefix(b)`.
  - On/off tests at length 120 and 121.

### OS-05: Epoch-qualified generations, self-describing objects (G2, G9; prepares G1) — **Done** (a2b5d72; Content-MD5 folded into the sha256 pass in 10ed2b8)

- **Goal:**
  - Generation names cannot collide across owners.
  - Every orphan can be attributed to the epoch that wrote it.
  - Any generation, on its own, holds enough to rebuild a manifest.
- **Files:**
  - `internal/durable/durable.go` (`GenObject`, `Manifest.Validate`)
  - `internal/objstore/durable.go` (`PutGeneration` metadata)
  - `internal/vactor/writeback.go` (`upload` passes the epoch)
- **Change:**
  - `GenObject(key, gen, epoch) = KeyPrefix(key)+"gen/<gen 20d>-e<epoch 20d>.db"`.
  - User metadata: `sha256`, `epoch`, `generation`, `key`, `format=sqlite3-v1`.
  - The 8-try `ErrExists` loop stays only as a defensive measure. Across owners it can no longer trigger,
    because one owner holds each epoch (safety argument, step 1).
- **Acceptance:**
  - Contract-suite cases:
    - two epochs upload the same generation number and both succeed;
    - the metadata round-trips;
    - `Validate` rejects an object whose name carries an epoch greater than the manifest's. The rule is
      `objEpoch <= m.Epoch`, because a Stamp raises the manifest's epoch without renaming the object.

### OS-06: Generation GC (fixes G1) — **Done** (092ac13; sweeper runs as `dbtool gc`; "bucket bytes flat" is `TestBucketBytesFlatThroughS3` on SeaweedFS, not a 10-minute wbbench run)

- **Goal:** storage per database is bounded by about (K+1) × its size, plus orphans younger than the
  grace window. The guide's GC rules GC1–GC6 are enforced in code.
- **Files:**
  - `internal/durable` (a new pure `PlanGC(manifest, objects, now, policy) []string`)
  - `internal/durable.Store` (a new `DeleteGenerations(ctx, key, names)` port method, and `ListGenerations`
    for the sweeper only)
  - `internal/objstore/durable.go`
  - `internal/vactor` (owner-side retention after `publish` succeeds)
  - a sweeper command or a `RunMaintenance` mode
- **Change:**
  1. **Owner-side retention, without LIST.** After a successful publish of G, under `e.wb` (so it is
     serialized with publishes and drained by Fence), delete G−K−1. That key is computable from the
     manifest history we hold in `e.man`, or we keep the last K names in the entry. Default K = 2. The
     manifest's current generation and `e.pending` are never deleted.
  2. **Orphan sweeper** (Post-CP, one shard `dbs/<h2>/` at a time). Per key, read the manifest (M, with
     LastModified T_M) and list `gen/`. Delete an object o only when all of these hold:
     - o is not M's generation;
     - o is not among M's K predecessors;
     - `o.LastModified + grace < T_M`;
     - `now − o.LastModified > grace`.
     The default grace is 2 × `WriteBackTimeout`, which is 10 min. Why this is safe: every publish that
     names o is conditional on a manifest version read before o was uploaded. Once the manifest has been
     rewritten after o, that version is gone, and by guide rule W3 it can never come back. The grace
     window covers a publish request that was delayed in flight. It also covers `Host.publish`'s
     "adopt pending, then retry" path (`writeback.go:270-272`). Both are bounded by the write-back timeout.
  3. Deletes go in batches through `RemoveObjects`. GC never touches the manifest.
- **Acceptance:**
  - `PlanGC` spec table with on/off points at the grace boundary and at K.
  - `FuzzDurableHostModel` gets a `GC` op and a new invariant: "the manifest's generation, and every
    generation an in-flight activation read, is fetchable". Activations that read the manifest before GC
    ran must succeed, or fail with `ErrNotFound` only for keys this host does not own.
  - wbbench reports bytes in the bucket per database, flat over a 10-minute run.

### OS-07: Server-checked upload integrity (fixes G5) — **Done** (c9f25b7)

- **Goal:** the server rejects a PUT whose body differs from what we hashed.
- **Files:** `internal/objstore/durable.go`
- **Change:**
  - Set `PutObjectOptions.SendContentMd5 = true` on the generation and manifest PUTs. Compute MD5 in the
    same pass as SHA-256 in `vactor.hashFile`, or let minio-go compute it.
  - If OS-03 shows that a backend ignores `Content-MD5`, record that, and keep the HEAD size check plus
    the fetch-side hash as the only protection.
  - Optionally, a `GET`-back hash on the first generation after each process start, to sample the check.
- **Acceptance:** an `httptest` proxy that flips one body byte makes `PutGeneration` fail with
  `Permanent` (BadDigest), and no object exists afterwards.

### OS-08: Per-operation metrics (fixes G7) — **Done** (873461f; in `objstore.Store.Metrics`, not a `durable.Metered` decorator; Host.Stats `Requests`/`RequestCostUSD` and wbbench `reqs`/`$/M` columns done in CP-8 via `durable.RequestCounter`: 4 requests, $10.80 per million write-backs)

- **Goal:** for each S3 operation kind (GET manifest, GET generation, PUT generation, PUT manifest, HEAD,
  LIST, DELETE), count requests by `Kind`, plus bytes and a latency histogram.
- **Files:**
  - a new `internal/durable/metered.go`: a decorator like `vactor.checkedStore` (`writeback.go:608-631`),
    always on
  - `Host.Stats`
  - wbbench output
- **Change:**
  - Log one `logx` line per failed or retried request: op, key, bytes, duration, status, kind. Successful
    requests are counted only.
  - wbbench prints requests per write-back and an estimated $/million write-backs at AWS list prices, as
    a column (Ch 11 "report cost with performance").
- **Acceptance:**
  - A unit test checks the counts across a scripted write-back on the fake.
  - A wbbench run shows 4 requests per write-back, matching G7. The note goes to `knowledge/writeback/`.

### OS-09: Drop the redundant manifest pre-read (G7, optional) — **Skipped**: the pre-read is 1 GET of 5 requests per write-back (~5% of its AWS request cost: GET $0.0004/1k vs PUT $0.005/1k) and a few ms next to a whole-file upload; it keeps the stored-epoch check as defence in depth and is what lets an operator replace an unreadable manifest (OS-12)

- **Goal:** a write-back makes 3 requests instead of 4, and a Stamp makes 2 instead of 3.
- **Change:**
  - `PublishManifest` stops calling `ReadManifest` (`durable.go:140-151`). The `If-Match` already
    rejects any manifest that changed, including a Stamp to a higher epoch.
  - The "lower epoch on the current version" check moves fully into the pure `CheckPublish`/`CheckStamp`.
    Every caller already runs them (`writeback.go:257`, `durable.go:255`, `writeback.go:465`).
  - Change the contract case "publish at a lower epoch than stored is ErrStale even on the current
    version" to a caller-level test. Record the port change in the `durable.Store` doc comment.
- **Acceptance:** all suites and fuzzers pass, and the OS-08 counts drop by one per publish.
- **Do it only if** OS-08 shows publish requests matter. Otherwise the defense in depth is worth one GET.

### OS-10: Local generation cache (fixes G6) — **Done** (9c79330; `Options.GenCacheBytes`, off by default; 256 MiB re-warm ~1 s → 1–4 ms on SeaweedFS)

- **Goal:** a re-warm of an idle database whose manifest has not moved costs 1 GET and no download.
- **Files:**
  - `internal/vactor/host.go` (replace the `KeepLocal` refusal at 191-192)
  - `writeback.go` (`release`, `fetchDurable`)
- **Change:**
  - On a clean passivation, keep the file renamed to `<cache>/gen/<sha256>.db`. A byte-budget LRU limits
    the cache.
  - On activation, read the manifest. If a cached file with the manifest's sha256 exists, hard-link or
    copy it into place. Otherwise fetch it.
  - A dirty passivation uploads first. The uploaded file is the new generation, so it gets cached under
    its hash.
  - The cache is never trusted without the manifest naming its exact hash.
- **Acceptance:**
  - Spec cases: hit, miss, a hash mismatch counts as a miss, and eviction at the budget on/off point.
  - `FuzzDurableHostModel` with the cache on: invariants unchanged.
  - Coldload/wbbench: re-warm latency for a 256 MiB database drops from about 1 s to under 10 ms.

### OS-11: Scoped credentials and TLS (fixes G8)

- **Change:**
  - `deploy/seaweedfs/s3.json` gets three identities, each scoped to the bucket (`Read:<bucket>`, etc.):
    - `agent`: Read, Write;
    - `gc`: Read, List, Write;
    - `bench`: Admin, for dev only.
  - `objstore.Config` gains `Secure bool` and an optional CA file.
  - agentd refuses `Secure=false` unless `-dev` is set.
- **Acceptance:**
  - OS-03 includes a check that the `agent` identity cannot LIST.
  - An agentd config test covers the TLS refusal.

### OS-12: Storage recovery tool and runbook (fixes G9) — **Done** (8dc01c5; runbook `knowledge/writeback/2026-11-05-storage-runbook.md`)

- **Change:** add `cmd/dbtool` with these subcommands:
  - `inspect <key>`: shows the manifest, the generations with their metadata, and orphans.
  - `verify <key>`: fetches the manifest's generation and checks its hash.
  - `rebuild-manifest <key> --gen G --epoch E`: requires the epoch from the placement row. It publishes
    only with `If-None-Match` when no manifest exists, or `If-Match` on a manifest the operator saw.
- The runbook goes in `knowledge/writeback/<date>-storage-runbook.md`. It covers: corrupt generation, lost
  manifest, orphan growth, cost spike, 503 SlowDown storm.
- **Acceptance:** an integration test against the fake and SeaweedFS: corrupt a generation and see
  `verify` fail; delete the manifest, rebuild it, and activate.

### OS-13: WAL-shipping amendments (fixes G10, docs only) — **Done** (7861533; `knowledge/writeback/2026-11-05-wal-shipping-amendments.md`)

- Amend the WAL note before any code is written:
  - Segment key: `gen/<G>-e<E>/wal/<seq 20d>-e<E>.wal`. Segments are create-only.
  - Segment header: magic, key, generation, epoch, start sequence, WAL salt. Footer: end sequence, CRC32C.
  - Restore never adopts a segment past `through` that has a different epoch. A segment with the same
    epoch is adopted only by the same process after a restart, before it reconciles, and only if
    continuity and CRC pass.
  - Use a max ship interval plus a size trigger (Ch 11: 4–16 MB for slow streams).
  - Find segments past `through` with a computed-key HEAD probe (`seq+1`, `seq+2`, …), not LIST.

## 5. Testing implications

The approach is the one in `knowledge/testing/README.md` and `~/AI/writeups/go-testing.md` (spec tables,
fakes with fault switches, contract suites, model fuzzing). This plan adds no new test style.

| Item | Spec tables | Fakes / faults | Contract suite (fake, in-process S3, SeaweedFS) | Fuzz / model | Load (wbbench / coldload / sqlbench) |
|---|---|---|---|---|---|
| OS-01 | `Classify` on/off | `httptest` S3 returning 503/412/timeout | — | — | — |
| OS-02 | Stamp outcomes | `ApplyThenStale`, `LoseUploadReply` | duplicate-upload-by-hash | both model fuzzers get the faults | — |
| OS-03 | — | — | **is** the compat suite | 200×N concurrent rounds per backend | — |
| OS-04 | length 120/121 | — | layout cases | `FuzzKeyPrefixDisjoint` | — |
| OS-05 | `Validate` epoch rule | — | cross-epoch same-gen, metadata round trip | — | — |
| OS-06 | `PlanGC` grace and K | GC racing an activation | delete/list cases | `GC` op plus invariant | bucket bytes flat over 10 min |
| OS-07 | — | byte-flip proxy | Content-MD5 case | — | hashing cost in wbbench |
| OS-08 | counts per scripted op | — | — | — | requests and $ per write-back |
| OS-10 | hit/miss/budget | hash-mismatch file | — | cache on in the model | re-warm latency |
| OS-12 | — | corrupt object | `verify`/`rebuild` on SeaweedFS | — | — |

Run benchmarks and any fuzzing longer than a few seconds under `scripts/lock.sh results/.bench-lock`.

## 6. Out of scope / not adopted

- **Leases and clock-based expiry (Ch 10).** The fencing design has no clocks on purpose (safety argument).
  The book's lease listing is also wrong (it takes over with `If-None-Match`). If leases are ever added, to
  bound how long a zombie can serve stale reads, they live in Postgres placement and not in S3 objects.
  That is the control-plane plan's open item.
- **S3 as the placement authority (Ch 10, Ch 16).** Placement belongs in Postgres: it needs primary reads,
  a CAS in a single statement, and moves inside one transaction (fencing's Postgres note). Ch 10's own
  rule agrees: when coordination must be atomic with data, use the database.
- **Bucket versioning and Object Lock (Ch 9, 13, 14).** Generations already are application-level
  versions, and OS-06 keeps K of them. SeaweedFS support for versioning and Object Lock is not verified.
  OS-03 records it but nothing depends on it.
- **A policy that requires conditional writes on PutObject (Ch 13).** The printed policy is wrong, and
  SeaweedFS has no bucket policies of that kind. OS-03 plus the adapter enforce it instead.
- **LSM/SSTables, Bloom filters, Parquet, queues and Git backends (Ch 6–9).** SQLite is the storage
  engine, and we store whole files or WAL segments. Only the patterns transfer.
- **Multi-region and CRR (Ch 12).** No multi-region requirement exists yet.
- **CDN tier, SSE-KMS and IRSA specifics (Ch 11, 13, 16).** These are AWS-specific. Revisit them if the
  deployment moves to AWS.
- **Multipart uploads for durable generations.** We refuse files over 5 GiB today (`durable.go:81`). We
  adopt multipart only after OS-03 shows that `If-None-Match` on `CompleteMultipartUpload` works on the
  backend in use. At that point, add a lifecycle rule that aborts incomplete MPUs after 1 day (Ch 14).
