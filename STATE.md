# mansio — project state

## 2026-11-07 — Go-platform C4 dataset (Astra build, Opus final pass)

- Goal/spec: Nick's “Make c4 diagrams for mansio with Astra and have opus do final pass”; bundle
  `pages/public/architectures/mansio-go-platform.json` + `pages/content/mansio-go-platform/inventory.md`,
  describing main `6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc` (revision `main-6330c9f-go-platform`).
- Status: **Opus final pass: PASS with fixes** on `feat/c4-dataset`; ready for coordinator landing. No merge.
- Done: Astra build `119257b` (30 elements, nine views, catalog registration additive, four earlier bundles
  untouched). Opus fix commit `7f561d9`: tags deduped (most elements carried `external` up to eight times,
  styling in-scope components as external) and `external` kept only where the c4-sdk validator requires it
  plus the caller application; four source-backed container relationships added (agentd → controld ticket
  keys, edge → controld VM registry, agentd → placement authority in durable mode, controld → S3 drop,
  built/paused); lifecycle/controld descriptions widened to create/move/suspend/resume/drop/restore;
  durable-mode-only lifecycle RPC labelled; two sources pinned (`server/cmd/agentd/durable.go`,
  `server/internal/control/ports.go`). Now 30 elements, 52 relationships, 26 sources.
- Verified by Opus against source at the pin: move protocol (`server/internal/owner/move.go`), owner
  admission/fence/stamp (`owner/state.go`), actor/mailbox/group commit/read pool (`internal/actor/actor.go`),
  request ids in `_mansio_requests` inside the write savepoint (`actor/reqid.go`), controld duties
  (`cmd/controld/main.go`, `internal/control/controller.go`, `control/locator/doc.go`), edge
  (`cmd/edge/main.go`), agentd modes (`cmd/agentd/main.go`, `control.go`, `durable.go`), IR route
  (`internal/front/ops.go`), SDK re-run/unguarded find/transport (`sdk/recording.ts`, `sdk/transport.ts`),
  JSONL sink (`internal/eventlog/log.go`), S3 pause
  (`server/knowledge/writeback/2026-11-06-backup-paused-open-issues.md`). S3 reads as built-but-paused
  everywhere; no lanes/planes design material appears as built. All 26 source paths pass `git cat-file -e` and
  URL tails match.
- Validation after fixes: Node 22 `npm run typecheck`, `npm test` (**273 passed, five files**),
  `npm run build` green; `decodeArchitecture` no diagnostics; framework `decodeDataset` + nine `projectView`
  pass; `deno fmt --check --config deno.json` clean on touched files. Scratch proof project re-pinned to
  `7f561d9`, `c4v build` passed, all nine views re-rendered: `/root/runs/mansio-c4-opus-stills/manifest.md`.
- Next: coordinator lands `feat/c4-dataset`. Open decisions/blockers: none. Owner: coordinator.
- Resume: worktree `/root/Software/mansio-work/c4-dataset`, branch `feat/c4-dataset`; compare
  `git ls-remote origin refs/heads/feat/c4-dataset` with HEAD. Nothing running. Reports:
  `/root/runs/mansio-c4-astra-report.md`, `/root/runs/mansio-c4-opus-report.md`.

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
