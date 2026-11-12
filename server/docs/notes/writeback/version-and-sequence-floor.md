# The version and sequence floor

Why a document's `_version` and the commit sequence (`seq`) are never reissued after a database rolls back to
an older generation, and how the floor (`seal << 32`) does it.

It fixes open issue 2 of the [backup open issues](backup-open-issues.md) and finding 2 of the
[cluster simulation](../determinism/cluster-simulation.md). It does not depend on whether the write-back
design is replaced: any design that can roll a database back needs it.

## The rule

A write sets `_version = max(v, floor)`, where `v` is 1 for an insert and the old version plus 1 for a patch,
and `floor = seal << 32`. The seal is the stamp count in the database's manifest, read when the host activates
the database from the store. So a version names one content of a document for good; spec §6.3 and §13
(revision 1.2) say what a client may rely on.

- Code: `internal/docexec/version.go` (`VersionFloor`, `run.version`; the long comment there is the
  reference).
- The seal reaches it as an "incarnation": `vactor.Host.load` reads the manifest →
  `Opener.Open(path,
  incarnation)` → `actor.Config.Incarnation` → `actor.Tx.Incarnation()` →
  `docactor.Call.Fn` sets `docexec.State.Floor`. A warm key keeps the incarnation it was loaded under, also
  across a later stamp and across `reinstate` (same file, nothing rolled back).
- No store (local-disk mode): incarnation 0, no floor, versions count 1, 2, 3 as before.
- Durable mode: the owner stamps before it first admits, so a new database is at seal 1 and its first versions
  are 4294967296, 4294967297, … Three callers pinned small numbers on a durable agent and now only require
  that versions rise: `tests/e2e/platform/scenarios/documents.ts` (`_version === 1`, `i + 2`),
  `backup_restore.ts` (`+ 1`) and `scripts/platform/smoke.ts` (`1, 2, 3, 4`, final `5`).

## Why the seal had to survive a new generation

The first sketch was `max(prev+1, seal<<32)`. It is that formula, after one change it needed. `Manifest.Seal`
used to restart at 0 with each generation (`NextManifest` built a manifest without it), so two incarnations
could share a seal:

1. restore generation G, the owner stamps: seal 1, floor 2^32;
2. write, publish G+1 (seal back to 0), write more, kill -9: the last writes are lost, at 2^32 + k;
3. restore G+1, the owner stamps: seal 1 again, the same floor, and the next writes take 2^32 + k again.

Measured on the saved vector (swarm seed 2761), 2026-11-10:

| Code                                              | `go test ./internal/sim` on the vector                                                                   |
| ------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| before (no floor)                                 | `conservation: a full bank read sums to 398, want 400 (balances [95 115 120 68] versions [38 41 31 44])` |
| floor, seal restarting per generation             | `version reuse: account 0 read at version 4294967331 with balance 95, earlier with 78`                   |
| floor, seal kept across generations (what landed) | passes                                                                                                   |

So the seal now survives a new generation: `durable.Manifest.Succeeding(stored)` copies it, in the host's
publish (`vactor/writeback.go`), the fake store's `Seed`, and `dbtool rebuild-manifest` when the stored
manifest is readable. `(Epoch, Generation, Seal)` still only rises.

Rejected: packing generation or epoch beside the seal (a 2^32 stride leaves 21 bits; they do not fit, and
without the stride an incarnation's `prev+1` walks into the next one's range); a floor kept in the database
file (the file is what rolls back, so two restores of one generation would read the same floor); the compiled
SQL (`max("_version" + 1, ?)`): it would change `core/compile` and
`intermediate-representation/vectors/compile.json` for a statement that runs once per document per
incarnation. The floor is one more `UPDATE` after the compiled write, only when the compiled result is below
the floor.

## Limits (known, accepted)

- 53 bits: versions are JSON numbers. 2^21 stamps (restarts and moves) per database; after that, and for a
  single document patched past 2^53 − 1, writes fail with `StorageError` (`TestVersionFloor`). An incarnation
  that patches one document 2^32 times and then loses the tail could meet the next floor.
- A host that restores without a stamp in between has the same floor as before. The owner always stamps before
  it admits (`owner.Table.stamp`), so this is only hosts run without `internal/owner` (the single-node sim,
  `docexec`'s crash fuzz), which have no fencing either.
- `dbtool rebuild-manifest` over an **unreadable** manifest cannot know the old seal: the count restarts at 0.
  The rebuild is itself a rollback, so versions issued after the generation it picks can then repeat. Not
  fixed (it needs an operator-supplied seal).
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
- The cluster checker: `(account, _version)` read with two balances is a violation now (class `version-reuse`;
  it was a counter). The bank model no longer assumes `+1`: it learns versions from the answers and requires
  that they rise (`bankState` in `internal/sim/cluster_model.go`).
- Swarm, 2026-11-10: `scripts/sim-swarm.sh 150 2700` (seeds 2700–2849, 2761 among them): 150 passed.
- `tests/unit/sdk_read_path_test.ts`: "read path 4: the cache takes the version the commit answered, not the
  last plus one".

## The commit sequence (spec revision 1.3)

**The flaw.** `seq` (spec §6.5, revision 1.1) counted on from the restored value. A client that ran a `find`
before a kill -9 held the sequence of a lost state; after the restart its `assert {unchangedSince}` held
against a table with other documents than the ones it saw. Same class as the `_version` bug (a precondition
passes against other state), for guarded selects and for a run's snapshot identity.

**The probe**, now `internal/docexec/seq_test.go` `TestSequenceIsNotReissuedAfterARollback` (durable host over
the fake store; publish, a lost note and a lost list, the guard a client derives, kill -9, owner-style stamp,
restore; three rounds, published between the crashes), 2026-11-11:

| Code   | The test                                                                                                                                                                                                                                   |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| before | `round 0, before a new write: notes unchanged since the lost sequence 3: <nil>, want a conflict`; with that check skipped: `round 0: the restored database issued sequence 2 (lost: 3, which was a lost note)`                             |
| after  | passes: the guard at the lost sequence is a `ConflictError` (`check: "unchanged"`) before any new write, after a new note, and on `lists` (not written since the restore); the new note answers 2^32 in round 0, above every lost sequence |

**The rule** (code and the long comment: `internal/docexec/seq.go`):

1. A mutation that changes a row leaves `max(seq + 1, floor)`, the floor being the one `_version` takes
   (`State.Floor`, `seal << 32`). What a restored database answers is above every sequence an earlier
   incarnation answered.
2. The first such mutation of an incarnation (it finds the sequence below the floor) marks **every** table
   changed at the floor. A table not written since the restore keeps the last change the file has, at most the
   restored sequence, and a guard from the lost range (restored sequence, floor) would hold against it.
3. `unchangedSince: n` with `n` above the database's sequence is a conflict (`actual` is the table's last
   change, so it can be below `expected`; the error's `seq` shows why). This covers the time between the
   restore and rule 2's mutation, and it is the one rule a database without a floor shows too.

Each rule is needed: the test fails with any one of the three taken out (1: a lost sequence is answered again;
2: `lists unchanged since the lost sequence` holds after a new note; 3: the same before a new write).

Rejected: remembering the lost range (restored sequence, floor) in the file instead of marking every table, so
that a guard from before a restart that lost nothing would still hold. The file is what rolls back: a second
restore of the same generation would need the ranges of every earlier incarnation, which are not in it. The
mark costs one conflict per guarded select per restart (the client reads again), and one upsert of a row per
table per incarnation.

**Limits**: the version floor's, above (no floor without a store; a host that restores without a stamp; 2^21
stamps, after which a mutation that changes a row fails with `StorageError`, a delete too:
`TestSequenceFloor`; `dbtool rebuild-manifest` over an unreadable manifest; `dbtool restore` + `import`).
Durable mode: sequences start at 2^32 (one caller pinned `"seq":1` on a durable agent:
`cmd/agentd/backup_test.go`, which now carries the sequence its insert answered).

**Protocol**: `intermediate-representation/spec/v1.md` §5.5, §6.5, §8.1 and §14 (revision 1.3). Vectors:
`execute.json` "table assert: a sequence the database has not reached" (rule 3, both layouts; vectors run with
floor 0, so rules 1 and 2 are Go tests, as the version floor's are). `decode.json` is unchanged: no request
changes shape. The SDK needs no change (a conflict's `seq` and `current` already rebase its read set, and a
table listed in `current` is read again); its test model (`tests/unit/sdk_model.ts`) has rule 3.

**The sim now exercises table preconditions** (`internal/sim/cluster_client.go`, `cluster_model.go`): every
third operation of a client, when it is a transfer, guards its read with `assert {unchangedSince}` on
`accounts` instead of the two `expect.version` (by count, not by a draw, so seeds draw what they drew before).
The bank model judges a transfer by the versions read either way, so a guard that holds against other accounts
does not linearize. And a read's `(account, seq)` seen with two versions is a violation (class
`sequence reuse`). Measured 2026-11-11, `SIM_SHRINK=0 scripts/sim-swarm.sh 150 2700` (seeds 2700–2849):

| `seq.go`                                            | Cluster seeds                                                                                                                                                                                                                                     |
| --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| the three rules taken out (the code before the fix) | 94 passed, 56 failed: `sequence reuse` in all 56 (e.g. `account 2 read at sequence 31 with version 4294967299, earlier with 4294967301`), and in two of them the damage itself: `conservation: a full bank read sums to …` and `not linearizable` |
| as landed                                           | 150 passed, 0 failed                                                                                                                                                                                                                              |

The saved vectors in `testdata/cluster` replay the changed workload (a third of their transfers are guarded
now) and pass. No vector was saved for the sequence: the docexec test is the regression, and every swarm seed
now checks the class.
