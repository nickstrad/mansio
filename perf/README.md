# k6 perf scripts

## Install

k6 is installed at `/usr/local/bin/k6` from the official Grafana Linux amd64 tarball:

```sh
curl -fsSL -o /tmp/k6.tar.gz \
  https://github.com/grafana/k6/releases/download/v0.56.0/k6-v0.56.0-linux-amd64.tar.gz
tar -xzf /tmp/k6.tar.gz -C /tmp
install -m 0755 /tmp/k6-v0.56.0-linux-amd64/k6 /usr/local/bin/k6
k6 version   # k6 v0.56.0 (commit/50afb99947, go1.23.4, linux/amd64)
```

Pinned version: **v0.56.0**, source URL above. This recipe is also recorded in the knowledge store
(`kb search "k6"`) since the droplet had no k6 install before this item.

## How the scripts reach agentd

k6 cannot run the TypeScript SDK, so `perf/lib/intermediate-representation.js` sends each tic-tac-toe handler
to agentd's `POST /v1/ops` the way `sdk/client.ts` does: a read is one query request; `makeMove` reads the
game, applies the rules locally and commits `[assert, patch, insert]`. A rule failure or a stale
`expectedVersion` fails locally, as in the SDK, without a request. Every request carries
`X-Trace-Id: <vu>-<iter>-<run-id>`, `X-Mansio-Handler` and a ticket. `perf/lib/api.js` is the only module the
scenarios call.

A run needs a running agentd that serves `local:tic-tac-toe` (static mode) and these environment variables:

| Variable                      | Meaning                                                                   |
| ----------------------------- | ------------------------------------------------------------------------- |
| `READ_TICKET`, `WRITE_TICKET` | Tickets for `TENANT:APP_ID`, read and write permission (`sdk/ticket.ts`). |
| `SCHEMA_FINGERPRINT`          | The app schema's fingerprint in its layout (`sdk/fingerprint.ts`).        |
| `BASE_URL`                    | agentd's query listener; default `http://127.0.0.1:7070`.                 |
| `APP_ID`, `TENANT`            | Defaults `tic-tac-toe` and `local`.                                       |
| `K6_RUN_ID`                   | The run id: artifact directory and trace-id suffix; default `k6-smoke`.   |

`startScratchAgent` in `tests/e2e/scratch.ts` does the whole setup for a test: an agentd in static mode with
an event log, a fresh ticket issuer, both tickets and the fingerprint. `tests/e2e/observability_test.ts` uses
it to run `perf/smoke.js` and join k6's latencies to agentd's event log
(`deno run -A tests/e2e/observability_test.ts`; needs `k6` and Go). For a manual run, the same steps by hand
(from the repository root; the tickets are valid for 50 minutes):

```sh
export MANSIO_DIR="$(mktemp -d)"
(cd server && go build -o "$MANSIO_DIR/agentd" ./cmd/agentd)
mkdir -p "$MANSIO_DIR/dbs" && touch "$MANSIO_DIR/dbs/local:tic-tac-toe.db"
deno eval '
import { schema } from "./examples/tic-tac-toe/schema.ts";
import { withStorage } from "./sdk/schema.ts";
import { schemaFingerprint } from "./sdk/fingerprint.ts";
import { generateIssuer, mintTicket, PERM_READ, PERM_WRITE } from "./sdk/ticket.ts";
const dir = Deno.env.get("MANSIO_DIR");
const stored = withStorage(schema, "columns");
await Deno.mkdir(`${dir}/schemas/tic-tac-toe`, { recursive: true });
await Deno.writeTextFile(`${dir}/schemas/tic-tac-toe/v1.json`, JSON.stringify(stored));
const issuer = await generateIssuer();
await Deno.writeTextFile(`${dir}/keys.json`, issuer.keysFile);
const iat = Math.floor(Date.now() / 1000) - 1;
const mint = (perms: number) =>
  mintTicket(issuer.key, {
    kid: issuer.kid, tid: "local", dbid: "local:tic-tac-toe", vm: "vm-local",
    epoch: 1, sub: "k6", perms, iat, exp: iat + 3000,
  });
console.log(`export READ_TICKET=${await mint(PERM_READ)}`);
console.log(`export WRITE_TICKET=${await mint(PERM_WRITE)}`);
console.log(`export SCHEMA_FINGERPRINT=${await schemaFingerprint(stored)}`);
' >"$MANSIO_DIR/tickets.env"
cat >"$MANSIO_DIR/agentd.json" <<'JSON'
{ "listen": "127.0.0.1:7070", "vm": "vm-local", "db_dir": "dbs", "keys_file": "keys.json",
  "schemas_dir": "schemas", "max_ticket_ttl": "1h", "log_file": "agentd.log" }
JSON
"$MANSIO_DIR/agentd" -config "$MANSIO_DIR/agentd.json" &
. "$MANSIO_DIR/tickets.env"
```

Add `"event_log": {"dir": "logs"}` to the config to get the server-side records for `perf:ingest` and the
`k6-vs-platform` analysis query ([docs/observability.md](../docs/observability.md)).

## `smoke.js`

1 VU for 30s, replaying the sample app's call order against `tic-tac-toe`: `createGame` → `getGame` → 5x
`makeMove` → `winsByMark`. Each iteration creates a fresh game, so iterations never collide. Thresholds:
`http_req_failed` rate < 1%, `http_req_duration` p(95) < 250ms, `checks` rate == 100%.

A down or unreachable server makes every request fail (`http_req_failed` always counts connection-level
failures, and `perf/lib/api.js`'s `http.setResponseCallback` also counts non-200 responses), which fails the
thresholds and makes k6 exit non-zero — the script never reports a vacuous pass when it can't reach agentd.

## Run it

With agentd running and the variables above set, from the repository root:

```sh
deno task perf:smoke
```

The task creates `logs/k6/<run-id>/` itself (k6 does not create missing directories for its own output files,
and `handleSummary` in `perf/smoke.js` writes `summary.json` into that directory), then runs k6 with
`--out json=logs/k6/<run-id>/raw.json` — so both artifacts land under `logs/k6/<run-id>/` on every run. Set
`K6_RUN_ID` to choose the run id (e.g. `K6_RUN_ID=2026-10-10-smoke deno task perf:smoke`); it defaults to
`k6-smoke`. `perf/lib/api.js` reads the same variable, so the run id in the console summary and the directory
always agree.

To run k6 directly without the task wrapper, create the directory first (same pre-created directory for both
outputs):

```sh
export K6_RUN_ID=my-run; mkdir -p "logs/k6/$K6_RUN_ID"
k6 run --out json="logs/k6/$K6_RUN_ID/raw.json" perf/smoke.js
```

Both files land under `logs/k6/<run-id>/`, which is gitignored — this is local/CI run output, not something to
commit. `deno task perf:ingest` loads `raw.json` into the `k6_points` table (see `docs/adr/0003-k6-perf.md`).

## Load scenarios

Three scenarios sit beside `smoke.js`. They share `perf/lib/scenarios.js` (seeding, the weighted read mix,
conflict-aware writes, artifact paths, `handleSummary`) and call the API only through `perf/lib/api.js`.

| task                   | script             | shape                                                                                                                                         | thresholds                                                                                            |
| ---------------------- | ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `deno task perf:load`  | `mutation_load.js` | `ramping-vus` 1 → 20 over 1 min, hold 1 min; each iteration plays its own game                                                                | `checks` rate > 0.99; `http_req_duration{expected_response:true}` p(95) < 400; `http_req_failed` < 1% |
| `deno task perf:query` | `query_load.js`    | `constant-arrival-rate` 50 rps for 2 min over 30 seeded games; `getGame`/`listGames`/`moveHistory`/`winsByMark` 4:2:2:1                       | `http_req_failed` < 1%; `http_req_duration` p(95) < 500; `checks` rate > 0.99; no dropped iterations  |
| `deno task perf:mixed` | `mixed.js`         | `constant-arrival-rate` 50 rps for 3 min, 80% reads (same mix) / 20% writes (one move of the VU's own game, or `createGame` when it finished) | same as `perf:query`; **the default scenario for regression comparison**                              |

**Conflicts are a success.** `mutation_load.js` deliberately attempts a stale-`expectedVersion` move before
every third move of a game. It must be rejected with `ConflictError` (409); as in the SDK, the rejection
happens locally after the read of the game, so no commit is sent. The run records it in the custom `conflicts`
counter and a check. A stale write that is _accepted_ fails a check.

**Seeding** happens in k6's `setup()`: `query_load.js` and `mixed.js` create 30 games (X wins, O wins, draws,
in-progress) through the API before the timed phase, so a fresh database needs no preparation. Seed requests
appear in `raw.json` under the `::setup` group with trace id `0-0-<run-id>`.

### Running a scenario with artifacts

The `perf:*` tasks are plain `k6 run <script>`; k6 only writes `raw.json` when given `--out`, and it creates
no directories. So set the run id, create the directory, and pass `K6_OUT`:

```sh
export K6_RUN_ID=2026-10-10-mixed
mkdir -p "logs/k6/$K6_RUN_ID"
K6_OUT="json=logs/k6/$K6_RUN_ID/raw.json" deno task perf:mixed
```

Both `logs/k6/<run-id>/raw.json` (newline-delimited JSON; `read_json_auto` reads the mixed file directly, then
filter `WHERE type='Point'`) and `summary.json` (written by the scenario's `handleSummary`, with `run_id` and
`scenario` added) land there. `K6_RUN_ID` also feeds the `X-Trace-Id` suffix, so the trace ids in agentd's
event log match the directory name. If you forget it the run id falls back to `k6-smoke`, which is smoke's
directory — always set it. Use a fresh database per run (a new `MANSIO_DIR` above) so runs do not see each
other's games.

Thresholds are enforced by k6: a crossed threshold makes the task exit 99. The environment variables work as
for `smoke.js`.

### What these numbers mean

The thresholds and the numbers they produce are for **regression detection** — compare a run to an earlier run
of the same scenario on the same machine — not capacity claims. The reference numbers recorded before IR-6
(2026-10-10: `perf:load` p(95) 162 ms, `perf:query` 31 ms, `perf:mixed` 57 ms) were measured against the
removed Deno platform and are not comparable with agentd runs.

## Not a benchmark

One agentd on one host, one database per app, k6 on the same host: treat these scripts as functional and
regression checks against the live `/v1/ops` surface, not as capacity claims. The Go server's own load tools
(`sqlbench`, `frontbench`, `wbbench`) are in `server/cmd/`; see [server/README.md](../server/README.md).
