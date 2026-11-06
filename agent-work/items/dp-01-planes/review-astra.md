# DP-01 design review

Verdict: **FAIL** — repair the routing contradiction and specify the admission and trust contracts below
before accepting this proposal. The requested architecture is sound enough to keep; this verdict does not call
for a different topology or a solved placement algorithm.

Reviewer: Codex Astra. Actor: `astra-dp-01`. Date: 2026-10-20. Reviewed HEAD:
`332db38253ab2436422122f08fc6ce17dc7beb1b`, including implementation `db0bb43`, the original brief and
addendum 1. Review scope is the design, not a claim that the proposed distributed system works.

The control plane owns placement and routing; the gateway provides one access path; VM agent processes own
last-mile execution and lifecycle. Tenant-scoped names resolve to immutable database IDs. Independent files,
queues and writer locks avoid the old per-tenant serialization domain. The uneven deployment demonstrates both
a tenant spanning VMs and a VM hosting several tenants. These are the right foundations.

The offline move also has the right fundamental ordering: persist MOVING, fence the source, settle work,
verify the target, atomically publish ownership, activate the target, then clean the source. Requiring source
fence acknowledgement and refusing guessed failover is a defensible first version. Durable fences and
request-ID state are already documented; they are not missing features. The remaining issue is making their
operational contract explicit enough that delayed commands and restarts cannot defeat that ordering.

## Prioritized tweaks

### 1. P1 — Keep routing authority out of the query-result transport path

**Diagram-level; required.** In `mansio-planes-proposal.json`, change `dp-forward`,
`flow-query-right-node-s8/s18/s19`, and the corresponding inventory rows. The router belongs to `control`, but
`dp-forward` runs from that router to the gateway forwarder. Step s18 explicitly returns query results through
the router. This contradicts `doc-data`, `doc-forwarder` and the container relationships `c-resolve` and
`c-forward`, and makes control-plane availability and bandwidth dependencies stronger than advertised.

Show authorization calling the control-plane router, receiving a route response on the reverse of that lookup
relationship, then invoking the gateway forwarder locally. Return rows from agent to forwarder to query
ingress/client. Keep the router authoritative without making it a query proxy. Rework `flow-query-stale`
similarly: s4 currently uses `dp-placement`, which is router-to-Postgres, without showing the gateway's
renewed routing RPC. Update both plain and STE explanations with the repaired sequence.

### 2. P1 — State exactly what authorizes admission during outages and restarts

**Design-level; required.** Extend `doc-node-admission`, `doc-placement-router`, `doc-flow-move-database`,
`doc-flow-move-failed`, and inventory DP6/DP7 with a small state/availability table. “Validate owner epoch and
database state at each admission” does not say whether that check is local, an RPC, or a lease check. Only
stale-route refresh explicitly describes unavailable metadata. Routing authority can remain in the control
plane while enforcement uses durable local state, but that choice must be stated.

A conservative first contract would be: new resolutions fail closed when the authority is unavailable;
existing access continues only under explicitly defined locally installed authority; no ownership transfer
occurs without confirmed source fencing. Alternatively stop all admissions on authority loss. Choose and label
one policy, including authorization expiry/revocation behavior. Distinguish an already-running agent from a
restarted agent, which must reconcile ownership and durable fences before opening files.

Require agent lifecycle commands to carry database ID, operation ID, expected epoch/generation and intended
transition. Reject superseded activation, unfence and delete commands; make acknowledgement durable before
replying. For example, a delayed source-cleanup command from A→B must not delete the newly active database
after a later B→A move. Serialize conflicting lifecycle operations per database. Include controller crash
after the placement commit but before activation acknowledgement, and agent restart with orphaned workers. No
split-brain is apparent under the stated happy-path assumptions; this closes the unspecified recovery cases
rather than alleging an already-demonstrated double writer.

### 3. P1 — Define agent command authorization and the trusted compilation boundary

**Design-level; required.** Add the trust contract to `doc-authorization`, `doc-ingress`, `doc-forwarder`,
`doc-node`, and `doc-node-admission`; label it on `c-lifecycle`, `c-forward` and the agent boundary.
“Authenticated RPC” and trusted identity forwarding are useful starts, but authentication alone does not
specify which service may perform which operation.

State that the gateway may execute authorized database operations but cannot provision, unfence, move or
delete; the control-plane lifecycle identity may issue those commands subject to the operation/epoch checks;
agent-to-agent transfer is scoped to the named operation, source, target and database. Explain whether agents
trust a gateway's authorization assertion or verify a scoped capability, and how that authorization expires.

`doc-ingress` carries operation IR alongside compiled SQL/binds, and the existing classifier trusts IR.
Specify that trusted server code derives matching IR and SQL from a validated request. Do not permit a
customer to pair “bounded” IR with arbitrary SQL, or use SQL file-access features to escape the selected
database. State the supported query surface and where it is enforced. This is a design boundary statement, not
a request to implement a security subsystem in DP-01.

### 4. P2 — Preserve bounded scheduling when replacing tenant lanes with database lanes

**Design-level.** Extend `doc-lanes`, `doc-node-admission`, `doc-deadline`, and inventory DP9. The proposal
honestly records shared-lane kill blast radius, but independent SQLite locks alone do not give resource
isolation. One tenant can create many databases and consume every lane, handle, queue slot or byte of disk.

Carry forward the lanes proposal's bounded pool, queue admission, handle-cache and fairness concepts, keyed by
database, with aggregate tenant and VM ceilings. Specify bounded queues, read concurrency, writer admission,
handle eviction, disk/WAL headroom and lifecycle/backup I/O budgets. Tenant quotas need not become a
tenant-wide execution lock. Reserve enough agent capacity to fence or cancel work while execution is
saturated. Say explicitly that logical/file isolation is provided while VM/process failure and resource
contention remain shared. “0..N” is a cardinality model, not unlimited physical capacity.

### 5. P2 — Keep placement open, but give it constraints and a feedback path

**Design-level.** Amend `doc-provisioner`, `doc-metadata`, A3, and the control-plane component view. Leaving
the algorithm OPEN satisfies the addendum; leaving its inputs implicit is less useful. Name eligible agent
health/readiness, compatible software/schema versions, available disk plus WAL/snapshot/move headroom,
resource limits and any locality constraints as inputs. Reserve capacity for concurrent creates and staged
moves, and reject placement when no VM is eligible. Balancing weights and optimization can remain undecided.

Add an agent→control-plane health/size/usage relationship and explain freshness. Agent reporting is repeated
in the documents but has no explicit relationship among the 54 modeled edges. The reader should be able to see
how the control plane obtains the information on which placement depends.

### 6. P2 — Show backup recovery and VM loss as end-to-end flows

**Design-level.** Expand `doc-backups`, `doc-lifecycle-policies`, `planes-control-components` and the dynamic
flow set. Backup consistency, verification and generation-aware restore are already described. Add the missing
completion boundary: agent creates a consistent snapshot, uploads and verifies it, then publishes a durable
manifest/status that makes it eligible for restore. Record database/generation identity, checksum, format
compatibility and completion state. State the intended recovery-point/recovery-time policy as OPEN rather than
implying continuous protection; WAL streaming is explicitly deferred.

For a lost VM, show every database it hosted becoming unavailable, detection versus proof of fencing, and
operator recovery from a verified backup. Retain the existing no-automatic-failover rule: inability to contact
an old writer is not permission to activate another. Define an explicit isolation/proof-of-fence prerequisite
for any future destructive recovery. Also explain how backup deletion can finish when the former hosting agent
is permanently lost. `flow-drop-database-s7/s8` currently delegates erasure of all copies and archives to that
agent without showing a replacement cleanup executor or object-store confirmation. Keeping DROPPING pending is
safe, but its permanent-failure recovery path is still missing.

### 7. P2 — Draw both move participants and make checkpoint ordering unambiguous

**Diagram-level.** `flow-move-database` and `flow-move-failed` collapse source and target onto one `node`
participant. The step text distinguishes A/B, but the rendered graph cannot show who acknowledges the fence,
receives the copy or becomes active. Add an illustrative instance-level companion showing source agent, target
agent and their transfer, using the same logical agent type. Keep control-plane instructions distinct from
snapshot bytes moving between agents; identify the selected transfer mechanism as proposed.

Change s4's “close all handles; checkpoint after settlement” to an explicit sequence: settle requests,
checkpoint with a controlled maintenance connection, close that connection and remaining handles, then
acknowledge quiescence and copy. Match the inventory and both prose variants. Do not imply checkpointing
without any connection, or acknowledge no live handles before maintenance completes.

### 8. P3 — Improve the overview and record the remaining differences from lanes

**Diagram-level.** The committed `planes-data-components.png` Fit capture makes most text too small to read.
Keep the main data-plane view to ingress/auth, authoritative lookup, forwarding and agent admission; link to a
separate agent execution detail for classifier/FFI/deadlines. The committed move capture shows a four-node
relationship graph, not visibly distinct source/target participants; tweak 7 would improve that explanation.
This review inspected those existing captures, not a newly rendered browser session.

Inventory DP1/DP9/DP10 already explain the intentional departures: many files per tenant, no tenant-sticky
lane set, offline snapshots instead of online WAL transfer, and omitted observability. Add an explicit note
that the lanes proposal's lease/heartbeat contract is not currently specified here, alongside the chosen
replacement from tweak 2. Also record whether bounded pool/scheduler/handle policies are retained or deferred
(tweak 4), so omission cannot be mistaken for removing backpressure. Existing/mixed/planned markings for the
FFI, classifier, routing seam and proposed processes are otherwise appropriately conservative; retain them.

## Validation and handoff

Read the briefs, builder report, new bundle and inventory, relevant lanes inventory, current routing code,
repo orientation and shared event inbox. Inspected the committed data-component and move screenshots.
Read-only integrity checks passed for 44 elements, 54 relationships, 59 documents with plain/STE text, 14
diagrams and 89 interactions: referenced endpoints/documents exist, diagram relationships remain in scope, and
element/relationship IDs appear in the inventory. These checks establish structural consistency, not correct
distributed-system semantics. The query-result path finding demonstrates that distinction.

`git diff --exit-code 351a51b --` for both older bundles and their content trees returned 0. Root CI and Pages
acceptance were not rerun for this design-only review; the builder's recorded passes remain builder evidence.
Only this review file is written and committed, as explicitly requested. That restriction also takes
precedence over event publication and separate knowledge edits. No dataset edits, merge or deployment.

Required repair: tweaks 1–3. Tweaks 4–7 are substantive proposal improvements; document explicit deferrals
where the next design decision is intentionally left open. Tweak 8 is presentation and comparison cleanup.
Keep all corresponding JSON documents, STE drafts, scenarios and inventory rows synchronized in the repair.

Learning assessment: the distinction between routing authority and query transport, durable epoch-checked
agent commands, and database isolation versus shared resource budgets is worth preserving. This committed
review is the ownership-safe handoff to the coordinator for a later `docs/knowledge/` entry; no reusable
script was produced. Leave this worktree and branch in place for the authorized repair round.
