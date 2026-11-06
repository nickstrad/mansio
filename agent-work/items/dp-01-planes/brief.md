# DP-01 — Control-plane / data-plane C4 dataset: tenants own many databases (brief)

Unit: new design item DP-01 in the mansio repo. Builder: Codex Sol (`gpt-6.1-sol`), actor `sol-dp-01`.
Branch `item/dp-01-planes` off `main` `351a51b`. Worktree `/root/Software/mansio-work/dp01` (already created).
Reviewer afterwards: Codex Astra (design review + tweak suggestions), then ONE repair round by Sol.
Date: 2026-10-20. Authorization: Nick, in chat — "Have a codex sol agent make a c4 diagram in mansio in
which there is control plane that does crud for tenants SQLite files and tenants can have arbitrary
amounts of these. And then the data plane is logic that takes queries and executes them in right node
with that tenants SQLite db. If anything seems useful from current scale out diagrams it can use it.
Keep existing scale out diagrams in the codebase too."

## The design idea (Nick's, do not re-decide)

- A **control plane** that performs CRUD and lifecycle operations over tenants' SQLite database
  files. A tenant can own an **arbitrary number** of databases (0..N) — the database, not the
  tenant, is the unit of placement, isolation, backup, and lifecycle.
- A **data plane** that accepts queries and executes each one on the **right node** — the node that
  hosts the addressed database — against that database's SQLite file.
- Product framing: customers simply *have databases* and get **dynamic access to isolated data** —
  any of a customer's databases is reachable through the same access path regardless of which node
  hosts it. This deliberately replaces the "one big SQLite file per tenant" model: no monolithic
  per-tenant file, no tenant-wide lock domain.
- Reuse anything useful from the existing scale-out design (`mansio-lanes-proposal`: placement table,
  forwarder, lane supervisor, statement lanes, WAL frames) — adapted so placement/lifecycle are
  keyed by **database**, not by tenant.

## Orientation (in order)

1. `AGENTS.md`, `CLAUDE.md`, `agent-work/event-log.md`, `agent-work/COORDINATION.md`; `pages/README.md`.
2. `pages/content/mansio-current/inventory.md` and `pages/content/mansio-lanes-proposal/inventory.md` —
   conversion decisions D1–D5 / P1–P5 stand; mirror the datasets' status conventions
   (`existing` / `planned` / `mixed` with notes) and document conventions (plain + STE write-ups).
3. `pages/public/architectures/mansio-current.json` and `pages/public/architectures/mansio-lanes-proposal.json`
   — the bundle shapes that decode; mirror their structure, element/relationship/document grammar, and
   how datasets are registered for the site's architecture dropdown.
4. `docs/knowledge/` for the landed facts the design builds on: the `ControlPlaneRouter` seam in
   current code (Storage and clients unchanged when the router implementation swaps), the FFI
   statement execution layer (classifier, deadlines, interrupt), child-process lanes as isolation.
5. c4-sdk constraints if needed: `/root/Software/c4-sdk` (read-only) fixtures/docs for diagram kinds.

Environment: `export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:$HOME/.deno/bin:$PATH"`.

## Build (one implementation commit on `item/dp-01-planes`)

Owns: `pages/public/architectures/mansio-planes-proposal.json` (new),
`pages/content/mansio-planes-proposal/**` (new), `pages/tests/**` (extend only if the harness requires
per-dataset coverage; never weaken), `pages/README.md` (dataset section only),
`agent-work/items/dp-01-planes/**`. The two existing dataset bundles and their content must remain
**byte-identical** — this dataset is an addition, never a replacement.

Dataset id: `mansio-planes-proposal`. Contents:

1. **System context** and **container** views that make the control plane / data plane split the
   headline structure: control plane (management path) vs data plane (query path) vs the nodes that
   host database files.
2. **Control plane component view**: tenant registry and database registry (CRUD: create/drop
   tenants; create/drop databases for a tenant; configure; inspect), provisioning (place a new
   database file on a node and register its placement), lifecycle operations (move a database
   between nodes, suspend/resume, per-database backup/restore, delete with its data), and the
   metadata store. Design note: the registry/placement metadata lives in Postgres, accessed with
   hand-written SQL (repo rule: no ORM) — say so in the documents, briefly.
3. **Data plane component view**: query ingress/API, authentication and (tenant, database)
   authorization, the placement router (resolves database → node; this is where the landed
   `ControlPlaneRouter` seam pays off), the forwarder, node-local execution through the landed FFI
   layer with statement lanes / classifier / deadlines, and result return. Show how access is
   *dynamic*: a session addresses any of the tenant's databases by name/id and is routed correctly
   without the client knowing the node; placement is resolved at open and refreshed when a
   database moves.
4. **Dynamic diagrams** for at least these flows, each one `dynamic` diagram with a `scenario: Step`
   tree, reusing model relationships: (a) **create database** — control-plane provisioning end to
   end; (b) **query to the right node** — data-plane routing and execution; (c) **move a database**
   — relocation between nodes including how in-flight access is fenced/quiesced and when the
   placement record flips; (d) **drop database** — teardown including file removal and registry
   cleanup. Branching failure paths are separate named scenarios — never fake measurements.
5. **Deployment view** (`environment: "illustrative"`): a small multi-node fleet with databases
   distributed unevenly across nodes (one tenant with several databases on different nodes; one
   node hosting databases of several tenants), marked proposed/illustrative in its documents.
6. **Inventory** at `pages/content/mansio-planes-proposal/inventory.md`: every element/relationship/
   step mapped to its source (this brief, the lanes-proposal sources, or landed code), with the
   design decisions and any discrepancies recorded. Documents: plain + STE write-ups for every new
   diagram and element, and the write-ups must state the core shift plainly: tenant → many
   databases; database = unit of placement/isolation/lifecycle; one-file-per-tenant is superseded
   *in this proposal* (the lanes dataset stays as-is).
7. Status marking: control/data-plane elements are `planned`; landed pieces reused as-is (FFI
   execution layer, router seam) are `existing` or `mixed` per the inventory conventions.
8. Registration: the new dataset must appear in the site's architecture dropdown exactly the way
   the existing two do. If registration is data-driven, adding the bundle suffices; if a code list
   exists, make the minimal registration change and say so in your report. No other viewer changes.

## Acceptance (paste outputs in your report)

- `cd pages && npm ci && npm run check` green; root `deno task fmt:check` and `deno task lint` green;
  Playwright smoke green.
- `git diff --stat` (or checksums) proving both existing dataset bundles unchanged.
- List the diagram kinds in the new dataset and the flows covered.

## Protocol

- Log events via `work-scripts/bash/log-event.sh --actor sol-dp-01 --branch item/dp-01-planes`.
- Commits: the brief (`git add -f` if ignored), then ONE implementation commit
  `pages: add mansio-planes-proposal dataset (control plane / data plane, tenant → many databases)`;
  trailer `Co-Authored-By: Codex Sol <noreply@openai.com>`. Push the branch; never touch `main`.
- Report: SHAs, acceptance outputs, registration mechanism found, design decisions taken, anything
  from the lanes proposal you reused or deliberately dropped (and why), reviewer hand-off notes.
  Leave the worktree in place.
