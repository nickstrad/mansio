# mansio — project state

## 2026-11-11 — bug fixes: the commit sequence after a rollback, and the two red Go tests (Fable 5.2, Nick-directed)

- Goal/spec: brief `/root/briefs/mansio-bug-fixes-brief.md` (Nick, 2026-11-10: "Have fable fix legit bugs and
  commit all this to main"). Item 1: the open finding of the version-fixes section below (`seq` and
  `assert {unchangedSince}` after a rollback; Nick's go covers the protocol change). Items 2 and 3: the two
  baseline-red Go tests. One commit per item, a simplify pass, then a merge to main from
  `/root/Software/mansio-work/main-merge`.
- Status: **merged to main and pushed: merge `f140dea`** (`git merge --no-ff feat/bug-fixes` on top of
  `a31efc8`, pushed 2026-11-11). Commits: item 2 `3f374c1`, item 3 `0687ffe`, item 1 `00dabc8`, simplify
  `c6d8e8d`. All three items are done and `go test ./...` is green: no baseline failure is left. No
  cross-family review ran: the brief directed Fable to merge after the simplify pass.
- How it was merged: as the version-fixes merge was, in `/root/Software/mansio-work/main-merge` on a detached
  HEAD at `origin/main`, pushed with `git push origin HEAD:main`. **The local `main` ref and Twin's checkout
  (`/root/Software/mansio`) are behind**: run `git pull --ff-only` there to catch up.
- Verified on the merged tree (identical to the verified branch head `1e43c5c`): `go test -count=1 ./...` exit
  0 (63 packages ok); `make platform-up` exit 0 (`warm cache 1 and 1, cold 2, stale cache 2`,
  `platform smoke OK`); `make platform-down` exit 0.
- Item 1, the sequence floor: **done** `00dabc8`. Reference:
  `server/knowledge/writeback/2026-11-10-version-floor.md` (its last section is the fix record); spec
  `intermediate-representation/spec/v1.md` §5.5, §6.5, §14 (revision 1.3).
  - The rule (the note's sketch, adopted as it stood; `server/internal/docexec/seq.go`): a mutation that
    changes a row leaves `max(seq + 1, floor)`, the floor `_version` takes; the first such mutation of an
    incarnation marks every table changed at the floor; `unchangedSince: n` with `n` above the database's
    sequence is a `ConflictError` (`check: "unchanged"`). Durable mode: sequences start at 2^32.
  - Probe, `docexec` `TestSequenceIsNotReissuedAfterARollback`: before,
    `notes unchanged since the lost
    sequence 3: <nil>, want a conflict` and (that check skipped)
    `the restored database issued sequence 2 (lost: 3)`; after, it passes, and fails again with any one of the
    three rules removed. `TestSequenceFloor` pins the arithmetic and the exhausted case.
  - Vectors: `execute.json` +2 ("table assert: a sequence the database has not reached", both layouts);
    `decode.json` unchanged (no request changes shape). SDK unchanged; its test model has the new rule.
  - The sim exercises table preconditions now: every third client operation, when a transfer, is guarded by
    `assert {unchangedSince}` on `accounts`; `(account, seq)` read with two versions is a violation.
    `SIM_SHRINK=0 scripts/sim-swarm.sh 150 2700`: with the fix taken out, 56 of 150 seeds fail
    (`sequence
    reuse` in all, `conservation` and `not linearizable` in two); as landed, 150 passed.
  - One caller pinned `"seq":1` on a durable agent: `cmd/agentd/backup_test.go`
    (`TestDurableOpsWithSchemasDir`), which now carries the sequence its insert answered.
- Item 2, `TestRetentionBoundsTheTree`: **done** `3f374c1`. **The test was the wrong side** (the sweeper is
  right to compare file times with its clock). The test's clock is fake but the files it wrote carried
  wall-clock times: it stamped only the open file, once per five records, so files that rolled inside a batch
  looked newer than the fake "now" and were never swept. Now each record is followed by setting its file's
  time to the fake clock, so every closed file a sweep sees carries a fake time. Evidence: unfixed, it fails
  today and with the fake clock at 2020-01-05 (`the tree holds 35424 bytes`); fixed, it gives the same numbers
  with the fake clock at 2020-01-05, 2026-11-10 and 2027-11-10
  (`retention deleted 31 files (42228 bytes); 9504
  bytes left under a 10 000 limit`). No assertion changed.
- Item 3, `TestAdminFetchLimitAppliesToTheNextFetches`: **done** `0687ffe`. **The test's fake was the wrong
  side; the product is right** (the limit applies: with the fake fixed, the unchanged test sees 1 fetch in
  flight and 2 queued at limit 1, 3 in flight at limit 3). Cause: `s3test`'s `Dribble` fault slept once per
  byte, and a timer plus a flush per byte cannot keep a high rate: asked for 16 KiB/s it delivered 1.5 KiB/s
  on this VM (probe: 16 384 bytes in 10.6 s). The test's "slow" fetches therefore ran into the 5 s S3 deadline
  it sets and the queries answered 504 `expired` and 503 `unavailable`. `Dribble` now paces against the clock
  (each round sends the bytes that are due); `TestDribbleSendsAtTheRate` also pins the high rate (16 KiB in
  about a second). The agentd test is unchanged and passed 6 of 6 runs.
- Simplify pass: **done** `c6d8e8d` (behaviour unchanged). The cluster checker has one reuse check for both
  `(account, _version)` and `(account, seq)` (`seenWith`); the sim's guarded transfer builds its patches once
  instead of building and deleting `expect`; `Dribble` lost two dead `max()`; the docexec test reads the
  sequence with `db.seq()`. Reviewed and left: `run.unchangedSince` (two callers, the assert and the
  conflict's `current`), `run.next` (two callers). After it, the sim still catches the fix taken out (seeds
  2849 and 2841: `sequence reuse`) and passes with it.
- Verification on `c6d8e8d` (2026-11-11, this VM), **all green, no baseline failure left**: `gofmt -l .`
  empty; `go vet ./...` ok; `go test -count=1 ./...` exit 0 (63 packages ok, also on `00dabc8` before the
  simplify pass); `deno task ci` exit 0 (341 unit, 61 property); `work-scripts/bash/e2e-linux.sh`
  `E2E PASS
  seed=20261010 scenarios=8`; `make platform-up` exit 0
  (`warm cache 1 and 1, cold 2, stale cache 2`, `platform smoke OK`), `make platform-down` exit 0. Not run:
  `go test -race ./...` and `-tags noassert` (the brief's bar is `go test ./...`).
- Next: nothing is open from this brief. Follow-ups, none blocking: `go test -race ./...` and `-tags noassert`
  were not run here; a restart costs each guarded select one conflict (the note's "Rejected" paragraph says
  why the cheaper rule was not taken).
- Resume: nothing is running and no platform is up (`docker ps | grep mansio-platform` is empty). The branch
  `feat/bug-fixes` and its worktree `/root/Software/mansio-work/bug-fixes` are merged and can be removed
  (`git worktree remove`; `server/results/sim-swarm/` there holds the two swarm outputs, ignored by git).

## 2026-11-10 — version fixes: `_version` after a rollback, EnsureBucket, per-checkout Compose project (Fable 5.2, Nick-directed)

- Goal/spec: brief `/root/briefs/mansio-version-fixes-brief.md`. Item A: MANSIO-REFACTORS item 3 (`_version`
  reuse after a rollback). Item B: recorded platform-up follow-up 2 (`objstore.EnsureBucket` and "already
  owned by you"). Item C: recorded follow-up 1 (the fixed Compose project name). One commit per item, then a
  simplify pass, then a merge to main from `/root/Software/mansio-work/main-merge`.
- Status: **merged to main and pushed: merge `e127a46`** (`git merge --no-ff feat/version-fixes` on top of
  `7dcb164`, pushed 2026-11-10). Commits: A `67d4deb`, B `e4a24cf`, C `4a6d2c0`, simplify `263b591`. All three
  items are done, the simplify pass is done, verification is at baseline. No cross-family review ran: the
  brief directed Fable to merge after the simplify pass.
- How it was merged: as the read-path merge was, in `/root/Software/mansio-work/main-merge` on a detached HEAD
  at `origin/main`, pushed with `git push origin HEAD:main`. **The local `main` ref and Twin's checkout
  (`/root/Software/mansio`) are behind**: run `git pull --ff-only` there to catch up.
- Verified on the merged tree (identical to the verified branch head `e6d4d6b`): `make platform-up` exit 0
  (`warm cache 1 and 1, cold 2, stale cache 2`, `platform smoke OK`), `make platform-down` exit 0; `gofmt`,
  `go vet ./...`; `go test -race` on the touched packages (only the baseline
  `TestAdminFetchLimitAppliesToTheNextFetches` fails); `deno task test` 341 passed; `deno task test:prop` 61
  passed.
- Item A, the version floor: **done** `67d4deb`. Reference:
  `server/knowledge/writeback/2026-11-10-version-floor.md`; spec `intermediate-representation/spec/v1.md` §6.3
  and §13 (revision 1.2).
  - The rule: a write sets `_version = max(v, floor)`, `floor = seal << 32` (`docexec.VersionFloor`), the seal
    being the manifest's stamp count at activation (host → `Opener.Open(path, incarnation)` →
    `actor.Tx.Incarnation()` → `docexec.State.Floor`). No store: no floor, versions as before. Durable mode:
    versions start at 2^32.
  - The note's `max(prev+1, seal<<32)` was evaluated against the code and adopted with one change it needed:
    `Manifest.Seal` restarted with each generation, so two incarnations could share a seal; it now survives a
    new generation (`Manifest.Succeeding`). Evidence on the saved vector (swarm seed 2761): before,
    `conservation: a full bank read sums to 398, want 400`; with the floor on a restarting seal,
    `version reuse:
    account 0 read at version 4294967331 with balance 95, earlier with 78`; as landed, the
    vector passes and moved to `server/internal/sim/testdata/cluster/` (`testdata/open` is empty).
  - Tests: `docexec` `TestVersionIsNotReissuedAfterARollback` (a patch expecting a pre-rollback version is a
    `ConflictError`; no lost version is issued again; fails under either mutant) and `TestVersionFloor`; the
    cluster checker now treats a reused `(account, _version)` as a violation and the bank model takes versions
    from the answers; SDK "read path 4: the cache takes the version the commit answered, not the last plus
    one" (`Recording.committed(results)`: the SDK no longer counts a patched document's version itself).
  - Verified: `deno task ci` exit 0 (341 unit, 61 property); `gofmt`, `go vet`; `go test ./...` fails only
    `cmd/agentd` `TestAdminFetchLimitAppliesToTheNextFetches` (the known baseline) and `internal/eventlog`
    `TestRetentionBoundsTheTree`, which fails the same way on an untouched `origin/main` checkout when run
    late in the UTC day (20:42 UTC here; it passed in the earlier runs recorded below) and touches nothing
    this branch changes; `go test -race` and `-tags noassert` on the touched packages (`docexec`, `vactor`,
    `durable`, `actor`, `sim`, `front`, `owner`, `agentglue`, `cmd/dbtool`, `cmd/coldload`) ok;
    `scripts/sim-swarm.sh 150 2700` (seeds 2700–2849): 150 passed; `work-scripts/bash/e2e-linux.sh`
    `E2E PASS
    seed=20261010 scenarios=8`; `make platform-up` exit 0
    (`warm cache 1 and 1, cold 2, stale cache 2`, `platform smoke OK`, on versions that start at 2^32) and
    `make platform-down` clean.
  - Callers that pinned version numbers on a durable agent now require only that versions rise:
    `tests/e2e/platform/scenarios/{documents,backup_restore}.ts`, `scripts/platform/smoke.ts`.
  - Limits, accepted (in the note): 2^21 stamps per database, then writes fail closed;
    `dbtool
    rebuild-manifest` over an unreadable manifest, and `dbtool restore` + `import` into a key
    without a manifest, restart the seal (an operator rollback the floor does not see).
- Item B, `EnsureBucket` is idempotent: **done** `e4a24cf`. `objstore.Store.EnsureBucket` treats the store's
  `BucketAlreadyOwnedByYou` to its create as success (the bucket is there and ours); another account's name
  (`BucketAlreadyExists`, or a refused HEAD) still fails.
  - Tests: `objstore` `TestEnsureBucket` (five bucket states on the in-process S3, `s3test.SetBucket`) and
    `TestEnsureBucketFromTwoStartersAtOnce`; both fail without the fix.
  - The `up.sh` workaround (creating the bucket with `weed shell` before the agents start) is **removed**: it
    is unnecessary now, and without it `make platform-up` exercises the path production takes. Four
    `platform-up` / `platform-down` cycles without it: all exit 0, `platform smoke OK`, no
    `S3.BUCKET_CHECK_FAILED`; on each agent `mansio_s3_requests_total{op="bucket",kind="ok"} 2` and every
    error kind 0 (both agents sent the create; the platform's SeaweedFS answers a second create 409
    `BucketAlreadyOwnedByYou`, probed directly).
  - Verified: `gofmt`, `go vet`; `go test ./internal/objstore/...` and `-race` on `objstore`, `vactor`,
    `cmd/agentd` ok.
- Item C, a Compose project per checkout: **done** `4a6d2c0`. `scripts/platform/lib.sh` derives the project
  from the resolved state directory (`realpath -m` of `PLATFORM_DIR`, default `<checkout>/.platform`):
  `mansio-platform-<first 8 hex of its sha256>`. `compose.yml` takes the name from `PLATFORM_PROJECT` (it has
  none of its own and refuses without it); the marker in the state directory is `.<project>`, so `down.sh`
  deletes only a directory this path's `up.sh` made. Comments in `up.sh`, `down.sh`, `compose.yml` and the
  `Makefile` say so.
  - Proof (2026-11-10, this worktree and a second checkout of the same commit at `/tmp/vf-base`):
    `make platform-up` here → project `mansio-platform-9d884e4e`, smoke OK. `make platform-down` in the second
    checkout → `removing compose project mansio-platform-caed965e`, exit 0; here, the same two container ids
    still running, both volumes present, all four daemons alive, and `make platform-up` again (smoke only)
    `platform smoke OK`. `make platform-up` in the second checkout while this one is up →
    `port 15432 is in
    use`, exit 2, nothing created (accepted: the host ports are fixed).
    `make platform-down` here → exit 0; containers identical to the pre-run list, 0 `mansio-platform` volumes
    and networks, `.platform/` gone, no platform port held.
  - Verified: `shellcheck -x scripts/platform/*.sh` clean; six `platform-up` / `platform-down` cycles on the
    new names, all exit 0.
- Simplify pass: **done** `263b591` (behaviour unchanged). Removed the sim's `Stats.VersionReuse` and its
  counter (dead once a reused version is a violation); `s3test`'s bucket HEAD/PUT answers are one status
  table; `docexec`'s test `restart()` and `crash()` share `abandon()`. Reviewed and left: the incarnation's
  path through `actor` (four signatures, no shorter road to `Tx`), `Manifest.Succeeding` (three callers).
- Final verification on `263b591` (2026-11-10, this VM, `/tmp/vf-verify.sh`): `gofmt -l` empty;
  `go vet
  ./...`; `go test ./...`: only the two baseline failures below; `go test -race` and
  `-tags noassert` on `internal/{docexec,vactor,durable,actor,sim,front,owner,agentglue,objstore}/...`,
  `cmd/{dbtool,coldload,
  agentd}`: only `TestAdminFetchLimitAppliesToTheNextFetches`; `deno task ci` exit 0
  (341 unit, 61 property); `work-scripts/bash/e2e-linux.sh` `E2E PASS seed=20261010 scenarios=8`;
  `shellcheck -x` clean; `make
  platform-up` exit 0 (`warm cache 1 and 1, cold 2, stale cache 2`,
  `platform smoke OK`), `make platform-down` exit 0.
- Baseline failures, both reproduced on an untouched `origin/main` (`7dcb164`) checkout, neither touched here
  (item 2 owns the green baseline):
  - `cmd/agentd` `TestAdminFetchLimitAppliesToTheNextFetches` (known).
  - `internal/eventlog` `TestRetentionBoundsTheTree` (**new to this list**, from backpressure B9): its fake
    clock is fixed at 2026-11-10 09:00 local but the files it writes carry real modification times, so once
    the real clock is past the fake one the files look newer than "now" and are never swept. It passed in this
    morning's runs and fails from now on, on every checkout.
- The open finding of this run (the commit sequence after a rollback) and both baseline failures above are
  fixed by the bug-fixes run (the section at the top).
- Resume: nothing is running and no platform is up (`docker ps | grep mansio-platform` is empty). The branch
  `feat/version-fixes` and its worktree `/root/Software/mansio-work/version-fixes` are merged and can be
  removed (`git worktree remove`).

## 2026-11-10 — read path: survey changes 1–4 and the colocated-runner explainer (Fable 5.2, Nick-directed)

- Goal/spec: brief `/root/briefs/mansio-read-path-brief.md`; the spec is the survey's §3.2 changes 1–4
  (`docs/programmatic-schema-platform-survey.md` on branch `docs/programmatic-schema-survey`, worktree
  `/root/Software/mansio-work/schema-survey`), framed by its §3.1 and §3.4. Nick approved "keep placement B,
  do changes 1 to 4" on 2026-11-09. One commit per change, then `docs/colocated-runner-explainer.md` (changes
  5–7, no code), a simplify pass, and a merge to main from `/root/Software/mansio-work/main-merge`.
- Status: **merged to main and pushed: merge `8fa7779`** (`git merge --no-ff feat/read-path-1234` on top of
  `8075b8d`, pushed 2026-11-10). All four changes are implemented, the explainer is written, the simplify pass
  is done, and verification is at baseline. No cross-family review ran: the brief directed Fable to merge
  after the simplify pass.
- How it was merged: as the backpressure merge was. `main` is checked out in `/root/Software/mansio` (Twin's
  checkout, not to be worked in), so the merge was made in `/root/Software/mansio-work/main-merge` on a
  detached HEAD at `origin/main` and pushed with `git push origin HEAD:main`. **The local `main` ref and
  Twin's checkout are behind**: run `git pull --ff-only` there to catch up.
- Verified on the merged tree (identical to the verified branch head `f31bdda`): `make platform-up` →
  `makeMove round trips through the edge: warm cache 1 and 1, cold 2, stale cache 2`, `platform smoke OK`;
  `make platform-down` clean; `deno task test` 340 passed; the SDK and decode property tests; `gofmt`,
  `go vet` and `go test -race` on `internal/docexec/...`, `internal/intermediate-representation`,
  `internal/front/...`, `cmd/agentd` (only the baseline `TestAdminFetchLimitAppliesToTheNextFetches` and,
  under `-race`, `TestIndexNamesDecodeFast` fail; the latter passes without `-race`).
- Baseline before any change (this VM, `8075b8d`): `deno task ci` exit 0 (320 unit, 61 property);
  `go test ./...` fails only `cmd/agentd` `TestAdminFetchLimitAppliesToTheNextFetches` (the known baseline
  failure of the section below).
- Change 1, batch reads and one snapshot per run: **done** `906bbdc`.
  - Protocol (IR revision 1.1, `intermediate-representation/spec/v1.md` §6.5 and §12): every successful answer
    carries `seq`, the database's commit sequence (one per applied mutation that changed a row; stored in
    `_mansio_changes`, the row with the empty table name; created by `docexec.Init`, and by the first mutation
    on an older file); a query may hold `assert` ops, checked in the snapshot its reads come from.
  - SDK (`sdk/recording.ts`, `sdk/client.ts`): reads asked for together are one request (`db.getMany`, or
    `Effect.all([...], { batching: true })`); every read request after a run's first carries the asserts of
    what the run has read so far, so after each answer the whole read set holds in one snapshot
    (`Recording.snapshot`); a failed assert abandons the run (`Restart`) and the client re-runs the handler,
    for queries as for mutations; reads too many to share a request with the asserts go bare and must answer
    one sequence. A mutation that wrote nothing sends no commit.
  - Tests: `tests/unit/sdk_read_path_test.ts` ("read path 1: …", six tests, including a transfer landing
    between two reads of one run); execute vectors `seq counts the mutations that changed a row`,
    `query with
    asserts that hold`, `query with an assert that fails`; Go
    `TestSequenceOnAFileWithoutTheTable`; the Go model fuzz (`internal/docexec/model_fuzz_test.go`) now
    predicts `seq` and the two sequence statements.
  - Verified: `deno task ci` exit 0 (326 unit, 61 property); `gofmt`, `go vet`; `go test ./...` fails only the
    baseline test.
  - Deviations from the survey: (1) the survey proposed that the SDK compare sequences and re-run when two
    reads straddled a commit. Built instead: the later request carries the earlier reads' asserts, so a commit
    that did not touch what the run read costs nothing, and only a real change re-runs. Sequence comparison
    remains as the fallback when the asserts do not fit in the request. This needed `assert` in a query, which
    the survey did not list. (2) Effect's request batching batches only under `batching: true`; reads that are
    merely concurrent stay separate requests. (3) Not in the survey: a read-only mutation no longer sends an
    asserts-only commit, because its reads are one snapshot already.
  - Local logic later: `Reads` is the executor's only way out and answers `{ result, seq }`, so the executor
    can run against anything that answers reads at a named sequence; `Recording.snapshot` exposes the run's
    snapshot identity; nothing here trades against local execution.
- Change 2, guard `find`: **done** `2868127`.
  - Protocol (`intermediate-representation/spec/v1.md` §5.5): an `assert` without `id` is a table
    precondition, `{"expect": {"unchangedSince": n}}`: no applied mutation after sequence `n` changed a row of
    the table. A failed one is `ConflictError` with `check: "unchanged"` and no `id`. `_mansio_changes` keeps
    each table's last change beside the database's sequence (one upsert per mutation that wrote).
  - SDK: every `find` adds its table to the run's read set at the sequence it was answered at
    (`Recording.preconditions()` now answers document asserts, then table asserts); the guard rides in later
    reads (snapshot) and in the commit. `find(...).unguarded()` is the per-call opt-out.
  - Tests: "read path 2: …" (four tests: the phantom insert that now conflicts and re-runs, the same without a
    writer in two trips, `unguarded()` committing the skew, finds in the snapshot); decode vectors for the
    table assert (7) and execute vectors `table assert: …` (3, one is the phantom at the protocol level); the
    Go model fuzz generates table asserts; the SDK property test pins the derived table assert.
  - Verified: `deno task ci` exit 0 (330 unit, 61 property); `gofmt`, `go vet`; `go test ./...` fails the
    baseline `TestAdminFetchLimitAppliesToTheNextFetches`, plus timing flakes met while both suites ran at
    once: `internal/intermediate-representation` `TestIndexNamesDecodeFast` (passes alone, 3 of 3) and
    `internal/bench` `TestCancelStopsSendingButFinishesTheFiles`, `TestCancelDoesNotWaitOutBackoffs`,
    `TestRunWritesEveryFile` (about 1 run in 6 alone; the package imports nothing this branch changes; the
    first two are listed as baseline flakes in the section below).
  - Deviations: none from the survey's design (b). The survey's "default-on with a per-call opt-out" is
    `unguarded()`. The guard's cost is the survey's: any change to the table is a conflict, also one the
    select would not have seen.
  - Local logic later: a guard is a plain IR assert (`Guard` in `sdk/recording.ts`), so a local speculative
    run states its read set in the same form. The trade taken: table granularity now; a range assert (spec
    §10) can be added as a narrower `Guard` without changing how guards travel.
- Change 3, conflict answers carry the current state: **done** `5eeb650`.
  - Protocol (`intermediate-representation/spec/v1.md` §8.1): a `ConflictError`'s `fields` gain `seq` (the
    sequence of the state the request ran against) and `current`: one entry for the failing operation and one
    for every later `assert` that fails too (`{op, table, id, doc}`, or `{op, table, changed}` for a table
    precondition), all against the state the failing operation saw. Capped at 65 536 bytes of canonical JSON;
    a cut list carries `currentTruncated: true`. Server: `server/internal/docexec/witness.go`.
  - SDK: `Recording.rebase(error, sentGuards)` turns the conflict and the run's read set into a `ReadSet`
    (documents and unchanged-table select answers, valid at the conflict's `seq`); the client passes it to the
    next run as `seed`, on a commit conflict and on a read conflict alike. What the next run uses of the seed
    it asserts again. No `current`, or a truncated one: the next run reads everything again.
  - Tests: "read path 3: …" (three tests; the first is the brief's "conflict re-run that does not re-read":
    query, refused commit, commit, where it was query, commit, query, commit); execute vectors
    `conflict
    carries …` and `conflict on …` (4); Go `TestConflictCurrentIsBounded`.
  - Verified: `deno task ci` exit 0 (333 unit, 61 property); `gofmt`, `go vet`; `go test ./...` fails only the
    baseline test.
  - Deviations: (1) the survey said "check every assert rather than stopping at the first"; the server lists
    the failing operation and every later assert that fails, not asserts placed after a write that ran
    (nothing runs after the failure, so they are evaluated against the state the failing operation saw). The
    SDK puts all asserts first, so for it this is every assert. (2) Beyond the survey: select answers from
    tables the conflict did not name are kept too, and an identical guarded `find` repeated in one run is
    answered from the run's own read set.
  - Local logic later: `ReadSet` is plain data (documents, select answers, one sequence) and
    `RecordingOptions.seed` takes one from anywhere, which is the shape a local store would hand a speculative
    run; `rebase` is the pure step a local re-run or rebase would call with the server's verdict.
- Change 4, version-checked cached reads: **done** `5e77f02` (+ tests `655b266`, platform smoke `62a8422`).
  - SDK only. `sdk/cache.ts`: `ReadCache` (`get`/`put`/`delete`) and `memoryCache(maxDocuments = 1024)`, an
    LRU that keeps the newer `_version` and hands out copies. `ClientOptions.cache` (default: one
    `memoryCache()` per client; `false` turns it off; any `ReadCache` may be passed).
  - A mutation handler's `get` is answered from the cache when it holds the document; the entry is
    `unverified` until a request has carried its assert (the run's next read, or its commit). A run that ends
    without a commit (it failed locally, or wrote nothing) and still has unverified reads sends one query of
    its asserts first (`Recording.verify`); a failed one re-runs the handler on the current document. So no
    answer and no failure reaches the caller on an unchecked cached read: the survey's red line.
  - Filled by: every `get` and select answer (queries too), a conflict's `current`, and the client's own
    commits (a document read then written is cached as computed; a deleted one is dropped). Not cached: a
    `find`'s answer, a document the run inserted (its `_creationTime` is the server's to say) or wrote without
    reading. A commit that failed or may have applied drops what it wrote from the cache. A conflict that does
    not say which documents are stale drops them all. Query handlers do not read the cache.
  - Tests: "read path 4: …" (six tests; the brief's "stale-cache mutation that cannot commit" is the second)
    and "read path: makeMove is two round trips cold and one warm; …" on the sample app.
  - Verified: `deno task ci` exit 0 (340 unit, 61 property); `work-scripts/bash/e2e-linux.sh`
    `E2E PASS seed=20261010 scenarios=8`; `make platform-up` exit 0 with the smoke now playing four moves
    through the edge (`makeMove round trips through the edge: warm cache 1 and 1, cold 2, stale cache 2`,
    `platform smoke OK`), `make platform-down` exit 0 and nothing left.
  - Round trips for `makeMove` (the survey's representative mutation; one `get`, one commit). Before
    (`8075b8d`, from its tests): 2, and 4 when the commit conflicts (the reads again, then the commit). After:
    2 cold, **1 warm**, and a conflict or a stale cache costs **one** more (the refused request carries the
    document). Measured on the real platform by the smoke, and pinned by the unit tests above.
  - Deviations: (1) the survey's "queries do not use the cache unless the call opts in to staleness": the
    opt-in is not built; queries never read the cache. (2) The cache is on by default; the survey did not say.
    It cannot produce a wrong answer, and the one-trip mutation is the point of the change. (3) The first
    mutation after `createGame` is still two trips, because an inserted document is not cached.
  - Local logic later: `ReadCache` is the seam a local store would implement (documents with versions);
    `unverified` + `verify` is the rule a speculative run needs (compute on local data, let the server check).
    The trade taken: the cache holds documents only, not select answers, so a `find` is always a round trip;
    caching selects safely needs the table's sequence to be known locally, which is what a synced local store
    would bring.
- Explainer: **done** `98bd9bf`, `docs/colocated-runner-explainer.md` (changes 5, 6 and 7; no code). Its
  local-logic verdict: feasible as an evolution; nothing in changes 1–4, the runner or in-agentd execution
  forecloses it; the full version needs a local store and a change feed, neither needed yet.
- Candidate that could ride along later (named by the explainer, **not built**, for Nick): a mutation's answer
  carrying the request's clock reading (the one reading of spec §6.3). The client would then know the
  documents its commit inserted exactly, so inserts become cacheable (the first `makeMove` after `createGame`
  drops from two round trips to one) and a local store could apply its own commits without reading them back.
- After the changes: `64b422e` (not a simplification, so its own commit): an oversized commit (`ir-op-limit`)
  and an unserializable answer were reported before the run's cached reads had been asserted; they now go
  through the same check as a handler's own failure.
- Simplify pass: **done** `0b533bb` (no behaviour change). The Go executor reads `_mansio_changes` with one
  statement for every request (the second statement shape, the pre-scan for table asserts and its flag are
  gone); the recording executor builds restarts and its own conflicts through two helpers and computes a
  select's key once; the test model computes a table's last change in one place. The client's run loop was
  collapsed to one outcome path in `64b422e`. Reviewed and left alone: `witness.go`, the decoders,
  `sdk/cache.ts`. Docs brought in line: `SKIM.md` excerpts `799dc37`; knowledge entry
  `docs/knowledge/intermediate-representation/2026-11-10-read-path.md` `a326e64`.
- Final verification (this VM, 2026-11-10, branch head after the simplify pass; logs `/tmp/rp/s-*.log`):
  - `deno task ci` exit 0: fmt, lint, check, 340 unit tests, 61 property tests.
  - Go: `gofmt -l .` clean, `go vet ./...` ok. `go test -race ./...` fails only tests that fail or flake on
    the untouched baseline: `cmd/agentd` `TestAdminFetchLimitAppliesToTheNextFetches` (every run) and
    `TestDrainTimeoutIsReported`, `internal/intermediate-representation` `TestIndexNamesDecodeFast` (all three
    listed in the section below), `cmd/sqlbench` `TestInterruptMidRunFinishesFiles` (2 of 6 on `8075b8d` in
    the merge worktree, 1 of 4 here; the package imports nothing this branch changes), and once
    `cmd/frontbench` `TestKeygenSeedAndRunEveryMode` ("0 req" in a timed run while the whole suite ran; 14 of
    14 alone here, 6 of 6 on the baseline). `go test -tags noassert ./...` fails only
    `TestAdminFetchLimitAppliesToTheNextFetches`.
  - `work-scripts/bash/e2e-linux.sh`: `E2E PASS seed=20261010 scenarios=8`.
  - `make platform-up` exit 0:
    `smoke: makeMove round trips through the edge: warm cache 1 and 1, cold 2, stale
    cache 2`,
    `platform smoke OK`; `make platform-down` exit 0, `.platform/` gone.
  - Not run: the gated Go suites that need `MANSIO_PG_URL` / `COLDLOAD_S3` (no code they cover changed).
- Not done, for Nick: no cross-family review ran on this branch; the brief directed Fable to merge after the
  simplify pass. Open candidates: the clock reading in mutation answers (above); a query-side opt-in to cached
  reads (survey change 4's "unless the call opts in to staleness").
- Next: nothing is running and nothing is pending for this work. For Nick to decide: change 5 (the explainer
  ends with the build order an approval would start), and the two open candidates above.
- Resume / cleanup: branch `feat/read-path-1234` (worktree `/root/Software/mansio-work/read-path-1234`) and
  the merge worktree `/root/Software/mansio-work/main-merge` are left for inspection; both can be removed with
  `git worktree remove`. No platform is up (`docker ps | grep mansio-platform` is empty). Scratch logs:
  `/tmp/rp/`.

## 2026-11-10 — backpressure fixes B1–B11 (Fable 5.2 build + simplify + merge, Nick-directed)

- Goal/spec: brief `/root/briefs/mansio-backpressure-fixes-brief.md`; findings and order in
  `/root/Software/book-notes-work/backpressure-plan/books/backpressure-in-microservices/APPLICATION-PLAN.md`
  (section 3: five waves). One commit per finding, then a simplify pass, then merge to main from
  `/root/Software/mansio-work/main-merge` (never the main checkout). Notes per finding:
  `server/knowledge/backpressure/`.
- Status: **merged to main and pushed: merge `039b7ed`** (`git merge --no-ff feat/backpressure-fixes` on top
  of `e9488c5`, pushed 2026-11-10; `origin/main` = `039b7ed` before this note's own commit). Every in-scope
  finding is implemented, the simplify pass is done, verification is at baseline.
- How it was merged: `main` is checked out in `/root/Software/mansio` (Twin's checkout, not to be worked in),
  so git refuses a second worktree on that branch. The merge was made in
  `/root/Software/mansio-work/main-merge` with a detached HEAD at `main`'s commit and pushed with
  `git push origin HEAD:main`. **The local `main` ref and Twin's checkout are still at `e9488c5`**: run
  `git pull --ff-only` there to catch up.
- Verified on the merged tree (identical to the verified branch head, `git diff 2fa420b 039b7ed` is empty):
  `make platform-up` → `platform smoke OK`, `make platform-down` clean; `gofmt`, `go vet`, `go test -race` on
  the 23 touched packages (only the baseline `TestAdminFetchLimitAppliesToTheNextFetches` fails, as on
  `e9488c5`); `deno task test` 320 passed.
- Per finding (commit; overload scenario or main test):
  - B1 (wave 1): **done** `0277e36` (+ notes moved `77778e0`). Per-queue `mansio_queue_*` families, actor
    depth in durable mode, `mansio_vactor_dirty`, controld pgx pool stats. `TestOverloadIsVisiblePerQueue`.
  - B2 (wave 2): **done** `65d1031`. Deadlines on every query-listener route; tenancy reads the body before
    taking a slot. `TestStalledBodiesHoldNoSlot`.
  - B3 (wave 2): **done** `3164ec6`. Edge refuses before reading the body; `max_in_flight`.
    `TestRefusalsCostNoBodyRead`. Deviation: the pre-body check looks at the bulkhead and breaker but holds
    nothing while the client sends (see `2026-11-10-edge-admission.md`).
  - B4 (wave 2): **done** `b5071dd`. `locator.load_concurrency` (10) and `locator.create_concurrency` (2);
    budget enforced at parse time. `TestLoadSlotsBoundStoreReads`. Deviation: `pg.max_conns` default 20 → 32
    and `api_concurrency` kept at 16 (the plan's example cut it to 4, which breaks `TestGate`'s 8-way
    creates).
  - B5 (wave 2): **done** `2429639` (+ test timeout `89f3c30`). `drain_delay` on edge and controld (default
    0), controld closes at its budget, snapshot and LIST contexts.
  - B6 (wave 3): **done** `0b0edec`. `verify_slots` before the credential checks, per-database claim flights
    instead of the global mutex, drain before the body. `TestOpsCredentialChecksAreBounded`,
    `TestClaimStormIsBoundedAndObeysDeadlines`.
  - B7 (wave 3): **done** `0c2987e`. Would-expire shedding at admission. `TestOverloadShedsInsteadOfExpiring`
    (before 203 expired / queue 61, after 0 expired / 203 shed / queue 9).
  - B8 (wave 4): **done** `9d39f8b` + `fb70e0e`. One contract table (`server/internal/front/contract.go`) →
    `docs/retry-contract.md` and the shared vectors `intermediate-representation/vectors/retry.json`, run by
    both clients; the SDK stops retrying `expired`; reroutes after the first are charged to the budget.
    Deviation (`fb70e0e`): the locator's `moving` is not charged, because charging it failed requests during
    routine moves (`TestClientGate`).
  - B11 (wave 4): **done** `253976e` (one resizable `slots.Pool`, `mansio_queue_depth`) + `15cc322`
    (`server/docs/capacity.md` generated from the config defaults,
    `server/deploy/prometheus/backpressure.rules.yml`, runbooks, levers: `agentctl limit`, `controld limit`,
    the edge's `admin_listen`).
  - B10 (a) (wave 5): **done** `9db6910`. The write-back window stated exactly in `README.md` and `docs.md`;
    alert `MansioUnpublishedWritesOld`; `TestStoreOutageShowsAsGrowingDirtyAge`.
  - B9, event-log retention (wave 5): **done** `4c50817`. `event_log.max_total_bytes` (4 GiB) and `max_age`
    (45 days) enforced by the writer; records lost at close are counted. `TestRetentionBoundsTheTree`. Not
    done and not gated (outside this brief's wave 5 scope): exporting the owner-slot count and rotating the
    operational log.
  - B10 (b) `max_dirty_age` and B9's warm-set default: **skipped-gated** (Nick's paused LiteFS / write-back
    decision); not to be implemented in this run.
- Baseline (clean worktree at `e9488c5`): failures that exist before any change and are not this branch's:
  `internal/intermediate-representation` `TestIndexNamesDecodeFast` (timing assertion, fails under `-race`);
  `cmd/agentd` `TestAdminFetchLimitAppliesToTheNextFetches` (fails every run here); `cmd/agentd`
  `TestSIGTERMMeetsBudget` (2 of 8 runs under `-race`, same rate on the baseline); `internal/control/locator`
  `TestLoadTimeoutBoundsALoadNobodyWaitsFor` (hung once for the 10 min timeout, passes on rerun);
  `internal/bench` `TestCancelDoesNotWaitOutBackoffs` (1 of 10 under `-race`, same on the baseline).
- Simplify pass: `a235c2d` (no behaviour change): one `tlsx.Loopback` instead of two copies, the edge's verify
  slots on the shared `slots.Pool`, an unused accessor removed, `vactor.Host.Open` renamed `OpenDBs`. The
  larger simplification landed inside B11 (`253976e`): one resizable `slots.Pool` replaced five
  channel-semaphore + counter pairs. `7a7f2e3` corrects one metric's description.
- Verification (final, at `7a7f2e3`, this VM, 2026-11-10; logs under `/tmp/bp/final-*.log`):
  - Go, clean snapshot worktree: `gofmt -l .` clean, `go vet ./...` ok; `go test -race ./...` 60 packages ok,
    failing only the baseline tests listed above (`cmd/agentd` `TestAdminFetchLimitAppliesToTheNextFetches`,
    `TestSIGTERMMeetsBudget`, `TestDrainTimeoutIsReported`; `internal/bench`
    `TestCancelDoesNotWaitOutBackoffs`; `internal/intermediate-representation` `TestIndexNamesDecodeFast`);
    `go test -tags noassert
    ./...` 62 packages ok, failing only
    `TestAdminFetchLimitAppliesToTheNextFetches`.
  - Gated Go suites with throwaway Postgres + SeaweedFS (`MANSIO_PG_URL`, `COLDLOAD_S3`): `cmd/controld` (all
    gates, including `TestGate`, `TestClientGate`, `TestLocatorGate`) and `internal/placement/pg` pass.
  - `deno task ci` exit 0: fmt, lint, check, 320 unit tests, 61 property tests.
  - `work-scripts/bash/e2e-linux.sh`: `E2E PASS seed=20261010 scenarios=8`.
  - `make platform-up` → `platform smoke OK` (write and read through the edge), `make platform-down` clean;
    run before and after the simplify pass. On the live platform: the new `mansio_queue_*` series on controld,
    both agents and the edge, `mansio_actor_queue_depth` in durable mode, and `controld limit` changing
    `mansio_queue_capacity{queue="resolve_load"}`.
- More baseline flakes met on the way (each reproduced on the untouched baseline): `cmd/agentd`
  `TestDrainTimeoutIsReported` (6 of 12 under `-race` on the baseline); `internal/metrics` `TestProcess` and
  `internal/bench` `TestCancelStopsSendingButFinishesTheFiles` (once each under load; both tests untouched).
- Open, for Nick: B10 (b) `max_dirty_age` and B9's warm-set default (gated, not built); B9's owner-slot count
  and operational-log rotation (not gated, outside the brief's wave 5 scope); `promtool` is not installed
  here, so `backpressure.rules.yml` is parsed as YAML and its series are checked, but the rules were not
  loaded by a Prometheus.
- Next: nothing is running and nothing is pending for this work. For Nick to decide: the two gated items, and
  whether to take the two small ungated B9 leftovers (owner-slot count, operational-log rotation).
- Resume / cleanup: branch `feat/backpressure-fixes` (worktree
  `/root/Software/mansio-work/backpressure-fixes`) and the merge worktree
  `/root/Software/mansio-work/main-merge` are left for inspection; both can be removed with
  `git worktree remove`. The scratch worktrees and the throwaway test containers this run made were removed.
  How to run the Postgres/S3-gated Go suites on this VM:
  `server/knowledge/backpressure/2026-11-10-running-the-gated-suites.md`.

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
  `internal/bench`, `internal/intermediate-representation`, and `internal/objstore`; these files are
  unchanged. These wider checks are not green and require a separate coordinator investigation.
- Next: none for this repair. The wider Deno/Go check failures noted below remain a separate investigation.
- Open decisions/blockers: none for this repair. Review/landing belongs to the coordinator.
- Resume: worktree `/root/Software/mansio-work/fix-pages`, branch `fix/pages-after-rename`. Use `git log -1`
  for the repair SHA and `git status --short --branch` for push/worktree status. No detached agent run is
  needed; rerun the three npm commands in `pages/` if validation is needed.
- Learning assessment: preserve the rename/pinned-path gotcha here within the brief's authorized scope; no
  reusable script or additional knowledge-file changes are needed.
