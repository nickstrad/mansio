# determinism/ — Clock, Rand and IDGen seams for deterministic simulation (DST)

Owner: DST builder (IR protocol plan §6.4, `mansio/docs/plans/completed/intermediate-representation-protocol.md`). Newest first.

## 2026-11-06 DST-2 and DST-3: cluster simulation, swarm, shrinking (branch `dst/2-cluster`)

[Full note](2026-11-06-dst2-dst3-cluster-sim.md). `sim.RunCluster` runs 2–4 agents (agentd's request path via
the new `internal/agentglue`), the real controller, locator, edge and ticket rotation (`ticket.Rotate`), a
linearizable placement fake, S3 fake, and a simulated network (delay, loss, duplication, spikes, partitions)
from one seed; nemesis ops: partition, kill -9, restart, attest-dead + restore, move, create, cache loss, S3
storms, Postgres down. Checker: one owner per epoch, epochs rise, nothing applied twice, no write lost across a
move, kill -9 losses only inside a parameterized RPO, per-database linearizability with the IR-5a crash
segments, bank conservation. Determinism needs doors (one request per actor per step), sub-ms phases and
per-goroutine trace streams. DST-3: `MANSIO_SIM_SWARM=1` varies fault rates per seed, `ShrinkCluster` cuts
a failure to a minimal script, vectors in `testdata/cluster`, nightly `scripts/sim-swarm.sh`. Found: periodic
write-backs stopped while a fence was pending (RPO; fixed in `owner.Table.Publish`); `_version`
reuse after a kill -9 rollback broke `expect` (fixed 2026-11-10:
[the version floor](../writeback/2026-11-10-version-floor.md)).

## 2026-11-05 DST-1: single-node simulation (branch `dst/1-single-node`)

[Full note](2026-11-05-dst1-single-node-sim.md). `internal/sim` runs one agent (front → owner → vactor → real
SQLite actors → `durabletest` store behind a breaker) from one seed inside a `testing/synctest` bubble:
`synctest.Wait` for quiescence, then exactly one clock timer (`dettest.Clock.Step`, new) or one sim event
(crash, restart, storm) per step, so the same seed gives a byte-identical trace (`TestSameSeedSameTrace`,
SHA-256 of the trace, green under `-race`, `-count=3`). Faults: per-call S3 stalls on the clock, failed calls,
lost replies, 503 storms, lost client replies, statement failures, crashes (a gate cuts the dead process
off; restart from the owner directory, whose writes are fsynced before `Put` returns, plus the S3 manifest).
Checker: nothing applied twice, nothing applied that "did nothing", generations extend each other, writes
acknowledged before a published write-back survive a crash, no loss across a fault-free RPO window
(parameterized, default `WriteBackEvery + Tick + 1 s`). Found: a **false supersede after a lost publish
reply** in `vactor.publish` (a 412 with a failed re-read came back as ErrStale; fixed) and two map-order plans (fixed). The DST-0 gaps closed: `durabletest`
(`UseClock`, `SetCallFaults`), `vactortest.Fetcher.Clock`, `ownertest.Placement.UseClock`. Replay:
`MANSIO_SIM_SEED=<s> MANSIO_SIM_TRACE=1 go test ./internal/sim -run TestSeeds -v`; more seeds:
`MANSIO_SIM_SEEDS=500`. Review follow-ups (same day): every pending publish kept (late landings), ctx-aware
`e.wb`, operator passivations and late-landing publishes in the sim, per-database RPO health; finding
(fixed 2026-11-06, [reopen re-stamp](../fencing/2026-11-06-reopen-restamp.md)): a crashed process's in-flight
publish landing after the restart made the new process go stale; zombies are now on by default
(`sim.Config.NoZombies` turns them off).

## 2026-11-05 DST-0: determinism hygiene (branch `dst/0-hygiene`)

**What exists.** `internal/det` is the one seam package:

| Port | Real | Simulated / seeded |
|---|---|---|
| `det.Clock`: `Now`, `NewTimer`, `NewTicker`, `AfterFunc`, `WithDeadlineCause` | `det.System()` | `dettest.NewClock(start)` |
| `det.Rand` (= `math/rand/v2.Source`, safe for concurrent use) | `det.SystemRand()` | `det.NewRand(seed)` (PCG under a mutex) |
| `det.IDGen`: `NewID()` | `det.SystemIDs("c")` (`c` + 12 random hex + `-` + base-36 counter) | `det.NewIDs("c", rand)` (same format) |

Helpers: `det.Or` / `det.RandOr` (nil → real), `det.Since`, `det.Until`, `det.Sleep(ctx, clk, d)`,
`det.WithTimeout(ctx, clk, d)`, `det.WithDeadline(ctx, clk, t)`.

**Deadlines are on the clock too.** A context deadline is compared with `Now()` all over the request path
(client wait-past-deadline check, actor `StmtTimeout`, front stmt budget), so a simulated `Now` with wall-clock
contexts would be incoherent. `context.WithTimeout`/`WithDeadline` are therefore banned in simulated packages
along with `time.Now`. `dettest.Clock`'s contexts report the simulated deadline, end with `DeadlineExceeded`
and the given cause when `Advance` reaches it, and contexts derived from them with the `context` package get
the same error and cause (they implement `AfterFunc`, so the context package propagates through it
synchronously). Cancellation from a plain `context` parent reaches them asynchronously.

**`dettest.Clock` semantics** (what DST-1 can rely on): time moves only in `Advance(d)`; due timers fire one at
a time in (due time, creation order), `Now()` reads each timer's due time while it fires; `AfterFunc` and
deadline callbacks run on the `Advance` goroutine; channels are buffered 1 and never block (dropped ticks, like
`time.Ticker`); Go 1.23 Stop/Reset semantics (no stale value). `Next()` is the next due time (an event loop
jumps there), `Pending()` counts armed timers, `AwaitPending(n)` waits in real time until a goroutine has
reached its wait.

**Where it is threaded** (every field defaults to the real implementation, so `cmd/` is unchanged):

- `actor.Config.Clock` (queue expiry, `StmtTimeout`, idle passivation timer, request-id stamps, timings);
  `(*actor.Actor).Clock()` so waiters (front `ActorExecutor`) time on the same clock.
- `vactor.Options.Clock` + `Options.Rand` (replaces `Options.Now`): activation deadlines, failure backoff
  jitter, touch/idle ages, manifest stamps, maintenance ticker, write-back and shutdown deadlines.
- `owner.ReconcileOptions.Clock` + `.Rand`: per-key deadlines, round timing, backoff wait.
- `client.Config.Clock`, `.Rand`, `.IDs`: call deadline, retry/reroute waits and jitter, route TTL and refresh
  jitter, write request ids.
- `control.Controller.Clock` (Restore/ResumeStuck per-key deadlines); `locator.Config.Clock` (replaces
  `Config.Now`: caches, ticket expiry, load deadlines).
- `edge.Config.Clock` (replaces `Config.Now`), `edge.RegistryConfig.Clock`; `keyfetch.Config.Clock`.
- `front.NewServer(..., clock det.Clock)` and `front.NewReadiness(..., clock det.Clock, ...)` (replace the
  `front.Clock func() time.Time` type). `fronttest.Clock` is now a `det.Clock` with a hand-set `Now` over real
  timers (its old behaviour).
- Already pure and unchanged: `breaker` (takes `now`), `ratelimit` (takes `now`), `retry` (takes `*rand.Rand`),
  `ticket` (takes `now`), owner `Table`, `durable.PlanGC`/`CheckPublish`, `control.Policy.Pick`.
  `durable.WithBreaker(..., now)` keeps its `func() time.Time` (pass `clk.Now`).

**The lint.** `internal/det/lint_test.go` (`TestSimulatedPackagesUseSeams`, runs in `go test ./...`) parses
the non-test files of: actor, breaker, client, control, control/locator, durable, edge, front, keyfetch,
owner, ratelimit, retry, ticket, vactor (each with its reason in the file). Banned: `time.Now/Since/Until/
After/AfterFunc/NewTimer/NewTicker/Tick/Sleep`, `context.WithTimeout/WithDeadline(+Cause)`, any math/rand
package-level function (explicit sources `rand.New(rand.NewPCG(..))`, `NewZipf` are fine), any `crypto/rand`,
any `*/uuid` package. Allowlist (stale entries fail): `front/http.go: time.Now` — `net.Conn` deadlines are
wall-clock instants. A self-test feeds it a file that breaks every rule.

**How DST-1 should use it.** One seed → `r := det.NewRand(seed)`; one `clk := dettest.NewClock(t0)`; pass
`clk`/`r` into `actor.Config`, `vactor.Options`, `owner.ReconcileOptions`, `client.Config` (+ `det.NewIDs`),
etc. The event loop: run every runnable goroutine to quiescence (or step them explicitly), then
`next, ok := clk.Next(); clk.Advance(next.Sub(clk.Now()))`. Seeded ids make request ids replayable.

**Still non-deterministic (left on purpose; DST-1/2 must handle):**

- ~~`durabletest.Store` (`Faults.Delay`, `Hang`) and `vactortest.Fetcher` (`Delay`) wait in real time~~ (DST-1:
  on a `det.Clock` now). `s3test`/`faultnet`/`objstore` are real-network adapters (real time by nature).
- Goroutine scheduling: the system is still concurrent (actor goroutines, vactor activations, `parallel`).
  DST-1 needs either a step-driven harness around the pure parts or one-at-a-time scheduling; the seams alone
  do not order goroutines.
- SQLite `busy_timeout` and modernc's internals use wall time; the actor's statement interrupt is on the
  clock only through the ctx it passes.
- `errs.Error.Time` (log timestamp), `logx.NewID` (log ids), `fsx` temp names: not state.
- `cmd/` wiring: controld's ticket-key generation (`cmd/controld/signing.go`, `crypto/rand`) and rotation
  timer, agentd's startup reconcile loop (`cmd/agentd/startup.go`, global PCG seed) and probes. DST-2 must lift
  the controld issuer loop into a package (rotation itself, `keys.rotate(now)`, already takes `now`).
- Not linted and on real time: `durabletest` (the fake store DST-1 will use: `Faults.Delay`/`Hang`,
  `time.Now` stamps) and `placement/file` (poll loop). Lint them, or give them a `det.Clock`, before DST-1 relies
  on them.
- Map iteration order is randomized by the runtime and not covered by the lint: any decision that ranges over a
  map (plans, sweeps, reports) must sort keys first to replay from a seed.
- `dettest.Clock` fixes (2026-11-05 review): Advance never moves time back (re-entrant or concurrent calls),
  tickers coalesce big jumps, contexts derived from an ended clock context propagate asynchronously (no
  deadlock in the context package), an ended parent wins over a past deadline; the lint also rejects dot/blank
  imports of covered packages and versioned uuid paths.
- Adapters kept on real time: `placement/pg` (has its own `Config.Now`), `placement/file` (poll), `objstore`
  retry jitter, `control/agenthttp`. DST-2 uses a placement fake, not these.

**Evidence.** Deterministic tests that drive a transition by the simulated clock only:
`owner.TestReconcileUntilDoneOnSimulatedClock` (exact backoff waits, identical across two runs with seed 42),
`locator.TestLoadTimeoutBoundsALoadNobodyWaitsFor` (load deadline fires at exactly 20 ms simulated),
`vactor.TestRunMaintenanceStopsAndReports` (no tick until one interval of simulated time, then exactly one),
`dettest` tests (order, ticker, Stop/Reset, deadline contexts and their children). Tests whose stores or
networks take real time use a shifted system clock (`vactor` `newShifted`, `newDurableRealTime`).
