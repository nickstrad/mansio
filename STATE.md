# Pages rename repair — 2026-11-06

- Goal/spec: fulfill Nick's “fix the mansio GitHub Pages deploy after the ts-db → mansio rename” brief;
  workflow: `.github/workflows/pages.yml`; acceptance: unchanged source checks and passing Pages build.
- Status: data fix validated locally; repair recorded on `fix/pages-after-rename`; not merged or deployed.
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
- In progress: none; repair ready for coordinator review.
- Broader checks: Deno formatting/lint/typechecking passed, but `deno task ci` exited 139 during ingestion
  property tests (log: `/tmp/mansio-fix-pages-ci.log`). Go race tests reported failures in `cmd/agentd`,
  `internal/bench`, `internal/ir`, and `internal/objstore`; these files are unchanged. These wider checks are
  not green and require a separate coordinator investigation.
- Next: coordinator obtains independent review, then lands the branch and checks the main Pages run.
- Open decisions/blockers: none for this repair. Review/landing belongs to the coordinator.
- Resume: worktree `/root/Software/mansio-work/fix-pages`, branch `fix/pages-after-rename`. Use `git log -1`
  for the repair SHA and `git status --short --branch` for push/worktree status. No detached agent run is
  needed; rerun the three npm commands in `pages/` if validation is needed.
- Learning assessment: preserve the rename/pinned-path gotcha here within the brief's authorized scope; no
  reusable script or additional knowledge-file changes are needed.
