# From client to the Go process that holds the database: control-plane plan

Status: plan, revised after the hardening pass · Date: 2026-11-05 · Baseline: `books/adoption` at `9178d4c`
(first version at `main` `61ddfbe`/`bbf184b`). Design source: mansio option 4
(`/Users/nick/Software/mansio/docs/architecture/scale-out-direct-actors.md`) and the greenfield proposal
(`/Users/nick/Software/mansio/docs/architecture/scale-out-go-greenfield.md` §8). Rules:
[coding-style.md](coding-style.md), [object-storage.md](object-storage.md) and [postgres.md](postgres.md). The
rule citations below (`RI …` for integration-point rules, now
[coding-style.md §5](coding-style.md#5-integration-points-timeouts-retries-backpressure); `OS …` for
object-storage rules, now [object-storage.md](object-storage.md); `SQL G-n` for SQL rules, now
[coding-style.md §7](coding-style.md#7-sql) and [postgres.md](postgres.md)) and the item ids (`RI-NN`,
`PG-NN`, `OS-NN`) are historical labels from the hardening pass that preceded this plan; the docs above state
the rules without them. Agent reports: `docs/notes/{writeback,fencing,front}/`; contract changes: last entry
of `shared_state.md`.

## 1. Where we stand

On `books/adoption`, `go vet ./...` and `go test -race ./...` pass for all 38 packages (checked 2026-11-05).
The pieces are hardened per the three books but still **not wired together**: agentd serves local files.

| Piece                          | Package                                    | State                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| ------------------------------ | ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Actor per database             | `internal/actor`                           | Done. Savepoint per write in group commit (PG-01); `Do` bounded by the ctx deadline (RI-08); FKs + `STRICT` via `internal/sqlitedsn` (PG-05).                                                                                                                                                                                                                                                                                                                                                                                                           |
| Cold ⇄ warm, durable           | `vactor`, `durable`                        | Done. **Layout v2**: `durable.KeyPrefix` = `dbs/<h2>/<escaped key>/`, epoch-qualified self-describing generations (`docs/notes/writeback/layout.md`). Ambiguous writes re-read (OS-02, RI-14). Fetch bulkhead + activation deadline → `vactor.ErrBusy` (RI-02). S3 breaker `durable.WithBreaker` over `internal/breaker`, per-key tombstones (RI-03). Per-job maintenance deadlines, `oldest_dirty_age` (RI-05). `Host.Shutdown(ctx)` (RI-06). Generation cache `Options.GenCacheBytes`, off by default (OS-10). GC with retention current + 2 (OS-06). |
| S3 client                      | `internal/objstore`                        | Done. One retry layer (idempotent ops only), per-op deadline + stall timeout, Content-MD5, `Classify`, `Store.Metrics()`, paged listing (RI-01, OS-01/07/08). `TestS3Compat` X1–X11 pass on SeaweedFS 4.47 single-node (OS-03).                                                                                                                                                                                                                                                                                                                         |
| Storage ops                    | `cmd/dbtool`                               | Done (OS-12). `inspect`, `verify`, `orphans`, `gc` (orphan sweeper), `rebuild-manifest`; runbook `docs/notes/writeback/storage-runbook.md`.                                                                                                                                                                                                                                                                                                                                                                                                             |
| Single owner                   | `internal/owner`                           | Done. `Admit`, `Fence`, `Reconcile`, `Mover`; `ReconcileAll(ctx, ReconcileOptions)` (16 workers, 5 s per key), `ReconcileUntilDone`, `Config.OnChange`. **`Placement` exists only as the fake** (`ownertest`).                                                                                                                                                                                                                                                                                                                                          |
| Network front                  | `front`, `ticket`, `cmd/agentd`            | Done. 11 wire codes incl. **`unavailable`** (503 + computed `Retry-After`; `cmd/agentd/unavailable.go`). HTTP body/write deadlines, VM-wide admission cap (RI-09). `/livez`, `/readyz` (canary), `/metrics` on the lifecycle listener (RI-07, RI-11). Phased shutdown under one budget (`cmd/agentd/lifecycle.go`). **agentd still runs `front.StaticHost` + `startupAdmitter{epochs(...)}`**: no vactor, no owner, no S3 config, no Claim/Fence endpoints. CP-1 wiring points are marked in `cmd/agentd/main.go` (`newAgent`, `serve`).                |
| Test tools                     | `faultnet`, `objstore/s3test`              | Done (RI-13). A TCP fault proxy and a misbehaving fake S3 that together cover the `RI H4` fault table. Golden tests for `queries/*.sql` (PG-06).                                                                                                                                                                                                                                                                                                                                                                                                        |
| Client retry                   | `internal/retry`                           | Done (RI-10: full jitter, windowed budget, `Attempt.ServerHint`); wired in CP-6: `internal/client` passes Retry-After, `outcome.Unavailable` retried by default, `internal/bench` passes `retry.HintOf`.                                                                                                                                                                                                                                                                                                                                                |
| Placement authority (Postgres) | `internal/placement/pg`                    | Done (CP-3): schema `0001`, one query per method, RI §P pool + breaker, contract suite, faults and PG-04 gate pass on Postgres 17 (`deploy/postgres`, `MANSIO_PG_URL`). Not wired yet (CP-4).                                                                                                                                                                                                                                                                                                                                                           |
| Control plane                  | `cmd/controld`, `internal/control`         | Done (CP-4): registries (0002), M8 policy, create/move/suspend/drop/M5 restore, resumer from table scans; agentd on `placement_pg`; RI-16 admin + `agentctl`; OS-11 scoped identities, TLS.                                                                                                                                                                                                                                                                                                                                                             |
| Locator, issuer                | `internal/control/locator`, `cmd/controld` | Done (CP-5): `POST /v1/resolve` (API keys), notified cache (0003), key ring + `GET /v1/keys`, agentd `keys_url`.                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| Client                         | `internal/client`                          | Done (CP-6): locator-routed, cached routes, re-resolve on wrong_owner/fenced/ticket_invalid/no answer, Retry-After + windowed budget, `X-Timeout-Ms`, one request id per write; `frontbench locator`.                                                                                                                                                                                                                                                                                                                                                   |

## 2. The three flows we need

All three go through the same client loop:

```
client ── resolve(db) ──► locator ──► { endpoint, epoch, gen, ticket, ttl }   (cached; refreshed at ~80% TTL)
client ── POST /v1/query (Bearer ticket) ──► agentd(vm) ──► front ──► owner.Admit ──► vactor.GetAt ──► actor
          ◄── ok | wrong_owner/fenced (421), ticket_invalid (401) → re-resolve
             | shed / unavailable (503) → wait ≥ Retry-After, same endpoint
```

| Flow                                       | Control plane does                                                                                 | Agent does                                                                                                                                     |
| ------------------------------------------ | -------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| **A. Route to the process that has it**    | Locator reads the placement row (replica OK) and issues a ticket at `{vm, epoch, gen}`.            | Verifies the ticket, `Admit(db, epoch)` (ACTIVE at that epoch), `GetAt` → warm actor, or re-warm (1–4 ms from the generation cache).           |
| **B. Brand-new database**                  | Policy picks a VM. `CAS(absent → ACTIVE(vm, epoch 1, gen 0))`, then the agent's lifecycle `Claim`. | `Claim` reads placement on the primary, records ACTIVE durably, `Stamp(epoch 1, gen 0)` creates the manifest, creates an empty database.       |
| **C. Existing cold database on a process** | A move: `owner.Mover.Move`, or a fresh `Claim` at epoch + 1 if the old VM is confirmed dead (M5).  | `Claim` at the new epoch, `Stamp`; the first `GetAt` fetches and verifies the generation (`OS V1`) and warms it (≈ 1.2 ms + size ÷ ~250 MB/s). |

Warm → cold on the same VM needs no control plane: the eviction planner passivates (upload, then delete), and
the next request re-warms. The placement row doesn't change.

## 3. Build order

Each step ends with a gate. Every new adapter passes its contract suite three ways (`RI H4`): against the
fake, the real service, and the real service through `faultnet`/`s3test` with the H4 fault table. Env gates:
`COLDLOAD_S3` (SeaweedFS in Docker), `MANSIO_PG_URL` (`docker compose` Postgres).

### CP-1. Wire agentd on one node — **Done** (747abf3; deferred: per-actor queue metrics in durable mode, `ownerAdmitter` per-request `p.Done` allocation, a shared atomic-file helper for owner + placement/file, vactor `CheckActivation` taking a ctx, discard-on-release driven by owner transitions; notes in `docs/notes/front/durable-mode.md`)

The glue every agent report specified. No new service: a file-backed `Placement` stands in for Postgres.

- **Host:** replace `front.StaticHost` with a durable-mode `vactor.Host`. Executor =
  `host.GetAt(ctx, c.DB,
  c.Epoch)` → `front.ActorExecutor` on `res.DB.(*actordb.DB).Actor()`; run
  `fronttest.RunExecutorSuite` on it. Fetch only when a claim actually activates the key.
- **Admitter:** an `owner.Table` adapter replaces `startupAdmitter{epochs(...)}`:
  `t.Admit(db, owner.Epoch(e))` returns `p.Done`. `ErrWrongOwner` → `front.ErrWrongOwner`;
  `ErrFenced`/`ErrMoving`/`ErrSuperseded` → `front.ErrFenced`. Keep `depAdmitter`/`depExecutor` outermost so
  `unavailableCauses` answer `unavailable` (`RI F2`).
- **Owner ↔ write-back glue:** `Publish` → `host.WriteBackAt`, `Discard` after a successful `Fence`; `Head` →
  `store.ReadManifest`; `Stamp` → `host.Stamp`; `durable.ErrStale` (now always another writer, `OS R4`) →
  `owner.ErrStale`; `Options.CheckActivation` → `Table.CheckActivation`; `Options.Guard` → `Table.Publish`,
  with `Discard` on `ErrSuperseded`.
- **Wiring checklist** (the marked points in `cmd/agentd/main.go`):
  - [x] S3: one long-lived `objstore` client (`OS CA2`);
        `b, _ := breaker.New(breaker.Config{OnChange:
        breakerLog(log, "s3")})` (defaults = `RI §P`: 50
        % of ≥ 20 calls in 10 s, cool-down 5 s);
        `durable.WithBreaker(store, b, func(err error) bool { return objstore.Classify(err) == objstore.Transient }, nil)`
        (`RI C1–C4`).
  - [x] Metrics: `s3Metrics(reg, store.Metrics)`; `breakerMetrics(reg, {"s3": b})` (`"placement"` joins at
        CP-3); `vactorMetrics(reg, vhost, ticks.lastTick)` with `ticks.report` as the `RunMaintenance`
        reporter; `ownerEvents.onChange` as `owner.Config.OnChange` and `ownerEvents.register(reg)` (`RI O1`,
        `RI O3`).
  - [x] Startup: `owner.Open` with `NewFileStore(dir)`, then
        `ReconcileUntilDone(ctx, ReconcileOptions{},
        reconcileLog(log))` as the background job
        replacing `st.openAll`. Reconciled keys serve; the rest answer `unavailable` (`RI H1`).
  - [x] Shutdown: the `host.close` phase calls `vhost.Shutdown(ctx)` (oldest-dirty first, the rest kept
        locally, count logged). The owner table needs no phase (`RI H3`).
  - [x] Readiness stays startup + canary; the S3 breaker and S3 canary HEAD are metrics only (§4, `RI H2`).
  - [x] Config: S3 endpoint/bucket, cache dir, owner dir, `gen_cache_bytes` → `Options.GenCacheBytes` (sized
        from the cache-dir budget). S3 keys go in a 0600 secrets file outside the config (`OS S2`; RI-16's
        secrets file, pulled forward because CP-1 is the first config with credentials).
- **Lifecycle endpoints** on the existing lifecycle listener, control-plane identity only, never queued behind
  queries (`RI B4`), each with a deadline (`RI T1`): `Claim`, `Fence` (idempotent), `Forget`, `Health` (active
  databases, bytes, load).
- **Placement:** a file-backed single-node adapter that passes `ownertest`'s `TestPlacementCAS`.
- **Gate:**
  - e2e: claim → write over HTTP → passivate → kill -9 → restart → reconcile → read back. Every acknowledged
    write that was written back is present; the old epoch gets `wrong_owner`; time-to-first-served-key <
    time-to-all-reconciled (RI-07).
  - Faults (`RI H4`): S3 `Refuse`/`Blackhole` → cold activations `unavailable` + `Retry-After`, warm keys
    `ok`, `/readyz` 200, `/metrics` shows the breaker open. `ApplyThenDropReply` on a publish → resolved by
    re-read.
  - SIGTERM with dirty keys meets `shutdown_budget`; `TestBucketBytesFlatThroughS3` still holds.

### CP-2. Request ids (PG-09) — **Done** (d7c921d; the HTTP kill -9 form of the gate waits for CP-1's wiring — actor-level crash/move tests cover the database half)

A `_mansio_requests` table (`STRICT, WITHOUT ROWID`; DDL and cost in
[the request-ids note](notes/front/request-ids.md)) in each tenant database, so it travels with snapshots.
`insert … on conflict (request_id) do nothing returning` inside the write's savepoint makes "seen before" and
"apply" one decision (`SQL G-8`, `SQL G-10`); retention is a batched delete on the actor (`SQL G-17`). It
resolves front's `ambiguous` 504s and makes retries after a move safe (`RI R2`).

- **Gate:** the same id retried after a crash and after a move applies exactly once; a failed user statement
  leaves no id row; `FuzzActorModel` gains a duplicate-id op.

### CP-3. Postgres `Placement` adapter (PG-02, PG-03, PG-04, RI-12) — **Done** (06204e3; not wired into agentd/controld (CP-4); the file adapter (CP-1) should run `ownertest.RunPlacementSuite`)

`internal/placement/pg`, implementing `owner.Placement` (`internal/owner/ports.go`).

- **Schema (PG-02):** `schema/0001_placement.sql` (conventions in
  [postgres.md](postgres.md#schema-conventions)): domains, `not null`, indexed FKs, `collate "C"` keys, the
  epoch-monotonic trigger; numbered, transactional, embedded, recorded in `schema_version` (`SQL G-1`, `G-3`,
  `G-12`, `G-14`); no counter columns (`SQL G-15`).
- **Queries (PG-03):** one file per port method (§5.2: `get`, `create`, `cas` with the flip and `moves` row in
  one statement, `moving`, `vm_load`); `pgx/v5`, read committed, primary reads only (`SQL G-2`, `G-4`, `G-9`).
  0 rows → `ErrConflict`; no row → `ErrNotFound`; trigger violation → a bug, not `ErrConflict`; lock timeout
  and connection errors match neither sentinel.
- **Stability (RI-12, `RI §P` Postgres sheet, `SQL G-6`):** pgxpool `MaxConns` = reconcile workers + lifecycle
  concurrency, acquire under ctx (`RI T6`); `connect_timeout` 3 s, `statement_timeout` 2 s, `lock_timeout` 1
  s, `idle_in_transaction_session_timeout` 5 s (§P wins over PG-03's 5 / 2 / 10 s), `application_name`, TCP
  keepalives; `MaxConnIdleTime` below any NAT/LB idle timeout; never lower `synchronous_commit`. A breaker per
  pool, registered as `"placement"`, not a readiness check (§4).
- **Gate:**
  - `TestPlacementCAS` against the fake, the file adapter and Postgres; `TestMoveCrashPoints` "reply lost"
    with the real adapter.
  - Golden SQL: `schema/test/constraints.sql` + expected output, each constraint rejecting its off-point
    (`SQL G-20`, `G-21`).
  - Faults through `faultnet` (`RI H4`): `Blackhole`/`HalfOpen` → Get/CAS fail within `statement_timeout + ε`
    with a non-sentinel error; the stale pooled connection is recycled without a hang; a row held in an open
    transaction → non-sentinel error within `lock_timeout`.
  - **Concurrency (PG-04, `SQL G-22`):** 64 goroutines CAS one key → exactly one wins, epoch +1; a many-key
    move race leaves the invariants query empty; p50/p99 CAS latency at 1/16/64 workers in
    `docs/notes/fencing/`.

### CP-4. `cmd/controld`: registries and lifecycle (RI-16, OS-11) — **Done** (cf4be26; deferred: restore runs keys one at a time, a shared cmd helper package (reporter, secrets, JSON endpoints) for agentd/controld, the resumer does not re-fence suspended databases, no locator yet so the gate resolves through Postgres; notes in `docs/notes/front/controld.md`)

- Tables (a new migration, CP-3 rules): `tenants`, `databases` (tenant owns 0..N, default database), `vms`
  (registry and heartbeats; load computed by `vm_load.sql`).
- The placement policy is a pure function: least-loaded VM by active databases and bytes, with a cap (mansio's
  open M8 decision).
- The lifecycle controller: create (flow B), move (`owner.Mover`, flow C), drop (placement tombstone first,
  then the prefix, `OS GC`), suspend. It talks to agents only through the lifecycle endpoints, with a deadline
  and a breaker per agent (`RI T1`, `RI C1`). Stuck moves resume from `moving.sql`, a table scan, never NOTIFY
  (`SQL G-11`).
- **Runtime controls (RI-16):**
  `POST /admin/{breaker/s3:trip|reset, fetch:limit=N, drain, passivate?key=,
  loglevel}` bound to
  `admin_listen` only; `cmd/agentctl` as a thin scriptable client (`RI C4`).
- **Before agents run on more than one VM (OS-11):** scoped S3 identities (`agent` Read/Write, `gc`
  Read/List/Write, `bench` dev only); `objstore.Config.Secure` + CA file; agentd refuses `Secure=false`
  without `-dev` (`OS S1`, `OS S2`). The Postgres DSN joins the secrets file.
- **Gate:**
  - Two agentd processes on one host: create 100 databases across both; move one under write load with no lost
    or duplicate write (CP-2 ids); kill one agent and restore its databases elsewhere from their last
    generation after an operator attests the VM is dead (M5).
  - RI-16: lower `fetch:limit` at runtime and the next fetches respect it; trip the S3 breaker and requests
    get `unavailable` with no S3 calls.
  - OS-11: `TestS3Compat` shows the `agent` identity cannot LIST; a config test covers the TLS refusal.

### CP-4b. Soak (RI-15) — **Harness built, not run** (scripts/soak/run.sh, configs/soak/soak.env, scripts/soak/analyze.sql; deferred by the user 2026-11-05. A 60 min run was started and stopped before results; the 72 h run is still to do.)

72 h of frontbench through agentd + controld in S3 durable mode, with nightly idle hours, and the `RI H4`
fault modes during a load run (`RI H5`). Track fds, RSS, goroutines, cache-dir bytes, bucket object count and
`oldest_dirty_age` (`RI S3`); find the one constraint per workload; write the per-feature SLA table.

- **Gate:** every steady-state gauge has slope ≈ 0 after the first hour; a results note in
  `docs/notes/writeback/`.

### CP-5. Locator and ticket issuer (PG-10) — **Done** (9f98cc4; deferred: per-database grants and revocation floors, TLS on the resolve/keys endpoints (CP-7), caching minted tickets, a shared atomic-file helper (now 4 copies), the locator as its own binary; notes in `docs/notes/front/locator.md`). Follow-ups closed on `cp/fin-loc` ([note](notes/front/locator-controld-edge-fixes.md)): TLS for `/v1/resolve`/`/v1/keys`, unknown-key rate limiting, `fsx.WriteAtomic`.

Inside controld first; split out later.

- `resolve(tenant, db, principal)` → endpoint, epoch, gen, ticket, TTL. Reads replicas, takes a primary read
  when `min_epoch` is set, single-flights per database, returns `MOVING` with `Retry-After`; bounded cache
  (`RI B6`).
- **Invalidation (PG-10):** `0003_notify.sql` (0002 was CP-4's registries) trigger
  `pg_notify('placement', database_id || ':' || epoch)`; the locator LISTENs first, then loads, then applies.
  A lost notification only costs a `wrong_owner` and a re-resolve (`SQL G-11`).
- The issuer signs with a rotating Ed25519 key ring; agents fetch public keys (`ticket.KeySet` already
  overlaps kids). Principals start as API keys, checked once per ticket.
- **Gate:** with the locator stopped, traffic continues until tickets expire; after a move, clients reach the
  new VM within one re-resolve; a cached locator sees a move before TTL; killing the LISTEN connection
  mid-move → reconnect, reload, correct resolve.

### CP-6. Client (RI-10 client part) — **Done** (95bdc88; Go client `internal/client` only, the TS SDK is still to do; deferred: the locator should answer `moving` until the new owner's claim lands (the client tolerates the window with ≤ 5 re-resolves); notes in `docs/notes/front/go-client.md`). Follow-ups closed on `cp/fin-loc` ([note](notes/front/locator-controld-edge-fixes.md)): the locator answers `moving` until the claim lands (0005 NOTIFY on registry flips).

A Go test client first, then mansio's TS SDK (`RI §P` client sheet).

- Location cache refreshed ahead of TTL with jitter; re-resolve on `wrong_owner`, `fenced`, `ticket_invalid`
  or a connect failure, with bounded backoff. Deadline sent as `X-Timeout-Ms` (`RI T4`); the same request id
  on retried writes.
- **Finish RI-10:** add `unavailable` to `internal/outcome` (parsed from the wire, retried by default); pass
  the response's `Retry-After` as `retry.Attempt.ServerHint`, so the wait is `max(hint, jittered delay)`
  (`RI R3`); spend the windowed budget (`RI R4`).
- **Gate:** a `frontbench` mode through the locator measures resolve hit/miss latency and the error rate
  during a move; with the S3 breaker tripped, the first retry waits ≥ `Retry-After`;
  `configs/retry-storm.json` keeps retries/first ≤ ratio over every 1 s window.

### CP-7. Edge and TLS — **Done** (fc434e8; deferred: resolver from controld's VM registry (static `agents` now, `edge.Resolver` seam), TLS + unknown-API-key rate limiting for controld's `/v1/resolve` and `/v1/keys` (building blocks `tlsx`, `edge.SourceLimiter`), mTLS edge → agent, edge admin controls, numbers on separate hosts; notes in `docs/notes/front/edge.md`). Follow-ups closed on `cp/fin-loc` ([note](notes/front/locator-controld-edge-fixes.md)): registry resolver (`edge.RegistryRoutes`), controld TLS + rate limiting, `logx.Reporter`/`logx.Open` for controld and edge (agentd still has its copy); mTLS, edge admin controls, separate-host numbers remain.

Option 4 §3.6: a stateless proxy that routes on the ticket's `vm` claim, so agents stay private. Rate-limit
garbage tickets there (each costs a 26 µs verify). TLS client → edge → agent.

### CP-8. Operations debt — **Done** (3339c71; deferred: leases decision (options note, needs the user); Linux fsync only in a Docker Desktop VM, re-measure on target hardware; SQLite page-cache mutex follow-up; snapshot does not compact free pages)

- Snapshot through the SQLite backup API from a read connection, removing the `VACUUM INTO` write stall (~170
  ms per 128 MiB).
- Measure on Linux with `F_FULLFSYNC`-equivalent durability.
- Decide on leases, to bound a partitioned zombie's stale reads in time. Today only its next publish stops it.
- Shard admission by tenant if the single mutex shows up in profiles.
- Fix the flaky `internal/workload` 30 ms statement timeout under load.
- OS-08 leftovers: `Host.Stats` and wbbench request/cost columns.
- `TestS3Compat` on MinIO and multi-node SeaweedFS before either is used (`OS X`).

After CP-6, a client can reach the right Go process for any database: warm, new or cold. That's the "client →
go process with the db" milestone. For mansio itself, three things are still needed: the handler runtime
spike, the compiler port, and document operations replacing raw SQL on the wire (the reserved `op` field).

## 4. Decisions

| Decision                                                                                                                                                                    | State                                                                              | Where                                                                                                                        |
| --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| Dependency failures get their own wire code `unavailable` (503 + computed `Retry-After`), never `shed` or `internal`                                                        | Made                                                                               | `docs/notes/front/wire-contract.md`, `RI F2`                                                                                 |
| Remote breakers (S3, placement) never fail readiness. S3 open → cold activations 503, warm traffic unaffected, write-backs deferred, alert on `oldest_dirty_age`            | Made (user, 2026-11-05)                                                            | `RI C5`, `RI H2`                                                                                                             |
| Layout v2; retention current + 2; orphan sweeper as `dbtool gc`                                                                                                             | Made                                                                               | `docs/notes/writeback/layout.md`, `docs/notes/writeback/gc-and-generation-cache.md`                                          |
| WAL shipping with an output gate puts an S3 PUT on the write-ack path (SLA inversion). Degraded mode under an open breaker: refuse writes, or ack locally with a larger RPO | **Open**: needs the user before any WAL code                                       | [object-storage.md, open questions](object-storage.md#open-questions), `docs/notes/writeback/wal-shipping-design.md` (OS-13) |
| Placement policy (M8): least-loaded live VM by dominant share of the database and byte caps, then databases, bytes, id; deterministic                                       | Made (CP-4)                                                                        | `internal/control/policy.go`, `docs/notes/front/controld.md`                                                                 |
| Leases                                                                                                                                                                      | Open (options + recommendation, needs the user with the placement-outage question) | `docs/notes/fencing/leases-options.md`                                                                                       |

## 5. Assumptions the plan rests on (from the fencing safety argument)

- Postgres CAS is atomic, epochs only move forward, and Claim and Reconcile read the primary.
- S3 conditional PUT is atomic. **Verified 2026-11-05 on single-node SeaweedFS**
  (`docs/notes/writeback/s3-compatibility.md`; `TestS3Compat` X1–X11:
  `docs/notes/writeback/s3-compatibility.md`). **Also on MinIO and on a 2-filer SeaweedFS cluster with a
  shared Postgres store, raced across gateways** (CP-8, `docs/notes/writeback/s3-compatibility.md`); filers
  with separate stores are not safe.
- `fsync` is durable. On macOS it isn't, without `F_FULLFSYNC`. On Linux (Docker Desktop VM) a durable commit
  costs ~93 µs, 4.3 µs per write with group commit (`docs/notes/sqlbench/linux-fsync.md`).
- An operator restores around a VM only when it's really dead. There are no leases and no clocks in the safety
  path (`OS W6`).
