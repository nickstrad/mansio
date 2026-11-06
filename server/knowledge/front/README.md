# front: the agent's network front (tickets, admission, executor port)

Owner: front agent (state: `docs/state_files/STATE-front.md`). Code: `internal/ticket` (+ `tickettest`),
`internal/front` (+ `fronttest`), `cmd/agentd`, `cmd/frontbench`, `configs/agentd/`, `scripts/front/bench.sh`,
small additive `internal/actor` change (`Request.MaxRows`, `Result.Columns/Values`, `ErrTooManyRows`).

Question it answers: what does it cost to put actor-per-database SQLite behind a network front with option 4's
tickets and admission, and what shape must the front have so fencing's `owner` and a handler runtime plug in?

Notes:
- [2026-11-05 wire contract](2026-11-05-wire-contract.md): request, response, the ten codes with HTTP statuses
  and Retry-After, health, shutdown. **Internal, trusted-caller protocol** (raw SQL); `op` is reserved.
- [2026-11-05 handler executor](2026-11-05-handler-executor.md): what a TypeScript handler executor needs from
  the `Executor` port (transaction scope, synchronous db calls on the actor goroutine, savepoint per handler).
- [2026-11-05 request ids](2026-11-05-request-ids.md) (CP-2): a write's `request_id` applies at most once per
  database; a retry is answered `ok` with the original count and `"duplicate": true`. Resolves `ambiguous`.
- [2026-11-05 agentd durable mode](2026-11-05-agentd-durable-mode.md) (CP-1): the wiring of vactor + owner + S3,
  the lifecycle endpoints (Claim, Fence, Forget, Health), the secrets file, and the CP-1 gate results.
- [2026-11-05 agentd admin controls](2026-11-05-agentd-admin-controls.md) (CP-4, RI-16): `admin_listen` (loopback
  only), breaker trip/reset, fetch limit, drain, passivate, log level; `cmd/agentctl`; gate results.
- [2026-11-05 locator follow-ups](2026-11-05-locator-followups.md): `moving` until a new owner's claim lands (0005
  NOTIFY), controld TLS, unknown-key rate limiting, edge routes from controld's registry, `fsx`/`logx` helpers.
- [2026-11-05 edge](2026-11-05-edge.md) (CP-7): the stateless edge (`internal/edge`, `cmd/edge`): ticket filter
  (cache, per-source rate limit on misses, bounded verifies), routing on the vm claim, per-agent bulkhead and
  breaker, TLS on both hops with a private CA; gate results and overhead numbers.
- [2026-11-05 client](2026-11-05-client.md) (CP-6): `internal/client` (locator-routed, Retry-After + budget, one request id per
  write) and `frontbench locator`; 0 failures in 140k requests overlapping moves; resolve hit 0.2 µs, miss ~0.7 ms.
- [2026-11-05 locator](2026-11-05-locator.md) (CP-5, PG-10): resolve API, notified cache, API keys, rotating
  ticket key ring, agentd key fetch; gate results.
- [2026-11-05 metrics](2026-11-05-metrics.md): `/metrics` catalog, outcome classes, message codes, the shed
  Retry-After estimate (RI-11).
- Raw numbers: [2026-11-05-matrix/](2026-11-05-matrix/) (`matrix.txt`, `micro.txt`, one JSON per run).

## Findings (newest first)

### 2026-11-05 (CP-7): the edge adds one loopback round trip; a forged-ticket flood costs its own source

[Edge note](2026-11-05-edge.md): at conc 4 the edge adds ~70 µs p50 (plain) and ~110–135 µs (TLS h2); on one
shared 18-core host it costs ~35–40 % of peak throughput (a second HTTP server on the same cores). A forged-ticket
flood from the same IP as valid traffic: 297,818 forged requests, 180 verified, valid traffic kept 91 % and all
`ok`. Ticket cache hit 32 ns, full verify 26 µs.

### 2026-11-05 (CP-8): admission does not need sharding by tenant

[Profile](2026-11-05-admission-profile.md): in `frontbench -mode pipeline` (194k req/s, conc 512) `Admission` is
~1–2 % of mutex delay and absent from the CPU profile; ~51 % of mutex delay is SQLite's shared page-cache mutex
(follow-up). Revisit above ~10 %.

### 2026-11-05: the front costs ~7% in process; HTTP costs ~7× on one machine

Machine: Apple M5 Max, 18 cores, 128 GB, macOS, Go 1.26.4, `modernc.org/sqlite` v1.61.1. Date 2026-11-05.
Command: `scripts/front/bench.sh` under `scripts/lock.sh results/.bench-lock` (= `make front-bench`): 16 databases
from `frontbench seed` (sqlbench's schema generator, seed 1, default options), 64 closed-loop clients, 2 s
warmup, 10 s measured, uniform over databases, 4 tenants. Point reads (`SELECT * FROM t WHERE id = ?`, rows
returned) or inserts (`INSERT INTO t (...) VALUES (...)`). Default actor config (WAL, synchronous NORMAL,
mailbox 256, batch 64, no read pool). Client and server run on the **same machine**, so the HTTP rows include
the client's HTTP and JSON work on the same cores.

| mode (what is in the path) | reads ok/s | p50 / p99 / p99.9 µs | inserts ok/s | p50 / p99 / p99.9 µs |
|---|---|---|---|---|
| `inproc`: actor Submit/Wait (= `actor.Do`) + row collection | 331,146 | 134 / 833 / 1,209 | 81,069 | 234 / 9,192 / 14,084 |
| `pipeline`: + JSON encode/decode, ticket (cached), Admitter, admission | 309,350 (−7%) | 136 / 936 / 1,418 | 65,142 (−20%) | 246 / 13,101 / 18,171 |
| `http` h1: agentd over loopback HTTP/1.1 | 46,426 (−86%) | 1,152 / 4,592 / 6,063 | 30,741 (−62%) | 1,041 / 20,300 / 29,350 |
| `http` h2c: agentd over loopback HTTP/2 cleartext | 32,954 | 1,934 / 2,512 / 2,835 | 19,228 | 3,058 / 12,798 / 21,202 |

Per-request overhead, micro benchmarks (`micro.txt`; `go test -bench`, parallel ones on 18 cores):

| piece | cost |
|---|---|
| Ed25519 ticket verify (one core) | 26.3 µs |
| verified-ticket cache hit (parallel) | 0.053 µs |
| request JSON decode + validation + SQL guard | 0.90 µs, 19 allocs |
| response JSON encode (one row) | 0.30 µs |
| admission acquire + release, one hot key (parallel, one mutex) | 0.29 µs |
| whole pipeline, no-op executor, full verify each time (parallel) | 4.0 µs/op |
| whole pipeline, no-op executor, cached ticket (parallel) | 1.8 µs/op |
| loopback HTTP round trip incl. client, no-op executor, h1 / h2c (parallel) | 37.7 / 20.7 µs/op |

What this says (measured unless marked):

1. **The pipeline itself is cheap**: ~1.8 µs/request of CPU with a cached ticket, −7% read throughput versus
   calling the actor directly. Writes lose more (−20%) because the pipeline's goroutine work competes with the
   actors' commits on the same cores and smaller group-commit batches result (guess; batch sizes not recorded).
2. **Ticket verification is the one expensive step**: 26 µs of Ed25519 per request would cost more than the
   SQLite read (~3–6 µs). The cache (keyed by the exact token, re-checking every rule except the signature, and
   dropped when its kid leaves the key ring) makes it 53 ns. Clients reuse one ticket for its whole TTL (option
   4), so the hit rate is ~100% in steady state; a cold start or an attacker sending fresh garbage pays 26 µs
   each (bounded per core; see risks).
3. **HTTP is the dominant cost**: ~20–38 µs of CPU per request across client and server on loopback, ~10×
   the pipeline. With both ends on one machine the h1 run is CPU-bound in `net/http`, not in SQLite.
4. **h2c was slower than h1 here** (33k vs 46k reads/s): Go's client multiplexes all 64 clients over one TCP
   connection, which serializes on that connection's framing. The no-op micro benchmark (one server, many
   parallel clients) shows the opposite (20.7 vs 37.7 µs/op), so h2 wins on CPU and loses on single-connection
   contention. Not measured: several h2 connections, or a real network.
5. Compared with Cloudflare's documented ~1,000 req/s soft limit per Durable Object, one database behind this
   front on loopback serves far more; the front is not the per-database limit, the transport is the per-VM one.

Not measured yet: a real network between client and agent (RTT dominates there), TLS, more databases than
cores, a slow-query mix through the front, the separate health listener under saturation, Linux.

### 2026-11-05: what the tests found

- **FuzzVerify, first 30 s run:** Go's base64 decoder skips `\r` and `\n`, so a valid ticket with a CR inside
  also verified. Not a forgery (same signed bytes), but one ticket had many spellings, each a cache miss that
  costs a 26 µs signature check. Fix: refuse any byte outside the base64url alphabet before decoding
  (`ticket.split`); the input is kept as a regression in `internal/ticket/testdata/fuzz/FuzzVerify/`.
- Fuzz totals (2026-11-05, under the bench lock): FuzzVerify ~11–17M execs per 30 s, FuzzDecodeRequest 1.2M,
  FuzzLimiterModel 0.64M; no other failures. Coverage of `internal/front` + `internal/ticket` from their tests:
  97.6% of statements (uncovered: the panic recovery outside `Handle`, a body read error, SQLITE_BUSY mapping).

## Design in one paragraph

Pure domain (`ticket` verify and encoding, `front` request decoder + SQL guard, the limiter, the wire mapping)
with ports `KeySet`, `Clock`, `Admitter`, `Executor`, `Host`; thin adapters (`front/http.go`,
`front/executor.go` over `internal/actor`, `cmd/agentd`). Tickets are a compact versioned binary encoding
signed with Ed25519 (no JWT: no `alg` to confuse, signature checked before claims are decoded, canonical
decoding, ~200 bytes, stdlib only). Admission caps in-flight and queued requests per tenant and per database,
FIFO per tenant, sheds exactly when a request cannot run and a queue is full, and drops waiters at their
deadline; deadlines come from the client (`X-Timeout-Ms`) and flow to the actor as its queue deadline and
statement timeout. Every outcome maps through one table to the ten wire codes.

## How the other agents plug in

- **fencing** (`internal/owner`): `front.Admitter` is `Admit(db, epoch) (release func(), error)`. Adapter in
  `cmd/agentd` (shared_state.md 2026-11-05 "Admitter adapter"): `Permit.Done` is the release;
  `ErrWrongOwner` → `front.ErrWrongOwner`; `ErrFenced`/`ErrMoving` → `front.ErrFenced`; `ErrUnavailable` →
  `front.ErrShed`. Replace the `front.StaticAdmitter` built by `epochs()` in `cmd/agentd/main.go` with it.
- **writeback** (`internal/vactor`): either implement `front.Host` (`Lookup(ctx, db) (*actor.Actor, error)`) over
  `GetAt` with the actor behind `actordb`, or implement `front.Executor` directly (the `Call` carries `Epoch` and
  `Gen`), and run `fronttest.RunExecutorSuite` against it. Replace `openHost` in `cmd/agentd`.
- **handler runtime** (later): a new `front.Executor`; see the handler-executor note.

## Risks and open questions

- Raw SQL from a trusted caller: the SQL guard is a heuristic. A read is not proven read-only (no
  `sqlite3_stmt_readonly` through database/sql). In production the compiler is the boundary; never expose this
  protocol to clients.
- An unauthenticated flood of distinct garbage tickets costs 26 µs each at most (signature check), and nothing
  rate-limits it yet. Connection and request-rate caps per source belong in the edge or the front (option 4 §5.2).
- `ambiguous` writes: resolved by request ids since CP-2 ([note](2026-11-05-request-ids.md)); retry with the same
  `request_id` within `RequestTTL` (24 h).
- One admission mutex per agent: 0.29 µs per acquire+release under full contention; shard by tenant if it
  shows up.
- No TLS; option 4 recommends a stateless edge in front, which terminates it.
