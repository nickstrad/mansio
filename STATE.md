# mansio — project state

## 2026-11-07 — Go-platform C4 dataset build

- Goal/spec: Nick's “Make c4 diagrams for mansio with Astra and have opus do final pass”; build brief for
  `mansio-go-platform`, baseline `6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc`.
- Status: bundle built on `feat/c4-dataset`, awaiting **Opus final pass and coordinator landing**. No merge.
- Created: `pages/public/architectures/mansio-go-platform.json` and
  `pages/content/mansio-go-platform/inventory.md`; 30 elements, 48 relationships, nine views, 24 pinned
  sources. Catalog registration is additive; the four earlier bundles and default remain unchanged.
- Evidence/learning: `agentd` is the Go server and VM daemon; `edge` is a separate proxy. Handlers and their
  retries run in the caller's SDK. S3 is implemented but paused, explicitly labeled throughout. The starting
  server C4/README documents cover sqlbench only. The inventory preserves these findings and source mappings
  within this build's ownership; no reusable script or additional knowledge-file edit is needed.
- Validation: Node 22 Pages `npm run typecheck`, `npm test` (**271 passed, five files**) and `npm run build`
  passed. `deno fmt --check --config deno.json` passed on all seven changed files. Framework `decodeDataset`
  and nine `projectView` calls pass; references, parent chains, dynamic steps and all 24 pinned paths pass
  independent checks. All nine desktop views and narrow context were rendered and inspected; compact cards
  retain full notes in documents.
- Extra browser check: initial full run was interrupted (exit 143). Rerun: **51 passed, one timeout** in the
  unchanged planes-proposal containers view; that exact test then passed alone (**1 passed**). No test was
  weakened. Full browser green is not claimed. The four earlier bundles are byte-identical to baseline.
- Next: coordinator assigns a fresh Opus final pass, then lands on PASS. Review/landing owner: coordinator. No
  architectural decision is pending from this build. Build changes and learning notes are ready to commit and
  push together; use the branch HEAD as the build checkpoint.
- Resume: `/root/Software/mansio-work/c4-dataset`, branch `feat/c4-dataset`; use `git log -1` for build SHA
  and compare `git ls-remote origin refs/heads/feat/c4-dataset` with HEAD. No detached agent needs
  relaunching. Rerun Node 22 `npm run typecheck`, `npm test`, `npm run build` in `pages/`; run Deno format
  checks with `--config deno.json` from the root. Final report: `/root/runs/mansio-c4-astra-report.md`.

## 2026-11-06 — cache-dividend topic proposals (docs only)

- Goal/spec: Nick's brief "propose cache-dividend video topics from the mansio repo": survey the repo and
  write ranked feature-explainer proposals for the cache-dividend series (short narrated videos that discuss a
  repo's code and design and never run it).
- Status: `docs/cache-dividend-topics.md` written on branch `docs/cd-topics` (worktree
  `/root/Software/mansio-work/cd-topics`); 12 ranked topics, each with evidence citations, the house-format
  angle, what the viewer sees, and an implemented-vs-design-stage caveat; top 3 named at the end. Docs-only:
  nothing under `core/`, `sdk/`, `server/`, `tests/` or `pages/` changed.
- Awaiting: **Nick's picks** (which topics, in what order). Landing the branch is the coordinator's job.
- Note for the next reader: the brief described multi-node scale-out as proposal-stage; the survey found the
  Go multi-node path implemented and exercised (IR-5a `9dc62fc`, DST-2/3 `339f6aa`, e2e `scale_out`), while
  the `pages/` lanes/planes datasets remain proposals and the S3 write-back is paused (`7cf4ba6`). The doc
  says so per topic.
- Resume: `git log -1 -- docs/cache-dividend-topics.md` for the SHA; edit the doc in place for revisions and
  keep `deno fmt --check docs/cache-dividend-topics.md` green.

## 2026-11-06 — Pages rename repair (ts-db → mansio)

- Goal/spec: fulfill Nick's “fix the mansio GitHub Pages deploy after the ts-db → mansio rename” brief;
  workflow: `.github/workflows/pages.yml`; acceptance: unchanged source checks and passing Pages build.
- Status: rename repair landed on main (merge 5ea8b27, fix commit 0a2424d, Opus review PASS WITH NITS); Pages
  run 42248090392 (on eb46a36) concluded success; https://nickstrad.github.io/mansio/ verified live (HTTP
  200).
- Context: repository renamed `nickstrad/ts-db` → `nickstrad/mansio`; vm-1 checkout moved
  `/root/Software/ts-db` → `/root/Software/mansio`; origin is `git@github.com:nickstrad/mansio.git`.
- Done: reproduced run 42233396362's failure at base `de7d7594c33e7b9b816df0b050a1ff43505282d8`: 6 failed /
  234 passed. Six sources in the planes proposal/fable bundles had renamed `path` fields but old `tsdb-*` URL
  tails at pre-rename commits. Deployment configuration was already correct.
- Fix: re-pin proposal's `src-lanes` / `src-lanes-inventory` and fable's same two plus `src-planes` /
  `src-planes-inventory` to rename commit `d8e29b678d4b60e16658d892bed4ade2c8deb86f`, with `mansio-*` URL
  tails; update the six matching inventory hrefs. Other sources and tests remain unchanged. This commit
  contains the renamed files; changing URL tails at the old pins would break the links.
- Validation: Pages `npm run typecheck`, `npm test` (240 passed, 5 files), and `npm run build` passed. All six
  URL paths exist at their revisions (`git cat-file -e`); revisions, paths and inventory hrefs agree. Other
  source records are unchanged. Build output is ignored/untracked in this checkout and is excluded from the
  commit.
- In progress: none for this repair.
- Broader checks: Deno formatting/lint/typechecking passed, but `deno task ci` exited 139 during ingestion
  property tests (log: `/tmp/mansio-fix-pages-ci.log`). Go race tests reported failures in `cmd/agentd`,
  `internal/bench`, `internal/ir`, and `internal/objstore`; these files are unchanged. These wider checks are
  not green and require a separate coordinator investigation.
- Next: none for this repair. The wider Deno/Go check failures noted below remain a separate investigation.
- Open decisions/blockers: none for this repair. Review/landing belongs to the coordinator.
- Resume: worktree `/root/Software/mansio-work/fix-pages`, branch `fix/pages-after-rename`. Use `git log -1`
  for the repair SHA and `git status --short --branch` for push/worktree status. No detached agent run is
  needed; rerun the three npm commands in `pages/` if validation is needed.
- Learning assessment: preserve the rename/pinned-path gotcha here within the brief's authorized scope; no
  reusable script or additional knowledge-file changes are needed.
