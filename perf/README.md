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

## `smoke.js`

1 VU for 30s, replaying the same call order as `scripts/demo.ts` against the `tic-tac-toe` app: `createGame` →
`getGame` → 5x `makeMove` → `winsByMark`. Each iteration creates a fresh game, so iterations never collide.
Thresholds: `http_req_failed` rate < 1%, `http_req_duration` p(95) < 250ms, `checks` rate == 100%. Each
request carries `X-Trace-Id: <vu>-<iter>-<run-id>`.

A down or unreachable server makes every request fail (`http_req_failed` always counts connection-level
failures, and `perf/lib/api.js`'s `http.setResponseCallback` also counts non-200 responses), which fails the
thresholds and makes k6 exit non-zero — the script never reports a vacuous pass when it can't actually reach
the API.

## Run it

Start the server with a scratch data directory so test data doesn't collide with real app data:

```sh
DATA_DIR=$(mktemp -d) deno task serve
```

In another terminal, from the repo root:

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
commit. A later workstream joins these files for analysis (see `docs/adr/0003-k6-perf.md`).

Override target/auth with environment variables if needed: `BASE_URL` (default `http://127.0.0.1:8000`),
`APP_ID` (default `tic-tac-toe`), `TOKEN` (default `dev-only`, matching the server's dev default).

## Load scenarios

Three scenarios sit beside `smoke.js`. They share `perf/lib/scenarios.js` (seeding, the weighted read mix,
conflict-aware writes, artifact paths, `handleSummary`) and call the API only through `perf/lib/api.js`.

| task                   | script             | shape                                                                                                                                         | thresholds                                                                                            |
| ---------------------- | ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `deno task perf:load`  | `mutation_load.js` | `ramping-vus` 1 → 20 over 1 min, hold 1 min; each iteration plays its own game                                                                | `checks` rate > 0.99; `http_req_duration{expected_response:true}` p(95) < 400; `http_req_failed` < 1% |
| `deno task perf:query` | `query_load.js`    | `constant-arrival-rate` 50 rps for 2 min over 30 seeded games; `getGame`/`listGames`/`moveHistory`/`winsByMark` 4:2:2:1                       | `http_req_failed` < 1%; `http_req_duration` p(95) < 500; `checks` rate > 0.99; no dropped iterations  |
| `deno task perf:mixed` | `mixed.js`         | `constant-arrival-rate` 50 rps for 3 min, 80% reads (same mix) / 20% writes (one move of the VU's own game, or `createGame` when it finished) | same as `perf:query`; **the default scenario for regression comparison**                              |

**Conflicts are a success.** `mutation_load.js` deliberately sends a stale-`expectedVersion` move before every
third move of a game. The server must reject it; the run records that in the custom `conflicts` counter and a
check, and the request is declared an expected status (`400` today, `409` once the server maps
`ConflictError`), so it does not count in `http_req_failed`. A stale write that is _accepted_ fails a check.

**Seeding** happens in k6's `setup()`: `query_load.js` and `mixed.js` create 30 games (X wins, O wins, draws,
in-progress) through the API before the timed phase, so a fresh `DATA_DIR` needs no preparation. Seed requests
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
filter `WHERE type='Point'`, see `docs/haiku-experiments/findings/sq-04.md`) and `summary.json` (written by
the scenario's `handleSummary`, with `run_id` and `scenario` added) land there. `K6_RUN_ID` also feeds the
`X-Trace-Id` suffix, so once item 22 lands the trace ids in the server's events match the directory name. If
you forget it the run id falls back to `k6-smoke`, which is smoke's directory — always set it.

`work-scripts/bash/k6-scenario-run.sh <perf:task> [run-id] [k6 args]` does all of the above against a
throwaway server on a free port with a fresh scratch `DATA_DIR`:

```sh
bash work-scripts/bash/k6-scenario-run.sh perf:mixed 2026-10-10-mixed
```

Thresholds are enforced by k6: a crossed threshold makes the task exit 99 (verified by temporarily changing
`query_load.js` to `p(95)<1`). `BASE_URL`, `APP_ID` and `TOKEN` work as for `smoke.js`.

### What these numbers mean

These scenarios measure this prototype's documented design: **one writer connection per app and one serial
reader process** shared by every query. `perf:load` is bounded by the single writer, `perf:query` by the
serial reader — that is the known capacity limit in the repo README, and `query_load.js` is the scenario that
exercises it. The thresholds and the numbers they produce are for **regression detection** — compare a run to
an earlier run of the same scenario on the same machine — not capacity claims.

Reference run on the dev droplet (2026-10-10, one run each, before items 12–22 land): `perf:load` p(95) 162 ms
with ~4k conflicts counted; `perf:query` p(95) 31 ms; `perf:mixed` p(95) 57 ms.

## Not a benchmark

As the repo README states under "Deliberate limits": this is a local, trusted-application prototype with one
writer connection per app and one shared serial reader process, "enough to expose the design, not to establish
capacity." Treat `smoke.js` as a functional/regression check against the live HTTP surface, not as a capacity
or performance claim about this system.

## FFI production-binding harnesses

Run each harness independently on a quiet host; each prints JSON and fails its acceptance gates with a nonzero
exit. These are ports of `origin/research/fable-lang-switch:work-scripts/deno/ffi-sqlite-*.ts`, using the
landed `Database`/`Statement` API. The read-cost and interrupt reports retain the PoC's top-level metric keys,
with `loadavg` (1/5/15 minutes) added.

```sh
deno run -A perf/ffi_read_cost.ts
deno run -A perf/ffi_interrupt.ts
deno run -A perf/ffi_cap.ts
```

`ffi_read_cost.ts` measures 20,000 reused-statement point reads per path on identical 1,000-row WAL files. The
binding and `node:sqlite` each own a separate file. Blocking rounds alternate order, and result correctness is
checked outside the measured interval. Bind arrays and expected TEXT are precomputed; both blocking paths and
the nonblocking path get 2,000 excluded warm-ups. Both public APIs receive the same numeric values: Statement
takes the reused array and node:sqlite takes its spread values. Gates: blocking p50 ≤ 10.2 µs (6.8 × 1.5) and
≤ 1.3 × same-run node p50 + 1 µs (the plan's amended parity gate); nonblocking p50 ≤ 100 µs and p99 ≤ 3,000
µs.

`ffi_interrupt.ts` runs 50 one-second unbounded counts and aborts each through the binding's signal bridge. It
measures abort → typed `SqliteCancelled` settlement, including reset. Each sample has the PoC fields; the
top-level sample is the slowest interrupt. The PoC's `rc` field is dropped: the binding's `SqliteCancelled`
carries no result code, so the typed cancellation settlement is the measurement (the PoC read the step's raw
result code, 9 = `SQLITE_INTERRUPT`; an earlier port printed that as a hard-coded constant, which measured
nothing); every other PoC key is kept. Gates: interrupt p99 ≤ 20 ms and at least 90 ten-millisecond timer
ticks in each window.

`ffi_cap.ts` submits 32 unbounded counts in one main-thread turn, each on its own temp file and connection,
via `Statement.allAsync(undefined, { timeoutMs: 50 })`. Every cancellation must have reason `deadline` and
finish within 100 ms from its own submission. Cleanup awaits all connections before removing the scratch
directory.

**Gate status: the amended same-run `node:sqlite` parity gate PASSES.** The plan's FFI-06 read gate was
originally "binding blocking p50 ≤ node:sqlite p50 in the same run". Nick approved an amendment on 2026-10-13
(recorded in the plan, risk 19): **blocking p50 ≤ 1.3 × node:sqlite p50 + 1 µs**, the same formula FFI-01
used. `ffi_read_cost.ts` now asserts that formula and exits 0 on a quiet host. Quiet-host blocking p50 runs
7.35 / 7.64 / 7.57 µs against bounds of 1.3 × 6.93 + 1 = 10.01 µs, 1.3 × 7.21 + 1 = 10.37 µs and 1.3 × 7.10 +
1 = 10.23 µs: all three PASS. They remain 3.5-6 % above `node:sqlite` itself, so the original strict gate
would have failed; the Opus review traced that gap to the floor of four FFI crossings per read against node's
single native call, and the PoC's "6.35 vs 8.66 µs" win to a cold, unwarmed, non-interleaved harness. Every
other gate also passes (the PoC budget of 10.2 µs, the nonblocking budgets, the interrupt and cap gates).

Measured on vm-1, Deno 2.9.5, 2026-10-14, quiet-host runs (fair harness above), 20,000 samples per path per
run. All three runs fail the original strict parity but pass the amended gate; all three absolute blocking and
nonblocking budgets pass.

| Run | Load 1/5/15        | node p50/p99 µs | FFI blocking p50/p99 µs | FFI nonblocking p50/p99 µs | Strict parity | Amended gate (bound µs) |
| --- | ------------------ | --------------- | ----------------------- | -------------------------- | ------------- | ----------------------- |
| 1   | 0.50 / 1.35 / 1.76 | 6.93 / 28.00    | 7.35 / 29.97            | 43.85 / 322.94             | FAIL          | PASS (10.01)            |
| 2   | 0.70 / 1.38 / 1.77 | 7.21 / 29.24    | 7.64 / 29.66            | 47.56 / 745.04             | FAIL          | PASS (10.37)            |
| 3   | 0.70 / 1.38 / 1.77 | 7.10 / 29.19    | 7.57 / 31.80            | 47.80 / 480.43             | FAIL          | PASS (10.23)            |

Interrupt and cap results (earlier, loaded host, load 2.09 / 2.13 / 2.79; the review re-ran both on a quiet
host at load 0.58 with interrupt p99 4.034 ms, minimum 90 ticks, and cap max 84.5 ms):

| Measurement   | Result                                       | Gate                 |
| ------------- | -------------------------------------------- | -------------------- |
| Interrupt p99 | 6.114 ms; minimum 92 ticks across 50 windows | Pass (50/50 windows) |
| 32-reader cap | 32/32 deadline cancellations; max 71.050 ms  | Pass (≤ 100 ms each) |

Historical read runs at higher host load (retained for the record; the quiet-host runs above are the
representative numbers):

| Run                      | Load 1/5/15        | node p50/p99 µs | FFI blocking p50/p99 µs | FFI nonblocking p50/p99 µs | Strict parity | Amended gate (bound µs) |
| ------------------------ | ------------------ | --------------- | ----------------------- | -------------------------- | ------------- | ----------------------- |
| First unit run (loaded)  | 1.84 / 2.08 / 2.78 | 8.28 / 34.65    | 8.96 / 37.18            | 49.52 / 419.00             | FAIL          | PASS (11.76)            |
| Earlier continuation run | 1.38 / 1.35 / 1.39 | 6.84 / 22.48    | 7.60 / 24.01            | 42.60 / 208.36             | FAIL          | PASS (9.89)             |

Every read run is retained in the evidence report; the quiet-host results supersede neither the historical
strict-parity failures nor the amended gate's own runs.

The committed research JSON (`deno-ffi-read.json`) has blocking 6.35 / 27.77 µs, node 8.66 / 34.18 µs and
nonblocking 44.31 / 241.36 µs; its load file spans 3.30 → 4.93. `deno-ffi.json` has one interrupt at 0.408 ms
with 95 ticks. A 50-sample tail is a different statistic from that single observation. Apart from the approved
parity amendment (Nick, 2026-10-13; plan risk 19), no failed threshold is relaxed by these ports.

`deno task integration` also drives a 50 ms deadline over real HTTP using two API-key tenants: 500,000 JSON
games make `winsByMark`'s grouped count run past the deadline, while primary-key reads on the other tenant
remain responsive. It asserts a flat point-read p99 < 50 ms and prints the host load average beside it. It
uses an isolated database created on the dev Postgres (`DATABASE_URL`, default
`postgres://mansio:mansio@localhost:5432/mansio`) and drops it on exit. After server shutdown it runs
`logs:ingest` and `logs:query deadline-hits-per-handler-hour`, asserting the deadline rows have
`path = 'nonblocking'`. Old JSONL without `path` projects NULL; ingest adds the column idempotently to
existing v2 event logs.

Integration runs, newest first (every recorded p99 is under the plan's flat 50 ms; the harness asserts that
flat budget and prints the load average beside it). The `logs:query deadline-hits-per-handler-hour` result had
the same shape every time, e.g.:

```json
{
  "app_id": "tic-tac-toe-json",
  "handler_kind": "query",
  "name": "winsByMark",
  "path": "nonblocking",
  "deadline_hits": 20
}
```

- Second continuation (final repaired code): 20/20 HTTP 504, 152 competing point reads, p99 **20.06 ms** at
  load 5.63 / 5.62 / 4.13 (a heavily loaded host; it passed the flat 50 ms); `logs:ingest` 758 lines, zero
  rejects.
- First continuation, after the correlation repair: 20/20 HTTP 504, 181 competing point reads, p99 **16.44
  ms** at load 2.13 / 2.38 / 2.13; ingest 874 lines, zero rejects. Its first attempt failed catalog
  attribution (19 matched, one unmatched) because the SQL timestamp was 1 ms after the HTTP completion
  timestamp. The join now allows the documented 1 ms rounding at both request-window boundaries. The repaired
  query also returns all 20 matched hits against that attempt's retained database; the failure remains in the
  evidence.
- First unit run (pre-repair, historical): 20/20 HTTP 504, 153 competing point reads, p99 **22.10 ms** at load
  1.79 / 2.09 / 2.81; ingest 762 lines, zero rejects.

An earlier interrupt run overlapped the FFI-07 seed sweep and failed the tick gate (minimum 61 ticks); its
latency p99 was 6.293 ms. Both runs are retained under `agent-work/items/ffi-06-evidence/`. A scratch old-v2
migration check ran the DDL twice and preserved 165 SQL rows with NULL paths.
