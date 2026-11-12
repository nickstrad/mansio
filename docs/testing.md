# Testing

Since IR-6 (the cut-over item of the intermediate representation, or IR, plan) the server is the Go server in
`server/`; the Deno platform and its tests are gone (ADR 0013). The TypeScript tests cover what is left in TS:
the SDK (`sdk/`), the sample app (`examples/tic-tac-toe/`), `core/` (schema, plan, errors, the compiler that
is the oracle for Go, the IR decode/validation rules and the event-log record format), the shared IR vectors
(`intermediate-representation/`) and the offline event-log CLI (`eventlog/`, `scripts/logs.ts`). The e2e suite
is the Go server's conformance suite.

| Tier                   | What it proves                                                                                                                      | Runner                                                                                                              |
| ---------------------- | ----------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| Unit on core/ and sdk/ | Schema validation, normalization, SQL compilation/decoding, errors, IR decode/validation/schema/compile vectors, SDK client, rules  | `deno task test` (`tests/unit/`, `tests/core_test.ts`)                                                              |
| Property               | Generated schemas, plans, values and SDK runs, with shrinking and deterministic seeds; compiled SQL against `node:sqlite`           | `deno task test:prop` (`tests/property/`)                                                                           |
| Event-log CLI          | Exactly-once JSONL → DuckDB ingest, retention sweep, the analysis catalog, the CLI's own logger, under `ConfigTest`/`EventSinkTest` | `deno task test` and `test:prop` (`eventlog_test`, `retention*`, `logger_test`, `analysis_catalog_test`, `offsets`) |
| Go server              | Everything the server does: compiler port, executor, actors, tenancy, durability, the cluster sim (DST)                             | `deno task go:ci` (gofmt, vet, `go test -race`, `go test -tags noassert`); see `server/README.md`                   |
| End to end             | The real binaries (agentd, controld, the edge, dbtool) driven through the SDK and the CLIs, against Postgres and SeaweedFS          | `scripts/dev/e2e-linux.sh` (`deno task e2e` inside Linux)                                                           |

## Required gate

```sh
scripts/dev/deno-linux.sh 'deno task ci'
deno task go:ci
```

`deno task ci` runs fmt:check, lint, check, test, test:prop in order. Run it on Linux: on macOS the DuckDB
event-log suites hang ("Promise resolution is still pending"), on main before IR-6 as well. `deno task ci:all`
runs both gates. Neither includes e2e or the k6 observability test.

## Replaying a property failure

`tests/property/arbitraries.ts` sets 200 runs and the default seed 20261010, prints it in test names, and
appends a replay command to fast-check's counterexample report. Replay the reported seed, for example:

```sh
PROP_SEED=20261010 deno task test:prop
```

Substitute the failing integer, keep the same source/dependency versions, and preserve the reported
counterexample, shrink path and test title. The harness exposes a seed override, not a shrink-path environment
option; replaying the whole seed redoes the original generation and shrinking. A different seed explores new
cases rather than reproducing that failure. `scripts/dev/property-seed-sweep.sh` sweeps one file over a seed
range.

## IR vectors

`tests/unit/intermediate_representation_vectors_test.ts` runs `decode.json`, `validation.json`, `schema.json`
and `compile.json` against the TS implementation, and checks that every committed vector file (`execute.json`
included) and the compile corpus still match their generators
(`scripts/intermediate-representation/gen-vectors.ts`,
`scripts/intermediate-representation/gen-compile-corpus.ts`). `execute.json` runs only in Go
(`server/internal/docexec`); its TS run went with the Deno platform's reference executor at IR-6.
`scripts/dev/intermediate-representation-differential.sh` runs the compile corpus at scale against the Go
compiler.

## End to end

```sh
scripts/dev/e2e-linux.sh                       # all scenarios
scripts/dev/e2e-linux.sh --filter documents    # deno test args pass through
```

The suite needs Linux (process groups, `setsid`, `/proc`), so the script runs it in `denoland/deno:2.9.5` with
a sibling Postgres and SeaweedFS, and cross-compiles agentd, dbtool, controld and the edge on the host. It
runs eight seeded scenarios (`tests/e2e/platform/seed.ts`): documents, queries, backup/restore, tenant
isolation, auth, log analysis, admin lifecycle and scale-out. Each starts its own agents with scratch
directories and ephemeral ports; the run verifies cleanup and an unchanged `git status`, and fails if its
runtime bound is exceeded. `BACKEND=deno` is gone; the script still accepts a leading `go` and refuses `deno`.

The k6 observability acceptance test runs k6 smoke against a scratch agentd, ingests both sides into DuckDB
and joins client and server latencies:

```sh
deno run -A tests/e2e/observability_test.ts
```

It needs `k6` and Go (or `AGENTD_BIN`) and is host-dependent; see [perf/README.md](../perf/README.md).

## Boundary checks and intended scope

`tests/unit/boundaries_test.ts` applies the console and environment rules to the shipped library only:
`core/`, `eventlog/`, `sdk/`, `examples/tic-tac-toe/`. There, `console.*` is allowed only in
`eventlog/logger.ts` (the documented degradation path), and `Deno.env` only in `eventlog/config.ts`. Scripts,
tests, perf and work-scripts are outside those two scans because CLI output and test/setup environment access
are legitimate there. The core import/Deno rule and the untyped-throw rule (`core/`, `eventlog/`) keep their
own file scopes. The rules that kept `node:sqlite` out of the processes that used the FFI SQLite binding went
with the binding.
