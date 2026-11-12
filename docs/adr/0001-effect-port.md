# ADR 0001 — Port the platform to Effect 3.22.2, with a pure `core/` and JSONL as the durable log

- **Status:** accepted
- **Date:** 2026-10-10
- **Deciders:** the plan at `docs/plans/completed/effect-port-and-observability.md`; recorded by plan item 02
- **Supersedes / superseded by:** none
- **Related:** [ADR 0002 — error taxonomy](0002-error-taxonomy.md)

## Context

The repo is a working document-db MVP: a typed TypeScript document API (`sdk/`), a SQLite writer and a DuckDB
reader in two OS processes (`platform/`), a tic-tac-toe sample app (`examples/tic-tac-toe/`), and a 20-test
regression suite. It works, and almost none of it is unit-testable.

Three facts drove the decision:

1. **Dependency injection is constructor arguments and module-level state.** `platform/server.ts` is a
   top-level-`await` script that opens databases, spawns a child process, registers signal handlers and starts
   `Deno.serve` as a side effect of being imported. Nothing in it can be exercised without doing all of it.
2. **Every failure is `throw new Error(string)`** — 51 sites — and `platform/server.ts` converts all of them
   into HTTP 400. A stale `_version` and a full writer queue are indistinguishable from a validation error to
   any client.
3. **Observability is `console.error` behind `TRACE_SQL=1`.** There is no way to ask "which plans actually
   run", "what is p95 for this handler", or "what happened for request X".

A fourth fact constrains every answer: `platform/duckdb.ts` carries a load-bearing comment. The reader process
never imports Node's SQLite, because DuckDB's `sqlite` extension and Node's SQLite are two copies of the
SQLite library in one address space. The two-process split exists for that reason alone.

## Decisions

### 1. Effect 3.22.2, not 4.x

Effect 4.0.0 shipped 2026-10-01 and 4.0.2 on 2026-10-07. The 3.x line's last release is 3.22.2 (2026-09-09),
and the surrounding ecosystem still peers on `effect@^3.22.2` (`@effect/platform` is at `0.97.2`). Porting a
working codebase onto a nine-day-old major is the wrong risk to take at the same time as the port itself. The
service, layer and tagged-error concepts carry forward, so revisiting 4.x after this phase is a mechanical
follow-up rather than a redesign.

The pin lives in `deno.json`'s import map as `"effect": "npm:effect@3.22.2"` and is locked in `deno.lock`.
Every module imports the bare specifier `"effect"`, never a versioned specifier.

### 2. No `@effect/platform` or `@effect/platform-node`

The whole HTTP surface is about 25 lines of regex routing over `Deno.serve`. Wrapping `Deno.serve` in
`Effect.acquireRelease` keeps the dependency surface at two packages (`effect`, `fast-check`) and avoids
pulling a Node-API platform implementation into a Deno process that already carries two native libraries it
must keep apart. If the HTTP layer later needs `HttpApi` or OpenAPI generation, adopting
`@effect/platform@0.97.2` is a self-contained change behind the `platform/http/` modules.

### 3. `core/` is pure, and the purity rule is mechanical

Nothing in `core/` may import from `platform/`, from `sdk/`, from Node built-in modules, or from native npm
packages, and nothing in `core/` may touch the `Deno` global. The one permitted dependency is `effect` itself:
`Data`, `Either`, `Option` and friends are pure data structures, and the taxonomy in `core/errors.ts` is built
on `Data.TaggedError`.

This is what makes the rest of the plan possible. Validation, plan compilation, row decoding, IPC framing, log
rotation and retention, and the event-log offset arithmetic all become total functions over plain data, which
is also what makes them property-testable. Item 26 adds a test that greps `core/` for violations so the
boundary cannot rot; item 02's narrower version of that check is `grep -rE 'from "(\.\./)*platform/' core/`
returning nothing.

The division of labour:

- `core/` — pure. Returns `Either`/`Option`; never performs I/O.
- `platform/` — effectful. Services, layers, and the only modules that touch the outside world.
- `sdk/` — the app-facing seam. Effect-typed; re-exports from `core/` where apps need the types.
- `examples/tic-tac-toe/` — business rules. After item 10, the tic-tac-toe rules themselves are a pure
  `examples/tic-tac-toe/rules.ts`, so the game logic is testable without a database at all.

### 4. JSONL is the durable log write path; DuckDB is a derived analysis sink

Workstream 4 wants an append-only event log; workstream 5 wants to run analytical SQL over it. The obvious
design — "mutations append straight into a DuckDB table" — is forbidden by the constraint above, because it
would put DuckDB in the writer process alongside Node's SQLite.

So: **every process writes rotating JSONL files** (`logs/<role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl`)
as its durable record, and the DuckDB event tables are _derived_ by ingesting those files. Ingestion runs in
the reader process, which already owns DuckDB. The consequences, all of them deliberate:

- The durable write path is an append to an open file descriptor. It cannot fail a mutation, and it adds no
  native-library risk to the writer.
- Analysis is a separate, restartable, idempotent step. Losing the DuckDB file loses no data; it is rebuilt by
  re-ingesting the JSONL tree.
- Ingestion needs a byte-offset state table (`_ingest_state`) and must be exactly-once. That is the hard part,
  and it is isolated in one item (21) with property tests rather than smeared across the write path.
- Only `INSERT` is ever issued against the event tables. `_ingest_state` is the single mutable one.

### 5. The writer process must not import DuckDB

Stated separately because it is the invariant most likely to be broken by a well-meaning later change.
DuckDB's `sqlite` extension loads its own copy of the SQLite library. Node's `node:sqlite` loads another. Two
copies of SQLite in one address space, both holding handles on the same database file, is undefined behaviour
— mismatched locking, WAL-index assumptions and page-cache state, with corruption as the failure mode rather
than an exception.

Therefore:

- `platform/sqlite.ts` (and anything the writer process imports) never imports `@duckdb/node-api`.
- `platform/duckdb.ts` and `platform/reader.ts` never import `node:sqlite` — which is also why
  `platform/duckdb.ts` keeps its own local `Trace` type rather than importing one from the writer.
- Anything the server needs from DuckDB — a query, an event-log ingest, an analysis query — it asks for over
  the reader IPC protocol frozen in `platform/services.ts` (`ReaderRequest` / `ReaderReply`). That protocol
  has an `ingest` and an `analysis` request kind for exactly this reason.
- The reader attaches the app's SQLite file `READ_ONLY` through DuckDB's extension, so the two processes never
  both hold a write handle.

Items 15, 16, 21 and 26 each carry a grep-level acceptance criterion enforcing this.

### 6. Service tags are declared once, up front

`platform/services.ts` declares a `Context.Tag` and an interface type for every service in the plan's §1.2
table before any implementation exists, and `core/errors.ts` declares the whole error taxonomy before any
module raises one. Both are deliberately _declarations only_. The tag union, the error payload shapes and the
service signatures are the expensive things to change later, because every other item codes against them;
freezing them in one reviewed commit is what lets the remaining items run concurrently.

## The six workstreams and the items that deliver them

| Workstream                                                       | Items                                      |
| ---------------------------------------------------------------- | ------------------------------------------ |
| 1 — Effect port (services, layers, errors, DI)                   | 01, 02, 08, 10, 15, 16, 17, 18, 19, 22, 26 |
| 2 — Testability (pure core, swappable interfaces)                | 04, 05, 06, 07, 09, 10, 20                 |
| 3 — Unit + fast-check property tests                             | 04, 05, 06, 07, 12, 13, 20, 21             |
| 4 — Event logging (append-only DuckDB, real Logger)              | 07, 11, 21                                 |
| 5 — Observability (DuckDB analysis, folder rotation + retention) | 06, 11, 21, 23, 24                         |
| 6 — Perf (k6 + DuckDB analysis)                                  | 03, 14, 25                                 |
| Docs and boundary enforcement                                    | 02, 23, 26, 27                             |

Read as prose, workstream by workstream:

1. **Effect port.** Item 01 pins dependencies and writes the complete task table. Item 02 (this ADR's own
   item) declares the error taxonomy and the service tags. Item 08 makes `platform/config.ts` the single
   environment boundary. Item 10 converts the SDK seam (`Execute`, `Documents`, `defineFunctions`, the app
   handlers, the client) to `Effect`. Items 15, 16, 17 and 18 turn the four adapters — SQLite writer, DuckDB
   reader, reader IPC client, object store — into scoped services. Item 19 does the same for backups. Item 22
   is the composition root: layers, HTTP, auth, scheduling and shutdown. Item 26 deletes the compatibility
   wrappers and adds the boundary tests.
2. **Testability.** Items 04 to 07 extract the pure modules (`core/schema.ts`, `core/ipc/framing.ts`,
   `core/logging/rotation.ts`, `core/events.ts`), item 08 removes the hidden `Deno.env` dependency, item 09
   makes the compiler total, item 10 makes the SDK injectable, and item 20 builds the test layers plus the
   conformance suite that keeps `StorageTest` honest against real SQLite.
3. **Unit and property tests.** Each extraction item ships its own unit tests (04, 05, 06, 07); items 12 and
   13 add the two fast-check waves over schema, rules, framing, rotation, and compiler plan invariants; item
   20 ports the 20-test regression net; item 21 property-tests ingest idempotence.
4. **Event logging.** Item 07 fixes the event taxonomy and redaction rules, item 11 builds the real Effect
   `Logger` plus the bounded-queue JSONL sink, item 21 builds the append-only DuckDB tables and the idempotent
   ingest.
5. **Observability.** Item 06 makes rotation and retention pure policy, item 11 applies it in the sink, item
   21 lands the tables, item 23 adds the named analysis catalog and the `logs` CLI, and item 24 adds the
   retention sweeper that actually deletes files.
6. **Perf.** Item 03 installs k6 and lands a smoke scenario against the current server, item 14 adds the load
   scenarios and output capture, and item 25 joins k6's client-side numbers to the server's own handler
   latencies in DuckDB.

**Docs and boundary enforcement** runs alongside: item 02 writes this ADR and ADR 0002, item 23 writes
`docs/observability.md`, item 26 adds the grep-level boundary tests, and item 27 regenerates `README.md` and
`core/compile/GENERATED_SQL.txt`.

## Consequences

**Good.** Failures become typed and carry structured fields, so HTTP statuses are correct and logs are
analysable (ADR 0002). The pure `core/` is property-testable. `ReaderClientTest` lets the entire HTTP surface
be tested in one process with no subprocess and no DuckDB. Scoped resources make shutdown ordering and
interruption-safe rollback expressible, which the current try/catch code cannot do at all.

**Costs.** Two new runtime dependencies and a learning curve. Every module gains an `Effect` in its signature,
so the port cannot be partial — hence the explicit compatibility wrappers in items 04, 09 and 10 and their
removal in item 26. The behaviour changes called out in the plan (`ConflictError` → 409, `QueueFullError` →
503), plus `RequestTooLarge` → 413 and `BackupAlreadyRunning` → 409 (see ADR 0002), are visible to clients.

**Deferred.** Effect 4.x. `@effect/platform`. Deterministic simulation testing, which the pure `core/` plus
`StorageTest` makes reachable but which is explicitly future work.

## Alternatives considered

- **Keep `throw`, add a status map keyed on message regexes.** Cheapest, and it is what the code already
  half-does (`sdk/client.ts` reads `body.error`). Rejected: message matching is exactly the fragility this
  plan exists to remove, and it does nothing for testability.
- **Hand-rolled DI (pass a context object through every call).** No dependency, no learning curve. Rejected:
  it gives none of scoped resources, interruption-safe finalizers, structured concurrency, or a typed error
  channel — and those, not the injection, are the hard parts here.
- **Mutations append to DuckDB directly.** One write path, no ingest step. Rejected by the
  two-copies-of-SQLite constraint in decision 5.
- **A third process owning DuckDB for the event log.** Would isolate the native library cleanly. Rejected as
  unnecessary: the reader process already owns DuckDB and is already reachable over a JSON-lines protocol.
