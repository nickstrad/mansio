# CLAUDE.md

Repo conventions for Claude Code in `mansio`. For the multi-agent build/review/merge protocol (batches, model
assignment, side quests), see `AGENTS.md` — this file does not repeat it.

## What this repo is

A document-db MVP: TypeScript document API (`sdk/`), SQLite-backed storage and a DuckDB reader (`platform/`),
a tic-tac-toe sample app (`app/`), and tests/scripts. It is mid-way through the Effect-TS port and
observability plan at `docs/plans/effect-port-and-observability.md` — read that plan's §0 and §1 before
touching `core/`, `platform/`, or `sdk/`.

## Environment

```sh
export PATH="$HOME/.deno/bin:$PATH"   # deno 2.9.5 is not on PATH in agent shells
```

Use `deno task <name>` for everything (see the plan's §2.3 for the full task list once item 01 lands:
`fmt:check`, `lint`, `check`, `test`, `test:prop`, `ci`, `explain`, `integration`, `serve`, `logs:*`,
`perf:*`). Don't hand-roll `deno run`/`deno test` invocations when a task exists.

## Branch and worktree etiquette

- Each unit follows the global fresh-agent lifecycle (see `~/.claude/CLAUDE.md`) plus this repo's
  [unit-of-work rules](AGENTS.md#unit-of-work-rules-repo-additions-to-the-global-lifecycle), including
  ownership-safe handoffs.
- Plan items build on `item/NN-slug` branches in worktrees under `/root/Software/mansio-work/`.
- Side quests build on `haiku/sq-NN` branches — see `docs/side-quests.md`.
- If you are not the agent assigned to a given branch/worktree, do not edit files in it or push to it. Check
  `AGENTS.md` and the current coordination state (`agent-work/COORDINATION.md`, if present) before assuming a
  branch is free to touch.

## Docs-only vs. implementation work

Some sessions in this repo are explicitly docs-only (updating `docs/side-quests.md`, `AGENTS.md`, `CLAUDE.md`,
or writing findings files) and must not implement plan items. If a task description says docs-only, stay out
of `core/`, `platform/`, `sdk/`, `app/`, `tests/`, `scripts/`, `perf/`, and `deno.json` entirely, even if a
fix looks trivial.

## Commit style

One logical change per commit; subject line `<area>: <imperative summary>` (e.g.
`core: extract total schema validation`). Match the plan's item-per-commit convention when landing plan work.

## Coordination entrypoint

Read `AGENTS.md` for the shared agent event-log requirement and
[`agent-work/event-log.md`](agent-work/event-log.md) for executable write/read commands at session start.
