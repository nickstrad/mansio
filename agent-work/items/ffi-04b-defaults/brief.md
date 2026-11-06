# FFI-04b — Binding connection defaults match `node:sqlite`

Builder: Codex Astra (`gpt-6-astra`), actor `astra-ffi-04b`. Assigned branch `item/ffi-04b-defaults`, worktree
`/root/Software/mansio-work/ffi04b`, base `51a5283`. Independent review is assigned to a fresh Claude Opus 5.5
unit by the coordinator.

## Authorization and ownership

Implement and push this item only; do not merge or touch `main` or another worktree. Permission profile:
unrestricted filesystem and network, no approval prompts. Run suites sequentially because tenant tests share
the dev Postgres. `deno.json` is frozen.

Owns:

- `platform/sqlite_ffi/library.ts`: fixed DQS signature for variadic `sqlite3_db_config`.
- `platform/sqlite_ffi/database.ts`: checked DML/DDL DQS disable and foreign-key pragma at every open.
- `platform/sqlite_ffi/README.md`: connection-default paragraph.
- `tests/unit/sqlite_ffi_crud_test.ts`: additive tests.
- `docs/knowledge/gotchas.md`: supersede FFI-04's connection-default mismatch.
- `agent-work/items/ffi-04b-defaults/*`: brief and acceptance evidence, force-added.

Read callers without edits: Effect `openDatabase`, storage, document reader, test inspection helper. The
defaults belong in `Database.open`, before SQL and busy timeout, including read-only opens. Capture native
errors before cleanup; cleanup must not replace the original throw. Preserve the existing busy/close
discipline.

## Acceptance

- Unknown quoted columns fail with typed `SqliteError` and native text containing `no such column`.
- `PRAGMA foreign_keys` is 1, including read-only connections.
- Orphan inserts fail with extended code 787 (`SQLITE_CONSTRAINT_FOREIGNKEY`).
- DQS is disabled for DML and DDL; valid quoted identifiers and single-quoted strings still work.
- Existing point-read parity remains green; defaults run once per open.
- `deno task lint`, `check`, `test`, `test:prop`, `integration`; touched-file `deno fmt --check`.
- Known baseline: repository-wide fmt fails on unowned `docs/knowledge/decisions.md`.
- Known timing flakes: IPC at line 663, number dialects at line 243, FFI cancellation at line 76.

## Source verification and interpretation

`/usr/include/sqlite3.h` is absent. Verified DQS_DML = 1013, DQS_DDL = 1014 and the `(int, int*)` arguments
against [SQLite's connection configuration reference](https://www.sqlite.org/c3ref/c_dbconfig_defensive.html)
on 2026-10-13. The implementation uses the requested `(pointer, i32, i32, pointer) -> i32` shape. The
reference also documents ENABLE_FKEY; the original brief's assertion that foreign keys cannot be enabled
through `db_config` is incorrect. The implementation retains the explicitly requested pragma.

The binding intentionally exposes native text in `sqliteMessage`, with an empty inherited `message`. Tests
assert `sqliteMessage`; changing the error taxonomy is outside this item.

## Delivery

One implementation commit, subject `platform: match node:sqlite connection defaults in the FFI binding`.
Separate brief/evidence commit, subject `agent-work: add FFI-04b brief`. Push the assigned branch, then report
SHAs, validation counts, open paths, learning assessment and reviewer focus. Retain this worktree for
coordinator inspection and subsequent cleanup.

## Fixup after Opus review

Opus review (NITS, `review/ffi-04b-opus`) on `f01425f`; fixup by `claude-sonnet-ffi-04b-fixup`.

- NIT 1: `loadLibrary` now refuses darwin/aarch64 (Apple arm64 variadic convention makes the fixed
  `sqlite3_db_config` signature invalid) with `SqliteLibraryError` `load-failed`; comment, `gotchas.md` and
  README updated. Unit test covers the pure `unsupportedPlatform(os, arch)` predicate; the refusal itself
  cannot run on this Linux host without faking `Deno.build`, which was not done.
- NIT 2: every failure in the open-time defaults block reports `op: "open"`; a non-OK `sqlite3_db_config`
  message comes from `sqlite3_errstr(rc)`. Untested (reachable only on OOM, no mocking).
- Validation: lint, check, test (675 passed, 0 failed, 1 ignored), test:prop (79 passed), touched-file fmt all
  pass.
