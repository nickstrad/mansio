# Docs follow-up for PG-01 (C4 diagrams + knowledge index) — brief

Unit: docs-only follow-up after PG-01 landed on `main` as merge `b789441` (item `item/pg-01-pages` @ 8043f49).
Branch: `docs/c4-pg-01` · Worktree: `/root/Software/mansio-work/c4pg01` (already created from main) · Builder: Claude
Sonnet (actor `claude-sonnet-c4-pg-01`) · Date: 2026-10-15. Rules: AGENTS.md "Diagram maintenance",
`docs/architecture/AGENTS.md`. Docs-only: only `docs/architecture/**` and `docs/knowledge/**`.

PG-01 adds `pages/` (an npm/Vite static site that renders the repo's C4 diagrams through a vendored copy of c4-sdk),
`.github/workflows/pages.yml` (GitHub Pages deploy), a README section and three knowledge entries. It changes no
runtime code: nothing under `core/`, `platform/`, `sdk/`, `app/`. Read `git diff 2d30330 b789441 --stat` and the
`pages/README.md`. Decide whether any C4 diagram element, edge or note changes. Expected: the running platform's
context, container and component pairs are unchanged (the site is a documentation artifact built in CI, not a
process of the platform, and it talks to no platform container); if you conclude otherwise, say why and update both
formats. Either way, add a PG-01 audit row to `docs/architecture/README.md` (SHA `b789441`) and move the stamp,
and add one sentence to the audited-state paragraph pointing at `pages/` as the interactive rendering of these
diagrams (current set pinned to `main` `2d30330`, proposed set `mansio-lanes-proposal`).

Also fix, in `docs/knowledge/index.md`, the "Where the project stands" line that still says FFI-08 is pending
review: FFI-01..08 incl. FFI-04b landed (FFI-08 merge `0b82504`, plan record `2d30330`), and PG-01 landed as
`b789441`. Keep the entries PG-01 added.

Commit as `docs: PG-01 touches no C4 surface` (or `docs: update C4 diagrams for PG-01`), and a second commit
`docs: refresh the knowledge index status line` for the index fix, each ending with
`Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Run `python3 docs/architecture/diagrams/render.py --check`
and, from the repo root with no path arguments, `deno fmt --check` (`export PATH="$HOME/.deno/bin:$PATH"`; with an
explicit path deno would format at width 80). Push `docs/c4-pg-01`; do not touch `main`. Log `task_started` /
`task_completed` with `work-scripts/bash/log-event.sh --actor claude-sonnet-c4-pg-01 --branch docs/c4-pg-01`.
Report: commit SHAs, what changed and why, check output, learning assessment.
