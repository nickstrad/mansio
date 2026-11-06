# 2026-11-05 — request ids: a write applies at most once per database (CP-2, PG-09)

Author: CP-2 agent. Amends [the wire contract](2026-11-05-wire-contract.md) (owned by the front agent; not
edited) and contract 3 in `shared_state.md`. Design source: [art-of-postgresql plan §5.3](../../docs/books/art-of-postgresql/plan.md),
[control-plane plan CP-2](../../docs/control-plane-plan.md). Code: `internal/actor/reqid.go`, `actor.go`
(`apply`, `sweep`), `internal/front/executor.go`, `server.go` (`Response.Duplicate`), `request.go`
(`ReservedPrefix`). Tests: `internal/actor/reqid_test.go`, `FuzzActorModel` (request-id op),
`fronttest.RunExecutorSuite` (two new cases), `TestDuplicateRequestIDAnswersOK`.

## Decision: a duplicate answers `ok` with the original's count and `"duplicate": true`

We had two choices for a write whose `request_id` the database already applied: replay the original outcome,
or return a bare "already applied" ok. We do both in one answer: code `ok` (200), `count` = the rows the
original write affected (stored with the id), and `"duplicate": true`. The statement does not run again.

Why:

- **The client's retry loop stays simple.** Every path that says "a write may have committed" (`expired` with
  `ambiguous: true`, `internal`, a dropped connection, a reply lost in a move) ends the same way: retry with
  the same `request_id` until a definite answer. The definite answer is `ok` whichever attempt applied it, and
  the count is the same as the first attempt would have returned. A client that doesn't care ignores the flag.
- **`duplicate` is information, not a different outcome.** Logs and tests can tell "applied now" from
  "applied before"; a client that wants to know whether its retry was redundant can.
- A separate code (e.g. `already_applied`) would be a twelfth code every client must handle and would make a
  successful write look like a failure.

The stored result is the reply, not the statement: a duplicate is answered from the record even if running
the statement now would fail (e.g. a retried INSERT whose row exists — the original's).

## Semantics, code by code

| first attempt answered | id row? | retry with the same `request_id` |
|---|---|---|
| `ok` | yes | `ok`, `duplicate: true`, original `count`; nothing runs |
| `expired`, `ambiguous: true` / `internal` / no answer | if it committed | `ok` + `duplicate` if it committed, otherwise it applies now |
| `expired` (not ambiguous), `shed`, `unavailable`, `wrong_owner`, `fenced`, `ticket_invalid` | no (never ran) | applies normally |
| `aborted`, `stmt_timeout`, `bad_request` (statement rejected) | no (rolled back with the write) | runs again: same input usually fails the same way |

Rules:

- **Scope: one database.** Ids live in the database (`_mansio_requests`), so they travel with snapshots,
  write-backs and moves: a retry reaches whichever owner holds the database and is answered there. The same id
  on two databases is two requests.
- **Writes only.** `request_id` stays required for writes and optional for reads; reads ignore it.
- **Only applied writes are remembered.** The id is claimed inside the write's savepoint before the statement
  runs; any failure rolls back the claim with the write (PG-01), so a failed write leaves its id free.
- **Retention:** `actor.Config.RequestTTL`, default 24 h. A retry after the TTL applies again. The TTL must
  outlast every client retry window, including a stalled move (CP-4's gate: no duplicate write during a move).
- **Same id, different payload:** not detected. The second request gets the first's answer. Clients must not
  reuse ids (frontbench now generates `fb-<run>-<seq>`). A payload hash in the row would let us answer
  `bad_request` instead; not done (no client reuses ids on purpose, and it costs a hash per write).
- **Reserved names:** client SQL naming `_mansio_` anything is `bad_request`, so a client cannot read, forge or
  drop ids.

## Storage

```sql
create table if not exists _mansio_requests (
  request_id text not null primary key, applied_ms integer not null, result blob
) strict, without rowid;
create index if not exists _mansio_requests_applied on _mansio_requests (applied_ms);
```

Per write with an id, in its savepoint: `insert … on conflict (request_id) do nothing` (one row affected ⇒
new; none ⇒ duplicate, answered from `select result …`), the statement, then `update … set result = ?`.
`result` is a version byte and the rows affected as a varint, so a handler executor can store a richer reply
later under a new version.

Departures from the plan's §5.3, both measured or reasoned:

- The claim reads `sqlite3_changes()` instead of `returning request_id`: the same single-statement decision,
  but RETURNING needs a query cursor through `database/sql`, ~1.7 µs more per write.
- An index on `applied_ms`: without it, each retention sweep scans the whole table under the write lock
  (24 h × 100 writes/s ≈ 8.6 M rows). It costs ~1 µs per write.
- The table is created lazily, inside the first batch with an id after each open, not at open: opening a
  fetched generation never changes its file (the generation cache's hash check stays valid).

Retention is one statement per sweep, `delete … where request_id in (select … where applied_ms < ? order by
applied_ms limit 1000)`, run in its own savepoint at the start of a write batch at most once per
`RequestSweep` (default 1 min), again in the next batch while a sweep hits the limit. A failed sweep never
fails the batch's writes. Idle databases are not swept (they gain no rows).

## Cost

`BenchmarkWritesWithRequestID` (`go test ./internal/actor -run '^$' -bench WritesWithRequestID -benchtime 2s
-count 3`), Apple M5 Max, Go 1.26.4, modernc.org/sqlite v1.61.1, WAL, synchronous NORMAL, 16 parallel
writers, one database, `INSERT` of a new row per write, ids `req-<n>`; 2026-11-05:

| | no id | with id |
|---|---|---|
| max_batch 64 | 1.57 µs/write | 5.56 µs/write |
| max_batch 1 | 9.9 µs/write | 23.6 µs/write |

Two extra statements and two more B-tree pages (table, index) per commit. That is 180k writes/s on one
database at batch 64; HTTP alone costs 20–38 µs per request (front README), so through agentd the id is a
small share. Variants measured on the way (batch 64): with RETURNING 7.2 µs; RETURNING, no index 6.2 µs;
claim without the result update 3.2 µs (not correct: no answer for a duplicate).

## Gate (CP-2) and where it is tested

- Same id after a crash: `TestRequestIDAfterCrash` — a child process applies the write and `os.Exit`s without
  closing (the WAL is left behind); the parent reopens and retries: duplicate, applied once.
- Same id after a move: `TestRequestIDAfterMove` — snapshot (VACUUM INTO) → a new actor on the copy; the
  retry is a duplicate; a write the old owner applied after the snapshot is not in the copy and applies there.
- A failed user statement leaves no id row: `TestFailedWriteLeavesNoRequestID` (constraint failure, a write
  that ends the transaction plus its aborted batch-mate, a statement timeout).
- `FuzzActorModel` has a request-id op (four ids over INSERT OR REPLACE / INSERT / DELETE) and checks after
  each flush that the database's id rows equal the model's applied ids. 60 s run, 38k execs, no failures.
- The end-to-end form (claim → write over HTTP → kill -9 → restart → retry) needs CP-1's wiring; the actor
  tests above cover the database half.

## Follow-ups (not done)

- **Per-batch claims** (review, 2026-11-05): look up all of a batch's ids in one `select … where request_id in
  (…)` and insert the applied ones in one multi-row statement before COMMIT: 2 statements per batch instead of
  2 per write. It moves the claim out of each write's savepoint, so PG-01's per-write rollback must then skip
  failed writes explicitly; worth it only if the +4 µs/write shows up through agentd.
- **Enforce `_mansio_` in the actor**, not only in front's lexical guard (a SQLite authorizer, if modernc exposes
  one), before callers other than front send client SQL (the handler executor).
- **Payload hash** in the id row, to answer `bad_request` when a client reuses an id for a different write.
