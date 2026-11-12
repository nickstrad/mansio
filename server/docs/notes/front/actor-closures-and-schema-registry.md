# Actor closure requests, the schema registry, and (tenant, app) identity

How the actor runs a Go closure inside a transaction, how app schemas are registered and fetched, and how a
(tenant, app) pair names a database. These are the pieces `POST /v1/ops` runs on (see
[docexec and the ops route](../ir/docexec-and-ops-route.md)).

## Actor closures (`internal/actor/fn.go`)

- `Request.Fn func(*Tx) (Output, error)` runs instead of `SQL`: a write in its own `SAVEPOINT w` in the batch
  (claim → Fn → record → `RELEASE`), a read in `BEGIN … ROLLBACK`. Everything the SQL path has (group commit,
  `Deadline`, `StmtTimeout` over the whole closure, shedding, request ids, a savepoint per write and stopping
  the batch when a write ends the transaction) applies unchanged.
- `Tx`: `Exec`, `Query(sql, maxRows, args…)` (bounded, copied values), `Now()` (one clock reading: the batch's
  time for a write, also the id row's `applied_ms`), `ReadOnly()`. A dead `Tx` (after Fn returned) answers
  `ErrTxDone`.
- Statement guard `closureSafe`: refuses a first word of
  BEGIN/COMMIT/END/ROLLBACK/SAVEPOINT/RELEASE/PRAGMA/ATTACH/ DETACH/VACUUM and anything after the first
  top-level `;` (skipping comments, quoted strings and identifiers). It is an error (`ErrTxControl`), not an
  assertion: cheap, and it keeps the savepoint intact in both builds.
- **A read closure on the write connection needs `PRAGMA query_only`.** Reads share the writer when
  `read_conns = 0`; `Tx.Exec` is refused in a read, but `Query("DELETE … RETURNING")` would still write and
  take the write lock. The writer is switched `query_only = 1` around the closure (read-pool connections
  already are); if switching back fails the writer is closed after the batch and reopened by the next.
- Panics in Fn are recovered into a `*panicx.Panic` error for that request only (its savepoint rolls back).
- Records: `0x01` + varint rows (SQL), `0x02` + `Output.Body` bytes (closure). The two kinds share the id
  space; an id applied by the other kind answers `ErrRecordKind` (nothing runs).
- **Model-fuzz finding:** an id claimed by a batch-mate of the other kind answers `ErrRecordKind` even if that
  batch-mate's batch is then aborted — same as a constraint error caused by a rolled-back batch-mate (an own
  error stays). Harmless (nothing of the request ran) but the model must allow it.

## Schema registry

- Postgres `0006_app_schemas.sql`: `schemas(fingerprint, body)` content-addressed, with a CHECK that the
  fingerprint is `'sha256:' || encode(sha256(convert_to(body, 'UTF8')), 'hex')` (the database enforces it);
  `app_schemas(app, fingerprint)`, app in the `database_name` domain.
- controld: `PUT /v1/apps/{app}/schema` (admin) `{"fingerprint","schema"}`, idempotent (`created`);
  `GET /v1/apps/{app}/schemas/{fingerprint}` (admin or control token).
- **Exact bytes:** the fingerprint is over the fingerprint text byte for byte. `json.RawMessage` on decode
  keeps the sent bytes, so `"schema": {…}` from a TS `JSON.stringify({fingerprint, schema})` hashes right. On
  encode, `encoding/json` _compacts and HTML-escapes_ a `RawMessage`, so the GET answer carries the text as a
  JSON string.
- agentd: `internal/schemareg.Client` (`schemas_url`): bounded LRU (entries + bytes, no TTL), single-flight
  detached fetches under a timeout, ≤ 16 distinct in flight, breaker "schemas", re-hash on every fetch,
  `Validate` hook for docschema. Fake: `schemaregtest`.

## (tenant, app) identity

- Decision: **deterministic key `tenant ":" app`**, no mapping table: the [controld](controld.md) registry
  already names databases `tenant:name` with `name` in `database_name`, `durable.KeyPrefix` and
  `front.CheckDBID` accept it, and ADR 0011 wants no extra mapping. Cost: an app name is the lowercase
  `database_name` subset of the spec's wire ids.
- `POST /v1/resolve {"tenant","app"[,"create"]}`; `create` makes the locator answer `ErrCreatable` (also
  `ErrNotFound`) for a never-registered database when the key may write; controld then creates it (app must
  have a registered schema) and resolves again from the primary. Explicit create:
  `POST /v1/apps/{app}/databases`.

## Traps and fixes

- **A swallowed transaction-ending error must poison the closure.** After `INSERT OR ROLLBACK` /
  `RAISE(ROLLBACK)` / an I/O error ends the batch transaction, the next statement on the connection
  _autocommits_: a closure that ignored the error and wrote again committed that row while both requests
  answered `ErrAborted`. Now a write closure's first statement error poisons its `Tx` (`Tx.Err()`): later
  statements answer it and the request fails with it even if Fn returns nil. Fuzz mode 7 covers it.
- `Request.Check func(*Tx) error` runs first (read-only Tx), before the id claim, in the savepoint or read
  transaction: the spec's fingerprint check precedes the duplicate answer.
- Guard also refuses `CREATE` with an unquoted `TEMP`/`TEMPORARY` word (a temp trigger would outlive the
  closure).
- The agents' schema GET uses the locator's bulkhead and deadline, not the admin API's.
