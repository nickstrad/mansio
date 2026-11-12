# The cluster simulation, the swarm and shrinking

How 2–4 agents, controld, the locator and the edge run from one seed over a simulated network, what the
checker proves across moves, kills and restores, and how swarm runs and the shrinker turn a failure into a
saved vector.

Code: `internal/sim/cluster*.go`, `internal/sim/ir_workload.go`, `internal/agentglue`, `ticket.Rotate`,
`tickettest.MemKeys`, `scripts/sim-swarm.sh`. Builds on the
[single-node simulation](single-node-simulation.md) (the loop, streams, gate, trace, health and RPO rule).

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
  route cache, single-flight loads, notifications, `Reset`), the issuer's rotation (`ticket.Rotate`, lifted
  out of `cmd/controld/signing.go`; `keyDir` is now a `ticket.KeyStore`), `ticket.Signer`, agents' and the
  edge's ticket verification against fetched key rings, `edge.Edge`.
- **Fakes**: Postgres (the authority is `ownertest.Placement`, a linearizable CAS that refuses epoch
  regressions; the registry is an in-memory `control.Registry` like control's test fake), S3 (`durabletest`),
  `control/agenthttp` and `keyfetch` (their calls are direct functions over the sim network), the TS SDK (a Go
  client with its rules: same request id on every attempt, reroute with `min_epoch` after `wrong_owner` /
  `fenced`, re-resolve after `ticket_invalid`, retry `shed`/`unavailable`/`aborted`/`expired`, at most 12
  attempts in 10 s).
- **Workload** (one schema for every database: `registers`, `accounts`, `log`): register writes and reads,
  bank transfers (read two accounts, then patch both with `expect: {version}`; every third transfer guards its
  read with `assert {unchangedSince}` on `accounts` instead, see
  [the version and sequence floor](../writeback/version-and-sequence-floor.md)), full bank reads. Every
  mutation also inserts `log {rid}` with a server-drawn id, so a database file shows which mutations it holds
  and how often. Setup (registers 0, accounts 100) is written and waited on until published before clients
  start.
- **Nemesis** (each class its own stream and exponential schedule; recorded as an op script): `partition` (any
  node — an agent, the edge, controld, Postgres, S3, the clients — from a subset of the others), `kill` (kill
  -9 of an agent, half the time a database's owner) then `restart` or `attest` (an operator attests it dead
  and `Restore`s its databases; the VM stays down; at least two VMs stay), `move` (`Controller.Move`,
  policy-picked target), `create` (a new database mid-run, its clients start once set up), `cacheloss`
  (`Locator.Reset`), `storm` (S3 503s), `pgdown`. Rotation runs on its schedule. Placement and registry
  changes notify the locator after 1–50 ms, or the notification is lost (`Faults.Notify`).
- **Knobs from the seed**: 2–4 VMs, 1–3 boot databases, 1–4 clients each, tick 1/5 s, `WriteBackEvery` 2/10/30
  s, idle passivation, every fault class on (2/3) or off with a rate. Swarm mode then scales each class by a
  factor in [0, 2], a third of them to 0.

## The network (`cluster_net.go`)

A call is two messages on the clock: the request (0.2–5 ms, or a spike up to `SpikeMax`; lost with `Drop`;
delivered twice with `Duplicate`) and the answer (same; lost with `Drop`). A partition loses every message
between the pair; a request to a down agent is refused (nothing ran); an agent killed while serving never
sends its answer. Delivery does not wait for the caller: a request whose caller gave up still lands and runs.
Every draw comes from the caller's stream; a handler gets its own stream seeded by its call, for the calls it
makes (the edge's forward). A duplicated mutation tells the checker (the client cannot know), so no answer
then proves "nothing ran".

## Determinism

The single-node loop plus three rules, all found by `TestClusterSameSeedSameTrace` failing:

1. **Doors** (`doors`, `agentglue.Executor.Order`): a request that reaches its database's actor waits until
   the loop lets it through, lowest arrival number first, one per step. Requests woken together (an activation
   finishing) would otherwise race to the actor.
2. **Phases**: periodic loops started together (each agent's maintenance ticker, controld's loop) start after
   a random phase. Timers due at one instant fire in creation order, and goroutines started in one step create
   theirs in the scheduler's order.
3. **Trace streams per goroutine**: lines that two goroutines may write in one step use different streams
   (`op/<n>` per controller operation, `life/<key>/<tag>` per lifecycle call, `create/<key>`), since a step's
   lines are sorted by stream only. Sorted key-id lists.

Evidence: `TestClusterSameSeedSameTrace` (3 seeds by default; `MANSIO_SIM_SEEDS`) green with `-count=3` and
`-race`; 120 seeds (60 plain, 60 swarm) run twice each, all byte-identical.

## The checker (`cluster_model.go`)

| When                                                                                                  | Property                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| ----------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| every committed write (server side, live process)                                                     | one VM per (database, epoch); epochs never go down in commit order (**at most one owner serves writes per epoch**)                                                                                                                                                                                                                                                                                                                                                                              |
| every owner-table transition                                                                          | a VM's epoch for a key only rises; every authority CAS keeps the fake's invariants (epochs rise; a new owner gets a higher epoch)                                                                                                                                                                                                                                                                                                                                                               |
| every step, each manifest                                                                             | generation and epoch never go down; a new generation extends the last; no mark twice (**nothing applied twice**), none unknown, none of a write every answer said did nothing, none whose final answer was a definite failure (e.g. `ConflictError` for an applied transfer)                                                                                                                                                                                                                    |
| every answer                                                                                          | `duplicate` only for a write an earlier attempt may have applied                                                                                                                                                                                                                                                                                                                                                                                                                                |
| an owner starts serving (owner table → Active: claim after create/move/restore, reopen after restart) | the generation it starts from holds every write committed so far, except, after a kill -9 of the owner, writes lost inside the RPO; **a graceful move loses nothing** (any committed write missing is a violation, acknowledged or not)                                                                                                                                                                                                                                                         |
| end of the run                                                                                        | the same against the final manifests (as a crash); then per database: registers and the bank **linearizable** (`internal/linearize`), segmented at the kills of the database's owner (Jepsen rules: ops across a crash are dropped (reads) or unknown (writes); a later segment starts from exactly the generation the first owner after the crash started from: each register's / account's last write in its marks, or the setup's; carried-over unknown writes it already holds are dropped) |
| every full bank read                                                                                  | balances sum to the setup total                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| every read                                                                                            | an `(account, _version)` seen with two balances is a violation (`version-reuse`); an `(account, seq)` seen with two versions is a violation (`sequence reuse`)                                                                                                                                                                                                                                                                                                                                  |

**RPO** (not decided; parameterized, `ClusterConfig.RPO`, default `WriteBackEvery + Tick + 1 s`): a write lost
at a kill is judged by its **commit time** (when the agent answered; the client may hear the ok much later
through a slow network): a violation iff it committed before a write-back or fence that published, or a
fault-free stretch at least RPO long fits between its commit and the crash. Unhealthy time, scoped to the
write: S3 faults on that database's calls; stalls of any call while its committing VM owned the database (the
tick writes back one database after another); that VM's breaker open; a partition cutting that VM from S3 or
Postgres; storms (global). Partitions of clients, the edge or controld excuse nothing. A write whose commit a
restore lost before its ok arrived is judged when the ok arrives. Losses inside the RPO are counted
(`Stats.LostInRPO`; of those, `Stats.LostExcused` were older than the RPO and excused only by faults: 2990
losses in 115 of 2000 swarm seeds, mostly storms and the database's own S3 faults; every other excused loss is
inside the plain RPO window).

Teeth (production mutants, by hand, reverted): request-id dedupe off (actor) — caught by 6/6 seeds (applied
twice, final answer `ConflictError` for an applied transfer, non-linearizable registers); `Fence` without its
final write-back — 12/12 (committed writes lost across a move); `Admitter` admitting fenced/moving keys — 5/12
in 5 min (writes acknowledged by the old owner after the fence lost at the destination's claim).

## Harness bugs found on the way (fixed in the sim, not production)

- A reply from a process killed while serving reached the client (a kill -9 drops it).
- "Nothing ran" was inferred across attempts: a later attempt or a late-landing earlier one may apply; `maybe`
  is now sticky per request id, and every attempt reopens `none`.
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
   interval and a kill -9 then lost them outside the RPO. `Publish` now admits a key `Fencing` at its own
   epoch until the fence's own write-back has published (`slot.finalAt`, set under the publish lock, so the
   fence's generation is the last of its epoch). Regression: `owner.TestPublishWhileAFenceIsPending`.
2. **Fixed: `_version` reuse after a kill -9 rollback** (swarm seed 2761). After a crash the database restarts
   from the last published generation; a document's `_version` counted up again from the rolled-back value, so
   a version number already observed before the crash named a different document after it. A transfer read
   before the crash (or retried across it) passed its `expect: {version}` against the other document and the
   bank stopped summing (398 vs 400). Shrunk to three kill/restart pairs of one VM. Fixed by
   [the version floor](../writeback/version-and-sequence-floor.md); the vector is
   `testdata/cluster/version-reuse-after-rollback.json` and a reused version is a violation.

No seed produced an `Internal` (defect) answer.

## Seeds run (Apple M5 Max, 2026-11-06)

| Run                                                                          | Seeds                                                                  | Result                                        |
| ---------------------------------------------------------------------------- | ---------------------------------------------------------------------- | --------------------------------------------- |
| swarm, before the checker fixes                                              | 1–1000, 300 more                                                       | 1 harness bug per few hundred (fixed, above)  |
| swarm                                                                        | 1001–3000 (2000)                                                       | 1998 pass; 2761 (finding 2), 1963 (finding 1) |
| plain (no swarm), final code                                                 | 1–1000                                                                 | 1000 pass                                     |
| swarm, final code                                                            | 5001–5500                                                              | 500 pass                                      |
| determinism                                                                  | 60 plain + 60 swarm, twice                                             | byte-identical                                |
| swarm, after review fixes (scoped RPO health, exact post-crash start states) | 1001–3000                                                              | 1999 pass; 2761 (finding 2, then not fixed)   |
| determinism, after review fixes                                              | 30 plain + 30 swarm twice; defaults `-count=3`, swarm `-race -count=3` | byte-identical                                |

## Swarm and shrink

```sh
go test ./internal/sim                                       # single-node (8 + 4 IR seeds), cluster (6 seeds), determinism, vectors
MANSIO_SIM_SWARM=1 MANSIO_SIM_SEEDS=500 go test ./internal/sim -run TestClusterSeeds     # swarm (MANSIO_SIM_FIRST=n)
MANSIO_SIM_SWARM=1 MANSIO_SIM_SEED=388 MANSIO_SIM_TRACE=1 go test ./internal/sim -run TestClusterSeeds -v   # replay
MANSIO_SIM_SWARM=1 MANSIO_SIM_SHRINK=388 MANSIO_SIM_SAVE=1 go test ./internal/sim -run TestClusterShrink -v  # shrink, save to testdata/open
scripts/sim-swarm.sh [seeds] [first]    # nightly: make sim-swarm SEEDS=2000 / deno task sim:swarm
```

`ShrinkCluster`: the drawn schedule becomes an explicit script (`Scripted`, `Script`); the duration is cut to
just past the first violation; the script is delta-debugged (halves, then smaller chunks, then single ops);
network and S3 fault classes and notifications are turned off one by one; clients and boot databases cut. A
change is kept only while the run fails in the same `FailureClass` (split brain, conservation, not
linearizable, lost beyond the RPO, ...), so the shrinker does not drift to another bug. The fence mutant's
seed 1 shrinks in 11 runs to one move, one client, 10 s: committed as `testdata/cluster/move-handoff.json`.
`TestClusterVectors` replays every vector (each must pass). `MANSIO_SIM_SAVE=<dir>` writes the shrunk vector
to `<dir>` (`1`: `testdata/open`, replayed only by `MANSIO_SIM_OPEN=1 ... TestClusterOpenFindings`, which also
checks each open vector's `expect` class); the nightly script writes them to its `results/sim-swarm/<time>/`
directory. Moving a vector to `testdata/cluster` is a manual step taken with the fix.

## Gaps

- **Group commit and `aborted`** are not exercised: the doors let one request reach an actor per step, and a
  statement takes zero simulated time, so batches stay single. A clock-aware stall inside a statement would
  open them (also needed for statement timeouts, a gap of the single-node simulation too).
- **Not simulated**: HTTP between controld and the agents (`control/agenthttp`'s codes and breaker),
  `keyfetch`, the Postgres adapter and listener (a fake with notifications), replica lag
  (`Locate(primary=false)` reads the primary), `objstore`, drops (`Controller.Drop`), suspend/resume, tenancy
  credentials, the TS SDK itself (a Go client with its rules), controld crashes and more than one controld.
- **Partitions are fail-fast**: a cut S3 or Postgres call fails at once instead of hanging to its deadline;
  network messages to a cut node are lost (the caller waits for its deadline).
- **Health is approximate**: a stall counts against the database's authority owner at the time, not the VM
  that made the call; storms excuse every database.
- One shared agent clock (no clock skew between nodes; tickets have `Skew` 2 s anyway).
- No CI job runs the nightly swarm.
