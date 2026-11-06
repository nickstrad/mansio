# PG-02b — Architecture selector with N entries, sticky chrome, no comparison view (brief)

Unit: build item PG-02b of `docs/plans/pages-diagrams.md` (`/root/Software/mansio-work/c4react/docs/plans/pages-diagrams.md`,
read-only; read §0 "Selector" and the PG-02b section). Builder: Claude Opus 5.5, actor `opus-pg-02b`. Branch
`item/pg-02b-selector` off `main` `f52c817` (PG-02 landed as `f52c817`). Worktree `/root/Software/mansio-work/pg02b`. Reviewer
afterwards: Codex Astra. Date: 2026-10-15. Authorization: Nick's direction `/root/fable-pg-dropdown.md` AND its amendment `/root/fable-pg-dropdown-amend.md` (read both verbatim; the amendment drops side-by-side comparison for now and requires sticky top chrome) under
his go `/root/fable-c4-docs-go.md`; build delegated to Opus per `/root/fable-c4-docs-delegate-opus.md`.

Environment: `export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:$HOME/.deno/bin:$PATH"`; `grep` is a ugrep wrapper (use
`command grep`); run root `deno fmt`/`deno fmt --check` only from the repo root with no path arguments.

Read: `AGENTS.md`, `CLAUDE.md`, `pages/README.md`, `pages/src/*` (App.tsx, routes.ts, sdk.ts — sdk.ts is read-only for you),
`pages/tests/*`, `pages/public/catalog.json`, and the PG-01 review reports (`git show origin/review/pg-01-astra:agent-work/items/pg-01-pages/review-astra.md`).

Owns: `pages/src/**` (in `sdk.ts` you may ONLY delete comparison-only helpers such as the in-memory route port; the `c4-sdk` import wiring, layout port and hash route port stay); `pages/tests/**`; `pages/public/catalog.json` (add `defaultId` only);
`pages/README.md`; the "Architecture diagrams site" section of root `README.md` (framing wording only; never the
Configuration table); `agent-work/items/pg-02b-selector/**`. Do not touch `pages/vendor/**`, bundles under
`pages/public/architectures/`, inventories, the workflow, `docs/`.

Build (ONE implementation commit `pages: sticky top bar with an architecture dropdown; drop the comparison view`):
1. Catalog: optional `defaultId` in `catalog.json` (set to `mansio-current`); the app defaults to it, else the first entry.
   Entries are peers: no constant names a "proposed" or "current" entry (`PROPOSED_ID`-style constants go).
2. Single view: a labelled, keyboard-reachable dropdown (native `<select>` is fine) listing every entry by title; selecting
   one navigates to that architecture's default diagram via the SDK route port; deep links still work.
3. Remove the comparison view: the compare mode, its `#/compare/<a>/<b>` route, the pairing logic, the second
   provider/in-memory route port, and every comparison-only test (routes tests on `counterpart`/pairing, the side-by-side
   and component-pairing Playwright smokes). One architecture is shown at a time. Keep Not Found behaviour for unknown ids.
3b. Sticky top chrome: the architecture dropdown and the navigation/back controls (`DiagramNavigation` from `c4-sdk`, or
   the equivalent the app already renders) live in a top bar pinned while the page scrolls (`position: sticky; top: 0` or
   fixed), so switching architecture and going back work from anywhere in a tall diagram. Plain CSS; keep the canvas height
   sensible under the bar.
4. Copy: remove "Current · Proposed", "Side by side", "current vs proposed" and similar two-slot framing from UI strings, `pages/README.md`
   and the root README section; describe the site as "N architectures, the current one by default". Per-element status
   wording inside bundles and inventories is data and stays untouched.
5. Tests: update `routes.test.ts`/`smoke.spec.ts`; add a fixture catalog with THREE entries (reuse the two real bundles plus a
   tiny third bundle under `pages/tests/fixtures/` — not under `public/`) proving the selector works with no code change for N=3; assert the default selection; add a smoke test that scrolls a
   tall diagram page and asserts the dropdown and the back control remain within the viewport.
6. `pages/README.md`: document the selector, `defaultId`, the sticky bar, that there is no comparison view for now (Nick, 2026-10-14), and how to add an architecture (catalog entry + bundle + inventory).

Acceptance (paste outputs): `cd pages && npm ci && npm run check` green (tsc, vitest, build, sync --check, Playwright); root
`deno task fmt:check`, `deno task lint`, `deno task check` green; `command grep -rni "PROPOSED_ID\|compare\|side by side\|current vs proposed" pages/src pages/tests pages/README.md README.md` prints nothing; `git status` clean.

Protocol: events via `work-scripts/bash/log-event.sh --actor opus-pg-02b --branch item/pg-02b-selector`; commits: brief
(`git add -f agent-work/items/pg-02b-selector/brief.md`), ONE implementation commit, no knowledge edits (hand off); trailer
`Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`; push; never touch `main`. Report SHAs, acceptance outputs, design
decisions (route shape, default handling), reviewer notes, learning, hand-offs. Leave the worktree.
