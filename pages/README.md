# mansio architecture diagrams site

An interactive C4 viewer for mansio, meant to be used **while designing a feature**: N architectures in one
dropdown, the current one (what runs on `main`) by default, each drilled from the system context down to
containers, components and code. It is not a replacement for the Markdown diagrams in
[`docs/architecture/`](../docs/architecture/README.md) (the audited source of truth for the current system);
it renders converted copies of them, with every element traceable to a commit-pinned source.

The `mansio-lanes-proposal` entry covers only the unbuilt scale-out design (statement lanes, the placement
table, the forwarder, the lane supervisor, the peer node and WAL frames): its context, containers, the inside
of one lane, the four key flows and an illustrative two-node deployment. The FFI execution layer (FFI-01 to
FFI-08: binding, classifier, deadline and 504) has landed on `main` and is part of `mansio-current`.

Future URL: <https://nickstrad.github.io/mansio/> (GitHub Pages is **not enabled yet**; see
[Deploying](#deploying)).

## What it shows

Today the catalog lists four architectures:

| Catalog entry            | Content                                                                                                                                                                |
| ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `mansio-current`         | The default: the four audited pairs of `docs/architecture/`, a landscape and a code view                                                                               |
| `mansio-lanes-proposal`  | Statement lanes and placement: context, containers, lane components, flows and a deployment                                                                            |
| `mansio-planes-proposal` | Control plane / data plane: tenants own 0..N databases; database-scoped placement, isolation and lifecycle                                                             |
| `mansio-planes-fable`    | Fable revision of the planes proposal: route grants, lane view, default database, VM-loss recovery, backup manifests, instance moves, candidate placement rule (M1–M8) |

The `mansio-planes-proposal` addition supersedes one-file-per-tenant **in this proposal only**. It adds
control/data-plane component views, create/query/move/drop scenarios with separate failure alternatives, and
an illustrative uneven three-VM deployment: n SQLite files across m VMs. Routing and the placement registry
belong to the control plane; the gateway consults them. Every VM runs **VM agent (process)** as the last mile
for all query and lifecycle file work. The control plane chooses each database’s VM, but the placement policy
remains an explicit open question. Existing scale-out content is preserved. Its
[inventory](content/mansio-planes-proposal/inventory.md) records the database-aware router extension, fencing
protocol, fail-closed online admission, service trust boundaries, bounded resource budgets and the distinction
between landed FFI code and proposed lane processes. The data overview links to agent execution detail.
Backup/VM-loss flow expansion and source/target move instance companions are explicitly deferred in the
inventory and documents. Registration adds one entry to `public/catalog.json`; no viewer code changes.

The `mansio-planes-fable` entry (DP-02) is the planes proposal with the eight modifications from
[`docs/architecture/scale-out-comparison-fable.md`](../docs/architecture/scale-out-comparison-fable.md) §4
applied, generated from the DP-01 generator plus a patch
(`agent-work/items/dp-02-fable-planes/build-dataset.py`), so everything an M-number does not name is carried
over unchanged. M1 replaces the per-request single-use decision with a time-boxed **route grant** held by the
gateway (new `grant-cache`, a grant-reuse flow with no control-plane call, and a stale-route retry that
obtains a _fresh decision_); M3 adds the lane view inside the agent with backups on a read-only snapshot
connection; M4 specifies the database selection surface and the per-tenant `default` database; M5 closes the
VM-loss deferral (operator-attested isolation with provider proof, then restore elsewhere; refusal without
attestation); M6 makes backup completion a recorded manifest keyed by (database, generation); M7 adds
instance-level move companions (drawn as illustrative deployment views, because c4-sdk places deployment
instances only there); M8 draws a candidate v1 placement rule labelled **proposed, pending Nick's decision**;
M2 (build order) is recorded in documents only. It is a proposal: everything new is `planned`, and the landed
FFI pieces keep the planes dataset's `existing`/`mixed` marks. M1 and M5 alter contracts Astra reviewed in
DP-01, so they need Nick's word and a re-review. Its [inventory](content/mansio-planes-fable/inventory.md)
records F1–F11. The three older datasets are byte-identical.

### The architecture dropdown and the sticky bar

A bar pinned to the top of the window (`position: sticky`) holds a labelled dropdown (a native `<select>`,
reachable with Tab) that lists every catalog entry by its `title`, in catalog order, and the SDK's diagram
navigation (Back, Up, breadcrumbs and related views). Switching architecture or going back therefore works
from anywhere in a tall diagram page. When the navigation is wider than the bar it scrolls sideways inside the
bar instead of making the bar taller.

Choosing an entry opens its bundle's `defaultDiagramId` through the SDK hash route port, so it is a normal
history entry: Back (the bar's button or the browser's) returns to the previous architecture. One architecture
is shown at a time. **There is no comparison view for now** (Nick, 2026-10-14): entries are peers, and no code
names a "current" or a "proposed" entry; "current" is only the catalog default.

URLs are the SDK's own deep links, `#/architectures/<entry id>/diagrams/<diagram id>[/elements/<id>]`. An
empty hash opens the catalog default. Unknown architectures, unknown diagrams and malformed links show Not
Found (with a link to each architecture); nothing falls back silently.

### `defaultId`

`public/catalog.json` is the SDK catalog plus one optional site-level key:

```json
{
  "defaultId": "mansio-current",
  "entries": [{ "id": "…", "title": "…", "url": "architectures/….json" }]
}
```

`defaultId` names the entry shown when the URL names none; without it the first entry is the default. A
`defaultId` that names no entry stops the site with an error. The SDK's catalog schema is strict, so the site
lifts `defaultId` off the catalog response before the SDK loader reads it (`src/catalog.ts`); any other
unknown key is still rejected.

### Adding an architecture

No code changes (the tests prove it with a three-entry fixture catalog):

1. Write the bundle as `public/architectures/<id>.json` and its conversion inventory as
   `content/<id>/inventory.md` (see [Authoring and verifying a dataset](#authoring-and-verifying-a-dataset)).
   The bundle `id` must equal the catalog entry `id`.
2. Add `{ "id": "<id>", "title": "<dropdown label>", "url": "architectures/<id>.json" }` to `entries` in
   `public/catalog.json`, at the position it should take in the dropdown. Change `defaultId` only if the new
   entry should open by default.
3. Add the new id to the catalog test in `tests/bundles.test.ts`, then run `npm run check`.

### Diagram kinds per dataset

All seven C4 kinds are drawn across the catalog (`tests/views.test.ts` checks it). The
`mansio-planes-proposal` entry has context, container, three component views, nine dynamic scenarios and one
illustrative deployment; `mansio-planes-fable` has context, container, four component views (adding the lane
view), fifteen dynamic scenarios and three illustrative deployment views (the fleet plus the two
instance-level move companions). Neither adds a landscape or code view. The original two entries retain these
views:

| Kind       | `mansio-current`                                  | `mansio-lanes-proposal`                                            |
| ---------- | ------------------------------------------------- | ------------------------------------------------------------------ |
| Landscape  | `landscape` (shared by both)                      | none: the proposal adds no person or system                        |
| Context    | `context`                                         | `lanes-context`                                                    |
| Container  | `containers-platform`, `containers-observability` | `lanes-containers`                                                 |
| Component  | `components-server`                               | `lanes-components` (one statement lane)                            |
| Code       | `code-document-reader`                            | none: no code-level elements are drawn for unbuilt code            |
| Dynamic    | none                                              | the four key flows and their named alternatives (`flow-*`)         |
| Deployment | none                                              | `lanes-deployment` (illustrative two-node layout, not the droplet) |

## Run, build, check

Node 22.23.2 and npm 10.9.8 (the versions c4-sdk targets). This is an npm project of its own; the repo root
stays Deno.

```sh
export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:$HOME/.deno/bin:$PATH"
cd pages
npm ci
npm run dev          # http://localhost:5173/
npm run build        # dist/ at base /mansio/ (gitignored)
npm run preview      # serve dist/ at http://localhost:4173/mansio/
npm run check        # typecheck + unit tests + build + browser smoke
```

| Script         | What it does                                                                                            |
| -------------- | ------------------------------------------------------------------------------------------------------- |
| `typecheck`    | `tsc -b` over the viewer and the tests, against the c4-sdk package's declarations                       |
| `test`         | vitest: every bundle decodes with `decodeArchitecture` and **zero** diagnostics; catalog, routes, views |
| `build`        | `vite build --base /mansio/`                                                                            |
| `test:browser` | Playwright against `vite preview` of `dist/`: dropdown, four entries, sticky bar, deep links, Not Found |

The browser smoke reuses Playwright 1.64.0's Chromium from `~/.cache/ms-playwright` (already installed on vm-1
for c4-sdk); elsewhere run `npx playwright install chromium` once.

## Layout

```text
pages/
  index.html, src/             the viewer; src/sdk.ts is the ONLY module that touches c4-sdk
  public/catalog.json          { defaultId?, entries: [{ id, title, url }] }; entry id = bundle id
  public/architectures/        one ArchitectureBundle v1 JSON per catalog entry
  content/<dataset>/           inventory.md: source node/edge → JSON id → documents, discrepancies, decisions
  tests/                       *.test.ts (vitest), smoke.spec.ts (Playwright)
  tests/fixtures/              the three-entry test catalog and its tiny third bundle (never published)
```

Root toolchain constraints that shape this folder: `deno fmt --check` and `deno lint` at the repo root scan
everything under `pages/` except `node_modules/` and gitignored paths, and `deno.json` is frozen, so every
committed file here is fmt- and lint-clean. `.gitignore` ignores directories named `dist/`, `build/`, `data/`,
`store/`, `objects/`, `snapshots/`, `logs/`, `tmp/` and similar **at any depth**: never use those names for
content (bundles live in `public/architectures/`).

## Authoring and verifying a dataset

A dataset is one `ArchitectureBundle` v1 JSON file (c4-sdk `docs/plans/implementation.md` §3; the decoder in
c4-sdk is the oracle) plus a conversion inventory.

1. **Pin the source.** Every `sources[].url` is
   `https://github.com/nickstrad/mansio/blob/<40-hex commit>/<path>` with `revision` = that commit (the test
   enforces it). Read unmerged branches with `git show <sha>:<path>`.
2. **Map, don't redraw.** Each source box becomes one element (one element per running thing across views),
   each numbered source edge one relationship with `displayNumber` = the source number. Labels stay verbatim;
   `technology` carries the protocol. Use the source's boundaries as groups and mirror its key in the legend.
3. **Status.** `existing` only for what is on `main`; `planned` for what is not; `mixed` (with `plannedNotes`)
   for an existing element or relationship gaining planned duties. Re-check "planned" source claims against
   `main`: what has landed since is `existing`, with a note in the inventory.
4. **Abstraction.** A library (such as `libsqlite3`) is an implementation dependency of a process, not a
   container. Timing numbers go into documents with provenance, never into labels. Do not invent code-level
   elements for unbuilt code.
5. **Documents.** Each diagram, and each element whose source has prose, gets a `plain` text and an `ste` text
   (ASD-STE100 Issue 9, `reviewStatus: "draft"`; a draft is not a certification).
6. **Inventory.** `content/<dataset>/inventory.md` lists source node/edge → JSON id → document ids, every
   Mermaid-versus-terminal discrepancy and every conversion decision. The test fails if an element or
   relationship id is missing from it.
7. **Verify.** `deno fmt --config deno.json pages/public pages/content` from the repo root (the root fmt check
   covers them; without `--config` deno formats at width 80 because of `pages/package.json`), then `npm test`,
   then look at the result in `npm run dev`.

`mansio-current` is pinned to `main` at `2d30330` (audit stamp `0b82504`). When the Markdown diagrams change,
refresh it from the new commit: diff the pairs, update elements, relationships, documents and sources, and
re-run the checks.

Views with no Markdown source follow the same rules. A **dynamic** view reuses model relationships (its edge
numbers are their static numbers), uses `reverse` only for a response, uses `parallel` only for things that
both happen in no given order, and draws each alternative outcome as its own named diagram
(`tests/views.test.ts` checks that every step's relationship has both endpoints in the view). The **code**
view copies declarations from `platform/document_reader.ts` at the pinned commit as text; refresh it when that
file changes. The **deployment** is illustrative and every part of it is `planned`.

## The c4-sdk dependency

The site consumes the published [`c4-sdk`](https://www.npmjs.com/package/c4-sdk) package from npm, pinned
exactly (`0.1.0`) in `package.json` and locked in `package-lock.json`. Its only peers are `react` and
`react-dom`; React Flow, ELK, Markdown, zod and the dialog are bundled inside it, so the site does not list
them. Only `src/sdk.ts` imports it, through the package's exports: `c4-sdk` (React components and adapters),
`c4-sdk/core` (decode, routes, types; tests import it too), `c4-sdk/styles.css`, and
`c4-sdk/layout-worker.js?url`, which Vite fingerprints and serves under `/mansio/` for
`createElkLayoutPort(() => new Worker(layoutWorkerUrl, { type: "module" }))`. Never patch the package; adapt
`src/sdk.ts` to it. To upgrade, `npm install --save-exact c4-sdk@<version>`, check its README for changed
exports or peers, then run `npm run check`.

## Deploying

`.github/workflows/pages.yml` runs on pushes to `main` and on manual dispatch: `npm ci`, typecheck, unit
tests, `npm run build`, `dist/index.html` copied to `dist/404.html`, then `configure-pages`,
`upload-pages-artifact` and `deploy-pages`. **GitHub Pages is not enabled on the repository** (the Pages API
answered 404 on 2026-10-15). Enabling it (Settings → Pages → Source: GitHub Actions) is a human step for Nick
or Twin, and the first deploy is externally visible. Until then, the deploy job fails on every push to `main`;
after enabling, use "Run workflow" for the first deploy.
