> ARCHIVED 2026-11-05: workstream done and merged to main. Findings: `knowledge/writeback/`.

# STATE — writeback agent (warm → cold durability for virtual actors)

Last updated: 2026-11-05. Branch `writeback`, worktree `/Users/nick/Software/mansio-server-wt/writeback`.

## Goal

Make vactor's warm → cold path durable: write-back before passivation, periodic write-back (RPO), eviction
policy, per contract 1 in `shared_state.md` (2026-11-05 10:15) and option 4 of mansio's
`docs/architecture/scale-out-direct-actors.md`.

## Plan

1. `internal/durable`: manifest + generation layout, `Store` port, `ErrStale`, pure checks, fake + contract suite. ✅
2. `internal/actor` additive hooks: `Commits()`, `Snapshot(ctx, path)` (VACUUM INTO on the writer). ✅
3. `internal/vactor`: durable activation, `WriteBack`, `Passivate` writes back first, `Discard`, `Stamp`,
   `Tick`/`RunMaintenance`, pure `PlanTick` (idle + LRU budget + RPO triggers). ✅
4. Tests: spec tables, fakes with faults, `FuzzDurableHostModel`, `FuzzPlanTick`, -race regression. ✅
5. `internal/vactor/actordb`: DurableDB over internal/actor + SQLite integration tests. ✅
6. `internal/objstore`: S3 adapter for `durable.Store` (If-Match / If-None-Match), env-gated contract suite. ✅
7. `cmd/wbbench`: write-back time by size, dirty-check cost; numbers in `knowledge/writeback/`. ✅
8. Knowledge: README, WAL-shipping design note, numbers; index line. Simplify pass. Merge to main (lock). ✅

## Status (newest first)

- 2026-11-05: done. Simplify pass; gate green (gofmt, vet, `-race`, `noassert`); fuzz FuzzDurableHostModel
  60 s + 90 s, FuzzHostModel 15 s, FuzzPlanTick 10 s + 15 s, all clean. Coverage: durable 95.6%, vactor 95.3%,
  objstore 59.4% (old S3-only paths need COLDLOAD_S3). SeaweedFS contract suite not run (Docker down).
  Hooks for fencing (`CheckActivation`, `Guard`, `WriteBackAt`, `Stamp`) and front (`actordb.DB.Actor`) posted.
- 2026-11-05: objstore durable adapter + in-process fake S3 (found minio-go AutoChecksum sends no
  x-amz-checksum-sha256 on this path); wbbench numbers (VACUUM INTO ~0.8 GB/s, blocks writes meanwhile).
- 2026-11-05: steps 1–4 done, green (`-race`, `noassert`); FuzzDurableHostModel 90 s / 91k execs clean
  after two model fixes (both model bugs, inputs committed as seeds).

## Files I own

`internal/durable/**`, `internal/vactor/**`, `internal/objstore/**`, `cmd/wbbench/**`, `knowledge/writeback/**`,
`STATE-writeback.md`. Additive edits in `internal/actor/actor.go` + `internal/actor/snapshot_test.go`.

## APIs others rely on

- `durable.Store`, `durable.Manifest`, `durable.ErrStale`, `durable.Stamp`, `durable.CheckPublish`.
- `vactor.Host`: `GetAt(ctx, key, epoch)`, `WriteBack(ctx, key)`, `Passivate(ctx, key) error`, `Discard(key)`,
  `Stamp(ctx, key, epoch, gen)`, `Tick(ctx)`, `RunMaintenance(ctx, every, report)`.
- `actor.(*Actor).Commits()`, `actor.(*Actor).Snapshot(ctx, path)`.

## API changes

- `vactor.Host.Passivate(key)` → `Passivate(ctx, key) error`; `PassivateAll(workers)` now returns `error`.
- New: `vactor.Options.{Store, CreateMissing, Policy, WriteBackTimeout, Workers, Now, CheckActivation, Guard}`,
  `Stats.{WriteBacks, WriteBackFailures, BytesUploaded}`, `vactortest.Opener.FailSnapshot`, `vactortest.Put`.
- `actor`: `Commits()`, `Snapshot(ctx, path)` (additive).
