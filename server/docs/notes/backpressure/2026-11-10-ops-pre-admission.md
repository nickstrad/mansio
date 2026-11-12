# /v1/ops: nothing remote before admission without a bound (B6, 2026-11-10)

Scope: credential modes only (single-node `api-key` / `session` tenancy and `create_dbs`). The ticket path used
behind controld is a local signature check and only gains the drain-before-body order.

Code: `internal/front/ops.go` (`verifyOps`, `OpsConfig.VerifySlots`), `internal/front/http.go` (`ops`),
`cmd/agentd/tenancy.go` (`claimingPlace`). Tests: `TestOpsCredentialChecksAreBounded` and
`TestClaimStormIsBoundedAndObeysDeadlines` (the overload scenarios), `TestConcurrentCreatesRespectTheTenantCap`,
`TestOpsDrainingIsRefusedBeforeTheBody`.

What a credential request does before admission, and what now bounds each step:

| Step | May call | Bound |
|---|---|---|
| drain check | nothing | now before the body is read |
| `Credentials.Authenticate`, schema lookup, `CredentialPlace` | tenancy store, schema registry, placement authority | `verify_slots` (default 64): a request that finds none free is `shed` at once; `queue="verify"` |
| claim or create of a database not active here | placement authority (several calls) | one flight per database shared by its requests; at most `lifecycle_concurrency` flights; another database beyond that is `shed`; `queue="credential_place"` |

Before, the claim path took one process-wide `sync.Mutex` across the authority calls: with a slow placement
store and many first-use databases, requests queued on it without limit, in no gauge, and without regard to
their deadline. Measured in the tests: with the store's calls at 300 ms and 2 flights allowed, a third
database is refused in under 100 ms, a waiter with a 50 ms deadline leaves at 50 ms, and the flight still
answers the other waiters; with the tenancy store stuck and 2 verify slots, 10 of 12 requests are shed while
it is stuck and ticket requests are served throughout.

Design points:

- A flight runs on its own goroutine under `lifecycle_timeout`, not under the first caller's context: a caller
  that gives up does not cancel the claim for the others, and the work is still bounded.
- Creates are serialized among themselves (one slot), because the caps count the keys the owner table knows and
  a key enters it at its claim; claims of existing rows run in parallel. The caps stay exact.
- Warm requests are unchanged: the fast path (`tab.State(db)` is active) takes no lock and no flight, and a
  warm request holds its verify slot for the microseconds of three cache lookups.
- Not done: `front.Server` still counts requests in flight without capping them (the plan lists it as evidence,
  not as a change); the verify slots and admission are the caps.
