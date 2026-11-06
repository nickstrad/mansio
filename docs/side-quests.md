# Side quests

Side quests (`sq-NN`) are small, bounded investigations that produce a written finding while the plan items
(see [`docs/plans/effect-port-and-observability.md`](plans/effect-port-and-observability.md)) are in flight.
The protocol is in `AGENTS.md` ("Side-quest protocol"). This page is the quest board.

A side quest must **never modify plan item files** (anything under `core/`, `platform/`, `sdk/`, `app/`,
`tests/`, `scripts/`, `perf/`, or `deno.json`). Its output is a findings file, not a code change to the
product tree.

Findings live at `docs/haiku-experiments/findings/<quest-id>.md`. Scripts live in `work-scripts/` (see
`work-scripts/INDEX.md`). Side-quest branches are `haiku/sq-NN`.

The main agent appends new quests to the board. Subagents claim one quest, investigate, and report back; they
do not add quests themselves.

## Board

| ID    | Question                                                                                                                                                                                                                                     | Why it matters                                                                                                                                                                               | Done-criteria                                                                                                                                                                                                                                                             |
| ----- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| sq-01 | What batching/backpressure patterns do Effect-TS `Logger`/sink implementations use in practice, and which fit a `Queue.dropping` + `forkScoped` consumer like item 11 describes?                                                             | Item 11 (Opus, logger service) is one of the highest-risk items; backpressure or finalizer-ordering mistakes lose events or hang shutdown.                                                   | 2–3 concrete patterns with tradeoffs from Effect-TS docs/source/examples, and an explicit recommendation for this repo's constraints (bounded queue, batch by size-or-time, flush-on-shutdown). No code written.                                                          |
| sq-02 | What is the baseline JSONL append throughput (events/sec, MB/sec) on this droplet, using plain `Deno.FsFile` append-mode writes with no batching?                                                                                            | Item 11's acceptance criterion "10,000 events in under 2s" needs a raw floor to judge whether the bar is trivial or tight on this hardware.                                                  | Short scratch script (not committed to the product tree) measuring append throughput at a few record sizes, numbers recorded in the finding.                                                                                                                              |
| sq-03 | Which fast-check arbitrary-construction strategies (recursive, `fc.letrec`, sized, shrink-friendly) best fit generating `Node`/`Fields`/`Table` values ahead of item 12?                                                                     | Item 12 needs recursive schema nodes (arrays nesting to depth 2) that also shrink to minimal counterexamples. Picking the wrong strategy up front costs rework.                              | Writeup naming recommended fast-check APIs for this data shape, with an uncommitted sketch of depth-bounded recursive generation and a note on shrinking.                                                                                                                 |
| sq-04 | Which k6 output format (`--out json=`, `--out csv=`, experimental `--out influxdb=`, etc.) is most convenient for DuckDB ingestion via `read_json_auto`, and what gotchas affect item 25's `perf/ingest_k6.ts`?                              | Items 03, 14 and 25 depend on k6's raw output shape; finding out during item 25 that the JSON is not flat enough would be costly.                                                            | Comparison table of 2–3 formats against ease of DuckDB ingestion, a concrete recommendation, and an example of the JSON shape k6 emits per metric point.                                                                                                                  |
| sq-05 | Can Claude Code CLI work with AGENTS.md alone, or does it need CLAUDE.md? Validate empirically how the CLI discovers and respects each file, and their precedence when both exist.                                                           | The repo hand-maintains two agent files. Consolidating or syncing them depends on this answer; the wrong setup silently degrades all agent work.                                             | Empirical tests in scratch dirs (AGENTS.md-only, CLAUDE.md-only, both-present, parent-dir discovery); a concrete recommendation on the branch; findings at `findings/sq-05.md`. Not merged without Opus review and human approval.                                        |
| sq-06 | Force transaction conflicts against the Storage service (item 15) and observe how the platform handles them: concurrent conflicting writes, read-modify-write races, write-while-backup. Detected, retried, silent last-write-wins, or lost? | Item 15 is a serialized Storage queue; conflicts may be impossible by construction or surface as silent lost updates. The verdict decides whether a commutative-operation handler is needed. | Stress script saved to `work-scripts/deno/` and indexed; observed-behaviour table (scenario → outcome); verdict with evidence: "handler not necessary because X" or "handler needed". Findings at `findings/sq-06.md`. Not merged without Opus review and human approval. |
| sq-07 | Can the intermittent exit-139 (SIGSEGV) in mansio test runs be reproduced on demand, and where does it live: `duckdb.node` native code, the Deno FFI boundary, or the test harness?                                                          | The crash fires ~1/15 on `offsets_test.ts` and mid-suite in `deno task test`; quarantining the test would hide a wild-pointer crash in the runtime.                                          | Stress script saved to `work-scripts/bash/` and indexed; crash-rate measurement with dmesg attribution by child pid; corrected fault-location verdict. Findings at `findings/sq-07.md`.                                                                                   |

Quests `sq-08` through `sq-13` have findings files under `docs/haiku-experiments/findings/` but are not on
this board table; their status was not recorded here when the board was moved.

## Status

| ID    | Status                                                 |
| ----- | ------------------------------------------------------ |
| sq-01 | done — [findings](haiku-experiments/findings/sq-01.md) |
| sq-02 | done — [findings](haiku-experiments/findings/sq-02.md) |
| sq-03 | done — [findings](haiku-experiments/findings/sq-03.md) |
| sq-04 | done — [findings](haiku-experiments/findings/sq-04.md) |
| sq-07 | done — [findings](haiku-experiments/findings/sq-07.md) |

## Usefulness review

Each finished quest gets a one-line judgment (USEFUL / MARGINAL / NOT USEFUL) and the plan item it served. The
Opus review of sq-01 to sq-04 (2026-10-10) is preserved in
[`docs/knowledge/haiku-experiment-record.md`](knowledge/haiku-experiment-record.md#side-quest-usefulness-opus-review-2026-10-10).

## Script convention

Scripts go in **Deno** for larger work, **bash** for bash-oriented work, saved under `work-scripts/` (`deno/`
or `bash/`), with a row appended to `work-scripts/INDEX.md`. Read the index first; the building block may
exist.
