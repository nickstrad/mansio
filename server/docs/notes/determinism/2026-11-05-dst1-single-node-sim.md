# 2026-11-05 — DST-1: the single-node deterministic simulation (`internal/sim`)

Author: DST builder. Plan: IR protocol plan §6.4 item 2 (`mansio/docs/plans/completed/intermediate-representation-protocol.md`). Branch
`dst/1-single-node`. Code: `internal/sim` (harness, checker, entry point), plus the clock-aware fakes in
`durabletest`, `vactortest`, `ownertest` and `dettest.Clock.Step`.

## What one seed runs

One agent process wired like `cmd/agentd` durable mode, in one process, from one seed:

```
clients (one per db, sequential, retry with the same request id)
   │ SQL workload: INSERT with request id (the mark) · INSERT that violates CHECK · SELECT all marks
   ▼
front.Server.Handle ─ depAdmitter{ownerAdmitter{owner.Table}} ─ depExecutor{hostExecutor{vactor.Host}}
   │                                │ FileStore (owner dir, survives crashes)    │ actordb → real SQLite actors
   │                                │ ownertest.Placement (the authority)        │ RunMaintenance on the ticker
   ▼                                ▼                                            ▼
                     durable.WithBreaker(gate → durabletest.Store on the clock, per-call faults)
```

- **Time**: one `dettest.Clock` (start = the synctest bubble's epoch, 2000-01-01 UTC). Every component takes
  it (actor, vactor, owner reconcile, front, breaker, durabletest, placement).
- **Randomness**: one seed, split into named streams (`config`, `storms`, `crashes`, `client/<db>`,
  `ids/<db>`, `faults/<db>`, `vactor-<n>`, `reconcile-<n>`) by splitmix(seed ⊕ fnv(name)). A stream never
  depends on another's draws, so per-database work can interleave in any order without moving any draw, and
  turning one fault class off leaves the others' schedules alone.
- **Knobs from the seed** (a small swarm): 1–3 databases; tick 1/5/15 s; `WriteBackEvery` 5 s/30 s/2 min,
  optional `WriteBackCommits`, `IdleAfter`, `MaxActive` 1–2; gen cache on/off; 0/1 read connections; each
  fault class on or off with a rate. The first trace line prints them all.
- **Faults**: per S3 call (from the db's own stream): stall (50 ms + up to 0.5/5/40 s, on the clock, so
  activation, statement and write-back deadlines fire inside it), failed call (FailRead/Upload/Fetch/Publish/
  Delete; HEAD → Down), applied-but-reply-lost (LoseUploadReply, LosePublishReply, ApplyThenStale); 503 storms
  (every call fails for 1 s–1 min, mean gap 1 or 5 min); the breaker opens and closes on its own; lost client
  replies (the agent answered, the client never saw it, so it retries the same id); statement failures
  (CHECK violations → `bad_request`, then the client reuses that request id for a valid write half the time);
  crashes (mean gap 1 or 4 min, restart after 0–1/20 s); idle and budget passivations; long think times
  (clock jumps up to minutes).
- **Workload port** (`sim.Workload`: `Seed`, `Send`, `Observe`, `Name`): the built-in `sim.SQL` sends raw SQL
  through `front.Server.Handle`. The request id is the mark the write leaves in the database, so the checker
  can read any database file (a published generation) and tell which writes it holds.

## Determinism: how steps are ordered

Everything runs inside a `testing/synctest` bubble (Go 1.26). The loop:

1. `synctest.Wait()` — every goroutine of the bubble (actors, activations, the maintenance loop, clients,
   database/sql's) runs until it is durably blocked; then the checker looks at the store and the step's trace
   lines are flushed.
2. Pick the earliest of: the clock's next timer, the sim's next event (crash, restart, storm). Timers win ties.
3. Keep the bubble's own clock level with the simulated one (`time.Sleep` to the target inside the bubble),
   so a stray wall-clock wait (SQLite's busy handler, database/sql timers) fires at the right instant too.
4. Fire exactly one timer (`dettest.Clock.Step`, new) or run the one event. Go to 1.

So only one thing becomes runnable per step, and the order of effects is the order of timers, not of the Go
scheduler. Where one event does wake several goroutines (a crash cancels every request), the per-database
streams keep their draws fixed, and the trace sorts each step's lines by stream before appending them.

**Evidence**: `TestSameSeedSameTrace` runs seeds 1–4 (1–2 under `-race`) twice and compares SHA-256 of the
full trace (and the first differing line on failure); green under `-race` and `-count=3`. A trace is typically
300–9,000 lines.

**Runs (M5 Max, 2026-11-05)**: `go test ./internal/sim` ~5 s (8 seeds + determinism + checker tests; 6 s
under `-race` with 3 seeds). 2,000 seeds (`MANSIO_SIM_SEEDS=2000`) in 250 s wall, all clean on the fixed
code, no `internal` answers; per seed 60–9,000 requests, up to 18 crashes, up to 260 injected S3 faults.

Two production changes this needed (commit `server: plan ticks and reconcile rounds in key order`):
`vactor.Tick` planned idle passivations in map order, and `owner.ReconcileAll` reconciled in `sync.Map`
order; both now sort by key. vactor `Workers` and reconcile `Workers` are 1 in the sim (parallel write-backs of
different keys would interleave their store calls).

## Crashes: fidelity

A crash happens at a quiescent point (between steps). It cuts the incarnation's **gate** first (`gate.go`):
from then on every store, placement and owner-directory call of the dead process fails with no effect; then
its contexts are cancelled and its host is torn down in the background (that teardown reaches nothing
durable). A restart builds a new incarnation on a **fresh cache directory** and the **same owner directory**,
loads it (`owner.Open`: ACTIVE → Closed) and reconciles in the background, exactly as agentd's startup does.

- **SQLite files**: not recovery state in durable mode. `vactor.NewHost` empties its cache directory and an
  agent always starts from the S3 manifest, so the crash drops the local files entirely. Real SQLite still
  runs every request, write-back snapshot (backup API) and passivation close. No VFS shim is needed today; one
  is needed (modernc supports registering a VFS) the day a restart reuses its local file (WAL shipping).
- **Owner directory**: `owner.FileStore` writes with `fsx.WriteAtomic` (temp file, fsync, rename, fsync dir)
  before `Put` returns, so at a quiescent crash point the directory **is** its last-fsync snapshot. A third of
  crashes also leave a torn temp file (`.tmp-torn-<n>`), as a crash inside a `Put` would; `Load` drops it.
- **Not modeled**: a crash *inside* a step (between an fsync and the next write of one operation), torn
  SQLite pages, and a PUT that lands after its client gave up (durabletest is linearizable: a call cut off by
  its context never applies).

## The checker (`model.go`)

Per database it keeps the client-side history (every write's status: pending, none = every answer said
nothing ran, maybe, acked, lost) in commit order, which is send order because each database has one
sequential client and SQLite does each write in zero simulated time. Checked:

| When | Property |
|---|---|
| every answer | a second `ok` without `duplicate` for one id = applied twice; `duplicate` for an id never applied; a rejected write acknowledged; a valid write refused as `bad_request` |
| every live read | no mark twice; no unknown mark; none of a write every answer said did nothing; commit order; **every acknowledged write present** |
| every step, each manifest | generation and epoch never go down; a new generation **extends the previous one** (prefix) and is a prefix of the history (no holes before its last mark) |
| every crash and at the end | the restored generation (what a restart starts from): same as a generation, plus **every write acknowledged before the start of a write-back or passivation that published is in it**, plus the RPO rule |

**RPO** (open decision, parameterized: `Config.RPO`). docs/notes/writeback states the crash loss bound as
`Policy.WriteBackEvery` + one tick + one write-back; the default is `WriteBackEvery + Tick + 1 s`. A write lost
at a crash is a violation iff the span from its acknowledgement to the crash contains a **fault-free stretch
at least RPO long**. Unhealthy time = any injected S3 fault (a point, or a stall's whole span), a storm, the
breaker not closed. What the agent should promise while S3 is down (refuse writes, or ack with a growing RPO)
is the open control-plane decision; the checker does not decide it. Lost writes inside the RPO are counted
(`Stats.LostInRPO`) and traced.

The checker's teeth: `model_test.go` (one failing history per property, on/off points of the RPO),
`checker_test.go` (end to end through the real agent: a workload that applies every mark twice, one that
loses the newest write). By hand, 2026-11-05: production mutants "request-id dedupe off" (caught by 10 of 30
seeds: applied twice) and "write-back marks one commit too many as published" (caught by 4 properties within
30 seeds) — reverted.

## Bugs it found (2026-11-05)

1. **False supersede after a lost publish reply** (vactor `publish`; fixed, regression tests
   `TestOwnPendingPublishIsNotAnotherWriter`, `TestOwnResentPublishIsNotAnotherWriter`). Two shapes, both on
   a single node with no other writer:
   - a publish applies, its reply is lost and the re-read fails, so it is pending; the next publish
     (conditional on the old version) gets 412 *because of it*, its re-read fails too, and the host returned
     that 412 as `durable.ErrStale` and overwrote the pending manifest with the new one (seeds 192, 373 under
     the first seed→knob mapping);
   - a publish applies on a re-send and answers 412 (`ApplyThenStale`) while its re-read fails: returned as
     `ErrStale` though nothing else was pending (seeds 668, 1586, 1919).

   The owner table's `Publish` turns `ErrStale` into a durable **superseded fence**: every later request
   answered `fenced`, and the acknowledged writes since the last publish were never written back (lost at the
   next crash, far outside the RPO). Fix: a 412 whose re-read failed is never `ErrStale` (guide R4: stale only
   when a re-read shows another writer; safety does not need the eager answer, every later publish is still
   conditional); `e.pending` keeps every publish whose outcome was never seen (all are conditional on one
   version, so at most one applied) plus the newest one the re-read saw unapplied, and adopts whichever a
   re-read shows; with 8 unseen it re-reads before sending more; a publish the breaker refused is not pending.
   On the committed code, 9 of seeds 501–2000 failed this way; `FuzzDurableHostModel` could not reach it (one
   fault switch per step, so never a lost reply *and* a failed re-read in one write-back).
2. **Map-order plans** (above): not a correctness bug, a replay bug.
3. `FuzzDurableHostModel` updates found while fixing 1: a failed stamp at epoch E left the model's epoch at
   E−1, so its "other owner" bumped to E with a manifest identical to our pending stamp (impossible with real
   fencing: one owner per epoch); a fenced write-back with the re-read failing now expects an unknown outcome,
   not `ErrStale`; the model counts unseen pending stamps for the re-read cap. Fuzzed 7 min clean after each.

## 2026-11-05 review follow-ups

Evidence after them: 1,000 seeds clean (151 s), `TestSameSeedSameTrace -count=3` with and without `-race`,
`FuzzDurableHostModel` 150 s clean.

- **Seen pending publishes are kept too** (vactor; `TestLatePendingPublishIsStillOurs`): the first fix kept
  only the newest pending publish the re-read had seen unapplied, so an older one landing late was taken
  for another writer. Now every pending publish is kept (at most one of them can ever apply), capped at 8
  in all, after which publish re-reads first; a 412 whose re-read shows our own version is an unknown outcome
  too, so a store's 412 alone never comes back as `ErrStale`.
- **Late-landing publishes in the sim**: `durabletest.Faults.LandLate` (+ `LandTag`, `Store.SetLandIf`) (no reply now, applied up to 10 s
  later on the clock, condition checked when it lands); drawn under `LoseReply`.
- **Waiters honour ctx** (vactor): `e.wb` is a one-slot channel lock; `Passivate`, `WriteBack` and `Stamp`
  give up at their deadline (a passivation undoes its mark, the key stays warm). With that the sim issues
  **operator passivations** too (`Mix.Passivate`).
- **RPO health per database**: a failed call or lost/late reply on one database no longer excuses another's
  loss; storms, the breaker and stalls (the tick writes databases back in turn) stay global.
- **Restore judged at each key's reopen stamp** (since 2026-11-06; before: at the restart), not the crash: a
  late publish of the dead process that lands before the restarted process's stamp *applied* (any reply; the
  sim spots it as a manifest it sent, unique by `Seal`) is what the restart starts from; none lands after.
  Durable marks (no-op passivations) of a key still closed are ignored.
- **Finding, fixed 2026-11-06 — zombie publishes after a restart** (was `Config.Zombies`; 2
  of 500 seeds, e.g. 180, 232): a publish the crashed process sent can still land *after* the restarted
  process read the manifest. The restart reopens at the **same epoch** and does not re-stamp the manifest,
  so the old PUT's condition still holds; the new process's next publish gets 412, its re-read shows a
  manifest that is none of its own, so it goes stale (`passivate -> stale`; a tick write-back makes the
  owner fence the key superseded) and its newer acknowledged writes cannot be published. Fix belongs in
  owner/agentd startup: re-stamp the manifest on reopen (a new version fails every earlier in-flight
  publish) or reopen at a new epoch. Fixed by the re-stamp
  ([../fencing/2026-11-06-reopen-restamp.md](../fencing/2026-11-06-reopen-restamp.md)); zombies are on by
  default now (`Config.NoZombies` turns them off).

## How to run and replay

```sh
go test ./internal/sim                                   # 8 seeds (3 under -race) + determinism + checker tests
MANSIO_SIM_SEEDS=500 go test ./internal/sim -run TestSeeds   # more seeds (MANSIO_SIM_FIRST=n to start elsewhere)
MANSIO_SIM_SEED=192 MANSIO_SIM_TRACE=1 go test ./internal/sim -run TestSeeds -v   # replay one, print its trace
MANSIO_SIM_SHRINK=192 go test ./internal/sim -run TestShrink -v                   # cut duration, faults, dbs
MANSIO_SIM_KEEPGOING=1 ...                                                         # run past the first violation
```

A failure prints the seed's knobs, the violations, the last 40 trace lines and the replay command. A trace
line is `<sim seconds> <stream> <event>`; streams: `c/<db>` client requests and answers, `s3/<db>` store
calls that reached the store and their outcome class, `fault/<db>` injected faults, `tick` maintenance,
`owner/<db>` ownership transitions, `breaker`, `reconcile`, `sim` crashes/restarts/storms, `check` generations
seen and violations.

## How IR plugs in (IR-2b)

Implement `sim.Workload` for document operations: `Seed` builds the app's initial database (the docschema
DDL), `Send` posts an IR request (with its request id) to the `/v1/ops` entry point, `Observe` lists the marks
a document table holds in insertion order (e.g. the request id stored in each inserted document). Add that
entry point to the `sim.Agent` interface and set it in `newAgent` next to `Front` (marked `IR hook` in
`workload.go`). The checker needs nothing else; IR-specific invariants (`_version` +1 per patch, `expect`
preconditions) belong in the workload's own `Observe`/answer checks or in the `FuzzDocExecModel`.

## Known gaps (for DST-2 and later)

- **Glue copy (must fix before DST-2)**: `sim/glue.go` mirrors `cmd/agentd/glue.go` and `unavailable.go`
  (unchanged through IR-2b, main 12047d4). Lift both into one shared package before DST-2 builds on it, so
  the sim cannot drift from what agentd runs.
- **Multi-writer branches of the publish fix** (another owner's manifest, stale epochs, adopting a pending
  stamp) are covered only by `FuzzDurableHostModel` and unit tests until DST-2 adds a second agent.
- **Not in the sim**: the HTTP transport, `objstore` and minio-go (real network, retry jitter on global rand;
  `s3test`'s fault modes are mirrored by durabletest switches instead), `placement/file` and `/pg`, the
  edge, controld, locator, client library, tickets (stubbed), Postgres.
- **One sequential client per database**: group-commit batches, `aborted`, and two requests waiting on one
  activation are not exercised; one event waking two goroutines of one database would make the order
  scheduler-dependent.
- **Statement timeouts** never fire: SQLite takes zero simulated time. A clock-aware stall inside a statement
  (a registered SQL function) would cover `stmt_timeout` and `expired, ambiguous`.
- **No epoch changes**: one owner at epoch 1 (moves, bumps, fences are DST-2).
- **Per-call faults only on `durabletest`**: no torn uploads that HEAD reports whole; late landing for
  publishes only (not generation uploads); zombies after a restart land by default.
- Wall-clock-only code in the bubble (database/sql, SQLite busy handler) is deterministic only because the
  bubble's clock is; outside synctest the harness is not deterministic.
