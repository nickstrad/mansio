# The front's wire contract

The agent's `POST /v1/query` protocol: request, response, the eleven codes with HTTP statuses and Retry-After,
how dependency failures become `unavailable`, transport limits, health and shutdown.

Code: `internal/front/codes.go` (`Spec`, `Classify`, `Unavailable`, `ErrUnavailable`), `request.go`, `http.go`
(sampled WARN), `internal/agentglue/unavailable.go` (the mapping list). Tests: `TestSpecTable`,
`TestPipelineWireCodes`, `TestHTTPStatusesAndHeaders`, `TestHTTPUnavailableLogsSampledWarn`,
`internal/agentglue/unavailable_test.go`. Write retries and duplicates: [request ids](request-ids.md). The
document protocol `POST /v1/ops` runs through the same pipeline:
[docexec and the ops route](../intermediate-representation/docexec-and-ops-route.md).

**This is an INTERNAL, TRUSTED-CALLER protocol.** Requests carry parameterized SQL. In production clients
never send SQL: the trusted front compiles document requests, and only the compiled form reaches an
`Executor`. The `op` field is reserved for that compiled operation; today it is refused with `bad_request`.
When it lands it replaces `sql`/`args`, and framing, tickets, admission and the codes below stay as they are.

## Request

`POST /v1/query`, JSON body, ticket in `Authorization: Bearer <ticket>`, optional deadline in
`X-Timeout-Ms: <ms>` (relative, 1..3,600,000; clamped to `max_timeout`; default `default_timeout`).

```json
{"db": "db-7", "request_id": "r-123", "kind": "read" | "write", "sql": "SELECT * FROM t WHERE id = ?", "args": [42]}
```

- `db`: 1..128 bytes of `[A-Za-z0-9._:-]`, not starting with `.`; must equal the ticket's `dbid`.
- `request_id`: required for writes, optional for reads; 1..64 bytes, same charset. Echoed back.
- `kind`: `read` needs ticket perm `read`, `write` needs `write` (a write ticket does not imply read).
- `sql`: one statement (`;` only at the end), ≤ `max_sql`. Reads must start with SELECT / WITH / VALUES and
  not look like a write (`actor.IsWrite`). Writes may also start with INSERT / UPDATE / DELETE / REPLACE /
  CREATE / DROP / ALTER. Never allowed: BEGIN/COMMIT/ROLLBACK/SAVEPOINT/RELEASE (the actor owns the
  transaction), ATTACH/DETACH (other files), PRAGMA, VACUUM. This guard is defense in depth, not a security
  boundary (see the risks in the [README](README.md)).
- `args` (≤ `max_args`): `null`, `true`/`false` (→ 1/0), integer literals (must fit int64), other numbers
  (float64; finite), strings, and `{"b64": "<base64>"}` for blobs. Arrays and other objects are refused.
- Unknown fields, trailing data, and bodies over `max_body` (413) are refused.

Validation order: body size → JSON and fields → ticket present → ticket verify (needs `db` from the body) →
`Admitter.Admit(db, epoch)` → per-tenant/per-database admission → `Executor`.

## Response

Always JSON, every code:

```json
{
  "code": "ok",
  "request_id": "r-123",
  "columns": ["id", "a"],
  "rows": [[42, "x"]],
  "count": 1,
  "timing": { "admit_us": 0, "exec_us": 41 }
}
```

`count` is rows returned (read) or affected (write). Blobs come back as `{"b64": ...}`. On failure: `message`
(fixed text per code; `bad_request` explains which input is wrong; driver and internal text never appear),
`retry_after_ms` when there is a `Retry-After`, `ambiguous: true` on an `expired` write that may have
committed, and `log_id` on `internal` (the agent's JSON log has the chain and the stack under that ID).

Integers above 2^53 lose precision in JavaScript clients (same rule as mansio's number boundaries).

## Codes (the `Spec` table)

Eleven codes, in this order:
`ok, shed, unavailable, expired, stmt_timeout, aborted, wrong_owner, fenced,
ticket_invalid, bad_request, internal`.
Clients that switch on the code must handle all of them; `cmd/bench/frontbench` counts codes by name.

| code             | HTTP                                                              | Retry-After                                                                      | from                                                                                                                                                                             | client should                                                                                                                          |
| ---------------- | ----------------------------------------------------------------- | -------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `ok`             | 200                                                               | —                                                                                |                                                                                                                                                                                  | done                                                                                                                                   |
| `shed`           | 503                                                               | 1, or the VM queue's drain estimate when admission sheds ([metrics](metrics.md)) | admission queue full, actor mailbox full, SQLITE_BUSY/LOCKED, agent draining, actor closed                                                                                       | nothing ran; retry after Retry-After with backoff + jitter                                                                             |
| `unavailable`    | 503                                                               | ≥ 1, computed (see below)                                                        | anything wrapping `front.ErrUnavailable`: Admitter or Executor dependency failure (owner not reconciled, authority or manifest unreadable, S3 breaker open, fetch bulkhead full) | nothing ran; retry after `Retry-After` with backoff and jitter                                                                         |
| `expired`        | 504                                                               | —                                                                                | deadline passed in the admission queue or the actor's queue; or no reply by deadline + grace                                                                                     | not ambiguous: never ran, retry with a fresh deadline; `ambiguous: true`: a write may have committed, retry with the same `request_id` |
| `stmt_timeout`   | 422                                                               | —                                                                                | statement timeout interrupted SQLite                                                                                                                                             | nothing changed; don't retry unchanged                                                                                                 |
| `aborted`        | 409                                                               | —                                                                                | a batch-mate write timed out, the batch rolled back                                                                                                                              | nothing changed; retry at once                                                                                                         |
| `wrong_owner`    | 421                                                               | —                                                                                | Admitter: epoch mismatch / not owned; Host: db not here                                                                                                                          | re-resolve (locator), retry with the new ticket                                                                                        |
| `fenced`         | 421                                                               | 1                                                                                | Admitter: database fenced here                                                                                                                                                   | re-resolve after Retry-After                                                                                                           |
| `ticket_invalid` | 401 (+ `WWW-Authenticate: Bearer`)                                | —                                                                                | any `ticket.Err*` (missing, malformed, version, kid, signature, claims, expired, not yet valid, TTL, audience, db, permission)                                                   | get a new ticket                                                                                                                       |
| `bad_request`    | 400 (413 body too large, 404 path, 405 method, 408 body too slow) | —                                                                                | validation; SQLite rejected the statement (syntax, constraint, type, read-only); too many rows                                                                                   | fix the request                                                                                                                        |
| `internal`       | 500                                                               | —                                                                                | anything unexpected, including recovered panics                                                                                                                                  | report `log_id`; a write may or may not have committed                                                                                 |

The ticket's distinct sentinels are kept for logs (debug level) and tests; the wire says only
`ticket_invalid`.

## `unavailable`: a dependency is down

Without this code, any error the front did not recognize was `internal` (500, "a write may or may not have
committed", one ERROR line with a log ID per request). With S3 and Postgres behind the front, a dependency
outage would page on-call with thousands of "bugs", and clients would not back off (500 has no `Retry-After`).
A dependency being unavailable is neither a bug nor a bad request.

A new code rather than reusing `shed`: `shed` means "this agent is at its caps" (admission, mailbox, busy),
`unavailable` means "a dependency is down or recovering". Clients retry both the same way, but operators and
dashboards must tell overload from outage, and the retry hint differs (queue drain vs breaker cool-down).

Body: same as every other code — `code`, `request_id`, `message` ("a dependency is unavailable; nothing ran"),
`retry_after_ms`. No `log_id` (it is not a bug).

**Logging.** `unavailable` answers log at WARN, at most `front.UnavailableLogPerSecond` (1) line per second
per handler, with `event: FRONT.UNAVAILABLE`, the error chain text, `request_id`, `tenant`, and `suppressed` =
the number of unavailable answers not logged since the previous line. Never ERROR.

**Which errors map to it.** The front only knows `front.ErrUnavailable`; it imports no other component's
package. The agent glue (`internal/agentglue`) wraps the Admitter and the Executor (`depAdmitter`,
`depExecutor`) and maps every error that `errors.Is` one of `unavailableCauses`: `owner.ErrUnavailable` (not
reconciled since restart; authority or manifest unreadable), `vactor.ErrBusy` (fetch queue full, or an
activation hit its own deadline), `durable.ErrUnavailable` (S3 breaker open or a store system failure; its
time-to-half-open travels as the retry hint), `vactor.ErrInDoubt` (too many publishes of the key in doubt) and
`pg.ErrUnavailable` (Postgres placement authority unreachable). Adding a cause is one line in
`unavailableCauses`. A bare `context.DeadlineExceeded` is deliberately not mapped, because it also means the
caller's own deadline passed (`expired`). An owner adapter must map `ErrUnavailable` to `front.ErrUnavailable`
(or go through `depAdmitter`), not to `front.ErrShed`.

## Computed Retry-After

Errors may carry a hint (`front.WithRetryAfter(err, d)`, or any error implementing `front.RetryHint`). For
codes with a Retry-After (`shed`, `unavailable`, `fenced`) the answer sends max(Spec floor, hint rounded up to
seconds), capped at `front.MaxRetryAfter` (60 s). Admission's `shed` hint is the queue-drain estimate in
[metrics](metrics.md).

Client side (`internal/retry`): the zero `Policy` is safe (3 attempts, exponential 50 ms → 5 s, full jitter);
`Attempt.ServerHint` makes `Decide` wait max(hint, backoff); `Budget` is a token bucket (Ratio per first
attempt, at most `Burst` = 10 banked). The [Go client](go-client.md) uses it and retries `unavailable`.

Measured 2026-11-05 (Apple M5 Max, 18 cores, macOS; `configs/retry-storm.json` trimmed to its three `good`
phases — calm 5k qps 10 s, overload 60k qps 15 s, recover 5k qps 10 s — 1,000 dbs, zipf 1.2): 999,999 first
attempts, 59,899 retries (0.060 overall). Per 1 s window (by `sched_ms`) retries/first ranged 0.042–0.091, so
every window stayed ≤ the 0.1 ratio.

## Transport limits

- `bad_request` can come with HTTP **408**: the body did not arrive within `body_read_timeout` (agentd,
  default 5 s). The write deadline is the request deadline + `reply_grace` + 1 s.
- Admission has VM-wide caps `vm_in_flight` (default 4 × GOMAXPROCS × `db_in_flight`) and `vm_queue` (default
  4 × `vm_in_flight`); a request over them is `shed`, like the tenant and database caps. agentd refuses a
  config whose caps do not nest (db ≤ tenant ≤ VM).
- The health listener has read, write and idle timeouts.

## Health

`GET /healthz` → 200 `{"status":"ok","in_flight":N}`, or 503 `{"status":"draining"}` once shutdown started. It
touches neither admission nor actors; `health_listen` puts it on a second listener as well.

Liveness is not readiness. Both live on the lifecycle listener (`health_listen`; without one they share the
query listener):

- `GET /livez` → always 200 `{"status":"live","draining":…}` while the process answers — also while draining,
  so an orchestrator does not restart an agent mid-drain.
- `GET /readyz` → 200 `{"status":"ready"}`, or 503 `{"status":"draining"}` /
  `{"status":"not_ready","failed":{check: why}}`. It answers from the last probe (every `ready_interval`, each
  bounded by `ready_timeout`; a result older than 3 intervals is not trusted). Checks: `startup` (every
  database tried once), `canary` (a marked read on the `~canary` database through admission → executor →
  actor; `~` is not a valid client db id, so it never touches tenant data; an admission shed passes — overload
  is not a reason to leave rotation — but a shed from its own actor fails), and in durable mode one
  `<dep>_breaker` per remote dependency. Transitions log `AGENT.READY` / `AGENT.NOT_READY`.

Startup listens first, then opens databases in the background (`startup_workers` at a time, failures retried
with backoff). A database not open yet answers `unavailable` (503 + Retry-After), not `wrong_owner`.

## Shutdown (agentd)

SIGINT/SIGTERM starts one `shutdown_budget` (default 60 s), spent in phases, in order: `front.drain` —
`Server.Drain` (new requests `shed`, healthz 503) and `http.Server.Shutdown`, at most `drain_timeout` (≤ the
budget) → `host.close` with what is left — close every actor (queued work is answered `shed`); in durable mode
a vactor host's `Shutdown(ctx)` runs here: dirty databases written back oldest-unpublished first, the rest
closed with their local copies kept and counted → `lifecycle.close`. A phase that runs out does not skip the
next. Each phase logs `AGENT.PHASE_DONE`/`AGENT.PHASE_FAILED`, then `AGENT.SHUTDOWN_DONE`. Exit 0 when every
phase finished; exit 1 with a log ID otherwise (requests cut — writes among them ambiguous — or databases not
closed in time).
