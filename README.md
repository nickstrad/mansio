# Mansio — a TypeScript document API over a Go SQLite server

**Application authors write schemas and document handlers, not SQL.** Handlers run in the caller's process, in
the TypeScript SDK. The SDK turns their database work into requests of a small, versioned protocol (the
intermediate representation, or IR, [`ir/spec/v1.md`](ir/spec/v1.md)) and sends them to the Go server, which
validates them, compiles them to SQLite SQL and runs them with one goroutine per database.

Step-by-step setup: [docs/guide.md](docs/guide.md). Learning paths: [docs/learning/](docs/learning/README.md).

## Repository layout

| Path                    | What it is                                                                                                                                                                                                                                   |
| ----------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `examples/tic-tac-toe/` | The tic-tac-toe sample: `schema.ts` (typed fields and indexes), `functions.ts` (queries and mutations, no SQL), rules.                                                                                                                       |
| `sdk/`                  | Schema builder, `defineFunctions`, the document API (`db.ts`), the client (`client.ts`, `recording.ts`, `transport.ts`).                                                                                                                     |
| `core/`                 | Pure code: schema validation, query plans, compilation to SQL (`compile/`), tagged errors, IR types.                                                                                                                                         |
| `ir/`                   | The IR protocol spec and the shared vectors both implementations are tested against.                                                                                                                                                         |
| `scripts/explain.ts`    | The compiler's SQL for both layouts (`core/compile/GENERATED_SQL.txt`).                                                                                                                                                                      |
| `server/`               | The Go server (module `github.com/nickstrad/mansio/server`): `agentd`, `controld`, `edge`, `mansioctl`, `dbtool` and their tests.                                                                                                            |
| `tests/`                | Unit and property suites; `tests/e2e/platform/` is the Go server's conformance suite, driven through the SDK.                                                                                                                                |
| `perf/`                 | k6 load scripts against agentd's `/v1/ops`. See [perf/README.md](perf/README.md).                                                                                                                                                            |
| `docs/`                 | The [user guide](docs/guide.md), [learning paths](docs/learning/README.md), ADRs, testing and observability references, design notes, and the architecture diagram site (`docs/pages/`, published at <https://nickstrad.github.io/mansio/>). |
| `scripts/dev/`          | Scripts that run the CI gate and the e2e suite in Linux containers.                                                                                                                                                                          |

The Go binaries:

- **`agentd`** serves `POST /v1/ops`. In **static mode** it serves the SQLite files of a local directory; in
  **durable mode** (an `s3` section) each database is a chain of immutable generations in S3, written back
  periodically and on shutdown. Callers authenticate with Ed25519-signed tickets, or, with a `tenancy`
  section, with app tokens (`off`), tenant API keys (`api-key`) or API keys plus email/password sessions
  (`session`). An optional `event_log` section writes the request log.
- **`controld`** is the control plane for multi-node deployments: placement, the locator, the ticket issuer.
- **`edge`** is the front door that forwards to agents.
- **`dbtool`** inspects, verifies, restores and imports the durable copies in S3.
- **`mansioctl`** is the admin CLI for a running platform: tenants and keys, database and VM lifecycle,
  limits, and one agent's runtime controls.

The config reference for each is its Go config type (`server/cmd/agentd/config.go`, with one comment per
field) and the sample configs in `server/configs/`. [server/README.md](server/README.md) describes the
server's commands and packages.

## Verification

```sh
scripts/dev/deno-linux.sh 'deno task ci'   # fmt:check, lint, check, test, test:prop
deno task go:ci                                  # gofmt, go vet, go test -race, go test -tags noassert
scripts/dev/e2e-linux.sh                   # the e2e suite on Linux, with Postgres and SeaweedFS
deno task explain > core/compile/GENERATED_SQL.txt            # regenerate the compiler artifact
```

Run `deno task ci` through `deno-linux.sh`, which runs a command in a clean Linux container with this checkout
mounted: natively on macOS one property test (`tests/property/json_number_test.ts`) fails, because SQLite
there rounds a huge JSON double differently. `e2e-linux.sh` cross-compiles `agentd`, `dbtool`, `controld`,
`edge` and `mansioctl`, starts Postgres and SeaweedFS sibling containers, and runs `tests/e2e/platform/` in a
Deno container. [docs/testing.md](docs/testing.md) describes the suites.

## App-facing client

The client takes the functions object and the schema as values, runs a handler locally and sends its database
work to agentd as IR requests.

```ts
import { Effect } from "effect";
import { createClient } from "./sdk/client.ts";
import { functions } from "./examples/tic-tac-toe/functions.ts";
import { schema } from "./examples/tic-tac-toe/schema.ts";

const client = createClient({
  functions,
  schema, // in the database's layout: withStorage(schema, "json") for tic-tac-toe-json
  tenant: "local",
  app: "tic-tac-toe",
  endpoint: "http://127.0.0.1:7070", // agentd
  token: ticketOrToken, // a string, or a function asked per request kind (a ticket minter)
  // Multi-node instead of endpoint/token: locator: { url: "http://controld:7080", apiKey: "mansio_…" },
});
const { gameId } = await Effect.runPromise(
  client.mutation("createGame", { xPlayer: "alice", oPlayer: "bob" }),
);
const game = await Effect.runPromise(client.query("getGame", { gameId }));
await Effect.runPromise(
  client.mutation("makeMove", { gameId, playerId: "alice", cell: 0, expectedVersion: game!._version }),
);
```

Names, argument and result types are inferred from the functions object. `createPromiseClient` takes the same
options and returns promises. With a `tenancy` section on agentd, the credential decides the tenant.

How a handler runs: reads asked for together (`db.getMany`, `Effect.all([...], { batching: true })`) are one
query request, and every later read request carries the preconditions of what the run has read so far, so all
the reads of one run hold in **one snapshot**. In a mutation, writes are buffered (inserts get client-side
UUIDv7 ids; later reads in the same handler see the buffered writes), and a `get` is answered from the
client's document cache when it holds the document. When the handler returns, one atomic mutation request
carries the writes plus **preconditions derived from the reads**: `get` → the version it saw, or
`exists: false`; `find` → its table unchanged since the read (`find(...).unguarded()` opts out). A cached read
is asserted the same way, so a warm mutation is one round trip. If another writer changed something the
handler read, the server answers `ConflictError` on a derived precondition, with the current state, and the
client **re-runs the handler** on that state (bounded, with jitter) without reading it again. So **handlers
must have no side effects beyond `db`**: they may run more than once. One commit holds at most 64 operations
(derived asserts + writes), else the call fails locally with `RuleError`. App-raised errors (`makeMove`'s own
`ConflictError` on a stale `expectedVersion`, a `RuleError`) fail the handler locally and reach the caller as
`RemoteError` with the status and tag the server would have used; transport trouble that outlasts the retry
policy and budget is `Unavailable`. Every call carries one `X-Trace-Id` on all its requests. Details:
`sdk/client.ts`, `sdk/recording.ts`, `sdk/transport.ts`.

## Query language and semantics

Supported: `get`, `find` with top-level scalar equality, chained `where`, numeric `gt`, `sort`, `limit`,
`collect`, `first`, `count`, `countBy`; and `insert`, `patch`, `delete` within mutations.

```ts
Effect.gen(function* () {
  const games = yield* db.find("games", { status: "playing" })
    .sort("_creationTime", "desc").limit(20).collect();
  const wins = yield* db.find("games", { status: "won" }).countBy("winner");
  return { games, wins };
});
```

The first operation becomes an IR `select`:

```json
{
  "op": "select",
  "table": "games",
  "filters": [{ "field": "status", "op": "eq", "value": "playing" }],
  "order": { "field": "_creationTime", "direction": "desc" },
  "limit": 20
}
```

Only the server converts it to SQL. Identifiers are schema-validated, values use bound parameters, and array
predicates and unknown fields fail explicitly. This is not a compiler for arbitrary JavaScript closures,
MongoDB's query language or Convex's full API, and there is no fetch-everything-and-filter fallback.

All fields are required unless explicitly nullable; there is no separate missing-field state. Arrays are
validated recursively and stored as JSON text. Optional fields, nested-object schemas, joins, array
predicates, projection, cursors and search are not implemented. Collection reads default to 100 results and
are capped at 1000. `count` counts all matching documents regardless of limit; `countBy` groups all matches
and caps returned groups. Sorting adds `_id` as a stable tie-breaker and spells out null ordering. The
normative rules are in [`ir/spec/v1.md`](ir/spec/v1.md) §5.

`_id`, `_creationTime` and `_version` are server-owned metadata; each patch increments `_version`. Typed
clients cannot patch metadata. Arguments and writes also receive runtime validation.

### Number and text contracts

- **Numbers.** A `number` field is a JS double: a REAL column in the columns layout. In the json layout the
  document is `JSON.stringify` text, which writes an integral double at or beyond 2^53 as integer text, and
  `json_extract` would read that back as an exact INTEGER. The compiler therefore wraps a json-layout number
  read in `CAST(... AS REAL)`, so filters, ordering, `countBy` keys and index expressions agree across
  layouts. Oracle regressions 1a and 1b in `tests/property/sqlite_oracle_test.ts` pin it for the TS compiler,
  and `server/internal/doccompile`'s `TestSelectMatchesReference` (whose number domain includes 2^53 and
  `1693240866440448768`) for the Go one.
- **Embedded NUL.** A string or enum value (at any array depth), an id or a filter value containing U+0000 is
  refused before anything runs, with
  `SchemaError { reason: "type-mismatch", expected: "text without U+0000" }` (ADR 0005, spec §5).

## Two physical layouts, same logical schema

`tic-tac-toe` uses columns. Strings/enums become TEXT, integers/booleans INTEGER, numbers REAL, and arrays
JSON-encoded TEXT. Generated tables use SQLite STRICT mode.

`tic-tac-toe-json` uses `_id`, `_creationTime`, `_version`, and one `doc TEXT` JSON document per row. The same
schema validates the JSON. Index declarations compile to SQLite JSON expression indexes. In columns mode,
`status = "playing"` targets the `status` column; in JSON mode the compiler uses
`json_extract(doc,
'$.status')`. The decoder returns the same document shape in either layout.
`core/compile/GENERATED_SQL.txt` shows both.

Each database stores its schema. A request carries the schema's fingerprint; a mismatch is `SchemaDriftError`
(409) and nothing runs. There is no automatic migration.

## Transaction and consistency contract

A request runs in one transaction on its database's actor (inside the actor's group-commit batch, as a
savepoint): a mutation's operations apply atomically and in order, or none do; a query sees one consistent
snapshot and never writes ([`ir/spec/v1.md`](ir/spec/v1.md) §6). Mutations carry a `requestId`; a retry of an
applied mutation with the same id answers the original result with `"duplicate": true` (§7). Concurrency
control is optimistic: the SDK's derived preconditions make a handler that read stale state fail with
`ConflictError` and re-run. A statement that runs past agentd's `statement_timeout` is interrupted and the
request fails with `StatementDeadlineError` (504); nothing changed.

A successful mutation acknowledges the local SQLite commit, not a durable copy. In durable mode the S3 copy
follows on the write-back schedule (`maintenance.write_back_every`, default 10 s) and on clean shutdown, so a
host loss can lose acknowledged writes inside that window. The window is about 11 s with the defaults, not 10:
the setting, plus up to one 1 s maintenance tick, plus the time the copy and upload take. It has no upper
bound while S3 is unreachable or its breaker is open: write-backs fail fast, the database stays warm and
writes keep being acknowledged, so the unpublished window is as long as the outage. agentd exports it as
`mansio_vactor_oldest_dirty_age_seconds` (with `mansio_vactor_dirty`, the number of databases with unpublished
writes), and `server/deploy/prometheus/backpressure.rules.yml` alerts when it passes 60 s. This is not a
replicated database.

## Errors

Failures are `{ "error": { "tag", "message", "op"?, "fields"? } }` with the status from the table in
[`ir/spec/v1.md`](ir/spec/v1.md) §8: 400 `SchemaError`/`CompileError`, 401 `AuthError`/`ticket_invalid`, 404
`NotFoundError`, 409 `ConflictError`/`SchemaDriftError`/`aborted`, 413 `RequestTooLarge`, 421
`wrong_owner`/`fenced`, 429 `QuotaExceeded`, 503 `shed`/`unavailable`, 504 `StatementDeadlineError`/`expired`,
500 for server faults. The SDK retries or reroutes the transient ones itself. Handler-side failures
(`RuleError`, an app's `ConflictError`) never reach the server; the client reports them with the status the
server would have used (`core/errors.ts:toHttpStatus`).

## Observability: the request log and metrics

With an `event_log: { dir, trace_sql }` section, agentd writes its request log: JSON lines (http, handler, tx,
sql, plus queue, backup and lifecycle) under `<dir>/server/<date>/`, every request's records under its
`X-Trace-Id`, through a bounded queue that never blocks a request, into files it rolls by hour and size and
deletes past a size and an age limit. A log collector ships them as they are; `jq` or the `duckdb` CLI read
them in place. Health and overload are Prometheus metrics on `/metrics`, with `/healthz`, `/livez` and
`/readyz`. [docs/observability.md](docs/observability.md) is the reference: the record fields, the limits and
counters, shipping, and worked queries.

## Tasks

| `deno task …`                                           | Purpose                                                                         |
| ------------------------------------------------------- | ------------------------------------------------------------------------------- |
| `fmt:check`, `lint`, `check`, `test`, `test:prop`, `ci` | Format, lint, types, unit, property, and the aggregate gate.                    |
| `go:ci`, `ci:all`                                       | The Go checks; both gates.                                                      |
| `sim:swarm`                                             | The Go server's deterministic-simulation seed swarm (`server/scripts/`).        |
| `explain`                                               | Compiler output for both layouts; redirect to `core/compile/GENERATED_SQL.txt`. |
| `perf:smoke`, `perf:load`, `perf:query`, `perf:mixed`   | k6 scenarios against agentd.                                                    |

`deno task e2e` runs the e2e suite directly; it needs Linux, Postgres and S3, which `scripts/dev/e2e-linux.sh`
provides.

## Deliberate limits

Reactive subscriptions, optimistic UI, durable actions/jobs, automatic schema migrations, range preconditions
for `find` (IR v1.1), and a query optimizer are not included. Handlers are statically imported modules in the
caller's process; the server never runs application code. `playerId` in the sample is supplied by the caller,
not a verified identity. Durability in durable mode is asynchronous write-back with a time window, not
synchronous replication. Benchmark real workloads rather than assuming every declared index accelerates every
query.

## Architecture diagrams site

[`docs/pages/`](docs/pages/README.md) is an interactive C4 viewer of this repo's architecture, rendered from
converted copies of the Markdown diagrams in [`docs/architecture/`](docs/architecture/README.md) (the source
of truth; the site's copies may lag them) plus design proposals. It is a separate npm project (Node 22.23.2);
run it with `cd pages && npm ci && npm run dev`.

## Primary references

- SQLite JSON functions: https://www.sqlite.org/json1.html
