# mansio user guide

Write TypeScript schemas and named document handlers; the SDK runs the handlers in your process and sends
their reads and writes to the Go server, `agentd`, as IR requests (`POST /v1/ops`,
[ir/spec/v1.md](ir/spec/v1.md)). This guide runs one local agentd for the two tic-tac-toe sample apps, drives
it with the SDK, and reads its event log. Read the README's [Deliberate limits](README.md#deliberate-limits)
before using it for anything real.

```text
Your program (SDK + handlers)
       | IR query and mutation requests, bearer credential
       v
agentd: POST /v1/ops, one actor per SQLite database
       |                              |
       | <tenant>:<app>.db files      | event_log: JSONL under <dir>/server/<date>/
       v                              v
db_dir (static mode)            deno task logs:ingest -> DuckDB event log
or S3 generations (durable)
```

Run the shell blocks in order, in one Bash session, from the root of the repository.

## Installation

You need Deno 2.9.5 (the version CI uses) and Go 1.26 or newer.

```sh
export PATH="$HOME/.deno/bin:$PATH"
deno --version
go version
```

Keep the committed `deno.lock`. The first run needs network access for the npm packages (Effect, DuckDB) and
the Go modules.

## Configure and start agentd

agentd reads one JSON config file (`-config`); unknown fields are errors and relative paths are relative to
the config file. Every field is documented on the `Config` type in `server/cmd/agentd/config.go`; sample
configs are in `server/configs/agentd/`. The setup below is static mode (the databases are files in `db_dir`)
with `tenancy` mode `off`, where each app has its own bearer token. The e2e harness
(`tests/e2e/platform/harness.ts`, `startGoAgent` and `startGoTenancy`) writes configs of the same shape.

Build the binary and write the files it needs: each app's schema in its storage layout under
`schemas/<app>/v1.json`, and a ticket key file (`keys_file` is always required, even when no tickets are
used).

```sh
export MANSIO_DIR="$(mktemp -d)"
(cd server && go build -o "$MANSIO_DIR/agentd" ./cmd/agentd)
mkdir -p "$MANSIO_DIR/dbs" "$MANSIO_DIR/logs"
deno eval '
import { schema } from "./app/schema.ts";
import { withStorage } from "./sdk/schema.ts";
import { generateIssuer } from "./sdk/ticket.ts";
const dir = Deno.env.get("MANSIO_DIR");
for (const [app, layout] of [["tic-tac-toe", "columns"], ["tic-tac-toe-json", "json"]] as const) {
  await Deno.mkdir(`${dir}/schemas/${app}`, { recursive: true });
  await Deno.writeTextFile(`${dir}/schemas/${app}/v1.json`, JSON.stringify(withStorage(schema, layout)));
}
await Deno.writeTextFile(`${dir}/keys.json`, (await generateIssuer()).keysFile);
'
```

The tenancy secrets file holds the admin token and the app tokens (each at least 16 characters). agentd
refuses it unless only its owner can read it. These tokens are for local use only.

```sh
export ADMIN_TOKEN=dev-admin-token-0001
export COLUMN_APP_TOKEN=dev-columns-token-0001
export JSON_APP_TOKEN=dev-json-token-00001
cat >"$MANSIO_DIR/tenancy.json" <<JSON
{"admin_token": "$ADMIN_TOKEN",
 "app_tokens": {"tic-tac-toe": "$COLUMN_APP_TOKEN", "tic-tac-toe-json": "$JSON_APP_TOKEN"}}
JSON
chmod 600 "$MANSIO_DIR/tenancy.json"
cat >"$MANSIO_DIR/agentd.json" <<'JSON'
{
  "listen": "127.0.0.1:7070",
  "vm": "vm-local",
  "db_dir": "dbs",
  "create_dbs": true,
  "keys_file": "keys.json",
  "schemas_dir": "schemas",
  "tenancy": { "mode": "off", "secrets_file": "tenancy.json" },
  "log_file": "logs/agentd.log",
  "event_log": { "dir": "logs", "trace_sql": true }
}
JSON
```

- `create_dbs` creates a database (`<tenant>:<app>.db` in `db_dir`) on its first request; without it static
  mode serves only the files it finds at start. It needs `schemas_dir`.
- In `off` mode the tenant is `default`, so the databases are `default:tic-tac-toe` and
  `default:tic-tac-toe-json`.
- `log_file` is agentd's own JSON log (default stderr). `event_log` is the event log for `deno task logs:*`;
  `trace_sql` adds one record per SQL statement.

Check the config, then start agentd and wait until it is ready:

```sh
"$MANSIO_DIR/agentd" -config "$MANSIO_DIR/agentd.json" -check
"$MANSIO_DIR/agentd" -config "$MANSIO_DIR/agentd.json" &
agentd_pid=$!
until curl -fs http://127.0.0.1:7070/readyz; do sleep 0.2; done; echo
```

agentd's flags are `-config`, `-addr-file` (write the listener addresses once serving), `-check` and `-dev`
(local development: allows plain-HTTP S3).

## Core concepts

A document has schema-defined fields plus server-owned `_id`, `_creationTime` and `_version`. A patch
increments `_version`; application writes cannot set these fields. Fields are required unless explicitly
nullable. Documents belong to **collections**, which the code calls tables (`defineTable`).

The `columns` layout stores scalar fields in typed SQLite STRICT columns and arrays as JSON text. The `json`
layout stores the metadata columns and one `doc TEXT` JSON body per row, with JSON expression indexes. Both
use the same schema and return the same document shape. The schema value you give the client must be in the
database's layout (`withStorage(schema, "json")` for `tic-tac-toe-json`). A request whose schema fingerprint
differs from the database's fails with `SchemaDriftError` (409). There is no automatic migration.

A **query** handler's reads each go to the server as a query request and see one consistent snapshot. A
**mutation** handler's reads go out at once, its writes are buffered, and on return one atomic mutation
request carries the writes plus a precondition for every document the handler read with `get`. If another
writer changed one of those documents, the server answers `ConflictError` and the client re-runs the handler
on fresh reads. **Handlers may run more than once: keep them free of side effects beyond `db`.** See the
README's [transaction contract](README.md#transaction-and-consistency-contract).

## SDK walkthrough

The schema is TypeScript data: [app/schema.ts](app/schema.ts) declares the two collections, `games` and
`moves`, with their fields and indexes.

Handlers are Effects. `yield*` composes database operations and typed failures; `validatePlayers` returns an
Either with `RuleError`, which can also be yielded. This is the shape of [app/functions.ts](app/functions.ts):

```ts
import { Effect } from "effect";
import { defineFunctions } from "./sdk/functions.ts";
import { v } from "./sdk/schema.ts";
import { schema } from "./app/schema.ts";
import { validatePlayers } from "./app/rules.ts";

const { query, mutation } = defineFunctions(schema);
export const functions = {
  listGames: query({
    args: { status: v.enum("playing", "won", "draw") },
    handler: ({ db }, { status }) =>
      Effect.gen(function* () {
        return yield* db.find("games", { status })
          .sort("_creationTime", "desc").limit(20).collect();
      }),
  }),
  createGame: mutation({
    args: { xPlayer: v.string(), oPlayer: v.string() },
    handler: ({ db }, { xPlayer, oPlayer }) =>
      Effect.gen(function* () {
        yield* validatePlayers(xPlayer, oPlayer);
        const gameId = yield* db.insert("games", {
          xPlayer,
          oPlayer,
          board: Array(9).fill(null),
          turn: "X",
          status: "playing",
          winner: null,
        });
        return { gameId };
      }),
  }),
};
```

The client takes the functions object and the schema as values. Save this module at the repository root (for
example as `guide.ts`) and run it with `deno run --allow-net --allow-env guide.ts` while agentd runs. It
creates a game, reads it, makes a move, and then deliberately breaks a rule. The optional third argument of
`query`/`mutation` is the call's trace id.

```ts
import { Effect } from "effect";
import { createClient } from "./sdk/client.ts";
import { functions } from "./app/functions.ts";
import { schema } from "./app/schema.ts";

const client = createClient({
  functions,
  schema,
  tenant: "default",
  app: "tic-tac-toe",
  endpoint: "http://127.0.0.1:7070",
  token: Deno.env.get("COLUMN_APP_TOKEN")!,
});
const program = Effect.gen(function* () {
  const { gameId } = yield* client.mutation(
    "createGame",
    { xPlayer: "alice", oPlayer: "bob" },
    "guide-create",
  );
  const game = yield* client.query("getGame", { gameId }, "guide-read");
  console.log(game);
  yield* client.mutation("makeMove", { gameId, playerId: "alice", cell: 4, expectedVersion: game!._version });
  yield* client.mutation("createGame", { xPlayer: "alice", oPlayer: "alice" }).pipe(
    Effect.catchTag("RemoteError", (error) =>
      Effect.sync(() => {
        if (error.remoteTag !== "RuleError") throw error;
        console.log(`Expected rejection: ${error.status} ${error.remoteTag}`);
      })),
  );
});
await Effect.runPromise(program);
```

Calls return Effects; run them at your program's boundary (`createPromiseClient` returns promises instead).
Failures are typed. `RemoteError` carries the error's `status` and `remoteTag`; an app-raised `RuleError` or
`ConflictError` fails locally, with the status the server would have used. `Unavailable` means the server
could not be reached, or kept answering "retry later", past the retry policy and budget. The statuses and tags
are the table in [ir/spec/v1.md](ir/spec/v1.md) §8.

For `tic-tac-toe-json`, use `withStorage(schema, "json")` from `sdk/schema.ts` as the schema and
`JSON_APP_TOKEN` as the token.

## Authentication and tenancy

agentd accepts two kinds of bearer credential on `/v1/ops`:

- **Tickets**: short-lived Ed25519-signed tokens naming one database, its agent (`vm`) and read or write
  permission. agentd verifies them against `keys_file` (or controld's `keys_url`). In production controld's
  locator issues them; for development `sdk/ticket.ts` mints them (`generateIssuer`, `mintTicket`), as
  `startGoAgent` in `tests/e2e/platform/harness.ts` and `startScratchAgent` in `tests/e2e/scratch.ts` do. A
  read ticket does not allow writes and a write ticket does not allow reads.
- **Tenancy credentials** (a `tenancy` section): `off` (one token per app, from the secrets file), `api-key`
  (tenant API keys) or `session` (API keys plus email/password sessions at `/api/auth/sign-up/email` and
  `/api/auth/sign-in/email`). `api-key` and `session` keep tenants, keys and users in Postgres (`pg_dsn` in
  the secrets file); the credential decides the tenant. In those modes agentd also serves `/admin/tenants*`,
  which `scripts/tenants.ts` drives:

```sh
BASE_URL=http://127.0.0.1:7070 ADMIN_TOKEN=… deno run -A scripts/tenants.ts list
```

Its commands are `create`, `list`, `show`, `deactivate`, `issue-key` and `revoke-key`; run it without
arguments for usage. In `off` mode there are no tenant routes. The design is
[ADR 0012](docs/adr/0012-single-node-auth-port.md); `TenancyConfig` in `server/cmd/agentd/tenancy.go`
documents the section. Multi-node deployments authenticate at controld instead; see `server/`.

## Backups and restore

Static mode has no backups: the databases are the files in `db_dir`. For durability run agentd in **durable
mode**, with an `s3` section (`endpoint`, `bucket`, `prefix`), a `secrets_file` (`s3_access_key`,
`s3_secret_key`, `control_token`; mode 0600), `cache_dir`, `owner_dir` and a placement authority
(`placement_dir` on a single node). `tests/e2e/platform/durable.ts` (`startDurableAgent`) writes a complete
single-node durable config; `server/configs/agentd/durable-local.json` is another. `docker/README.md` starts a
local S3 (SeaweedFS); plain-HTTP S3 needs agentd's `-dev` flag.

In durable mode each database is a chain of immutable generations in S3. agentd writes a dirty database back
every `maintenance.write_back_every` (default 10 s), when it passivates an idle database, and on clean
shutdown. `POST /admin/backup?key=<tenant>:<app>` on the admin listener (`admin_listen`, loopback only) writes
the database back now and answers the manifest of the generation that holds it.

**This is asynchronous write-back, not replication.** A mutation acknowledges the local SQLite commit; a host
loss can lose acknowledged writes newer than the last write-back.

`dbtool` works on the S3 copies. It takes `-endpoint`, `-bucket` and `-root` (the agent's prefix) and reads
the keys from `DBTOOL_S3_ACCESS_KEY` and `DBTOOL_S3_SECRET_KEY`. Without a command it prints its usage:

```sh
(cd server && go build -o "$MANSIO_DIR/dbtool" ./cmd/dbtool)
"$MANSIO_DIR/dbtool" || true
```

- `dbtool restore [-gen G] <key> <new file>` downloads the current generation (or generation `G`, e.g. a
  backup's), verifies its size, checksum and SQLite integrity, and creates the file. It never overwrites.
- `dbtool import <file> -tenant T -app A -schema F [-placement-dir D -vm V]` makes an existing SQLite file the
  first generation of `T:A`. The file's stored schema must equal `F`.
- `inspect`, `verify`, `orphans`, `gc` and `rebuild-manifest` are for operators; the runbook is
  `server/knowledge/writeback/2026-11-05-storage-runbook.md`.

## Observability

agentd writes the event log when its config has an `event_log` section: `dir` (required), `level` (`debug`,
`info` (default), `warn`, `error`), `trace_sql`, `buffer` and `max_file_bytes`. Files are
`<dir>/server/<date>/events-<HH>-<pid>-<seq>.jsonl`. Every `/v1/ops` request gets an http record and a handler
record (the handler name comes from the SDK's `X-Mansio-Handler` header), plus tx records and, with
`trace_sql`, sql records. The response echoes the request's `X-Trace-Id`; agentd generates one if it is absent
or invalid.

The `deno task logs:*` CLI reads that tree. Its settings are environment variables declared in
`eventlog/config.ts`; the important ones are `LOG_DIR` (the event log's `dir`), `EVENTLOG_DUCKDB` (the DuckDB
file, default `<DATA_DIR>/eventlog.duckdb`) and `DATA_DIR`. Stop agentd (it drains the log on shutdown), then
ingest and query:

```sh
kill -TERM "$agentd_pid"; wait "$agentd_pid"
export LOG_DIR="$MANSIO_DIR/logs" DATA_DIR="$MANSIO_DIR/data" EVENTLOG_DUCKDB="$MANSIO_DIR/eventlog.duckdb"
deno task logs:ingest
deno task logs:list
deno task logs:query handler-throughput --format json
deno task logs:query trace --param traceId=guide-read --format json
deno task logs:sweep --dry-run
```

`logs:ingest` is idempotent: it loads only JSONL it has not seen. The DuckDB file has a single writer: while
one `logs:ingest` holds it, other `logs:*` commands fail with a lock error rather than wait. `logs:sweep`
without `--dry-run` ingests, then gzips or deletes JSONL files that are closed and fully ingested; the DuckDB
file is never pruned. `logs:list` names the catalog queries and the question each answers.
[docs/observability.md](docs/observability.md) describes the tables, parameters and retention rules;
[perf/README.md](perf/README.md) the k6 scenarios and the `perf:ingest` loader.

agentd's own operational log is `log_file` (JSON lines; a failure names a log ID). `/healthz`, `/livez`,
`/readyz` and `/metrics` (Prometheus) are on the `health_listen` listener, or on the query listener when there
is none.

## Troubleshooting

Each entry is symptom → cause → fix.

- **agentd exits at start with a config error** → an unknown field, a missing required field or an invalid
  value; the message names it → fix the config and rerun with `-check`.
- **`secrets file … has mode …; make it 0600`** → the secrets file is readable by others → `chmod 600` it.
- **401 `AuthError`** → no bearer, or the wrong one: in `off` mode each app's token reaches only that app →
  use the app's token from the secrets file.
- **401 `ticket_invalid`** → a ticket that is expired, signed by a key not in `keys_file`, for another `vm` or
  database, or without the permission the request needs → mint a new one.
- **421 `wrong_owner` in static mode** → the database is not served: without `create_dbs`, agentd serves only
  the `.db` files it found at start → create the file before starting, or set `create_dbs` (with
  `schemas_dir`).
- **409 `SchemaDriftError`** → the client's schema (or its layout) differs from the database's → use the same
  schema value and `withStorage` layout the database was created with, or a fresh database.
- **409 `ConflictError` from `makeMove`** → a stale `expectedVersion` → read the game again before the next
  move.
- **503 `shed` or `unavailable`, 504 `expired`** → the agent is overloaded or a dependency is down; the SDK
  already retried within its budget and the call's deadline → reduce load, check agentd's log and metrics.
- **504 `StatementDeadlineError`** → a statement ran past `statement_timeout` → narrow the read (an index, a
  lower limit) or raise the timeout.
- **`deno task logs:*` fails with a lock error** → another `logs:ingest` holds the DuckDB file → wait for it
  to finish and rerun.
- **The event log is empty** → agentd has no `event_log` section, or `level` is `error` → add the section and
  restart.
- **Anything else** → agentd's `log_file` names a log ID for every failure. See `server/` for the server's
  design notes.
