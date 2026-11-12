# mansio user guide

Write TypeScript schemas and named document handlers; the SDK runs the handlers in your process and sends
their reads and writes to the Go server, `agentd`, as intermediate representation (IR) requests
(`POST /v1/ops`, [intermediate-representation/spec/v1.md](../intermediate-representation/spec/v1.md)). This
guide runs one local agentd for the two tic-tac-toe sample apps, drives it with the SDK, and reads its request
log. Read the README's [Deliberate limits](../README.md#deliberate-limits) before using it for anything real.

```text
Your program (SDK + handlers)
       | IR query and mutation requests, bearer credential
       v
agentd: POST /v1/ops, one actor per SQLite database
       |                              |
       | <tenant>:<app>.db files      | event_log: request log, JSONL under <dir>/server/<date>/
       v                              v
db_dir (static mode)            jq, duckdb, or your log collector
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

Keep the committed `deno.lock`. The first run needs network access for the npm packages (Effect) and the Go
modules. `deno task check` type-checks the SDK, the examples and the tests, which confirms the setup. The
Observability section uses `jq`.

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
import { schema } from "./examples/tic-tac-toe/schema.ts";
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
- `log_file` is agentd's own JSON log (default stderr). `event_log` turns on the request log under
  `logs/server/` (see Observability); `trace_sql` adds one record per SQL statement.

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

All the reads of one handler run hold in **one snapshot**. Reads asked for together (`db.getMany`, or
`Effect.all([...], { batching: true })`) are one query request; a read that depends on an earlier answer is
its own round trip, and carries the preconditions of what the run has read so far, so the server checks them
in the snapshot it reads from. A **mutation** handler's writes are buffered, and on return one atomic mutation
request carries them plus a precondition for everything the handler read: the version of every document it got
with `get`, and "this table has not changed since" for every `find` (so "no row matches, so insert one" is
safe; `find(...).unguarded()` opts a display-only read out). If another writer changed any of it, the server
answers `ConflictError` with the current state, and the client re-runs the handler on that state without
reading it again. A mutation's `get` is answered from the client's document cache when the cache holds the
document; its version is asserted like any other read, so a warm mutation is one round trip and a stale cache
costs one more, never a wrong commit (`cache: false` on the client turns the cache off). **Handlers may run
more than once: keep them free of side effects beyond `db`.** See the README's
[transaction contract](../README.md#transaction-and-consistency-contract).

## SDK walkthrough

The schema is TypeScript data: [examples/tic-tac-toe/schema.ts](../examples/tic-tac-toe/schema.ts) declares
the two collections, `games` and `moves`, with their fields and indexes.

Handlers are Effects. `yield*` composes database operations and typed failures; `validatePlayers` returns an
Either with `RuleError`, which can also be yielded. This is the shape of
[examples/tic-tac-toe/functions.ts](../examples/tic-tac-toe/functions.ts):

```ts
import { Effect } from "effect";
import { defineFunctions } from "./sdk/functions.ts";
import { v } from "./sdk/schema.ts";
import { schema } from "./examples/tic-tac-toe/schema.ts";
import { validatePlayers } from "./examples/tic-tac-toe/rules.ts";

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
import { functions } from "./examples/tic-tac-toe/functions.ts";
import { schema } from "./examples/tic-tac-toe/schema.ts";

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
could not be reached, or kept answering "retry later", past the retry policy and budget, or that a request
`expired` (not retried: its deadline is spent). The statuses and tags are the table in
[intermediate-representation/spec/v1.md](../intermediate-representation/spec/v1.md) §8; what the client does
by itself after each answer (retry, re-resolve or fail, and what it charges to its retry budget) is the one
table in [docs/retry-contract.md](retry-contract.md), which the Go client follows too.

For `tic-tac-toe-json`, use `withStorage(schema, "json")` from `sdk/schema.ts` as the schema and
`JSON_APP_TOKEN` as the token.

## Authentication and tenancy

agentd accepts two kinds of bearer credential on `/v1/ops`:

- **Tickets**: short-lived Ed25519-signed tokens naming one database, its agent (`vm`) and read or write
  permission. agentd verifies them against `keys_file` (or controld's `keys_url`). In production controld's
  locator issues them; for development `sdk/ticket.ts` mints them (`generateIssuer`, `mintTicket`), as
  `startGoAgent` in `tests/e2e/platform/harness.ts` does. A read ticket does not allow writes and a write
  ticket does not allow reads.
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
[ADR 0012](adr/0012-single-node-auth-port.md); `TenancyConfig` in `server/cmd/agentd/tenancy.go` documents the
section. Multi-node deployments authenticate at controld instead; see `server/`.

## Backups and restore

Static mode has no backups: the databases are the files in `db_dir`. For durability run agentd in **durable
mode**, with an `s3` section (`endpoint`, `bucket`, `prefix`), a `secrets_file` (`s3_access_key`,
`s3_secret_key`, `control_token`; mode 0600), `cache_dir`, `owner_dir` and a placement authority
(`placement_dir` on a single node). `tests/e2e/platform/durable.ts` (`startDurableAgent`) writes a complete
single-node durable config; `server/configs/agentd/durable-local.json` is another.
`docs/local-infrastructure.md` starts a local S3 (SeaweedFS); plain-HTTP S3 needs agentd's `-dev` flag.

In durable mode each database is a chain of immutable generations in S3. agentd writes a dirty database back
every `maintenance.write_back_every` (default 10 s), when it passivates an idle database, and on clean
shutdown. `POST /admin/backup?key=<tenant>:<app>` on the admin listener (`admin_listen`, loopback only) writes
the database back now and answers the manifest of the generation that holds it.

**This is asynchronous write-back, not replication.** A mutation acknowledges the local SQLite commit; a host
loss can lose acknowledged writes newer than the last write-back: about 11 s of them with the defaults (the 10
s setting, up to one 1 s tick, and the copy itself), and as much as the whole outage while S3 is unreachable,
because writes keep being acknowledged while write-back fails. Watch `mansio_vactor_oldest_dirty_age_seconds`
and `mansio_vactor_dirty` on `/metrics`; the shipped alert rules
(`server/deploy/prometheus/backpressure.rules.yml`) fire when the oldest unpublished write is older than 60 s.

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
  `server/docs/notes/writeback/storage-runbook.md`.

## Observability

With an `event_log` section, agentd writes its **request log**: one JSON line per `/v1/ops` request (`http`),
per handled request (`handler`), per transaction (`tx`) and, with `trace_sql`, per SQL statement (`sql`), all
under the request's trace id. The SDK sends one `X-Trace-Id` per call (the third argument of `query` and
`mutation`, else a random UUID) and its handler's name in `X-Mansio-Handler`; agentd sends the trace id back
and generates one if it is absent or invalid. The files are
`<dir>/server/<date>/events-<HH>-<pid>-<seq>.jsonl` (local date and hour), written within milliseconds of each
answer.

Find the walkthrough's traced read, with its SQL, while agentd still runs (you need `jq`):

```sh
ls "$MANSIO_DIR"/logs/server/*/
jq -c 'select(.traceId == "guide-read")
  | {kind, name, status, outcome, sqlText, rowCount, durationMs} | with_entries(select(.value != null))' \
  "$MANSIO_DIR"/logs/server/*/*.jsonl
```

The answer is the request (`http`), the two statements of its snapshot read (`sql`: your `games` lookup and
the `_mansio_changes` counters behind read preconditions), the transaction (`tx`) and the handler run
(`handler`):

```text
{"kind":"http","name":"getGame","status":200,"durationMs":0.115}
{"kind":"sql","outcome":"ok","sqlText":"SELECT * FROM \"games\" WHERE \"_id\" = ? ORDER BY \"_id\" ASC LIMIT ?","rowCount":1,"durationMs":0.024}
{"kind":"sql","outcome":"ok","sqlText":"SELECT table_name, seq FROM \"_mansio_changes\"","rowCount":2,"durationMs":0.008}
{"kind":"tx","outcome":"commit","durationMs":0.05}
{"kind":"handler","name":"getGame","outcome":"ok","durationMs":0.111}
```

Use `guide-create` for the first mutation. Failed requests, by handler and error tag:

```sh
jq -r 'select(.kind == "http" and .status >= 400) | [.name, .status, .errorTag] | @tsv' \
  "$MANSIO_DIR"/logs/server/*/*.jsonl | sort | uniq -c
```

The walkthrough's `RuleError` is not there: the SDK rejected it locally, so it never reached agentd. Bind
values are never logged, and bearer tokens are redacted.

[docs/observability.md](observability.md) is the reference: every record's fields, the writer's queue and size
and age limits (`buffer`, `max_file_bytes`, `max_total_bytes`, `max_age`), how to ship the files to a log
store, and queries with the optional `duckdb` CLI (slowest statements, errors and latency by handler).

`/healthz`, `/livez`, `/readyz` and `/metrics` (Prometheus) are on the `health_listen` listener, or on the
query listener when there is none; `curl -s http://127.0.0.1:7070/metrics | grep mansio_eventlog` shows the
request log's counters (records written, dropped, lost). agentd's own operational log is `log_file` (JSON
lines; a failure names a log ID). Stop agentd when you are done:

```sh
kill -TERM "$agentd_pid"; wait "$agentd_pid"
```

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
- **503 `shed` or `unavailable`** → the agent is overloaded or a dependency is down; the SDK already retried
  within its budget and the call's deadline → reduce load, check agentd's log and metrics.
- **504 `expired`** → the request's deadline passed before an answer; the SDK does not retry it
  ([retry contract](retry-contract.md)) → retry from the application with a fresh deadline if the work still
  matters (a mutation keeps its `requestId`, so it applies at most once), and treat a steady stream of them as
  overload.
- **504 `StatementDeadlineError`** → a statement ran past `statement_timeout` → narrow the read (an index, a
  lower limit) or raise the timeout.
- **No files under `<dir>/server/`** → agentd has no `event_log` section → add it and restart.
- **A request is missing from the request log** → `level` is above `info` (only failures are kept), or the
  trace id you searched for is not the one sent (agentd replaces one that is not 1 to 128 characters of
  `A-Za-z0-9._:-`; the answer's `X-Trace-Id` header has the one it used), or the queue was full →
  `mansio_eventlog_records_total{result="dropped"}` on `/metrics` and `queue` records in the log count the
  dropped records; raise `event_log.buffer` or reduce load.
- **No `sql` records** → `trace_sql` is off (only failed statements are logged) → set it and restart.
- **Anything else** → agentd's `log_file` names a log ID for every failure. See `server/` for the server's
  design notes.
