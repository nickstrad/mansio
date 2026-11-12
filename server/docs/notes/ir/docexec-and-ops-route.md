# `internal/docexec` and `POST /v1/ops`

The Go executor for IR requests and the agent route that serves them: request flow, database creation from the
schema registry, error mapping, test results, and known limits.

Spec: [`ir/spec/v1.md`](../../../../ir/spec/v1.md) §6–§8. The actor closures and schema registry it runs on:
[actor closures](../front/actor-closures-and-schema-registry.md).

## What was built

| Piece      | Where                              | Notes                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| ---------- | ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Executor   | `internal/docexec`                 | `Run(tx, schema, req, ids)` (the ops), `CheckExpect`, `Check` (fingerprint vs `_platform_schema`), `Init` (DDL + `_platform_schema`, byte for byte as the TS reference executor did), `Schemas` (decoded-schema cache keyed by text), `Decode`, `CanonicalText` (registry hook), `NewUUIDv7`; port `Tx`/`Rows`. Errors: `*Failure{Err *ir.Error, Cause error}`, `ErrUninitialized`. Domain, imports no adapter; in the det lint list. |
| Actor glue | `internal/docexec/docactor`        | `Call` (an `ir.Request` as `actor.Request{Check, Fn}`, `Wrap` for tests), `InitFn`, `InitRequest`, `Tx` (adapts `*actor.Tx`). Adapter.                                                                                                                                                                                                                                                                                                |
| Route      | `internal/front/ops.go`, `http.go` | `Server.EnableOps(OpsConfig)`, `HandleOps`, `OpsPath = "/v1/ops"`; `Call.Fn/Check`, `Reply.Body/Value`; port `SchemaSource` (+ `ErrSchemaUnknown`). Headers `X-Timeout-Ms`, `X-Trace-Id` (logged), `X-Log-Id` (answer of an internal failure).                                                                                                                                                                                        |
| Wiring     | `cmd/agentd/schemas.go`            | durable: `registrySchemas` over `schemareg.Client` (Validate = `CanonicalText`); static: `schemas_dir` (`<app>/<name>.json`). controld `checkSchema = canonicalSchema`.                                                                                                                                                                                                                                                               |
| Decode     | `core/ir/decode.ts`, `internal/ir` | `db.tenant` / `db.app` use the registry domains; 9 new decode vectors (104).                                                                                                                                                                                                                                                                                                                                                          |

## Request flow

decode (`ir.DecodeBody`, body read to `MaxBodyBytes+1`) → ticket for `tenant:app` (read perm for a query,
write for a mutation) → `Admitter.Admit(db, epoch)` → admission slot → executor with a closure: `Check` reads
`_platform_schema` (one statement; the catalog only if that fails) and compares fingerprints **before** the
actor's request-id claim, so a drifted duplicate answers drift; `Fn` runs the ops; a mutation's `Output.Body`
(canonical JSON of the result array) is the `0x02` record a duplicate replays byte for byte.

A database without a schema record (a new, empty file) answers `ErrUninitialized` from `Check`; the route
fetches the registered text for `(app, request.schema)`, runs `docexec.InitFn` as a write closure (no request
id; it only compares if another request initialized first) and runs the request again. A query does this too
(the DDL is the registry's, not the caller's write). Unregistered fingerprint on a new database:
`SchemaDriftError {expected: null, actual}`.

## Error mapping (`front.opsError`)

| Cause                                                                                                                                     | Answer                                                                                                                                                                                                                |
| ----------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `*docexec.Failure` without `Cause` (decode, validation, expect, unknown table, drift…)                                                    | its tag/status/op/fields (spec §8)                                                                                                                                                                                    |
| statement timeout (`Result.TimedOut`)                                                                                                     | `StatementDeadlineError` 504 (op kept)                                                                                                                                                                                |
| `*docexec.Failure` with `Cause` (a statement failed)                                                                                      | `StorageError` 500 (op kept; cause in the log)                                                                                                                                                                        |
| `actor.ErrRecordKind` (id used by a raw SQL write)                                                                                        | `SchemaError` 400, path `requestId`                                                                                                                                                                                   |
| `*panicx.Panic`, anything unexpected                                                                                                      | `Defect` 500                                                                                                                                                                                                          |
| `ErrShed`/draining, `ErrUnavailable`, `ErrExpired`/`ErrNoReply`, `ErrAborted`, `ErrWrongOwner`/`ErrUnknownDB`, `ErrFenced`, ticket errors | lowercase tags `shed` 503, `unavailable` 503, `expired` 504 (`fields.ambiguous: true` for a mutation without reply), `aborted` 409, `wrong_owner` 421, `fenced` 421, `ticket_invalid` 401; Retry-After as `/v1/query` |
| transport refusals (405, 408, bad `X-Timeout-Ms`)                                                                                         | `SchemaError` with that status; path `body`, or `header:X-Timeout-Ms` (spec §11.14)                                                                                                                                   |
| registry wait or creation write past the deadline                                                                                         | `expired` 504, not ambiguous                                                                                                                                                                                          |

5xx bodies carry no `fields` (spec §8) except `expired`'s `ambiguous`.

## Results

- `execute.json` 122/122 against a real actor (both layouts, final rows and `finalRequests`), 0.4 s. The
  `_mansio_requests` expectation moved from `final.requests` to a sibling `finalRequests` (no clash with a
  table named `requests`), and every vector where no mutation succeeds now carries `[]` (86/122 vectors
  compare the table).
- `decode.json` 104/104 through the live HTTP route (`TestOpsDecodeVectors`).
- `FuzzOpsRoute` (any body, valid ticket, real actor holding the vector schema, seeded with every vector
  request): 2 × 2 min (8.8 M and 8.0 M execs), no 5xx. Its first run "found" a 504: the test rig mixed a
  frozen fake clock (server) with the actors' system clock; the rig now uses the system clock. Not a product
  bug; no crasher kept.
- `FuzzDocExecModel` (pure Go model vs real actor; modes: layout × plain actor / durable host × read pool): 90
  s plain-actor only (56 k execs), then 2.5 min (112 k) and 5 min (59 k execs, corpus 633) over every mode,
  clean; no crasher to keep. `TestDocExecModelRandom` runs 64 seeded scripts over every mode in `go test` (2
  s; ~40 crashes and ~50 passivations among them). Mutation-checked: weakening `expect.fields` equality is
  found in ~25 s.
- `FuzzDecodeRequest` (`internal/ir`) after the tenant/app narrowing: 1 min, 23.6 M execs, clean.

## Known limits

- **Init on a database with tables but no `_platform_schema` row succeeds silently**: the DDL is
  `CREATE … IF
  NOT EXISTS`, as in TS, so a table left by raw `/v1/query` SQL with other columns stays; later
  operations on it may then fail as `StorageError`. Only files created empty (or by this route) are safe.
- **`NewUUIDv7` has no in-millisecond counter**: 48 bits of ms then 74 random bits, so ids drawn within one ms
  are unordered among themselves; a collision (negligible) fails that insert on the primary key as a
  `StorageError` and nothing commits.
- A registry fetch that outlives the request's deadline (or a client that leaves) answers `expired`, not
  ambiguous; so does a creation write with no reply (only the idempotent creation may have committed).

## Lessons

- **A write closure must never run a statement that can fail on purpose.** The first statement error poisons
  the closure's `Tx` (see [actor closures](../front/actor-closures-and-schema-registry.md)), so "try
  `SELECT … FROM _platform_schema`, fall back if the table is missing" is fine in `Check` (read-only Tx) but
  would fail `Init`; `Init` probes `sqlite_master` first.
- **Injected statement failures need the model to count statements.** The model counts docexec's plan (one per
  select/get/load, one per write) so it knows which op an injected failure lands in; a protocol error before
  that point wins. Cheap, and it pins the executor's statement plan.
- **Server ids per request**: the fuzz gives each request its own id prefix, so ids a failed request drew do
  not shift later requests' ids (the actor rolls back rows, not generators).
- Fake clocks: never mix a frozen `fronttest.Clock` for the server with real actors; the executor times its
  wait on the actor's clock.

## Existing model fuzzers and IR

- `FuzzActorModel` (package `actor`, internal test): not extended — `docexec` imports `actor`, so an internal
  test cannot import it (cycle), and closures already have `FuzzActorClosureModel`. IR on a real actor,
  including group-commit batches (2–3 mutations submitted together, each in its savepoint) and the read pool,
  is in `FuzzDocExecModel`.
- `FuzzDurableHostModel` (`vactor`): not extended — it runs on fake databases (`vactortest.Opener`, string
  contents) and models generations under store faults; IR needs real SQLite. Instead `FuzzDocExecModel`'s
  durable mode runs IR on `vactor.Host` + `actordb` + `durabletest.Store` with write-back, passivation and
  crash (the model reverts to the last published snapshot, request ids included, and a retried id applies
  again).
- `FuzzOwnershipModel` (`owner`): not extended — its processes are owner tables with abstract writes, no hosts
  or actors; adding real hosts per simulated process is the job of the
  [simulation harnesses](../determinism/README.md). Route-level fencing (`wrong_owner`, `fenced`) is tested
  through the admitter.
