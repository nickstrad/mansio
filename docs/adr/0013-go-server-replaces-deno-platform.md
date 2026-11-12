# ADR 0013 — The Go server replaces the Deno platform

- **Status:** accepted
- **Date:** 2026-11-06
- **Deciders:** `docs/plans/completed/replace-platform-with-go.md` (removed; see git history) §5 phase 7, §6;
  recorded by IR-6 (`docs/plans/completed/intermediate-representation-protocol.md` (removed; see git history)
  §5)
- **Related:** [ADR 0009](0009-intermediate-representation-is-the-app-wire.md),
  [ADR 0010](0010-compiler-port-with-ts-oracle.md), [ADR 0012](0012-single-node-auth-port.md); supersedes
  [ADR 0001](0001-effect-port.md)'s process split, [ADR 0004](0004-eventlog-duckdb-in-reader-only.md) and
  [ADR 0008](0008-statement-execution-via-ffi.md) for the server
- **Implements:** branch `ir/06-cutover` (deletes the Deno platform's `platform/` tree; adds `eventlog/`)

## Decision

1. **The Go server in `server/` is the only server.** agentd (single node: static, tenancy or durable mode),
   controld, the edge and dbtool serve every app over the IR (`POST /v1/ops`, ADR 0009). The Deno platform
   (`platform/`: the SQLite FFI writer, the DuckDB reader child process, Better Auth, the `TENANCY` modes, the
   S3 backup orchestrator, `/admin/logs/*`) is deleted, with its tests, its CLIs (`scripts/auth.ts`,
   `scripts/restore.ts`, `scripts/db/migrate.ts`, `scripts/demo.ts`), `demos/`, and its Deno tasks (`serve`,
   `restore`, `db:migrate`, `integration`, `demo`). The SDK drops its pre-IR `createLegacyClient`.
2. **The e2e suite is the Go server's conformance suite.** `BACKEND=deno` is gone; the eight scenarios run on
   Go only (`scripts/dev/e2e-linux.sh`).
3. **What stays in TypeScript:** `sdk/`, `examples/tic-tac-toe/`, `core/` (schema, plan, errors, the compiler
   that is the oracle for Go per ADR 0010, the IR rules, and the event-log record format `core/events.ts` with
   `core/eventlog/` and `core/logging/`), the shared IR spec and vectors (`intermediate-representation/`),
   `perf/` (k6, now always over `/v1/ops`), and the offline event-log CLI.
4. **The event-log ingest and DuckDB analysis stay in TypeScript, as an offline CLI** (`scripts/logs.ts` over
   `eventlog/`). Go has no ingest: agentd writes JSONL in the `core/events.ts` format (IR-4c, goldens shared
   with Go), and the CLI ingests it into DuckDB, sweeps retention and runs the named catalog. The ingest,
   tables, retention, logger and catalog modules move from the platform unchanged; their configuration is cut
   down to the log tree and the event-log file (`DATA_DIR`, `LOG_*`, `EVENTLOG_DUCKDB`).
5. **The TS reference executor is dropped, not moved.** It ran on the platform's FFI binding; Go runs every
   `execute.json` vector (`server/internal/docexec`). The TS run keeps decode, validation, schema and compile
   vectors, and the check that every vector file matches its generator (whose execute expectations are
   hand-written).

## Why

- Phases 0 to 6 of the replace plan and IR-0 to IR-5a ported every e2e scenario to Go and passed them on both
  backends; keeping two servers meant every change was made and tested twice.
- The platform's scaling issues (one JS event loop and global locks for every tenant's SQLite work,
  `docs/architecture/scaling_issues.md`) are what the Go agent's one-actor-per-database design removes.
- Porting the ingest to Go would duplicate about 3k lines of tested code for no user-visible change; the JSONL
  contract between the two is pinned by shared goldens.
- The executor's only remaining job was a second run of vectors Go already runs; keeping it would have kept
  the FFI binding (about 2k lines) alive for that.

## Consequences

- `deno task ci` covers only the surviving TS (fmt, lint, check, unit and property tests); it must run on
  Linux (`scripts/dev/deno-linux.sh`) because the DuckDB event-log suites hang on macOS. `deno task go:ci`
  covers the server.
- There is no TS server to fall back to. Behavior that only the Deno platform had is gone: the legacy
  `/apps/:app/:kind/:name` routes, server-side handlers, the platform's own S3 snapshot format and
  `scripts/restore.ts`, `/admin/logs/*`, Better Auth's other endpoints (ADR 0012 lists what was not ported).
  Deno-written data files remain importable with `dbtool import` and Deno tenants with
  `controld import-mansio` (ADR 0012); the e2e check that imported a freshly Deno-written file went with the
  platform.
- The open durability question is unchanged and is now the only server's: writes are acknowledged before
  write-back, so a kill -9 can lose the window (`docs/plans/completed/intermediate-representation-protocol.md`
  §9; S3 backup work is paused).
- ADRs 0001, 0004 and 0008 now describe history for the server; their TS-side rules that still apply (pure
  `core/`, a single DuckDB writer, the text storage contract) stay in force for what remains.
