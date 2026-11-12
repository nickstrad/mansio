# agentd's event log

What agentd writes to its JSONL event log, what each record means when handlers run in the SDK, and how the
writer behaves under load. Code: `server/internal/eventlog`, `server/internal/front/opstrace.go`,
`server/cmd/agentd/events.go`.

## The pieces

| File                                 | Role                                                                                   |
| ------------------------------------ | -------------------------------------------------------------------------------------- |
| `server/internal/eventlog/record.go` | the envelope and the 7 kinds, the record key order, redaction (byte goldens)           |
| `server/internal/eventlog/log.go`    | `Log`: bounded queue, one writer, file layout and rolls, drop reports, `Stats`         |
| `server/internal/front/opstrace.go`  | `OpsObserver`/`OpsTrace`: the adapter's trace id, handler name, sizes; tx + statements |
| `server/cmd/agentd/events.go`        | `event_log` config, OpsEvent → http/handler/tx/sql records, lifecycle, metrics         |
| `sdk/transport.ts`, `sdk/client.ts`  | `X-Mansio-Handler: <handler name>` on every request of a call                          |
| `perf/lib/ir.js`, `perf/lib/api.js`  | k6 on `/v1/ops`: the handlers as the SDK sends them                                    |

## Decisions

- **agentd only writes JSONL.** The envelope, key order, file layout and rolls match the TypeScript event log
  the Deno platform wrote. agentd has no ingest or query endpoint (no `/admin/logs/*` on agentd or controld):
  that would put DuckDB, or a child process, behind a Go endpoint for no gain. Ingesting and querying the
  files is an offline job outside the server; when this was built it was a TypeScript CLI over DuckDB.
- **Records carry role `server`**, the role the Deno platform's records had, so analysis keyed on the role
  works unchanged.
- **Never block the request path.** Telemetry must not slow or stall the work it observes. `Emit` is a
  non-blocking send to a bounded channel (8192); when it is full the record is counted, reported as
  `queue events dropped` before the next batch, and exposed as `mansio_eventlog_records_total{result}`. Write
  failures lose records (counted) and WARN at most once a minute.
- **What a Go `handler` record is.** The SDK runs the handler; agentd sees its reads and its commit as
  separate requests. agentd emits one `handler` record per decoded request (`name` from `X-Mansio-Handler`,
  duration = the pipeline), so mutation-vs-query latency and k6-vs-server comparisons still work, and the
  bound client p95 ≥ server p95 still holds per request. App errors (`ConflictError` from `makeMove`,
  `RuleError`) are local to the SDK and never logged by agentd.
- **Redaction** matches the TypeScript encoder's: Bearer tokens, `X-Amz-*` parameters, secret-named keys, line
  breaks, depth 32. Bind values are never logged (no opt-in in Go). `errorDetail` is the error body the client
  got, not the Go cause (that stays in the slog log with its log id).
- **Backups**: a durable-mode `POST /admin/backup` is one `backup` record (`publish-latest`, emitter
  `backups`, the manifest's object, size, sha256); the write-back has no phases to report. Not e2e-covered.
- **Trace ids**: `X-Trace-Id` is accepted when it is a trace token (1–128 of `[A-Za-z0-9._:-]`), else agentd
  draws a UUIDv7 from the server's id source; it is echoed on every `/v1/ops` answer while the event log is
  on.

## Tests

- `go test -race ./...`, `-tags noassert`, and the gated suites.
  `server/internal/eventlog/testdata/goldens.jsonl` holds byte goldens taken from the TypeScript encoder's
  output; `record_test.go` holds the Go encoder to them.

## Traps

- **JS regexp semantics are not Go's.** The format was defined by a JavaScript encoder. JS `\s` is Unicode
  white space plus U+FEFF (Go's is ASCII); the `i` flag without `u` folds ASCII only (Go's `(?i)` also folds
  U+017F and U+212A); `toLowerCase` maps U+212A to `k`; `Array#sort` orders keys by UTF-16 units; numbers use
  `Number#toString` (`1e+21`, `1e-7`). The Go encoder spells each out, and the goldens pin them.
- **File names are scanned, never counted.** seq = 1 + the highest on disk for that hour and pid, opened
  `O_EXCL`, so a restarted pid 1 never appends to a torn file.
- The flush barrier's trace id ends in `-<run id>`, so joining k6 requests to server records counts one more
  request on the server side: the barrier is a `/v1/ops` read.
