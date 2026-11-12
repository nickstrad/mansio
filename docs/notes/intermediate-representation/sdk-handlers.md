# The SDK runs handlers and talks `/v1/ops`

How the SDK (`sdk/`) turns a handler into IR requests, tells its own conflicts from the app's, retries and
routes requests, and how the e2e suite runs it against the Go server. For how reads and commits work since the
read-path changes (one snapshot, guarded finds, cached reads), see [the read path](read-path.md).

## The pieces

| File                      | Role                                                                                                |
| ------------------------- | --------------------------------------------------------------------------------------------------- |
| `sdk/client.ts`           | `createClient({functions, schema, tenant, app, endpoint+token \| locator, …})`; handler retry loop  |
| `sdk/recording.ts`        | `Recording`: a `WriteDB` that reads remotely, buffers writes, derives `assert`s                     |
| `sdk/overlay.ts`          | read-your-writes for `find` (pure)                                                                  |
| `sdk/transport.ts`        | `/v1/ops` sender (port of the Go client, `server/internal/client`), `staticRouter`, `locatorRouter` |
| `sdk/retry.ts`            | policy, windowed budget, `decide` (pure)                                                            |
| `sdk/ids.ts`, `ticket.ts` | UUIDv7 ids; dev ticket minter (byte-identical to `internal/ticket` v1, Go golden in the unit test)  |
| `tests/e2e/platform/*`    | `startGoAgent`, `client()` (the SDK)                                                                |

## How a mutation becomes one request

1. `client.mutation(name, args)` looks the handler up locally (`UnknownHandlerError` 404 /
   `HandlerKindMismatch` 400) and runs `definition.invoke(recording, args)`.
2. `recording.get(t, id)`: first time, one query request `[get]`; the answer is the entry's **base** and the
   entry is **guarded**. Later gets answer from the cache, with the run's buffered writes applied (a patch
   bumps `_version` as the server will; an insert's `_creationTime` is the caller's clock — provisional).
3. `recording.find(...)` builds the same `SelectPlan` as `Documents`; with no buffered writes to that table it
   is one query `[select]`. With writes it sends **one** query (one snapshot)
   `[select with limit + displaced,
   get displaced…]`, where _displaced_ are the documents the run patched
   or deleted (an insert's fresh id has no server row, so it is not counted or fetched). A displaced document
   the run had not read takes that get as its base. `overlay` drops the touched documents' server state, adds
   their local state where it matches, and re-sorts/re-cuts (`count`/`countBy` adjusted the same way): exact,
   because at most `displaced` rows (groups) drop out. The widened limit is capped at 1000 (no cursor in v1):
   when the cap bites and the server's answer is full, the correction is accepted only if it still has `limit`
   entries none of which sorts after the server's last row; otherwise the handler fails `RuleError find-limit`
   (loud, never short). More than 63 displaced documents in one table fails `RuleError ir-op-limit`.
4. `insert` draws a UUIDv7 and answers it; `patch`/`delete` answer nothing; all are appended to the op list. A
   patch of a document the run knows is missing fails `NotFoundError` locally.
5. On return: `ops = [assert per guarded entry (version, or exists:false)] ++ writes`. Empty → no request (a
   handler that read nothing and wrote nothing). More than 64 ops fails locally (`RuleError ir-op-limit`,
   400). Otherwise one mutation with a fresh `requestId` per handler run (transport retries of that commit
   resend the same body and id).

**Derived vs app conflict.** Asserts are always the first `asserts` ops, so a `ConflictError` whose
`op <
asserts` is ours (re-run), and so is `check: "exists"` on an insert (a client-id collision: a re-run
draws new ids). Every other conflict goes to the caller. App conflicts never reach the server: `makeMove`
checks `expectedVersion` against what it read and fails locally. Re-runs: `handlerRetries` (4), pause
`random · min(250, 10·2^run)` ms, never past the call's deadline (`timeoutMs`, 10 s, covering all runs).

**Errors.** Read failures inside a handler escape as a private defect (`Escape`) so `DbError` stays the
handler's channel, and come out as the `ClientError` they were. Handler failures become `RemoteError` with
`toHttpStatus` of their tag and the error as `cause` (so e2e `remoteError(…, 409, "ConflictError")` holds).
`Unavailable` (`core/errors.ts`) is "no definite answer": `code`, `mayHaveApplied`.

## Transport

Same decisions as `internal/client/decide.go`: `shed`/`unavailable`/`aborted`/`expired` retry with
`max(Retry-After, full jitter 50 ms·2^n ≤ 5 s)`, 3 attempts, budget 0.1/first with burst 10;
`wrong_owner`/`fenced`/`ticket_invalid`/no answer reroute (≤ 5, no budget). The body is built once with
`canonical`, so a retried mutation keeps its `requestId` and a commit that landed answers `duplicate: true`.
`X-Timeout-Ms` = time left minus min(10 %, 100 ms). Static routes: a no-answer retries the same endpoint, a
`ticket_invalid` asks a token function again, `wrong_owner`/`fenced` are final (`Unavailable`). Locator
routes: `POST /v1/resolve {"tenant","app"[,"create"][,"min_epoch"]}`, cached until TTL − min(TTL/10, 1 s),
background refresh at 80 % ± 10 %, single-flight, `min_epoch = epoch + 1` after `wrong_owner`/`fenced`, the
primary's answer clears it. Unit-tested against a fake controld; the `scale_out` e2e scenario runs it on a
cluster ([multi-node](multi-node.md)).

## E2E

```sh
scripts/dev/e2e-linux.sh                                # all scenarios on the Go server
scripts/dev/e2e-linux.sh --filter "/documents|queries/"
deno task e2e                                           # on a Linux host with Docker and Go
```

The script exists because the suite needs Linux and Docker Compose cannot publish Postgres to a container's
loopback: it runs `postgres:17` on a private network (`E2E_POSTGRES_URL`, which `main.ts` accepts instead of
starting Compose), cross-compiles agentd for Linux on the host (`AGENTD_BIN`), and mounts the checkout and its
git common dir at their host paths (the hermeticity check runs `git status`). `startGoAgent` writes a fresh
Ed25519 issuer (`keys.json`), an empty `local:<app>.db` per app (static mode serves only files present at
start), `schemas/<app>/v1.json` in each app's layout, and an agentd config with `listen 127.0.0.1:0` +
`-addr-file`; tickets are minted per request kind (read for queries, write for mutations), 10 min TTL. Both
layouts: the scenarios loop over `tic-tac-toe` (columns) and `tic-tac-toe-json` (json).

## Tests

- `tests/unit/sdk_client_test.ts`: sample-app op lists, app vs derived conflicts, lost reply → duplicate,
  shed/aborted/expired, budget, ticket refresh, locator re-resolve, ticket golden, UUIDv7.
- `tests/property/sdk_test.ts`: derived asserts = the gets (versions), asserts first, reads equal a shadow
  model with the writes applied, committed state = the handler's view; concurrent increments under faults
  converge (`ok ≤ n ≤ ok + ambiguous`, exact without faults). Mutation-checked: dropping the overlay, guarding
  after a write, and changing the body between retries are each caught.
- `intermediate-representation/vectors/execute.json`: 4 SDK-generated scenarios × 2 layouts (`sdk: …`, from
  `scripts/intermediate-representation/gen-vectors.ts` `recordSdk`): what the SDK sends for
  createGame/makeMove/deleteGame and a derived-assert conflict, run by Go docexec (`TestExecuteVectors`).

## Traps

- **Schema layout is part of the fingerprint.** A client for `tic-tac-toe-json` must pass
  `withStorage(schema, "json")`, else every request is `SchemaDriftError`.
- **`createClient` throws** without `endpoint`, `locator` or `transport` (programming error at startup).
- **The deterministic simulation does not run the TS SDK.** The cluster simulation uses a Go client with the
  SDK's rules (same request id on every attempt, reroute and retry as above); the `sdk:` vectors pin what the
  SDK sends (asserts first, same body on transport retry, new id on re-run). To drive the SDK itself
  deterministically, its seams are `transport`, `fetch`, `now`, `random`, `sleep`, `newId` and `newRequestId`.
- `find` inside a mutation was unguarded in IR v1. Since the read-path changes a `find` asserts its whole
  table unless it is `.unguarded()`, and a run reads at one snapshot ([the read path](read-path.md)).
