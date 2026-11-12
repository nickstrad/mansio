# mansio architecture diagrams site

An interactive C4 viewer for mansio, meant to be used **while designing a feature**. It shows one
architecture: **what runs on `main`**. That means the Go control plane (controld, Postgres placement), the Go
data plane (the edge, agentd and its SQLite files, the paused S3 write-back) and the TypeScript SDK in the
caller process, drilled from the system context down to components and the key flows. Every element traces to
a commit-pinned source.

Live at <https://nickstrad.github.io/mansio/>, deployed by `.github/workflows/pages.yml` on every push to
`main`.

## What it shows

The catalog has one entry, `mansio-go-platform` (bundle `public/architectures/mansio-go-platform.json`,
inventory [`content/mansio-go-platform/inventory.md`](content/mansio-go-platform/inventory.md)), pinned to
`main` `bffb5e9`:

| Kind      | Views                                                                                                  |
| --------- | ------------------------------------------------------------------------------------------------------ |
| Context   | `context`                                                                                              |
| Container | `containers` (request path), `observability` (JSONL → offline DuckDB)                                  |
| Component | `caller-components` (SDK + read cache), `components` (agentd), `storage-events`, `control-plane`       |
| Dynamic   | `query-flow` (resolve once, then the cached-ticket data path), `database-move` (fence before the flip) |

There is no landscape, code or deployment view: `main` has no deployment the dataset could assert, and code
views are not drawn.

On 2026-11-11 the site was cut down to `main` only. The Deno-era `mansio-current` dataset (the platform
deleted at IR-6) and the scale-out proposals (`mansio-lanes-proposal`, `mansio-planes-proposal`,
`mansio-planes-fable`) were removed; they stay in git history, and the design documents in
`docs/architecture/` that cite them are history.

## Keeping main in sync

The dataset describes `main` at the commit in its `revision` and source pins. When a merge to `main` changes
something the diagrams draw or describe (a process, a component's duty, a relationship, a flow, or a claim in
a document such as "paused" or "open issue"), refresh it in the same change or right after:

1. List what changed since the pin: `git log --first-parent <pin>..main` and
   `git diff --stat <pin>..main -- server sdk core intermediate-representation eventlog`; the merge messages
   and the notes in `server/docs/notes/` summarise each merge.
2. Update elements, relationships, documents (both `plain` and `ste`) and diagrams; add a source for each new
   piece of evidence.
3. Re-pin every source to the new commit (`url` and `revision`; check each path with
   `git cat-file -e <sha>:<path>`), and update the bundle `title`/`revision` and the catalog entry `title`.
4. Record the refresh in the inventory's conversion notes and add every new element and relationship id to its
   mapping tables.
5. Run the checks below.

### The dropdown and the sticky bar

The viewer keeps the catalog mechanism, so the bar still holds a labelled dropdown (now with one entry) and
the SDK's diagram navigation (Back, Up, breadcrumbs and related views), pinned to the top of the window. URLs
are the SDK's deep links, `#/architectures/<entry id>/diagrams/<diagram id>[/elements/<id>]`. An empty hash
opens the catalog default. Unknown architectures, unknown diagrams and malformed links show Not Found; nothing
falls back silently.

### `defaultId`

`public/catalog.json` is the SDK catalog plus one optional site-level key:

```json
{
  "defaultId": "mansio-go-platform",
  "entries": [{ "id": "…", "title": "…", "url": "architectures/….json" }]
}
```

`defaultId` names the entry shown when the URL names none; without it the first entry is the default. A
`defaultId` that names no entry stops the site with an error. The SDK's catalog schema is strict, so the site
lifts `defaultId` off the catalog response before the SDK loader reads it (`src/catalog.ts`); any other
unknown key is still rejected.

### Adding an architecture

The site shows `main` only (Nick, 2026-11-11), so do not add proposal datasets without asking. The mechanism
stays in case that changes, and it needs no code changes (the tests prove it with a two-entry fixture
catalog):

1. Write the bundle as `public/architectures/<id>.json` and its conversion inventory as
   `content/<id>/inventory.md` (see [Authoring and verifying a dataset](#authoring-and-verifying-a-dataset)).
   The bundle `id` must equal the catalog entry `id`.
2. Add `{ "id": "<id>", "title": "<dropdown label>", "url": "architectures/<id>.json" }` to `entries` in
   `public/catalog.json`, at the position it should take in the dropdown. Change `defaultId` only if the new
   entry should open by default.
3. Add the new id to the catalog test in `tests/bundles.test.ts`, then run `npm run check`.

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

| Script         | What it does                                                                                                       |
| -------------- | ------------------------------------------------------------------------------------------------------------------ |
| `typecheck`    | `tsc -b` over the viewer and the tests, against the c4-sdk package's declarations                                  |
| `test`         | vitest: every bundle decodes with `decodeArchitecture` and **zero** diagnostics; catalog, routes, views            |
| `build`        | `vite build --base /mansio/`                                                                                       |
| `test:browser` | Playwright against `vite preview` of `dist/`: the one entry, every view renders, sticky bar, deep links, Not Found |

The browser smoke reuses Playwright 1.64.0's Chromium from `~/.cache/ms-playwright` (already installed on vm-1
for c4-sdk); elsewhere run `npx playwright install chromium` once.

## Layout

```text
docs/pages/
  index.html, src/             the viewer; src/sdk.ts is the ONLY module that touches c4-sdk
  public/catalog.json          { defaultId?, entries: [{ id, title, url }] }; entry id = bundle id
  public/architectures/        one ArchitectureBundle v1 JSON per catalog entry
  content/<dataset>/           inventory.md: source node/edge → JSON id → documents, discrepancies, decisions
  tests/                       *.test.ts (vitest), smoke.spec.ts (Playwright)
  tests/fixtures/              the two-entry test catalog and its tiny fixture bundle (never published)
```

Root toolchain constraints that shape this folder: `deno fmt --check` and `deno lint` at the repo root scan
everything under `docs/pages/` except `node_modules/` and gitignored paths, and `deno.json` is frozen, so
every committed file here is fmt- and lint-clean. `.gitignore` ignores directories named `dist/`, `build/`,
`data/`, `store/`, `objects/`, `snapshots/`, `logs/`, `tmp/` and similar **at any depth**: never use those
names for content (bundles live in `public/architectures/`).

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
7. **Verify.** `deno fmt --config deno.json docs/pages/public docs/pages/content` from the repo root (the root
   fmt check covers them; without `--config` deno formats at width 80 because of `docs/pages/package.json`),
   then `npm test`, then look at the result in `npm run dev`.

`mansio-go-platform` is pinned to `main` at `bffb5e9`; see [Keeping main in sync](#keeping-main-in-sync).

Views with no Markdown source follow the same rules. A **dynamic** view reuses model relationships (its edge
numbers are their static numbers), uses `reverse` only for a response, uses `parallel` only for things that
both happen in no given order, and draws each alternative outcome as its own named diagram
(`tests/views.test.ts` checks that every step's relationship has both endpoints in the view).

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
`upload-pages-artifact` and `deploy-pages`. GitHub Pages is enabled with GitHub Actions as the source, so
every push to `main` redeploys <https://nickstrad.github.io/mansio/>.
