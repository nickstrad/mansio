# Plan: apply _Release It!_ (Nygard, 1st ed.) to ts-db-go

Status: plan · Date: 2026-11-05 · Baseline: `main` at `bbf184b` · Companion: [guide.md](guide.md) (the
integration-points and stability rules this plan installs).

Sources: book summaries `~/AI/books/release-it/chapters/*.md`, checked against
`~/AI/books/release-it/raw/chapters-text/` for the points below that carry a recommendation (ch5 §5.1 retries,
ch4 §4.9 self-monitoring fail-fast, ch5 §5.3 reserved admin capacity). Project: `README.md`, `STATE.md`,
`docs/control-plane-plan.md` (cited as **CP**), `knowledge/{fencing,front,writeback,coldload,testing}/`, and the
code in `internal/` and `cmd/agentd`. Testing builds on `knowledge/testing/README.md` and
`~/AI/writeups/go-testing.md` §7 (fakes with fault switches); it is not redone here. Chapter numbers are
"ch4 §4.1" style.

## 1. Why this book matters here

ts-db-go is about to become a distributed system. CP step 1 wires the agent to S3 (`vactor` + `durable` +
`objstore`), step 3 adds a Postgres placement authority, steps 4–6 add controld, a locator and a retrying
client. Nygard's central claim (ch4 §4.1, ch4 §4.3) is that **integration points are the number-one source of
cracks and missing timeouts are the number-one way cracks spread**. Every one of those steps adds an
integration point. The building blocks are careful about *correctness* (fencing, conditional manifests,
ambiguous-outcome handling). The gaps are about *stability*: unbounded waits on S3, hidden retries inside
minio-go, no bulkhead on fetches, no breaker, no per-integration metrics, and shutdown and startup that scale
with the number of databases. Now, before CP step 1 glues them together, these are cheap to fix. Ch15 says they
are expensive to fix later.

## 2. What the project already does well

| Book idea | Already here |
|---|---|
| Bounded queues, shed instead of block (ch5 §5.5 Fail Fast, ch9 §9.1) | `actor.Submit` never blocks; a full mailbox gives `ErrMailboxFull` (`internal/actor/actor.go:138-153`). The read queue sheds too (`actor.go:295-302`). |
| Deadlines travel with the work (ch5 §5.1) | `Request.Deadline` expires queued work unrun; `StmtTimeout` interrupts SQLite (`actor.go:43-47`, `run1` `actor.go:362-375`). The front caps client deadlines (`internal/front/server.go:139-145, 166`). |
| Bulkheads per tenant and database (ch5 §5.3) | `front.AdmissionLimits` (`internal/front/limiter.go:13-32`): per-tenant and per-database in-flight and queue caps. They are work-conserving, and memory is bounded by active keys. |
| Unbounded result sets (ch4 §4.11) | `Limits.MaxRows` (default 10,000) gives `bad_request` rather than an unbounded reply (`internal/front/request.go:51`). The manifest read is capped at 64 KiB (`internal/objstore/durable.go:65`). |
| Handshaking (ch5 §5.6) | Closed wire-code set with `503 + Retry-After` for shed and `421` for re-resolve (`internal/front/codes.go:51-62`). |
| Hot path free of the slow dependency (ch4 §4.10 SLA inversion) | `owner.Table.Admit` is local and lock-free (9 ns, `internal/owner/table.go:195-208`). Postgres is only on Claim/Reconcile; S3 is only on cold activation and write-back. Write acks do not wait for S3 (write-back is store-and-forward, as ch5 §5.1 recommends). |
| Ambiguous outcomes are explicit (ch11 VIP failover) | `durable.Store` postconditions name the "may or may not have applied" case (`internal/durable/durable.go:102-108`). `vactor.publish` re-reads to resolve it (`internal/vactor/writeback.go:251-276`). |
| Clean startup and shutdown (ch14) | agentd opens every database before it listens (`cmd/agentd/main.go:92-107`, the "master switch"). It drains with a bound (`main.go:155-169`). It exits 1 with a log ID when the drain is cut short. |
| Config as an operator UI (ch14) | Unknown fields are rejected, every problem is reported at once, defaults are explicit, and the listener binds `127.0.0.1` by default (`cmd/agentd/config.go:54-145`). |
| Logs for operators (ch17) | Log once at the top with a log ID and the chain (`internal/front/http.go:113-131`, `cmd/agentd/main.go:263-302`). |
| Interface-level fault fakes (ch5 §5.7, partly) | `durabletest.Faults` (Down, FailUpload, LosePublishReply, Delay…), `ownertest.Placement` (Down, Delay, Stale, LoseReply), and an in-process `fakeS3` HTTP server (`internal/objstore/fakes3_test.go:16-30`). |

## 3. Gaps and risks (ranked by impact)

Each finding: book idea → where → what goes wrong → evidence.

**G1. Cold activation has no deadline; a hung S3 hangs the key and burns the caller's whole budget.**
Ch4 §4.1/§4.9, ch5 §5.1. `fetchDurable` calls `ReadManifest` and `FetchGeneration` with `h.ctx`, which only
`Close` cancels (`internal/vactor/writeback.go:70, 94`; plain mode `internal/vactor/host.go:356`). The S3
transport bounds dial (10 s) and response headers (60 s), but not the body (`internal/objstore/objstore.go:58-67`).
Scenario: SeaweedFS sends headers and then stalls mid-body. The activation goroutine waits forever. Every Get
for that key joins the same activation (`host.go:283-295`) and each waits until its own ctx ends, so each
request costs its full deadline. Because the entry stays in the map, the key cannot be retried until the
process closes.

**G2. Hidden retry layer: minio-go retries 10× by default, including 503/429.** Ch4 §4.3 ("blindly retrying
transient errors hammered a failing layer"), ch5 §5.1. `minio.New` is called without `MaxRetries`
(`objstore.go:68-71`). minio-go v7.4.1 then uses `MaxRetry = 10` with 200 ms–1 s backoff
(`~/go/pkg/mod/github.com/minio/minio-go/v7@v7.4.1/retry.go:34,44,48`) and retries 500/502/503/504/429 and any
transport error (`retry.go:120-155`, loop at `api.go:740`). Scenarios:
- (a) S3 answers `503 SlowDown`. Every logical call becomes up to 11 requests, and our own retries (vactor
  `upload` 8 tries `writeback.go:208-215`, owner `StampRetries`, client retries) multiply on top.
- (b) Worst case per GET is about 11 × 60 s of header wait.
- (c) A conditional PUT whose reply is lost is retried by minio, gets 412, and is reported to us as
  `ErrStale` although *our* write applied. `vactor.publish` survives this by re-reading (`writeback.go:264-269`).
  The warm path of `vactor.Stamp` does not: it sets `e.stale = true` on any `ErrStale` (`writeback.go:469-473`).
  The key then refuses every later write-back, and owner's stamp loop gives up after 3 tries
  (`internal/owner/table.go:415-440`). This is a liveness bug, not a safety bug. Confirm it with RI-13's
  lost-reply switch.

**G3. Every request for a broken key re-hits S3: no breaker, no negative cache.** Ch5 §5.2, ch4 §4.6
(self-denial). "A failed activation is forgotten, so a later Get tries again" (`host.go:12, 331`). Scenario:
S3 is down and 5,000 cold keys get traffic at 20k req/s. Each request starts a manifest GET, so the agent
becomes S3's worst client exactly when S3 is weakest, and it recovers last.

**G4. No bulkhead on fetches; concurrency past the knee lowers throughput.** Ch5 §5.3, ch9 §9.1. Each new key
starts `go h.activate` (`host.go:274`). The only bound is `MaxConns` 1024 (`objstore.go:55-57`). Measured knee:
132 MiB fetches give 253 MB/s at c=1 and 134 MB/s at c=64 (`knowledge/coldload/2026-11-04-sizes/README.md`).
Scenario: after a restart, or a mass move onto a VM, hundreds of keys warm at once. All of them slow down, and
the disk fills with `.part` files before any key is ready.

**G5. Dependency failures surface as `internal` 500s, not as "unavailable".** Ch5 §5.5 ("report system
failures differently from application failures"). `front.Classify` maps any unknown error to `Internal`
(`internal/front/codes.go:111`). The CP-step-1 executor adapter will return vactor and S3 errors from `GetAt`.
Those become 500 "a write may or may not have committed" (`codes.go:61`), with an ERROR log line per request
(`http.go:113-131`). Scenario: an S3 outage pages on-call with thousands of "bugs", and clients don't back off,
because 500 has no `Retry-After`.

**G6. Maintenance has no per-job timeout, so one hung write-back stops Steady State.** Ch5 §5.4. `Tick` runs
each job with the caller's ctx (`internal/vactor/maintain.go:78-103`). `RunMaintenance` passes its long-lived
ctx (`maintain.go:124`). `WriteBackTimeout` applies only to `PassivateAll`/`Close` (`host.go:106`,
`writeback.go:497-499`). Scenario: one PUT stalls, `Tick` never returns, and no later tick runs. Eviction stops,
the cache directory and open fds grow, and RPO grows silently. Separately, generations are never
garbage-collected (`knowledge/writeback/README.md` "Open risks"), and `objstore.List` builds one unbounded map
of every key under a prefix (`objstore.go:197-207`). That is ch4 §4.11 waiting to happen once GC lists the
bucket.

**G7. Shutdown and startup scale with database count.**
- Ch14 "clean shutdown … bounded by a timeout". `Host.Close` → `PassivateAll` gives each key up to 5 min, 8 at a
  time, with no overall budget (`writeback.go:483-503, 546-558`). With 10k dirty keys and slow S3, SIGTERM can
  take hours, and the orchestrator will `kill -9` first.
- Ch14 startup. CP step 1 says "retry `ReconcileAll` until it succeeds. Don't serve before that."
  `ReconcileAll` is serial over all keys, with two remote calls each and no per-call timeout
  (`table.go:586-601`). One slow Postgres row delays every key. Yet `Admit` already fails fast per key while
  it is unreconciled (`ErrUnavailable`, `internal/owner/state.go:233`), so the agent can safely serve
  reconciled keys while the rest catch up.

**G8. `actor.Do` ignores the caller's deadline for execution.** Ch5 §5.1. `Do` builds a request with no
`StmtTimeout` or `Deadline` (`actor.go:155-162`). `ctx` only bounds the wait. `actordb.Query` uses it
(`internal/vactor/actordb/actordb.go:35`), so vactor's `QueryTimeout` (`host.go:410-414`) abandons a first or
warm query that keeps running and holding the write connection.

**G9. Health checks don't exercise the work path.** Ch2 (the green status page over a hung EJB pool), ch13
synthetic transactions. `/healthz` checks only `Draining()` (`http.go:42-53`). With every actor wedged, S3 down,
or the owner table unreconciled, it still says `ok`. There is no readiness/liveness split.

**G10. No transparency per integration point.** Ch17 "what to expose", ch16 (average latency hid hung
requests). `vactor.Stats`, `Admission.Stats` and `Directory.Stats` exist (`host.go:423`, `admission.go:68`,
`internal/actor/directory.go:112`), but agentd exposes none of them. There is no latency histogram, in-flight
high-water mark, breaker state, or "age of last successful write-back" for S3, and nothing yet for Postgres.

**G11. HTTP server lacks body and write timeouts; there is no VM-wide cap.** Ch4 §4.4 (users), ch5 §5.3.
`http.Server` sets only `ReadHeaderTimeout`/`IdleTimeout` (`cmd/agentd/main.go:119-120`). A client can dribble
a 1 MiB body forever (`http.go:82`). Admission caps are per tenant and per database (`limiter.go:13-18`), so
the VM-wide in-flight count is tenants × 256, which is unbounded.

**G12. Client retry defaults and budget are unsafe for production reuse.** Ch5 §5.1, ch4 §4.3. With no
config, `retry.Policy` means no backoff and no jitter (`internal/retry/retry.go:57-68`). `Budget` is cumulative
since start (`retry.go:143-171`), so credit banked in a quiet hour pays for a storm later. `Delay` ignores the
server's `Retry-After` (`retry.go:89-115`). CP step 6 builds the real client on this package.

**G13. Doc drift on a safety assumption.** `internal/objstore/durable.go:29-32` still says conditional PUT is
"UNVERIFIED". `knowledge/writeback/2026-11-05-s3-conditional-put-verified.md` and CP §4 say it is verified
on single-node SeaweedFS. Operators read comments as truth (ch17 "voodoo operations").

## 4. Refactor items

Size S ≤ ½ day, M ≤ 2 days, L > 2 days. "Pre-CP" means do it before or alongside CP step 1. The step named is
where it lands.

| ID | Item | Gap | Size | When | Depends |
|---|---|---|---|---|---|
| RI-13 | Fault-injecting network harness (S3 + Postgres) | all | M | pre-CP | — |
| RI-01 | S3 client budget: one retry layer, per-op deadlines, stall detection | G1 G2 G13 | M | pre-CP | RI-13 |
| RI-14 | Warm `Stamp` re-reads on ambiguous `ErrStale` | G2c | S | pre-CP | RI-13 |
| RI-08 | `actor.Do` propagates ctx deadline into execution | G8 | S | pre-CP | — |
| RI-02 | Activation deadline + fetch bulkhead (bytes and count in flight) | G1 G4 | M | CP 1 | RI-01 |
| RI-03 | S3 circuit breaker + per-key failure backoff | G3 | M | CP 1 | RI-02 |
| RI-04 | `ErrUnavailable` wire mapping (503 + Retry-After, WARN not ERROR) | G5 | S | CP 1 | — |
| RI-09 | HTTP body/write deadlines + VM-wide admission cap | G11 | S | CP 1 | — |
| RI-05 | Maintenance per-job deadlines, staleness gauge, paged listing | G6 | M | CP 1 | RI-01 |
| RI-06 | Bounded shutdown budget across front, host and owner | G7 | M | CP 1 | RI-05 |
| RI-07 | Startup readiness: serve while reconciling; live/ready split; canary | G7 G9 | M | CP 1 | RI-04 |
| RI-11 | Metrics + state-transition logs on the lifecycle listener | G10 | M | CP 1 | RI-03 |
| RI-12 | Postgres placement adapter stability rules | — | M | CP 3 | RI-13, RI-11 |
| RI-10 | Handshaking: adaptive Retry-After; retry honors it; windowed budget; safe defaults | G12 | M | CP 6 | RI-04 |
| RI-16 | Runtime controls + scriptable admin (`agentctl`), secrets file | G10 | M | CP 4 | RI-11 |
| RI-15 | Longevity, capacity-knee and fault-under-load runs | all | L | after CP 4 | RI-13 |

#### RI-13 — Fault-injecting network harness — **Done** (32dfce4; Postgres side not built yet: no Postgres adapter exists)

- **Goal.** Ch5 §5.7: a *devious* stand-in that breaks transport and protocol, not only the Go interface.
  Today's fakes (`durabletest`, `ownertest`) behave only within the interface. Ch5 §5.7 says mocks cannot
  find a missing socket timeout.
- **Files.** New `internal/faultnet` (TCP proxy with per-connection behaviors). Extend
  `internal/objstore/fakes3_test.go` into an exported `internal/objstore/s3test` package.
- **Change.**
  - `faultnet.Proxy{Upstream}` with switchable modes: `Refuse`, `AcceptThenSilence`, `Blackhole` (accept,
    read, never reply), `ResetAfter(n bytes)`, `Latency(d)`, `Throttle(bytes/s)`, `HalfOpen` (stop
    forwarding, keep the socket).
  - `s3test.Server` modes: `HeadersThenStall`, `Dribble(1 B/s)`, `TruncateBody`, `WrongContentLength`,
    `Status(503 SlowDown | 500 | 429)` for the next N requests, `ApplyThenDropReply` (conditional PUT
    applied, connection reset before the response), `CorruptBody`. Every mode is a counter-limited switch so a
    test can fail N times, then heal.
- **Acceptance.** `faultnet` self-tests for each mode, with `-race`. `RunStoreSuite` still passes through the
  proxy with all faults off.
- **Size.** M.

#### RI-01 — S3 client budget — **Done** (20c4311; a GET makes ≤ 5 requests under 503s, a PUT exactly 1, not "2 per call")

- **Goal.** Exactly one retry layer, and a deadline on every S3 call (guide rules T1, R1).
- **Files.** `internal/objstore/objstore.go`, `durable.go`.
- **Change.**
  - `Config` gains `MaxRetries` (default 1, so minio does not retry; `vactor` and owner already retry with
    knowledge of ambiguity), `OpTimeout` (manifest/HEAD/small PUT, default 5 s), `MinThroughput` (default
    32 MiB/s) and `StallTimeout` (default 10 s).
  - Body reads go through a `stallReader` that fails the read when no byte arrives for `StallTimeout`.
  - Large transfers get a deadline of `OpTimeout + size/MinThroughput`, applied inside the adapter when the
    caller's ctx has a later or no deadline.
  - `ResponseHeaderTimeout` drops to `OpTimeout`.
  - Fix the stale "UNVERIFIED" comment (`durable.go:29-32`).
- **Acceptance.**
  - With RI-13, `HeadersThenStall` makes `FetchGeneration` fail within `StallTimeout + 1 s`, leaving no `.part`.
  - `Status(503)×5` produces exactly 2 HTTP requests per call (count them at the server).
  - `Dribble` fails `GetToFile` before its size-derived deadline.
- **Size.** M.

#### RI-14 — Warm `Stamp` resolves ambiguity like `publish` — **Done** (6d768de; the fake switch is `durabletest.Faults.ApplyThenStale`, and since RI-01 removed the `MaxRetries` knob the G2c re-send is reproduced by `s3test.ApplyThenPreconditionFailed` next to `ApplyThenDropReply`)

- **Change.** In `vactor.Stamp` (`writeback.go:469-474`), on any error re-read the manifest. If the stored
  manifest `Same(next)`, adopt it and succeed. Set `e.stale` only when the stored epoch is greater than ours.
- **Acceptance.**
  - New `durabletest` mode: `LosePublishReplyAsStale` (applies, then returns `ErrStale`).
  - `TestWarmStampLostReplyIsNotSuperseded` passes, and `FuzzDurableHostModel` gains the switch.
  - The same test passes through `s3test.ApplyThenDropReply` with `MaxRetries` 2 (proving G2c is real) and 1.
- **Size.** S.

#### RI-08 — `actor.Do` bounds execution — **Done** (049f5ca)

- **Change.** `Do` sets `Deadline` from `ctx.Deadline()` and `StmtTimeout = time.Until(deadline)` when ctx has
  one (`actor.go:155-162`). agentd and vactor never pass a deadline-free ctx (guide T2).
- **Acceptance.**
  - `TestDoWithDeadlineInterruptsStatement`: a `slow_cte` under a 50 ms ctx is interrupted, and the next
    request runs within 100 ms.
  - The vactor first-query timeout test asserts the actor is free afterward.
- **Size.** S.

#### RI-02 — Activation deadline and fetch bulkhead — **Done** (3797f59)

- **Goal.** No activation outlives its budget, and fetches stay below the measured knee (ch5 §5.3, ch9 §9.1).
- **Files.** `internal/vactor/host.go`, `writeback.go`.
- **Change.**
  - `Options.ActivationTimeout` (default 30 s, and `OpTimeout + size/MinThroughput` once the manifest is known).
    `activate` derives its ctx from `h.ctx` with that timeout.
  - `Options.MaxFetches` (default 16, per the coldload knee) and `MaxFetchBytes` (default 1 GiB in flight) form
    a weighted semaphore. `MaxFetchQueue` (default 256) bounds keys waiting for it. When the queue is full,
    `GetAt` returns `vactor.ErrBusy` at once (guide B2).
- **Acceptance.**
  - Spec table on the semaphore at 0/1/limit/limit+1.
  - `TestActivationTimesOutAndIsRetryable` (fake store `Delay` > timeout: Get fails in ≤ timeout + ε, and the
    next Get starts a fresh activation).
  - `TestFetchBulkheadShedsBeyondQueue`.
  - coldload `sizes` rerun at c=64 shows throughput ≥ the c=16 figure.
- **Size.** M.

#### RI-03 — S3 circuit breaker and per-key failure backoff — **Done** (7244750)

- **Goal.** Stop calling S3 when it is failing (ch5 §5.2), and stop one broken key from being refetched on
  every request (ch4 §4.6).
- **Files.** New `internal/breaker` (pure state machine: Closed/Open/HalfOpen, failure-rate window, clock
  port), plus a `durable.Store` decorator `breakerStore` in `internal/vactor` or `internal/objstore`.
- **Change.**
  - The breaker counts only *system* failures (timeouts, 5xx, connection errors). It does not count
    `ErrNotFound`, `ErrStale` or `ErrExists`, which are application answers (ch5 §5.5).
  - Open → `durable.ErrUnavailable` (new sentinel) with `RetryAfter` = time to half-open.
  - Separately, a failed activation leaves a tombstone `{until: now + backoff(n)}` (100 ms → 5 s, jittered).
    Gets before `until` fail fast with the same cause.
  - Every transition is logged (guide O3).
- **Acceptance.**
  - `FuzzBreakerModel` (byte script of ok/fail/tick vs a reference model).
  - `TestS3OutageCallsBounded`: with the store `Down` for 10 s and 1,000 req/s on 100 cold keys, the store
    sees ≤ (window probes + 100 × backoff steps) calls, not 10,000.
  - After heal, all keys activate within one half-open interval plus backoff.
- **Size.** M.

#### RI-04 — Unavailable is not internal — **Done** (cfdeadf)

- **Files.** `internal/front/codes.go`, the CP-1 executor adapter in `cmd/agentd`.
- **Change.**
  - Add `front.ErrUnavailable`, classified as `Shed` (503) with `Retry-After`, *or* a new code `unavailable`
    (decide; adding a code changes contract 3 and needs `TestSpecTable` and the wire doc updated).
  - The adapter maps `vactor.ErrBusy`, `durable.ErrUnavailable`, activation timeouts and `owner.ErrUnavailable`
    to it.
  - Log these at WARN, sampled, never one ERROR per request.
- **Acceptance.** `TestPipelineWireCodes` rows for each source. An e2e run with S3 down gives 503 + Retry-After,
  zero `internal`, and fewer than N log lines per second.
- **Size.** S.

#### RI-09 — HTTP deadlines and a VM-wide cap — **Done** (0d6d0ec)

- **Change.**
  - In `httpHandler.query`, set `http.NewResponseController(w).SetReadDeadline(now + BodyReadTimeout)` before
    `io.ReadAll` and `SetWriteDeadline(deadline + grace)` before writing. `ReadTimeout` stays unset because h2c
    streams share the connection.
  - Give the health server `IdleTimeout` and `WriteTimeout`.
  - `AdmissionLimits.VMInFlight` (default 4 × GOMAXPROCS × DBInFlight) and `VMQueue`, enforced in `limiter`
    with the same invariant style.
- **Acceptance.**
  - `TestSlowBodyIsCut` (1 B/s body → 408/400 within the timeout).
  - Limiter spec table and model fuzz extended with the VM cap. The invariant becomes Σ tenants in-flight ≤
    VMInFlight.
- **Size.** S.

#### RI-05 — Steady-state maintenance — **Done** (8735cd4; paged listing + generation GC left to OS-06)

- **Change.**
  - `Tick` wraps each job in `context.WithTimeout(ctx, WriteBackTimeout)` (`maintain.go:78-103`). `TickReport`
    gains `TimedOut`.
  - `RunMaintenance` never lets one tick overlap the next. A tick still running at the next interval is
    reported, not stacked.
  - Add gauges `oldest_dirty_age` and `last_writeback_ok`.
  - `objstore.List` becomes paged (`ListPage(ctx, prefix, after, limit)`), with the generation GC from CP §3
    step 8 built on it. GC deletes generations older than the manifest's generation − K, by page.
- **Acceptance.**
  - `TestTickHungWriteBackDoesNotStopMaintenance` (store `Delay` on one key: other keys still passivate, and
    the next tick runs).
  - GC spec table (keep current, keep K, delete orphans older than T).
  - A 10k-object prefix listed with constant memory.
- **Size.** M.

#### RI-06 — Bounded shutdown — **Done** (4d7d756; agentd has no vactor host until CP-1: its `host.close` phase bounds the actors, and `vactor.Host.Shutdown(ctx)` slots in there)

- **Change.**
  - agentd gets one `shutdown_budget` (default 60 s), split into three phases: front drain
    (`drain_timeout`), then `Host.Close(ctx)` with the remainder, then exit.
  - `Close` gains a ctx. Past the deadline it stops starting write-backs and closes the rest with local files
    kept (already safe, `writeback.go:553-556`). It logs one line naming the count and the cache dir.
  - Order dirty keys by oldest unpublished commit first.
- **Acceptance.** `TestCloseHonorsDeadline` (500 dirty keys, store `Delay` 1 s: Close returns within budget +
  ε, and kept files equal un-written keys). The e2e SIGTERM run meets the budget.
- **Size.** M.

#### RI-07 — Startup readiness and real health — **Done** (1ece893; agentd opens databases in the background as the stand-in for `owner.ReconcileUntilDone` until CP-1; the CP-1 kill -9 gate and the S3 canary HEAD wait for CP-1)

- **Change.**
  - Listen early. `ReconcileAll` becomes `ReconcileAll(ctx, workers, perKeyTimeout)` (bounded concurrency,
    default 16 workers, 5 s per key), retried in the background with backoff. Unreconciled keys are already
    refused per key (`ErrUnavailable` → RI-04 503).
  - Split `/livez` (process up, admin pool responsive) from `/readyz`. Ready means reconcile has finished at
    least once and the canary passes. (Decided 2026-11-05 with the user: an open S3/placement breaker does
    not fail readiness, so an S3 outage does not drain every agent; it is a metric and an alert. Guide C5.)
  - The canary is a synthetic transaction (ch13): a tiny local canary database read through the real
    admission → actor path, plus a periodic cheap S3 HEAD on a canary object. It is marked so it never pollutes
    tenant data.
  - Health and admin live on the lifecycle listener, not the query listener (reserved capacity, ch5 §5.3; ch2).
- **Acceptance.**
  - `TestServesReconciledKeysWhileOthersPending`.
  - `TestReadyzFalseWhenActorsWedged` (the canary actor is blocked, so readyz is 503 within one interval).
  - The CP-1 kill -9 e2e gate also asserts time-to-first-served-key < time-to-all-reconciled.
- **Size.** M.

#### RI-11 — Transparency — **Done** (aaaaf61; S3, breaker, vactor and owner adapters are tested but registered at CP-1; placement waits for RI-12; shed Retry-After queue-drain estimate done)

- **Change.**
  - The lifecycle listener serves `/metrics` (Prometheus text format, from stdlib counters; no new dependency
    unless the user approves one).
  - Per integration point (`s3`, `placement`, `front`, `actor`):
    - requests and outcomes by class: `ok`, `app_error`, `timeout`, `refused`, `5xx`, `ambiguous`
    - latency histogram, which includes requests that never completed (they are counted at their deadline,
      ch16)
    - in-flight and its high-water mark
    - breaker state and transition count
  - Plus vactor `Stats`, admission stats, and actor queue depth p99 across actors.
  - Log every state transition (breaker, fence, supersede, reconcile done, drain start/end) at INFO/WARN with a
    stable message code (`S3.BREAKER_OPEN`) (ch17 message catalog).
- **Acceptance.** A test scrapes `/metrics` after a scripted fault sequence and checks every counter named in
  guide O1. `knowledge/front/` gains the metric catalog.
- **Size.** M.

#### RI-12 — Postgres placement adapter stability

- **Change.** Implement the adapter per `knowledge/fencing/2026-11-05-postgres-placement.md`, plus guide §P:
  - pgxpool `MaxConns` sized to the reconcile workers plus lifecycle concurrency, and an acquire timeout via ctx
  - `connect_timeout`, `statement_timeout`, `idle_in_transaction_session_timeout` and `lock_timeout` set per
    session, and TCP keepalives on
  - `MaxConnIdleTime` below any NAT or LB idle timeout (the ch4 §4.1 "5 a.m. problem")
  - a breaker per pool
  - connection errors are never `ErrConflict` or `ErrNotFound`
- **Acceptance.** `TestPlacementCAS` against the fake and against Postgres through `faultnet`. Under `Blackhole`
  and `HalfOpen`, Get/CAS fail within `statement_timeout + ε` with an error that is not `ErrConflict`. A stale
  pooled connection after `HalfOpen` heals is recycled without a hang.
- **Size.** M.

#### RI-10 — Handshaking end to end — **Done** (629193e; queue-drain Retry-After estimate deferred to RI-11 metrics; client wiring — `ServerHint` from the response and an `unavailable` code in `internal/outcome` — lands with control-plane CP-6)

- **Change.**
  - `Retry-After` becomes computed: breaker time-to-half-open, queue-drain estimate, or 1 s floor.
  - `retry.Policy` defaults become exponential backoff, full jitter, base 50 ms, max 5 s, `MaxAttempts` 3.
    The `none` modes stay for the bad-client bench profiles, which set them explicitly.
  - `Decide` takes `ServerHint time.Duration` and waits `max(hint, delay)`.
  - `Budget` becomes a token bucket refilled at `Ratio` per first attempt, capped (e.g. 10 + 0.1 × recent
    rate), so banked credit expires.
- **Acceptance.**
  - Spec table rows for the hint.
  - `FuzzDelayBounds` extended.
  - The `retry-storm` config rerun: the good profile's retries/first ≤ ratio over every 1 s window (today only
    over the whole run).
- **Size.** M.

#### RI-16 — Runtime controls and admin CLI

- **Goal.** Ch16 (component-level control without a deploy) and ch14 (CLI first).
- **Change.**
  - The lifecycle listener gains `POST /admin/{breaker/s3:trip|reset, fetch:limit=N, drain, passivate?key=,
    loglevel}`, with control-plane identity only.
  - `cmd/agentctl` is a thin scriptable client.
  - S3 keys and the Postgres DSN move to a separate 0600 secrets file outside the config (ch12). Admin binds
    only to `admin_listen`.
- **Acceptance.** e2e: lower `fetch:limit` at runtime, and the next fetches respect it. Trip the breaker, and
  requests get 503 with no S3 calls.
- **Size.** M.

#### RI-15 — Longevity and capacity

- **Change.**
  - A 72 h soak (frontbench through agentd with S3 durable mode) at moderate load, with nightly idle hours to
    surface idle-timeout and leak bugs (ch3 sidebar).
  - Track fds, RSS, goroutines, cache-dir bytes, bucket object count and `oldest_dirty_age` (all flat at
    steady state).
  - Capacity: find the one constraint per workload (ch8) for fetch-heavy, write-back-heavy and hot-key mixes.
  - Write the per-feature SLA table (ch13), e.g. warm read, cold activation and write-back RPO, with their
    dependencies multiplied out (ch4 §4.10).
- **Acceptance.** A results note in `knowledge/sqlbench/` or `knowledge/writeback/`. Every steady-state gauge
  has slope ≈ 0 after the first hour.
- **Size.** L.

**Decision this plan surfaces (needs the user).** STATE "Next (a)", the WAL-shipping output gate, would put an
S3 PUT on the write-ack path. That is SLA inversion (ch4 §4.10): write availability ≤ S3 PUT availability ×
agent availability. If adopted, it needs a stated degraded mode (refuse writes vs. ack locally with a larger
RPO) behind the RI-03 breaker. That is a business decision (ch5 §5.2).

## 5. Testing implications

All of this follows `knowledge/testing/README.md`: pure decisions get spec tables and model fuzzing, adapters
get contract suites run against fake and real, and fakes have fault switches. Release It! adds two things.
**Failures below the interface** (RI-13 harness, because ch5 §5.7 says mocks can't produce them) and
**failures under load and over time** (RI-15).

| Item | New tests | Level |
|---|---|---|
| RI-13 | Per-mode self-tests; `RunStoreSuite` through the proxy with no faults | integration |
| RI-01 | `HeadersThenStall`, `Dribble`, `Status(503)×N` request count, `TruncateBody` → no `.part` | integration (fake S3), gated SeaweedFS rerun |
| RI-14 | Lost-reply-as-stale switch in `durabletest`; `FuzzDurableHostModel` op | unit + fuzz |
| RI-08 | ctx deadline interrupts `Do`; actor free afterward | unit (real SQLite) |
| RI-02 | Semaphore spec table; activation timeout; shed beyond queue; coldload c=64 rerun | unit + bench |
| RI-03 | `FuzzBreakerModel`; outage call-count bound; heal time | unit + fuzz + integration |
| RI-04 | Wire-code table rows; e2e S3-down: 0 `internal` | unit + e2e |
| RI-05 | Hung job doesn't stop ticks; GC spec table; paged list memory | unit + integration |
| RI-06 | Close honors deadline; SIGTERM budget e2e | unit + e2e |
| RI-07 | Serve-while-reconciling; readyz on wedged actor | unit + e2e (CP-1 gate) |
| RI-09 | Slow body cut; limiter VM-cap model fuzz | unit + fuzz |
| RI-10 | Hint table; windowed budget; `retry-storm` rerun | unit + bench |
| RI-11 | Metric catalog scrape after fault script | integration |
| RI-12 | `TestPlacementCAS` via `faultnet` Blackhole/HalfOpen | integration (env-gated Postgres) |
| RI-15 | 72 h soak with idle hours; per-workload constraint | load (sqlbench/frontbench/wbbench) |

Rule for every new integration point (guide §H): the adapter's contract suite runs three ways, against the
fake, against the real service, and against the real service through `faultnet`, and one test per row of the
guide's failure table (refused, silent, stall, slow, 5xx, lost reply) must exist before the adapter merges.

## 6. Proposed guide

See [guide.md](guide.md). It should be linked from `knowledge/index.md` and from the repo-wide coding-style
summary. Small additions to existing docs:

- `knowledge/testing/README.md` checklist: "adapter suite also runs through `faultnet`".
- `~/AI/writeups/go-coding-style.md` §6: "every ctx passed to I/O has a deadline".

## 7. Out of scope / not adopted

- **J2EE/JVM specifics** (ch9 §9.2 JSP permgen, ch10 §10.4 GC generations, `SoftReference`, JMX/SNMP/CIM in
  ch17). The Go equivalents are a bounded cache size, `GOMEMLIMIT`, and a Prometheus-text `/metrics`.
  Nothing more.
- **Sessions, cookies, AJAX, HTML bloat, the reload button** (ch7, ch9 §9.3–9.6, §9.10). There are no browser
  sessions; the stateless ticket replaces them. The idea that carries over (admission control for hostile
  traffic) is RI-09 and CP step 7's edge rate limit.
- **Decoupling middleware as a product** (ch5 §5.8 MQ/tuple spaces). Write-back is already store-and-forward.
  We don't add a broker. The book's own "decide early vs. last responsible moment" contradiction is noted in
  the summary.
- **Hardware load balancers, cluster servers and VIP failover** (ch11, ch13). The locator plus epochs play
  that role. The useful residue is "expect mid-request errors and lost replies", which the ambiguity handling
  already covers.
- **Integration databases, expand/contract migrations** (ch18). Relevant to the Postgres schema later. Adopt
  then, with a schema-version check at controld startup (ch18 "schema version table"). Not part of this plan.
- **OpsDB** (ch17). Use the metrics backend the deployment provides, and keep thresholds outside the code
  (ch17 "exoskeleton").
- **Linear availability arithmetic** (ch13 cost of each nine). We do the per-feature dependency product
  (RI-15), not a revenue model; this is not a product with customers yet.
