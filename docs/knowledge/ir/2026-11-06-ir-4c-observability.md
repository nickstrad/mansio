# 2026-11-06 — IR-4c: agentd writes mansio's event log; the TS CLI ingests and queries it

Question: how does the Go backend feed the event-log analysis (`docs/observability.md`) without DuckDB in Go,
and what do its records mean when handlers run in the SDK? Built on `ir/04c-observability` from `main` at
`6352bfb`. Plan: [`docs/plans/replace-platform-with-go.md`](../../plans/replace-platform-with-go.md) §5 phase
5, [`docs/plans/ir-protocol.md`](../../plans/ir-protocol.md) IR-4.

## The pieces

| File                                 | Role                                                                                   |
| ------------------------------------ | -------------------------------------------------------------------------------------- |
| `server/internal/eventlog/record.go` | the envelope and the 7 kinds, `core/events.ts` key order, redaction (byte goldens)     |
| `server/internal/eventlog/log.go`    | `Log`: bounded queue, one writer, TS file layout and rolls, drop reports, `Stats`      |
| `server/internal/front/opstrace.go`  | `OpsObserver`/`OpsTrace`: the adapter's trace id, handler name, sizes; tx + statements |
| `server/cmd/agentd/events.go`        | `event_log` config, OpsEvent → http/handler/tx/sql records, lifecycle, metrics         |
| `sdk/transport.ts`, `sdk/client.ts`  | `X-Mansio-Handler: <handler name>` on every request of a call                          |
| `perf/lib/ir.js`, `perf/lib/api.js`  | k6 with `BACKEND=go`: the handlers as the SDK sends them to `/v1/ops`                  |
| `tests/integration.ts`               | `startScratchAgent`: agentd + event log + minted tickets, offline `ingest`             |
| `tests/e2e/.../log_analysis.ts`      | both backends; no reader IPC; Go = JSONL + CLI                                         |

## Decisions

- **Ingest and admin logs stay in TS, offline.** agentd only writes JSONL; `deno task logs:ingest` /
  `logs:query` with `LOG_DIR = event_log.dir` are the ingest and the analysis (plan §7: keep the TS ingest as
  a CLI). No `/admin/logs/*` on agentd or controld: it would put DuckDB (or a Deno child) behind a Go endpoint
  for no gain, and DuckDB's single-writer lock already forces "ingest, then query".
- **Role `server`**, so `SOURCE_ROLES`, retention and the k6 join (`role = 'server'`) need no change.
- **Never block the request path** (RI B1/B2): `Emit` is a non-blocking send to a bounded channel (8192); full
  → counted, reported as `queue events dropped` before the next batch (the TS sink's rule), exposed as
  `mansio_eventlog_records_total{result}`. Write failures lose records (counted), WARN at most once a minute.
- **What a Go `handler` record is.** The SDK runs the handler; agentd sees its reads and its commit as
  separate requests. agentd emits one `handler` record per decoded request (`name` from `X-Mansio-Handler`,
  duration = the pipeline), so `mutation-vs-query-latency` and `k6-vs-platform` work, and the client-p95 ≥
  server-p95 bound still holds per request. App errors (`ConflictError` from `makeMove`, `RuleError`) are
  local to the SDK and never logged by agentd.
- **Redaction** is core/events.ts's (Bearer tokens, X-Amz-* parameters, secret-named keys, line breaks, depth
  32); bind values are never logged (no opt-in in Go). `errorDetail` is the error body the client got, not the
  Go cause (that stays in the slog log with its log id).
- **Backups** (after IR-4b landed): a durable-mode `POST /admin/backup` is one `backup` record
  (`publish-latest`, emitter `backups`, the manifest's object, size, sha256); the write-back has no phases to
  report. Not e2e-covered: `log_analysis` runs a static-mode agent.
- **Trace ids**: `X-Trace-Id` accepted when it is a TRACE_TOKEN (1–128 of `[A-Za-z0-9._:-]`), else a UUIDv7
  from the server's id source; echoed on every `/v1/ops` answer while the event log is on.

## Results

- Go: `go test -race ./...`, `-tags noassert`, gated; `internal/eventlog` goldens equal `toJsonLine` output.
- e2e: `work-scripts/bash/e2e-linux.sh go` runs `log_analysis` on agentd (with documents, queries), and `deno`
  still passes it.
- `BACKEND=go deno run -A tests/e2e/observability_test.ts` passes natively on macOS (k6 0.56 from Homebrew);
  `OBS_SINK=noop` fails it as it should. The Deno variant passes in `denoland/deno:2.9.5` with the Linux k6
  binary copied out of `grafana/k6:0.56.0`.

## Traps

- `cli()` in the harness runs with a cleared environment: the Go scenario passes `cleanEnv()` plus `LOG_DIR`,
  `EVENTLOG_DUCKDB`, `DATA_DIR` (a GoAgent has no `env`).
- The flush barrier's trace id ends in `-<run id>`, so the k6 join's server side counts one more request than
  the client side on Go (the barrier is a `/v1/ops` read there, a `/health` on Deno).
- `deno check tests/integration.ts` fails on macOS on `main` too (`Uint8Array<ArrayBufferLike>` vs
  `BufferSource`); use `work-scripts/bash/deno-linux.sh 'deno task check'`.
