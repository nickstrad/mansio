# Scale-out: lanes versus planes, a comparison and a pick (Fable)

Status: analysis · Author: Fable 5.1 (with nickstrad) · Date: 2026-10-20 · Branch: `main` · Code baseline:
`main` at `eb5df16` (DP-01 landed).

Nick asked for one document that compares the two scale-out proposals in `docs/pages/`, picks one, and says
whether and how I would modify the pick. This is that document. It changes nothing else: both datasets, the
code and every existing document stay as they are.

## Answer

**I pick planes** (`mansio-planes-proposal`), with **eight modifications**, three of which import pieces of
the lanes proposal and two of which close deferrals the DP-01 review left open. The deciding reason is the
unit of placement: a database rather than a tenant. The lanes model is the special case of the planes model
where every tenant owns exactly one database, and nothing in lanes can express the general case without
becoming planes. The second reason is that planes has a specified, adversarially reviewed lifecycle (create,
move, drop, fence, restart) where lanes has a paragraph and a lease. What lanes does better, a lighter control
plane that is not on the serving path, I would import rather than give up: modification M1 below.

## 0. How to read this, and where I stand

Three kinds of statement appear below, and I mark them:

- **Landed** means it runs on `main` at `eb5df16` and is drawn as `existing` in `mansio-current` (46 elements,
  79 relationships, all `existing`, pinned to `2d30330`; nothing under `core/`, `platform/` or `sdk/` changed
  between `2d30330` and `eb5df16`, which only added `docs/pages/` content and docs).
- **Lanes says** or **planes says** means the dataset, its inventory or its documents state it. Where the
  dataset is silent and only a source document speaks (the lanes README on `docs/ffi-c4` at `81d9936`, my
  scale-out ranking on `arch/fable-ranking`), I say which.
- **My opinion** is marked as such, and §3 to §5 are all opinion.

**Disclosure.** The lanes proposal is my design. It is pick F of my scale-out ranking of 2026-10-12
(`scale-out-final-fable.md`, branch `arch/fable-ranking`), drawn as C4 by me on `docs/ffi-c4` and converted to
the dataset by PG-01 and PG-02. Three positions from that work recur here, and a reader may want to discount
them: that the in-node half (child-process lanes, write-first scheduling, snapshots off the writer lock)
should be built before any placement work; that placement should be a table in the existing registry with "no
new service to operate"; and that ownership should be lease-fenced. In this comparison I keep the first,
import the second in a weakened form (M1), and drop the third in favour of the planes fencing rule, and I say
why at each point. I also ranked "Opus A: control/data plane split with explicit placement" fourth in that
ranking because it did not address in-node blocking. Planes is, in shape, Opus A with the in-node lanes added,
Astra 2's fencing rules adopted, and placement moved from the tenant to the database. My own ranking said that
with the lanes added, A "converges on F". It did, and then it went one step further than F on granularity. So
picking planes is consistent with what I wrote then, not a reversal, but the reader should know that the
in-node half I praise in planes is the half I designed.

## 1. The two proposals in their own terms

### 1.1 Lanes: statement lanes and tenant placement

Dataset `mansio-lanes-proposal` (revision `docs-ffi-c4-81d9936-on-main-2d30330-pg02`): 39 elements (22
`planned`, 9 `existing`, 8 `mixed`), 46 relationships, 12 diagrams (context, container, one component view of
a lane, 8 dynamic flows, one illustrative deployment), 28 documents. Converted 2026-10-15 from
`docs/plans/ffi/` on `docs/ffi-c4` and corrected against `main` at `2d30330`, where the FFI execution layer
had landed.

As its authors would state it: the platform keeps one SQLite file per tenant, which exists today, and adds two
things. Across nodes, a `placement` table beside the tenant row in the existing Postgres registry maps each
tenant to an owner node with a lease and an epoch. Any node authenticates a request, derives the tenant from
the credential, and either executes locally or forwards to the owner with the trusted tenant id, the placement
epoch and a request id; a stale epoch answers `WrongOwner`, the forwarder refreshes the placement once and
retries; the client contract `createClient(baseURL, appId, token)` is unchanged. Inside a node, supervised
child-process statement lanes own the tenant files: each tenant is sticky to one lane; a lane has a bounded
statement pool with a per-tenant sub-cap (an over-cap tenant gets a typed `QueueFull` for itself only), a
handle LRU with a read pool, a write-first scheduler (a mutation waits only on its own file's writer lock,
never on read permits), a request-id store written inside the mutation's transaction, and a deadline watchdog
that interrupts a long statement through `sqlite3_interrupt` and, only if the interrupt does not return within
the grace period, asks the supervisor to kill the lane. Snapshots move to a read-only connection so a backup
never holds the writer lock. WAL frames to object storage are phase two. The design's figures come from
measured proofs of concept on vm-1 (2026-10-12): point read 6 to 7 µs on the blocking FFI step, 44 to 69 µs on
the nonblocking step, interrupt about 0.4 ms, lane kill 3.8 ms plus 51 ms to a replacement's first answer, 3
descriptors per writer handle and 2 per reader.

What the lanes dataset deliberately does not draw: a component view of the API process after the change, the
observability path (unchanged apart from one JSONL directory per lane), any tenant lifecycle (create, move,
drop), node loss, or backup end to end. Its README and my ranking say moves are "a four-step epoch dance" done
by an admin CLI command, and node loss is "restore its tenants onto another node from `latest.json`".

### 1.2 Planes: control plane, data plane, tenants own many databases

Dataset `mansio-planes-proposal` (revision `dp-01-repair-2026-10-20`): 44 elements (40 `planned`, 2 `mixed`, 2
`existing`), 55 relationships, 15 diagrams (context, container, three component views, 9 dynamic flows, one
illustrative deployment), 60 documents. Built by Codex Sol as DP-01 on 2026-10-20 from Nick's brief and
addendum, reviewed by Codex Astra (FAIL on three required tweaks, repaired, then PASS WITH NITS), landed as
`eb5df16`.

As its authors would state it: a tenant owns zero or more databases, and the **database** (an immutable
`database_id`, with a name unique within its tenant that is never a path) is the unit of placement, file,
isolation, backup and lifecycle. A **control plane** owns the registries (tenants, databases, placement as
`(database_id, owner_node, epoch)`, durable operation records, all in Postgres with hand-written SQL and
compare-and-swap transactions), the provisioner, the lifecycle controller (move, suspend/resume,
backup/restore, drop) and the **routing authority** that resolves `(tenant, database)` to a VM and epoch. A
**data plane** (the gateway: ingress, session and database authorization, forwarder) consults that authority
and forwards each request to the hosting VM over authenticated RPC; rows return through the forwarder, never
through the router. Every VM runs a **VM agent (process)**, a daemon that is the only thing on the VM that
touches tenant files; it executes queries through private statement-lane workers (the landed FFI binding,
classifier and deadline path, reused unchanged), and on instruction from the control plane's lifecycle
identity it provisions, fences, checkpoints, copies out or receives, restores and deletes files, and reports
health, size and usage. Placement of a new database is a control-plane decision; the selection policy is an
explicit open question (Nick's addendum: do not invent a solved mechanism), but eligibility inputs are named
and a VM with stale health reports is ineligible.

The repaired contracts are the dataset's distinctive content. Admission: every request, including one on an
existing session, needs an online one-request decision from the authority, bound to database id, epoch and
generation, principal, permission, request id and a deadline of at most 30 s; no prefetch, no reuse, no lease;
when the authority cannot be confirmed, all new admissions fail closed. The agent checks the decision against
its own durable local `ACTIVE` epoch and fence. Moves are offline per database: CAS `ACTIVE` to `MOVING`,
fence the source (all admissions, including existing sessions), settle, checkpoint through a maintenance
connection, close every handle, acknowledge the fence durably, stream the snapshot agent to agent under a
move-scoped credential, verify, flip owner and epoch in one Postgres transaction, activate the target only
after it reads that commit, then clean the source. An unreachable source means the database is unavailable,
not failed over. Lifecycle commands carry database id, operation id, expected epoch and generation and the
intended transition, are serialized per database, and are rechecked before filesystem effects, so a delayed
A-to-B cleanup cannot delete A's later generation after B-to-A. Drops remove files, sidecars, staged copies
and backups before the live registry rows go, keep a non-routable tombstone, and never report success while
cleanup is pending. The trust boundary separates an execute-only gateway identity from the lifecycle identity,
and only trusted gateway code derives operation IR and SQL from a validated document request.

What planes explicitly defers (recorded in the inventory): end-to-end backup publication, VM-loss recovery and
a replacement cleanup executor for a lost agent (review tweak 6); instance-level source/target move diagrams
(tweak 7); placement policy and weights, numeric limits, fairness weights, report intervals, RPO and RTO.

### 1.3 The landed baseline both start from

Landed, drawn in `mansio-current`: one API process; every application SQLite file opened through the FFI
binding over the system `libsqlite3` (`platform/sqlite_ffi/`); the statement classifier on the operation IR
(`classify.ts`), which never reads SQL text; unbounded reads on the nonblocking path under
`statementDeadlineMs` with `sqlite3_interrupt` and HTTP 504; one file per tenant at `TENANTS_DIR/<tenant>.db`
behind the `TenantStorageRouter` seam, whose config already reserves `router: "control-plane"` but whose
interface is `resolve(tenantId)`; the Postgres registry (tenants, API keys, Better Auth users, sessions,
memberships); `Storage` with one writer per file under `Semaphore(1)`, `WRITER_QUEUE_CAP = 1000`, a 64-entry
leased handle cache with 60 s idle expiry; the document reader with a 32-permit read cap and a 1000-deep
admission cap shared by all tenants; backups by `VACUUM INTO` on the writer's queue, then `quick_check`,
SHA-256, immutable PUT, manifest and `latest.json`; and the supervised DuckDB reader child. Neither proposal
changes the FFI layer; both reuse it as `existing`.

## 2. Compare and contrast

The table is the summary; the subsections carry the evidence.

| Axis                   | Lanes                                                       | Planes                                                                                   |
| ---------------------- | ----------------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| Unit of placement      | Tenant (one file)                                           | Database (one file; tenant owns 0..N)                                                    |
| Writer-lock domain     | Tenant                                                      | Database                                                                                 |
| Process isolation unit | Lane (sticky set of tenants)                                | Lane worker behind the agent (set of databases); same blast radius, admitted             |
| Routing authority      | Each API node, from a cached placement table                | Control-plane service, consulted per request by RPC                                      |
| Registry down          | Serve from cache until the lease expires, then stop writes  | All new admissions fail closed within one RPC                                            |
| Fencing a move         | Lease by the owner's clock, plus epoch                      | Durable fence acknowledged by the source, plus epoch; no clock                           |
| Unreachable owner      | Lease expiry lets the move proceed                          | Database unavailable until an operator proves isolation (deferred)                       |
| Process roles          | API node (with lanes and reader), Postgres, object store    | Gateway, control plane, VM agent (with lanes), Postgres, backup store                    |
| Lifecycle drawn        | Point read, long query, forward, retry, lane kill (8 flows) | Create, query, stale route, deadline, move, drop, each with a failure scenario (9 flows) |
| Component depth        | Inside a lane: 9 components with measured timings           | Control plane 7, data plane 5, agent 5; lane internals by reference                      |
| Distance from landed   | 17 of 39 elements `existing` or `mixed`                     | 4 of 44 elements `existing` or `mixed`                                                   |
| Client contract        | Unchanged                                                   | Adds database selection per session or call (planned, unspecified)                       |
| Product                | A database per tenant, scaled invisibly                     | Customers hold databases and reach any of them through one path                          |

### 2.1 Tenancy and isolation

Lanes says the tenant is the file, the placement row and the lane-sticky unit. A tenant's hot writes and its
heavy scans share one WAL, one `BEGIN IMMEDIATE` queue and one lane. The lanes answer to a hot tenant is tweak
4 of my ranking, hot-tenant lane pinning: give that tenant its own lane so its write bound is its own
duration. That bounds interference from other tenants; it cannot bound interference from the tenant's own
scans, because they are in the same file.

Planes says the database is the file and every file has its own transaction and writer-lock domain (DP1,
`doc-files`). A tenant can put orders in one database and archive in another, on the same VM or on different
VMs (the illustrative deployment: Alpha has orders and archive on A and analytics on B). Planes is honest that
this is logical and file isolation only: a lane worker can serve several databases, so a kill affects every
in-flight request on it, and VM failure and resource contention remain shared (DP9, tweak 4 repair). It
carries the lanes proposal's bounded pool, scheduler and handle-cache policies forward keyed by database, with
aggregate tenant and VM ceilings, so a tenant with many databases cannot consume the VM.

My opinion: on this axis planes is strictly more expressive. The lanes model is planes with N fixed at 1, and
the tenant-wide writer lock is a real limit that no amount of lane scheduling removes.

### 2.2 Placement granularity and its consequences

Lanes places tenants; my ranking says a new tenant lands on the least-loaded live node at creation, with node
capacity derived from `nofile`, a reserve, and 3 descriptors per writer plus 2 per pooled reader (tweak 5).
One row per tenant; a move is a whole tenant.

Planes places databases, n files across m VMs. More rows, more moves, finer bin-packing, and a per-database
move that leaves the tenant's other databases serving (`doc-flow-move-database`: "Other databases can remain
active, except for the documented shared-lane kill blast radius"). It leaves the selection policy open on
Nick's instruction, but it names the inputs (fresh agent health, compatible versions, disk with WAL, snapshot
and move headroom, resource ceilings, locality) and adds the `cp-health` report edge with incarnation and
sequence numbers so stale reports make a VM ineligible without ever being taken as proof of fencing.

The consequence planes records and lanes does not have: the landed router seam resolves a tenant, not a
database (DP4), so planes needs a database-aware `ControlPlaneRouter`, a database-selection surface in the
client and the operation IR, and authorization per selected database. That is new API, and the dataset says so
without specifying it. Lanes needs no API change at all.

### 2.3 Routing: where the logic lives and what fails when it is down

Lanes says the placement router is a component of every API process, reading the owner from its cache and the
table only on a miss (the 30 s TTL pattern the registry already uses for hashed keys), with
`WrongOwner`-plus-one-refresh for staleness. "There is no new service to operate." Ownership is a heartbeat
lease the node judges by its own clock; when the registry is unreachable, cached placement keeps routing and a
node stops serving a tenant's writes when its lease expires. The dataset draws this only as the label
"placement lookup and lease heartbeat" on `ctx-4` and `ctr-2`; the lease semantics are in my ranking, not in
any flow.

Planes says routing authority is a control-plane component reached by RPC from gateway authorization, and
after the repair, that the authority issues a single-use decision per request with no prefetch and no lease.
Read `flow-query-right-node` as drawn: 21 steps, of which the gateway makes one registry call (name, ownership
and grants, `dp-auth-sql`) and one routing call (`dp-route`) before it forwards. Both are control-plane calls,
on every request, including the second request of a session that just succeeded. When the authority cannot be
confirmed, the state table says: reject new requests, including existing sessions; admitted work settles
within its deadline. Astra's review offered the alternative of "existing access continues only under
explicitly defined locally installed authority" and the repair chose to stop everything. That choice is
defensible for a first design because it leaves no case where a stale authorization executes, but its cost is
that the control plane and Postgres are now tier-one serving dependencies and every query pays an RPC round
trip on a path whose landed p50 is about 4 ms (`getGame`, SP-06).

My opinion: this is the one axis where lanes is better as written, and it is the axis I modify in planes (M1).
The property that actually prevents a stale request from executing is the agent-side check against its durable
local epoch and fence, which both designs have in some form; single-use decisions add revocation latency
bounded by 30 s, which a cache TTL bounds equally well.

### 2.4 The execution path and what it reuses

Both reuse the landed FFI binding, the IR classifier and the deadline and interrupt path unchanged, and both
propose child-process lanes as the kill unit with a watchdog that escalates to the supervisor only after an
interrupt fails to return. Both keep the write-first rule, the bounded pool and the request-id store.

Lanes draws the inside of a lane: IPC endpoint, classifier, write-first scheduler, bounded pool, watchdog,
handle LRU with read pool, request-id store, FFI binding, logger, with the timings above attached in
`doc-measurements`. It leaves one gap: the statement request carries compiled SQL, but the landed classifier
reads the IR (discrepancy 11), so a lane needs the operation or its class on the wire.

Planes closes that gap by carrying operation IR alongside SQL and binds (DP9) and adds the trust rule that
only gateway code derives both from a validated document request, so a client can never pair a bounded IR with
an unbounded statement. It does not draw lane internals; its agent component view is admission, classifier,
execution, deadline and files, and the inventory retains the lane policies by reference.

Where execution runs differs: lanes runs it inside the API process's children on the same node that fronts the
request or on its peer; planes runs it behind a separate agent daemon, with a gateway tier in front. The extra
hop is an authenticated RPC in both cases (the forward); planes always pays it, lanes pays it only when the
receiving node is not the owner.

### 2.5 Failure semantics

**Restarts.** Lanes draws the lane kill and respawn: in-flight statements fail with a typed lane-restart
error, the supervisor respawns a lane with the same sticky tenant set, in-flight mutations are resolved by
request-id lookup, never replayed. It does not draw an API-process restart. Planes draws no lane kill flow of
its own (the deadline alternative describes it) but specifies the agent restart: stay closed, terminate and
confirm exit of orphan workers before opening any file, reconcile committed owner, epoch, generation and
fences online, and stay closed if the authority is unavailable. It also specifies the controller crash between
placement commit and activation acknowledgement: the target is the sole owner and stays closed until
idempotent activation reconciles; never roll back.

**Moves and fencing.** Lanes: freeze in placement, snapshot plus last WAL frames (phase two) to the object
store, restore on the new owner, flip and bump the epoch, with the epoch flip waiting for the old lease to
expire or the old owner to acknowledge the freeze. Planes: the full offline protocol in §1.2 with the
checkpoint ordering corrected in the repair, a pre-flip rollback that needs a verified intact source and
confirmed target cleanup and uses a fresh epoch, and after-flip failures that reconcile forward only.

**Split-brain exposure.** Lanes has a window: a partitioned old owner whose clock runs slow can believe its
lease is live after the registry considers it expired. Astra's point from the ranking, "a metadata epoch alone
cannot fence a live old SQLite writer", is why the lease exists at all, and a lease by the owner's clock
narrows the window without closing it. Planes closes it by construction: no flip without the source's durable
fence acknowledgement, and the price is availability, since an unreachable source leaves the database
unavailable.

**VM loss.** Lanes (ranking F5, not drawn): restore the node's tenants elsewhere from `latest.json`, recovery
point equal to the backup interval, under lease expiry. Planes: every hosted database becomes unavailable;
detection is not proof of fencing; no replacement writer activates without operator-verified isolation; the
recovery design is deferred, as is the question of who deletes a lost agent's backups during a drop.

My opinion: planes is safer and less complete; lanes is more complete and less safe. The right fix is to
complete planes (M5), not to loosen it.

### 2.6 Operability day to day

Lanes is one process type per node (API with lanes and the reader child), Postgres and the object store;
placement is a table and a CLI command; the only new operational surface is one JSONL directory per lane.
Planes is three service roles (gateway, control plane, agent) plus Postgres and a backup store, and every
lifecycle operation is a durable operation record with an id, a status and a tombstone, so "what happened to
database X" has an answer in a table. Planes omits observability on purpose (DP10, P8), so lane JSONL, the
DuckDB event log and the analysis catalog are not placed in the new topology. Planes leaves every numeric
limit open; lanes gives defaults for some (per-tenant sub-cap `min(lane_cap / tenants_on_lane, 8)` in my
ranking, `WRITER_QUEUE_CAP` today).

### 2.7 What each leaves deliberately open

Lanes: the SQL-versus-IR wire format; pool size and back-pressure under hundreds of long statements (the
study's own limit); the request-id table (inferred to live in the tenant file); the watchdog-to-supervisor
transport; the API process component view; every lifecycle flow; node loss.

Planes: placement policy and weights; numeric limits and fairness; report interval and freshness; RPO and RTO;
backup completion manifest, VM-loss recovery and replacement cleanup (tweak 6); instance-level move diagrams
(tweak 7); the database-aware client API (DP4); whether a tenant's databases share one application schema (the
create flow provisions "file/schema" per database and says no more); and, from the re-review's nit, that a
reopen after `WrongOwner` must obtain a fresh decision bound to the new epoch.

### 2.8 Adoption and migration from today

Lanes is incremental on the landed code. Its first step, the in-node half, needs no schema or API change and
has two acceptance tests that fail on `main` today (one tenant runs a 2 s statement while another's
once-a-minute mutation commits within 50 ms; a 1 GB backup runs while that file keeps committing). Its second
step registers the one existing node as the owner of every existing tenant and turns on the reserved
`router: "control-plane"`. The existing file per tenant is already the placement unit.

Planes needs the same in-node work (it retains it), plus a metadata schema with databases, operations and
placement, the agent daemon, the gateway split, the trust boundary and the lifecycle controller. Existing
tenant files have to become a database each, with a minted immutable id and a placement row. The client needs
a way to name a database. Measured by what the datasets draw, 4 of 44 planes elements are landed or partly
landed against 17 of 39 for lanes; the reused FFI code is the same in both, so the difference is the number of
new boxes, not reuse.

### 2.9 The product each implies

Lanes implies a per-tenant database service: an application gets one database, and the platform hides how it
is placed and scaled. The whole caller contract is `createClient(baseURL, appId, token)`.

Planes implies customers who hold databases: as many as they want, created and dropped by API, each with its
own lifecycle, reachable through one path by name regardless of where it lives. That is the Turso or D1 shape
(database per user, per environment, per project), and it is the framing Nick gave in the DP-01 brief. It is
also the framing under which the lanes model's tenant-wide file is not a scaling detail but a product limit.

## 3. The pick: planes

I pick planes. The deciding reasons, ranked:

1. **The database is the right unit of placement, isolation and lifecycle.** It removes the tenant-wide
   writer-lock domain, lets a tenant's hot and cold data live in different files on different VMs, and makes
   lanes a degenerate case rather than an alternative. Every lane-level mitigation for a hot tenant (pinning,
   sub-caps) still works inside planes, keyed by database, and planes already says so.
2. **It is the product Nick stated, and the two products are not the same product.** Arbitrary databases per
   tenant with dynamic access is a one-way door: lanes cannot be extended to it without re-keying placement,
   files, backups and request ids by database, which is DP1. Building lanes first would mean building its
   placement half twice.
3. **Its lifecycle is specified and has survived a hostile review.** Create, move, drop, their failure
   alternatives, restart, controller crash, delayed-command protection, generation-scoped cleanup, and the
   trust boundary between the execute-only gateway identity and the lifecycle identity are all written down,
   and Astra's FAIL-to-PASS arc tightened exactly the parts (routing on the result path, admission during
   outages, trusted compilation) that are expensive to get wrong later. The lanes dataset was converted
   faithfully from my diagrams but never reviewed for distributed semantics at this depth; its fencing is one
   sentence in a ranking document.
4. **The VM agent is the right seam.** A daemon that is the only toucher of tenant files, taking a narrow
   instruction set from a lifecycle identity and execute-only work from a gateway identity, is the natural
   home for the lane supervisor, for health and size reporting, and for provider-level operations later
   (power-off attestation in M5). Lanes puts all of that into the API process, where it competes with HTTP.
5. **Nothing of value in lanes is lost.** FFI reuse, child-process lanes, write-first scheduling, bounded
   pools, request-id dedup, `WrongOwner` refresh and the measured in-node numbers all carry over, and the
   planes repair explicitly retains the pool, scheduler and handle policies.

What lanes does better, and why it does not flip the pick: it keeps the control plane off the serving path
(§2.3), it has a stated node-loss story (§2.5), it draws the inside of a lane with numbers (§2.4), and it is
smaller and incremental (§2.8). The first two are modifications I make to planes below, the third is an
import, and the fourth is a cost I accept because the product requires the extra machinery anyway.

## 4. Modifications to the pick

Eight, each concrete. "Where" names the dataset part that would change if the modification is accepted; this
document changes nothing itself. Modifications that alter a contract Astra reviewed and passed (M1, M5) need
Nick's word and a re-review, not just a dataset edit.

### M1. Gateway route grants with a TTL, instead of a single-use decision per request

- **What changes.** The routing authority issues a _route grant_ per `(gateway, database_id)` bound to the
  owner VM, epoch and generation, the principal's grant version and a TTL (30 s, the number the repair already
  uses as the decision deadline). The gateway may use a grant for any number of requests until it expires, is
  invalidated by `WrongOwner`, or is revoked by a grant-version bump. The agent keeps checking every request
  against its durable local `ACTIVE` epoch and fence, which is what prevents stale execution. New resolutions
  still fail closed when the authority is unreachable.
- **Where.** `doc-placement-router`, `doc-node-admission`, the admission state table (row "Authority
  unavailable, running agent" becomes "existing grants serve until their TTL; no new grants"), steps s2 and s5
  to s9 of `flow-query-right-node`, and the re-review's nit on `flow-query-stale` (a reopen obtains a fresh
  grant bound to the new epoch).
- **What it buys.** Two control-plane RPCs per request become two per grant lifetime; the control plane stops
  being a tier-one serving dependency for steady traffic; an outage degrades to "no new opens after the TTL".
- **What it costs.** Revocation of a grant takes up to the TTL instead of up to the decision deadline, which
  is the same 30 s bound. A grant cache in the gateway with invalidation on `WrongOwner`. Astra's tweak 2 and
  its PASS are reopened on this one point.
- **Origin.** This is the lanes proposal's cached placement with `WrongOwner` refresh (my F1), moved to the
  gateway. It is _not_ the lanes lease: the owner never serves on a timer, only on durable state set by a
  fenced flip, so no clock enters the fencing argument.

### M2. Build the in-node half first, keyed by database, with the lanes acceptance tests

- **What changes.** The adoption order is written into the dataset's inventory as a decision: (1) agent plus
  lanes plus bounded pools on one VM, with handles, queues and request ids keyed by `database_id`; (2) the
  metadata schema and a default database per tenant (M4); (3) gateway, route grants, forwarder; (4) lifecycle
  flows. Step 1 is accepted by the two tests from §2.8 re-targeted to databases, and gated by the lanes
  measurement: `getGame` p50 through a lane against the landed 4.1 ms baseline, with more than about 2 ms
  added reopening the worker variant for reads only.
- **Where.** A new inventory decision; no diagram changes.
- **What it buys.** The contention that violates "hot write never starved" and "no statement blocks everyone"
  is fixed on today's hardware before any placement exists, and the riskiest cost (lane IPC on the hot path)
  is measured first.
- **What it costs.** Nothing in design; it defers the visible product feature (many databases) by one step.
- **Origin.** My ranking's "what to build first". Discount accordingly; the planes authors did not disagree,
  they did not order the work at all.

### M3. Adopt the lane component view inside the agent, plus snapshots off the writer lock and lane pinning

- **What changes.** The agent's worker internals are the lanes component view (IPC endpoint, classifier,
  write-first scheduler, bounded pool, watchdog, handle LRU with read pool, request-id store, binding, logger)
  re-keyed by database, with `doc-measurements` attached, and with two additions the planes documents do not
  state: backups and move snapshots run on a read-only connection (today `Storage.snapshot` runs on the
  writer's queue, `platform/backup.ts`), and placement may pin a database to a dedicated lane. Lane assignment
  is tenant-affine by default (a tenant's databases share lanes with each other before they share with another
  tenant's), so a kill's blast radius is first the tenant's own databases.
- **Where.** `planes-agent-components` gains the lane view as a drill-down; `doc-lanes`,
  `doc-lifecycle-policies` and `doc-flow-move-database` state the read-only snapshot connection; the placement
  record gains an optional pin.
- **What it buys.** The backup-versus-hot-write stall (the largest single write stall on `main`) is closed;
  the measured numbers travel with the design; the shared-lane blast radius DP9 admits is bounded by tenant.
- **What it costs.** A read-only snapshot connection costs 2 descriptors per database during the snapshot;
  pinning is one more placement attribute to operate.
- **Origin.** Lanes (my tweak 4 and F4c). The tenant-affine lane rule is new here.

### M4. A default database per tenant and a specified selection surface

- **What changes.** Every existing tenant file becomes database `default` with a minted immutable id and a
  placement row (owner = the one node, epoch 1). A client that names no database gets `default`, so
  `createClient(baseURL, appId, token)` keeps working. Selection is a `database` option on the client and a
  per-call override; the gateway resolves name to id under the session's tenant and stamps `database_id` into
  the operation IR after authorization, so the classifier and the agent never see a name. The name `default`
  is reserved per tenant.
- **Where.** DP4 and `doc-placement-router` (the planned `ControlPlaneRouter` becomes `resolve(databaseId)`
  with `TenantContext` extended by a database), `doc-ingress`, `doc-authorization`, a new migration note in
  the inventory.
- **What it buys.** The migration path from today in one step, with unchanged clients, and an end to the
  dataset's "database-aware API is planned work" without saying what it is.
- **What it costs.** One reserved name; the schema question in §2.7 must be answered (my default: a database
  carries its own application schema fingerprint, as `_platform_schema` does per file today).
- **Origin.** New here; it follows from the landed `routing.ts` and `prepare` logic.

### M5. Close the VM-loss deferral with operator-attested isolation and restore-elsewhere

- **What changes.** A lifecycle operation `recover-from-loss(vm)` with a hard precondition: the operator
  attests the VM is isolated, backed by a provider-level proof (the VM is powered off or its network is
  detached through the provider API, and the control plane records that response). Then, per database the VM
  hosted: CAS the placement to `FENCED-BY-OPERATOR` at a fresh epoch, run the existing restore flow from the
  latest _recorded_ backup (M6) onto an eligible VM as a fenced generation swap, and activate. Recovery point
  is the backup interval, stated as such. For a drop whose hosting agent is permanently lost, the control
  plane's lifecycle identity deletes the backup-store objects itself (the backup store is not VM-local, so the
  agent-only rule does not apply to it), and the local files are recorded as destroyed with the attested VM.
- **Where.** `doc-backups`, `doc-lifecycle`, `doc-lifecycle-policies`, `flow-drop-failed`, and one new dynamic
  flow with its failure alternative (attestation absent means the operation is refused).
- **What it buys.** The largest deferral (tweak 6) gets a bounded first answer using only pieces the design
  already has: verified backups, the restore flow, generation swaps and fresh epochs. It stays explicitly not
  automatic failover.
- **What it costs.** An attestation primitive and a provider dependency; a human in the loop for every VM
  loss; the old file on a VM that comes back must be treated as garbage (its epoch is dead) and removed by a
  generation-scoped cleanup, which the delayed-command rule already covers.
- **Origin.** Lanes F5 (restore from `latest.json`) for the mechanism; the attestation requirement is Astra's
  "explicit isolation/proof-of-fence prerequisite" made concrete.

### M6. Backup completion is a recorded manifest, keyed by database and generation

- **What changes.** The landed pipeline (snapshot, `quick_check`, SHA-256, immutable PUT, manifest,
  `latest.json`) runs inside the agent per database, and the backup operation completes only when the agent's
  acknowledgement carries the manifest's object key, checksum, database id and generation and the control
  plane records them in the operation record. A backup is a restore candidate only if it is recorded there.
- **Where.** `doc-backups`, `doc-lifecycle-policies`, `c-node-backup`; `cp-health` or the backup operation's
  reverse edge carries the manifest.
- **What it buys.** Tweak 6's "completion boundary" with no new mechanism, and the input M5 needs.
- **What it costs.** A metadata column set and one more acknowledgement to persist before replying.
- **Origin.** Landed `platform/backup.ts`, re-keyed.

### M7. Draw the instance-level move

- **What changes.** `flow-move-database` and `flow-move-failed` get companion diagrams whose participants are
  instances (source agent A, target agent B, the lifecycle controller, the metadata store), using the same
  instance grammar the deployment views already use, so the fence acknowledgement, the agent-to-agent stream
  and the activation are drawn between distinct boxes. The lanes dataset did this for the node-to-node forward
  (P16); the same device works here.
- **Where.** Two new dynamic diagrams linked from the logical ones.
- **What it buys.** The re-review's remaining visualization gap closes; reviewers of M5 can see who holds what
  credential.
- **What it costs.** Diagram work only.
- **Origin.** Astra tweak 7, deferred by the repair.

### M8. A v1 placement rule, as a decision request to Nick

- **What changes.** Nothing in the dataset until Nick decides; his addendum forbids inventing a solved
  placement mechanism there, and I respect that. My recommendation for v1: among eligible VMs (the repair's
  eligibility inputs unchanged), choose the one with the lowest descriptor-weighted load, where a VM's
  capacity is `nofile` minus a reserve, and each database costs 3 descriptors for its writer plus 2 per pooled
  reader, with a size-based tiebreak. Weights and locality stay open.
- **Where.** Inventory decision A3, if accepted.
- **What it buys.** A create flow that can run end to end without a human choosing a VM.
- **What it costs.** It is naive about CPU and I/O; it is meant to be replaced by the `cp-health` data once
  there is some.
- **Origin.** Lanes tweak 5, the descriptor formula.

I would not change the fencing rule (no flip without the source's durable acknowledgement) or the no-lease
admission on the agent side. My own lease-fenced tweak was the weaker guarantee; planes is right to require
proof, and M5 is how proof is obtained when the source is gone.

## 5. What would change my mind

- **The product reverts to one database per tenant.** If Nick decides tenants do not hold arbitrary databases,
  reason 2 disappears and reason 1 shrinks to a hot-and-cold split the platform cannot expose. Then lanes,
  plus the planes lifecycle contracts re-keyed by tenant, is the smaller build and the better pick.
- **M1 is rejected and the per-request RPC is measured as unacceptable.** If the control plane must stay on
  the path of every request and that adds more than about 2 ms to the 4 ms p50 (the lanes gate), the
  node-local cached placement of lanes becomes the only design that meets the latency the landed system has.
- **Operations capacity stays at one person for a long time.** Three service roles plus Postgres is a real run
  cost; if that cost dominates and the many-databases product can wait, lanes first and planes later is
  defensible, at the price of building placement twice.
- **A node-loss SLO is adopted.** Then neither proposal is enough: both state RPO as the backup interval and
  neither has a live replica. My ranking's answer stands: return to Astra 3's replicated shards for the
  tenants that need it, or adopt WAL streaming, which both proposals defer.
- **The in-node measurement fails.** If child-process lanes add too much on the hot path in step 1 of M2, both
  proposals lose their in-node half equally; that would not change the pick between them, but it would reopen
  the worker variant for reads in whichever is built.

## 6. Sources checked

- `docs/pages/public/architectures/mansio-lanes-proposal.json` and
  `docs/pages/content/mansio-lanes-proposal/inventory.md` (P1 to P17, discrepancies 1 to 11);
  `docs/pages/public/architectures/mansio-planes-proposal.json` and
  `docs/pages/content/mansio-planes-proposal/inventory.md` (A1 to A4, DP1 to DP13, repair dispositions); every
  plain document in both bundles.
- `docs/pages/content/mansio-current/inventory.md`; `docs/architecture/README.md` audited-state table;
  `docs/notes/decisions.md`, `glossary.md`, `index.md`.
- `agent-work/items/dp-01-planes/brief.md`, `addendum-1.md`, `review-astra.md`, `review-astra-rereview.md`,
  `report.md`.
- `docs/plans/ffi/README.md` on `docs/ffi-c4` at `81d9936`; `docs/architecture/lang-switch-fable.md` at
  `d97bcf8`; `docs/architecture/scale-out-final-fable.md` and `scale-out-fable.md` on `arch/fable-ranking`.
- Landed code: `platform/tenants/routing.ts`, `platform/tenants/schema.sql`, `platform/sqlite.ts`
  (`WRITER_QUEUE_CAP`, handle cache), `platform/document_reader.ts` (`DOCUMENT_READ_CONCURRENCY_CAP`),
  `platform/backup.ts`, `platform/services.ts` (`router: "local" | "control-plane"`).
