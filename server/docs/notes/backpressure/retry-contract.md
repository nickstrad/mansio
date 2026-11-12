# One retry contract for both clients

The single generated retry table that the Go client, the TypeScript SDK and controld are all tested against.

Source: `server/internal/front/contract.go` (`Contract()`: the agent's answers from `front.Spec`, which gained
an `Action` column, plus controld's resolve answers). Generated from it by
`go test ./internal/front -run TestContractFiles -update`: the user document `docs/retry-contract.md` and the
shared vectors `intermediate-representation/vectors/retry.json`. Held to it: the Go client
(`internal/client/contract_test.go`), the TypeScript SDK (`tests/unit/sdk_retry_contract_test.ts`), controld's
resolve answers (`TestResolveAnswersMatchTheContract`), and the committed files themselves
(`TestContractFiles`).

What changed in behaviour:

|                                                                                                    | Before                                                                                                | Now                                                                                                                               |
| -------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| SDK on `expired`                                                                                   | retried (up to 3 attempts), resubmitting work whose deadline had passed                               | final: `Unavailable{code: "expired", mayHaveApplied}`; the Go client's default already did not retry it                           |
| `Retry-After` cap                                                                                  | 60 s on the server and in the Go client, 30 s in the SDK                                              | 60 s everywhere                                                                                                                   |
| Reroutes after an agent's answer or silence (`wrong_owner`, `fenced`, `ticket_invalid`, no answer) | never charged to the retry budget; the first waited only the server hint (0 when there was no answer) | the first is free and waits a jittered delay of up to the backoff base (50 ms); each later one backs off and takes a budget token |
| Reroute on resolve `moving`                                                                        | not charged                                                                                           | still not charged (see below); it now also waits at least the jittered backoff                                                    |
| resolve `no_capacity` (503 + `Retry-After`)                                                        | failed the call in both clients                                                                       | retried like `unavailable`                                                                                                        |

Why the reroute change matters: every transport failure is a reroute, so an agent going away turned each call
in flight into up to five immediate re-resolves against controld. With a budget of one token the Go client now
makes 3 resolves where it made 6 (`TestReroutesAfterTheFirstSpendTheBudget`; the SDK test shows the same 3
against 6 and waits of 25 ms and 50 ms where the first was 0).

Not changed: a bench profile may still opt in to retrying `expired`, `stmt_timeout` or `internal` through
`retry_on` (that is how frontbench models a badly behaved client); the contract and the vectors describe the
default policy. `front.Spec`'s prose column still says what the caller may do itself (for `expired`: retry
with a fresh deadline, same request id).

Why `moving` is not charged. Charging every reroute after the first to the budget, the locator's `moving` too,
made `TestClientGate` fail (gated: Postgres + SeaweedFS; frontbench in locator mode, 16 callers on 4
databases, a move every second): 4 of about 15 600 requests during moves ended `resolve:moving`. A planned
move makes every caller of that database reroute two or three times at the same moment, and the 10-token
bucket cannot absorb that burst. `moving` is controld itself pacing clients with `Retry-After`, not a failure,
so it stays uncharged (the older comment in `decide.go` said the same); the reroutes that are charged (an
agent that answered `wrong_owner` or `fenced`, or did not answer) are charged after the first. With that the
gate passes (4 of 4 runs).
