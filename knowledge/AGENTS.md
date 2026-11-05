# knowledge/ — shared notes for agents working in ts-db-go

More than one agent works in this repo at the same time. This folder holds what they learn, so the next agent
(or the next session after a context clear) does not have to find it again.

## Rules

1. One folder per topic or workstream: `knowledge/<topic>/`. Put notes (`*.md`) and artifacts (small CSVs,
   report output, charts, configs used) for that topic inside it. Large raw data stays in `results/`; link it.
2. Every topic folder has a `README.md`: what it is, who owns it, newest finding first.
3. Add one line per topic folder (and per important note) to `knowledge/index.md`. Keep the index short.
4. Write facts with the evidence: the numbers, the config, the command, the machine, the date
   (absolute, e.g. 2026-11-04). Say what is measured and what is guessed.
5. Edit only your own topic folders. To correct another agent's note, add a dated note in your folder and link
   it from theirs' index line, or tell the user.
6. Each agent's live plan and status stays in its state file at the repo root (`STATE.md`,
   `docs/state_files/STATE-coldload.md` (archived)), not here. This folder is for findings that should outlast the work.

## Agents and state files

| Agent | State file | Topic folders |
|---|---|---|
| sqlbench (actor-per-database load test) | `STATE.md` | `sqlbench/`, `multi-agent-coordination/` |
| writeback (warm → cold durability: generations, manifest, write-back, eviction) | `STATE-writeback.md` (archived to `docs/state_files/`) | `writeback/` |
| coldload (SQLite files in object storage → virtual actors) | `docs/state_files/STATE-coldload.md` (archived) | `coldload/`, `testing/`, `concurrency-in-go/` |

Coordination between agents (state files, `shared_state.md`, locks): [multi-agent-coordination/](multi-agent-coordination/README.md).
