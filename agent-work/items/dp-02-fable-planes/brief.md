# Fable task — DP-02: build the revised planes dataset (your M1–M8, as a new C4 dataset)

You are Fable (Claude Fable 5.1) in the mansio repo. Nick's request, verbatim: "Have it make
another c4 diagram patch of what it proposes and then commit and push so it will be another
drodown in the ui" — i.e., turn the modified design from your comparison doc into a real C4
dataset of its own, so the diagrams site gains a fourth architecture in its dropdown.

## Setup

- Work in a NEW worktree: `git worktree add /root/Software/mansio-work/dp02 -b item/dp-02-fable-planes main`
  from `/root/Software/mansio` (main is at `3d25004`, your comparison commit). Work only there.
- Actor for event logs: `claude-fable`, branch `item/dp-02-fable-planes`
  (`work-scripts/bash/log-event.sh`).
- Environment: `export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:$HOME/.deno/bin:$PATH"`.

## Read first

- `docs/architecture/scale-out-comparison-fable.md` — your own comparison and the M1–M8
  modifications. This dataset is those modifications, drawn.
- `agent-work/items/dp-01-planes/brief.md`, `addendum-1.md`, `review-astra.md`,
  `review-astra-rereview.md`, `report.md` — the planes design record, its contracts, and its
  two recorded deferrals (your M5–M7 close them).
- `pages/public/architectures/mansio-planes-proposal.json` and
  `pages/content/mansio-planes-proposal/inventory.md` — the base design you are patching, and
  the dataset conventions (bundle grammar, status marking, plain + STE documents, inventory).
- `pages/README.md` — how datasets are registered for the architecture dropdown.

## Build

New dataset id: `mansio-planes-fable`. Title in the catalog/README along the lines of
"Fable revision: control/data planes with route grants (M1–M8 applied)" — refine the wording
if the catalog format wants it shorter.

Contents: the planes design with your modifications applied — at minimum:

- **M1** Route grants: authorization resolves (tenant, database) once and issues a time-boxed
  **route grant**; the gateway forwarder caches and reuses it until TTL expiry or revocation;
  a stale-route retry obtains a *fresh decision* (say exactly that). Draw the grant as an
  element/document and in the query dynamic flow; the router stays authoritative but out of
  the per-request path while a grant is valid.
- **M3** The agent process gets its own component detail: the lane view inside the agent
  (classifier, FFI execution, deadlines, lane pinning), with snapshots taken off the writer
  lock.
- **M4** A default database per tenant and a specified selection surface: how a request names
  a database, and what happens when it names none.
- **M5** VM loss, end to end (closing the DP-01 deferral): detection, operator-attested
  isolation of the lost VM, restore-elsewhere from verified backups, and the standing rule
  that inability to contact the old writer is never permission to activate a copy without
  that attestation.
- **M6** Backup completion as a recorded manifest keyed by (database, generation): snapshot →
  upload → verify → manifest published → eligible for restore, as elements and a dynamic flow.
- **M7** Instance-level move diagrams: source agent and target agent as distinct participants,
  with the checkpoint-ordering fix from the DP-01 repair (settle → checkpoint via maintenance
  connection → close → acknowledge quiescence → copy → flip → activate).
- **M8** Your v1 placement rule, drawn as the proposal's candidate policy and labeled in its
  documents as **proposed, pending Nick's decision** — it is a decision request, not a
  settled fact.
- **M2** is build order, not architecture: note the sequencing in the dataset documents
  (in-node half first, keyed by database, lanes acceptance tests reused) rather than drawing
  it as a component.

Everything carried over from `mansio-planes-proposal` that M1–M8 does not change stays
semantically identical (registries, epoch-checked lifecycle commands, admission policy,
bounded scheduling keyed by database). Status marking stays honest: this is a proposal —
`planned`, with landed pieces (`existing`/`mixed`) marked exactly as in the planes dataset.

Deliverables mirror DP-01: the bundle `pages/public/architectures/mansio-planes-fable.json`,
`pages/content/mansio-planes-fable/inventory.md` (every element/relationship/step sourced —
to the planes dataset, your comparison doc's M-numbers, or this brief), plain + STE documents
for every diagram and element, dropdown registration the same way DP-01 registered planes,
`pages/README.md` dataset section, tests extended only if the harness requires per-dataset
coverage (never weakened).

## Hard constraints

- The three existing dataset bundles (`mansio-current`, `mansio-lanes-proposal`,
  `mansio-planes-proposal`) and their content trees remain **byte-identical**. This is an
  addition, never an edit of the planes dataset.
- No viewer code changes beyond dataset registration, if registration even needs it.

## Acceptance (paste outputs in your final message)

- `cd pages && npm ci && npm run check` green; root `deno task fmt:check` and
  `deno task lint` green; Playwright smoke green.
- `git diff --exit-code main -- pages/public/architectures/mansio-current.json pages/public/architectures/mansio-lanes-proposal.json pages/public/architectures/mansio-planes-proposal.json pages/content/mansio-current pages/content/mansio-lanes-proposal pages/content/mansio-planes-proposal` (or checksums) proving the existing datasets are unchanged.
- List the new dataset's diagram kinds and which M each diagram/document realizes.

## Commits

On `item/dp-02-fable-planes`: the brief (copy this file to
`agent-work/items/dp-02-fable-planes/brief.md`, `git add -f` if ignored), then ONE
implementation commit `pages: add mansio-planes-fable dataset (Fable revision of the planes
proposal, M1–M8 applied)`; trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
Push the branch; do NOT merge to main and do NOT touch main — Twin verifies and lands.
End your final message with: commit SHAs, acceptance summary, the dataset's diagram list,
and anything you deliberately left out (and why). Leave the worktree in place.
