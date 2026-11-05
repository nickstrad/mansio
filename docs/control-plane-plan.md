# From client to the Go process that holds the database: control-plane plan

Status: plan · Date: 2026-11-05 · Baseline: `main` at `61ddfbe` (writeback `3a5dd97`, fencing `2103e0c`,
front `61ddfbe` landed). Design source: ts-db option 4
(`/Users/nick/Software/ts-db/docs/architecture/scale-out-direct-actors.md`) and the greenfield proposal
(`/Users/nick/Software/ts-db/docs/architecture/scale-out-go-greenfield.md` §8). Agent reports and notes:
`knowledge/{writeback,fencing,front}/`, `docs/state_files/STATE-{writeback,fencing,front}.md`.

## 1. Where we stand

On `main`, `go vet` and `go test -race ./...` pass for all 25 packages (checked 2026-11-05). The building
blocks exist, are tested, and are **not wired together**:

| Piece                       | Package                     | State                                                                                                                                                                                          |
| --------------------------- | --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Actor per database          | `internal/actor`            | Done. Plus `Commits()`, `Snapshot()`, `MaxRows`.                                                                                                                                               |
| Cold ⇄ warm, durable        | `internal/vactor`, `durable`, `objstore` | Done. Generations plus a conditional manifest. Passivate uploads first. Write-back runs by interval or commit count. Eviction by idle time and an LRU budget. `GetAt(ctx, key, epoch)`. |
| Single owner                | `internal/owner`            | Done. Durable local state, `Admit` (9 ns), `Fence`, `Reconcile`, storage stamp, resumable `Mover`. **`Placement` exists only as a fake.**                                                    |
| Network front               | `internal/front`, `ticket`, `cmd/agentd` | Done. Ed25519 tickets, admission, wire codes, `/v1/query`, `/healthz`. **agentd still uses `StaticAdmitter` and `StaticHost`**: no vactor and no owner. **No lifecycle (admin) listener.** |
| Placement authority (Postgres) | none                     | Specified in `knowledge/fencing/2026-11-05-postgres-placement.md`; not built.                                                                                                                  |
| Control plane, locator, issuer | none                     | Not started.                                                                                                                                                                                   |
| Client with location cache  | none                        | Not started.                                                                                                                                                                                   |

## 2. The three flows we need

All three go through the same client loop:

```
client ── resolve(db) ──► locator ──► { endpoint, epoch, gen, ticket, ttl }   (cached; refreshed at ~80% TTL)
client ── POST /v1/query (Bearer ticket) ──► agentd(vm) ──► front ──► owner.Admit ──► vactor.GetAt ──► actor
          ◄── ok | wrong_owner/fenced (421) → re-resolve | shed (503, Retry-After) | ticket_invalid (401) → re-resolve
```

| Flow                                         | Control plane does                                                                                                                                                                   | Agent does                                                                                                                                                     |
| -------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **A. Route to the process that has it**      | Locator reads the placement row (replica OK) and issues a ticket at `{vm, epoch, gen}`.                                                                                             | Verifies the ticket, `Admit(db, epoch)` (ACTIVE at that epoch), `GetAt` → warm actor (or re-warm if it was passivated).                                       |
| **B. Brand-new database**                    | Policy picks a VM. `CAS(absent → ACTIVE(vm, epoch 1, gen 0))`, then the agent's lifecycle `Claim`.                                                                                  | `Claim` reads placement on the primary, records ACTIVE durably, `Stamp(epoch 1, gen 0)` creates the manifest, creates an empty database.                    |
| **C. Existing cold database on a process**   | Placement row exists, the database is cold everywhere. Placing it on a new VM is a move: `owner.Mover.Move` (or a fresh `Claim` at epoch + 1 if the old VM is confirmed dead, M5). | `Claim` at the new epoch, `Stamp`, then the first request's `GetAt` fetches the manifest's generation and warms it (coldload: ≈ 1.2 ms + size ÷ ~250 MB/s). |

Warm → cold on the same VM needs no control plane: the eviction planner passivates (upload, then delete), and
the next request re-warms. The placement row doesn't change.

## 3. Build order

Each step ends with a test gate. Steps 1–2 need no new service. The S3 gate (`COLDLOAD_S3`) runs now that Docker is up; the
Postgres gate needs a `docker compose` Postgres.

1. **Wire agentd on one node.** This is the glue every agent report specified.
   - **Host:** replace `openHost` with a `vactor` host in durable mode (`Options.Store` = objstore or fake). The
     `front.Executor` adapter is `host.GetAt(ctx, c.DB, c.Epoch)` → `front.ActorExecutor` on
     `res.DB.(*actordb.DB).Actor()`. Run `fronttest.RunExecutorSuite` against it.
   - **Admitter:** replace `epochs()` with an `owner.Table` adapter. `t.Admit(db, owner.Epoch(e))` returns
     `p.Done`. Map `ErrWrongOwner` → `front.ErrWrongOwner`, `ErrFenced`/`ErrMoving`/`ErrSuperseded` →
     `front.ErrFenced`, `ErrUnavailable` → `front.ErrShed`.
   - **Owner ↔ writeback glue:**
     - `Publish` → `host.WriteBackAt`, and `Discard` after a successful `Fence`.
     - `Head` → `store.ReadManifest`; `Stamp` → `host.Stamp`.
     - `durable.ErrStale` → `owner.ErrStale`.
     - `Options.CheckActivation` → `Table.CheckActivation`; `Options.Guard` → `Table.Publish`, and `Discard` on
       `ErrSuperseded`.
     - Fetch only when a claim actually activates the key (fencing's fuzz finding).
   - **Startup:** `owner.Open(...)` with `NewFileStore(dir)`, then retry `ReconcileAll` until it succeeds. Don't
     serve before that.
   - **Lifecycle listener** (new; a separate port, control-plane identity only): `Claim`, `Fence`
     (idempotent), `Forget`, `Health` (active databases, bytes, load).
   - **Placement:** a file-backed single-node `Placement` adapter, so one agent runs end to end without
     Postgres.
   - **Gate:** an e2e test of claim → write over HTTP → passivate → kill -9 → restart → reconcile → read back.
     Every acknowledged write that had been written back is present. A query at the old epoch gets
     `wrong_owner`.
2. **Request-id store.** A table inside each database, written in the same transaction as the write, so it
   travels with snapshots. It resolves front's `ambiguous` 504s and makes client retries after a move safe.
   **Gate:** retry the same request id after a crash and after a move; it's applied exactly once.
3. **Postgres `Placement` adapter** per the fencing note: primary reads, single-statement CAS, an
   epoch-monotonic trigger, and the flip plus `moves` row in one transaction. **Gate:** the
   `TestPlacementCAS` contract suite against the fake and Postgres (env-gated, `docker compose` Postgres).
4. **`cmd/controld`: registries and lifecycle.**
   - Tables: `tenants`, `databases` (tenant owns 0..N, default database), `vms` (agent registry and heartbeats).
   - The placement policy is a pure function: least-loaded VM, by active databases and bytes, with a cap. This is
     ts-db's open M8 decision.
   - The lifecycle controller: create (flow B), move (`owner.Mover`, flow C), drop (tombstone), and suspend.
     It talks to agents only through the lifecycle listener.
   - **Gate:** two agentd processes on one host. Create 100 databases, spread across both. Move one under write
     load with no lost or duplicate write. Kill a VM's agent and restore its databases elsewhere from their last
     generation, after an operator attests the VM is dead (M5).
5. **Locator and ticket issuer** (inside controld first; split out later).
   - `resolve(tenant, db, principal)` → endpoint, epoch, gen, ticket, TTL. It reads replicas, takes a primary
     read when `min_epoch` is set, single-flights per database, and returns `MOVING` with `Retry-After`.
   - The issuer signs with a rotating Ed25519 key ring. Agents fetch the public keys (the `ticket.KeySet` port
     already overlaps old and new kids). Principals start as API keys, checked once per ticket and not per
     request; sessions come later.
   - **Gate:** with the locator stopped, running traffic continues until tickets expire. After a move, clients
     reach the new VM within one re-resolve.
6. **Client.** A Go test client first, then ts-db's TS SDK. It keeps a location cache, refreshes ahead of TTL
   with jitter, and re-resolves on `wrong_owner`, `fenced`, `ticket_invalid` or a connect failure, with bounded
   backoff. Request ids on writes. **Gate:** a `frontbench` mode that goes through the locator. Measure
   resolve-hit and resolve-miss latency, and the error rate during a move.
7. **Edge and TLS** (option 4 §3.6): a stateless proxy that routes on the ticket's `vm` claim, so agents stay
   private. Rate-limit garbage tickets there; each costs a 26 µs verify.
8. **Operations debt surfaced by the three agents:**
   - Garbage-collect old generations and orphans; the bucket currently grows without bound.
   - Snapshot through the SQLite backup API from a read connection, to remove the write stall during
     `VACUUM INTO` (~170 ms per 128 MiB).
   - Measure on Linux with `F_FULLFSYNC`-equivalent durability.
   - Decide on leases, to bound a partitioned zombie's stale reads in time. Today only its next publish stops
     it.
   - Shard admission by tenant if the single mutex shows up in profiles.
   - Fix the flaky `internal/workload` 30 ms statement timeout under load.

After step 6, a client can reach the right Go process for any database: warm, new, or cold. That's the "client
→ go process with the db" milestone. For ts-db itself, three things are still needed: the handler runtime
spike (`knowledge/front/2026-11-05-handler-executor.md`), the compiler port, and document operations
replacing raw SQL on the wire (the reserved `op` field).

## 4. Assumptions the plan rests on (from the fencing safety argument)

- Postgres CAS is atomic, epochs only move forward, and Claim and Reconcile read the primary.
- S3 conditional PUT is atomic. **Verified 2026-11-05 on single-node SeaweedFS** (`knowledge/writeback/2026-11-05-s3-conditional-put-verified.md`; `TestS3Compat` X1–X11: `knowledge/writeback/2026-11-05-s3-compat-seaweedfs.md`); a multi-node cluster isn't tested.
- `fsync` is durable. On macOS it isn't, without `F_FULLFSYNC`.
- An operator restores around a VM only when it's really dead. There are no leases and no clocks in the safety
  path.
