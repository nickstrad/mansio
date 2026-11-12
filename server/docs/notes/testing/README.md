# Testing

How the Go server is tested: domain code separated from integration code, contracts asserted in code, and a
fixed order of test techniques from table tests to model-based fuzzing. Shared code: `internal/assert`; fakes
with fault switches live in `<pkg>test` packages (`internal/vactor/vactortest`, `internal/objstore/s3test`,
`internal/faultnet`). The approach follows Maurício Aniche, _Effective Software Testing_.

- [Running the Postgres- and S3-gated Go suites](gated-suites.md): throwaway Postgres and SeaweedFS for
  `MANSIO_PG_URL` / `COLDLOAD_S3`, and the bucket that must be created by hand.
- [sqlbench: what the testing approach found](sqlbench-lessons.md): the DSN `#` bug found by model fuzzing, a
  boundary found by a property test, a whole-run invariant, a pure pacer.
- [Timing tests under load](load-tolerant-timing.md): assert attempt counts, not elapsed time; a "flaky"
  objstore test was a real retry bug (a transport timeout classified as the caller's).
- [sqlbench: a retry registered after the cancel sweep was waited out](cancel-backoff-race.md): a
  check-then-act race between a cancel sweep and a backoff registration; read the flag under the sweep's lock.

## 1. Design so it can be tested: domain vs integration

- **Domain** packages hold decisions and rules and do **no I/O**; they reach the world only through small
  interfaces (ports) that they define. Pure functions for decisions (`plan(have, want) → moves`).
- **Integration** code (adapters, `cmd`) is thin: translate between the world and domain types, validate
  input, log. `main` only wires; it calls `realMain(ctx, args, stderr) int` so tests can drive it.
- coldload example: `internal/coldload` (config validation, level plan, `RunLevel` via a `Getter` port, CSV
  row layout) and `internal/vactor` (virtual-actor host via `Fetcher`/`Opener` ports) are tested with fakes in
  milliseconds; `internal/objstore` (S3), `cmd/bench/coldload/adapters.go` (actor opener), `template.go`
  (SQLite), `recorder.go` (CSV) are the adapters.

## 2. Contracts in code: preconditions, postconditions, invariants, assertions

**Validation vs contracts**:

|                | Validation                                       | Contract (assertion)                         |
| -------------- | ------------------------------------------------ | -------------------------------------------- |
| Guards against | bad input: users, files, network, other services | bugs in our own code                         |
| Where          | the edge: config loaders, handlers, adapters     | inside: pre/postconditions, invariants       |
| On failure     | return an error                                  | `assert.That` panics before bad data spreads |

- **Never assert on input.** Ask "could a typo, a bad file or a hostile client trip this?" If yes → error.
- **Validate once at the edge, then trust.** coldload: `Config.Normalize` is the only validation; later code
  states it as a precondition (`pre: c was normalized`) and asserts it cheaply.
- **Write the contract in the doc comment**, one line each:

  ```go
  // Get returns key's open database, activating it if needed.
  //
  //	post: err == nil ⇒ res.DB != nil, and the key stays active until passivated
  //	post: res.Act != nil ⇒ this caller started the activation, so res.Waited is false
  //	post: a failed activation is not cached: the next Get tries again
  ```
- **Assert where it is impactful**, not everywhere:
  - interfaces any future adapter could break → a _checked wrapper_ that asserts the postconditions on every
    call (coldload: `vactor.checkedFetcher`: file size == `Bytes`, no `.part` left, no file after an error);
  - state that, if wrong, corrupts results silently (coldload: activation either fully succeeded or left
    nothing; recorder `written + dropped == added`; `RunLevel` `Dispatched == OK + Failed`);
  - outputs of planners consumed elsewhere (coldload: every planned level has ≥ 1 key and `Conc ≥ 1`).
- **Invariants run after mutations, never on reads.** Guard O(n) checks with `if assert.Enabled { … }`
  (`assert.That` evaluates its arguments even when disabled).
- **On by default; `-tags noassert` compiles them out.** Test both builds. A failed assertion is a bug: the
  top level reports it as exit 70 with a log ID.
- **Postcondition checks must not mask panics**: do not put assertions in a `defer` that also runs while a
  panic unwinds (coldload hit this: the assertion fired first and hid the real panic; moved into the return
  path).
- **Each assertion must be able to fail in some test** (mutation thinking): flip the condition — does a test
  catch it? e.g. `TestRecorderDropsWhenFullInsteadOfBlocking` exercises the recorder invariant.

## 3. Test levels and techniques (in the order to apply them)

1. **Specification-based, table-driven**: one case per partition, plus **on and off points** of every boundary
   (0/1/many, empty, limit and limit+1). coldload: `TestParseSize` (1 byte on point, 0.5 and `0.0001K` off
   points, 1 PiB on/off, NaN/Inf), `TestNormalizeRejects`, `TestLevels` (PerLevel < / > Count, cursor wrap,
   dup level), `TestRunLevelConcurrencyBound` (c=1, c>keys, 1 key), recorder buffer 0/1/over.
2. **Structural**: `go test -coverpkg=./... -coverprofile=c.out ./... && go tool cover -func=c.out`; read
   uncovered blocks: error branch → inject with a fake; impossible → assertion; dead → delete.
3. **Fakes with fault switches** in `<pkg>test` packages, mutex-guarded: coldload `internal/vactor/vactortest`
   (`Fetcher{Down, Delay}`, `Opener{FailOpen, FailQuery}`; the fake db fails if its file vanished — that
   catches use-after-delete races).
4. **Contract suite run against every implementation** (Liskov): `vactortest.RunFetcherSuite` runs against the
   fake always (`TestFakeFetcherContract`) and against real S3 when `COLDLOAD_S3` is set
   (`cmd/bench/coldload/s3_integration_test.go`). A unit test against a fake only means something if the fake
   passes the same suite as the real adapter.
5. **Model-based fuzzing** for anything stateful: bytes → a script of ops, run against the real thing and a
   trivial model, compare after every step. coldload `FuzzHostModel` (Get / Passivate / toggle fetch down /
   toggle open failure / PassivateAll, with and without KeepLocal; model = active keys, files on disk, fetch
   counts). It found a real leak on its first run: a fetched file stayed on disk when open failed. Commit
   `testdata/fuzz/` failures as regression tests.
6. **Integration tests** per adapter incl. failure paths and error wrapping (`errs.Outermost(err).Module`,
   `errors.Is` sentinels): S3 down → wrapped `objstore` error, not `ErrNotFound`; `actorOpener` never creates
   a missing file; template builder reuse/rebuild.
7. **Top level / few e2e**: `TestRealMainExitCodes` (usage 2, ok 0, bad config 1 with log ID),
   `TestRealMainStoreDownIsAnExpectedErrorWithLogID` (printed ID is in the JSON log with the chain); manual
   e2e: `scripts/coldload/run.sh configs/coldload/smoke.json all` and SIGINT → exit 130, cache removed.
8. **Concurrency regressions with `-race`**: `TestGetRacingPassivateNeverServesDeletedFile`,
   `TestConcurrentGetsOnColdKeyShareOneActivation`, `TestRunLevelForwardsWorkerPanic`.

## 4. Failures below the interface, object storage, SQL, and time

**Failures below the interface** (a dependency fails in ways its interface does not show):

- Every adapter's contract suite runs three ways: against the fake, the real service, and the real service
  through `internal/faultnet` (TCP faults) or `internal/objstore/s3test` (HTTP/S3 faults).
- Minimum fault table: refused, accept-then-silence/blackhole, headers-then-stall, dribble, 5xx storm,
  applied-but-reply-lost, truncated/corrupt body, half-open after idle.

**Object storage**:

- Run `TestS3Compat` (X1–X12) against every S3 backend and version before use; record results in
  [the S3 compatibility note](../writeback/s3-compatibility.md).
- Fault switches `ApplyThenStale`, `LoseUploadReply` (durabletest), `ApplyThenPreconditionFailed` (s3test).
- `FuzzDurableHostModel` carries the GC op and its invariant (never delete current, pending, or K retained).

**SQL**:

- Every `.sql` file has a golden-output regression test (`go test ./queries -update` to refresh; review
  diffs).
- Every schema constraint has an off-point test (a row that must be rejected).
- CAS and other contended paths get a test with many workers on the same key.

**Under load and over time**:

- Fault modes also run during a frontbench/wbbench run; soak with idle hours before calling a component done.

## Commands

```sh
go test -race ./...                         # unit + integration (S3 tests skip without COLDLOAD_S3)
go test -tags noassert ./...                # same with assertions compiled out
COLDLOAD_S3=localhost:8333 go test ./cmd/bench/coldload -run S3
go test -run='^$' -fuzz=FuzzHostModel -fuzztime=30s ./internal/vactor
```

## Checklist for a change

- [ ] Doc comment states pre/postconditions; invariants named on the type.
- [ ] Input validated at the edge with errors; contracts asserted inside (impactful ones only).
- [ ] Table tests per partition with on/off boundary points.
- [ ] Each assertion can fail in some test.
- [ ] Fakes with fault switches; contract suite run against fake, real, and real through a fault proxy.
- [ ] Adapters tested against the minimum fault table.
- [ ] A model-based fuzz target if the code has state.
- [ ] Adapter integration tests incl. failure paths and error wrapping.
- [ ] New S3 backend or version: `TestS3Compat` run and recorded.
- [ ] New `.sql`: golden-output test; new schema constraint: off-point test; contended path: many-workers
      test.
- [ ] Timing assertions count attempts; upper bounds have seconds of slack.
- [ ] Fault modes run under load; soak with idle hours before calling a component done.
- [ ] `go test -race` and `go test -tags noassert` both pass.
