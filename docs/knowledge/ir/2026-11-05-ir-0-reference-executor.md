# 2026-11-05 — IR-0: the reference executor against today's engine

Question: what does running IR on today's engine change, and what did IR-0 have to decide? Read at `32a7f6a`
(`main`), built on `ir/00-spec`.

## How it relates to today's mutation path

`platform/ir/executor.ts` does not go through `Storage.mutation`. `Storage` runs a handler with a `Documents`
executor whose ops are `insert`/`patch`/`delete` without client ids, results or preconditions
(`platform/sqlite.ts`, `execute` ~l.866). The reference executor opens its own connection and reuses the same
pieces: `compileDDL`, the `_platform_schema` fingerprint check, `compileSelect`/`decode` and
`compileInsert`/`compileReplace`/`compileDelete`, so its files are interchangeable with `Storage`'s. It adds
mansio-server's `_mansio_requests` table (same DDL as `internal/actor/reqid.go`). It emits no events and has
no queue or deadline; IR-3 wires it into `Storage` for that. Checked: `tests/unit/ir_vectors_test.ts` (execute
vectors, both layouts) passes under `deno-linux.sh`.

Differences from today's semantics, all deliberate (spec §11):

- **One clock reading per request.** Today each insert reads `Clock` itself (`platform/sqlite.ts` insert
  case), so inserts in one handler can get different `_creationTime`s. IR reads once per request, so a replay
  reproduces it.
- **Patch merges decoded documents**, as today: `select` returns decoded docs, and the merge is
  `{...stored fields, ...patch}` then a full `compileReplace`. `{}` still bumps `_version`.
- **Delete of a missing id** answers `{deleted: false}` (today: silently `null`).
- **A write in a query** is a 400 decode error, not `QueryWriteAttemptError` (500): with IR the client sent
  it, so it is the request's fault.
- **Client-id collisions** are checked with a primary-key read before the insert and reported as
  `ConflictError` (`check: "exists"`), not left to fail as a `StorageError` from the PK constraint.

## Wire tag reuse

`core/errors.ts`'s `ConflictError` carries numeric `expected`/`actual`; IR's `exists` and `fields` checks need
other values. Rather than widen the core class, `core/ir/errors.ts` defines `PreconditionConflict` with
`_tag: "ConflictError"` (and `FingerprintMismatch` with `_tag: "SchemaDriftError"`, because the core class
names a file path). Same tag on the wire, so callers switching on `tag` need no new case.

## Trap: SQLite tests cannot run on Apple silicon

`platform/sqlite_ffi` refuses darwin/aarch64 (see the 2026-10-13 entry in `../gotchas.md`), so on a Mac every
SQLite-backed test fails with `SqliteLibraryError load-failed`, and Deno 2.7 additionally reports
`Uint8Array<ArrayBufferLike>` type errors in `platform/sqlite_ffi/database.ts` that 2.9.5 does not. Run CI in
Linux: `work-scripts/bash/deno-linux.sh 'deno task ci'` (denoland/deno:2.9.5 + `libsqlite3-0`; node_modules in
a Docker volume so the Mac's is untouched). Checked on this branch: `fmt:check`, `lint`, `check` and
`test:prop` pass that way, and `test` passes except 16 tests that fail identically on clean `main` in the same
container: the Postgres-backed tenant tests (nothing listens on `127.0.0.1:5432` unless `make up` runs the
compose `db`) and two FFI-05 statement-deadline tests (`document_reader_test.ts`, `storage_test.ts`) whose
workloads finish before the deadline there (_unverified_ why).
