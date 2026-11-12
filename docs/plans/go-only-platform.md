# Plan: TypeScript only in the SDK

Status: phase 1 done (2026-11-12, ADR 0014). Phases 2 to 4 are proposed and wait for a decision.

## Goal

The platform is Go. TypeScript is the client: the SDK, the parts of `core/` the SDK imports, the examples, and
the tests of those. Today other TypeScript sits beside them at the repository root and makes the layout hard
to read. This plan removes it in phases, each of which leaves every gate green.

What the SDK and the examples import from `core/` today (computed from their imports): `errors.ts`, `plan.ts`,
`schema.ts`, and `intermediate-representation/{json,schema,types}.ts`. Everything else in `core/` exists for
something that is not the SDK.

| TypeScript today                                                                                                                            | Why it exists                                                                                                     | Phase |
| ------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- | ----- |
| `eventlog/`, `scripts/logs.ts`, `core/events.ts`, `core/eventlog/`, `core/logging/`, `perf/ingest_k6.ts`, `scripts/duckdb/` and their tests | The offline event-log tool: removed, not ported                                                                   | 1     |
| `scripts/tenants.ts`                                                                                                                        | Tenant admin CLI against agentd's admin API                                                                       | 2     |
| `core/compile/`, `scripts/explain.ts`, `core/compile/GENERATED_SQL.txt`, the compile vectors' generator and their tests                     | The TypeScript SQL compiler: the reference the Go compiler is checked against (ADR 0010); the SDK does not use it | 3     |
| `core/intermediate-representation/{decode,errors,semantics}.ts`, `scripts/intermediate-representation/gen-vectors.ts`                       | The TypeScript reference decoder and the generator of the shared vectors                                          | 3     |
| `sdk/`, the rest of `core/`, `examples/`, `tests/` for them, `tests/e2e/`                                                                   | The client, and the suite that drives the Go binaries through it                                                  | stays |

`tests/e2e/` stays TypeScript on purpose: it is the only suite that runs the real SDK against the real server.
`perf/*.js` stays JavaScript because k6 scripts are JavaScript.

## Phase 1: remove the offline event-log tool (done)

### Decision (Nick, 2026-11-12)

Delete the TypeScript event-log tool and do not port it. It was a private log pipeline for local use: load
agentd's JSON lines into a DuckDB file, track offsets, answer twelve canned questions, trim old files. A
deployed platform gets those from its logging and metrics stack, so a Go port would be a tool to retire.

What stays is the part a deployment needs: **agentd's structured request log**. `server/internal/eventlog`
writes one JSON line per HTTP request, handler run, SQL statement and transaction, all carrying the request's
trace id, through a bounded queue, into rolled files that the writer itself keeps under a size and age limit.
A log collector can ship those files as they are. Health and overload are Prometheus metrics, which exist.

| Need                              | Before             | After                                                                                  |
| --------------------------------- | ------------------ | -------------------------------------------------------------------------------------- |
| Is it healthy, is it overloaded   | not covered        | `/metrics` and the shipped alert rules                                                 |
| What happened to this request     | `logs:query trace` | Search the request log by trace id (`jq`, `duckdb`, or the log store it is shipped to) |
| Throughput, error rates, slow SQL | catalog queries    | Metrics for the first two; a query over the request log for the third                  |
| Retention                         | `logs:sweep`       | The writer's own limits; the log store's retention                                     |

### What is deleted

`eventlog/`, `scripts/logs.ts`, `scripts/duckdb/`, `core/events.ts`, `core/eventlog/`, `core/logging/`,
`perf/ingest_k6.ts`, `tests/layers/`, `tests/e2e/observability_test.ts`, the event-log, events, logger,
retention, rotation, offsets and catalog tests in `tests/unit/` and `tests/property/`, the `logs:*` and
`perf:ingest` tasks and the `@duckdb/node-api` import in `deno.json`, and the boundary rules in
`tests/unit/boundaries_test.ts` that only concerned those files. Error types in `core/errors.ts` that only the
tool used go too. Nothing replaces them in Go. The rest of `tests/` is left as it is.

### What changes

- **The record format has one owner.** `server/internal/eventlog` (`record.go`) is the definition; its Go
  tests are the format's tests. The golden test that kept Go and TypeScript byte-identical goes with
  `core/events.ts`; keep the golden records as Go test data if the Go tests do not already pin the bytes.
- **The e2e scenario** `tests/e2e/platform/scenarios/log_analysis.ts` checks the request log directly: after a
  traced query and mutation, the JSON lines hold the http, handler, sql and tx records with that trace id.
- **Docs.** `docs/observability.md` becomes the reference for the request log: where the files are, the record
  kinds and their fields, the writer's limits and counters, how to ship it, and three or four worked
  one-liners that read the files directly (`jq`, and `duckdb -c "... read_json_auto('logs/**/*.jsonl')"`): one
  request by trace id, the slowest SQL, errors by handler. `docs/guide.md` ("Observability",
  "Troubleshooting"), `docs/testing.md`, `README.md`, `CLAUDE.md`, `server/README.md`, `perf/README.md` and
  the learning paths follow. A new ADR supersedes ADR 0004 and ADR 0007 (ingest before retention). The diagram
  dataset in `docs/pages/` loses the "Offline log CLI" and "Derived DuckDB copy" elements and its
  observability view shows the request log and the metrics endpoints.

### Done when

- `deno task go:ci`, `scripts/dev/deno-linux.sh 'deno task ci'`, `scripts/dev/e2e-linux.sh` and the diagram
  site's `npm run check` pass.
- No file listed under "What is deleted" remains, and `git grep -i duckdb` finds only the worked one-liners,
  history in ADRs and notes, and the benchmark report scripts under `server/`, which use the `duckdb` CLI for
  a different purpose.
- A person can follow `docs/guide.md` "Observability" from a fresh checkout and find their own request, with
  its SQL, in the request log.

### What was decided while doing it

- **The goldens were already Go test data.** `server/internal/eventlog/testdata/goldens.jsonl` held one
  `{name, line}` per record and `record_test.go` already asserted every kind against it; the TypeScript test
  only wrote and checked the same file. Nothing was added: the file is now frozen Go test data, and only the
  comments and failure messages of `record_test.go` and `log_test.go` were changed to stop naming the deleted
  TypeScript files.
- **Also deleted, because they existed only for the tool:** `tests/helpers/run.ts` (the Effect test runner
  over `tests/layers/`, used only by the retention test) and `tests/e2e/scratch.ts` (the scratch agentd of the
  k6 join). From `core/errors.ts`: `IngestError`, `AnalysisError`, `UnknownAnalysisQuery`, `IngestPhase` and
  `QueueName`. `ConfigError` stays: it is a member of `StartupError` and the exhaustiveness test, and removing
  it changes the taxonomy beyond this phase.
- **macOS.** With the DuckDB suites gone, `deno task ci` no longer hangs natively on macOS, but one property
  test fails there and passes in Linux: `tests/property/json_number_test.ts` (a JSON double near 3.6e275
  compared with `gt`; macOS SQLite rounds it differently). It was hidden behind the hang before. So the docs
  keep "run the gate through `scripts/dev/deno-linux.sh`", with that reason instead of the hang.
- **`git grep -i duckdb` also finds the TypeScript and Go compilers' `duckdb` dialect** (`core/compile/`,
  `core/compile/GENERATED_SQL.txt`, their tests, `server/internal/doccompile/dialect.go`). That is the
  compiler's second dialect, phase 3's subject, not the event-log tool, so it stays.
- **Go comments and one message still name the tool**: the package comments of `server/internal/eventlog`
  (`log.go`, `record.go`, `retention.go`), `server/cmd/agentd/events.go` and `config.go` (`checkEventLog`'s
  "LOG_DIR for deno task logs:ingest" error text), `events_test.go`, and the generated
  `server/docs/capacity.md` runbook line ("before the offline ingest took them", from
  `server/cmd/agentd/capacity_test.go`). They were left to the agent rewriting Go comments, to keep the merge
  simple; they should be fixed when that lands.
- **perf.** `perf/lib/api.js` imported `./ir.js`, renamed earlier to `intermediate-representation.js`, so
  every k6 scenario failed to load; the import is fixed. The k6 tasks are unchanged apart from `perf:ingest`.
- **Diagrams.** The Markdown C4 pairs in `docs/architecture/` (context and observability) were redrawn as well
  as the site dataset: the operator reads or ships the request log and scrapes `/metrics`; no log store or
  Prometheus server is drawn, because none is part of the platform.

### Later, with the control plane hardening (not in this plan)

Log shipping to a store, request tracing (spans in place of the per-request records), and dashboards.

## Phase 2: `scripts/tenants.ts` in Go (proposed)

Move the tenant admin commands (`create`, `list`, `show`, `deactivate`, `issue-key`, `revoke-key`) into
`agentctl`, which already talks to agentd's admin listener. Small.

## Phase 3: one compiler, one decoder (proposed, needs a decision)

Delete the TypeScript SQL compiler and reference decoder, `deno task explain` and the vector generator. The Go
implementation becomes the only one; the shared vectors stay as frozen test data that the Go tests run and the
SDK's tests use for what the SDK encodes. This reverses ADR 0010 (the TypeScript compiler as oracle), so it
needs a decision and a new ADR. Open question: who generates new vectors when the protocol changes (a Go
generator, or hand-written cases).

## Phase 4: one folder per language (proposed)

After phases 1 to 3 the root holds only client code and neutral material. Fold what is left of `core/` into
`sdk/` (for example `sdk/core/`), move `tests/unit` and `tests/property` beside it, and leave at the root:
`sdk/`, `examples/`, `server/`, `intermediate-representation/` (spec and vectors), `tests/e2e/`, `docs/`,
`scripts/`, `perf/`.
