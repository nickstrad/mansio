# determinism/ — Clock, Rand and IDGen seams for deterministic simulation (DST)

Owner: DST builder (IR protocol plan §6.4, `ts-db/docs/plans/ir-protocol.md`). Newest first.

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

- `durabletest.Store` (`Faults.Delay`, `Hang`) and `vactortest.Fetcher` (`Delay`) wait in real time
  (`time.After`) and measure with `time.Now`; the sim's S3 fake must take a `det.Clock` before stalls can be
  simulated. `s3test`/`faultnet`/`objstore` are real-network adapters (real time by nature).
- Goroutine scheduling: the system is still concurrent (actor goroutines, vactor activations, `parallel`).
  DST-1 needs either a step-driven harness around the pure parts or one-at-a-time scheduling; the seams alone
  do not order goroutines.
- SQLite `busy_timeout` and modernc's internals use wall time; the actor's statement interrupt is on the
  clock only through the ctx it passes.
- `errs.Error.Time` (log timestamp), `logx.NewID` (log ids), `fsx` temp names: not state.
- `cmd/` wiring: controld's ticket-key generation (`cmd/controld/signing.go`, `crypto/rand`) and rotation
  timer, agentd's startup reconcile loop (`cmd/agentd/startup.go`, global PCG seed) and probes. DST-2 must lift
  the controld issuer loop into a package (rotation itself, `keys.rotate(now)`, already takes `now`).
- Adapters kept on real time: `placement/pg` (has its own `Config.Now`), `placement/file` (poll), `objstore`
  retry jitter, `control/agenthttp`. DST-2 uses a placement fake, not these.

**Evidence.** Deterministic tests that drive a transition by the simulated clock only:
`owner.TestReconcileUntilDoneOnSimulatedClock` (exact backoff waits, identical across two runs with seed 42),
`locator.TestLoadTimeoutBoundsALoadNobodyWaitsFor` (load deadline fires at exactly 20 ms simulated),
`vactor.TestRunMaintenanceStopsAndReports` (no tick until one interval of simulated time, then exactly one),
`dettest` tests (order, ticker, Stop/Reset, deadline contexts and their children). Tests whose stores or
networks take real time use a shifted system clock (`vactor` `newShifted`, `newDurableRealTime`).
