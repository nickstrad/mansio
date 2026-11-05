# Integration points and stability guide (ts-db-go)

Status: proposed · 2026-11-05 · From _Release It!_ (Nygard, 1st ed.) · Plan and evidence: [plan.md](plan.md)

Rules every agent follows when code crosses a process boundary (S3, Postgres, HTTP in or out, the client) or
queues work (actor mailboxes, read pool, admission, fetches). Each rule: **rule** → why → example. Items that
bring today's code into line are `RI-NN` in the plan. Testing basics live in `knowledge/testing/README.md`;
this guide adds failure-below-the-interface tests (§H).

## Integration points

| Point | Code | On request path? | Owner of retries |
|---|---|---|---|
| S3 / SeaweedFS | `internal/objstore` via `durable.Store`, `vactor.Fetcher` | cold activation only | vactor (write-back), owner (stamp); minio: none |
| Postgres placement | `owner.Placement` adapter (CP step 3) | no (Claim, Reconcile, Move) | owner / Mover |
| Inbound HTTP | `internal/front`, `cmd/agentd` | yes | the client |
| Client → locator / agent | CP step 6, `internal/retry` | yes | client, once |
| Actor mailbox / read pool | `internal/actor` | yes | none (shed) |
| Local disk (fsync, cache dir) | `owner.FileStore`, vactor cache | yes (commit) | none |

A new integration point is a design change: add a row here and a sheet in §P.

## T — Timeouts

**T1. Every remote call has a deadline. No exceptions, including background work.**
A missing timeout lets a slow dependency's hang become ours (ch4 §4.3, ch16). A slow response is worse than a
refusal (ch4 §4.1).

```go
ctx, cancel := context.WithTimeout(parent, s.cfg.OpTimeout) // never h.ctx alone (RI-02)
defer cancel()
m, v, err := store.ReadManifest(ctx, key)
```

**T2. Never pass a deadline-free ctx to I/O.** `context.Background()` and long-lived host or ticker contexts are
roots, not budgets. Derive a per-call deadline from them.
Why: `Tick(ctx)` with the maintenance ctx let one PUT hang all maintenance (RI-05).

**T3. Bound bodies by time as well as size.** Large transfers get `OpTimeout + size/MinThroughput`, and a stall
detector fails a read that gets no bytes for `StallTimeout`. `ResponseHeaderTimeout` alone doesn't protect a
body that stops halfway (RI-01).

**T4. Deadlines shrink as they go down.** An inner timeout is less than the outer one minus the work left.
Never retry past the caller's deadline (ch5 §5.1).

```go
stmt := min(s.cfg.StmtTimeout, max(deadline.Sub(now), time.Millisecond)) // front/server.go:166
```

**T5. Waiting is not working: an abandoned wait must stop the work.** If a caller gives up, the statement or
transfer it started is interrupted or bounded too.
Why: `actor.Do` let the caller leave while SQLite kept running (RI-08).

**T6. Every pool checkout and every semaphore acquire takes the ctx.** No unbounded blocking acquire (ch9
§9.1). pgxpool `Acquire(ctx)`, `sem.Acquire(ctx, n)`, admission `Acquire(ctx, …)`.

## R — Retries

**R1. One retry layer per call chain, at the layer that understands the outcome.** Turn off library retries
(`minio.Options{MaxRetries: 1}`, no driver auto-retry). Layered retries multiply: 11 minio attempts × 8 vactor
uploads × client retries (ch4 §4.3, plan G2).

**R2. Only retry what is safe, and say so in the error.** Error text states whether anything changed and whether
a retry is safe (`go-coding-style.md` §4). Ambiguous outcomes are re-read, not retried blindly.

```go
v, err := store.PublishManifest(ctx, next, prev)
if err != nil { // may have applied: read back before deciding (writeback.go:264)
	if stored, sv, rerr := store.ReadManifest(ctx, key); rerr == nil && stored.Same(next) { return sv, nil }
}
```

**R3. Back off exponentially with full jitter, cap the attempts, and honor `Retry-After`.** Use
`max(serverHint, jitteredDelay)`. The defaults in `internal/retry` must be safe; "none" is for bench profiles
only (RI-10).

**R4. Retries spend a windowed budget.** Use a token bucket refilled per first attempt, with a cap, so credit
cannot be saved up for a storm (RI-10).

**R5. Prefer queue-and-retry-later to retry-now.** Return an answer quickly and let background work catch up.
Write-back is the model: acks don't wait for S3, and failed write-backs stay dirty for the next tick (ch5
§5.1).

**R6. A refused or failed key backs off per key.** After a failed activation, the key holds a short jittered
tombstone. Later requests fail fast instead of re-fetching (RI-03).

## B — Bulkheads and backpressure

**B1. Every queue is bounded, and a full queue sheds. It never blocks.** Mailbox, read queue, admission queues,
fetch queue, CSV writer.

```go
select {
case a.mailbox <- r:
	return nil
default:
	return ErrMailboxFull // actor.go:146-151
}
```

**B2. Each dependency gets its own concurrency limit, sized to its measured knee.** S3 fetches: count and
bytes in flight (default 16 / 1 GiB, from the coldload knee). Postgres: the pool size. One dependency's
slowness must not take slots another needs (ch5 §5.3, ch16).

**B3. Caps are nested: database ≤ tenant ≤ VM.** The VM-wide cap exists so many tenants cannot exceed the
process (RI-09).

**B4. Reserve capacity for control.** Health, metrics and lifecycle (Claim/Fence) run on the lifecycle
listener, never queued behind queries (ch5 §5.3 admin threads, ch2).

**B5. No slow I/O while holding a lock that the request path takes.** `vactor` runs activation in its own
goroutine outside `h.mu`. Keep it that way (ch4 §4.5 hidden locks).

**B6. Bound every result set and listing.** `MaxRows` on reads, `LimitReader` on manifests, paged object
listing (RI-05). "Zero, one, lots" (ch4 §4.11).

## C — Circuit breakers

**C1. Wrap each remote dependency in a breaker** (`internal/breaker`, RI-03): Closed → Open on a failure
rate in a window → HalfOpen after a cool-down, one probe → Closed or back to Open.

**C2. Count only system failures.** Timeouts, connection errors and 5xx trip it. `ErrNotFound`, `ErrStale`,
`ErrConflict` and bad input do not; they are answers (ch5 §5.5).

**C3. Open means fail fast with a distinct error and a retry hint.**

```go
if !b.Allow(now) {
	return errs.Wrap(module, fmt.Errorf("%w: s3 breaker open", durable.ErrUnavailable), "nothing changed, retry after %v", b.Until(now)-now)
}
```

**C4. Breakers are visible and operable.** Log every transition with a message code. Expose state and the
transition count. Ops can trip and reset them (RI-11, RI-16).

**C5. Open-circuit behavior is a product decision, written down.** For example: S3 open → cold activations
`503`, warm traffic unaffected, write-backs deferred (RPO grows, alert on `oldest_dirty_age`).

## F — Fail fast and error classes

**F1. Check what you need before doing expensive work.** Admit, then admission, then executor. Reject on
ticket, ownership, breaker state or full queues before touching SQLite or S3.

**F2. Dependency unavailable ≠ bug ≠ bad request.** System failures go on the wire as `503 + Retry-After`
(log at WARN, sampled). Bugs are `internal` with a log ID (ERROR). Bad input is `400`. Never let an S3 outage
classify as `internal` (RI-04).

**F3. A request that cannot finish in time is refused, not started.** Expired queued work is answered
`expired` unrun (`actor.go` `split`).

## S — Steady state

**S1. Everything that accumulates has a reaper running at least as fast.** Generations → GC; cache dir →
eviction; local fenced records → `Forget`; logs → rotation outside the process (ch5 §5.4).

**S2. Background loops can't stall.** Each job has its own deadline. A tick never overlaps the next. A stuck
tick is reported, not silently waited on (RI-05).

**S3. Track the age of the oldest unfinished thing.** `oldest_dirty_age`, oldest unreconciled key and oldest
queued request are steady-state gauges; slope ≈ 0 is the health signal.

## H — Health, lifecycle, test harness

**H1. Startup: validate, open, then accept, and accept per key as early as safe.** Reconcile with bounded
concurrency and a per-key timeout. Serve reconciled keys while the rest are refused with `ErrUnavailable`
(RI-07).

**H2. Liveness ≠ readiness, and readiness exercises the real path.** `/readyz` runs a marked canary through
admission → actor and checks breaker state (ch2, ch13 synthetic transactions).

**H3. Shutdown has one budget.** Drain front, then write back oldest-dirty first until the budget ends, then
close with local copies kept and the count logged (RI-06).

**H4. Every adapter's contract suite runs three ways:** against the fake, the real service, and the real
service through `faultnet`/`s3test` (RI-13). The minimum failure table for each adapter:

| Fault | Harness mode | Expected |
|---|---|---|
| connection refused | `faultnet.Refuse` | fails in < 100 ms, `unavailable` |
| accept, no reply | `AcceptThenSilence` / `Blackhole` | fails at the op timeout |
| headers, then stall | `s3test.HeadersThenStall` | fails at the stall timeout, no partial file |
| 1 byte/s | `Dribble` | fails at the size-derived deadline |
| 5xx / throttling | `Status(503)×N` | exactly one retry layer; breaker counts it |
| applied, reply lost | `ApplyThenDropReply` | outcome resolved by re-read, not misreported |
| truncated / corrupt body | `TruncateBody` / `CorruptBody` | `ErrCorrupt`, nothing activated |
| half-open after idle | `HalfOpen` | stale connection recycled, no hang |

**H5. Test under load and over time.** Fault modes also run during a frontbench/wbbench run. Soak with idle
hours (ch3) before calling a component done.

## O — Observability

**O1. Per integration point, expose:** requests by outcome class (`ok`, `app_error`, `timeout`, `refused`,
`5xx`, `ambiguous`), latency histogram (in-flight requests count at their deadline, so hung work shows),
in-flight + high-water, breaker state + transitions, last success time. Per pool or queue: size, in use, high
water, times full (ch17).

**O2. Expose every counter, and keep thresholds out of the code.** Alert rules live in the monitoring config
(ch17 "exoskeleton").

**O3. Log for operators.** ERROR = someone must act (breaker open long, write-back failing past RPO, bug).
WARN = degraded but self-healing. One stable message code per event (`S3.BREAKER_OPEN`, `OWNER.SUPERSEDED`).
Correlation IDs: `request_id`, `log_id`, db key, epoch. Log state transitions, not every refusal.

## P — Per-integration sheets (defaults)

**S3** (`objstore`)
- Dial 5 s, `OpTimeout` 5 s, `StallTimeout` 10 s, `MinThroughput` 32 MiB/s.
- minio `MaxRetries` 1. Fetch bulkhead 16 / 1 GiB, queue 256.
- Breaker on 50% system failures over ≥ 20 calls in 10 s, cool-down 5 s.
- Metrics per op: `get_manifest`, `fetch`, `put_gen`, `publish`, `head`, `list`.

**Postgres** (placement, controld)
- pgxpool `MaxConns` = reconcile workers + lifecycle concurrency; acquire under ctx.
- `connect_timeout` 3 s, `statement_timeout` 2 s, `lock_timeout` 1 s, `idle_in_transaction_session_timeout`
  5 s, TCP keepalives on.
- `MaxConnIdleTime` < the smallest NAT/LB idle timeout (ch4 §4.1 "5 a.m. problem").
- Primary reads only for the port. A connection error never maps to `ErrConflict`/`ErrNotFound`. Breaker per
  pool.

**Inbound HTTP** (`agentd`)
- `ReadHeaderTimeout` 5 s, body read deadline 5 s, write deadline = request deadline + grace, `MaxHeaderBytes`
  16 KiB, `MaxBody` 1 MiB.
- Admission caps DB/tenant/VM. Health and admin on the lifecycle listener.

**Client** (CP step 6)
- Per-request deadline sent as `X-Timeout-Ms`. `http.Client` with dial and response-header timeouts.
- Exponential backoff + full jitter, honoring `Retry-After`; windowed budget.
- Re-resolve on `421`/`401`. The same `request_id` on retried writes.

**Actor / read pool**
- Every request carries `Deadline` and `StmtTimeout`. Mailbox 256, read queue = mailbox. A full queue sheds.
- `VACUUM INTO` snapshots count as writer time; keep them under the write-back deadline.
