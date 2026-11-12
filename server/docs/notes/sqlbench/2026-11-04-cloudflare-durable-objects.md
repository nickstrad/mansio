# How Cloudflare Durable Objects get their per-object speed, and what we can reuse (2026-11-04)

Sources (read 2026-11-04):
- Limits: https://developers.cloudflare.com/durable-objects/platform/limits/
- "Zero-latency SQLite storage in every Durable Object": https://blog.cloudflare.com/sqlite-in-durable-objects/
- "Durable Objects: Easy, Fast, Correct — Choose three": https://blog.cloudflare.com/durable-objects-easy-fast-correct-choose-three/

## What Cloudflare says

- **Per-object limit**: "a soft limit of 1,000 requests per second" per object; simple `get()`s may go higher;
  an overloaded object queues, then returns an overloaded error. No 3,500 figure found in the docs. CPU limit
  30 s per request (configurable to 5 min for SQLite-backed). 10 GB per SQLite-backed object.
- **SQLite in the same thread** as the application: "not just in the same process, but in the very same
  thread". Query API is synchronous (no await); "A query can complete in microseconds"; data is usually in the
  page cache, local SSD next.
- **Input gates**: while a storage operation runs, no other events are delivered to the object, so plain
  sequential code is race-free without locks (our mailbox gives the same: one request at a time).
- **Output gates**: writes do not block the code; outgoing responses are held until the writes they depend on
  are durable; on write failure the response becomes an error and the object restarts.
- **Write coalescing**: writes without an intervening await are grouped and stored atomically, and "Writes will
  be coalesced (even if you await them), so that the output gate waits only for O(1) network round trips".
- **Durability without local fsync on the hot path** (Storage Relay Service): SQLite always in WAL mode; the
  VFS captures WAL changes; each commit's changes go to 5 followers in other data centers, confirmed when 3
  acknowledge. Followers batch to object storage every ≤10 s or ≤16 MB; a snapshot is uploaded when logs since
  the last one exceed the database size (restore needs ≤ ~2× DB size); logs kept 30 days = point-in-time
  recovery. Failover: a new instance may start once 3 of 5 followers stop confirming the old one.

## What the per-object limit tells us

Their 1,000 req/s is not a SQLite limit. Our measurement of one actor (one goroutine, one connection,
`modernc.org/sqlite`, M5 Max, `go test ./internal/actor -bench . -benchtime 2s`):

| Benchmark (one database)                              | ns/op  | ≈ ops/s  |
| ----------------------------------------------------- | ------ | -------- |
| Point read, SQLite called directly (no actor hop)     | 4,825  | 207k     |
| Point read through the actor, one caller              | 6,638  | 151k     |
| Point read through the actor, 72 callers              | 5,958  | 168k     |
| 80% reads / 20% updates, 72 callers                   | 6,156  | 162k     |
| Writes, 288 callers, group commit, synchronous=NORMAL | 3,137  | 319k     |
| Writes, 288 callers, one txn each, NORMAL             | 17,188 | 58k      |
| Writes, 288 callers, group commit, synchronous=FULL   | 3,428  | 292k     |
| Writes, 288 callers, one txn each, FULL               | 60,550 | 16.5k    |

So a database actor's ceiling for cheap statements is ~150–300k ops/s, 150–300× Cloudflare's soft limit; their
limit comes from per-request JS isolate work and network/RPC, not SQLite. The actor hop costs ~1.8 µs/op.
Note: on macOS, SQLite's FULL calls fsync() but not F_FULLFSYNC (`PRAGMA fullfsync` is off), so it is not a
true flush to stable storage; Linux numbers for FULL will be slower.

## What we can reuse

1. **Same-thread, synchronous SQLite** — already the design (actor goroutine owns the connection). Remaining
   overhead is database/sql + channel hop; a driver API without database/sql (e.g. zombiezen.com/go/sqlite on
   modernc, or cgo mattn) could cut µs per op. Not measured yet.
2. **Write coalescing + output gate** — our group commit already holds replies until COMMIT. Measured gain:
   5.5× (NORMAL) to 18× (FULL) on one database. With coalescing, fsync per commit costs only ~10%.
3. **Durability by shipping the WAL, not by fsync** — the big idea to adopt: run SQLite with cheap local
   commits, ship WAL frames per group commit to replicas and/or object storage, and gate replies on that ack
   (output gate). Go tools exist: Litestream (WAL → S3, Go), LiteFS (FUSE replication, Go). Pairs with the
   coldload agent's SeaweedFS work (they own `internal/objstore`).
4. **Many small objects, not big ones** — Cloudflare scales by object count; a hot object is a design smell
   (shard it). Same for us: per-database ceilings are high, so the risk is slow statements and CPU, not
   SQLite throughput.
5. **They do not solve slow queries inside an object** — single thread, CPU time limit per request, overload
   error when the queue is too deep. Our statement timeout, queue-deadline drop and mailbox shedding are the
   same tools; our optional read pool goes further than they do.
