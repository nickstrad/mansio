# Multi-agent coordination: state files, a shared log, and file locks

Owner: sqlbench agent. Pattern in use in this repo since 2026-11-04, when two agents (sqlbench and coldload)
worked in `/Users/nick/Software/ts-db-go` at the same time. Copy it to any repo where more than one agent works.

## Why

Two agents in one working copy step on each other in three ways we saw on 2026-11-04:
1. **Same file, same time.** Both wrote `internal/{assert,errs,logx}` within a minute; one copy silently replaced
   the other. Appends to a shared notes file can interleave the same way.
2. **Shared machine.** One agent's benchmark (SeaweedFS at ~1700% CPU) skews the other's numbers.
3. **Lost context.** An agent restarted after a context clear does not know what the other one owns or changed.

## The pattern (four pieces)

| Piece | File | Rule |
|---|---|---|
| Per-agent state | `STATE.md`, `STATE-<agent>.md` at repo root | Each agent's goal, plan, status (newest first), findings, **files it owns**, APIs others rely on. Only its owner edits it. |
| Shared log | `shared_state.md` at repo root | Append-only conversation between agents: claims, API changes, machine use, "it builds again". Never edit an entry; reply with a new one. Append only via `scripts/shared-note.sh`. |
| Locks | `scripts/lock.sh <lock> <cmd...>` | Wrap any write to a file both agents may touch (`go.mod`, `knowledge/index.md`, ...) and every benchmark run (`results/.bench-lock`). |
| Durable knowledge | `knowledge/<topic>/` | Findings that outlast the work, one folder per topic, indexed in `knowledge/index.md` (see `knowledge/AGENTS.md`). |

Rules that made it work:
- **Claim before you create.** Post the claim in `shared_state.md` (with the lock) *before* writing a new shared
  package or file, then re-check the log. If the other agent already claimed it, use theirs.
- **Ownership is by path.** Each state file lists the paths its agent owns. Shared packages name one maintainer;
  the other agent makes additive changes only and posts signature changes first.
- **State files hold live status; the shared log holds what the other agent must know; knowledge holds facts.**

## The scripts

- [`scripts/lock.sh`](scripts/lock.sh) — `scripts/lock.sh <lock> <command> [args...]`. The lock is a directory
  (`mkdir` is atomic). `<lock>` is a name (`.locks/<name>`) or a path with a slash (e.g. `results/.bench-lock`).
  Waits while a live process holds it; removes it as stale if the owner pid is gone. Owner file:
  `<who> pid <pid> started <time>: <command>`; set `LOCK_WHO=<agent>`. Lock released on exit, INT or TERM.
- [`scripts/shared-note.sh`](scripts/shared-note.sh) — `scripts/shared-note.sh <agent> <topic> < body.md` appends
  `## <date time> — <agent>: <topic>` plus the body to `shared_state.md` under the `shared_state` lock.

Tested 2026-11-04 (macOS, bash 3.2): two concurrent writers serialized (`a1 a2 b1 b2`, never interleaved); a lock
with a dead owner pid was cleared and the command ran.

Limits: advisory only (an agent that writes without the lock is not stopped); same machine only (pid check);
`kill -0` cannot tell a reused pid from the original owner.

## Adopting it in a new repo

See [AGENTS.md](AGENTS.md).
