# work-scripts/

Reusable scripts produced during agent work in this repo. The human wants the code used during work kept, so
anything an agent writes to investigate, measure, or verify goes here instead of `/tmp` — committed, indexed,
and reusable as building blocks.

## Deterministic placement

| Kind of work                                                                              | Language          | Location             | Run with                                  |
| ----------------------------------------------------------------------------------------- | ----------------- | -------------------- | ----------------------------------------- |
| Larger / multi-step work (benchmarks, data munging, API probes, anything with real logic) | Deno (TypeScript) | `work-scripts/deno/` | `deno run -A work-scripts/deno/<name>.ts` |
| Basic bash-oriented things (one-liners grown up, file ops, greps, quick checks)           | Bash              | `work-scripts/bash/` | `bash work-scripts/bash/<name>.sh`        |

Rules:

1. **Pick by shape of the work, not by who you are.** Haiku, Sonnet, Opus, and the main agent all use this
   same directory — Haiku scripts live here too, collocated with regular work scripts (not in a separate
   haiku-only folder).
2. **Name deterministically:** `<area>-<slug>.<ext>` — area is the topic (`k6`, `bench`, `duckdb`, `git`, …),
   slug is a short kebab-case description. Examples: `deno/bench-jsonl-append.ts`, `bash/k6-output-sample.sh`.
3. **Bash scripts** start with `#!/usr/bin/env bash` and `set -euo pipefail`, and are `chmod +x`.
4. **Deno scripts** use explicit permissions instead of `-A` when practical, and document any
   network/filesystem access at the top.
5. **No secrets, tokens, or credentials** in scripts, ever. Parameterize with env vars and document which
   ones.
6. **Keep them runnable.** A script that only worked once on one machine is clutter — either make it portable
   or don't commit it.
7. **Index it.** After adding a script, append a row to `INDEX.md` (see below). No unindexed scripts.

## INDEX.md

The index is the contract. Every agent that adds a script appends one row:

`| script | date | author | purpose | usage | notes |`

- `script`: path relative to `work-scripts/`
- `date`: YYYY-MM-DD
- `author`: which agent/model added it (e.g. `haiku (sq-02)`, `sonnet (item 14)`, `main agent`)
- `purpose`: one line — what question it answers or what job it does
- `usage`: the exact command to run it
- `notes`: dependencies, gotchas, or "superseded by X"

Before writing a new script, **read `INDEX.md` first** — the building block you need may already exist. Prefer
extending an existing script over adding a near-duplicate; note the extension in the `notes` column.
