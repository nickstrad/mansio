# Luna experiments and side quests

Purpose: this is the log for experimenting with Luna (`gpt-6-luna`, always run on **high effort**) — the
repo's fast/cheap tier since the model remap (see `AGENTS.md` "Model duality") — on real items from
[`docs/plans/effect-port-and-observability.md`](plans/effect-port-and-observability.md), plus a running
side-quest board for small investigative tasks that produce useful findings while the longer plan items are in
flight.

The evaluation question is twofold:

1. **Can Luna-on-high do what Haiku 4.5 could not?** The Haiku experiments (see
   [`docs/knowledge/haiku-experiment-record.md`](knowledge/haiku-experiment-record.md)) returned a formal
   verdict: 0 of 3 items indistinguishable from Sonnet — Haiku 4.5 nails shape fast but drops safety-critical
   details, violates scope, and rates itself 5/5 regardless. Luna runs a tier up (Sonnet-class model, high
   effort) at the fast/cheap price point. Each experiment item below is judged against the same bar Haiku
   faced: is the output indistinguishable from what Sonnet would have produced?
2. **Do fast/cheap iterations buy insights?** Side quests (lx-01…) are insight-generating investigations, not
   production code — seed sweeps, query benchmarks, consistency audits, load-shape exploration.

This file is docs-only. Nothing here touches plan item files, the `item/NN-slug` worktrees under
`/root/Software/mansio-work/`, or `agent-work/` (the coordinator's own scratch space).

---

## Experiment items

**All experiment work is executed by Luna (`codex exec --model gpt-6-luna`, high effort).** Nothing here is
done by Sonnet, Opus, Sol, or Astra — the "Plan baseline" column below shows the model the plan assigns to
each item, purely as the comparison baseline for judging Luna's output. The Haiku experiment restrictions
apply unchanged: Luna **never merges to `main`** without an Opus review (same acceptance criteria the plan
item defines) **and** explicit human approval. A PASS verdict alone does not authorize a merge.

Picked from the remaining Sonnet/sol-tier items (Opus/astra-tier items are out of scope for the fast tier by
definition). Selection criteria: small, bounded, mechanical or editorially bounded, minimal cross-file
dependencies, verifiable acceptance criteria.

| ID      | Item | Title                          | Plan baseline | Status   | Rationale                                                                                                                                                                                                                                                             |
| ------- | ---- | ------------------------------ | ------------- | -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| luna-23 | 23   | Analysis catalog, logs CLI     | Sonnet / sol  | proposed | A fixed list of named SQL queries over the schema frozen in item 21, plus a thin logs CLI. The query list is enumerated by the plan — the work is transcribing spec to SQL and wiring the CLI. Bounded file set; every query is runnable, so acceptance is checkable. |
| luna-28 | 28   | `demos/`: runnable demos       | Sonnet / sol  | proposed | Three self-contained demos over fixed SDK interfaces. Bounded and mechanical; acceptance is running each demo. Tests whether Luna produces working end-to-end code without scope creep — the exact failure mode that sank haiku-14.                                   |
| luna-29 | 29   | `docs.md`: platform user guide | Sonnet / sol  | proposed | The plan itself flags this one as a judgment call — editorial choices about what a new developer needs. If Luna matches Sonnet here, the fast tier covers judgment-heavy docs work too, not just mechanical transcription.                                            |

Rules for every experiment item:

- Luna works on a branch named `luna/NN-slug` (e.g. `luna/23-analysis-catalog`), in its own git worktree —
  never inside an `item/NN-slug` worktree and never on `main`.
- Luna runs on high effort, always. A run without high effort does not count as an experiment data point.
- When an experiment item finishes (merged, rejected, or abandoned), the branch stays until the human says it
  can be deleted.
- Each experiment item writes its findings to `docs/luna-experiments/findings/luna-NN.md` (see
  [Findings](#findings) below) and fills in the [scorecard](#luna-quality-scorecard) template.

---

## Side quests

Small, read-only or scratch-only investigations — each sized to finish in one short session. A side quest must
**never modify plan item files** (anything under `core/`, `platform/`, `sdk/`, `app/`, `tests/`, `scripts/`,
`perf/`, or `deno.json`). Output is a written finding, not a code change to the product tree.

| ID    | Question                                                                                                                                                                   | Why it matters                                                                                                                                                                                             | Done-criteria                                                                                                                                                                                                                                           |
| ----- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| lx-01 | Run the property suites (`tests/property/`, items 12/13) across a sweep of `PROP_SEED` values (≥200 seeds). Which invariants fail on which seeds?                          | A property suite that only passes on lucky seeds erodes trust in every item that leans on it. A seed sweep finds flaky invariants cheaply, before they bite a builder mid-item.                            | A table of invariant → failing seeds (or "clean across N seeds"), with the worst offender's minimal reproduction noted. Findings at `findings/lx-01.md`. Scratch runner saved to `work-scripts/deno/` and indexed (read `work-scripts/INDEX.md` first). |
| lx-02 | Benchmark the DuckDB analysis queries (`scripts/duckdb/` plus item 23's catalog list) at 1M and 12M ingested rows. Which queries are slow, and why?                        | Item 23's queries will run against production-scale event logs. Knowing the slow ones now — missing predicate pushdown, full scans, bad join order — guides query/index design before the catalog freezes. | A timing table (query × row count → wall time) with the top-3 slowest queries diagnosed to a likely cause. Findings at `findings/lx-02.md`. Benchmark script saved to `work-scripts/deno/` and indexed.                                                 |
| lx-03 | Audit docs-vs-code consistency: the README "Configuration" section vs the item-08 options table in `platform/config.ts` vs actual env-var reads in code. List every drift. | Items 27 and 29 will regenerate docs against a settled codebase; a drift list now tells those items exactly what to fix instead of rediscovering it.                                                       | A drift table (option → README says / table says / code says, with `file:line` references), or a clean bill with the check method recorded. Findings at `findings/lx-03.md`.                                                                            |
| lx-04 | Explore k6 load shapes against a local server: ramp-up, spike, sustained. At what shape does the server first degrade (p99 latency, error rate)?                           | Gives item 25's e2e perf work a known breaking envelope to test against, instead of guessing thresholds. Not production scenarios — just find the knee.                                                    | A breaking-point table (shape → first-degrading metric and value), with the exact k6 flags used so item 25 can reproduce. Findings at `findings/lx-04.md`. Scratch scripts saved to `work-scripts/` and indexed.                                        |

| ID    | Status   |
| ----- | -------- |
| lx-01 | proposed |
| lx-02 | proposed |
| lx-03 | proposed |
| lx-04 | proposed |

The main agent (not Luna) appends new side quests here as they come up; Luna subagents claim one, investigate,
and report back — they do not add new quests to this board themselves.

#### Script convention for Luna agents

If your quest or item needs a script (benchmark, probe, measurement): write it in **Deno** for larger work,
**bash** if it is bash-oriented — and save it in `work-scripts/` (`deno/` or `bash/`), collocated with all
other work scripts, not in a luna-only folder. Then append a row to `work-scripts/INDEX.md`. Read the index
first: the building block you need may already exist.

---

## Findings

Convention: every completed quest or experiment item writes exactly one findings file at
`docs/luna-experiments/findings/<quest-id>.md` (e.g. `findings/lx-01.md`, `findings/luna-23.md`). That path is
unique per quest/item — a Luna agent only ever writes its own findings file, never a shared file, so
concurrent Luna runs can't collide on an edit.

- Luna agents: investigate, then write (only) `docs/luna-experiments/findings/<id>.md`. Do not edit this
  board, `MEMORY.md`, or any other quest's findings file.
- The main agent: after a quest/item finishes, updates its `Status` in the tables above to link the findings
  file, and commits both the status change and the new findings file together.

---

## Luna quality scorecard

Fill in after each experiment item (not side quests — those are judged on the finding's usefulness, not code
quality). Same shape as the Haiku scorecard, so the two fast-tier evaluations stay comparable.

```
Item: luna-NN (plan item NN — <title>)
Branch: luna/NN-slug (<sha>)
Date: YYYY-MM-DD

Correctness:         [ ] matches acceptance criteria   [ ] partial   [ ] fails
Test discipline:     [ ] wrote/extended required tests  [ ] missing coverage
Owns-scope adherence:[ ] touched only its Owns set
                     [ ] strayed into: <files>
Review verdict:      [ ] PASS   [ ] NITS   [ ] FAIL   (Opus review)
Merged to main:      [ ] yes (human-approved)   [ ] no

Luna-to-Sonnet conversion verdict:
[ ] Luna output is indistinguishable from what Sonnet would have produced --
    this item class can move to Luna going forward.
[ ] Luna output is usable but needed meaningful correction -- Luna viable for
    this item class only with Opus review in the loop.
[ ] Luna output not acceptable for this item class -- keep on Sonnet/Opus.

Reasoning (one line): <...>

Notes:
- <verified facts, measurements, and caveats>
```
