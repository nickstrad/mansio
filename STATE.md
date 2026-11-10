# mansio — project state

## 2026-11-10 — backpressure fixes B1–B11 (Fable 5.2 build + simplify + merge, Nick-directed)

- Goal/spec: brief `/root/briefs/mansio-backpressure-fixes-brief.md`; findings and order in
  `/root/Software/book-notes-work/backpressure-plan/books/backpressure-in-microservices/APPLICATION-PLAN.md`
  (section 3: five waves). One commit per finding, then a simplify pass, then merge to main from
  `/root/Software/mansio-work/main-merge` (never the main checkout).
- Status: **in progress — wave 1 done, wave 2 next (B2).**
- Per finding:
  - B1 (wave 1, See): **done** `0277e36`. `mansio_queue_capacity` / `mansio_queue_rejected_total{reason}` /
    `mansio_queue_wait_seconds` per fixed `queue` label, actor depth in durable mode, `mansio_vactor_dirty`,
    controld pgx pool stats and admin-API refusal count. Catalog:
    `server/knowledge/front/2026-11-05-metrics.md`. Overload scenario: `TestOverloadIsVisiblePerQueue`
    (`server/cmd/agentd/metrics_test.go`).
  - B2, B3, B4, B5 (wave 2), B6, B7 (wave 3), B8, B11 (wave 4), B10 (a), B9 event-log retention (wave 5): not
    started.
  - B10 (b) `max_dirty_age` and B9's warm-set default: **skipped-gated** (Nick's paused LiteFS / write-back
    decision); not to be implemented in this run.
- Baseline (clean worktree at `e9488c5`, `go test -race`): two failures exist before any change and are not
  this branch's: `internal/ir` `TestIndexNamesDecodeFast` (a timing assertion) and `cmd/agentd`
  `TestAdminFetchLimitAppliesToTheNextFetches` (504s; fails on every run here). `internal/control/locator`
  `TestLoadTimeoutBoundsALoadNobodyWaitsFor` hung once for the 10 min test timeout and passed on rerun
  (flaky).
- Verification so far (B1): `gofmt -l .` clean, `go vet ./...` ok; `go test -race` ok on `internal/metrics`,
  `actor`, `front`, `vactor/...`, `tenancy/...`, `agentglue`, `sim`, `det`, `cmd/controld`; `cmd/agentd` has
  only the baseline failure; same packages under `-tags noassert`.
- Resume: worktree `/root/Software/mansio-work/backpressure-fixes`, branch `feat/backpressure-fixes`;
  `git log
  --oneline main..HEAD` shows which findings are committed (B-number in each subject). Nothing runs
  in the background. A scratch baseline worktree may exist at `/root/Software/mansio-work/bp-baseline`
  (detached `e9488c5`; safe to `git worktree remove`).

## 2026-11-10 — `make platform-up` / `make platform-down` (Opus build, Fable final pass)

- Goal/spec: MANSIO-REFACTORS item 1
  (`/root/Software/book-notes-work/backpressure-plan/books/backpressure-in-microservices/MANSIO-REFACTORS.md`),
  brief `/root/briefs/mansio-platform-up-brief.md`: one command builds the daemons, generates a dev CA, ticket
  keys and secrets, starts Postgres, the object store, controld, two agentd and the edge, registers a schema,
  creates a tenant and key, and runs one SDK write and read through the edge; `platform-down` removes it.
- Status: **Fable 5.2 final pass: PASS WITH FIXES** on `feat/platform-up` (Nick directed 2026-11-09 that this
  pass replaces the Astra review for this build only); ready for coordinator landing. No merge, no push.
- Built: `9fb5592` `controld keygen -dir DIR [-public FILE]` (keygen home: **controld**, the ticket issuer; it
  seeds an empty `ticket_keys.dir` in the exact format controld serves from — a fresh ring signs with its only
  key at once — and optionally writes the public key set a static agentd/edge takes as `keys_file`; refuses a
  ring that already holds keys; test `cmd/controld/keygen_test.go`). `c5a4790` sample configs
  (`agentd/durable-local.json`, `agentd/durable-pg.json`, `controld/local.json`) now use bucket `mansio-test`,
  the one `make s3-init` creates. `5688317` `make platform-up` / `make platform-down` →
  `scripts/platform/{up.sh,down.sh,lib.sh,compose.yml,smoke.ts}`, `deno task platform:smoke`, `.platform/`
  gitignored. Assembled from `server/scripts/soak/run.sh` (TLS from a throwaway CA, config shapes, process
  handling) and `tests/e2e/platform/cluster.ts` (api-key tenancy, schema registry, SDK locator client).
- How it works: own Compose project `mansio-platform` (Postgres `127.0.0.1:15432`, SeaweedFS S3
  `127.0.0.1:18333`, project-scoped volumes), so it runs beside the root `make up` stack (5432/8333) and never
  collides with it. Daemons on loopback HTTPS from the dev CA: controld 27080, vm-a 27070-2, vm-b 27073-5,
  edge 27443/27444; durable agents on bucket `mansio-test` prefix `platform/`, Postgres placement schema
  `control`. State (binaries, CA, keys, secrets, configs, caches, logs, pid files) in `.platform/`; daemons
  are `setsid`-detached and outlive `make`. Proof: `smoke.ts` (Deno, `DENO_CERT` = the dev CA) registers
  `tic-tac-toe`, creates a tenant + API key via `/admin/tenants`, `createGame` then `getGame` through the
  locator route and the edge, and fails unless the document read back has the written id and players; then
  up.sh prints the edge's own `mansio_edge_agent_answers_total{status="2xx"}` count.
- Repeat behaviour: `make platform-up` while up rebuilds/restarts nothing and re-runs the smoke proof (new
  tenant each time). Leftover `.platform/` with any daemon not running: refuses and asks for
  `make platform-down`. Any failure after the start: prints daemon log tails and runs down.sh, so a failed up
  leaves nothing behind (exercised: the first run failed on an edge config error and tore down cleanly).
  `platform-down` is idempotent; it deletes `.platform/` only if it carries the `.mansio-platform` marker and
  kills only pids whose `/proc/<pid>/exe` is in `.platform/bin/`.
- Verified (2026-11-10, this VM): `make platform-up` from clean → exit 0, ~21 s,
  `platform smoke OK: wrote and read back game 01a1c334-da95-… (xPlayer alice-52cf15fe) via https://127.0.0.1:27443`,
  edge relayed 2 2xx answers; second `make platform-up` while up → exit 0, ~1 s, new tenant, read back
  `alice-e841f476`, edge count 4; `make platform-down` → exit 0, `.platform/` gone, 0 `mansio-platform`
  containers/volumes, other containers (`tsdb-postgres-1`, `tsdb-seaweedfs-1`) and volumes (`pg-data`,
  `seaweedfs-data`, …) unchanged (diffed before/after); up → down → up → down repeated twice, all exit 0;
  stale-state refusal checked by killing the edge. Checks: `gofmt -l` clean, `go vet ./cmd/controld/`,
  `go test -race ./cmd/controld/` and `-tags noassert` ok; `deno task fmt:check`, `lint`, `check` exit 0;
  `deno task test` 317 passed / 0 failed; `shellcheck -x scripts/platform/*.sh` clean. Full
  `go test -race ./...` and `deno task ci` property tests were not re-run (only `cmd/controld` and sample JSON
  configs changed in Go; the earlier section records their pre-existing failures).
- Deviations: (1) the platform does not reuse the root `docker-compose.yml` stack: platform-down must remove
  what it created without touching a developer's `make up` data, and on this VM 5432/8333 are already held by
  another stack — so it has its own project and ports. (2) controld and the agents still run with `-dev`, only
  to allow plain-HTTP S3 to the loopback SeaweedFS; every mansio listener is HTTPS. (3) The agents and edge
  fetch keys from controld's `/v1/keys`; the `-public` key-set file is written
  (`.platform/ticket-keys.public.json`) but not consumed by this topology. (4) `frontbench keygen` is left in
  place (the bench uses its issuer file format).
- Final pass (Fable 5.2, 2026-11-10, reviewed build head `c4043df` against base `394fce7`): all five
  acceptance items verified by re-running, not from the build's report. Fix commit `b1a2a6b`: (a) two agentd
  starting together both tried to create the missing bucket and the loser logged `S3.BUCKET_CHECK_FAILED`
  (WARN, "you already own it") on fresh runs — `up.sh` now creates `mansio-test` in SeaweedFS (`weed shell`)
  before any agent starts; (b) a relative `PLATFORM_DIR` was used after `cd`, so the build wrote under
  `server/<dir>`, up exited 127 and down left that directory behind — `lib.sh` now makes it absolute. Evidence
  (clean run, teardown and relative-dir run after the fixes; repeat, refusal and failure paths on the build
  head): `make platform-up` from clean → exit 0, ~16 s,
  `platform smoke OK: wrote and read back game 01a1c35d-b089-…
  (xPlayer alice-07510919) via https://127.0.0.1:27443`,
  edge 2 2xx answers, 0 WARN/ERROR lines in any daemon log, both agents `S3.PROBE_OK`, write-back landed in
  the bucket (41 KB after 25 s); independently of the SDK: `control.placement` row ACTIVE on vm-a,
  `OWNER.CLAIMED` in vm-a's log, the SQLite file in vm-a's cache. Up while up → exit 0, ~1 s, same pids and
  key id, second tenant, edge count 4. Edge killed → up refuses (make exit 2), state untouched. Port already
  held (`PLATFORM_S3_PORT=8333`) → refuses before creating anything, `tsdb-*` untouched. vm-b killed mid-start
  → "vm-b exited before it was ready", log tails, full teardown. `make platform-down` → exit 0 (and again with
  nothing up); `.platform/` gone, 0 `mansio-platform` containers/volumes/networks, no listeners on the
  platform ports, `docker ps -a` / volume / network lists identical to the pre-run snapshot. Four up/down
  cycles in total, all exit 0. Checks re-run: `gofmt -l
  cmd/controld` clean, `go vet`,
  `go test -count=1 -race` and `-tags noassert` on `./cmd/controld/` ok, `go build ./...` ok;
  `deno task fmt:check`, `lint`, `check` exit 0; `deno task test` 317 passed / 0 failed;
  `shellcheck -x scripts/platform/*.sh` clean. Not re-run: full `go test -race ./...` and
  `deno task test:prop` (same reason as the build: no Go outside `cmd/controld` changed).
- Known, left as is: (1) the Compose project name is fixed, so `make platform-down` from a second checkout
  removes the containers of a platform started from another checkout (its daemons and `.platform/` stay; up
  from a second checkout refuses on the held ports). (2) The bucket race itself is in agentd
  (`objstore.EnsureBucket` treats "already owned by you" as a failure; cosmetic, the probe passes) — a server
  follow-up, outside this build's scope.
- Next: coordinator lands `feat/platform-up`; then item 2 wires `make platform-up` / `make platform-down` as
  the CI end-to-end job (needs go, docker compose v2, openssl, curl, jq, deno 2.9.5 on the runner). Open
  decisions/blockers: none.
- Resume: worktree `/root/Software/mansio-work/platform-up`, branch `feat/platform-up`. Nothing is running
  (`ls .platform` absent, `docker ps | grep mansio-platform` empty); if a platform is found up,
  `make platform-down` removes it. Logs of a live platform: `.platform/<controld|vm-a|vm-b|edge>/*.log`.

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
