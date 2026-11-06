# PG-01 — Pages scaffold, interim c4-sdk copy, viewer, first datasets (brief)

Unit: build item PG-01 of `docs/plans/pages-diagrams.md` (on branch `plan/c4-react`; read it at
`/root/Software/mansio-work/c4react/docs/plans/pages-diagrams.md`, read-only — do not edit that worktree). Builder:
Claude Opus 5.5, actor `opus-pg-01`. Branch `item/pg-01-pages` off `main` `2d30330`. Worktree
`/root/Software/mansio-work/pg01` (already created). Reviewer afterwards: Codex Astra (cross-family). Date: 2026-10-15.
Authorization: Nick's go `/root/fable-c4-docs-go.md` and delegation `/root/fable-c4-docs-delegate-opus.md` (read both).

## Orientation (read first, in this order)
1. `AGENTS.md` (unit-of-work rules, build protocol, event log) and `CLAUDE.md`; `agent-work/event-log.md`.
2. The plan above: §0 direction, §1 facts (frozen `deno.json`, fmt/lint scan `pages/`, gitignored directory
   names, import contract, copy inventory, worker, versions, bundle format, hosting), §2 content sources, §3 PG-01.
3. c4-sdk at `/root/Software/c4-sdk` (read-only; never commit or push there): `docs/api/public-api.md`,
   `docs/plans/implementation.md` §3 (JSON contract) and "Milestone B", `README.md` "Source SDK API",
   `tests/package/consumer/app.tsx.template` (how a bundler consumer wires styles, worker, decode),
   `fixtures/seven-types.json` and `public/catalog.json` (shapes), `sdk-build.mjs` (what the npm build does to
   the source so the swap is understood), `package.json` (versions).
4. mansio sources to convert: `docs/architecture/AGENTS.md` (C4 conventions), `docs/architecture/README.md`
   (audited state and inventory), the four current diagram pairs and their `diagrams/*.py` sources on `main`;
   `git show origin/docs/ffi-c4:docs/plans/ffi/README.md` and the three proposed pairs on that branch (do NOT
   merge that branch; read with `git show`); `docs/architecture/lang-switch-fable.md`;
   `docs/plans/ffi-implementation.md` (what landed: FFI-01..08).

Environment: `export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:$HOME/.deno/bin:$PATH"` (node 22.23.2, npm 10.9.8,
deno 2.9.5). The network is available for `npm install`. Do not run mansio test suites concurrently with
another agent's suites (check `ps -eo pid,args | grep -E "deno (test|task)"` first); you only need
`deno task fmt:check`, `deno task lint`, `deno task check` here.

## Build (one implementation commit on `item/pg-01-pages`)

Owns: `pages/**`, `.github/workflows/pages.yml`, `agent-work/items/pg-01-pages/**` (this brief; `git add -f`),
one short "Architecture diagrams site" section in root `README.md` (never touch its Configuration table — a test
mirrors it), and `docs/knowledge/{index,decisions,gotchas}.md` entries. Edit nothing else; never edit
`deno.json`, `core/`, `platform/`, `sdk/`, `app/`, `tests/`, `scripts/`, `perf/`, `docs/architecture/`.

1. **Vendored copy** `pages/vendor/c4-sdk/`: `git -C /root/Software/c4-sdk fetch origin` then copy from
   `origin/main` (expected `a1929e43b4f72229524f2ee24a7e88de10ce8879`; record the SHA you actually used):
   `src/index.ts`, `src/core/**`, `src/lib/c4/**`, `LICENSE`, `THIRD_PARTY_NOTICES.md`. Write `sync.sh`
   (bash, `set -euo pipefail`) that takes a SHA, exports those paths with `git archive`/`git show`, applies the
   post-processing needed to pass the ROOT `deno fmt --check` and `deno lint` (e.g. `deno fmt` over the copy and
   a `// deno-lint-ignore-file` first line where rules fire — keep it minimal and deterministic), and with
   `--check` re-runs into a temp dir and diffs against the committed copy (exit non-zero on any difference).
   `PROVENANCE.md`: upstream URL, SHA, date, file count, exact post-processing, the swap checklist pointer.
   Do not modify the copied code by hand. Do not copy `src/App.tsx`, `src/main.tsx`, `src/components`,
   `src/lib/utils.ts`, demos, tests or configs.
2. **npm project** `pages/package.json` + lockfile (`npm install` once, commit `package-lock.json`), pinned to
   c4-sdk's versions (plan §1), `"engines": {"node": "22.23.2"}`, scripts at least: `dev`, `build`
   (`vite build --base /mansio/`), `typecheck`, `test`, `check` (= typecheck + test + build + `vendor/c4-sdk/sync.sh --check`).
   Vite + TS aliases: `c4-sdk` → `vendor/c4-sdk/src/index.ts`, `c4-sdk/core` → `vendor/c4-sdk/src/core/index.ts`,
   `c4-sdk/styles.css` → `vendor/c4-sdk/src/lib/c4/styles/c4.css`. Nothing in `pages/src` may import a
   `vendor/...` path directly; all SDK wiring (imports, layout port, route port, loader) lives in ONE module
   `pages/src/sdk.ts`. Keep the app shell small; plain CSS is fine (no Tailwind/shadcn needed).
3. **Viewer** (`pages/index.html`, `pages/src/**`): hash routing via `createHashRoutePort`, catalog at
   `public/catalog.json` with entries `mansio-current` and `mansio-ffi-proposal` (entry id = bundle id). Modes:
   Current · Proposed · **Side by side** (current and proposed diagrams of the same C4 level rendered next to
   each other, each with its own provider; the second viewer needs a non-URL route port — implement a small
   in-memory `RoutePort` in `sdk.ts` using the exported type; use only the public API of `c4-sdk`/`c4-sdk/core`).
   Deep links reload correctly; a missing diagram shows Not Found, never a silent fallback. Keyboard reachable.
   Site title "mansio architecture", a one-paragraph purpose line (used when designing features: current vs
   proposed), and a link back to the repo's `docs/architecture/` Markdown diagrams.
4. **Datasets** (`public/architectures/*.json`, formatted with `deno fmt`):
   - `mansio-current.json`: all four current pairs (context, platform containers, observability containers,
     server components) as `context`, two `container`, one `component` diagrams; every element/relationship
     `existing`; `detail` targets chain context → container → component; every node and edge from the source
     appears (edge `displayNumber` = the source's edge number); legends mirror the source keys; documents give a
     `plain` and an `ste` text (ASD-STE100 Issue 9, `reviewStatus: draft`) per diagram and per element where the
     source has prose; `sources` are commit-pinned GitHub URLs (`main` `2d30330`).
   - `mansio-ffi-proposal.json`: the proposed **context** and **container (data node with lanes)** diagrams from
     `docs/ffi-c4` `81d9936`. Status per element: lanes, placement table, forwarder, lane supervisor, WAL frames
     → `planned`; elements that exist on `main` today → `existing`; existing elements gaining planned duties →
     `mixed` with `plannedNotes`. The FFI binding, statement classifier and deadline/interrupt path are LANDED
     (FFI-01..08): where the source still says "(planned)" for them, convert as `existing` and write a
     conversion note. `libsqlite3` is an implementation dependency of a process, not a container. Timing
     numbers go in documents with provenance, never in labels. Leave the lane component, dynamic, deployment,
     landscape and code views to PG-02 (say so in the inventory).
   - `pages/content/<dataset>/inventory.md`: source node/edge number → JSON id → document ids, plus every
     discrepancy between the Mermaid and terminal versions and every conversion decision.
5. **Pages workflow** `.github/workflows/pages.yml` modeled on c4-sdk's (`on: push: branches: [main]` and
   `workflow_dispatch`; `actions/setup-node@v4` node 22 with npm cache keyed on `pages/package-lock.json`;
   `npm ci` and `npm run build` in `pages/`; copy `index.html` to `404.html`; `configure-pages@v5`,
   `upload-pages-artifact@v3`, `deploy-pages@v4`). GitHub Pages is not enabled on the repo yet; do not try to
   enable it (human gate) — note it in `pages/README.md`.
6. **Tests** `pages/tests/`: a Node test (vitest or `node:test`) that decodes both bundles and the catalog with
   `decodeArchitecture` from `c4-sdk/core` and fails on any error diagnostic or on a catalog id that does not
   match its bundle id; a test that every `sources[].url` is an `https://github.com/nickstrad/mansio/blob/<40-hex>/…`
   URL. Optional if feasible without a network browser download: a Playwright smoke against `vite preview`
   that the context diagram renders and side-by-side shows two canvases (c4-sdk has Chromium installed under
   its `node_modules`; if you cannot reuse it offline, skip and say so).
7. **Docs**: `pages/README.md` (purpose, run, build, how datasets are authored and verified, the exact npm swap
   checklist: delete `vendor/c4-sdk`, drop the aliases, `npm install c4-sdk@0.1.0`, worker factory from
   `c4-sdk/layout-worker.js?url` in `sdk.ts`, re-run `npm run check`); root `README.md` section linking
   `pages/` and the future `https://nickstrad.github.io/mansio/`.

## Acceptance (run all; paste outputs in your report)
- `cd pages && npm ci && npm run check` green; `npm run build` emits under `pages/dist/` (gitignored, not committed).
- `pages/vendor/c4-sdk/sync.sh --check` → zero diff.
- From the repo root: `deno task fmt:check`, `deno task lint`, `deno task check` all green with `pages/` present.
- `grep -rn "vendor/c4-sdk" --include=*.ts --include=*.tsx --include=*.json . | grep -v "^./pages/"` empty, and
  inside `pages/src` no import contains `vendor/`; `grep -rn "from '\.\./\.\./\(core\|platform\|sdk\)" pages/src` empty.
- `git status` clean after commit; no `node_modules`, `dist` or `.vite` committed.
- Report which c4-sdk SHA was copied and the exact post-processing applied.

## Protocol
- Log `task_started`/`task_completed` with `work-scripts/bash/log-event.sh --actor opus-pg-01 --branch item/pg-01-pages`;
  log `blocker` if stuck (e.g. npm registry failures after retries).
- Commits: (1) `agent-work: add PG-01 brief` (copy this file to `agent-work/items/pg-01-pages/brief.md`,
  `git add -f`), (2) ONE implementation commit `pages: add the architecture diagrams site with an interim c4-sdk copy`,
  (3) optional `docs: knowledge from PG-01` for `docs/knowledge/`. End each message with
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Push `item/pg-01-pages`. Never touch `main`.
- Report: commit SHAs, acceptance outputs, the copied SHA, design decisions (side-by-side route handling, lint
  post-processing), conversion notes worth a reviewer's attention, learning assessment, and hand-offs outside
  your Owns set (do not edit them). Leave the worktree for coordinator inspection.
