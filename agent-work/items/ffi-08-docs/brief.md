# FFI-08 — Docs, C4 diagrams, ADR and knowledge (builder brief)

Status: dispatched · Branch: `item/ffi-08-docs` · Worktree: `/root/Software/mansio-work/ffi08` · Base: `main` at
`aa26b27` (after FFI-06 and FFI-07 landed) · Builder: Claude Haiku 5.5 (actor `haiku-ffi-08`) · Reviewer: Astra
(`gpt-6-astra`), fresh unit · Landing: PASS **and explicit approval by Nick** (Haiku-tier item; Nick's ruling of
2026-10-13 ~23:03 CDT restates it) · Date: 2026-10-14.

**Docs-only.** CLAUDE.md rule: no edits under `core/`, `platform/`, `sdk/`, `app/`, `tests/`, `scripts/`, `perf/`,
`deno.json`, even if a fix looks trivial. If a doc claim can only be made true by a code change, write the claim as
it stands today and hand the code change to the coordinator in your report.

Environment: `export PATH="$HOME/.deno/bin:$PATH"` (deno 2.9.5).

Read first: `AGENTS.md` (unit-of-work rules; "Diagram maintenance"), `docs/architecture/AGENTS.md`,
`agent-work/event-log.md` (log `task_started` with `work-scripts/bash/log-event.sh --actor haiku-ffi-08 --branch
item/ffi-08-docs --type task_started --summary "FFI-08 …"`), `docs/knowledge/index.md`, the plan (see step 0), the
landed merges `git log --oneline --merges f2bf1c9^..HEAD` and their diffs for FFI-01..FFI-07, the per-item C4
follow-ups already on `main` (`git log --oneline --grep "C4 diagrams for FFI"`), `platform/sqlite_ffi/README.md`,
`perf/README.md` (FFI-06 numbers), `docs/testing.md` (FFI-07 rows), `docs/adr/README.md` and the existing ADRs.

## Step 0 — bring the plan onto your branch

The plan `docs/plans/ffi-implementation.md` lives on branch `plan/ffi-implementation`, not on `main`. First run
`git merge --no-edit origin/plan/ffi-implementation` in your worktree (docs-only, should not conflict; if it does,
stop and log a `blocker`). Your landing will carry the plan onto `main`.

## Owns

`docs/architecture/c4-component-server.*`, `docs/architecture/c4-container-platform.*` and their
`docs/architecture/diagrams/*.py` sources (verify against `platform/`: every box, edge and note must match the landed
code after FFI-07; the per-item follow-ups did most of this — your job is to re-verify, add what FFI-06/FFI-07
changed (event-log `path` column, the deadline catalog query, the perf harnesses as a dev-tool note if the diagram
convention has a place for it), and move the audit stamp to `aa26b27`), `docs/architecture/README.md`, `docs.md`
and `README.md` (usage: the 504 statement deadline, the three `sqlite.*` options and their defaults, the
`SQLITE_LIBRARY` env var, the `deadline-hits-per-handler-hour` analysis query), `docs/adr/0008-statement-execution-via-ffi.md`
(new — numbers 0006 and 0007 are taken; the plan said 0006: the decision, the §1.2 two-copies rule, the §1.3
threading model, the measured numbers from FFI-04/05/06 with loads, the amended parity gate and why, risks 15–23 as
consequences), `docs/adr/README.md` (index row), `docs/knowledge/decisions.md` and `docs/knowledge/gotchas.md`
(check: two SQLite copies; two `libsqlite3` on vm-1; `getCString` truncation; the DQS/foreign-key defaults; the
`-0`/NaN binding facts from FFI-07; the Muse headless approval lesson is VM-level and belongs in `kb`, not here —
leave it out), `docs/knowledge/index.md` (entries for the above), `docs/plans/ffi/README.md` (status line: execution
layer landed, lanes and placement still proposed), `docs/plans/ffi-implementation.md` (status → landed for
FFI-01..FFI-08 with merge SHAs; the batch table rows become "landed" lines; keep the history), `docs/testing.md`
only if a row is wrong, `agent-work/items/ffi-08-docs/*` (`git add -f`).

## Acceptance evidence

`python3 docs/architecture/diagrams/render.py --check` passes; both formats of each touched diagram updated; the ADR
cites the FFI-06 numbers with loads; `deno fmt --check docs/ README.md docs.md` green; `deno task fmt:check` green
repo-wide; the reviewer re-verifies the diagrams against `platform/`. `grep -rn "node:sqlite" docs/ README.md
docs.md` shows only historical or contrast wording, never a claim that the server uses it.

## Protocol

One implementation commit `docs: document the FFI statement execution layer (ADR 0008, C4, usage, knowledge)`.
Separate `agent-work: add FFI-08 brief` commit. Push `item/ffi-08-docs`; do not merge or touch `main`. Log
`task_completed`. Final output: commit SHA(s), the check outputs, the list of docs changed and why, any code-side
hand-offs (for example the stale `statementDeadlineMs` description in `platform/config.ts` that still says "once
FFI-05 lands" — you may NOT edit it; name it), learning assessment.

## Fixup (haiku-ffi-08-fixup)

Astra review at `caf1efe` returned NITS (review-astra.md, on `origin/review/ffi-08-astra`). Fixed in `bba6f7f`:
the `docs/knowledge/gotchas.md` tenant boundary entry is marked superseded (2026-10-14) with the current FFI and
in-process document query boundary; the strict-parity overhead is approximately 6.0 to 6.6 % in ADR 0008 and
`decisions.md`; ADR 0008 separates the same-run parity ratio from the absolute gate budgets, with load recorded.
`perf/README.md` still carries the old percentage and is a coordinator hand-off. Delta review pending; no landing.
