# PG-02 — Proposed dataset completion and the missing C4 types (brief)

Unit: build item PG-02 of `docs/plans/pages-diagrams.md` (`/root/Software/mansio-work/c4react/docs/plans/pages-diagrams.md`,
read-only). Builder: Claude Opus 5.5, actor `opus-pg-02`. Branch `item/pg-02-content` off `main` `b789441` (PG-01 landed
as `b789441`). Worktree `/root/Software/mansio-work/pg02` (already created). Reviewer afterwards: Codex Astra. Date: 2026-10-15.
Authorization: Nick's go `/root/fable-c4-docs-go.md`; build delegated to Opus per `/root/fable-c4-docs-delegate-opus.md`.

## Orientation (in order)
1. `AGENTS.md`, `CLAUDE.md`, `agent-work/event-log.md`; the plan (§0–§2, PG-02 row); `pages/README.md`;
   `agent-work/items/pg-01-pages/brief.md`, and the review reports via `git show origin/review/pg-01-astra:agent-work/items/pg-01-pages/review-astra.md` and `git show origin/review/pg-01-astra-rereview:agent-work/items/pg-01-pages/review-astra-rereview.md` (what PG-01 decided and what the reviewer flagged).
2. `pages/content/mansio-lanes-proposal/inventory.md` and `pages/content/mansio-current/inventory.md` (conversion
   decisions D1–D5, P1–P5 — keep them, do not re-decide).
3. c4-sdk (`/root/Software/c4-sdk`, read-only): `docs/plans/implementation.md` §3.1 (`code`, `dynamic`,
   `deployment`, `landscape` constraints; `Step` grammar) and Milestone B; `fixtures/seven-types.json`,
   `fixtures/dynamic.json`, `fixtures/deployment.json` (shapes that decode).
4. Sources: `git show origin/docs/ffi-c4:docs/plans/ffi/README.md` ("Key flows": point read, deadline/interrupt,
   forwarded mutation, kill/restart) and `docs/plans/ffi/c4-component-ffi-lane.*` at `81d9936`;
   `platform/document_reader.ts` on `main` (for the one code view); `docs/plans/ffi-implementation.md` (what landed).

Environment: `export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:$HOME/.deno/bin:$PATH"`. Do not touch the vendored
copy (`pages/vendor/c4-sdk/**`), `pages/src/sdk.ts`, the workflow, or anything outside `pages/`.

## Build (one implementation commit on `item/pg-02-content`)
Owns: `pages/public/architectures/mansio-lanes-proposal.json`, `pages/public/architectures/mansio-current.json` (only to add
the code view and the landscape; no other change), `pages/content/**`, `pages/tests/**` (extend, do not weaken),
`pages/README.md` (dataset section), `agent-work/items/pg-02-content/**`. Do NOT edit `docs/knowledge/*` (a concurrent docs follow-up owns the index); report durable learnings in your hand-offs instead.

1. Proposed dataset (`mansio-lanes-proposal`; FFI-01..08 is landed and in `mansio-current`, this set proposes lanes/placement only — keep that wording): add the **lane component** diagram (from `c4-component-ffi-lane` at `81d9936`; the landed
   binding/classifier/deadline parts are `existing`, lane-only parts `planned`, shared ones `mixed` with notes).
2. **Dynamic** diagrams for the README's four flows: point read, deadline/interrupt, forwarded mutation,
   kill/restart. Each is one `dynamic` diagram with a `scenario: Step` tree; branching failure paths are separate
   named scenarios or explicit `parallel` branches — never a fake measurement or a claim that both outcomes occur.
   Reuse model relationships; distinct interaction ids; `reverse` only for a response.
3. One **illustrative deployment** (`environment: "illustrative"` or similar): two data nodes with lane instances,
   marked proposed/illustrative in its documents; it is not a claim about the actual droplet.
4. **Landscape** of the modelled people and systems (both datasets may share one landscape in `mansio-current`; do
   not invent running systems).
5. One **code** view of current `platform/document_reader.ts` functions under its owning component, source-linked
   to `main`; members are text (name, signature, visibility), not code. Do not fabricate FFI classes for unbuilt code.
6. Inventories extended: every new element/relationship/step maps to a source line or flow step; discrepancies
   and decisions recorded. Documents: plain + STE (Issue 9, draft) for every new diagram and element with prose.
7. Side-by-side pairing: confirm the viewer pairs the new component views automatically (PG-01 pairs the first
   diagram of each kind drawn in both datasets); if the proposed set now has a diagram kind the current set lacks,
   say so in the README (no viewer code changes in this item — hand off instead).
8. Tests: the existing bundle tests must stay green and strict (zero diagnostics); add a test that each dynamic
   scenario references only relationships whose endpoints are members of that diagram.

## Acceptance (paste outputs)
`cd pages && npm ci && npm run check` green; root `deno task fmt:check` and `deno task lint` green; all seven C4
kinds decode across the two datasets (list them); Playwright smoke still green; `git status` clean.

## Protocol
Events via `work-scripts/bash/log-event.sh --actor opus-pg-02 --branch item/pg-02-content`. Commits: brief
(`git add -f`), ONE implementation commit `pages: complete the proposed dataset and add the missing C4 views`,
no knowledge commit (hand off learnings); trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Push the branch; never
touch `main`. Report SHAs, acceptance outputs, decisions, reviewer notes, learning, hand-offs. Leave the worktree.
