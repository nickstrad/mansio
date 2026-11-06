# FFI-06 continuation (fresh Sol unit): finish, validate, commit, push

You are a freshly spawned builder (Codex Sol, `gpt-6.1-sol`) continuing plan item FFI-06 in the mansio repo.
The previous Sol unit (`sol-ffi-06`) implemented the item in this worktree, /root/Software/mansio-work/ffi06
(branch `item/ffi-06-evidence`, still at `main` `f2bf1c9`, with UNCOMMITTED changes), ran the evidence, and
exited without committing because nine unit tests fail in files outside its Owns. Your event actor id is
`sol-ffi-06-cont`. Work ONLY in this worktree. Another review (FFI-07, Astra) may be running suites: before
any suite, check `ps -eo pid,args | grep -E "^ *[0-9]+ deno (test|task)"` and wait if one is running
(`tests/unit/tenants_test.ts` shares the dev Postgres).

Environment: `export PATH="$HOME/.deno/bin:$PATH"` (deno 2.9.5). Use `deno task <name>`; `deno.json` is
frozen.

Read first: `agent-work/items/ffi-06-evidence/brief.md` (the item brief),
`agent-work/items/ffi-06-evidence/report.md` and `STATE*.md` if present (the previous unit's evidence and
state), `agent-work/items/ffi-06-evidence/proposed-catalog-compat.patch`, `git status` and `git diff` (the
uncommitted implementation), `AGENTS.md`, `agent-work/event-log.md` (log `task_started` with
`work-scripts/bash/log-event.sh --actor sol-ffi-06-cont --branch item/ffi-06-evidence --type
task_started --summary "FFI-06 continuation …"`),
`docs/knowledge/gotchas.md` (FFI entries).

## Coordinator decisions (logged 2026-10-14)

1. **Owns extended**, additive only: `docs.md` (one bullet in the named-analysis-queries list),
   `docs/observability.md` (the `path` column row and the new query's section),
   `tests/unit/analysis_catalog_test.ts` (the catalog-name assertion gains the new name; the `sql_events`
   fixture INSERT gets an explicit column list so the 15th nullable column does not break positional values).
   Inspect the proposed patch against the actual failures before applying it; it is a proposal, not an order.
   Everything else in the original Owns stands; nothing else is owned.
2. **Strict node parity** (`perf/ffi_read_cost.ts`: binding blocking p50 must be ≤ `node:sqlite` p50 in the
   same run) FAILED in both previous runs (8.96 vs 8.28 µs at load ~2; 7.60 vs 6.84 µs at load ~1.4). Do not
   relax the gate. Do: (a) check the harness is fair — both sides reuse one prepared statement, bind the same
   way, decode the same columns, same row count, interleaved batches, warm-up excluded; the binding side
   should use the sync `Statement` path FFI-01's `sqlite_ffi_perf_test.ts` uses (which met 1.3× + 1 µs) and
   not add per-call allocations the node side lacks; (b) rerun on a quiet host (1-min load < 1.5; wait for it;
   print the load) at least 3 times, interleaved; (c) report every run's numbers honestly in the commit body
   and `perf/README.md`. If parity still fails, the item still commits and pushes with the gate recorded as
   FAILING in the commit body and report; the reviewer rules against the plan, and the coordinator takes a
   plan amendment to the human (FFI-04 measured end-to-end point reads 10 % faster than `node:sqlite`, so a
   per-statement micro-cost may be acceptable). Do not hide or reword the failure.

## Finish

1. Apply (or re-derive) the companion changes in the three extended files; run the nine failing tests; then
   the full gates: `deno task lint`, `check`, `fmt:check`, `test`, `test:prop`, `integration`, and
   `deno task ci` once everything else is green. Known timing flakes under load, not blockers:
   `tests/unit/ipc_test.ts:663`, `number_dialects_test.ts:243`, `sqlite_ffi_cancel_test.ts:76`.
2. Re-run `perf/ffi_read_cost.ts` per decision 2. Keep the other harness evidence from the previous unit
   (interrupt p99 6.1 ms / ≥ 92 ticks; cap 32/32 within 71 ms; integration 20 × 504 with point-read p99 22 ms;
   `logs:query` showing `path: "nonblocking"` and 20 deadline hits) and cite it; rerun any of it only if its
   inputs changed.
3. Commit: ONE implementation commit
   `perf: port the FFI harnesses, add the deadline integration scenario and the sql
   path column` including
   the three companion files (body: harness numbers with loads, PoC comparison, the parity result stated
   plainly, the golden-line change in `tests/unit/events_test.ts`, the
   `ALTER TABLE … ADD COLUMN IF
   NOT EXISTS` note). Then `agent-work: add FFI-06 brief and evidence`
   (`git add -f agent-work/items/ffi-06-evidence/` — brief, report, evidence logs; not multi-MB raw dumps).
   Knowledge (`docs/knowledge/gotchas.md`) only if you own it — you do not; put durable lessons in the report
   for the coordinator. Push `item/ffi-06-evidence`. Never merge or touch `main`, FFI-07's files, or other
   worktrees.
4. Log `task_completed` with validation and SHAs. Final output: commit SHAs, per-task pass/fail counts, the
   parity runs with loads, the `logs:query` evidence line, what the reviewer (Claude Opus 5.5) should look at
   first.
