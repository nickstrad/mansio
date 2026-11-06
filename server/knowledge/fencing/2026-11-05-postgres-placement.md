# 2026-11-05 — What a Postgres `Placement` adapter needs

Author: fencing agent. Port: `owner.Placement` (`internal/owner/ports.go`). Fake: `ownertest.Placement`. No
real adapter is written yet. This note lists what the control plane's adapter must do to satisfy the port's
contract.

## Row (option 4 §1/§3)

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

## Operations

- **Get**: `SELECT owner_vm, epoch, generation, state FROM placement WHERE database_id = $1` on the
  **primary**. The locator may read replicas; this port may not, because Claim and Reconcile need a
  linearizable read (see the safety argument). No row → `owner.ErrNotFound`.
- **CAS(expect, next)**: one statement:
  ```sql
  UPDATE placement SET owner_vm=$n_owner, epoch=$n_epoch, generation=$n_gen, state=$n_state, updated_at=now()
   WHERE database_id=$id AND owner_vm=$e_owner AND epoch=$e_epoch AND generation=$e_gen AND state=$e_state
  ```
  If it updates 1 row, the CAS succeeded. If it updates 0 rows, return `owner.ErrConflict`. With
  `expect.Epoch == 0`, use `INSERT ... ON CONFLICT DO NOTHING` (0 rows → ErrConflict). Single-row
  statements are atomic under READ COMMITTED, so no explicit lock is needed.
- **Invariants in the database**, not only in code: a `BEFORE UPDATE` trigger that rejects
  `NEW.epoch < OLD.epoch` and `NEW.owner_vm <> OLD.owner_vm AND NEW.epoch <= OLD.epoch`. The fake enforces
  the same rules (`TestPlacementCAS`).
- **The flip** (MOVING(src, E) → ACTIVE(dst, E+1, ack.gen)) and its `moves` row are written in one
  transaction. The target is activated only after that commit (`Mover` calls `Claim` on dst after the CAS
  returns).
- **Lost replies**: a CAS may commit while its reply is lost. `Mover.Move` is written to resume. It re-reads
  the row and continues, so the adapter does not need idempotency keys (`TestMoveCrashPoints`: "flip applied
  but its reply was lost").
- **Errors**: wrap driver errors as `errs.Wrap("placement", err, ...)`. A connection error must not match
  `ErrConflict` or `ErrNotFound`: Claim and Reconcile treat any other error as "unavailable; stay closed".
- **Contract suite to write with it**: run the same CAS and invariant cases as `TestPlacementCAS` against
  the fake and against Postgres (env-gated, as `COLDLOAD_S3` gates the S3 tests).
