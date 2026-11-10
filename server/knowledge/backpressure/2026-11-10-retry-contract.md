# One retry contract for both clients (B8, 2026-11-10)

Source: `server/internal/front/contract.go` (`Contract()`: the agent's answers from `front.Spec`, which gained
an `Action` column, plus controld's resolve answers). Generated from it by
`go test ./internal/front -run TestContractFiles -update`: the user document `docs/retry-contract.md` and the
shared vectors `ir/vectors/retry.json`. Held to it: the Go client (`internal/client/contract_test.go`), the
TypeScript SDK (`tests/unit/sdk_retry_contract_test.ts`), controld's resolve answers
(`TestResolveAnswersMatchTheContract`), and the committed files themselves (`TestContractFiles`).

What changed in behaviour:

| | Before | Now |
|---|---|---|
| SDK on `expired` | retried (up to 3 attempts), resubmitting work whose deadline had passed | final: `Unavailable{code: "expired", mayHaveApplied}`; the Go client's default already did not retry it |
| `Retry-After` cap | 60 s on the server and in the Go client, 30 s in the SDK | 60 s everywhere |
| Reroutes (`wrong_owner`, `fenced`, `ticket_invalid`, no answer, resolve `moving`) | never charged to the retry budget; the first waited only the server hint (0 when there was no answer) | the first is free and waits a jittered delay of up to the backoff base (50 ms); each later one backs off and takes a budget token |
| resolve `no_capacity` (503 + `Retry-After`) | failed the call in both clients | retried like `unavailable` |

Why the reroute change matters: every transport failure is a reroute, so an agent going away turned each call
in flight into up to five immediate re-resolves against controld. With a budget of one token the Go client now
makes 3 resolves where it made 6 (`TestReroutesAfterTheFirstSpendTheBudget`; the SDK test shows the same
3 against 6 and waits of 25 ms and 50 ms where the first was 0).

Not changed: a bench profile may still opt in to retrying `expired`, `stmt_timeout` or `internal` through
`retry_on` (that is how frontbench models a badly behaved client); the contract and the vectors describe the
default policy. `front.Spec`'s prose column still says what the caller may do itself (for `expired`: retry
with a fresh deadline, same request id).
