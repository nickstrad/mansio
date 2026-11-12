# CLAUDE.md

Repo conventions for Claude Code in `mansio`. The multi-agent build/review/merge protocol, the agents' state
files and their event log live under [`agent-work/`](agent-work/AGENTS.md); this file does not repeat them.

## What this repo is

A document database. A TypeScript SDK (`sdk/`) runs application handlers in the caller's process and speaks
the intermediate representation (IR) protocol (`intermediate-representation/spec/v1.md`, `POST /v1/ops`) to
the Go server (`server/`: agentd, controld, the edge, agentctl, dbtool). Also here: the shared core (`core/`:
schema, plan, errors, the TypeScript compiler that is the oracle for the Go one, IR rules, the event-log
record format), the shared IR test vectors (`intermediate-representation/vectors/`), the offline event-log CLI
(`eventlog/`, `scripts/logs.ts`), a sample app (`examples/tic-tac-toe/`), tests, and the docs (`docs/`, with
the architecture diagram site in `docs/pages/`).

## Environment

```sh
export PATH="$HOME/.deno/bin:$PATH"   # deno 2.9.5 is not on PATH in agent shells
```

Use `deno task <name>` for everything (`deno.json`: `fmt:check`, `lint`, `check`, `test`, `test:prop`, `ci`,
`go:ci`, `ci:all`, `e2e`, `explain`, `logs:*`, `perf:*`, `sim:swarm`). Don't hand-roll `deno run`/`deno test`
invocations when a task exists. Run `deno task ci` on Linux (`scripts/dev/deno-linux.sh 'deno task ci'`): the
DuckDB event-log suites hang on macOS. Run e2e with `scripts/dev/e2e-linux.sh`. See `docs/testing.md`.

## Conventions

- Go code follows `server/docs/coding-style.md`. Package folders are short and lowercase (`internal/ir`), and
  the package comment spells out what an abbreviated name means.
- When the Go and TypeScript sides must agree, the shared vectors decide; regenerate generated files
  (`docs/retry-contract.md`, `server/docs/capacity.md`, `core/compile/GENERATED_SQL.txt`) with the command
  named in their header or test rather than editing them.
- Keep `docs/pages/` (the diagram site) showing only what is on `main`; refresh it when the architecture
  changes (`docs/pages/README.md`, "Keeping main in sync").

## Branch and worktree etiquette

- Each unit of agent work follows the global fresh-agent lifecycle (see `~/.claude/CLAUDE.md`) plus this
  repo's unit-of-work rules in `agent-work/AGENTS.md`, including ownership-safe handoffs.
- Plan items build on `item/NN-slug` branches in worktrees under `/root/Software/mansio-work/`.
- If you are not the agent assigned to a given branch/worktree, do not edit files in it or push to it. Check
  `agent-work/AGENTS.md` and the current coordination state (`agent-work/COORDINATION.md`, if present) before
  assuming a branch is free to touch.

## Docs-only vs. implementation work

Some sessions are explicitly docs-only and must not implement plan items. If a task description says
docs-only, stay out of `core/`, `eventlog/`, `sdk/`, `examples/`, `server/`, `tests/`, `scripts/`, `perf/` and
`deno.json` entirely, even if a fix looks trivial.

## Commit style

One logical change per commit; subject line `<area>: <imperative summary>` (e.g.
`core: extract total schema validation`).

## Coordination entrypoint

Agents read `agent-work/AGENTS.md` for the shared event-log requirement and
[`agent-work/event-log.md`](agent-work/event-log.md) for the write/read commands at session start.
