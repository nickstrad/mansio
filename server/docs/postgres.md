# How mansio uses Postgres

Postgres holds the control plane's state: the placement authority that the whole fencing argument rests on
("the authority CAS is atomic, epochs only move forward"), the VM, tenant and database registries, API keys,
app schemas and the single-node tenancy store. All of it goes through one adapter, `internal/placement/pg`.
Tenant data never lives here; it is SQLite on the agents and generations in object storage
([object-storage.md](object-storage.md)). The SQL rules that hold for every dialect (SQLite, DuckDB, Postgres)
are in [coding-style.md §7](coding-style.md#7-sql); this page adds what is specific to Postgres.

## Files and migrations

- **Schema changes are numbered, transactional files**: `internal/placement/pg/schema/NNNN_name.sql`, each
  wrapped in `begin; … commit;` and inserting its own row into `schema_version`, so `psql -f` and `Migrate`
  apply it the same way. Postgres DDL is transactional, so a file applies completely or not at all. `Migrate`
  holds an advisory lock so two processes starting together do not race. **Never edit an applied file; add the
  next one.** Numbers have no gaps (checked at start).
- **One query per file, named after the port method**, in `internal/placement/pg/queries/`, embedded and
  loaded once. A query seen in logs or `pg_stat_statements` maps to one file. Each file starts with a comment
  that says what the query guarantees and what "0 rows" means:
  ```sql
  -- cas.sql — Placement.CAS on an existing row; also records the flip when MOVING → ACTIVE on another VM.
  ```
- **Layout**: lowercase keywords, one clause per line, right-aligned clause keywords, explicit join
  conditions. (Older SQL elsewhere in the repo is uppercase; leave it, and do not mix styles within one file.)

## Schema conventions

- **Invariants live in the schema**, so they hold whichever code path writes: `not null` on every required
  column, `check` constraints, primary and foreign keys, and **an index on the referencing side of every
  foreign key** (Postgres does not add one). A trigger is used only when a rule compares the old and new row,
  which a `check` cannot: `placement_epoch_monotonic` refuses an epoch decrease and an owner change without an
  epoch increase.
- **Domains state a repeated rule once**: `database_key` (text, 1..120 bytes, matching `owner.MaxKeyLen`),
  `vm_id`, `epoch` (≥ 1), `generation` (≥ 0).
- **Types**: `timestamptz` (stores UTC), `bigint` (never `serial`, which overflows), keys `text collate "C"`
  (byte order, identical on every host). Never put uniqueness on a timestamp.
- **Natural keys**: `placement` is keyed by the database key, with no surrogate id.
- **No hot counter rows.** A column that every write updates serializes the writers. Load per VM is computed
  (`vm_load.sql`, `count(*) … group by owner_vm`, served by an index); cache an aggregate only with a measured
  need and a stated staleness bound.
- **Frequently updated, single-writer rows stay unindexed on the hot column**: `vms.heartbeat` has no index
  and the table has `fillfactor = 70`, so heartbeats are HOT updates.
- **A small fixed set is a `check` list** (`state in ('ACTIVE', 'MOVING')`): it changes only with code, and
  the migration that changes the code rewrites the check.

## Queries and transactions

- **Values are always bind parameters** (pgx extended protocol: a value is never parsed as SQL).
- **One statement over a multi-statement transaction.** One statement sees one snapshot. `cas.sql` updates the
  placement row and, when the change is a move's flip, inserts the `moves` audit row, in one statement (a CTE
  over `update … returning`). The `moves` row exists if and only if the flip happened, and a replayed flip
  after a lost reply updates 0 rows and writes no second row.
- **Read committed, on purpose.** Under read committed, a conditional `update` racing another waits for the
  row, re-checks its `where` on the new version and updates 0 rows: exactly a CAS conflict. Repeatable read
  would turn that into a serialization error to remap. Raise isolation only for a named reason, and then retry
  on SQLSTATE `40001`.
- **Upserts use `on conflict`**, never "select, then insert": `create.sql` is
  `insert … on conflict
  (database_id) do nothing returning epoch`, and no row back means the key exists.
- **Primary only.** `Get` must be linearizable for Claim and Reconcile; the adapter refuses a DSN with
  fallback hosts (they may be replicas).
- **Correctness never depends on locks or notifications.** CAS plus constraints are the safety. Locks
  (`for update skip locked`, advisory locks) may only de-duplicate work; `Migrate`'s advisory lock is one such
  use. Work that must not be lost (moves to resume, VM-loss restores) is found by a table scan (`moving.sql`),
  never by NOTIFY.

### Error mapping

| What happened                                                                     | Error                                                                                | Counts against the breaker |
| --------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------ | -------------------------- |
| CAS matched 0 rows                                                                | `owner.ErrConflict`                                                                  | no                         |
| No row on `Get`                                                                   | `owner.ErrNotFound`                                                                  | no                         |
| Trigger, check, not-null or unique violation (`23514`, `23502`, `23505`, `23P01`) | `pg.ErrInvariant`: the caller asked for an illegal next row, a bug, never a conflict | no                         |
| Foreign-key violation (`23503`): the VM is not registered                         | `pg.ErrUnknownVM`                                                                    | no                         |
| Timeout, lock timeout, connection error, breaker open                             | `pg.ErrUnavailable` (with a retry hint when the breaker refused)                     | yes                        |

A connection error never maps to `ErrConflict` or `ErrNotFound`. After `ErrUnavailable` on a CAS the outcome
is unknown: re-read before deciding.

## Connections and pool budgeting

`pg.Config.poolConfig` is the one place session settings are built:

| Setting                               | Default                                    | Why                                                                                                 |
| ------------------------------------- | ------------------------------------------ | --------------------------------------------------------------------------------------------------- |
| `connect_timeout`                     | 3 s                                        | a dead host fails fast                                                                              |
| `statement_timeout`                   | 2 s                                        | server-side bound per statement                                                                     |
| `lock_timeout`                        | 1 s (must be below `statement_timeout`)    | a session holding a row cannot block every CAS on it                                                |
| `idle_in_transaction_session_timeout` | 5 s                                        | an abandoned transaction releases its row locks                                                     |
| call timeout (client side)            | `statement_timeout` + 500 ms               | bounds acquire + connect + round trip when the network, not the server, hangs                       |
| `MaxConnIdleTime`                     | 60 s                                       | below any NAT or load-balancer idle timeout, which would otherwise drop pooled connections silently |
| TCP keepalive                         | 15 s, 3 probes                             | a dead peer on an idle connection is found in about 45 s                                            |
| `application_name`                    | `<binary>/<module>`, e.g. `agentd/tenancy` | every session is traceable in `pg_stat_activity`                                                    |
| `default_transaction_isolation`       | read committed                             | the CAS semantics above                                                                             |
| `synchronous_commit`                  | on, forced                                 | the safety path never trades durability for latency                                                 |

The pool connects lazily (`MinConns` 0), so a process starts and serves what it can while Postgres is down.
Every pool has its own circuit breaker (`placement`, `tenancy`), and every acquire takes the caller's ctx.

**Budget the callers below the pool, so nobody queues inside it.** A caller that waits inside pgxpool waits
until its call timeout and then fails; a caller refused at a slot fails at once with a retry hint.

- **controld**: `pg.max_conns` 32. Resolve loads (`locator.load_concurrency` 10), creates on first use
  (`locator.create_concurrency` 2) and API calls (`api_concurrency` 16) take slots that sum to less than the
  pool; the config is refused otherwise. The rest is for the heartbeat, the move resumer and tenancy. The
  LISTEN connection is separate. N controld instances hold up to N × (`max_conns` + 1) connections; keep that
  under Postgres's `max_connections` minus other clients. Details:
  [controld pool budget](notes/backpressure/2026-11-10-controld-pool-budget.md).
- **agentd placement**: `max_conns` defaults to `startup_workers` + `lifecycle_concurrency` (16 + 16), the
  most callers that can use it at once.
- **agentd tenancy**: its own pool of 8 and its own breaker; admin and sign-in calls have their own slot
  limits in front of it.

## LISTEN/NOTIFY

A trigger (`0003_notify.sql`) sends `pg_notify('placement', database_id || ':' || epoch)` on every insert,
update and delete of a placement row. The epoch is in the payload so two changes in one transaction never fold
into one notification and a late one can be told from a fresh one. A second trigger (`0005_state_notify.sql`)
sends `database_state` when a registry row's state or move target changes. Notifications only invalidate
rebuildable caches (the locator's):

- **LISTEN first, then load or clear the cache, then apply notifications**, after every connect (`pg.Listen`,
  `ListenHooks.Ready`). The other order loses changes made in between.
- The listener uses a dedicated connection, not the pool, pings after a quiet period and reconnects with
  jittered backoff. The cache is trusted only while that connection is up.
- A lost notification is safe: a stale route ends in `wrong_owner` or `fenced` and the client re-resolves.

## Testing

- Postgres tests are gated on `MANSIO_PG_URL` (`deploy/postgres/`) and each runs in a fresh schema.
- **Every constraint has an off-point test**: `schema/test/constraints.sql` runs statement by statement and
  its output is diffed against `constraints.expected` (`-update` rewrites it; review the diff)
  (`TestConstraintsGolden`). A constraint that never fired in a test may not exist.
- The adapter passes the same `ownertest` contract suite as the fake, and passes it again through
  `internal/faultnet`: under a blackhole or a half-open connection, Get and CAS fail within the timeouts with
  an error that is not `ErrConflict`, and a stale pooled connection is recycled without a hang.
- **Concurrency tests put many workers on the same key** (`TestConcurrentCASOneKey`: 64 CASes from one row,
  exactly one wins, the epoch rises by one) and race moves over many keys (`TestMoveRaceManyKeys`, then
  `invariants.sql` must return no rows). Latency at 1, 16 and 64 workers is in
  [the adapter note](notes/fencing/2026-11-05-postgres-placement-adapter.md).
- Try schema changes and manual fixes in `begin; … rollback;` first. Scripts run `psql -X -v ON_ERROR_STOP=1`
  and never `ON_ERROR_ROLLBACK`. A manual fix is `begin`, DML with `returning`, check, `commit`, and filters
  on the natural key as well as any id.

## Not adopted

- **Business logic in PL/pgSQL.** Control-plane decisions are pure Go functions with property tests; the
  database keeps invariants (constraints, one trigger), not workflows.
- **Triggers that maintain counters or caches, materialized views, partitioning.** No derived data in the
  authority yet; `vm_load.sql` is cheap at the planned scale. Revisit if placement rows reach millions.
- **Lowering durability** (`synchronous_commit = off`) anywhere on the placement path.

## Open questions

- **Leases.** If they are ever built, a `tstzrange` with an exclusion constraint per database is the natural
  tool for "no overlapping leases" ([options](notes/fencing/2026-11-05-leases-options.md)).
- **`pg_stat_statements`** should be enabled on the control-plane database when it is deployed (operations,
  not code).
