# Plan: apply _The Art of PostgreSQL_ (Fontaine) to ts-db-go

Status: plan · Date: 2026-11-05 · Baseline: `main` at `bbf184b` · Companion: [guide.md](guide.md) (the SQL
style guide this plan proposes; the rules there are referenced as G-n).

Sources: book summaries `~/AI/books/art-of-postgresql/chapters/*.md`, raw text
`~/AI/books/art-of-postgresql/raw/chapters-text/`. Chapter numbers below are the **summary file numbers**
(they run one ahead of the book's own numbering; e.g. "ch 37" = file `37-isolation-and-locking.md`). Existing
practice this builds on: `~/AI/writeups/go-coding-style.md`, `~/AI/writeups/go-testing.md`,
`knowledge/testing/README.md` (Effective Software Testing, already adopted; not redone here),
`knowledge/fencing/2026-11-05-postgres-placement.md`, `docs/control-plane-plan.md`.

## 1. Why this book matters here

ts-db-go writes SQL in three dialects and is about to add a fourth role. SQLite is the data plane: the actor
(`internal/actor`) runs every tenant statement inside a group-commit transaction it owns, the generators
(`internal/schema`, `internal/workload`, `cmd/coldload/template.go`) build DDL and DML as Go strings, and the
request-id store (control-plane plan step 2) will put a table inside every tenant database. DuckDB is the
analysis plane (`queries/*.sql`). Postgres is the next big piece: the placement authority (plan step 3) and
`controld`'s registries (step 4) hold the one piece of state the whole fencing safety argument rests on
("authority CAS is atomic, epochs only move forward", `knowledge/fencing/2026-11-05-safety-argument.md`).
Fontaine's core claims map directly: treat the database as a concurrent service whose API is SQL (ch 3), put
invariants in constraints, not only in Go (ch 10, 29), let one statement do the work under one snapshot instead
of round-trip loops (ch 5, 41), understand exactly what an error does to the enclosing transaction (ch 37), and
treat SQL as code with style, files and regression tests (ch 6, 8). Most of the book is Postgres-specific; the
ideas about transactions, constraints, set-based SQL and SQL-as-code also hold for SQLite and DuckDB, and this
plan says which is which.

## 2. What the project already does well

| Book idea | Where ts-db-go already does it |
|---|---|
| Parameters sent apart from SQL text; prepared statements (ch 2 §1.6–1.7) | Every generated statement uses `?` binds, built once per table (`internal/workload/workload.go:141-205`, `Next` only draws args, `:251-317`). The actor keeps an LRU of prepared statements per connection (`internal/actor/conn.go:103-119`). The front decodes typed bind values and refuses `;` (`internal/front/request.go:186-213`). |
| Transaction boundaries fixed at design time, not by callers (ch 42, ch 5) | The actor owns `BEGIN IMMEDIATE … COMMIT` (`internal/actor/actor.go:311-344`); clients may not send transaction control, `PRAGMA`, `ATTACH` (`internal/front/request.go:172-178`). |
| Many writes in one transaction (ch 36 "multi-row inserts in one transaction") | Group commit (`actor.go:311`). |
| Set-based bulk insert (`insert … select` over a generated series, ch 2, 36) | Coldload template fill: `WITH RECURSIVE … INSERT … SELECT … randomblob(?)` (`cmd/coldload/template.go:25-27`); wbbench does the same (`cmd/wbbench/main.go:196`). |
| Race-free upsert (ch 39) | `INSERT … ON CONFLICT (id) DO UPDATE SET c = excluded.c` (`workload.go:158`). |
| Copy-and-swap rather than in-place destruction (ch 36) | `actor.Seed` builds under `.seeding` and renames (`conn.go:220-232`). |
| Modern analytic SQL (ch 16, 18) | `queries/report.sql` uses `count(*) FILTER`, `quantile_cont`, CTE stages, `QUALIFY row_number() OVER`, `nullif` guards, `PIVOT`; each section opens with a `.print` that states intent. |
| Invariants in the database; single-statement CAS; primary reads (ch 10, 29, 37) | The fencing note already specifies a conditional `UPDATE` as the CAS, `CHECK`s, an epoch-monotonic trigger, primary-only `Get` (`knowledge/fencing/2026-11-05-postgres-placement.md`). |
| Trace a query to its origin (ch 8, `application_name`) | sqlbench records `class` and `db` per attempt (`requests.csv`), which is the SQLite-side equivalent. |

## 3. Gaps and risks (ranked by impact)

### 3.1 A statement can end the group-commit transaction; later batch-mates autocommit and are reported failed (High)

- **Book:** in Postgres any error aborts the transaction and `COMMIT` becomes `ROLLBACK` (ch 37); psql's
  `ON_ERROR_ROLLBACK` survives that only by wrapping each statement in a savepoint (ch 7). Know what an error
  does to the enclosing transaction.
- **Code:** `writeBatch` treats a write as fatal to the batch only when it timed out (`actor.go:326`,
  `aborted = results[i].TimedOut`); any other error is the statement's own (`settle`, `plan.go:35-46`). SQLite
  can end the transaction on its own: `INSERT OR ROLLBACK`, `RAISE(ROLLBACK)` in a trigger (clients may send
  `CREATE`, `request.go:176`), and SQLITE_FULL/IOERR/NOMEM/BUSY may auto-rollback.
- **Scenario (reproduced with the sqlite3 CLI, 2026-11-05):** `BEGIN IMMEDIATE; insert (2); insert or rollback
  (dup 1); insert (3); COMMIT` → the duplicate rolls back the transaction, insert 3 runs in autocommit and
  **is committed**, `COMMIT` fails with "no transaction is active". `settle` then reports `endErr` for insert 2
  (correct: lost) and insert 3 (wrong: it is durable). The client retries insert 3 and duplicates it;
  `Commits()` is not incremented, so commit-count write-back (`internal/vactor/writeback.go`) under-counts.
- **Why it matters next:** the request-id store (control-plane step 2) writes its row as another statement in
  the batch. A batch-mate that ends the transaction breaks "applied exactly once" for every other request in
  the batch. Fix before step 2. → **PG-01**.

### 3.2 The placement schema in the fencing note is under-constrained and has no file, test or concurrency gate (High)

- **Book:** constraints carry the model (ch 29: `NOT NULL`, `CHECK`, domains, FKs with indexes on the referencing
  side); timestamps are `timestamptz` (ch 23); single-statement DML with `returning` and CTEs instead of
  multi-statement dances (ch 36, 41); under read committed a concurrent `UPDATE` on the same row waits, then
  re-checks its `WHERE` on the new version (ch 37); LISTEN/NOTIFY is for rebuildable caches, never queues
  (ch 40); test concurrency with many workers on the **same** key (ch 37).
- **Code/doc:** `moves` columns are all nullable and have no FK
  (`knowledge/fencing/2026-11-05-postgres-placement.md` "Row"); `owner_vm` references nothing although step 4
  adds `vms`; no key-length rule though keys are 1..120 bytes (`owner.ValidKey`, `internal/owner/ports.go:51`);
  the flip and its `moves` row are "one transaction" of two statements, with no statement given; the trigger's
  error is not distinguished from `ErrConflict`; no index for "databases on VM X" (placement policy, VM-loss
  restore); no `lock_timeout`, so a stuck session holding the row blocks `Claim` until the caller's ctx ends.
- **Scenario:** a `moves` row with `ack_generation NULL` is accepted, so the VM-loss decision (plan §3 step 4)
  reads "unknown" as 0 and restores from generation 0. → **PG-02, PG-03, PG-04**, design in §5.1.

### 3.3 Declared constraints and types that SQLite does not enforce (Medium)

- **Book:** integrity belongs in the database (ch 10, 29); let the type system reject impossible values
  (ch 22, 23). *SQLite translation:* SQLite enforces `REFERENCES` only with `PRAGMA foreign_keys = ON`, and
  column types only in `STRICT` tables (SQLite ≥ 3.37; modernc v1.61.1 ships a newer one).
- **Code:** the schema generator declares `parent_id INTEGER REFERENCES parent(id)` (`internal/schema/schema.go:158`)
  but the DSN never sets `foreign_keys` (`internal/actor/conn.go:85-86`), and no DDL uses `STRICT`
  (`schema.go:156`, `cmd/coldload/template.go:17-21`).
- **Scenario:** sqlbench's `delete` class removes parent rows while children keep pointing at them, so the
  `join` class silently returns fewer rows as a run goes on, and runs with different delete shares are not
  comparable. For ts-db tenants (compiler-generated schemas, plan §3 "compiler port") a declared FK that is not
  enforced is a lie the application will trust. → **PG-05**.

### 3.4 Report SQL has no types and no regression test; the same views are defined twice (Medium)

- **Book:** SQL is code: unit/regression tests with expected output, psql + `diff` (ch 8); a view is the one
  shared definition of a derived metric (ch 38); `except` against stored results is a cheap regression test (ch 16).
- **Code:** `read_csv(..., header = true)` relies on type sniffing (`queries/report.sql:5-8`,
  `queries/compare.sql:5-13`, `queries/coldload/report.sql:5-6`); the `req`/`smp`/`ph` views exist in two files
  with different options; nothing runs any `.sql` file in `go test` or `make sqlbench-check`.
- **Scenario:** a short run where every `final` value is empty or every `batch` is 0 sniffs a different type;
  a column rename in `internal/stats/writer.go` breaks section 14 silently until someone reads a report.
  → **PG-06**.

### 3.5 SQL built in Go strings with style drift (Low)

- **Book:** no positional `order by 2` (ch 8); explicit column lists; one connection-string builder rather than
  ad hoc concatenation (ch 2, the injection section, applies equally to DSNs).
- **Code:** `ORDER BY 2 DESC` (`internal/workload/workload.go:190,194`); `INSERT INTO meta VALUES (...)` without a
  column list (`cmd/coldload/template.go:57`); the template opens `"file:"+path+"?_pragma=…"` unescaped
  (`template.go:41`), the exact bug `actor.dsn` documents and fixes (`conn.go:81-92`).
- **Scenario:** a results directory containing `#` or `?` opens a different file. → **PG-07**.

### 3.6 Row-at-a-time seeding (Low)

- **Book:** for bulk loads use multi-row `values` in one transaction or `insert … select` (ch 36).
- **Code:** `Schema.Fill` inserts one row per `exec` (`internal/schema/schema.go:246-264`) although
  `Table.InsertSQL(n)` already builds multi-row statements (`schema.go:223`). SQLite has no network round trip,
  so the gain is per-call driver overhead only; `phases.csv` already records `seed_ms`. → **PG-08** (measure first).

### 3.7 Read-only classification is a keyword heuristic (Low, noted)

`actor.IsWrite` scans for `INSERT`/`UPDATE`/… substrings (`internal/actor/plan.go:49-66`), and reads run on the
writer connection when `read_conns = 0` (`actor.go:303`). The front's package comment already calls the guard
"defense in depth, not a boundary" and the compiled `op` form will replace raw SQL. The book's point (ch 2) is
to let the parser decide; SQLite's parser answer is `sqlite3_stmt_readonly`, which `database/sql` does not
expose. No item; revisit when the op compiler lands.

## 4. Refactor items

Order is the table order. "Before CP" = can land before the control-plane step it names; "with CP" = part of it.

| Item | Title | Size | Depends | When |
|---|---|---|---|---|
| PG-01 | Savepoint per write; detect a transaction ended under the actor | M | — | Before CP step 2 (request ids) |
| PG-02 | Placement schema as an embedded, versioned `.sql` file | S | — | With CP step 3 |
| PG-03 | Placement adapter: single-statement CAS and flip, error mapping, timeouts | M | PG-02 | With CP step 3 |
| PG-04 | Placement concurrency and contract gate | M | PG-03 | With CP step 3 |
| PG-05 | Enforce FKs and types in SQLite (`foreign_keys`, `STRICT`) | S | — | Before CP |
| PG-06 | Typed shared views and golden tests for `queries/*.sql` | S | — | Any time |
| PG-07 | Small SQL hygiene fixes (positional order by, column lists, DSN builder) | S | — | Any time |
| PG-08 | Multi-row seed fill, measured | S | — | Any time |
| PG-09 | Request-id table design for tenant databases | S | PG-01 | With CP step 2 |
| PG-10 | Locator cache invalidation with LISTEN/NOTIFY | M | PG-03 | With CP step 5 |
| PG-11 | Adopt the SQL guide | S | — | Now |

#### PG-01 — Savepoint per write; detect a transaction ended under the actor (ch 7, 37) — **Done** (345c802)

- **Files:** `internal/actor/actor.go` (`writeBatch`), `internal/actor/plan.go` (`settle`), `actor/fake_test.go`,
  `actor/model_fuzz_test.go`, `actor/sqlite_test.go`.
- **Change:** wrap each write in `SAVEPOINT w; <stmt>; RELEASE w` (prepared once, cached like `BEGIN`). On a
  statement error run `ROLLBACK TO w; RELEASE w`, so the error stays the statement's own and its batch-mates
  are unaffected (this is psql's `ON_ERROR_ROLLBACK`, and the same mechanism a future Postgres-backed executor
  needs, because there every error aborts the transaction). If `ROLLBACK TO`/`RELEASE` fails with "no such
  savepoint", the transaction is gone: stop running writes (the rest get `ErrAborted` and are never run, so
  nothing autocommits) and report the already-run writes as aborted. Add an executor-level postcondition:
  after a batch ends, `COMMIT` must not fail with "no transaction is active"; if it does, that is a bug (assert).
- **Acceptance:** a SQLite test reproduces §3.1 (`INSERT OR ROLLBACK` and a `RAISE(ROLLBACK)` trigger mid-batch):
  every write is either committed and `ok`, or not committed and not `ok`, checked by reading the table back.
  The fake executor gains an `endsTxn` fault and `FuzzActorModel` gains it as an op. `compare-actor` sqlbench
  config before/after: write p99 and commit p99 change ≤ 10% (record in `knowledge/sqlbench/`); if more, gate
  the savepoints to batches > 1 write.
- **Size:** M.

#### PG-02 — Placement schema as an embedded, versioned `.sql` file (ch 6, 28, 29, 23) — **Done** (06204e3; plus named constraints and indexes on `moves.from_vm`/`to_vm`)

- **Files (new):** `internal/placement/pg/schema/0001_placement.sql`, `internal/placement/pg/schema.go`
  (`//go:embed schema/*.sql`, an apply function that runs each file once inside a transaction and records it
  in `schema_version`).
- **Change:** the DDL in §5.1, lower-risk than the note's version: domains for key, VM id, epoch and generation;
  `NOT NULL` everywhere; FKs with indexes on the referencing columns; `collate "C"` on keys; the invariant
  trigger. Postgres DDL is transactional (ch 5), so each migration file is all-or-nothing.
- **Acceptance:** `psql -v ON_ERROR_STOP=1 -f 0001_placement.sql` on an empty database succeeds and a second
  apply through Go is a no-op; a SQL regression script (`schema/test/constraints.sql` + expected output, per
  G-20) shows each constraint rejecting its off-point (epoch 0, 121-byte key, a `moves` row naming an unknown VM, `from_vm = to_vm`,
  epoch decrease, owner change without epoch increase).
- **Size:** S.

#### PG-03 — Placement adapter (ch 2, 36, 37, 41) — **Done** (06204e3; adds `register_vm.sql` and `invariants.sql`; trigger/check → `pg.ErrInvariant`, unknown VM → `pg.ErrUnknownVM`, outages → `pg.ErrUnavailable`)

- **Files (new):** `internal/placement/pg/placement.go`, `internal/placement/pg/queries/*.sql` (one named query
  per file, embedded), adapter for `owner.Placement` (`internal/owner/ports.go:87-90`).
- **Change:** the queries in §5.2. Driver `pgx/v5` (extended protocol: binds are never parsed as SQL, ch 2).
  Pool sets `application_name=controld/placement` (ch 8) and per-session `lock_timeout = '1s'`,
  `statement_timeout = '2s'`, `idle_in_transaction_session_timeout = '5s'` (aligned with release-it guide §P). Read committed (ch 37: the second
  of two racing CASes waits, re-checks the `WHERE` on the new row and updates 0 rows → `ErrConflict`; repeatable
  read would turn that into a serialization error the adapter would have to remap). Error map: 0 rows →
  `owner.ErrConflict`; no row on Get → `owner.ErrNotFound`; SQLSTATE from the invariant trigger (`23514`,
  raised with constraint name `placement_epoch_monotonic`) → a bug, logged and surfaced as an assertion-class
  error, **not** `ErrConflict`; `55P03` lock timeout, connection errors → `errs.Wrap("placement", …)` that matches
  neither sentinel (Claim/Reconcile stay closed). Never `SET LOCAL synchronous_commit = off` on this pool (ch 33).
- **Acceptance:** `ownertest` contract suite (`TestPlacementCAS` cases) passes against the fake and Postgres
  (env-gated `TSDB_PG_URL`, as `COLDLOAD_S3` gates S3); a test that holds the row in an open transaction proves
  CAS returns a non-sentinel error within `lock_timeout`; `TestMoveCrashPoints` "reply lost" variant passes with
  the real adapter (cancel the ctx after the statement is sent).
- **Size:** M.

#### PG-04 — Placement concurrency and contract gate (ch 37) — **Done** (06204e3; numbers in knowledge/fencing/2026-11-05-postgres-placement-adapter.md)

- **Files:** `internal/placement/pg/placement_integration_test.go`, `cmd/sqlbench`-style load harness or a
  `go test -bench` under `scripts/lock.sh`.
- **Change/acceptance:** (1) 64 goroutines CAS the **same** key from the same expected row: exactly one
  succeeds, 63 get `ErrConflict`, epoch increased by exactly one. (2) 64 goroutines each create distinct keys
  and race moves: after the run, every `moves` row has a matching `placement` epoch ≥ `from_epoch + 1`, and the
  invariants query (every `moves` row has a `placement` row at an epoch > `from_epoch`) returns zero rows. (3) Record p50/p99 CAS latency and conflicts per second at 1, 16,
  64 workers in `knowledge/fencing/` (first Postgres numbers for the safety path).
- **Size:** M.

#### PG-05 — Enforce FKs and types in SQLite (ch 10, 22, 29) — **Done** (864a5a1)

- **Files:** `internal/actor/config.go` (new `ForeignKeys bool`, default true; JSON `foreign_keys`),
  `internal/actor/conn.go:85` (`&_pragma=foreign_keys(1)`), `internal/schema/schema.go:156-166` (emit
  `) STRICT;` and `ANY` never), `cmd/coldload/template.go:17-21`, `internal/workload` (`delete` targets leaf
  rows or uses `ON DELETE CASCADE`, chosen per run and recorded in `schemas.csv`).
- **Acceptance:** spec tests: inserting a child with a missing parent fails with a constraint error that maps to
  `error` (not `aborted`) and does not abort the batch (relies on PG-01); inserting `'abc'` into an `INTEGER`
  column of a generated table fails. A `smoke` run before/after shows the `join` class row count stable over
  the run. `keep_dbs` fingerprint bumps to `v3` (`schema.go:291`) so old seeded databases are not reused.
- **Size:** S.

#### PG-06 — Typed shared views and golden tests for `queries/*.sql` (ch 8, 16, 38) — **Done** (fcde79c)

- **Files:** new `queries/views.sql` (typed `read_csv(..., columns = {...})` for `requests`, `samples`, `phases`,
  `schemas`, generated from the column list in `internal/stats/writer.go`), `report.sql` and `compare.sql`
  `.read` it; new `queries/testdata/run/` (a 50-row fixture run) and `queries/testdata/report.expected`;
  `queries/report_test.go` (skips if `duckdb` is not on PATH).
- **Acceptance:** `go test ./queries/...` runs `duckdb -box < report.sql` in the fixture dir and diffs against
  the expected file; `-update` rewrites it. A Go test asserts the CSV header written by `internal/stats` equals the
  column list in `views.sql`, so a rename fails `make sqlbench-check`, not a report.
- **Size:** S.

#### PG-07 — SQL hygiene fixes (ch 2, 8) — **Done** (fbb2c82)

- **Files/changes:** `workload.go:190,194` → `count(*) AS n … ORDER BY n DESC`; `template.go:57` → explicit
  `(k, v)`; `template.go:41` → a shared `sqlitedsn.Build(path, pragmas)` (move `actor.dsn`'s escaping into a tiny
  package used by actor, template, wbbench).
- **Acceptance:** existing tests pass; a template test opens a directory named `a#b?c`.
- **Size:** S.

#### PG-08 — Multi-row seed fill, measured (ch 36) — **Skipped**: measured −14% seed time (< 20% bar), reverted; null result in knowledge/sqlbench/2026-11-05-pg08-multirow-seed.md (fbfc410)

- **Files:** `internal/schema/schema.go:246-264`: fixed chunks of 100 rows via `InsertSQL(100)` plus one tail
  statement (a fixed chunk size keeps the statement cache hot; 100 × 9 binds is far below SQLite's 32766 limit).
- **Acceptance:** fill is still deterministic (same rows for the same seed: existing `TestFill…` equality);
  `seed_ms_per_db` in report §12 for `ramp` drops; if it drops < 20%, revert and record the null result.
- **Size:** S.

#### PG-09 — Request-id table in each tenant database (ch 29, 36, 39) — **Done** (d7c921d, CP-2; as built: claim reads `sqlite3_changes()` instead of RETURNING, plus an `applied_ms` index; see knowledge/front/2026-11-05-request-ids.md)

- **Files:** design lands with CP step 2 (`internal/actor` or a new `internal/reqid`).
- **Change:** DDL and statements in §5.3: `STRICT, WITHOUT ROWID` table keyed by request id; insert with
  `ON CONFLICT DO NOTHING RETURNING` in the same savepoint as the user write (PG-01), so "seen before" and "apply"
  are one decision; retention by batched `DELETE … WHERE applied_ms < ?` on the actor, never row-at-a-time.
- **Acceptance:** CP step 2's gate (same id after crash and after move applies once) plus: a batch where the
  user statement fails leaves no request-id row (savepoint rolled back together).
- **Size:** S.

#### PG-10 — Locator cache invalidation with LISTEN/NOTIFY (ch 40) — **Done** (9f98cc4; trigger in `0003_notify.sql` on insert, update and delete; `pg.Listen`; gate in `cmd/controld` `TestLocatorGate`)

- **Files:** `cmd/controld` locator (CP step 5), trigger `placement_notify` in `0002_notify.sql`.
- **Change:** `after update on placement for each row` → `pg_notify('placement', database_id || ':' || epoch)`.
  The locator does LISTEN **first**, then loads/clears its cache, then applies notifications (ch 40 start
  order). Payload includes the epoch so two changes in one transaction never collapse. Losing a notification is
  safe: a stale route ends in `wrong_owner`/`fenced` and a re-resolve. Never use this channel for anything that
  must not be lost (moves to resume, VM-loss work): that is a table scan (§5.2 "resume").
- **Acceptance:** move a key while a locator holds it cached: next resolve returns the new epoch without waiting
  for TTL; kill the listener connection mid-move: the locator reconnects, reloads, and the resolve is correct.
- **Size:** M.

#### PG-11 — Adopt the SQL guide — **Done** (guide linked from docs/coding-style.md §7, AGENTS.md and knowledge/index.md; testing lines in knowledge/testing/2026-11-05-book-guides-testing.md)

Move [guide.md](guide.md) into force: link it from `knowledge/index.md` and the repo coding-style summary; add
the three lines in §6 of this plan to `knowledge/testing/README.md`. **Size:** S.

## 5. Postgres placement authority: schema and queries

This refines `knowledge/fencing/2026-11-05-postgres-placement.md`; the port contract
(`internal/owner/ports.go:79-90`) and the safety argument are unchanged. Postgres ≥ 16 assumed; PG 18 features
are marked.

### 5.1 Schema (`internal/placement/pg/schema/0001_placement.sql`)

```sql
-- Placement authority. Safety rests on: one CAS per change, epochs never decrease, owner changes only with an
-- epoch increase (knowledge/fencing/2026-11-05-safety-argument.md, step 1).
begin;

create domain database_key as text collate "C"
  check (octet_length(value) between 1 and 120);            -- owner.MaxKeyLen
create domain vm_id as text collate "C"
  check (value ~ '^[a-z0-9][a-z0-9.-]{0,62}$');
create domain epoch as bigint check (value >= 1);
create domain generation as bigint check (value >= 0);

create table vms (
  vm_id        vm_id primary key,
  endpoint     text not null,
  registered   timestamptz not null default now(),
  heartbeat    timestamptz not null default now(),       -- updated often: not indexed, so updates stay HOT
  dead_attested_at timestamptz,                          -- operator attests VM loss (M5); null = alive
  dead_attested_by text,
  check ((dead_attested_at is null) = (dead_attested_by is null))
) with (fillfactor = 70);

create table placement (
  database_id  database_key primary key,
  owner_vm     vm_id      not null references vms (vm_id),
  epoch        epoch      not null,
  generation   generation not null,
  state        text       not null check (state in ('ACTIVE', 'MOVING')),
  updated_at   timestamptz not null default now()
);
-- Placement policy and VM-loss restore ask "what does VM x own?"; FKs are not indexed automatically.
create index placement_owner_vm on placement (owner_vm);
-- The lifecycle controller resumes stuck moves; few rows are MOVING at a time.
create index placement_moving on placement (updated_at) where state = 'MOVING';

create table moves (
  database_id    database_key not null references placement (database_id),
  from_epoch     epoch        not null,
  from_vm        vm_id        not null references vms (vm_id),
  to_vm          vm_id        not null references vms (vm_id),
  ack_generation generation   not null,
  flipped_at     timestamptz  not null default now(),
  primary key (database_id, from_epoch),
  check (from_vm <> to_vm)
);

-- Invariants as a trigger: CHECK cannot compare old and new rows.
create function placement_epoch_monotonic() returns trigger language plpgsql as $$
begin
  if new.epoch < old.epoch
     or (new.owner_vm <> old.owner_vm and new.epoch <= old.epoch) then
    raise exception 'placement %: % at epoch % -> % at epoch %',
      old.database_id, old.owner_vm, old.epoch, new.owner_vm, new.epoch
      using errcode = 'check_violation', constraint = 'placement_epoch_monotonic';
  end if;
  return new;
end $$;

create trigger placement_epoch_monotonic
  before update on placement
  for each row execute function placement_epoch_monotonic();

create table schema_version (version int primary key, applied_at timestamptz not null default now());
insert into schema_version (version) values (1);

commit;
```

Choices, with the book's reason: `state` is a `CHECK` list, not an enum or lookup table (ch 25 prefers a
lookup table unless the set is truly fixed; this set is defined by `owner.PlaceState` and changes only with
code, so a `CHECK` that a migration rewrites is enough). Keys are `text collate "C"` so ordering and equality
are bytewise and identical on every host (ch 23). `bigint`, not `serial` (ch 23). No surrogate id on
`placement`: the natural key is the database key (ch 29, 31). `vms.heartbeat` is a hot, single-writer row per VM;
it stays unindexed and the table has `fillfactor = 70` so heartbeats are HOT updates (ch 9). Active-database
counts per VM are **computed** (`count(*) … group by owner_vm`, served by `placement_owner_vm`), never kept as a
counter column that every create would update (ch 37, 39: the hot-counter anti-pattern).

### 5.2 Queries (`internal/placement/pg/queries/`)

```sql
-- get.sql — Placement.Get. Primary only: Claim and Reconcile need a linearizable read.
select owner_vm, epoch, generation, state
  from placement
 where database_id = $1;
```

```sql
-- create.sql — CAS with expect.Epoch == 0. Zero rows returned ⇒ owner.ErrConflict.
   insert into placement (database_id, owner_vm, epoch, generation, state)
        values ($1, $2, $3, $4, $5)
   on conflict (database_id) do nothing
returning epoch;
```

```sql
-- cas.sql — CAS on an existing row; also records the flip when MOVING → ACTIVE on another VM.
-- One statement, one snapshot: the moves row exists iff the flip happened (ch 41 pattern).
-- Params: $1 key; $2..$5 expected owner, epoch, gen, state; $6..$9 next owner, epoch, gen, state.
with flipped as (
     update placement
        set owner_vm = $6, epoch = $7, generation = $8, state = $9, updated_at = now()
      where database_id = $1
        and owner_vm = $2 and epoch = $3 and generation = $4 and state = $5
  returning database_id
),
     recorded as (
     insert into moves (database_id, from_epoch, from_vm, to_vm, ack_generation)
          select database_id, $3, $2, $6, $8
            from flipped
           where $5 = 'MOVING' and $9 = 'ACTIVE' and $2 <> $6
       returning database_id
)
select count(*) from flipped;   -- 0 ⇒ owner.ErrConflict, 1 ⇒ success
```

The flip's audit row is derivable from `expect` and `next` (from-owner/epoch from `expect`, to-owner and
`Ack.Gen` from `next`), so the port needs no new field. A replayed flip after a lost reply finds the row
already changed, updates 0 rows, writes no second `moves` row, and `Mover` resumes from `Get` as today.

```sql
-- moving.sql — moves to resume (lifecycle controller). A table, not NOTIFY: this work must not be lost.
  select database_id, owner_vm, epoch, generation
    from placement
   where state = 'MOVING'
     and updated_at < now() - $1::interval
order by updated_at
   limit $2;
```

```sql
-- vm_load.sql — placement policy input: one set-based query, no per-VM loop (ch 5).
   select vms.vm_id, vms.endpoint, count(placement.database_id) as active_dbs
     from vms
          left join placement
                 on placement.owner_vm = vms.vm_id
                and placement.state = 'ACTIVE'
    where vms.dead_attested_at is null
 group by vms.vm_id, vms.endpoint
 order by active_dbs, vms.vm_id;
```

**Several controller instances.** Safety never depends on locks: two controllers racing a move both go
through `cas.sql`, and one gets `ErrConflict`. To avoid duplicate work only, a controller may claim stuck moves
with `select … for update skip locked` inside a short transaction, or hold `pg_try_advisory_lock` as a leader
lease. Neither is in the book (its only explicit lock is `LOCK TABLE … SHARE ROW EXCLUSIVE`, ch 41, too
coarse here); both are standard Postgres and are optional.

**PG 18 option.** `update … returning old.epoch, new.epoch` (ch 36) would let `cas.sql` return the previous row
on conflict for better error messages; not needed for correctness.

### 5.3 Request-id table in each tenant SQLite database (PG-09)

```sql
create table if not exists _tsdb_requests (
  request_id text    not null primary key,   -- client-supplied, validated at the front
  applied_ms integer not null,               -- unix ms, UTC (SQLite has no timestamptz)
  result     blob                            -- encoded reply, for an identical answer on retry
) strict, without rowid;
```

Per write, inside the write's savepoint (PG-01): `insert into _tsdb_requests (request_id, applied_ms) values (?,
?) on conflict (request_id) do nothing returning request_id`; no row returned ⇒ duplicate, `ROLLBACK TO` the
savepoint and answer from `result`. SQLite supports `RETURNING` since 3.35.

As built (CP-2): the claim drops `returning` and reads the rows affected (same single statement, ~1.7 µs less
per write through `database/sql`); `create index _tsdb_requests_applied on _tsdb_requests (applied_ms)` keeps
the retention sweep off a full scan; the table is created lazily in the first batch with an id. Details and
cost: `knowledge/front/2026-11-05-request-ids.md`.

## 6. Testing implications

Each item's tests follow `knowledge/testing/README.md` (spec tables with on/off points, contract suites against
fake and real, model fuzzing for state, env-gated integration). Additions:

| Item | New tests | Kind |
|---|---|---|
| PG-01 | Real-SQLite batch with `INSERT OR ROLLBACK` / `RAISE(ROLLBACK)`; fake `endsTxn` fault; `FuzzActorModel` op; sqlbench `compare-actor` before/after | integration, fault injection, model fuzz, load |
| PG-02 | `schema/test/constraints.sql` + `.expected`, run with `psql -X -v ON_ERROR_STOP=0 -a` and diffed (ch 8, Postgres's own regression style) | SQL regression |
| PG-03 | `ownertest` CAS contract suite vs fake and Postgres (`TSDB_PG_URL`); lock-timeout test; lost-reply test | contract, integration |
| PG-04 | Same-key CAS race, many-key move race, invariants query returns zero rows; latency numbers | concurrency, load |
| PG-05 | FK and type off-point tests; `smoke` run join-row stability | spec, load |
| PG-06 | DuckDB golden test; CSV header ⇔ `views.sql` test | regression |
| PG-08 | Deterministic-fill equality; `ramp` seed timing | spec, load |
| PG-09 | Exactly-once after crash/move; failed write leaves no id row | integration, model fuzz |
| PG-10 | Cached locator after a move; listener reconnect | integration |

Lines to add to `knowledge/testing/README.md` §3 (PG-11): (1) "SQL files are code: every embedded `.sql` file
has a regression test with expected output (G-20)." (2) "Postgres integration tests are gated on `TSDB_PG_URL`
and run in a fresh schema per test." (3) "Concurrency tests hit the **same** key with many workers; that is
where contention and CAS races show."

## 7. Out of scope / not adopted

- **Lowercase keywords for existing SQL.** The book prefers lowercase (ch 8). All current SQL (Go strings,
  `queries/*.sql`) is uppercase and consistent; rewriting it buys nothing. The guide (G-1) uses lowercase only for
  new Postgres `.sql` files under `internal/placement/pg/`, where the book's whole layout applies.
- **Business logic in stored procedures / PL/pgSQL** (ch 5): the control plane's decisions are pure Go functions
  with property tests (`owner.Step`, `nextMove`); the database keeps invariants (constraints, one trigger), not
  workflows.
- **Triggers that maintain counters or caches** (ch 39) and **materialized views** (ch 38): no derived data in
  the authority yet; `vm_load.sql` is cheap at the planned scale. Revisit if placement rows reach millions.
- **Extensions** (ch 43–52: hstore, trigrams, intarray, earthdistance, ip4r, hll, pgvector): no use case.
  `pg_stat_statements` (ch 9, 53) is adopted for the control-plane database when it is deployed (ops, not code).
- **Data-type tour beyond what is used** (ranges, multiranges, network types, ch 23): no validity periods yet.
  If leases are ever built (plan §3 step 8), a `tstzrange` with an exclusion constraint per database is the
  book's tool for "no overlapping leases".
- **Partitioning, Citus, federation** (ch 32, 33, 53): one control-plane database is the plan.
- **psql REPL setup** (ch 7) is a personal tool; the guide only keeps its two script-safety settings.
- **`json_populate_record` bulk upserts** (ch 40) and **MERGE** (ch 36): no batch sync of the authority exists.
