# FFI-07 — Property tests for the binding and the deadline (builder brief)

Status: dispatched · Branch: `item/ffi-07-props` · Worktree: `/root/Software/mansio-work/ffi07` · Base: `main`
at `f2bf1c9` (FFI-05 merge) · Builder: Claude Sonnet (actor `claude-sonnet-ffi-07`) · Reviewer: Astra
(`gpt-6-astra`), fresh unit · Date: 2026-10-14 · Runs CONCURRENT with FFI-06 (`item/ffi-06-evidence`, disjoint
Owns).

Plan: `git show origin/plan/ffi-implementation:docs/plans/ffi-implementation.md` — read §1.2, §2, §3 and
FFI-07 in §4. Also read `AGENTS.md`, `CLAUDE.md`, `agent-work/event-log.md` (log `task_started` first with
`work-scripts/bash/log-event.sh --actor claude-sonnet-ffi-07 --branch item/ffi-07-props --type task_started --summary
"FFI-07 …"`),
`docs/knowledge/index.md`, `docs/knowledge/gotchas.md` (FFI entries, "seed sweep", "Exit-139"),
`docs/testing.md`, `platform/sqlite_ffi/README.md`, `tests/property/sqlite_oracle_test.ts` and
`tests/property/arbitraries.ts` (generators to reuse), `tests/property/json_number_test.ts`,
`tests/unit/nul_contract_test.ts`, `tests/unit/sqlite_ffi_crud_test.ts` and `sqlite_ffi_cancel_test.ts` (what
the unit tests already pin), `tests/unit/ffi_classify_test.ts` (FFI-05 landed the classifier property there).

## Why

FFI-01..05 are unit-tested; this item adds the generator-driven invariants the plan relies on for crash-safety
and contract claims: value round trips through the binding against a `node:sqlite` oracle, interrupt totality,
and the classifier property if FFI-05's unit test does not already cover it.

## Owns

`tests/property/ffi_roundtrip_test.ts` (new), `tests/property/ffi_interrupt_test.ts` (new), `docs/testing.md`
(additive rows), `tests/unit/boundaries_test.ts` (ONLY the `node:sqlite` test allow-list: add your round-trip
file by exact path with a one-line justification; no other change), `agent-work/items/ffi-07-props/*`
(`git add -f`). Nothing else: not `deno.json` (frozen — the `test:prop` task globs `tests/property/`; confirm
your files are picked up), not `platform/**`, not FFI-06's files (`perf/*`, `tests/integration.ts`,
`core/events.ts`, `core/eventlog/*`, `platform/eventlog/*`, `platform/analysis/*`, `work-scripts/INDEX.md`),
not any other worktree.

## The one rule (plan §1.2)

Never open the same file through `node:sqlite` and the binding in one process. The oracle runs on its own
`:memory:` database; the binding runs on its own temp file. The boundary test
(`tests/unit/boundaries_test.ts`) allow-lists which test files may import `node:sqlite` by exact path; the
coordinator has pre-extended your Owns to that one list so you can add `tests/property/ffi_roundtrip_test.ts`
to it (exact path, one-line justification, the same style as the four existing entries). Touch nothing else in
that file.

## Work

1. Round trip (`ffi_roundtrip_test.ts`): for generated values of every column kind — text with U+0000 and lone
   surrogates, integers across the full int64 range (`bigint` under `readBigInts`), doubles including `-0`,
   `NaN` rejected as a typed error, blobs including empty — `bind → step → column` through the binding equals
   what `node:sqlite` returns for the same bind on a SEPARATE in-memory database — except U+0000 text, where
   the binding returns the full string and `node:sqlite` the truncated one; pin that difference explicitly
   (ADR 0005). Use `tests/property/arbitraries.ts` generators where they fit; fixed seed like the other
   property files (`seed=20261010` style) with the run count in the test name.
2. Interrupt totality (`ffi_interrupt_test.ts`): for random statements (long recursive CTEs, large scans over
   a seeded table, trivial point reads) and random interrupt delays 0–50 ms, a nonblocking step (`allAsync`
   with `timeoutMs` and/or `AbortSignal`) always terminates with either rows or `SqliteCancelled`, never hangs
   (bound each iteration with a generous outer wall-clock guard that FAILS the test if hit), and the
   connection answers the next statement; 200 iterations. Mind the gotchas entry from FFI-05:
   `sqlite3_interrupt` does not cut a `busy_timeout` wait — do not generate statements that block on a lock,
   or treat `SQLITE_BUSY` as a valid terminal outcome and say so.
3. Classifier property: FFI-05 landed `tests/unit/ffi_classify_test.ts` with a 2 000-plan property. Read it;
   if it already asserts `bounded ⇔ (_id eq filter present ∧ no aggregate)` over generated `SelectPlan`s, do
   not duplicate; add a row to `docs/testing.md` pointing at it. Otherwise add the property to
   `ffi_roundtrip_test.ts`'s sibling file set as `tests/property/ffi_classify_props.ts` (then that file is in
   your Owns too).
4. `docs/testing.md`: additive rows for the new suites (what they pin, seed, run count, how to sweep).

## Acceptance evidence

- `deno task test:prop` green with the new suites; `deno task lint`, `check`, `test` green; `deno fmt --check`
  on owned files. A 200-seed sweep with `work-scripts/bash/property-seed-sweep.sh` on your two files (read the
  script and gotchas "seed sweep" for usage) finds no failure other than known exit-139 reruns; record the
  sweep summary in the commit body. Known timing flakes under load, not blockers:
  `tests/unit/ipc_test.ts:663`, `number_dialects_test.ts:243`, `sqlite_ffi_cancel_test.ts:76`. Never run
  suites concurrently with another suite (`tests/unit/tenants_test.ts` shares the dev Postgres) — FFI-06 is
  building at the same time, so check `pgrep -af "deno test"` before starting a suite and wait if one is
  running; your seed sweep should target only your own files, not the whole suite.

## Protocol

One implementation commit `tests: add FFI round-trip and interrupt-totality properties` (body: generators,
seeds, sweep result, the pinned NUL difference). Separate `agent-work: add FFI-07 brief` commit
(`git add -f
agent-work/items/ffi-07-props/`). Knowledge in its own commit if durable. Push
`item/ffi-07-props` to origin; do not merge or touch `main`; do not edit FFI-06's files or worktree. Log
`task_completed` naming validation and SHAs. Final output: commit SHAs, per-task pass/fail counts, sweep
summary, any blocker (boundary allow-list), learning assessment, what the reviewer should look at first.

## Repair 1 (2026-10-14, `claude-sonnet-ffi-07-repair1`)

Astra FAIL at `3a3bcb2`: P2 recovery probe outside the guard, P2 cleanup swallowed close errors and grace
expiry, P3 doc overstated seed determinism, P3 brief not formatted. Fixed in the
`tests: bound the whole interrupt iteration and
fail on close errors` commit (interrupt test,
`docs/testing.md`) and this brief's `deno fmt`. Fault reproductions (not committed) now fail: close exit 1,
probe fails at the 15 s guard, native-disabled interrupt reports and SIGKILLs (137, was SIGSEGV 139), a
synchronous stall is killed by the watchdog thread after 25 s. `test:prop` 84 passed, `test` 686 passed,
50-seed interrupt sweep all PASS.

## Arbitration fixup (2026-10-14, `claude-sonnet-ffi-07-fixup2`)

Muse Wonder Full arbitration (NITS) required arming the watchdog across fixture setup and cleanup. Done in
`tests: arm the interrupt watchdog across fixture setup and cleanup`, confined to
`tests/property/ffi_interrupt_test.ts`: a beat before `withFixture` (covers `Database.open`), beats inside
`withFixture` before the seed insert, the baseline reads, `fn` and cleanup, and the header's layer-3 sentence
amended. The review-2 `fixture-stall.ts` injection now ends with the watchdog FATAL and exit 137 in 31 s
(before: external timeout, 124). Alone, lint, check, fmt pass; `test:prop` 84 passed; 50-seed interrupt sweep
50 PASS with no FATAL/HANG.
