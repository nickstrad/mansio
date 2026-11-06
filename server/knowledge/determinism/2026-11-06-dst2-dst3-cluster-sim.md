# 2026-11-06 — DST-2 and DST-3: the cluster simulation, the swarm and shrinking

Author: DST builder. Plan: IR protocol plan §6.4 items 3–4 (`mansio/docs/plans/ir-protocol.md`). Branch
`dst/2-cluster`. Code: `internal/sim/cluster*.go`, `internal/sim/ir_workload.go`, `internal/agentglue`,
`ticket.Rotate`, `tickettest.MemKeys`, `scripts/sim-swarm.sh`. Builds on
[DST-1](2026-11-05-dst1-single-node-sim.md) (the loop, streams, gate, trace, health and RPO rule).

## What one cluster seed runs

```
clients (1–4 per database; own streams, route cache, request ids; SDK retry/reroute rules)
   │ POST /v1/resolve ─────────────► controld: locator.Locator (real) ── locStore ─► registry fake + ownertest.Placement
   │ POST /v1/ops (ticket) ────────► edge.Edge (real: gate, per-agent breaker, bulkhead)
   │                                     │ transport (an http.RoundTripper over the sim network)
   ▼                                     ▼
 simulated network (delay, loss,   agentd × 2–4: front.NewHandler → HandleOps → agentglue.{DepAdmitter, Admitter}
 duplication, spikes, partitions)       → commits → agentglue.{DepExecutor, Executor(Order: door)} → vactor → SQLite
   ▲                                     │ owner.Table (FileStore per VM) ── authority view ─► ownertest.Placement
 controld: control.Controller ─ lifecycle calls (claim/fence/forget) ─► agentglue.Lifecycle
           ticket.Rotate(tickettest.MemKeys) every 10 s; ResumeStuck every 5 s; agents and edge fetch keys
                                         └── durabletest store (S3) on the clock, per-call faults, storms
```

- **Real code**: agentd's request path (front HTTP handler, `/v1/ops`, docexec, actors, vactor, owner table,
  breaker) through `internal/agentglue` (lifted from `cmd/agentd`: one copy, so the sim cannot drift);
  `control.Controller` (create, move, attest-dead, restore, ResumeStuck), `locator.Locator` (API-key auth,
  route cache, single-flight loads, notifications, `Reset`), the issuer's rotation (`ticket.Rotate`, lifted out
  of `cmd/controld/signing.go`; `keyDir` is now a `ticket.KeyStore`), `ticket.Signer`, agents' and the edge's
  ticket verification against fetched key rings, `edge.Edge`.
- **Fakes**: Postgres (the authority is `ownertest.Placement`, a linearizable CAS that refuses epoch
  regressions; the registry is an in-memory `control.Registry` like control's test fake), S3 (`durabletest`),
  `control/agenthttp` and `keyfetch` (their calls are direct functions over the sim network), the TS SDK (a Go
  client with its rules: same request id on every attempt, reroute with `min_epoch` after `wrong_owner` /
  `fenced`, re-resolve after `ticket_invalid`, retry `shed`/`unavailable`/`aborted`/`expired`, at most 12 attempts
  in 10 s).
- **Workload** (one schema for every database: `registers`, `accounts`, `log`): register writes and reads, bank
  transfers (read two accounts, then patch both with `expect: {version}`), full bank reads. Every mutation also
  inserts `log {rid}` with a server-drawn id, so a database file shows which mutations it holds and how often.
  Setup (registers 0, accounts 100) is written and waited on until published before clients start.
- **Nemesis** (each class its own stream and exponential schedule; recorded as an op script): `partition`
  (any node — an agent, the edge, controld, Postgres, S3, the clients — from a subset of the others), `kill`
  (kill -9 of an agent, half the time a database's owner) then `restart` or `attest` (an operator attests it
  dead and `Restore`s its databases; the VM stays down; at least two VMs stay), `move` (`Controller.Move`,
  policy-picked target), `create` (a new database mid-run, its clients start once set up), `cacheloss`
  (`Locator.Reset`), `storm` (S3 503s), `pgdown`. Rotation runs on its schedule. Placement and registry changes
  notify the locator after 1–50 ms, or the notification is lost (`Faults.Notify`).
- **Knobs from the seed**: 2–4 VMs, 1–3 boot databases, 1–4 clients each, tick 1/5 s, `WriteBackEvery`
  2/10/30 s, idle passivation, every fault class on (2/3) or off with a rate. `Swarm` (DST-3) then scales each
  class by a factor in [0, 2], a third of them to 0.

## The network (`cluster_net.go`)

A call is two messages on the clock: the request (0.2–5 ms, or a spike up to `SpikeMax`; lost with `Drop`;
delivered twice with `Duplicate`) and the answer (same; lost with `Drop`). A partition loses every message
between the pair; a request to a down agent is refused (nothing ran); an agent killed while serving never sends
its answer. Delivery does not wait for the caller: a request whose caller gave up still lands and runs. Every
draw comes from the caller's stream; a handler gets its own stream seeded by its call, for the calls it makes
(the edge's forward). A duplicated mutation tells the checker (the client cannot know), so no answer then
proves "nothing ran".

## Determinism

DST-1's loop plus three rules, all found by `TestClusterSameSeedSameTrace` failing:

1. **Doors** (`doors`, `agentglue.Executor.Order`): a request that reaches its database's actor waits until the
   loop lets it through, lowest arrival number first, one per step. Requests woken together (an activation
   finishing) would otherwise race to the actor.
2. **Phases**: periodic loops started together (each agent's maintenance ticker, controld's loop) start after a
   random phase. Timers due at one instant fire in creation order, and goroutines started in one step create
   theirs in the scheduler's order.
3. **Trace streams per goroutine**: lines that two goroutines may write in one step use different streams
   (`op/<n>` per controller operation, `life/<key>/<tag>` per lifecycle call, `create/<key>`), since a step's
   lines are sorted by stream only. Sorted key-id lists.

Evidence: `TestClusterSameSeedSameTrace` (3 seeds by default; `MANSIO_SIM_SEEDS`) green with `-count=3` and
`-race`; 120 seeds (60 plain, 60 swarm) run twice each, all byte-identical.

## The checker (`cluster_model.go`)

| When | Property |
|---|---|
| every committed write (server side, live process) | one VM per (database, epoch); epochs never go down in commit order (**at most one owner serves writes per epoch**) |
| every owner-table transition | a VM's epoch for a key only rises; every authority CAS keeps the fake's invariants (epochs rise; a new owner gets a higher epoch) |
| every step, each manifest | generation and epoch never go down; a new generation extends the last; no mark twice (**nothing applied twice**), none unknown, none of a write every answer said did nothing, none whose final answer was a definite failure (e.g. `ConflictError` for an applied transfer) |
| every answer | `duplicate` only for a write an earlier attempt may have applied |
| an owner starts serving (owner table → Active: claim after create/move/restore, reopen after restart) | the generation it starts from holds every write committed so far, except, after a kill -9 of the owner, writes lost inside the RPO; **a graceful move loses nothing** (any committed write missing is a violation, acknowledged or not) |
| end of the run | the same against the final manifests (as a crash); then per database: registers and the bank **linearizable** (`internal/linearize`), segmented at the kills of the database's owner (the IR-5a Jepsen rules: ops across a crash are dropped (reads) or unknown (writes); a later segment starts unknown, each register's / account's first value must be the setup's or one written by an op called before the crash) |
| every full bank read | balances sum to the setup total |

**RPO** (plan §9's open decision; parameterized, `ClusterConfig.RPO`, default `WriteBackEvery + Tick + 1 s`): a
write lost at a kill is judged by its **commit time** (when the agent answered; the client may hear the ok much
later through a slow network): a violation iff it committed before a write-back or fence that published, or a
fault-free stretch at least RPO long fits between its commit and the crash. Unhealthy time: S3 faults on that
database, storms, stalls, any agent's breaker open, any partition. A write whose commit a restore lost before
its ok arrived is judged when the ok arrives. Losses inside the RPO are counted (`Stats.LostInRPO`).

Teeth (production mutants, by hand, reverted): request-id dedupe off (actor) — caught by 6/6 seeds (applied
twice, final answer `ConflictError` for an applied transfer, non-linearizable registers); `Fence` without its
final write-back — 12/12 (committed writes lost across a move); `Admitter` admitting fenced/moving keys — 5/12
in 5 min (writes acknowledged by the old owner after the fence lost at the destination's claim).

## Harness bugs found on the way (fixed in the sim, not production)

- A reply from a process killed while serving reached the client (a kill -9 drops it).
- "Nothing ran" was inferred across attempts: a later attempt or a late-landing earlier one may apply;
  `maybe` is now sticky per request id, and every attempt reopens `none`.
- Writes were judged by the client's receipt time; a write committed just before a crash whose ok arrived
  after the restart was reported as lost after a publish. Now by commit time.
- The shrinker's script went nil after removing every op (`slices.Concat`), which re-enabled the drawn
  schedule: an explicit `Scripted` flag.
- The random phase before the agents' maintenance loops was up to one tick, delaying the first write-back past
  the RPO (seed 2519): phases are under 1 ms.
- Overlapping partitions or storms shared one health span (seed 2700): spans are numbered and counted.

## Production findings

1. **Fixed: write-backs stopped while a fence was pending** (`owner.Table.Publish`, swarm seed 1963). A
   `Fence` whose final write-back failed (S3 down) leaves the key `Fencing` until the controller retries; the
   periodic write-back refused a non-`Active` key, so acknowledged writes stayed unpublished for the retry
   interval and a kill -9 then lost them outside the RPO. `Publish` now admits a key `Fencing` at its own epoch
   until the fence's own write-back has published (`slot.finalAt`, set under the publish lock, so the fence's
   generation is the last of its epoch). Regression: `owner.TestPublishWhileAFenceIsPending`.
2. **Open: `_version` reuse after a kill -9 rollback** (swarm seed 2761, design decision, not fixed). After a
   crash the database restarts from the last published generation; a document's `_version` counts up again
   from the rolled-back value, so a version number already observed before the crash names a different
   document after it. A transfer read before the crash (or retried across it) passes its `expect: {version}`
   against the other document and the bank stops summing (398 vs 400). Shrunk to three kill/restart pairs of
   one VM: `testdata/open/version-reuse-after-rollback.json` (must keep failing; `MANSIO_SIM_OPEN=1 go test
   ./internal/sim -run TestClusterOpenFindings -v`). Replay: `MANSIO_SIM_SWARM=1 MANSIO_SIM_SEED=2761
   MANSIO_SIM_TRACE=1 go test ./internal/sim -run TestClusterSeeds -v`. One option: a version floor per
   incarnation (e.g. `max(prev+1, seal<<32)`) so versions never repeat across a rollback; it ties to plan §9's
   RPO decision (with RPO 0 there is no rollback). The checker also counts these as `Stats.VersionReuse`.

No seed produced an `Internal` (defect) answer.

## Seeds run (M5 Max, 2026-11-06)

| Run | Seeds | Result |
|---|---|---|
| swarm, before the checker fixes | 1–1000, 300 more | 1 harness bug per few hundred (fixed, below) |
| swarm | 1001–3000 (2000) | 1998 pass; 2761 (open finding 2), 1963 (finding 1, fixed) |
| plain (no swarm), final code | 1–1000 | 1000 pass |
| swarm, final code | 5001–5500 | 500 pass |
| determinism | 60 plain + 60 swarm, twice | byte-identical |

## Swarm and shrink (DST-3)

```sh
go test ./internal/sim                                       # DST-1 (8 + 4 IR seeds), DST-2 (6 seeds), determinism, vectors
MANSIO_SIM_SWARM=1 MANSIO_SIM_SEEDS=500 go test ./internal/sim -run TestClusterSeeds     # swarm (MANSIO_SIM_FIRST=n)
MANSIO_SIM_SWARM=1 MANSIO_SIM_SEED=388 MANSIO_SIM_TRACE=1 go test ./internal/sim -run TestClusterSeeds -v   # replay
MANSIO_SIM_SWARM=1 MANSIO_SIM_SHRINK=388 MANSIO_SIM_SAVE=1 go test ./internal/sim -run TestClusterShrink -v  # shrink, save a vector
scripts/sim-swarm.sh [seeds] [first]    # nightly: make sim-swarm SEEDS=2000 / deno task sim:swarm
```

`ShrinkCluster`: the drawn schedule becomes an explicit script (`Scripted`, `Script`); the duration is cut to
just past the first violation; the script is delta-debugged (halves, then smaller chunks, then single ops);
network and S3 fault classes and notifications are turned off one by one; clients and boot databases cut. Any
failure counts (the shape may change, as in DST-1). The fence mutant's seed 1 shrinks in 11 runs to one move,
one client, 10 s: committed as `testdata/cluster/move-handoff.json`. `TestClusterVectors` replays every
vector (each must pass). The nightly script shrinks every failing seed and writes its vector; fix the bug,
edit the vector's `why`, commit both.

## Gaps (for later)

- **Group commit and `aborted`** are not exercised: the doors let one request reach an actor per step, and a
  statement takes zero simulated time, so batches stay single. A clock-aware stall inside a statement would
  open them (also needed for statement timeouts, DST-1's gap).
- **Not simulated**: HTTP between controld and the agents (`control/agenthttp`'s codes and breaker), `keyfetch`,
  the Postgres adapter and listener (a fake with notifications), replica lag (`Locate(primary=false)` reads the
  primary), `objstore`, drops (`Controller.Drop`), suspend/resume, tenancy credentials, the TS SDK itself (a Go
  client with its rules), controld crashes and more than one controld.
- **Partitions are fail-fast**: a cut S3 or Postgres call fails at once instead of hanging to its deadline;
  network messages to a cut node are lost (the caller waits for its deadline).
- **Health is coarse**: any partition or any agent's open breaker counts as unhealthy for every database's RPO
  (conservative: it excuses more).
- One shared agent clock (no clock skew between nodes; tickets have `Skew` 2 s anyway).
