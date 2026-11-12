# Scaling issues vs. the mansio-server actor approach

- **Date:** 2026-11-05
- **Question:** Compare `docs/architecture/scaling_issues.md` with `../mansio-server`. What could mansio gain
  by building on that approach?
- **Read at:** mansio `addda3d`; mansio-server working tree as of 2026-11-05 (`STATE.md` last updated
  2026-11-04)
- **Evidence:** every number below was measured by mansio-server and comes from its knowledge folder
  (`knowledge/sqlbench/README.md`, `knowledge/sqlbench/2026-11-04-cloudflare-durable-objects.md`,
  `knowledge/coldload/2026-11-04-sizes/README.md`). Machine: Apple Silicon, 18 cores, macOS, Go 1.26,
  `modernc.org/sqlite`. Nothing was rerun for this entry. The mansio facts come from reading
  `platform/sqlite.ts`, `platform/services.ts` and `scaling_issues.md`. **No mansio measurement exists under
  the same workload.**

## What mansio-server is

mansio-server is a greenfield Go proof of concept that imports nothing from mansio. It runs one goroutine per
SQLite database, with no lock shared between databases. Each actor has:

- a bounded mailbox; a full mailbox answers "shed" instead of blocking the caller,
- group commit: all queued writes in one `BEGIN IMMEDIATE … COMMIT`,
- reads after the commit or on an optional read pool,
- a statement timeout (200 ms in the runs), plus a queue deadline that drops requests that waited too long,
- passivation when idle.

`internal/vactor` adds virtual actors on top. Activating a database fetches it from object storage
(SeaweedFS/S3), and the first caller's activation is shared by every concurrent caller for the same key.

Its stated goal (`STATE.md`) is mansio's scaling issues 2–5.

## Issue by issue

| mansio issue (`scaling_issues.md`)                     | What mansio-server measured                                                                                                                                                                                                                                                       | Carries over?                                                                                                                                     |
| ------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| **2. One event loop for all SQLite I/O**               | No shared lock: ~20k ok req/s at 99% success across 1k databases, against ~220 with one global lock. One actor reaches ~151k point reads/s and ~162k ops/s at 20% writes. CPU (~14 of 18 cores) is the next limit.                                                                | The design carries over; the Go runtime does not. In Deno, "many threads" means workers (lanes, planes M3).                                       |
| **commits under `synchronous=FULL`** (part of issue 2) | Group commit, 288 callers on one database: FULL 16.5k → 292k writes/s (18×); NORMAL 58k → 319k (5.5×). macOS FULL does not do `F_FULLFSYNC`, so Linux will be slower.                                                                                                             | mansio commits once per mutation under FULL (`platform/sqlite.ts:918`), so this is the largest single gain on offer. It needs savepoints (below). |
| **4. Global handle-cache lock, cold open inside it**   | Open plus first query takes 0.3–0.5 ms at every file size. A per-key single-flight activation turned 500 callers on one cold database into 1 fetch.                                                                                                                               | Yes, directly. The open is cheap; the global lock is the problem. Per-tenant single-flight with no global lock.                                   |
| **5. Shared read caps; a new connection per read**     | Each open database costs ~3 fds, ~2–3 goroutines and ~0.6–0.8 MB. One connection per database causes head-of-line blocking behind slow queries; 4 read connections fixed p50 at 5k dbs (21 → 0.11 ms) but RSS 3.8 → 7.1 GB, fds 15k → 55k, and more slow queries running at once. | Yes. Keep read connections open per tenant; make read pools a per-tenant setting, not a default. Cap the expensive query classes separately.      |
| **1. One process, one machine**                        | Coldload: fetch is 69–99% of time-to-ready, ready ≈ 1.2 ms + size ÷ ~250 MB/s; concurrency past ~4 only queues files ≥ 1 MiB; bound downloads by bytes in flight.                                                                                                                 | Yes. This is the evidence behind option 4's activate-on-first-use `actor-directory`, and the measured cost of moving a tenant.                    |
| **6. Backups**                                         | Not built. The Durable Objects note recommends shipping the WAL (Litestream-style) in place of fsync-plus-snapshot.                                                                                                                                                               | Direction only. It would replace `VACUUM INTO` on the writer queue. Unmeasured.                                                                   |
| **8. One writer per tenant**                           | One actor's ceiling for cheap statements is ~150–300k ops/s, 150–300× Cloudflare's per-object soft limit.                                                                                                                                                                         | Makes issue 8 much less urgent: slow statements and CPU hit first. Many databases per tenant can wait.                                            |
| **3. Auth**, **7. serial helpers**                     | Not covered.                                                                                                                                                                                                                                                                      | No. Option 4's tickets cover 3.                                                                                                                   |

## The constraint: handlers run inside the transaction

mansio-server's actor batches **independent SQL strings**. A mansio mutation is a **TypeScript handler**
(`examples/tic-tac-toe/functions.ts`) that `StorageService.mutation` (`platform/services.ts:344`) runs inside
one `BEGIN IMMEDIATE` transaction. The handler reads, decides, and writes, with `_version` optimistic
concurrency, while holding the write lock. Two things follow:

1. **A Go agent can't take mansio's writes as is.** The handler would run in TypeScript and send each
   statement to Go, so the write lock would be held across RPC round trips. That is worse than today. To make
   it work, the handlers would have to run inside the agent (embedded JS such as goja, or a port), or the
   mutation contract would have to change to "intent in, statements out".
2. **Group commit needs savepoints.** To batch N handlers into one COMMIT, run each inside
   `SAVEPOINT h_i … RELEASE` and roll back only the failing handler's savepoint, then commit once. That holds
   replies until the commit, which is mansio-server's group commit and Cloudflare's output gate. Batch size is
   bounded by handler time, not statement count. _Unverified_ for mansio: no one has measured savepoint
   overhead on the FFI binding.

Reads have no such problem. A query handler is read-only, so a Go reader that takes compiled
`{ text, values }` works today. It fits behind the existing `ReaderClient` seam (the server already sends
ping, ingest and analysis over IPC).

## What we could get, three ways

| Path                                               | Gets                                                                                                                                                                                                                               | Costs / risks                                                                                                                                                                                                                                                                                       |
| -------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **A. Port the actor pattern into mansio (Deno)**   | Fixes 4 and 5 in place: per-tenant actor with mailbox, shedding, queue deadline, single-flight open, kept-open read connections, no global lock. Adds group commit via savepoints. Scale-out issues 2–7 are all single-node fixes. | Issue 2 is still one JS thread until lanes or workers land (planes M3). Same Effect seams, so `Storage`'s conformance suite (`tests/core_test.ts:446`) keeps it honest.                                                                                                                             |
| **B. Go data plane for reads first, writes later** | Reads leave the event loop completely, using measured Go actor numbers. The path to option 4's VM agent, with `vactor` as the activation layer.                                                                                    | Two languages. Writes are blocked by the handler constraint. The compiler is TypeScript (`core/compile/`, 624 lines, pure). Either the TS front compiles and ships SQL, or the compiler is ported (smaller if SQLite-only: [compiler entry](../compiler/2026-11-05-sqlite-only-simplification.md)). |
| **C. Full Go rewrite**                             | One runtime.                                                                                                                                                                                                                       | Not justified by the evidence: the SDK, compiler and document semantics aren't the bottleneck. The locks and the single thread are, and A and B address them.                                                                                                                                       |

## Recommended next steps

1. **Measure mansio itself under mansio-server's workload shape**: many tenants, open-loop rate, ~1% slow
   statements. sqlbench's global-lock baseline was a deliberate worst case and is not mansio (mansio-server's
   own caveat). Without this number, the size of the gain is a guess. `perf/` and k6 already exist here.
2. **Path A, issue 4 first**: per-tenant single-flight open with a bounded router map. It is small, local, and
   backed directly by the coldload single-flight result.
3. **Prototype savepoint group commit** in `Storage` behind the conformance suite, measured under FULL on
   Linux.
4. Decide B only after 1–3, with option 4's actor-directory as the target shape.

## Correction (2026-11-05, same day)

Read `../mansio-server/internal/vactor/host.go:322-349`. `vactor` only goes cold → warm. `Passivate` closes
the database and deletes the local file (unless `KeepLocal`). It **does not upload** it, so a database that
takes writes while warm loses them on passivation. That's fine for coldload's read-only benchmark, but it is
not a cold-storage tier. There's no idle or budget eviction policy in `vactor` either.
`actor.Config.IdleClose` closes the connection only. mansio-server also has **no network listener**: actors
are called in-process through `(*Actor).Do`. Missing pieces for a real data plane: a network front, write-back
durability (upload on passivate and WAL shipping, with generations), and single-owner fencing. See
`docs/architecture/scale-out-go-greenfield.md` §8.
