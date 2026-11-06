# FFI-06 continuation 2 (fresh Sol unit): apply the fixture row, run the gates, commit, push

You are a freshly spawned builder (Codex Sol, `gpt-6.1-sol`) finishing plan item FFI-06 in the mansio repo. Two
previous Sol units implemented and measured the item in this worktree, /root/Software/mansio-work/ffi06 (branch
`item/ffi-06-evidence`, still at `main` `f2bf1c9`, with UNCOMMITTED changes), and stopped at ownership
questions. Your event actor id is `sol-ffi-06-cont2`. Work ONLY in this worktree. Before any suite, check
`ps -eo pid,args | grep -E "^ *[0-9]+ deno (test|task)"` and wait if one is running (another repair unit,
FFI-07, may be testing; `tests/unit/tenants_test.ts` shares the dev Postgres).

Environment: `export PATH="$HOME/.deno/bin:$PATH"` (deno 2.9.5). Use `deno task <name>`; `deno.json` is
frozen.

Read first: `agent-work/items/ffi-06-evidence/brief.md`, `brief-continuation.md`, `report.md` (the evidence so
far), `proposed-deadline-fixture.patch`, `git status`/`git diff`, `agent-work/event-log.md` (log
`task_started` with
`work-scripts/bash/log-event.sh --actor sol-ffi-06-cont2 --branch item/ffi-06-evidence --type task_started --summary
"FFI-06 continuation 2 …"`).

## Coordinator decisions (logged 2026-10-14, event `fable-ffi-coordinator`, this unit)

1. **Owns now include `tests/unit/analysis_catalog_test.ts` in full for any additive test or fixture change
   required by the `deadline-hits-per-handler-hour` query** — including the deadline fixture row in
   `proposed-deadline-fixture.patch`. Inspect the patch, apply it (or an equivalent), and move on. No further
   ownership questions on that file.
2. **Parity gate** (`perf/ffi_read_cost.ts`, binding blocking p50 ≤ `node:sqlite` p50 same run): your
   predecessor's three quiet-host runs (load 0.50–0.70) gave 7.35/7.64/7.57 µs vs 6.93/7.21/7.10 µs. That is a
   genuine ~6 % per-statement cost, inside FFI-01's accepted 1.3× threshold but outside this item's gate. **Do
   not rerun it and do not relax it.** Record the three runs with loads in the commit body and
   `perf/README.md`, state plainly that the strict gate fails and the absolute budgets pass, and leave the
   harness's exit status as it is. The coordinator is taking a plan amendment to the human; the reviewer will
   rule against the plan as written.

## Finish

1. Apply the fixture change; run the formerly failing test; then `deno task lint`, `check`, `fmt:check`,
   `test`, `test:prop`, `integration`, then `deno task ci`. Known timing flakes under load, not blockers:
   `tests/unit/ipc_test.ts:663`, `number_dialects_test.ts:243`, `sqlite_ffi_cancel_test.ts:76`.
2. Keep the preserved evidence (interrupt, cap, integration 20 × 504 with point-read p99 16.44 ms,
   `logs:query` `{"name":"winsByMark","path":"nonblocking","deadline_hits":20}`) and cite it; rerun only what
   your fixture change affects.
3. Commit: ONE implementation commit
   `perf: port the FFI harnesses, add the deadline integration scenario and the sql
   path column` (all
   product/test/doc files, including the three companion docs/test files and the fixture row; body: harness
   numbers with loads, PoC comparison, parity stated plainly as failing the strict gate, events golden-line
   change, `ALTER TABLE … ADD COLUMN IF NOT EXISTS` note, the catalog correlation rule). Then
   `agent-work: add FFI-06
   brief and evidence` (`git add -f agent-work/items/ffi-06-evidence/` — briefs,
   report, patches, small logs; omit multi-MB raw dumps). Push `item/ffi-06-evidence` to origin. Never merge
   or touch `main`, FFI-07's files, or other worktrees.
4. Log `task_completed` with validation and SHAs. Final output: commit SHAs, per-task pass/fail counts, the
   parity table, the `logs:query` line, what the reviewer (Claude Opus 5.5) should look at first.
