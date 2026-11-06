# Option 4 on a greenfield Go backend (mansio-server approach plus a control plane)

Status: proposal · Date: 2026-11-05 · Baseline: mansio `main` at `addda3d`; mansio-server working tree of
2026-11-05. Builds on: [option 4](scale-out-direct-actors.md) (locator control plane, direct database actors),
[scaling issues](scaling_issues.md), planes-fable M1–M8 ([comparison](scale-out-comparison-fable.md) §4), and
mansio-server's measurements (`../mansio-server/knowledge/sqlbench/`, `../mansio-server/knowledge/coldload/`).
It changes no code. Numbers marked _measured_ come from mansio-server's runs (Apple Silicon, 18 cores, macOS,
Go 1.26, `modernc.org/sqlite`). Everything else is design or estimate.

## Answer

**Yes, with one gate.** Build the backend in Go from scratch: mansio-server's actor-per-database data plane,
plus option 4's control plane (locator, ticket issuer, placement, lifecycle). Keep TypeScript only for the
SDK. This puts us in a better spot than building option 4 on the Deno platform, because the part of option 4
that costs most in Deno is free in Go, and mansio-server has already built and measured it.

The gate is **where app handlers run**. Handlers are TypeScript that runs inside the write transaction. A Go
backend has to embed a JS engine, or handlers must change language. Spike that first. If no engine passes, the
answer changes (§6).

## 1. Why Go plus option 4 beats Deno plus option 4

Option 4's agent is lanes (M3) holding database actors: a lane supervisor, lane workers, per-actor mailboxes,
statement deadlines, and kill-and-respawn for a lane whose interrupt fails. In Deno, each lane is a worker or
process with IPC, plus the existing FFI binding and its deadline machinery. In Go, a lane is a goroutine, and
SQLite runs on real OS threads.

| Option 4 component                         | Deno platform                                                                 | Go (mansio-server)                                                                                                                                                                                                                                                                                                                                                                              |
| ------------------------------------------ | ----------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `database-actor` (mailbox, writer, reads)  | To build: today it's a per-file `Semaphore(1)` writer queue on one event loop | **Built**: `internal/actor`. Bounded mailbox, shedding, queue deadline, statement interrupt via context, read pool, passivation. One actor: ~151k point reads/s, ~162k ops/s at 20% writes (_measured_).                                                                                                                                                                                        |
| Group commit (option 4 §4.1)               | To build, with savepoints                                                     | **Built** for SQL batches: 16.5k → 292k writes/s under FULL (_measured_). Needs savepoints per handler (§3).                                                                                                                                                                                                                                                                                    |
| `lane-supervisor`, lane workers (M3)       | To build: workers or processes, IPC, respawn on a stuck interrupt             | Mostly unnecessary: goroutines on the Go scheduler. ~20k ok req/s across 1k databases, CPU-bound at ~14 of 18 cores (_measured_).                                                                                                                                                                                                                                                               |
| `actor-directory` (activate/passivate)     | To build, replacing the 64-handle cache behind one global lock                | **Half built**: `internal/vactor` does cold → warm with single-flight per key (500 callers on one cold database → 1 fetch; open plus first query 0.3–0.5 ms, _measured_). Warm → cold is **not** built: `Passivate` closes the database and deletes the local copy without uploading (`host.go:335-342`), so writes made while warm are lost. There's no idle or budget eviction policy either. |
| Activation from object storage (moves, M5) | To build                                                                      | **Read path built**: `internal/objstore` plus `vactor`. Ready ≈ 1.2 ms + size ÷ ~250 MB/s; bound downloads by bytes in flight (_measured_, SeaweedFS in Docker). Write-back (upload on passivate, WAL shipping, generations) is not built.                                                                                                                                                      |
| Reader child, IPC, in-process DuckDB       | Exists (~1.5k lines) and is serial (hotspot 9)                                | Not needed. Analytics run offline with the DuckDB CLI over JSONL, as mansio-server does.                                                                                                                                                                                                                                                                                                        |
| SQLite FFI binding (~2k lines)             | Exists                                                                        | Not needed: `modernc.org/sqlite` (pure Go) or a driver without `database/sql`.                                                                                                                                                                                                                                                                                                                  |

Hotspots from option 4 §5.1, once the Go agent exists:

- **Rows 4–8 go away by construction**: event loop, handle-cache lock, `prepareLock`, shared read cap, backup
  on the writer queue. That is what the actor replaces.
- **Rows 1–3 move** to the ticket issuer, with a `pgx` pool and per-key single-flight.
- **Rows 9–11 go away**: no reader child, per-actor log fields, offline analytics.
- **Row 12** becomes per-database backups with recorded manifests (M6).

## 2. What carries over, what's dropped, what's new

**Carries over** (these are contracts and knowledge, not Deno code):

- The HTTP wire contract and error tags (`core/errors.ts` taxonomy as JSON), so `sdk/client.ts` doesn't
  change.
- The TS SDK: `sdk/client.ts`, `sdk/schema.ts`, and the handler API (`sdk/db.ts`, `sdk/functions.ts`), which
  runs inside the engine.
- Compiler semantics. Port the pure `core/compile/` (624 lines, SQLite only, see
  [knowledge entry](../knowledge/compiler/2026-11-05-sqlite-only-simplification.md)). Check it against
  `GENERATED_SQL.txt` byte for byte, and differentially against the TS compiler on fast-check plans.
- Document semantics: `_id`, `_creationTime`, `_version`, `expectedVersion` concurrency, the storage layouts,
  decode rules.
- Option 4's contracts unchanged: tickets (§3.1), admission (§3.2), stale-safe locator (§3.3), moves (§3.4),
  request ids (§3.5), edge-first exposure (§3.6). Planes-fable M1–M8.
- **The e2e suite** (`tests/e2e/platform/`, black-box HTTP plus process control) becomes the acceptance gate
  against the Go binary.
- Correctness lessons, as Go tests, after re-checking each premise under the new driver: number boundaries
  (item 30), the U+0000 text contract (ADR 0005), statement deadlines, event identity (ADR 0006).
- From mansio-server: `actor`, `vactor`, `objstore`, `errs`, `logx`, `retry`, its testing approach (contracts,
  fakes with fault switches, model-based fuzzing), and `sqlbench` as the load harness.

**Dropped:** Deno, the Effect platform layers, the FFI binding, the reader child and IPC, in-process DuckDB,
and Better Auth. Its scrypt hashes (N=16384, r=16, p=1, 64-byte key, NFKC password, hex salt string, stored as
`salt:hex`) can be verified with `x/crypto/scrypt`, so accounts migrate without a password reset. That is
_unverified_ against a real hash.

**New**, none of it built anywhere yet:

| Piece                                       | Notes                                                                                                               |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| Handler runtime                             | §3. The gate.                                                                                                       |
| Agent front                                 | Ticket verification (Ed25519), request validation, compile, admission caps per tenant and VM.                       |
| Durable local state                         | `ACTIVE` / `MOVING` / fences, epoch and generation per database. The agent refuses to activate without it.          |
| Request-id store                            | In the same transaction as the mutation; copied with snapshots.                                                     |
| Control plane: registries and placement row | Postgres: tenants, databases (tenant owns 0..N, default database M4), `(database_id, owner_vm, epoch, generation)`. |
| Locator and ticket issuer                   | Stateless replicas, single-flight per database, reads from replicas, `min_epoch` reads from the primary.            |
| Lifecycle controller                        | Create, suspend, drop with tombstone, offline fenced move, VM-loss restore (M5).                                    |
| Durability                                  | WAL shipping to object storage (Litestream-style) plus recorded manifests (M6). Not measured.                       |
| Stateless edge                              | Routes on the ticket's `vm` claim; agents stay private.                                                             |
| SDK location cache                          | Resolve, then ticket, then call. Refresh ahead of expiry; re-resolve on `WrongOwner`/`Moved`.                       |

## 3. The gate: handlers inside the transaction

A mansio mutation is a TypeScript Effect handler that reads, decides, and writes while holding
`BEGIN
IMMEDIATE`. mansio-server's actor batches standalone SQL. To join the two, run the handler **inside the
actor's goroutine**, in an embedded JS engine, with `db.*` bound synchronously to the actor's connection:

```
mailbox → batch of N mutations
  BEGIN IMMEDIATE
    for each: SAVEPOINT h_i → run handler in engine (db.* calls hit the actor's conn directly)
              → RELEASE h_i   or   ROLLBACK TO h_i (that handler's error only)
  COMMIT  → reply to all N (output gate)
```

This keeps today's semantics. It keeps the handler next to its data, with no IPC under the lock, which is the
Durable Objects model. And it gets group commit.

Engines to compare (none measured here): goja (pure Go), QuickJS compiled to WASM on wazero (pure Go, full
ES2023), and V8 through cgo (fastest, has a cgo cost). Pool runtimes **per app and lane, not per database**,
because handler code is per app. Today's per-database budget is ~0.7 MB (_measured_), and an engine per
database would dwarf it.

**Spike to run first:** bundle `app/functions.ts` (with Effect) using esbuild. Run `createGame` and `makeMove`
in each engine inside a mansio-server actor, with a minimal `db` binding. Measure:

- whether the bundle runs at all (generators, `Effect.gen`),
- added latency per handler, p50 and p99,
- memory per runtime,
- savepoint batch throughput under FULL.

Pass bar, to be agreed: the handler overhead is small next to the SQLite work (sub-millisecond `makeMove`),
and group commit still gives a several-fold gain.

## 4. Build order (each step gated by tests, not dates)

1. **Gates**: the handler-engine spike (§3); a mansio baseline under sqlbench's workload shape, so "better"
   has a before number; group commit and fsync numbers on Linux, since macOS FULL skips `F_FULLFSYNC`.
2. **Single-node agent, no control plane**: one Go binary, wire-compatible with mansio's tenancy-off mode.
   Actors, ported compiler and decode, handler engine. Gate: e2e `documents` and `queries` scenarios green
   against it. `GENERATED_SQL.txt` matches byte for byte.
3. **Tenancy and auth on one node**: tenant owns databases, default database. Go auth migrates Better Auth
   accounts. The issuer is embedded and the agent verifies tickets. Gate: e2e `auth` and `tenant_isolation`
   green, with the auth scenario rewritten for the new sign-in endpoints.
4. **Control plane split out**: placement row, locator, issuer, provisioning, agent registration and health,
   stateless edge, SDK location cache. Gate: two agents, tenants placed on both, the locator going down
   doesn't stop running traffic.
5. **Durability and moves**: WAL shipping, manifests (M6), offline fenced move (§3.4), activation from object
   storage, VM-loss restore (M5). Gate: e2e `backup_restore` and `admin_lifecycle`, plus a move under load
   with no lost or duplicated mutation (request ids).
6. **Retire the Deno platform**: analytics run on the DuckDB CLI over JSONL. `log_analysis` e2e green.

Steps 2–3 already fix scaling issues 2–5 on one node, before any control plane exists. This is planes-fable
M2's "in-node first" order, carried over.

## 5. Costs and risks

- **It's a rewrite.** The Deno platform is ~16k lines of TS, with ~34k lines of tests, most of them
  platform-specific. A Go backend reuses mansio-server's packages but still has to build everything in §2
  "New". _Estimate, unverified:_ smaller than the TS platform, because Effect layers, FFI, IPC and the reader
  child have no counterpart.
- **Two systems for a while.** mansio stays the reference until e2e parity. Don't add features to it in the
  meantime.
- **mansio-server hasn't run a mansio-shaped workload**: schema validation, handler transactions, document
  decode. Its numbers are an upper bound for cheap statements on macOS.
- **Trust boundary**: the agent runs the compiler and app handlers on every VM (option 4 §6). The engine's
  sandbox (no ambient I/O) becomes a security property, and needs tests.
- **Control plane and durability are unmeasured by anyone.** They're the largest new work and carry the most
  correctness risk (fencing, moves, WAL shipping).

## 6. What would change the answer

- **No engine passes the spike.** Then the choice is Go handlers (authors write server logic in Go; the TS SDK
  becomes client-only) or staying on Deno, where lane workers run TS handlers natively. Deno plus option 4
  remains viable. It just costs more on the data-plane side.
- **The mansio baseline is already close to the Go numbers** for our real workload. Then the in-node fixes in
  Deno (planes M2/M3) may be enough, and the rewrite buys only the control-plane path.
- **Linux fsync makes group commit gains small.** That would weaken the largest single win. It doesn't change
  the lock and event-loop wins.

## 7. Other approaches

Compared on what decides the choice: where handlers run, who owns the data plane (actors, durability, moves),
and what is left for us to build. The facts about outside products come from memory unless marked. The one
exception is Durable Objects, from mansio-server's sourced note
(`../mansio-server/knowledge/sqlbench/2026-11-04-cloudflare-durable-objects.md`). **Check these before
deciding.**

| # | Approach                                                                                                                                                                                                                                                                                                                    | Handlers                                                                          | Data plane owned by                                            | We build                                                                   | Main catch                                                                                                                                                                        |
| - | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- | -------------------------------------------------------------- | -------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A | **This proposal**: Go agent (mansio-server actors) plus option 4 control plane, embedded JS engine                                                                                                                                                                                                                          | TS in an engine, inside the actor                                                 | Us                                                             | Everything in §2 "New"                                                     | Engine spike; control plane and WAL shipping are all new                                                                                                                          |
| B | **Go data plane, Deno handler sidecar, optimistic commit.** A handler runs in Deno against a read snapshot and returns its write set plus a read set (doc ids with `_version`, plus each query's result ids). The Go actor validates the read set and group-commits in one transaction; on a conflict, the handler retries. | TS in Deno, outside the transaction                                               | Us                                                             | Same as A, minus the engine, plus read-set tracking in `sdk/db.ts`         | Validating query reads (phantoms) means re-running each query at commit; hot databases retry a lot. As I understand it, Convex does optimistic commit with index-range read sets. |
| C | **Stay on Deno, do it incrementally**: planes M2/M3 (in-node fixes, lane workers), then option 4                                                                                                                                                                                                                            | TS natively in lane workers                                                       | Us                                                             | Lanes with IPC and respawn, the actor model in TS, the whole control plane | Rebuilds in TS what mansio-server already has; SQLite stays behind FFI; more moving parts per VM                                                                                  |
| D | **Build on Cloudflare Durable Objects (SQLite-backed).** Each mansio database is one Durable Object; the control plane is a Worker plus D1 or Postgres.                                                                                                                                                                     | TS natively, same thread as SQLite, inside input/output gates (matches our model) | Cloudflare (WAL to a 3-of-5 quorum, placement, failover)       | Compiler and SDK on the DO API, the locator, auth, billing                 | Vendor lock-in, no self-hosting. Sourced limits: ~1k req/s soft per object, 10 GB per object, a CPU limit per request. Moves and placement aren't ours to tune.                   |
| E | **Managed database-per-tenant SQLite** (e.g. Turso/libSQL)                                                                                                                                                                                                                                                                  | TS in our stateless servers, over the network to the DB                           | The vendor (storage and replication); we run stateless compute | A stateless API tier, tenant → database mapping, auth                      | Every statement is a network round trip, so handlers hold write locks across RTTs; that's the same problem as running handlers outside the agent. Vendor lock-in.                 |
| F | **Replicated SQLite under today's platform** (LiteFS-style FUSE replication, or Litestream for backup only). Shard tenants across several mansio processes with a thin router; the primary moves with the lease.                                                                                                            | TS natively, unchanged                                                            | Us plus the replication tool                                   | A router, tenant → node map, lease handling                                | Doesn't fix issues 2–5 inside a node; it only spreads them out. I believe LiteFS is in maintenance mode; check before relying on it.                                              |
| G | **Drop database-per-tenant: shard Postgres** (Citus-style, tenant id as the distribution key)                                                                                                                                                                                                                               | TS in stateless servers                                                           | Postgres                                                       | A Postgres compiler dialect, tenancy keys everywhere                       | A different product: we lose per-tenant files, cheap passivation, per-tenant restore and moves, and the SQLite performance profile. Every past ADR assumes SQLite.                |

### How they rank for the goal (scale-out on our own control plane)

1. **A** if the engine spike passes. It is the only option that keeps handlers inside the transaction, in our
   own data plane, using code that has already been measured.
2. **B** if the spike fails but we still want Go. It keeps TS handlers without an engine, and pays for it with
   optimistic-commit complexity and retries on hot databases. Run the same spike harness with a sidecar to
   compare.
3. **D** is worth a short paper check before investing in A. It is our exact execution model (handler next to
   SQLite, coalesced writes, output gates) with durability and placement already solved. If self-hosting and
   control over placement are not hard requirements, it removes most of §2 "New". If they are hard
   requirements, D is out.
4. **C** is the low-risk incremental path, but it rebuilds the data plane in the language where it costs most.
5. **E, F, G** don't fit: E and F leave the in-node problems or move them onto the network, and G changes the
   product.

**Cheap way to decide between A, B and D:** one harness, three backends, the same `makeMove` workload:

- Go actor plus an embedded engine (A),
- Go actor plus a Deno sidecar with optimistic commit (B),
- a Durable Objects prototype of the same handler (D).

Compare handler p50/p99, throughput on one hot database, and code size.

## 8. "Is the control plane the last main step?" (checked against mansio-server's code)

A reasonable reading of mansio-server: one process hosts N SQLite actors and scales if SQL can reach them, and
coldload moves databases between cold storage and warm actors. Add a control plane that routes to the right
process, places new databases and warms them, and you're done. The code says three pieces sit between that and
a working system, and the control plane depends on two of them:

```
                       exists in mansio-server                     missing
                       ──────────────────                     ───────
client ──► [ network front: listener, tickets, admission, handlers ]          ← missing (1)
             │
             ▼
           actor per DB ✓  (mailbox, group commit, deadlines, shed)
             │
   cold ─► warm ✓  (download, single-flight, open, warm-up)
   warm ─► cold ✗  (Passivate deletes, never uploads)                         ← missing (2)
   crash while warm ✗  (writes since download are lost)                       ← missing (2)
   two processes warm the same DB ✗  (nothing stops it)                       ← missing (3)
             │
           control plane: route, place new, place + warm, move                ← missing (4)
```

1. **Network front.** A listener, the request protocol, ticket verification, per-tenant admission, and the
   handler runtime (§3). "If the SQL can be delivered" hides the hard part: mansio mutations aren't SQL,
   they're handlers that read and then write inside a transaction.
2. **Write-back durability.** Upload on passivate is the minimum. For crash safety, ship WAL frames
   continuously (Litestream-style) and record a generation per upload (M6). Without this, warm → cold is lossy
   and a node crash loses every write since activation. That makes moves unsafe as well.
3. **Single-owner fencing in the agent.** Object storage lets any process download any database. If a stale
   route or a retried placement warms the same database on two processes, both accept writes, and the last
   upload wins. Option 4 puts the guarantee in the agent: durable local `ACTIVE` state at an epoch and
   generation, rejection of older epochs, and a fence before handing off. Routing alone isn't enough, because
   routes are allowed to be stale (option 4 §3.3).
4. **Control plane.** Then the part you described: placement row, locator and tickets, create-new, place and
   warm, move (fence, upload, flip epoch, activate on the target), and VM-loss restore from the last
   generation.

With 2 and 3 in place, the control plane really is mostly routing and bookkeeping. A move becomes "passivate
on A with upload and fence, flip the epoch, activate on B", built on coldload's measured activation path.
Without 2 and 3, the control plane would route correctly to a system that loses or forks data.

Revised order for §4 steps 2–5: front plus handlers → write-back and fencing on one node (crash and passivate
tests) → control plane → WAL shipping and VM-loss restore.
