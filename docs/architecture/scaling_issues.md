# Scaling issues in the current architecture

Status: analysis · Date: 2026-11-04 · Baseline: `main` at `fac3e3e`. Companion to
[`scale-out-direct-actors.md`](scale-out-direct-actors.md) §5.1, which has the full hotspot table. Items
marked **(inferred)** were read from code, not run.

This lists what obviously will not scale as written: places where load from every tenant meets in one process,
one lock, one connection, or one file. Ranked by how soon they bite.

## 1. Everything runs in one process on one machine

- One API process serves every app and tenant. Tenant files live on local disk (`TENANTS_DIR/<tenant>.db`),
  and the per-file writer lock is an in-process `Semaphore(1)`. A second server instance can't share the work:
  it would see the same files only on the same disk, and two processes writing one file would fight over
  SQLite's lock (`busy_timeout=5000`) instead of queueing.
- There is no placement: the router maps a tenant to a local path (`platform/tenants/routing.ts`), and
  `router: "control-plane"` is reserved in config but not built.
- Consequence: capacity is one machine's CPU, disk and file descriptors, and there's no way to move a tenant
  off a busy machine or survive losing it.

## 2. All SQLite I/O shares one JavaScript event loop

- Writes, primary-key reads, `BEGIN` and `COMMIT` go through the synchronous FFI path on the main thread
  (`platform/sqlite.ts:48`, `:917-919`; `platform/sqlite_ffi/database.ts:174`).
- With `synchronous=FULL`, each commit's fsync runs on that thread. **(inferred)** A slow fsync, or a wait of
  up to 5 s on a locked file, stalls every request for every tenant, not just the one writing.
- Consequence: total write throughput is capped by one thread's fsync rate, however many tenants there are.

## 3. Auth hits Postgres on the request path through narrow points

- **API-key cache under one global lock** (`platform/http/auth.ts:110-139`). A single `Semaphore(1)` covers
  the cache lookup _and_ the Postgres query on a miss. One miss blocks every other request's auth, cache hits
  included. Capacity is 1024 entries with FIFO eviction, so above 1024 active keys the cache thrashes and
  nearly every request becomes a serialized Postgres query. Any admin key change clears the whole cache.
- **Session auth opens a new Postgres connection per request** (`platform/auth/service.ts:37-69`). Each Better
  Auth request creates a client with `max: 1`, runs `getSession` with caching off, a `memberships` query and
  an uncached `getTenant`, then closes the connection. Connection count and handshakes grow linearly with
  traffic. **(inferred)** The rate limit is one shared bucket for all callers.
- **One small registry pool** (`platform/tenants/store.ts:278`): postgres.js defaults, **(inferred)** 10
  connections, shared by auth misses and admin calls.

## 4. One global lock in front of every tenant's database handle

- The tenant handle cache (`platform/sqlite.ts:1015-1145`) holds 64 open handles with a 60 s idle expiry,
  behind one `Semaphore(1)`.
- A cold open runs _inside_ that lock: resolve, prepare (`BEGIN IMMEDIATE` plus DDL) and open (`:1099-1117`).
  Closing an evicted handle also runs under it. One cold tenant blocks every warm tenant's acquire and
  release.
- With all 64 handles leased, the next request waits with no timeout, and mutations have no request deadline
  (`platform/http/handlers.ts:175`).
- A second per-app lock, `prepareLock` (`platform/layers.ts:226-238`), sits on every tenancy query. The
  router's `locations` map (`platform/tenants/routing.ts:64`) grows with every tenant ever seen and is never
  evicted.
- Consequence: more than 64 active tenants per app means constant churn, with the churn serialized.

## 5. Process-wide read limits shared by all tenants

- 32 concurrent document reads and 1000 pending for the whole process (`platform/document_reader.ts:77`,
  `:417-422`). One tenant's slow scans (5 s statement deadline) can hold all 32 permits while every other
  tenant gets `QueueFull`.
- Every read opens and closes the file again (`:156`); there is no reuse of read connections.

## 6. Backups don't scale with data size or tenant count

- `VACUUM INTO` runs on the writer's queue (`platform/sqlite.ts:973-985`), so a backup blocks that database's
  writes for the whole snapshot.
- The snapshot is read fully into memory (`platform/backup.ts:322`) and uploaded as one PUT, with no multipart
  upload. Memory use grows with database size, and a large file can't finish inside the 60 s request timeout.
- Backups are per app, one at a time, in a sequential sweep (`platform/server.ts:174-188`).
- **(inferred)** In tenancy mode the snapshot path has no `TenantContext` (`platform/sqlite.ts:1078-1080`), so
  tenant databases may not be backed up at all.

## 7. Single serial helpers and single-file sinks

- **Reader child:** one per server, and it answers strictly one request at a time (`platform/reader.ts:3`,
  `:236`). It handles health pings, log ingest and analysis. A long ingest blocks `/health` until it times out
  after 30 s. After 5 restarts in 10 minutes the server treats it as fatal.
- **Event-log ingest:** one `eventlog.duckdb` file with one writer (`platform/eventlog/ingest.ts:465`).
- **Logging:** one queue of 10 000 events and one JSONL file per process (`platform/logging/logger.ts:326`,
  `file_sink.ts:160`). Under load it drops events for every tenant, and the log is not split by tenant.
- **IPC outbox:** one bounded queue of 1000 to the child (`platform/ipc.ts:226`, `:477`).

## 8. One writer per tenant is a hard ceiling

- Each tenant has exactly one SQLite file, so a tenant's writes and its heavy scans share one WAL and one
  writer lock. A busy tenant's write rate tops out at one file's commit rate, and a tenant can't split hot and
  cold data. Both scale-out proposals note this: lanes keeps it, while planes and option 4 lift it by letting
  a tenant own many databases.

## What each fix depends on

| Issue                        | Fixable on one node?                                                                        | Needs scale-out work                          |
| ---------------------------- | ------------------------------------------------------------------------------------------- | --------------------------------------------- |
| 2. One event loop for SQLite | Yes: statement lanes / worker processes (planes M3)                                         | No                                            |
| 3. Auth bottlenecks          | Yes: pooled connections, cached sessions, per-key single-flight in place of the global lock | Option 4 moves it to ticket issuance          |
| 4. Global handle-cache lock  | Yes: per-tenant single-flight, timeouts, a bounded router map                               | Option 4 replaces it with per-database actors |
| 5. Shared read caps          | Yes: per-tenant caps under a process ceiling, reused read connections                       | No                                            |
| 6. Backups                   | Yes: read-only snapshot connection, streaming/multipart upload, per-tenant backups          | Recorded manifests (planes M6)                |
| 7. Serial helpers and sinks  | Yes: separate health from analysis, per-lane logs                                           | No                                            |
| 1. One process, one machine  | No                                                                                          | Placement, routing, moves (planes / option 4) |
| 8. One writer per tenant     | No                                                                                          | Many databases per tenant (planes / option 4) |

Issues 2–7 can be fixed before any scale-out work, which matches planes-fable's M2 build order: in-node fixes
first, then placement.
