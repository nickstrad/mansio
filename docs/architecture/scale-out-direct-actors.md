# Scale-out option 4: control plane as a locator, clients talk to database actors directly

Status: proposal · Author: Claude Opus 5.5 (with nickstrad) · Date: 2026-11-04 · Branch: `main` · Code
baseline: `main` at `fac3e3e` (DP-02 landed). Builds on: `mansio-planes-fable` (DP-02) and
[`scale-out-comparison-fable.md`](scale-out-comparison-fable.md).

Options so far: (1) `mansio-current`, what runs today; (2) `mansio-lanes-proposal`; (3)
`mansio-planes-proposal`, revised by Fable as `mansio-planes-fable`. This document is **option 4**:
planes-fable with the gateway taken off the query path. It changes no code and no dataset. If accepted, it
becomes a DP-03 dataset brief.

## Answer

Keep everything planes-fable decided about placement, fencing, lifecycle, backup and recovery. Change one
thing: **who sits between the client and the database.**

- In planes-fable a query goes client → gateway → VM agent. The gateway authenticates, asks the control plane
  for a route grant (cached 30 s), compiles the request and forwards it. The control plane is off the hot
  path, but the gateway tier is on it for every query, and every row comes back through it.
- In option 4 the control plane is a **locator and ticket issuer**. A client asks it once "where is database D
  of tenant T?" and gets back an address and a signed, short-lived **access ticket**. The client caches both
  and sends queries **directly to the VM agent that owns D**. It asks again only when the ticket expires, the
  agent answers `WrongOwner`/`Moved`, or the connection fails.
- Inside the agent, each database is a **virtual actor**: one logical instance per database, activated on
  first use, passivated when idle, with its own mailbox, writer, handle set and request-id store. The actor is
  the unit we scale and iterate on (batching, pinning, read followers, splitting hot databases onto their own
  VM) without touching the control plane or the client contract.

The control plane leaves the query path entirely. A control-plane outage stops new resolutions after the
ticket TTL, not running traffic; and because the agent enforces the epoch, the locator may serve stale answers
safely, so it can be scaled horizontally off read replicas.

## 1. What changes from planes-fable

| Concern                    | Planes-fable (option 3)                                    | Option 4 (this)                                                                |
| -------------------------- | ---------------------------------------------------------- | ------------------------------------------------------------------------------ |
| Query path                 | Client → gateway → agent → gateway → client                | Client → agent → client                                                        |
| Who resolves location      | Gateway, via route grant from control plane (30 s TTL)     | Client SDK, via locator (ticket TTL, re-resolve on failure)                    |
| Who authenticates a query  | Gateway (session / API key against Postgres)               | Agent, offline, by verifying the ticket signature; no Postgres on the hot path |
| Who compiles document → IR | Trusted gateway code                                       | Trusted agent code (actor front), same compiler (`core/compile`)               |
| Stale-route enforcement    | Agent checks grant epoch vs durable local ACTIVE state     | Unchanged: agent checks ticket epoch vs durable local ACTIVE state             |
| Control plane down         | Existing grants serve until TTL, then gateway fails closed | Existing tickets serve until TTL; no new tickets; agents keep serving          |
| Scaling unit on the path   | Gateway tier (stateless) + agent                           | Database actor only                                                            |
| Agents exposed to clients  | No (gateway in front)                                      | Yes: per-VM TLS endpoint, or a stateless edge that routes on the ticket (§3.6) |

Everything else carries over unchanged: tenant owns 0..N databases (DP1); placement row
`(database_id, owner_vm, epoch, generation)`; offline fenced moves with no flip before the source's durable
fence acknowledgement; drop with tombstone; recorded backup manifests (M6); operator-attested VM-loss recovery
(M5); lanes inside the agent with write-first scheduling and snapshots off the writer lock (M3); default
database per tenant (M4); candidate placement rule (M8, still pending a decision).

## 2. Components

**Control plane** (off the query path)

- `locator`:
  `resolve(tenant, database name | id, principal) → { endpoint, database_id, epoch, generation,
  ticket, ttl }`.
  Read-only. Reads placement from a Postgres read replica or an in-memory snapshot fed by change
  notifications; stale reads are safe (§3.3). Coalesces concurrent resolves for the same database
  (single-flight) so a move or VM loss does not turn into N identical Postgres queries.
- `ticket-issuer`: signs access tickets with an Ed25519 key; publishes the public key set (with key ids) to
  agents and rotates it. Checks the principal's grants for the database at issue time. This is where the
  session / API-key lookup now happens, once per ticket instead of once per request.
- Registries, provisioner, lifecycle controller, backup registry, placement policy: unchanged from
  planes-fable. The lifecycle identity and its command contract are unchanged.

**VM agent** (the query path)

- `actor-front`: the agent's client-facing listener. Terminates TLS, verifies the ticket (signature, expiry,
  audience = this VM, key id), validates the document request and compiles it to operation IR plus SQL/binds
  with the landed compiler and classifier. This is the trust boundary that the gateway held in planes: **no
  customer SQL or customer IR is accepted**, only document requests, and the compiler runs in trusted agent
  code. Lifecycle commands still arrive only from the control plane's lifecycle identity on a separate
  listener.
- `actor-directory`: maps `database_id → actor` on this VM, activates on first ticketed request if and only if
  the durable local state is `ACTIVE` at the ticket's epoch and generation, passivates idle actors (closes
  handles, frees descriptors). Single activation per database is guaranteed by placement epoch plus the local
  fence, not by the directory.
- `database-actor` (one per active database, hosted in a lane worker): bounded mailbox (the per-database
  queue), write-first scheduler, writer handle and read pool, request-id store, deadline watchdog, snapshot
  connection. This is planes-fable's lane internals regrouped by database.
- `lane-supervisor`: unchanged from M3; assigns actors to lane workers (tenant-affine, pinnable), kills and
  respawns a lane whose interrupt fails.

**Client SDK**

- `location-cache`: `database → { endpoint, ticket, expires }`. Refreshes ahead of expiry (at ~80 % of TTL,
  jittered), re-resolves on `WrongOwner`, `Moved`, `TicketExpired`, connect failure or TLS failure, with
  bounded exponential backoff and jitter. `createClient(baseURL, appId, token)` keeps working: `baseURL` is
  the locator; `token` is exchanged for tickets. Mutations carry a client request id so a retry after an
  ambiguous failure is resolved by the actor's request-id store, never blindly replayed.

## 3. Contracts

### 3.1 Access ticket

Signed by the control plane; verified by the agent without any network call.

```
ticket = sign_ed25519(kid, {
  tid,  dbid,  vm,           // tenant, immutable database id, audience (owner VM)
  epoch, gen,                // must equal the agent's durable local ACTIVE state
  sub, perms, grant_ver,     // principal, permissions on this database, grant version
  iat, exp                   // exp - iat = ticket TTL (proposed 60 s; open)
})
```

The ticket is a capability for one database on one VM at one epoch. It does not grant lifecycle rights, other
databases, or other VMs. A move bumps the epoch, so every outstanding ticket for that database becomes useless
the moment the target activates, without any revocation traffic.

### 3.2 Admission at the agent (replaces the planes gateway + grant table)

| State                                                           | Admission                                                                                                                                                       |
| --------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Valid ticket, local `ACTIVE`, epoch and gen match               | Admit into the actor's mailbox (subject to mailbox, tenant and VM caps).                                                                                        |
| Ticket expired, bad signature, wrong `vm`                       | `TicketInvalid`; client re-resolves.                                                                                                                            |
| Epoch or gen older than local                                   | `WrongOwner` with no location hint; client re-resolves.                                                                                                         |
| Local state `MOVING`, `SUSPENDED`, `DROPPING`, or durable fence | Reject even with a valid ticket (`Moved` / `Unavailable`); the local fence overrides. Client re-resolves after backoff.                                         |
| `grant_ver` below the agent's revocation floor for `sub`        | `TicketRevoked`. Floors arrive as best-effort pushes from the control plane; the TTL bounds a missed push.                                                      |
| Control plane unreachable                                       | No effect on admission. Existing tickets serve until `exp`; new resolves fail; clients keep using cached tickets until they expire.                             |
| Agent restart                                                   | Unchanged from planes: stay closed, confirm orphan workers are gone, reconcile owner/epoch/gen/fences with the control plane; stay closed if it is unavailable. |

The property that prevents stale execution is the same as in planes-fable: the agent's durable local epoch and
fence. The ticket only makes the routing answer portable to the client.

### 3.3 Why the locator may be stale

A stale locator answer names the old VM or the old epoch. The old VM either rejects (it is fenced or no longer
holds the database) or never activates (wrong epoch). The client then re-resolves with
`min_epoch = last
seen + 1`, which forces the locator to read from the primary. So the locator can run as many
stateless replicas as needed, behind any load balancer, reading replicas or a cached snapshot. Correctness
lives at the agent, load lives at the locator, and they do not need to agree in real time.

### 3.4 Moves

The offline move protocol is unchanged: CAS `ACTIVE → MOVING`, fence source, settle, checkpoint, close
handles, durable fence acknowledgement, stream agent to agent, verify, flip owner and epoch in one Postgres
transaction, activate target after it reads that commit, clean source. What changes is what clients see:

1. While fenced, the source answers `Moved` to every ticket holder (no hint; the source must not guess the
   target before the flip commits).
2. Clients back off with jitter and re-resolve. The locator coalesces these. Until the flip commits it returns
   the database as `MOVING` with a retry-after; after the commit it returns the target and a new ticket at the
   new epoch.
3. Optional later: the control plane pushes a "moved" notice to clients holding long-lived connections, to
   shorten the gap. Not required for correctness.

### 3.5 Writes and request ids

Unchanged in substance: a mutation carries a client-generated request id; the actor records it in the same
transaction as the mutation (planes-fable `lane-request-ids`). On an ambiguous failure the client retries
against wherever the locator now points; the new owner's request-id store (copied with the snapshot) answers
"already applied" or applies it once.

### 3.6 Exposure: direct endpoint or stateless edge

Two ways to reach agents; the choice is open.

- **Direct.** Each VM has a stable name (`vm-<id>.data.<domain>`) and a TLS certificate; the locator returns
  that name. Lowest latency, one hop. Costs: every agent is internet-facing (rate limiting and connection caps
  move into the agent), certificate management per VM, browser CORS on agents.
- **Stateless edge.** A thin L7 proxy tier routes on the ticket's `vm` claim (or on the hostname the locator
  returned) with no lookup and no state. Agents stay private. One extra hop, but it is horizontally scalable
  and does no auth, compile or Postgres work, which is what made the planes gateway heavy.

Recommendation: start with the stateless edge (agents stay private, no per-VM certificates), keep tickets
audience-bound so moving to direct later needs no protocol change.

## 4. The actor as the scaling point

Because the actor owns everything about one database on the query path, scaling work becomes local to it:

1. **Group commit.** The mailbox already serializes writes for one database. Draining several queued mutations
   into one `BEGIN IMMEDIATE … COMMIT` (each with its own savepoint and request id) turns N fsyncs into one.
   This is the biggest available win for a hot single-writer SQLite file.
2. **Per-actor admission.** Mailbox depth, read permits and statement deadlines per database, under tenant and
   VM ceilings, replacing today's process-wide caps (§5).
3. **Pinning.** A hot actor gets its own lane (M3), then its own VM (an ordinary move). No client change: the
   next resolve returns the new location.
4. **Passivation.** Idle actors close handles; descriptor budget (M8) counts active actors, not hosted ones. A
   VM can host far more databases than it keeps open.
5. **Read followers (later).** An actor can ship WAL frames to follower actors on other VMs; the locator
   returns follower endpoints for read-only tickets (`perms = read`, a `follower` flag, bounded staleness).
   Writes still go only to the epoch owner. This is where a node-loss SLO would eventually come from; it is
   out of scope for v1.
6. **Splitting a database is not offered.** One database is one SQLite file with one writer; a tenant that
   outgrows that creates more databases (the planes product shape).

## 5. Contention and single points of load

Two lists: what the landed code concentrates in one place today (and how option 4 removes or moves it), and
the new concentration points option 4 itself creates.

### 5.1 Hotspots in the landed code (`main` at `fac3e3e`)

Ranked by likely impact. "Process-global" means one per server process, shared by every app and tenant. Rows
1, 2, 5 and 6 were re-read against the code; the rest come from a code survey and are marked **(inferred)**
where they were not run.

| #  | Hotspot                                                                                                                                                                                                                                                                                              | Where                                                                                              | Under load                                                                                                                       | Option 4                                                                                                                                           |
| -- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1  | **API-key cache holds one global lock across the Postgres call.** `Semaphore(1)` wraps cache lookup _and_ the miss query; 1024 entries, FIFO eviction, 30 s TTL; admin key changes take the same lock and clear the whole cache.                                                                     | `platform/http/auth.ts:110-139`, `platform/layers.ts:457`                                          | One miss blocks every other request's auth, hits included, for a Postgres round trip. Above 1024 active keys the cache thrashes. | Gone from the query path: the agent verifies tickets offline. The issuer must use a per-key single-flight, not a global lock.                      |
| 2  | **Session auth opens a new Postgres connection per request.** `postgres(url, {max: 1})` created and closed per request; `getSession` with cookie cache off, then a `memberships` SELECT, then an uncached `getTenant`. Better Auth rate limit is one shared bucket (inferred: size and persistence). | `platform/auth/service.ts:37-69`, `platform/http/auth.ts:166-172`                                  | TCP + auth handshake and ≥3 queries per request; Postgres connection count grows with request rate.                              | Moves to ticket issuance: once per ticket TTL per principal per database, not per request.                                                         |
| 3  | **One shared registry pool** (postgres.js default, inferred max 10); `SELECT … FOR UPDATE` on the tenant row for key issue and deactivate.                                                                                                                                                           | `platform/tenants/store.ts:278`, `:165`, `:201`                                                    | Pool exhaustion queues every auth miss and admin call behind ~10 connections.                                                    | Control plane only; locator reads replicas (§3.3).                                                                                                 |
| 4  | **All blocking SQLite work runs on the one JS event loop.** Writes, PK selects, `BEGIN`/`COMMIT` use the synchronous FFI path; `synchronous=FULL`, `busy_timeout=5000`.                                                                                                                              | `platform/sqlite.ts:48`, `:917-919`, `platform/sqlite_ffi/database.ts:174`                         | (Inferred) every commit's fsync, and up to 5 s of lock wait, stalls the whole process for all tenants.                           | Execution moves into lane workers per actor (M3); the agent front never runs SQLite. Group commit (§4) cuts fsyncs.                                |
| 5  | **Tenant handle cache: one global lock held during cold opens and evictions.** 64 handles, 60 s idle; resolve + prepare (`BEGIN IMMEDIATE` + DDL) + open all run inside `Semaphore(1)`; request 65 waits on a `Deferred` with no timeout, and mutations have no request deadline.                    | `platform/sqlite.ts:1015-1145` (lock `:1029`, open `:1099-1117`)                                   | One cold tenant blocks every warm tenant's acquire and release; with 64 leased handles requests wait unbounded.                  | Replaced by the per-VM actor directory: activation is per database (single-flight per id, not a global lock); passivation frees descriptors.       |
| 6  | **Per-app `prepareLock`** on every tenancy query; the router's `locations` map is never evicted.                                                                                                                                                                                                     | `platform/layers.ts:226-238`, `platform/tenants/routing.ts:64`                                     | Cheap when warm; a cold prepare (busy 5 s) holds it for all tenants of the app. Map grows with tenant count.                     | Provisioning happens once at create time (control plane → agent), not on the query path.                                                           |
| 7  | **Document reads share one process-wide cap** of 32 permits and 1000 pending; each query reopens the file read-only.                                                                                                                                                                                 | `platform/document_reader.ts:77`, `:156`, `:417-422`                                               | One tenant's slow scans (5 s deadline) can hold all 32 permits; others get `QueueFull`. Open/close per read.                     | Per-actor read pool with reused read connections, under tenant and VM ceilings.                                                                    |
| 8  | **Writer queue per database** (`WRITER_QUEUE_CAP = 1000`, `Semaphore(1)`); backup `VACUUM INTO` runs on it.                                                                                                                                                                                          | `platform/sqlite.ts:147`, `:482`, `:769-787`, `:973-985`                                           | Fine isolation per database, but a backup blocks that database's writers for the whole vacuum.                                   | This _is_ the actor mailbox. Snapshots move to a read-only connection (M3).                                                                        |
| 9  | **One reader child per server, strictly one request at a time** (ping/health, ingest, analysis); IPC queue 1000, 30 s call timeout, 5 restarts / 10 min then fatal.                                                                                                                                  | `platform/reader.ts:3`, `:236`, `platform/ipc.ts:226`, `:477`                                      | A long ingest or analysis blocks `/health` until timeout.                                                                        | Off the query path already; split health from analysis in the agent. Flag for an observability item.                                               |
| 10 | **Event-log ingest: one DuckDB file, one writer.**                                                                                                                                                                                                                                                   | `platform/eventlog/ingest.ts:465`, `:1035`                                                         | Off the hot path; long runs hit the 30 s IPC deadline while still committing.                                                    | Per-agent ingest later; out of scope here.                                                                                                         |
| 11 | **One log queue and one JSONL file per process**, dropping queue of 10 000; one `sql` event per statement; not partitioned by tenant.                                                                                                                                                                | `platform/logging/logger.ts:326`, `platform/logging/file_sink.ts:160`                              | Never blocks requests, but drops events for every tenant under load.                                                             | One sink per lane (lanes proposal); tenant/database id on every event.                                                                             |
| 12 | **Backup is per app and not tenant-aware.** One backup per app at a time, sequential sweep; whole snapshot read into memory, single PUT (60 s timeout, 3 retries). (Inferred, not run) in tenancy mode the snapshot path has no `TenantContext`, so tenant databases may not be backed up today.     | `platform/backup.ts:274-353`, `:322`, `platform/server.ts:174-188`, `platform/sqlite.ts:1078-1080` | Memory scales with database size; sequential sweep scales with app count; tenant files possibly skipped.                         | Per-database backups inside the agent with recorded manifests (M6); streaming/multipart upload. **Worth verifying now, independent of scale-out.** |

Takeaway: today's tightest spots are **auth** (rows 1–3) and **one event loop and one lock in front of every
tenant's SQLite** (rows 4–6). Planes-fable moves auth into the gateway tier, where rows 1–3 would still apply
per gateway; option 4 moves it to ticket issuance, once per ticket instead of once per request. Rows 4–8 are
what the per-database actor replaces.

### 5.2 New concentration points option 4 creates

| Point                         | Load shape                                                                     | Mitigation                                                                                                                                               |
| ----------------------------- | ------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Locator re-resolve storms     | All clients of one database re-resolve at once after a move, VM loss or expiry | Single-flight per database, `retry-after` while `MOVING`, client jitter and refresh-ahead at ~80 % TTL, stale reads allowed (§3.3)                       |
| Ticket issuance (auth lookup) | One Postgres auth/grant lookup per ticket instead of per request               | Cache principal → grants for the ticket TTL in the issuer; longer TTL trades revocation latency for load; revocation floors pushed to agents             |
| Hot actor (one database)      | Single SQLite writer, one mailbox                                              | Group commit, write-first scheduling, dedicated lane, dedicated VM, later read followers; per-database cap returns `QueueFull` to that database only     |
| Agent front per VM            | One process terminates every client connection for every database on the VM    | Front is thin (verify + compile); execution is in lane workers; multiple front workers via `reusePort`; connection and rate caps per tenant at the front |
| Placement primary (Postgres)  | Lifecycle writes and `min_epoch` reads                                         | Locator reads replicas by default; only `min_epoch` misses hit the primary; lifecycle writes are per-database serialized and rare                        |
| Key distribution              | Every agent needs the current ticket public keys and revocation floors         | Overlapping key validity (old + new kid), pull on unknown `kid` with backoff, push for floors; TTL bounds missed pushes                                  |
| Health / usage reports        | m VMs × report interval into the control plane                                 | Already in planes (`cp-health`); batch per VM, not per database                                                                                          |
| Observability fan-in          | Per-lane JSONL into one ingest and one DuckDB reader (§5.1)                    | Out of scope for option 4, as in DP-01 DP10; flagged for a separate item                                                                                 |

## 6. Costs and risks

- **Trust boundary moves into the agent.** The compiler and request validator now run on every VM that clients
  can reach. They must be the same trusted code the gateway would have run; customer SQL stays impossible. The
  agent's attack surface grows.
- **Clients become stateful.** The SDK holds location, tickets and retry logic. Non-SDK callers (curl, other
  languages) must implement resolve → ticket → call, or go through the stateless edge with a token exchange.
- **Revocation latency is the ticket TTL** unless the floor push arrives. Same bound as planes-fable's 30 s
  grant TTL if the TTL is set to 30 s.
- **More operational surface on agents**: TLS, rate limiting, connection caps, possibly CORS.
- **Cross-database requests** (one call touching two databases on different VMs) become client-side fan-out.
  Planes did not support atomic cross-database transactions either.

## 7. What would make this the wrong choice

- If clients are mostly untrusted browsers and per-VM exposure is unacceptable, the stateless edge is
  required, and the win over planes-fable shrinks to "no auth/compile/Postgres on the hop".
- If per-request revocation (not TTL-bounded) is a requirement, a gateway that checks every request is
  simpler.
- If the measured gateway hop in planes-fable is small next to SQLite time (a few hundred µs against a 4 ms
  p50), option 4's main gain is availability and fan-in, not latency.

## 8. Open questions

1. Ticket TTL (30 s like the M1 grant, or 60 s–5 min with revocation floors).
2. Direct agent endpoints or a stateless edge (§3.6).
3. Whether read followers are in v1 (recommended: no).
4. Group commit batch limits (max mutations, max wait).
5. Placement rule (M8), unchanged and still pending.

## 9. Sources checked

- `docs/pages/content/mansio-planes-fable/inventory.md` (F1–F11, admission table, trust boundary, move
  protocol).
- `docs/architecture/scale-out-comparison-fable.md` (§1–§4).
- Landed code, for §5.1: listed with each row.
