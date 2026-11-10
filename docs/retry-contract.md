# Retry contract

<!-- Generated from mansio-server's front.Contract. Do not edit: change server/internal/front and run -->
<!-- (cd server && go test ./internal/front -run TestContractFiles -update). -->

What a mansio client library does by itself after each answer. The TypeScript SDK and the Go client follow
this one table; both run it as test vectors ([`ir/vectors/retry.json`](../ir/vectors/retry.json)).

- **done**: the answer is returned.
- **retry**: the same request again on the same route, after the larger of `Retry-After` and a jittered
  backoff.
- **reroute**: the route is dropped, the database is resolved again (or a new ticket fetched), then the same
  request is sent again. The first reroute of a call waits a short jittered delay; later ones back off.
- **fail**: final. The caller gets the error and decides.

A mutation is always sent again with the same request id, so it applies at most once.

| From    | Code             | Status           | `Retry-After` | Client does | Retry budget            | A mutation may have applied | Meaning                                                                                                                                                                    |
| ------- | ---------------- | ---------------- | ------------- | ----------- | ----------------------- | --------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| agent   | `ok`             | 200              | no            | done        | not charged             | no                          | done (duplicate=true: this request_id was applied before; count is the original's)                                                                                         |
| agent   | `shed`           | 503              | yes           | retry       | charged                 | no                          | nothing ran; retry after Retry-After with backoff and jitter                                                                                                               |
| agent   | `unavailable`    | 503              | yes           | retry       | charged                 | no                          | nothing ran; a dependency is down or recovering: retry after Retry-After with backoff and jitter                                                                           |
| agent   | `expired`        | 504              | no            | fail        | not charged             | yes                         | the deadline is spent, so a client library does not retry it; the caller may, with a fresh deadline (ambiguous=true: a write may have committed, keep the same request_id) |
| agent   | `stmt_timeout`   | 422              | no            | fail        | not charged             | no                          | nothing changed; do not retry unchanged (the statement is too slow for the limit)                                                                                          |
| agent   | `aborted`        | 409              | no            | retry       | charged                 | no                          | nothing changed; safe to retry at once                                                                                                                                     |
| agent   | `wrong_owner`    | 421              | no            | reroute     | charged after the first | no                          | re-resolve the database (locator), then retry with the new ticket                                                                                                          |
| agent   | `fenced`         | 421              | yes           | reroute     | charged after the first | no                          | re-resolve after Retry-After; the database is moving or suspended                                                                                                          |
| agent   | `ticket_invalid` | 401              | no            | reroute     | charged after the first | no                          | get a new ticket, then retry                                                                                                                                               |
| agent   | `bad_request`    | 400              | no            | fail        | not charged             | no                          | fix the request; retrying it unchanged fails again                                                                                                                         |
| agent   | `internal`       | 500              | no            | fail        | not charged             | yes                         | report the log_id; a write may or may not have committed: the caller may retry with the same request_id                                                                    |
| agent   | `transport`      | none (no answer) | no            | reroute     | charged after the first | yes                         | no answer (connect failure, reset, unreadable body): the VM may be gone; a write may have committed, so it is retried with the same request id                             |
| resolve | `ok`             | 200              | no            | done        | not charged             | no                          | a route and a ticket                                                                                                                                                       |
| resolve | `moving`         | 503              | yes           | reroute     | charged after the first | no                          | the database is moving or being created: resolve again after Retry-After                                                                                                   |
| resolve | `shed`           | 503              | yes           | retry       | charged                 | no                          | controld refused the load (source rate limit, store reads or creates in flight); nothing ran                                                                               |
| resolve | `unavailable`    | 503              | yes           | retry       | charged                 | no                          | Postgres or an agent cannot do it now, or too many calls in flight                                                                                                         |
| resolve | `no_capacity`    | 503              | yes           | retry       | charged                 | no                          | no live vm under the placement caps right now                                                                                                                              |
| resolve | `timeout`        | 504              | no            | retry       | charged                 | no                          | the resolve deadline passed                                                                                                                                                |
| resolve | `transport`      | none (no answer) | no            | retry       | charged                 | no                          | no answer from controld                                                                                                                                                    |
| resolve | `bad_request`    | 400              | no            | fail        | not charged             | no                          | malformed request                                                                                                                                                          |
| resolve | `unauthorized`   | 401              | no            | fail        | not charged             | no                          | unknown, malformed or revoked credential                                                                                                                                   |
| resolve | `forbidden`      | 403              | no            | fail        | not charged             | no                          | the credential belongs to another tenant                                                                                                                                   |
| resolve | `not_found`      | 404              | no            | fail        | not charged             | no                          | no such database                                                                                                                                                           |
| resolve | `suspended`      | 409              | no            | fail        | not charged             | no                          | the database is suspended                                                                                                                                                  |
| resolve | `conflict`       | 409              | no            | fail        | not charged             | no                          | not allowed in the database's or tenant's state                                                                                                                            |
| resolve | `quota_exceeded` | 429              | no            | fail        | not charged             | no                          | create on first use past a cap: a quota, not an overload, so retrying cannot help                                                                                          |
| resolve | `internal`       | 500              | no            | fail        | not charged             | no                          | a bug; report the log_id                                                                                                                                                   |

Limits, the same in both clients by default:

| Limit                                         | Default |
| --------------------------------------------- | ------- |
| Attempts of one request on one route          | 3       |
| Reroutes of one call                          | 5       |
| Backoff: full jitter under base × 2^(n-1)     | 50 ms   |
| Backoff cap                                   | 5 s     |
| Longest `Retry-After` honored                 | 60 s    |
| Retry budget: tokens earned per first attempt | 0.1     |
| Retry budget: most tokens banked              | 10      |

No wait ends past the call's deadline: a retry that would is not made. The budget is shared by every call of
one client, so a client facing an outage adds at most a tenth more load in retries once its banked tokens are
spent. `expired` is not retried by the library because the deadline it was given is already spent;
resubmitting it would add load at the moment the server is slowest.
