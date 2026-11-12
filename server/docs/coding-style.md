# Coding style for mansio-server

Status: adopted · 2026-11-05 · Read this before planning or writing code. It is the short version. Each section
links to the full guide, which wins on detail.

| Source | Full guide | Covers |
|---|---|---|
| House Go style (Aniche ch. 4, Cox-Buday ch. 4–5) | `~/AI/writeups/go-coding-style.md` | layout, domain vs integration, contracts, errors, logging, concurrency |
| *Effective Software Testing* (Aniche) | [docs/notes/testing/README.md](notes/testing/README.md), `~/AI/writeups/go-testing.md` | test levels, fakes, contract suites, model fuzzing |
| *Concurrency in Go* (Cox-Buday) | [docs/notes/concurrency-in-go/README.md](notes/concurrency-in-go/README.md) | Result pattern, panic forwarding, error propagation |
| *Release It!* (Nygard) | [books/release-it/guide.md](books/release-it/guide.md) | timeouts, retries, bulkheads, breakers, health, fault harness |
| *The Art of PostgreSQL* (Fontaine) | [books/art-of-postgresql/guide.md](books/art-of-postgresql/guide.md) | SQL for SQLite, DuckDB, Postgres |
| *Object Storage as Infrastructure* | [books/object-storage/guide.md](books/object-storage/guide.md) | S3 key layout, conditional writes, integrity, GC |

All books: [books/README.md](books/README.md). Refactor plans per book: `docs/books/<book>/plan.md`.

## 1. Structure: domain vs integration

- **Domain packages decide; they do no I/O and do not log.** No `net/http`, `os`, `database/sql`, minio, or
  `slog` in them. They reach the world through small interfaces they define (ports).
- **Decisions are pure functions.** `plan(have, want) → moves`, `durable.CheckPublish(prev, next)`,
  `durable.PlanGC(...)`, `bench/plan.go`. A thin layer around them does the I/O. Test the pure part with tables.
- **Adapters are thin.** `objstore` (S3), the Postgres placement adapter, `cmd/*` adapters: translate, validate,
  wrap errors, log. No business rules.
- **Wire only in `cmd/`.** `main` calls `realMain(ctx, args, stdout, stderr) int`. Exit 0 ok, 1 expected error
  with log ID, 2 usage, 70 bug, 130 interrupted.
- **Generated SQL lives in pure packages** that return text + args (`internal/schema`, `internal/workload`).
  Standalone SQL lives in `.sql` files, embedded with `//go:embed` (SQL G-1).
- No package-level mutable state; no `utils`/`models` packages; a `doc.go` names each package's role and error
  module.

Full: `go-coding-style.md` §1–2.

## 2. Validation vs contracts

- **Validate once at the edge** (config loaders, handlers, adapters) and return errors. **Never assert on input.**
- **Contracts are for our own bugs**: `assert.That` (`internal/assert`) panics. Write them in doc comments:
  `pre:`, `post:`, `Invariant:`. Run `invariant()` after mutations, never on reads; guard O(n) checks with
  `if assert.Enabled`.
- **Checked wrappers** on interfaces any future adapter could break (e.g. `vactor.checkedFetcher`).
- Never put assertions in a `defer` that also runs while a panic unwinds.
- Each assertion must be able to fail in some test. Both builds pass: default and `-tags noassert`.

Full: `go-coding-style.md` §3, `docs/notes/testing/README.md` §2.

## 3. Errors and logging

- **Wrap once at the package boundary**: `errs.Wrap(module, err, "what failed; what changed; whether retry is safe")`.
  `Error()` is `message: cause`; never `%v` the cause into your message.
- **Sentinels with `errors.Is`, typed errors with `errors.As`.** Never branch on message text. Classify storage
  errors with `objstore.Classify` (Transient / Precondition / NotFound / Permanent / Canceled).
- **Three error classes, never mixed** (Release It F2): dependency unavailable → `503 + Retry-After`, WARN,
  sampled; bug → `internal` + log ID, ERROR; bad input → `400`. An S3 or Postgres outage is never `internal`.
- **Log once, at the top or in the adapter**: `logx` JSON, UTC, log ID, whole chain, origin stack. Do not log and
  return. One stable message code per operational event (`S3.BREAKER_OPEN`, `OWNER.SUPERSEDED`).

Full: `go-coding-style.md` §4–5, `concurrency-in-go/README.md`, Release It §F, §O.

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

Full: `go-coding-style.md` §6, `concurrency-in-go/README.md`, Release It T5, B5.

## 5. Integration points: timeouts, retries, backpressure

Every call that leaves the process (S3, Postgres, HTTP, local fsync) and every queue follows these. Full:
[release-it/guide.md](books/release-it/guide.md).

- **T: every remote call has a deadline**, derived per call from a root ctx. Never pass `context.Background()`,
  `h.ctx` or a ticker ctx straight to I/O. Bound bodies by time and size (stall timeout). Inner deadlines are
  shorter than outer ones; never retry past the caller's deadline. Pool and semaphore acquires take the ctx.
- **R: one retry layer per call chain.** Library retries off (`minio.Options{MaxRetries: 1}`). Inside `objstore`,
  only idempotent requests (GET, HEAD, LIST, DELETE) retry, with jittered exponential backoff (100 ms ×2, 5 s cap,
  ≤ 5 attempts). **Writes are never blindly retried: an ambiguous write is resolved by re-reading** (§6 W, R3).
  Client retries use full jitter, honor `Retry-After`, and spend a windowed budget. After a failed activation a
  key fails fast for a short jittered backoff.
- **B: every queue is bounded and sheds when full; nothing blocks the caller.** Each dependency has its own
  concurrency limit sized to its measured knee (S3 fetch 16 / 1 GiB in flight). Caps nest db ≤ tenant ≤ VM.
  Health and lifecycle never queue behind queries. Bound every result set and listing.
- **C: a circuit breaker per remote dependency** (`internal/breaker`). Only system failures trip it (timeouts,
  connection errors, 5xx); `ErrNotFound`/`ErrStale`/`ErrConflict` are answers. Open → fail fast with
  `ErrUnavailable` and a retry hint. Transitions logged and exposed.
- **S: everything that accumulates has a reaper** (generations → GC, cache → eviction, fenced records →
  `Forget`). Background jobs each have a deadline; a tick never overlaps the next; track oldest-unfinished age.
- **H: startup validates, opens, then serves each key as soon as it is reconciled.** Liveness ≠ readiness;
  readiness runs a real canary. Remote breakers (S3, placement) never fail readiness; they are metrics/alerts. Shutdown has one total budget.
- **Per-dependency defaults** (S3, Postgres, inbound HTTP, client, actor) are in Release It §P. A new
  integration point adds a row to its table and a sheet in §P.

## 6. Object storage

Full: [object-storage/guide.md](books/object-storage/guide.md). The book's code listings are buggy; follow the
guide, not the book.

- **K: one database owns one prefix**, built only by `durable.KeyPrefix(key)` (hash shard + escaped key). Never
  `"dbs/" + key`. Compute names; **no LIST on a request path** (only GC, `dbtool`, setup).
- **I: everything except `manifest.json` is create-only** (`If-None-Match: *`). Never reuse a tried number. Write
  data first, publish the pointer second, trust only the pointer. Objects carry metadata (`sha256`, `epoch`,
  `generation`, `key`) to rebuild it.
- **W: every write to a mutable object is conditional** (`If-Match` / `If-None-Match: *`) **and carries the
  fencing epoch** (`durable.CheckPublish`, `CheckStamp`). Every publish changes the bytes with strictly
  increasing `(epoch, generation)`. 412 and 409 both mean stale. **No clocks in correctness.** Coordination
  lives in Postgres, not S3.
- **R3/R4: after an ambiguous write, re-read before deciding**; set sticky `stale`/`superseded` only when the
  re-read shows another writer.
- **V: verify size + SHA-256 on every generation read** before use; leave no file on failure. `Content-MD5` on
  uploads.
- **GC: never delete the current, pending, or K retained generations.** Orphans only after the manifest was
  rewritten since their upload, plus a grace window. GC never writes the manifest and never crosses a prefix.
  Drop = placement tombstone first, then delete the prefix.
- **X: run `TestS3Compat` against every S3 backend/version before use**; record results in `docs/notes/writeback/`.
- One long-lived client per process; scoped credentials per component; TLS outside dev; metered requests.

## 7. SQL

Full: [art-of-postgresql/guide.md](books/art-of-postgresql/guide.md) (rules G-1 … G-23, tagged
[all]/[sqlite]/[duckdb]/[pg]).

- **Values are always bind parameters.** Formatted identifiers come only from generator word lists, never from
  requests. Build connection strings (DSNs, `file:` URIs) in one place.
- **The connection owner owns the transaction**; callers never send `BEGIN`/`SAVEPOINT`/`PRAGMA`.
- **Know what an error does to the transaction.** Wrap each grouped write in `SAVEPOINT`/`RELEASE`; if
  `ROLLBACK TO` fails, the transaction is gone — stop (SQLite `OR ROLLBACK` / I/O errors silently end it).
- **[pg] One statement over many**: CTEs + `returning`, read committed; a conditional `UPDATE` matching 0 rows is
  a CAS conflict. Upserts use `on conflict`, never select-then-insert. Locks and LISTEN/NOTIFY only save work;
  correctness comes from CAS + constraints.
- **Invariants in the schema**: `not null`, `check`, keys, FKs (index the referencing side). **[sqlite]**
  `PRAGMA foreign_keys = ON` and `STRICT` tables. **[pg]** `timestamptz`, `bigint`, `text collate "C"` keys,
  domains; every pool sets `lock_timeout`, `statement_timeout`, `idle_in_transaction_session_timeout` and
  `application_name`.
- **Migrations**: numbered, transactional files, never edited after applying.
- **Layout**: one clause per line, explicit joins, no `order by 2`, explicit insert column lists; set-based over
  loops; comments explain intent. Existing uppercase SQL stays uppercase; new Postgres files are lowercase.

## 8. Testing (what every change needs)

Full: [docs/notes/testing/README.md](notes/testing/README.md), `~/AI/writeups/go-testing.md`.

1. **Spec tables** per partition with on/off points of every boundary.
2. **Structural**: read uncovered blocks; error branch → inject with a fake; impossible → assertion; dead → delete.
3. **Fakes with fault switches** in `<pkg>test` packages; **one contract suite run against every
   implementation** (fake, real, and real-through-`faultnet`/`s3test` for adapters — Release It H4 fault table).
4. **Model-based fuzzing** for anything stateful; commit failing inputs to `testdata/fuzz/`.
5. **Adapter integration tests** including failure paths and error wrapping (gated: `COLDLOAD_S3`, `MANSIO_PG_URL`).
6. **Concurrency**: `-race`, many workers on the same key for CAS paths.
7. **SQL**: every `.sql` file has a golden-output regression test; each constraint has an off-point test.
8. **Under load and over time**: fault modes during a frontbench/wbbench run before calling a component done.

## 9. Checklist before done

- [ ] Domain code does no I/O and does not log; wiring only in `cmd/`
- [ ] Input validated at the edge; contracts documented and asserted where impactful
- [ ] Errors wrapped once at the boundary; messages say what changed and whether retry is safe; error class is
      right (unavailable / bug / bad input)
- [ ] Every remote call has a deadline; one retry layer; every queue bounded and shedding
- [ ] Object storage: names via `KeyPrefix`, conditional + fenced writes, re-read after ambiguity, verify hashes
- [ ] SQL: bind parameters, savepoints around grouped writes, constraints in the schema, golden tests
- [ ] Tests per §8; `gofmt -l .`, `go vet ./...`, `go test -race ./...`, `go test -tags noassert ./...` pass
- [ ] New integration point or storage layout → update the matching guide (and its contract test)
