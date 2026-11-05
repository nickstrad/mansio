# 2026-11-05 — wire code `unavailable` (amends contract 3)

Author: books/p2-front agent (Release It plan item RI-04). Amends
[the wire contract](2026-11-05-wire-contract.md) (owned by the front agent; not edited) and contract 3 in
`shared_state.md` (not edited). Code: `internal/front/codes.go` (`Unavailable`, `ErrUnavailable`, `Spec`,
`Classify`), `internal/front/http.go` (sampled WARN), `cmd/agentd/unavailable.go` (the mapping list). Tests:
`TestSpecTable`, `TestPipelineWireCodes`, `TestHTTPStatusesAndHeaders`, `TestHTTPUnavailableLogsSampledWarn`,
`cmd/agentd` `TestDependencyErrorsMapToUnavailable`.

## Why a new code

Before this, any error the front did not recognize was `internal` (500, "a write may or may not have
committed", one ERROR line with a log ID per request). Once CP step 1 wires vactor, durable and owner behind
the front, an S3 or Postgres outage would page on-call with thousands of "bugs", and clients would not back
off (500 has no `Retry-After`). Release It F2: dependency unavailable ≠ bug ≠ bad request.

We chose a new code over reusing `shed`: `shed` means "this agent is at its caps" (admission, mailbox, busy),
`unavailable` means "a dependency is down or recovering". Clients retry both the same way, but operators and
dashboards must tell overload from outage, and the retry hint differs (queue drain vs breaker cool-down).

## The row

| code | HTTP | Retry-After | from | client should |
|---|---|---|---|---|
| `unavailable` | 503 | ≥ 1 (computed, RI-10) | anything wrapping `front.ErrUnavailable`: Admitter or Executor dependency failure (owner not reconciled, authority or manifest unreadable; later S3 breaker open, fetch bulkhead full) | nothing ran; retry after `Retry-After` with backoff and jitter |

The set is now eleven codes, in this order: `ok, shed, unavailable, expired, stmt_timeout, aborted,
wrong_owner, fenced, ticket_invalid, bad_request, internal`. Clients that switch on the code must add it;
`cmd/frontbench` counts codes by name and needs no change.

Body: same as every other code — `code`, `request_id`, `message` ("a dependency is unavailable; nothing ran"),
`retry_after_ms`. No `log_id` (it is not a bug).

## Logging

`unavailable` answers log at WARN, at most `front.UnavailableLogPerSecond` (1) line per second per handler,
with `event: FRONT.UNAVAILABLE`, the error chain text, `request_id`, `tenant`, and `suppressed` = the number of
unavailable answers not logged since the previous line. Never ERROR.

## Which errors map to it (cmd/agentd)

The front only knows `front.ErrUnavailable`; it imports no other agent's package. `cmd/agentd` wraps the
Admitter and the Executor (`depAdmitter`, `depExecutor`) and maps every error that `errors.Is` one of
`unavailableCauses`:

- today: `owner.ErrUnavailable` (not reconciled since restart; authority or manifest unreadable).
- RI-02 adds `vactor.ErrBusy` (fetch queue full): one line in `unavailableCauses`.
- RI-03 adds `durable.ErrUnavailable` (S3 breaker open): one line. Its time-to-half-open should travel as a
  retry hint (`front.RetryHint`, RI-10) so `Retry-After` says when to come back.
- Activation timeouts: vactor should wrap them in one of the above. A bare `context.DeadlineExceeded` is
  deliberately not mapped, because it also means the caller's own deadline passed (`expired`).

Note: knowledge/front/README.md "How the other agents plug in" says the owner adapter maps
`ErrUnavailable → front.ErrShed`. Use `front.ErrUnavailable` instead (or wrap the Admitter in `depAdmitter`).

## Related transport limits (RI-09, same branch)

- `bad_request` can now come with HTTP **408**: the body did not arrive within `body_read_timeout` (agentd,
  default 5 s). The write deadline is the request deadline + `reply_grace` + 1 s.
- Admission gained VM-wide caps `vm_in_flight` (default 4 × GOMAXPROCS × `db_in_flight`) and `vm_queue`
  (default 4 × `vm_in_flight`); a request over them is `shed`, like the tenant and database caps. agentd
  refuses a config whose caps do not nest (db ≤ tenant ≤ VM).
- The health listener has read, write and idle timeouts.

## Handshaking (RI-10, same branch)

- **Computed Retry-After.** Errors may carry a hint (`front.WithRetryAfter(err, d)`, or any error implementing
  `front.RetryHint`). For codes with a Retry-After (`shed`, `unavailable`, `fenced`) the answer sends
  max(Spec floor, hint rounded up to seconds), capped at `front.MaxRetryAfter` (60 s). Not done: a
  queue-drain estimate for `shed` (needs service-rate metrics, RI-11).
- **Client side** (`internal/retry`): the zero `Policy` is now safe (3 attempts, exponential 50 ms → 5 s, full
  jitter); `Attempt.ServerHint` makes `Decide` wait max(hint, backoff); `Budget` is a token bucket (Ratio per
  first attempt, at most `Burst` = 10 banked). `internal/outcome` has no `unavailable` code yet; the CP-6 client
  needs one (or maps `unavailable` to `shed`) before it can retry on it.
- **Measurement** (2026-11-05, Apple M5 Max, 18 cores, macOS; `configs/retry-storm.json` trimmed to its three
  `good` phases — calm 5k qps 10 s, overload 60k qps 15 s, recover 5k qps 10 s — 1,000 dbs, zipf 1.2): 999,999
  first attempts, 59,899 retries (0.060 overall). Per 1 s window (by `sched_ms`) retries/first ranged
  0.042–0.091, so every window stayed ≤ the 0.1 ratio.
