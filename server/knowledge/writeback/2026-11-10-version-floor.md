# 2026-11-10 — the version floor: `_version` is not reissued after a rollback

Written by the version-fixes run (Fable 5.2, branch `feat/version-fixes`, MANSIO-REFACTORS item 3). It closes
open issue 2 of [backup paused, open issues](2026-11-06-backup-paused-open-issues.md) and finding 2 of the
[cluster sim note](../determinism/2026-11-06-dst2-dst3-cluster-sim.md). It does not depend on the LiteFS
decision: any design that can roll a database back needs it.

## The rule

A write sets `_version = max(v, floor)`, where `v` is 1 for an insert and the old version plus 1 for a patch,
and `floor = seal << 32`. The seal is the stamp count in the database's manifest, read when the host
activates the database from the store. So a version names one content of a document for good; spec §6.3 and
§13 (revision 1.2) say what a client may rely on.

- Code: `internal/docexec/version.go` (`VersionFloor`, `run.version`; the long comment there is the reference).
- The seal reaches it as an "incarnation": `vactor.Host.load` reads the manifest → `Opener.Open(path,
  incarnation)` → `actor.Config.Incarnation` → `actor.Tx.Incarnation()` → `docactor.Call.Fn` sets
  `docexec.State.Floor`. A warm key keeps the incarnation it was loaded under, also across a later stamp and
  across `reinstate` (same file, nothing rolled back).
- No store (local-disk mode): incarnation 0, no floor, versions count 1, 2, 3 as before.
- Durable mode: the owner stamps before it first admits, so a new database is at seal 1 and its first
  versions are 4294967296, 4294967297, … Three callers pinned small numbers on a durable agent and now only
  require that versions rise: `tests/e2e/platform/scenarios/documents.ts` (`_version === 1`, `i + 2`),
  `backup_restore.ts` (`+ 1`) and `scripts/platform/smoke.ts` (`1, 2, 3, 4`, final `5`).

## Why not the note's `max(prev+1, seal<<32)` as it stood

It is that formula, after one change it needed. `Manifest.Seal` used to restart at 0 with each generation
(`NextManifest` built a manifest without it), so two incarnations could share a seal:

1. restore generation G, the owner stamps: seal 1, floor 2^32;
2. write, publish G+1 (seal back to 0), write more, kill -9: the last writes are lost, at 2^32 + k;
3. restore G+1, the owner stamps: seal 1 again, the same floor, and the next writes take 2^32 + k again.

Measured, on the saved vector (swarm seed 2761), 2026-11-10, this VM:

| Code | `go test ./internal/sim` on the vector |
|---|---|
| before (no floor) | `conservation: a full bank read sums to 398, want 400 (balances [95 115 120 68] versions [38 41 31 44])` |
| floor, seal restarting per generation | `version reuse: account 0 read at version 4294967331 with balance 95, earlier with 78` |
| floor, seal kept across generations (what landed) | passes |

So the seal now survives a new generation: `durable.Manifest.Succeeding(stored)` copies it, in the host's
publish (`vactor/writeback.go`), the fake store's `Seed`, and `dbtool rebuild-manifest` when the stored
manifest is readable. `(Epoch, Generation, Seal)` still only rises (guide W3).

Rejected: packing generation or epoch beside the seal (a 2^32 stride leaves 21 bits; they do not fit, and
without the stride an incarnation's `prev+1` walks into the next one's range); a floor kept in the database
file (the file is what rolls back, so two restores of one generation would read the same floor); the compiled
SQL (`max("_version" + 1, ?)`): it would change `core/compile` and `ir/vectors/compile.json` for a statement
that runs once per document per incarnation. The floor is one more `UPDATE` after the compiled write, only
when the compiled result is below the floor.

## Limits (known, accepted)

- 53 bits: versions are JSON numbers. 2^21 stamps (restarts and moves) per database; after that, and for a
  single document patched past 2^53 − 1, writes fail with `StorageError` (`TestVersionFloor`). An incarnation
  that patches one document 2^32 times and then loses the tail could meet the next floor.
- A host that restores without a stamp in between has the same floor as before. The owner always stamps
  before it admits (`owner.Table.stamp`), so this is only hosts run without `internal/owner` (the DST-1
  single-node sim, `docexec`'s crash fuzz), which have no fencing either.
- `dbtool rebuild-manifest` over an **unreadable** manifest cannot know the old seal: the count restarts at
  0. The rebuild is itself a rollback, so versions issued after the generation it picks can then repeat.
  Not fixed (it needs an operator-supplied seal).
- `dbtool restore` + `import` into a key without a manifest (another store, or a new key) starts a new stamp
  count: generation 1, seal 0. The file keeps its versions, so the restored database counts on from them, and
  a version issued in the old store after that backup can be issued again in the new one. An operator restore
  is a rollback the floor does not see; clients must not carry versions across it. Not fixed (the old seal is
  not in the file, and cannot be: stamps after the backup are what matter).

## The SDK

`sdk/recording.ts` counted a patched document's version itself (`+1`) when it cached the document after a
commit. With a floor the server may answer more, and every warm run would have started with a conflict (one
wasted trip, never a wrong commit). `Recording.committed(results)` now takes the version the patch answered.
Inside a run the local `_version` is still the run's own count: provisional, like `_creationTime`.

## Tests

- `internal/docexec/version_test.go`: `TestVersionIsNotReissuedAfterARollback` (durable host over the fake
  store, three rounds of publish, lost patch and lost insert, kill -9, owner-style stamp, restore; a patch
  expecting the lost version is a `ConflictError`, and nothing takes a lost version again; fails under either
  mutant of the table above) and `TestVersionFloor` (the arithmetic and the exhausted case).
- `internal/sim/testdata/cluster/version-reuse-after-rollback.json`: the saved vector, now in the regression
  set (`go test ./internal/sim -run 'TestClusterVectors/version-reuse-after-rollback' -v`).
- The cluster checker: `(account, _version)` read with two balances is a violation now (class
  `version-reuse`; it was a counter). The bank model no longer assumes `+1`: it learns versions from the
  answers and requires that they rise (`bankState` in `internal/sim/cluster_model.go`).
- Swarm, 2026-11-10: `scripts/sim-swarm.sh 150 2700` (seeds 2700–2849, 2761 among them): 150 passed.
- `tests/unit/sdk_read_path_test.ts`: "read path 4: the cache takes the version the commit answered, not the
  last plus one".

## Open: the commit sequence has the same flaw (found here, not fixed)

`seq` (spec §6.5, revision 1.1) also counts on from the restored value. Probed on the durable test host: an
insert answers `seq` 2 and is lost in a kill -9; after the restart another insert answers `seq` 2; a client
that ran a `find` before the crash sends `assert {unchangedSince: 2}` and it passes, though the table holds
other documents than the ones it saw. Same class as the `_version` bug (a precondition passes against other
state), for guarded selects and for a run's snapshot identity. The sim does not exercise table preconditions,
so no seed shows it.

A fix needs more than the floor: a table untouched since the restore keeps its old, low sequence, so the
assert must also refuse a sequence the database never reached. Sketch: `seq = max(seq+1, floor)`; the first
mutation of an incarnation marks every table changed at the floor; `unchangedSince: n` with `n` above the
database's sequence is a conflict. That is a protocol change (spec §5.5, vectors). Owner: Nick to decide.
