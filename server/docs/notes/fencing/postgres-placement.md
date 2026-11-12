# Postgres placement adapter

How the `owner.Placement` port is implemented on Postgres (`internal/placement/pg`): the rows, the CAS, the
error map, the pool settings, the concurrency gates it passes, and its CAS latency.

Port: `owner.Placement` (`internal/owner/ports.go`). Fake: `ownertest.Placement`. Adapter:
`internal/placement/pg`.

## Rows

The original design:

```sql
CREATE TABLE placement (
  database_id  text PRIMARY KEY,
  owner_vm     text   NOT NULL,
  epoch        bigint NOT NULL CHECK (epoch >= 1),
  generation   bigint NOT NULL CHECK (generation >= 0),
  state        text   NOT NULL CHECK (state IN ('ACTIVE','MOVING')),   -- SUSPENDED/DROPPING later
  updated_at   timestamptz NOT NULL DEFAULT now()
);
-- Fence acknowledgements are recorded with the flip, for audit and for VM-loss decisions.
CREATE TABLE moves (
  database_id text, from_vm text, to_vm text, from_epoch bigint, ack_generation bigint,
  acked_at timestamptz, flipped_at timestamptz, PRIMARY KEY (database_id, from_epoch)
);
```

The built schema is `internal/placement/pg/schema/0001_placement.sql`: the same rows plus named constraints
and indexes on `moves.from_vm`/`to_vm`, applied by `Placement.Migrate` and recorded in `schema_version`.
Golden constraint test: `schema/test/constraints.sql` + `.expected`. One query file per method: `get`,
`create`, `cas` (flip + `moves` row in one statement), `moving`, `vm_load`, `register_vm`, `invariants`.

## Operations

- **Get**: `SELECT owner_vm, epoch, generation, state FROM placement WHERE database_id = $1` on the
  **primary**. The locator may read replicas; this port may not, because Claim and Reconcile need a
  linearizable read (see the [safety argument](safety-argument.md)). No row → `owner.ErrNotFound`.
- **CAS(expect, next)**: one statement:
  ```sql
  UPDATE placement SET owner_vm=$n_owner, epoch=$n_epoch, generation=$n_gen, state=$n_state, updated_at=now()
   WHERE database_id=$id AND owner_vm=$e_owner AND epoch=$e_epoch AND generation=$e_gen AND state=$e_state
  ```
  If it updates 1 row, the CAS succeeded. If it updates 0 rows, return `owner.ErrConflict`. With
  `expect.Epoch == 0`, use `INSERT ... ON CONFLICT DO NOTHING` (0 rows → ErrConflict). Single-row statements
  are atomic under READ COMMITTED, so no explicit lock is needed.
- **Invariants in the database**, not only in code: a `BEFORE UPDATE` trigger that rejects
  `NEW.epoch < OLD.epoch` and `NEW.owner_vm <> OLD.owner_vm AND NEW.epoch <= OLD.epoch`. The fake enforces the
  same rules (`TestPlacementCAS`).
- **The flip** (MOVING(src, E) → ACTIVE(dst, E+1, ack.gen)) and its `moves` row are written in one statement.
  The target is activated only after that commit (`Mover` calls `Claim` on dst after the CAS returns).
- **Lost replies**: a CAS may commit while its reply is lost. `Mover.Move` is written to resume. It re-reads
  the row and continues, so the adapter does not need idempotency keys (`TestMoveCrashPoints`: "flip applied
  but its reply was lost").

## Errors

0 rows → `owner.ErrConflict`; no row → `owner.ErrNotFound`; trigger/check/unique → `pg.ErrInvariant` (a bug);
FK on `owner_vm` → `pg.ErrUnknownVM`; timeouts, lock timeout, connection errors, open breaker →
`pg.ErrUnavailable` (never a sentinel). Bad input → `owner.ErrInvalid` before sending. A connection error must
never match `ErrConflict` or `ErrNotFound`: Claim and Reconcile treat any other error as "unavailable; stay
closed".

## Pool

Connect 3 s, `statement_timeout` 2 s, `lock_timeout` 1 s, `idle_in_transaction_session_timeout` 5 s, client
call deadline 2.5 s, TCP keepalive 15 s × 3, `MaxConnIdleTime` 60 s, `synchronous_commit = on` forced,
`application_name` set. Breaker per pool (`Placement.Breaker()`, registered as `"placement"`).

### Trap: pgx holds a pool slot for 15 s after a half-open call

When a call's ctx ends, pgx closes the connection asynchronously: cancel request, then a drain under a fixed
15 s deadline. On a half-open connection the drain never sees EOF, so the pool slot stays taken for 15 s (with
`MaxConns 1` every call failed for 15 s). Fix: the dialer wraps connections so any non-zero deadline is capped
at 1 s from now (`closeBudget`), and the cancel request's dial uses the connect timeout. pgx sets deadlines
only for these two cases (pgx v5.11.0 `pgconn.go`: `asyncClose`, the ctx watcher), so normal calls are
unaffected.

## Gates

All automated, run with `MANSIO_PG_URL` set against `deploy/postgres` (Postgres 17.11 in Docker), 2026-11-05:

| Gate                                  | Test                                                                                                                    | Result                                                                                                                             |
| ------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| Contract suite vs fake and Postgres   | `ownertest.RunPlacementSuite` via `TestPlacementCAS` in `ownertest` and `pg`                                            | pass                                                                                                                               |
| Lost flip reply with the real adapter | `pg.TestMoveCrashPoints/flip_applied_but_its_reply_was_lost`                                                            | pass: Move fails `ErrUnavailable`, the flip committed, resume claims at epoch 2, one `moves` row, writes intact                    |
| Each constraint rejects its off point | `TestConstraintsGolden`                                                                                                 | pass                                                                                                                               |
| Row held in an open transaction       | `TestLockHeldFailsWithinLockTimeout`                                                                                    | CAS fails `ErrUnavailable` after ≈ 1.0 s; Get unaffected                                                                           |
| Black hole / half-open / refused      | `TestBlackholeFailsWithinCallTimeout`, `TestHalfOpenRecyclesTheStaleConnection`, `TestBreakerOpensOnSystemFailuresOnly` | fail within 2.5 s + slack, non-sentinel; next call gets a fresh connection                                                         |
| 64 workers, one key, one expected row | `TestConcurrentCASOneKey` (20 rounds)                                                                                   | exactly 1 winner, 63 `ErrConflict`, epoch +1, every round                                                                          |
| Many-key move race                    | `TestMoveRaceManyKeys` (64 workers, 16 keys, 40 steps)                                                                  | invariants query empty; per key the `moves` rows are exactly `from_epoch` 1 … epoch−1 and equal the successful flips (≈ 400 flips) |

## CAS latency

Measured 2026-11-05 on an Apple M5 Max, macOS; Postgres 17.11 in Docker Desktop (18 CPUs, 4 GiB) on loopback;
pgx v5.11.0, pool 64; `MANSIO_PG_LATENCY=1 go test -run TestCASLatency ./internal/placement/pg`, 3 s per row,
no `-race`. Each worker loops Get → CAS (gen + 1); latency is the CAS call only (acquire + round trip +
commit, fsync on). "one key" = every worker on one key.

| mode          | workers | CAS calls |     p50 |     p99 |  CAS/s | conflicts/s |
| ------------- | ------: | --------: | ------: | ------: | -----: | ----------: |
| distinct keys |       1 |    10 831 |  177 µs |  291 µs |  3 610 |           0 |
| distinct keys |      16 |    45 753 |  697 µs | 1.12 ms | 15 251 |           0 |
| distinct keys |      64 |    62 358 | 1.94 ms | 4.32 ms | 20 786 |           0 |
| one key       |       1 |    10 647 |  180 µs |  230 µs |  3 549 |           0 |
| one key       |      16 |    25 835 | 1.91 ms | 4.51 ms |  8 612 |       7 279 |
| one key       |      64 |    21 698 | 5.91 ms | 44.6 ms |  7 233 |       6 420 |

Reading: a CAS costs ≈ 0.2 ms alone; the authority does ~20k CAS/s over distinct keys with 64 workers, far
above what moves and claims need. On one hot key, 85 % of CASes conflict (correctly) and the p99 grows to 45
ms from row-lock queueing: a stampede of controllers on one key is wasted work, which is why the controller
should de-duplicate moves (`for update skip locked` or a leader lease) even though safety does not need it.
Docker Desktop's VM adds latency over bare Linux; treat these as an upper bound for loopback.
