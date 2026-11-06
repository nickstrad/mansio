# STATE — coldload: SQLite files in object storage → virtual actors

> **ARCHIVED 2026-11-04.** The coldload work is done and committed (branch `coldload`). This file is kept as a
> record; it is no longer updated. Live coordination: `shared_state.md`. Findings: `knowledge/coldload/`.
> To pick the work up again, copy this file back to the repo root as `STATE-coldload.md` and add a status line.

Last updated: 2026-11-04 (newest status at the top of "Status").
Owner: the "coldload" agent. The other agent (sqlbench) owns `STATE.md`. Read both before touching shared files.

## Goal

Measure how fast one Go process can take a SQLite file that lives in object storage (SeaweedFS, S3 API, in
Docker), pull it to local disk, open it, and make a ready-to-serve **virtual actor** for it, for different file
sizes and different numbers of activations at once.

Questions:

1. Time from "activate db X" to "first query answered", split into fetch (S3 GET → local file), open (actor
   start, WAL pragma), first query, and optional warm-up (read every page into the page cache).
2. How that scales with file size (KiB → GiB) and with concurrency (1 → hundreds of activations at once).
3. Throughput: databases activated per second and MB/s from SeaweedFS at each concurrency.
4. Many requests for the same cold database at once must trigger one activation, not many.

## Knowledge folder (shared by both agents) — please use it

`/Users/nick/Software/mansio/server/knowledge/` holds notes and artifacts that should outlast the work of either agent.

- Read `knowledge/AGENTS.md` (rules; `knowledge/CLAUDE.md` points to it) and `knowledge/index.md` (one line per
  topic) before you start work.
- Make one folder per topic, `knowledge/<topic>/`, with a `README.md` (newest finding first) plus notes (`*.md`)
  and small artifacts (report output, CSV extracts, configs used). Large raw data stays in `results/`; link to it.
- Add a line to `knowledge/index.md` for each new topic folder. Edit only your own topic folders.
- Write facts with their evidence: the numbers, the config, the command, the machine, and an absolute date.
- Live plan and status stay in the state files (`STATE.md`, `STATE-coldload.md`), not in `knowledge/`.
- sqlbench agent: please make `knowledge/sqlbench/` for your findings (compare-actor vs global-lock, ramp,
  retry-storm) and add a row for it in `knowledge/AGENTS.md` and `knowledge/index.md`.
- Mine: `knowledge/coldload/`.

## Plan

1. `deploy/seaweedfs/docker-compose.yml`: single-container SeaweedFS (master + volume + filer + S3 on :8333).
2. `internal/objstore`: thin S3 client (minio-go): put file, get to file, list.
3. `internal/vactor`: virtual-actor host. `Get(ctx, key)` returns a running `actor.Actor`; first call activates
   (fetch → open → optional warm), concurrent callers share one activation (singleflight). Passivate =
   close actor + delete local copy. Records per-stage timings.
4. `cmd/coldload`: `seed` (build one template SQLite per size class, upload N copies) and `run` (activate
   databases at set concurrency levels, write `activations.csv` + `samples.csv`).
5. `queries/coldload/report.sql` + `scripts/coldload/*.sh`: DuckDB report.
6. Record findings here and in `knowledge/coldload/`.

## Files I own (the other agent should not edit)

`STATE-coldload.md`, `cmd/coldload/`, `internal/coldload/`, `internal/vactor/` (+ `vactortest/`),
`internal/objstore/`, `configs/coldload/`, `scripts/coldload/`, `queries/coldload/`, `results/coldload/`,
`deploy/seaweedfs/`, `knowledge/coldload/`, `knowledge/testing/`, `knowledge/concurrency-in-go/`.
Shared, maintained by me: `internal/assert`, `internal/errs`, `internal/logx` (see `shared_state.md`).

## Layout (after the 2026-11-04 /simplify refactor)

| Package | Kind | Holds |
|---|---|---|
| `internal/coldload` | domain, no I/O | `Config.Normalize` (only validation), `ParseSize`, `Levels` plan, `RunLevel` (Getter port, Result pattern, panic forwarding), CSV row layout |
| `internal/vactor` | domain + cache dir | virtual-actor `Host` via `Fetcher`/`Opener` ports; singleflight; passivation waits; checked Fetcher wrapper |
| `internal/vactor/vactortest` | test support | fakes with fault switches; `RunFetcherSuite` contract suite |
| `internal/objstore` | adapter | S3 (minio `Core.GetObject`: one GET; buffer free list; `ErrNotFound`) |
| `cmd/coldload` | wiring + adapters | `realMain`/reporter (exit 0/1/2/70/130, JSON log with log IDs), `s3Fetcher`, `actorOpener`, template builder, async recorder, process snapshot |

## Coordination with sqlbench (read this, sqlbench agent)

- **Scope split (user, 2026-11-04):** sqlbench owns everything about actors once they are running in a process:
  how many databases one process holds, per-database cost (memory, goroutines, fds), request throughput and
  latency. coldload owns only the path *object storage → local file → opened actor → first query*: fetch/open/warm
  time by file size and activation concurrency, activation throughput, and one-activation-per-key (singleflight).
  coldload dropped its `many` config (10k databases held at once) because that is sqlbench's question 3.
  The RSS/goroutine/fd columns in coldload's `levels.csv` are only a sanity check, not a per-database cost result.
- **Benchmarks share one machine.** coldload drives SeaweedFS to ~1700% CPU; a sqlbench run at the same time skews
  both. Proposal: before any benchmark run, take a lock dir `results/.bench-lock` (`mkdir` succeeds = you own it;
  write `owner`/`config`/`started` inside; remove it when done; if it exists, wait). coldload's `run.sh` will do
  this after the current runs. Please do the same in `scripts/run.sh`. Unit tests do not need the lock.
- Your `actor` change (GlobalLock removed, `JournalMode` added, default WAL) is fine for coldload: it builds, and
  I rely on WAL default. `sizes-nowarm` may have been built right as `actor.go` changed (19:29:09); effect is one
  extra `PRAGMA journal_mode` read at open (~µs), so no rerun needed.
- WAL shipping (your Next (a)): `internal/objstore` is yours to use. It has `PutFile`, `GetToFile`, `Size`,
  `List`, `DeletePrefix`. If you need e.g. `PutBytes`/ranged GET, tell me via your STATE.md or add it in a new
  file in that package (`internal/objstore/<yours>.go`) without changing existing signatures.

## Shared / borrowed

- **Imports** `internal/actor` (read-only use): `actor.New`, `(*Actor).Start(true)`, `Do`, `Close`, `Config`.
  Please keep those signatures stable, or note a change in `STATE.md` so I can follow.
- Does not import `internal/stats` (own small recorder/snapshot in `cmd/coldload`).
- `go.mod` / `go.sum`: I add `github.com/minio/minio-go/v7`. Only additions, no version changes to existing deps.
- `knowledge/`: shared notes folder for both agents (see `knowledge/AGENTS.md`).

## How to run

```sh
cd /Users/nick/Software/mansio/server
docker compose -f deploy/seaweedfs/docker-compose.yml up -d     # SeaweedFS, S3 on :8333 (key coldload / coldload-secret)
go test -race ./internal/vactor                                 # host tests (singleflight, retry after failure, passivate)
scripts/coldload/run.sh configs/coldload/smoke.json             # seed + run + DuckDB report (~5 s)
scripts/coldload/run.sh configs/coldload/sizes.json [seed|run|all]
scripts/coldload/report.sh results/coldload/<name>
bin/coldload -config configs/coldload/sizes.json plan           # print the plan only
scripts/coldload/cleanup.sh [local|objects|all|fuzz]            # free space (see below)
make help                                                       # coldload-* targets: bench, smoke, test, fuzz, clean, disk
```

Disk hygiene (automatic): `vactor.Host` empties its cache dir on start and removes it on Close (also on Ctrl-C via
`signal.NotifyContext`); `run.sh` runs `cleanup.sh local` on every exit (cache, `*.part`, templates) and refuses
to seed with < 20 GB free on the host or < 12 GB in the Docker VM; `make coldload-bench` ends with
`cleanup.sh all` (objects + SeaweedFS volume) unless KEEP=1. Manual: `cleanup.sh` (default) removes caches, `*.part`, templates (~1.2 GB); `objects` also empties the bucket
(`coldload clean` per config); `all` also runs `docker compose down -v` (SeaweedFS volume, ~9–15 GB when seeded).
It never touches other containers/volumes (there is an unrelated `trinkets-seaweedfs` on this machine).

Configs: `smoke` (3 sizes, tiny), `sizes` (64 KiB → 1 GiB × c = 1, 4, 16, 64, 256, full-scan warm-up, 500 duplicate
callers), `sizes-nowarm` (same objects, no warm-up).


Output per run in `results/coldload/<name>/`: `activations.csv` (one row per Get: fetch/open/first-query/warm/total
ms), `levels.csv` (one row per size × concurrency: act/s, MB/s, RSS/goroutines/fds before and after with all held
active), `run.json`. Templates: `results/coldload/templates/<class>.sqlite` (one per size class; every object in a
class is a byte-identical copy — fine for SeaweedFS, which does not dedupe).

## Design (as built)

- Object key `<prefix>/<class>/db-000000.sqlite`. Template = `meta` + `items(id, k, name, payload 1000 B random)` +
  index on `k`, WAL mode, checkpointed. Files come out ~20% over the nominal size (index + chunked fill).
- `vactor.Host.Get(key)`: activation runs on its own goroutine (a caller's cancel does not fail other waiters);
  stages: S3 GET → `<cache>/<key>.part` → rename → `actor.New(...).Start(true)` → first query via `actor.Do` →
  warm queries → ready. Failed activation is removed so the next Get retries. `Passivate` closes the actor and
  deletes the local copy (unless `keep_local`).
- Runner: per size class × concurrency level, `per_level` distinct keys, c workers, then snapshot (GC'd Go heap,
  RSS via `ps`, goroutines, fds) with all of them active, then passivate all.

## Status

- 2026-11-04 20:00: /simplify + testing refactor done (4 review agents: reuse, simplification, efficiency,
  altitude). Fixed: fetch did HEAD+GET (TTFB was the HEAD's) → one GET; copy buffer was bypassed; Get racing
  Passivate could serve a deleted file; failed open leaked the fetched file (found by `FuzzHostModel`);
  `shared`/dup special cases → `Result{Act, Waited}`; cache cleanup now owned by `Host`; signal via
  `NotifyContext`; errors via `internal/errs`, JSON log with log IDs; contracts via `internal/assert`.
  Tests: all coldload packages pass `-race` and `-tags noassert`; S3 contract suite passes against SeaweedFS.
  Rerunning `sizes` with the fixed fetch path (earlier numbers include an extra HEAD per fetch).
- 2026-11-04 19:36: Done with the first round. `sizes` and `sizes-nowarm` rerun with the async recorder: 7,377
  activations, 0 errors, 0 rows dropped. Write-up: `knowledge/coldload/2026-11-04-sizes/README.md`.
  `many` dropped (sqlbench's scope). `run.sh` takes the shared bench lock `results/.bench-lock`.
  Cleaned up: local caches, templates, SeaweedFS objects and volume (re-seed with `run.sh <config> all`, ~1 min).
- Next (not started, ask the user): (a) cap concurrent fetches by bytes in flight in `vactor` and re-measure;
  (b) `keep_local` local-disk-hit path; (c) lazy/page-level fetch for large files; (d) rerun with more Docker VM
  RAM or native SeaweedFS to remove the 1 GiB rig limit; (e) real S3 for TTFB.

## Findings

Details and tables: `knowledge/coldload/2026-11-04-sizes/README.md` (corrected rerun, 2026-11-04 ~20:10).

- Download is 69–99% of time-to-ready. Open 0.3–0.5 ms, first point query 0.03–0.05 ms at every size; full-scan
  warm-up 0.21–0.27 ms/MiB. At c = 1 ready ≈ 1.2 ms + size ÷ ~250 MB/s. Total bandwidth caps at ~250–265 MB/s
  (SeaweedFS in Docker Desktop); for ≥ 1 MiB, concurrency past ~4 only adds queueing. 64 KiB peaks at ~3,100
  activations/s (c = 16), ~1,100/s at c = 256. 1 GiB ~40–46 MB/s (VM page cache too small). 500 callers on one
  cold 1 GiB db → 1 fetch. First run's numbers had an extra HEAD per fetch (see the write-up's Correction).
