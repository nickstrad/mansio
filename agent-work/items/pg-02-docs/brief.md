# Docs follow-up for PG-02 (C4 audit row + knowledge hand-offs) — brief

Unit: docs-only follow-up after PG-02 landed on `main` as merge `f52c817` (item `item/pg-02-content` @ 412a616).
Branch `docs/c4-pg-02` · Worktree `/root/Software/mansio-work/c4pg02` (created from main) · Builder: Claude Sonnet (actor
`claude-sonnet-c4-pg-02`) · Date: 2026-10-15. Rules: AGENTS.md "Diagram maintenance", `docs/architecture/AGENTS.md`.
Docs-only: only `docs/architecture/**` and `docs/knowledge/**`. `export PATH="$HOME/.deno/bin:$PATH"`; run `deno fmt --check`
from the repo root with no path arguments.

PG-02 changes `pages/` only (datasets, inventories, tests, pages/README.md): no runtime code, no workflow. Read
`git diff b789441 f52c817 --stat` and `pages/content/*/inventory.md` (decisions P12–P16, D10–D12). Expected: no C4
element, edge or note changes; add a PG-02 audit row to `docs/architecture/README.md` (SHA `f52c817`), move the stamp,
and extend the sentence about `pages/` to say all seven C4 kinds are now rendered (landscape and code in the current set;
dynamic flows and an illustrative deployment in the proposal). Commit `docs: PG-02 touches no C4 surface`.

Knowledge hand-offs from the PG-02 builder (second commit `docs: knowledge from PG-02`): in `docs/knowledge/decisions.md`
record that c4-sdk requires the `external` tag on supporting containers in component views, so the tag spreads to the
container views in both datasets (inventory D3/P12) and the standing c4-sdk hand-off asks to allow the scope system's
sibling containers without it; in `docs/knowledge/gotchas.md` record that `pkill -f '<pattern>'` from an agent shell can
match the agent's own `bash -c` command line and kill the shell (exit 144): stop preview servers by PID or port. Update
`docs/knowledge/index.md` entries/status line (PG-02 landed `f52c817`). Trailer `Co-Authored-By: Claude Sonnet 5.5
<noreply@anthropic.com>`. Checks: `python3 docs/architecture/diagrams/render.py --check`, root `deno fmt --check`. Push
`docs/c4-pg-02`; never touch `main`. Log task_started/task_completed (`--actor claude-sonnet-c4-pg-02 --branch docs/c4-pg-02`).
Report: SHAs, what changed and why, check outputs, learning assessment.
