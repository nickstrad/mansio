# Coding style for mansio-server

Status: adopted · 2026-11-05 · Read this before planning or writing code. It is the short version. Where a
section links to a fuller doc, that doc wins on detail.

| Topic              | Full doc                                                                               | Covers                                                                   |
| ------------------ | -------------------------------------------------------------------------------------- | ------------------------------------------------------------------------ |
| House Go style     | `~/AI/writeups/go-coding-style.md`                                                     | layout, domain vs integration, contracts, errors, logging, concurrency   |
| Testing            | [docs/notes/testing/README.md](notes/testing/README.md), `~/AI/writeups/go-testing.md` | test levels, fakes, contract suites, model fuzzing                       |
| Concurrency        | [docs/notes/concurrency-in-go/README.md](notes/concurrency-in-go/README.md)            | Result pattern, panic forwarding, error propagation                      |
| Integration points | §5 below                                                                               | timeouts, retries, bulkheads, breakers, health, per-dependency defaults  |
| Object storage     | [object-storage.md](object-storage.md)                                                 | key layout, conditional writes, ambiguity, integrity, GC, backend checks |
| Postgres           | [postgres.md](postgres.md)                                                             | migrations, schema conventions, CAS queries, pool settings and budgets   |

## 1. Structure: domain vs integration

- **Domain packages decide; they do no I/O and do not log.** No `net/http`, `os`, `database/sql`, minio, or
  `slog` in them. They reach the world through small interfaces they define (ports).
- **Decisions are pure functions.** `plan(have, want) → moves`, `durable.CheckPublish(prev, next)`,
  `durable.PlanGC(...)`, `bench/plan.go`. A thin layer around them does the I/O. Test the pure part with
  tables.
- **Adapters are thin.** `objstore` (S3), the Postgres placement adapter, `cmd/*` adapters: translate,
  validate, wrap errors, log. No business rules.
- **Wire only in `cmd/`.** `main` calls `realMain(ctx, args, stdout, stderr) int`. Exit 0 ok, 1 expected error
  with log ID, 2 usage, 70 bug, 130 interrupted.
- **Generated SQL lives in pure packages** that return text + args (`internal/schema`, `internal/workload`).
  Standalone SQL lives in `.sql` files, embedded with `//go:embed`.
- No package-level mutable state; no `utils`/`models` packages; a `doc.go` names each package's role and error
  module.

Full: `go-coding-style.md` §1–2.

## 2. Validation vs contracts

- **Validate once at the edge** (config loaders, handlers, adapters) and return errors. **Never assert on
  input.**
- **Contracts are for our own bugs**: `assert.That` (`internal/assert`) panics. Write them in doc comments:
  `pre:`, `post:`, `Invariant:`. Run `invariant()` after mutations, never on reads; guard O(n) checks with
  `if assert.Enabled`.
- **Checked wrappers** on interfaces any future adapter could break (e.g. `vactor.checkedFetcher`).
- Never put assertions in a `defer` that also runs while a panic unwinds.
- Each assertion must be able to fail in some test. Both builds pass: default and `-tags noassert`.

Full: `go-coding-style.md` §3, `docs/notes/testing/README.md` §2.

## 3. Errors and logging

- **Wrap once at the package boundary**:
  `errs.Wrap(module, err, "what failed; what changed; whether retry is safe")`. `Error()` is `message: cause`;
  never `%v` the cause into your message.
- **Sentinels with `errors.Is`, typed errors with `errors.As`.** Never branch on message text. Classify
  storage errors with `objstore.Classify` (Transient / Precondition / NotFound / Permanent / Canceled).
- **Three error classes, never mixed**: dependency unavailable → `unavailable` (503 + `Retry-After`), WARN,
  sampled; bug → `internal` + log ID, ERROR; bad input → `400`. An S3 or Postgres outage is never `internal`:
  that would page on-call with thousands of "bugs", and clients would not back off, because a 500 carries no
  `Retry-After`.
- **Log once, at the top or in the adapter**: `logx` JSON, UTC, log ID, whole chain, origin stack. Do not log
  and return. One stable message code per operational event (`S3.BREAKER_OPEN`, `OWNER.SUPERSEDED`).

Full: `go-coding-style.md` §4–5, `concurrency-in-go/README.md`, §5 below.

## 4. Concurrency

- Goroutines report `{result, err, panic}` to the parent; the parent sets the policy.
- Whoever starts a goroutine makes sure it can stop: ctx, result channels buffered to the sender count, one
  closer, drain every result after the first error.
- Recover panics inside goroutines and re-panic in the parent.
- **No slow I/O while holding a lock the request path takes** (vactor activates outside `h.mu`).
- **Waiting is not working**: if a caller gives up, the work it started is interrupted or bounded too.
- **No wall clock, global rand or fresh ids in simulated packages** (the list in `internal/det/lint_test.go`):
  take a `det.Clock` (also for ctx deadlines: `det.WithTimeout`), `det.Rand` and `det.IDGen`, defaulting to
  `det.System()` / `det.SystemRand()` / `det.SystemIDs`; `go test ./internal/det` enforces it (DST-0).

Full: `go-coding-style.md` §6, `concurrency-in-go/README.md`.

## 5. Integration points: timeouts, retries, backpressure

Every call that leaves the process (S3, Postgres, HTTP in or out, the issuer key set, the schema registry,
local fsync) and every queue follows these rules. Integration points are where failures start, and a missing
timeout is how they spread: a dependency's hang becomes ours, and a slow answer is worse than a refusal.

| Point                                | Code                           | On the request path?                                                                     | Who retries                             |
| ------------------------------------ | ------------------------------ | ---------------------------------------------------------------------------------------- | --------------------------------------- |
| S3                                   | `objstore` via `durable.Store` | cold activation only; write-back is off the path                                         | `objstore` (idempotent requests only)   |
| Postgres                             | `placement/pg`                 | locator cache miss, tenancy credential miss; Claim, Reconcile and moves are off the path | the caller, after a re-read             |
| Inbound HTTP                         | `front`, `cmd/agentd`, `edge`  | yes                                                                                      | the client                              |
| Client → locator, agent              | `client`, `retry`              | yes                                                                                      | the client, the one retry layer         |
| Edge → agent                         | `edge`                         | yes                                                                                      | nobody: the edge never retries          |
| Agent → issuer keys, schema registry | `keyfetch`, `schemareg`        | registry: a database's first request per schema                                          | keys: the next refresh; schemas: nobody |
| Actor mailbox, read pool             | `actor`                        | yes                                                                                      | nobody: a full queue sheds              |

A new integration point is a design change: add a row here and to the defaults table below.

**Timeouts**

- **Every remote call has a deadline, background work included.** `context.Background()`, `h.ctx` and ticker
  contexts are roots, not budgets: derive a per-call deadline from them. (A maintenance tick once ran a PUT
  under the long-lived maintenance ctx, and one hung PUT stopped all maintenance.)
- **Bound transfers by time as well as size**: a deadline of `OpTimeout + size / MinThroughput` plus a stall
  timeout. A response-header timeout does not protect a body that stops halfway.
- **Deadlines shrink on the way down.** An inner timeout is less than the outer one minus the work left; never
  retry past the caller's deadline. The client sends its budget as `X-Timeout-Ms`; the edge forwards it
  shrunk.
- **Waiting is not working.** When a caller gives up, the work it started is interrupted or bounded too
  (`actor.Do` turns the ctx deadline into the statement timeout).
- **Every pool checkout and semaphore acquire takes the ctx.** No unbounded blocking acquire.

**Retries**

- **One retry layer per call chain, at the layer that understands the outcome.** Library retries are off
  (`minio.Options{MaxRetries: 1}`, no driver auto-retry). Layered retries multiply: minio-go's default of ten
  tries, under our own upload tries, under client retries, turns one failure into hundreds of requests.
- **Retry only what is safe.** Writes are never blindly retried: an ambiguous write is resolved by re-reading
  ([object-storage.md](object-storage.md#errors-retries-and-ambiguous-outcomes)). Retried client writes carry
  the same request id, so they apply at most once.
- **Exponential backoff with full jitter, a cap on attempts, and `Retry-After` honoured**: wait
  `max(server hint, jittered delay)`. The `internal/retry` defaults are safe (3 attempts, 50 ms base, 5 s
  cap); "no backoff" exists only for the bad-client bench profiles, which set it explicitly.
- **Retries spend a windowed budget**: a token bucket refilled per first attempt and capped, so credit saved
  in a quiet hour cannot pay for a storm.
- **Prefer "queue and retry later" to "retry now".** Write-back is the model: acks do not wait for S3, and a
  failed write-back stays dirty for the next tick.
- **A failed key backs off on its own.** After a failed activation the key holds a short jittered tombstone
  (from 100 ms, doubling); requests in that window fail fast instead of fetching again.

**Bulkheads and backpressure**

- **Every queue is bounded, and a full queue sheds; it never blocks the caller.** Mailbox, read queue,
  admission queues, fetch queue.
- **Each dependency has its own concurrency limit, sized to its measured knee**, so one slow dependency cannot
  take slots another needs. S3 fetches: 16 at once and 1 GiB in flight, 256 waiting (past the knee, total
  throughput falls: 132 MiB fetches ran at 253 MB/s one at a time and 134 MB/s at 64). Postgres: callers are
  budgeted below the pool ([postgres.md](postgres.md#connections-and-pool-budgeting)).
- **Caps nest: database ≤ tenant ≤ VM.** Without the VM cap, many tenants could exceed what the process holds.
- **Reserve capacity for control.** Health, metrics and lifecycle calls (Claim, Fence) use the lifecycle
  listener and never queue behind queries; runtime controls are on the loopback-only `admin_listen`.
- **No slow I/O while holding a lock the request path takes** (vactor activates outside `h.mu`).
- **Bound every result set and listing**: `MaxRows` on reads, a size limit on manifests, paged object listing.

**Circuit breakers** (`internal/breaker`)

- **One breaker per remote dependency**: S3, the placement and tenancy pools, the schema registry, each agent
  at the edge and at controld. By default it opens when at least 50 % of at least 20 calls in a 10 s window
  failed, stays open 5 s, then lets one probe through.
- **Only system failures count**: timeouts, connection errors, 5xx. `ErrNotFound`, `ErrStale`, `ErrConflict`
  and bad input are answers.
- **Open means fail fast** with `ErrUnavailable` and a retry hint (the time to the probe).
- **Breakers are visible and operable**: every transition is logged with a message code and exported as a
  metric; `mansioctl agent breaker s3 trip|reset` holds one open or closes it.
- **What an open breaker means for users is a decision, written down.** S3 open: cold activations answer
  `unavailable` (503 + `Retry-After`), warm traffic is unaffected, write-backs wait (the RPO grows; alert on
  `oldest_dirty_age`), and readiness stays up. An agent is not pulled from the load balancer for a remote
  dependency's outage, or one S3 outage would drain every agent.

**Fail fast**

- **Check what you need before expensive work**: ticket, ownership, breaker state and queue room are checked
  before SQLite or S3 is touched.
- **Work that cannot finish in time is refused, not started**: expired queued requests are answered `expired`
  unrun.
- Error classes on the wire are in §3: a dependency outage is `unavailable`, never `internal`.

**Steady state**

- **Everything that accumulates has a reaper at least as fast**: generations → GC, cache dir → eviction,
  fenced records → `Forget`, logs → rotation outside the process.
- **Background loops cannot stall**: each job has its own deadline, a tick never overlaps the next, and a
  stuck tick is reported.
- **Track the age of the oldest unfinished thing** (`oldest_dirty_age`, oldest queued request). A flat line is
  the health signal.

**Health and lifecycle**

- **Startup: validate, open, then serve each key as soon as it is reconciled.** Reconcile runs with bounded
  workers and a per-key timeout; unreconciled keys are refused with `unavailable` meanwhile. Postgres pools
  connect lazily, so startup does not block on them.
- **Liveness is not readiness, and readiness exercises the real path**: `/readyz` runs a canary request
  through admission and an actor. Remote breakers and the S3 probe (a manifest read every `ready_interval`)
  are metrics and alerts, not readiness.
- **Shutdown has one budget** (`shutdown_budget`, 60 s): drain the front, write back the oldest-dirty
  databases first until the budget ends, then close with local copies kept and their count logged.

**Observability**

- **Per integration point**: requests by outcome class (`ok`, `app_error`, `timeout`, `refused`, `5xx`,
  `ambiguous`); a latency histogram in which requests that never finished count at their deadline (averages
  hide hung requests); in-flight and its high-water mark; breaker state and transitions; last success. Per
  pool or queue: size, in use, high water, times full.
- **Export counters; keep thresholds out of the code.** Alert rules live in `deploy/prometheus/`.
- **Log for operators.** ERROR means someone must act (a long-open breaker, write-back failing past the RPO, a
  bug); WARN means degraded and self-healing. One stable message code per event; correlation fields
  `request_id`, `log_id`, database key, epoch. Log state transitions, not every refusal; count refusals.

**Defaults per dependency.** The `Config` types are the reference; these are the values to keep in mind.

| Dependency                               | Deadlines                                                     | Concurrency                                                           | Other                                                                                   |
| ---------------------------------------- | ------------------------------------------------------------- | --------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| S3 (`objstore.Config`, `vactor.Options`) | op 5 s, stall 10 s, transfer + size ÷ 32 MiB/s, dial 5 s      | fetches 16 / 1 GiB, queue 256                                         | breaker; GET/HEAD/LIST/DELETE tried ≤ 5 times                                           |
| Postgres (`pg.Config`)                   | connect 3 s, statement 2 s, lock 1 s, idle-in-transaction 5 s | one pool per process, callers budgeted below it                       | idle connections closed after 60 s; breaker per pool                                    |
| Inbound HTTP (agentd, edge)              | header 5 s, body 5 s, write = request deadline + grace        | admission per database, tenant and VM; edge: 4096 in flight per agent | 16 KiB headers; edge rate limit 20/s, burst 50 per source                               |
| Client (`client`, `retry`)               | call 5 s, resolve 3 s, dial 3 s                               | —                                                                     | 3 attempts, full jitter, windowed budget                                                |
| Locator (controld)                       | 3 s per resolve                                               | 256 resolves, 10 store loads, 2 creates                               | cache 100 000 entries, 5 min TTL, trusted only while LISTEN is up                       |
| Issuer keys, schema registry (agentd)    | 3 s per fetch; keys refreshed every 30 s                      | schema fetches: 16 at once, single-flight                             | a failed key fetch keeps the old ring; schema text is re-hashed against its fingerprint |
| Actor                                    | per-request deadline and statement timeout                    | mailbox 256, read queue the same                                      | a full queue sheds                                                                      |

**Open questions**

- A long soak (days, with idle hours to surface idle-timeout and leak bugs) has not been run; the harness is
  in `scripts/soak/`. Neither has a per-feature availability table (warm read, cold activation, write-back
  RPO) with each feature's dependencies multiplied out.
- WAL shipping would put S3 on the write-ack path; see [object-storage.md](object-storage.md#open-questions).

## 6. Object storage

Full: [object-storage.md](object-storage.md).

- **One database owns one prefix**, built only by `durable.KeyPrefix(key)`. Compute names; **no LIST on a
  request path** (only GC, `dbtool`, setup).
- **Everything except `manifest.json` is create-only.** Never reuse a tried number. Write data first, publish
  the pointer second, trust only the pointer. Objects carry the metadata to rebuild the pointer.
- **Every write to the manifest is conditional and carries the fencing epoch**; `(epoch, generation, seal)`
  only rises. 412 and 409 both mean stale. **No clocks in correctness.** Coordination lives in Postgres.
- **After an ambiguous write, re-read before deciding**; go stale only when the re-read shows another writer.
- **Verify size and SHA-256 on every generation read**; `Content-MD5` on every upload.
- **GC never deletes the current, pending or retained generations**, deletes orphans only after a grace
  window, never writes the manifest and never crosses a prefix. Drop = placement tombstone first, then the
  prefix.
- **Run `TestS3Compat` against every S3 backend and version before use.**

## 7. SQL

These hold for SQLite (the data plane), DuckDB (benchmark analysis, `queries/`) and Postgres (the control
plane). Postgres specifics: [postgres.md](postgres.md).

- **Values are always bind parameters**, so a value can never be parsed as SQL and identical text reuses the
  actor's prepared-statement cache. Formatted identifiers come only from the generators' own word lists, never
  from requests.
- **Build connection strings in one place** (`internal/sqlitedsn` for SQLite `file:` URIs; `pg.Config` for
  Postgres). A hand-built `file:` URI opens a different file when the path holds `#` or `?`.
- **The connection owner owns the transaction**; callers never send `BEGIN`, `SAVEPOINT` or `PRAGMA`. The
  actor owns `BEGIN IMMEDIATE … COMMIT`.
- **Know what an error does to the enclosing transaction.** In Postgres any error aborts it; in SQLite
  `OR ROLLBACK`, `RAISE(ROLLBACK)` and I/O errors end it, and later statements then silently autocommit. The
  actor wraps each write of a group commit in `SAVEPOINT`/`RELEASE`; if `ROLLBACK TO` fails, the transaction
  is gone, and the rest of the batch is not run.
- **Upserts use `on conflict`**, never "select, then insert" (the two-step form races).
- **Invariants live in the schema**: `not null`, `check`, keys, foreign keys with the referencing side
  indexed. **SQLite** enforces foreign keys only with `PRAGMA foreign_keys = ON` (every `sqlitedsn` connection
  sets it) and column types only in `STRICT` tables; repo-owned tables are `STRICT`, and `WITHOUT ROWID` for
  natural text keys. SQLite times are `integer` unix milliseconds UTC, named `*_ms`.
- **No hot counter rows**: compute aggregates with `group by`; a counter every write updates serializes
  writers.
- **Set-based over loops**: one query returns what the caller needs; bulk work is `insert … select` or
  multi-row `values` in one transaction.
- **Layout**: one clause per line for multi-line SQL, explicit joins (no `natural join`), no positional
  `order by 2`, explicit insert column lists. Comments explain intent. Existing uppercase SQL stays uppercase;
  new Postgres files are lowercase.
- **DuckDB reads benchmark CSVs through one typed view file** (`queries/views.sql`, explicit columns, no type
  sniffing); a test checks it against the CSV header the Go writer emits.
- **Migrations**: numbered, transactional files, never edited after they are applied.

## 8. Testing (what every change needs)

Full: [docs/notes/testing/README.md](notes/testing/README.md), `~/AI/writeups/go-testing.md`.

1. **Spec tables** per partition with on/off points of every boundary.
2. **Structural**: read uncovered blocks; error branch → inject with a fake; impossible → assertion; dead →
   delete.
3. **Fakes with fault switches** in `<pkg>test` packages; **one contract suite run against every
   implementation**. For adapters that means three runs: the fake, the real service, and the real service
   through a fault harness (`internal/faultnet` for TCP, `internal/objstore/s3test` for HTTP/S3), because an
   interface-level fake cannot find a missing socket timeout. Minimum faults per adapter:

   | Fault                     | Harness                                             | Expected                                       |
   | ------------------------- | --------------------------------------------------- | ---------------------------------------------- |
   | connection refused        | `faultnet` `Refuse`                                 | fails fast, `unavailable`                      |
   | accepted, no reply        | `AcceptThenSilence`, `Blackhole`                    | fails at the op timeout                        |
   | headers, then stall       | `s3test` `HeadersThenStall`                         | fails at the stall timeout, no partial file    |
   | 1 byte/s                  | `Dribble`, `Throttle`                               | fails at the size-derived deadline             |
   | 5xx or throttling         | `Status` (N times)                                  | exactly one retry layer; the breaker counts it |
   | applied, reply lost       | `ApplyThenDropReply`, `ApplyThenPreconditionFailed` | resolved by a re-read, not misreported         |
   | truncated or corrupt body | `TruncateBody`, `CorruptBody`                       | `ErrCorrupt`, nothing activated                |
   | half-open after idle      | `HalfOpen`                                          | the stale connection is recycled, no hang      |

4. **Model-based fuzzing** for anything stateful; commit failing inputs to `testdata/fuzz/`.
5. **Adapter integration tests** including failure paths and error wrapping (gated: `COLDLOAD_S3`,
   `MANSIO_PG_URL`).
6. **Concurrency**: `-race`, many workers on the same key for CAS paths.
7. **SQL**: every `.sql` file has a golden-output regression test; each constraint has an off-point test.
8. **Under load and over time**: fault modes during a frontbench/wbbench run before calling a component done.

## 9. Checklist before done

- [ ] Domain code does no I/O and does not log; wiring only in `cmd/`
- [ ] Input validated at the edge; contracts documented and asserted where impactful
- [ ] Errors wrapped once at the boundary; messages say what changed and whether retry is safe; error class is
      right (unavailable / bug / bad input)
- [ ] Every remote call has a deadline; one retry layer; every queue bounded and shedding
- [ ] Object storage: names via `KeyPrefix`, conditional + fenced writes, re-read after ambiguity, verify
      hashes
- [ ] SQL: bind parameters, savepoints around grouped writes, constraints in the schema, golden tests
- [ ] Tests per §8; `gofmt -l .`, `go vet ./...`, `go test -race ./...`, `go test -tags noassert ./...` pass
- [ ] New integration point or storage layout → update §5, [object-storage.md](object-storage.md) or
      [postgres.md](postgres.md), and the matching contract test
