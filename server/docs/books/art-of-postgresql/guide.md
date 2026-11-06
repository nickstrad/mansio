# SQL guide for mansio-server

Status: proposed (adopt with [plan.md](plan.md) PG-11) · Date: 2026-11-05 · Source: _The Art of PostgreSQL_
(Fontaine), chapter numbers as in `~/AI/books/art-of-postgresql/chapters/`.

Rules agents follow whenever they write or generate SQL in this repo. Each rule: **rule** → why → example.
Scope tags: **[all]** SQLite, DuckDB and Postgres · **[sqlite]** · **[duckdb]** · **[pg]** Postgres only.
Where a rule conflicts with `~/AI/writeups/go-coding-style.md` or `knowledge/testing/README.md`, those win;
this guide only adds SQL specifics.

## Where SQL lives

**G-1 [all] Standalone SQL lives in `.sql` files; generated SQL lives in one pure Go package.**
Why: SQL in files is versioned, commented and runnable as-is in `psql`/`duckdb`/`sqlite3` (ch 6). Generated SQL
(sqlbench schemas and workloads) stays in pure packages that return text + args and do no I/O
(`internal/schema`, `internal/workload`), so it can be table-tested.
New Postgres files use lowercase keywords and right-aligned clauses (ch 8); existing uppercase SQL stays as is
(do not mix within one file).

```go
//go:embed queries/*.sql
var queries embed.FS // internal/placement/pg: one named query per file, loaded once at start
```

**G-2 [pg] One query per file, named after its port method, with a header comment.**
Why: a query seen in logs or `pg_stat_statements` maps to one file (ch 6, 8). The header says what the query
guarantees and what "0 rows" means.

```sql
-- cas.sql — Placement.CAS. 0 rows ⇒ owner.ErrConflict. One statement: flip and moves row are atomic.
```

**G-3 [pg] Schema changes are numbered, transactional migration files, applied once.**
Why: Postgres DDL is transactional (ch 5), so a file is all-or-nothing; the same files build dev, CI and prod
(ch 28). `internal/placement/pg/schema/NNNN_name.sql`, wrapped in `begin; … commit;`, recorded in
`schema_version`. Never edit an applied file; add the next one.

## Parameters and connections

**G-4 [all] Values are always bind parameters; never format a value into SQL text.**
Why: the driver sends text and args separately, so a value can never be parsed as SQL (ch 2, injection), and
identical text reuses the actor's prepared-statement cache (`internal/actor/conn.go:103`).
Identifiers that must be formatted (generated table/column names) come only from the generator's own word
lists (`internal/schema/schema.go:90-98`), never from requests.

```go
fmt.Sprintf("UPDATE %s SET %s = ? WHERE id = ?", t.Name, c.Name) // ok: generated identifiers; value is ?
```

**G-5 [all] Build connection strings in one place.**
Why: a hand-concatenated `file:` URI opens a different file when the path has `#` or `?`
(`internal/actor/conn.go:81-92`; [PG-07](plan.md#4-refactor-items)). Postgres pools set
`application_name=<binary>/<module>` (e.g. `controld/placement`) so every session is traceable (ch 8).

**G-6 [pg] Every pool sets timeouts; safety-path pools never lower durability.**
Why: a session idle in a transaction holds row locks and blocks every CAS on that row (ch 37). Set
`lock_timeout`, `statement_timeout`, `idle_in_transaction_session_timeout`; a timeout is "unavailable", not a
conflict. Never `synchronous_commit = off` on the placement pool (ch 33).

## Transactions and errors

**G-7 [all] The owner of the connection owns the transaction; callers never send transaction control.**
Why: boundaries are a design decision (ch 42). The actor owns `BEGIN IMMEDIATE … COMMIT`
(`internal/actor/actor.go:311`); the front rejects `BEGIN`/`SAVEPOINT`/`PRAGMA` (`internal/front/request.go:172`).

**G-8 [all] Know what an error does to the enclosing transaction; isolate statements with savepoints.**
Why: in Postgres any error aborts the transaction and `COMMIT` becomes `ROLLBACK` (ch 37); in SQLite
`OR ROLLBACK`, `RAISE(ROLLBACK)` and I/O errors end it, and later statements silently autocommit. Wrap each
grouped write in `SAVEPOINT`/`RELEASE`; a failed `ROLLBACK TO` means the transaction is gone — stop
([PG-01](plan.md#4-refactor-items)).

**G-9 [pg] Prefer one statement to a multi-statement transaction; use read committed.**
Why: one statement sees one snapshot (ch 5). Compose with CTEs and `returning` (ch 36, 41). Under read
committed a racing conditional `UPDATE` waits, re-checks its `WHERE`, and updates 0 rows — exactly a CAS
conflict (ch 37). Raise isolation only for a named reason, and then add a retry loop for SQLSTATE `40001`.

**G-10 [all] Upserts use `on conflict`; never "select, then insert" or "update, if not found insert".**
Why: the two-step form races (ch 39); `on conflict … do nothing returning` tells you in one statement whether
you inserted.

```sql
insert into _mansio_requests (request_id, applied_ms) values (?, ?)
on conflict (request_id) do nothing returning request_id   -- no row ⇒ already applied
```

**G-11 [pg] Correctness never depends on locks or notifications; they only save work.**
Why: CAS + constraints are the safety. `for update skip locked` and advisory locks (not in the book; standard
Postgres) may de-duplicate controller work. LISTEN/NOTIFY only invalidates rebuildable caches — listen first,
then reload, then apply; never a queue (ch 40).

## Schema and constraints

**G-12 [all] Invariants go in the schema: `not null`, `check`, keys, FKs, and a trigger only when old and new
rows must be compared.**
Why: the database enforces them whichever code path writes (ch 10, 29). Required columns are `not null`
(ch 17). Index the referencing side of every FK yourself (ch 29). A trigger that rejects an update raises a
named constraint so Go can tell "bug" from "conflict" (plan §5.1).

**G-13 [sqlite] Make SQLite enforce what the DDL says: `PRAGMA foreign_keys = ON` and `STRICT` tables.**
Why: without them `REFERENCES` and column types are decoration (ch 22 translated;
[PG-05](plan.md#4-refactor-items)). Repo-owned tables use `strict` and,
for natural text keys, `without rowid`.

**G-14 [pg] Types: `timestamptz`, `bigint`, `text collate "C"` for keys, domains for repeated rules.**
Why: same cost, fewer surprises (ch 23): `timestamptz` stores UTC; `serial` overflows; byte-order collation is
identical on every host. A domain states a rule once (`database_key`: 1..120 bytes, matches `owner.MaxKeyLen`).
**[sqlite]** times are `integer` unix ms UTC, named `*_ms`. Never put uniqueness on a timestamp (ch 37).

**G-15 [all] No hot counter rows; compute aggregates.**
Why: a counter column every write updates serializes writers (ch 37, 39). Count with `group by` over an
indexed column (plan §5.2 `vm_load.sql`); cache only with a measured need and a stated staleness bound (ch 38).

## Writing queries

**G-16 [all] Layout: one clause per line for multi-line SQL, explicit `join … on/using`, no `natural join`,
no positional `order by 2`, explicit insert column lists, aliases named like variables.**
Why: least astonishment; changing a select list must not change ordering (ch 8).

```sql
-- no:  SELECT kind, COUNT(*) FROM t GROUP BY kind ORDER BY 2 DESC
SELECT kind, COUNT(*) AS n FROM t GROUP BY kind ORDER BY n DESC
```

**G-17 [all] Set-based over loops: one query returns exactly what the caller needs.**
Why: N small queries cost N round trips and N snapshots (ch 5). Bulk work uses `insert … select` or multi-row
`values` in one transaction (ch 36; `cmd/coldload/template.go:25`).

**G-18 [all] Comments explain intent, not syntax.**
Why: readers should never guess why a filter exists (ch 8). DuckDB report sections keep their `.print`
headline stating the question answered.

**G-19 [duckdb] Analysis reads CSVs through one typed view file.**
Why: one definition of each input (ch 38); explicit `columns = {...}` instead of type sniffing
([PG-06](plan.md#4-refactor-items)). Use `filter`,
`qualify`, `nullif` for divide-by-zero (ch 16, 18).

## Testing SQL

Follow `knowledge/testing/README.md`; SQL adds:

**G-20 [all] Every `.sql` file has a regression test with expected output.**
Why: known input → known output lets you rewrite queries safely (ch 8). Postgres: `psql -X -v ON_ERROR_STOP=1
-a -f test.sql | diff - test.expected` in a fresh schema. DuckDB: fixture CSVs + golden report. Update
golden files with `-update`, and review the diff.

**G-21 [pg] Each constraint has an off-point test; each port adapter passes the fake's contract suite.**
Why: a constraint that never fired in a test may not exist (ch 29 + Aniche on/off points). Postgres tests are
gated on `MANSIO_PG_URL`, like `COLDLOAD_S3`.

**G-22 [all] Concurrency tests put many workers on the same key.**
Why: contention and CAS races only show there (ch 37). Record latency at 1/16/64 workers in `knowledge/<topic>/`.

**G-23 [pg] Try changes in `begin; … rollback;` scripts first.**
Why: each run starts clean (ch 28, 39). In scripts use `psql -X -v ON_ERROR_STOP=1`; never
`ON_ERROR_ROLLBACK` outside an interactive session (ch 7). For manual fixes: `begin`, DML with `returning`,
check, then `commit`, and filter on the natural key as well as the id (ch 36).
