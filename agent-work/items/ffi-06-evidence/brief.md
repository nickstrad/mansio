# FFI-06 builder brief

Date: 2026-10-14. Actor: sol-ffi-06. Builder: Codex Sol. Branch: item/ffi-06-evidence. Worktree:
/root/Software/mansio-work/ffi06. Base: main f2bf1c9 (FFI-05). Concurrent FFI-07 owns disjoint property-test
files. Plan: origin/plan/ffi-implementation:docs/plans/ffi-implementation.md sections 2–5.

Port the read-cost and interrupt PoCs from origin/research/fable-lang-switch to the production
Database/Statement API, preserving JSON metric keys and adding load average. Add the 32-reader deadline
harness through Statement.allAsync with timeoutMs 50. Add a real HTTP two-tenant deadline scenario, point p99
below 50 ms (load scaling per risk 11), HTTP 504 and event-log path nonblocking evidence. Persist
KIND_KEYS.sql.path; update only the affected events golden line. Add a nullable sql_events.path projection and
idempotent ALTER on ingest, plus deadline hits per handler per hour in the analysis catalog. Mark the PoCs
superseded in the script index.

Owns: perf/ffi_read_cost.ts, perf/ffi_interrupt.ts, perf/ffi_cap.ts, perf/README.md (additive),
tests/integration.ts (additive), platform/eventlog/tables.ts, core/eventlog/ddl.ts,
platform/analysis/catalog.ts (one additive query), work-scripts/INDEX.md, core/events.ts (KIND_KEYS.sql.path),
tests/unit/events_test.ts (only affected golden), agent-work/items/ffi-06-evidence/*.

Not owned: binding, deno.json, FFI-07 files, other worktrees. No merge or main edits. Any ownership extension
requires explicit coordinator authorization.

Acceptance: blocking p50 <= 10.2 us and <= node in same run; nonblocking p50 <= 100 us, p99 <= 3000 us; 50
interrupt iterations, p99 <= 20 ms, >= 90 loop ticks per second; 32 deadline cancellations <= 100 ms each.
lint/check/test/test:prop/fmt:check/integration green, logs:ingest and logs:query evidence. Check pgrep for
deno test before each suite and wait for other suites to finish. Known timing flakes are documented, never
hide failures.

One implementation commit: perf: port the FFI harnesses, add the deadline integration scenario and the sql
path column Body contains load/numbers, golden-line change, ALTER note and logs evidence. Separate agent-work:
add FFI-06 brief commit. Push item/ffi-06-evidence only. Log task_completed with validation and SHAs; report
failures honestly. Reviewer is a fresh Claude Opus 5.5 unit, dispatched by the coordinator. Keep the worktree
until coordinator inspection.

## Fixup after Opus review

Actor `claude-sonnet-ffi-06-fixup`, commit `11f4e04` on `item/ffi-06-evidence`. The Opus review (300470d) failed
only the strict same-run `node:sqlite` parity gate (plan amendment pending with the human; thresholds and the
harness exit status untouched) and listed eight NITS, all addressed: (1) value assertions for the deadline catalog
query; (2) `rc` dropped from `perf/ffi_interrupt.ts` output; (3) README evidence labels/order; (4) INDEX.md
branch name and PoC author/date; (5) hour-bucket-safe integration assertion; (6) flat 50 ms p99; (7) module
header plus a `TRACE_SQL=false` pass; (8) upgrade note in docs/observability.md. Validation: `deno task ci`
exit 0 (687 unit, 79 property), `deno task integration` exit 0 (157 reads, p99 18.67 ms at load 2.19; second
pass 3/3 deadline events without TRACE_SQL), `perf/ffi_interrupt.ts` exit 0 on the third run (the first two
failed only the load-fragile tick gate at min 88 and 50 ticks while other agents were running).

## Gate fixup and final NIT

Actor `claude-sonnet-ffi-06-fixup2`. (1) `perf/ffi_read_cost.ts` and `perf/README.md` now encode the amended
parity gate, blocking p50 <= 1.3 x node:sqlite p50 + 1 us (Nick, 2026-10-13; plan risk 19); the harness exits 0 on
a quiet host (7.72 vs node 7.25 us, bound 10.43, load 1.25). (2) Astra's re-review NIT closed in
`tests/unit/analysis_catalog_test.ts`: new fixtures vary trace and time independently (a different-trace event
inside a same-app request window; events 1 ms and 2 ms before a request starts). Scratch mutations of the catalog
query confirm the test fails with the trace match removed and, separately, with the lower bound removed, and passes
unmutated. Validation: fmt --check, lint, check clean; full `deno task test` 687 passed, 0 failed.
