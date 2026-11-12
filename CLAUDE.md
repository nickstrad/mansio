# CLAUDE.md

Repo conventions for Claude Code in `mansio`. For the multi-agent build/review/merge protocol (batches, model
assignment), see `AGENTS.md` — this file does not repeat it.

## What this repo is

A document DB: a TypeScript client SDK (`sdk/`) that runs app handlers and speaks the IR protocol
(`ir/spec/v1.md`, `POST /v1/ops`) to the Go server (`server/`: agentd, controld, the edge, dbtool), a
tic-tac-toe sample app (`app/`), the shared core (`core/`: schema, plan, errors, the TS compiler that is the
oracle for Go, IR rules, the event-log record format), the shared IR vectors (`ir/vectors/`), the offline
event-log CLI (`eventlog/`, `scripts/logs.ts`), and tests/scripts. The Deno platform was deleted at IR-6 (ADR
0013). The last plan, `docs/plans/completed/ir-protocol.md`, is complete; the Go side has its own
`server/AGENTS.md` and `server/shared_state.md`.

## Environment

```sh
export PATH="$HOME/.deno/bin:$PATH"   # deno 2.9.5 is not on PATH in agent shells
```

Use `deno task <name>` for everything (`deno.json`: `fmt:check`, `lint`, `check`, `test`, `test:prop`, `ci`,
`go:ci`, `ci:all`, `explain`, `logs:*`, `perf:*`, `sim:swarm`; `e2e` is in `package.json`). Don't hand-roll
`deno run`/`deno test` invocations when a task exists. Run `deno task ci` on Linux
(`work-scripts/bash/deno-linux.sh 'deno task ci'`): the DuckDB event-log suites hang on macOS. Run e2e with
`work-scripts/bash/e2e-linux.sh`. See `docs/testing.md`.

## Branch and worktree etiquette

- Each unit follows the global fresh-agent lifecycle (see `~/.claude/CLAUDE.md`) plus this repo's
  [unit-of-work rules](AGENTS.md#unit-of-work-rules-repo-additions-to-the-global-lifecycle), including
  ownership-safe handoffs.
- Plan items build on `item/NN-slug` branches in worktrees under `/root/Software/mansio-work/`.
- If you are not the agent assigned to a given branch/worktree, do not edit files in it or push to it. Check
  `AGENTS.md` and the current coordination state (`agent-work/COORDINATION.md`, if present) before assuming a
  branch is free to touch.

## Docs-only vs. implementation work

Some sessions in this repo are explicitly docs-only (updating `AGENTS.md`, `CLAUDE.md`, or writing findings
files) and must not implement plan items. If a task description says docs-only, stay out of `core/`,
`eventlog/`, `sdk/`, `app/`, `server/`, `tests/`, `scripts/`, `perf/`, and `deno.json` entirely, even if a fix
looks trivial.

## Commit style

One logical change per commit; subject line `<area>: <imperative summary>` (e.g.
`core: extract total schema validation`). Match the plan's item-per-commit convention when landing plan work.

## Coordination entrypoint

Read `AGENTS.md` for the shared agent event-log requirement and
[`agent-work/event-log.md`](agent-work/event-log.md) for executable write/read commands at session start.
