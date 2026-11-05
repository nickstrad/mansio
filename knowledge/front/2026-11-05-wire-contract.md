# 2026-11-05 — the front's wire contract (contract 3)

Author: front agent. Code: `internal/front/codes.go` (`Spec`, `Classify`), `request.go`, `http.go`; tests:
`TestSpecTable`, `TestPipelineWireCodes`, `TestHTTPStatusesAndHeaders`.

**This is an INTERNAL, TRUSTED-CALLER protocol.** Requests carry parameterized SQL. In production clients never
send SQL: the trusted front compiles document requests, and only the compiled form reaches an `Executor`. The
`op` field is reserved for that compiled operation; today it is refused with `bad_request`. When it lands it
replaces `sql`/`args`, and framing, tickets, admission and the codes below stay as they are.

## Request

`POST /v1/query`, JSON body, ticket in `Authorization: Bearer <ticket>`, optional deadline in
`X-Timeout-Ms: <ms>` (relative, 1..3,600,000; clamped to `max_timeout`; default `default_timeout`).

```json
{"db": "db-7", "request_id": "r-123", "kind": "read" | "write", "sql": "SELECT * FROM t WHERE id = ?", "args": [42]}
```

- `db`: 1..128 bytes of `[A-Za-z0-9._:-]`, not starting with `.`; must equal the ticket's `dbid`.
- `request_id`: required for writes, optional for reads; 1..64 bytes, same charset. Echoed back.
- `kind`: `read` needs ticket perm `read`, `write` needs `write` (a write ticket does not imply read).
- `sql`: one statement (`;` only at the end), ≤ `max_sql`. Reads must start with SELECT / WITH / VALUES
  and not look like a write (`actor.IsWrite`). Writes may also start with INSERT / UPDATE / DELETE / REPLACE /
  CREATE / DROP / ALTER. Never allowed: BEGIN/COMMIT/ROLLBACK/SAVEPOINT/RELEASE (the actor owns the
  transaction), ATTACH/DETACH (other files), PRAGMA, VACUUM. This guard is defense in depth, not a security
  boundary (see "Risks").
- `args` (≤ `max_args`): `null`, `true`/`false` (→ 1/0), integer literals (must fit int64), other numbers
  (float64; finite), strings, and `{"b64": "<base64>"}` for blobs. Arrays and other objects are refused.
- Unknown fields, trailing data, and bodies over `max_body` (413) are refused.

Validation order: body size → JSON and fields → ticket present → ticket verify (needs `db` from the body)
→ `Admitter.Admit(db, epoch)` → per-tenant/per-database admission → `Executor`.

## Response

Always JSON, every code:

```json
{"code": "ok", "request_id": "r-123", "columns": ["id", "a"], "rows": [[42, "x"]], "count": 1,
 "timing": {"admit_us": 0, "exec_us": 41}}
```

`count` is rows returned (read) or affected (write). Blobs come back as `{"b64": ...}`. On failure: `message`
(fixed text per code; `bad_request` explains which input is wrong; driver and internal text never appear),
`retry_after_ms` when there is a `Retry-After`, `ambiguous: true` on an `expired` write that may have committed,
and `log_id` on `internal` (the agent's JSON log has the chain and the stack under that ID).

Integers above 2^53 lose precision in JavaScript clients (same rule as ts-db's number boundaries).

## Codes (the `Spec` table)

| code | HTTP | Retry-After | from | client should |
|---|---|---|---|---|
| `ok` | 200 | — | | done |
| `shed` | 503 | 1 | admission queue full, actor mailbox full, SQLITE_BUSY/LOCKED, agent draining, actor closed | nothing ran; retry after Retry-After with backoff + jitter |
| `expired` | 504 | — | deadline passed in the admission queue or the actor's queue; or no reply by deadline + grace | not ambiguous: never ran, retry with a fresh deadline; `ambiguous: true`: a write may have committed, retry with the same `request_id` |
| `stmt_timeout` | 422 | — | statement timeout interrupted SQLite | nothing changed; don't retry unchanged |
| `aborted` | 409 | — | a batch-mate write timed out, the batch rolled back | nothing changed; retry at once |
| `wrong_owner` | 421 | — | Admitter: epoch mismatch / not owned; Host: db not here | re-resolve (locator), retry with the new ticket |
| `fenced` | 421 | 1 | Admitter: database fenced here | re-resolve after Retry-After |
| `ticket_invalid` | 401 (+ `WWW-Authenticate: Bearer`) | — | any `ticket.Err*` (missing, malformed, version, kid, signature, claims, expired, not yet valid, TTL, audience, db, permission) | get a new ticket |
| `bad_request` | 400 (413 body too large, 404 path, 405 method) | — | validation; SQLite rejected the statement (syntax, constraint, type, read-only); too many rows | fix the request |
| `internal` | 500 | — | anything unexpected, including recovered panics | report `log_id`; a write may or may not have committed |

The ticket's distinct sentinels are kept for logs (debug level) and tests; the wire says only
`ticket_invalid`.

## Health

`GET /healthz` → 200 `{"status":"ok","in_flight":N}`, or 503 `{"status":"draining"}` once shutdown started.
It touches neither admission nor actors; `health_listen` puts it on a second listener as well.

## Shutdown (agentd)

SIGINT/SIGTERM → `Server.Drain` (new requests `shed`, healthz 503) → `http.Server.Shutdown` bounded by
`drain_timeout` → close listeners → close every actor (queued work is answered `shed`). Exit 0 when the drain
finished; exit 1 with a log ID when it did not (requests cut, writes among them ambiguous).
