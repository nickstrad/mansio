# Postgres placement adapter: concurrency gate and first CAS latency (CP-3, PG-04)

Date: 2026-11-05 · Code: `internal/placement/pg` · Agent: CP-3 · Refines
[2026-11-05-postgres-placement.md](2026-11-05-postgres-placement.md) (design) with the built adapter.

## What was built

- Schema `internal/placement/pg/schema/0001_placement.sql` (art-of-postgresql plan §5.1, plus named
  constraints and indexes on `moves.from_vm`/`to_vm`), applied by `Placement.Migrate`, recorded in
  `schema_version`. Golden constraint test `schema/test/constraints.sql` + `.expected`.
- One query file per method: `get`, `create`, `cas` (flip + `moves` row in one statement), `moving`,
  `vm_load`, `register_vm`, `invariants`.
- Error map: 0 rows → `owner.ErrConflict`; no row → `owner.ErrNotFound`; trigger/check/unique →
  `pg.ErrInvariant` (a bug); FK on `owner_vm` → `pg.ErrUnknownVM`; timeouts, lock timeout, connection errors,
  open breaker → `pg.ErrUnavailable` (never a sentinel). Bad input → `owner.ErrInvalid` before sending.
- Pool per RI §P: connect 3 s, `statement_timeout` 2 s, `lock_timeout` 1 s,
  `idle_in_transaction_session_timeout` 5 s, client call deadline 2.5 s, TCP keepalive 15 s × 3,
  `MaxConnIdleTime` 60 s, `synchronous_commit = on` forced, `application_name` set. Breaker per pool
  (`Placement.Breaker()`, register as `"placement"`).

## Gate results (all automated, `TSDB_PG_URL` set; `deploy/postgres`, Postgres 17.11 in Docker)

| Gate | Test | Result |
|---|---|---|
| Contract suite vs fake and Postgres | `ownertest.RunPlacementSuite` via `TestPlacementCAS` in `ownertest` and `pg` | pass |
| Lost flip reply with the real adapter | `pg.TestMoveCrashPoints/flip_applied_but_its_reply_was_lost` | pass: Move fails `ErrUnavailable`, the flip committed, resume claims at epoch 2, one `moves` row, writes intact |
| Each constraint rejects its off point | `TestConstraintsGolden` | pass |
| Row held in an open transaction | `TestLockHeldFailsWithinLockTimeout` | CAS fails `ErrUnavailable` after ≈ 1.0 s; Get unaffected |
| Black hole / half-open / refused | `TestBlackholeFailsWithinCallTimeout`, `TestHalfOpenRecyclesTheStaleConnection`, `TestBreakerOpensOnSystemFailuresOnly` | fail within 2.5 s + slack, non-sentinel; next call gets a fresh connection |
| 64 workers, one key, one expected row | `TestConcurrentCASOneKey` (20 rounds) | exactly 1 winner, 63 `ErrConflict`, epoch +1, every round |
| Many-key move race | `TestMoveRaceManyKeys` (64 workers, 16 keys, 40 steps) | invariants query empty; per key the `moves` rows are exactly `from_epoch` 1 … epoch−1 and equal the successful flips (≈ 400 flips) |

## Finding: pgx holds a pool slot for 15 s after a half-open call

When a call's ctx ends, pgx closes the connection asynchronously: cancel request, then a drain under a fixed
15 s deadline. On a half-open connection the drain never sees EOF, so the pool slot stays taken for 15 s
(with `MaxConns 1` every call failed for 15 s). Fix: the dialer wraps connections so any non-zero deadline is
capped at 1 s from now (`closeBudget`), and the cancel request's dial uses the connect timeout. pgx sets
deadlines only for these two cases (pgx v5.11.0 `pgconn.go`: `asyncClose`, the ctx watcher), so normal calls
are unaffected.

## CAS latency (PG-04, SQL G-22)

Apple M5 Max, macOS; Postgres 17.11 in Docker Desktop (18 CPUs, 4 GiB) on loopback; pgx v5.11.0, pool 64;
`TSDB_PG_LATENCY=1 go test -run TestCASLatency ./internal/placement/pg`, 3 s per row, no `-race`. Each worker
loops Get → CAS (gen + 1); latency is the CAS call only (acquire + round trip + commit, fsync on).
"shared" = every worker on one key.

| mode | workers | CAS calls | p50 | p99 | CAS/s | conflicts/s |
|---|---:|---:|---:|---:|---:|---:|
| distinct keys | 1 | 10 831 | 177 µs | 291 µs | 3 610 | 0 |
| distinct keys | 16 | 45 753 | 697 µs | 1.12 ms | 15 251 | 0 |
| distinct keys | 64 | 62 358 | 1.94 ms | 4.32 ms | 20 786 | 0 |
| one key | 1 | 10 647 | 180 µs | 230 µs | 3 549 | 0 |
| one key | 16 | 25 835 | 1.91 ms | 4.51 ms | 8 612 | 7 279 |
| one key | 64 | 21 698 | 5.91 ms | 44.6 ms | 7 233 | 6 420 |

Reading: a CAS costs ≈ 0.2 ms alone; the authority does ~20k CAS/s over distinct keys with 64 workers, far
above what moves and claims need. On one hot key, 85 % of CASes conflict (correctly) and the p99 grows to
45 ms from row-lock queueing: a stampede of controllers on one key is wasted work, which is why the
controller should de-duplicate moves (`for update skip locked` or a leader lease, plan §5.2) even though
safety does not need it. Docker Desktop's VM adds latency over bare Linux; treat these as an upper bound for
loopback.
