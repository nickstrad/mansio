# IR protocol: the SDK sends document operations; the Go backend runs them on the right actor

Status: plan · Date: 2026-11-05 · Baselines: mansio `main` at `addda3d`, mansio-server `main` at `1eac7d4` ·
Replaces the engine track of [replace-platform-with-go.md](replace-platform-with-go.md) (its D2/D3 and phases
0 and 2). Its other phases (one repo, tenancy and auth, durability, observability, cut-over) still apply, with
IR in place of handlers. Changes no code.

## 1. The idea

The SDK already describes every database access as a small intermediate representation (IR): `SelectPlan`,
`insert`, `patch` and `delete` (`sdk/db.ts:22-36`, `core/plan.ts`). Today the Deno server runs the handler
that produces it. Instead:

- **The SDK sends IR over the wire.** A query is one select. A mutation is an ordered list of operations,
  applied atomically, with **preconditions** that replace server-side read-decide-write.
- **The Go backend owns no TypeScript.** agentd validates the IR against the app's registered schema, compiles
  it to SQL (a Go port of `core/compile`, ~550 lines), and runs it on the database's actor, inside one
  savepoint in the group-commit batch. Routing, tickets, fencing, durability and moves are mansio-server as
  built.
- **Handlers run where the SDK runs.** `defineFunctions` handlers execute in the caller's process (an app
  backend). Their reads become preconditions automatically. A conflict comes back as `ConflictError` and the
  SDK retries the handler. This is optimistic concurrency, the model local-first sync needs later.

Scope now: trusted callers (app backends with a tenant API key or session). Untrusted end users calling
server-enforced rules is a later, optional feature (§8).

**Local-first later.** The IR is designed as a log entry: operations are deterministic, carry ids and
versions, are idempotent by request id, and replay in order. A device holding a local SQLite copy could apply
the same ops locally and sync them later (§7). We don't build sync now; we just don't design it out.

## 2. The protocol (v1)

### 2.1 Envelope

`POST /v1/ops`. Headers: `Authorization: Bearer <ticket | api key | session>`, `X-Trace-Id`, `X-Timeout-Ms`,
`Content-Type: application/json`. Body:

```json
{
  "v": 1,
  "db": { "tenant": "acme", "app": "tic-tac-toe" },
  "kind": "query | mutation",
  "requestId": "01JD…", // required for mutations; idempotency key (mansio-server CP-2)
  "schema": "sha256:…", // fingerprint the client compiled against
  "ops": [/* §2.2 */]
}
```

The response keeps mansio's wire: `200 {"result": [<one result per op>]}` or
`{"error":{"tag","message","op"?:<index>,"fields"?:{...}}}` with the statuses of `core/errors.ts:554-620`,
plus mansio-server's `unavailable` (503 + Retry-After), `shed`, `wrong_owner` / `fenced` (re-resolve) and
`duplicate: true` on an already-applied mutation.

### 2.2 Operations

| op       | Fields                                        | Result                              | Notes                                                                                                                    |
| -------- | --------------------------------------------- | ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `select` | `table, filters[], order?, limit, aggregate?` | rows, `count`, or `{value,count}[]` | Today's `SelectPlan` unchanged; `limit` 1..1000 (`core/plan.ts`)                                                         |
| `get`    | `table, id`                                   | doc or `null`                       | Sugar for `select _id eq, limit 1`; needed for clean preconditions                                                       |
| `insert` | `table, value, id?`                           | `_id`                               | `id` is client-generated (UUIDv7), optional now and required for offline use later; server checks it's a UUID and unused |
| `patch`  | `table, id, value, expect?`                   | `{ _version }`                      | Partial merge validated with `partial = true`; missing row → `NotFoundError`                                             |
| `delete` | `table, id, expect?`                          | `{ deleted: bool }`                 | Missing row is not an error unless `expect` says it must exist                                                           |
| `assert` | `table, id, expect`                           | —                                   | Precondition only; no write                                                                                              |

`expect` (preconditions; all must hold or the whole mutation fails with
`ConflictError{op, expected, actual}`): `{ "version": n }` (`_version == n`), `{ "exists": true|false }`,
`{ "fields": {f: v} }` (scalar equality). A range precondition (`assert` on a select's result count or ids) is
reserved for v1.1. It's needed when a decision depends on "no row matches", which is the phantom problem.

References: a later op may use `{"$ref": i}` for the `_id` produced by insert number `i` in the same request
(e.g. insert a game, then insert its first move). Only ids can be referenced, and only backwards.

Rules: ops run in order inside one savepoint; any failure rolls the request back; a mutation may contain
selects (they see earlier ops' writes, as today); a query may contain only `select`/`get`. Max 64 ops and the
existing 64 KiB body cap.

### 2.3 Server-owned fields and determinism

The server sets `_creationTime` (the actor's clock, at commit batch time) and `_version` (`+1` in SQL, as
today). `_id` comes from the client if given, otherwise from the server. Given the database state and the ops,
the result is deterministic: no randomness or wall-clock time apart from `_creationTime`, which the server
records once per request, so a replay reuses it.

### 2.4 Schema registration

Apps register their schema once: `PUT /v1/apps/:app/schema` on controld with the `defineTable` JSON and its
fingerprint (the same `JSON.stringify(schema)` as `_platform_schema`, so existing files open). agentd caches
it by fingerprint. A request whose `schema` fingerprint differs from the database's gets `SchemaDriftError`
(409), as today. Migrations stay out of scope, as today.

## 3. SDK changes (TypeScript)

- `Execute` becomes a **recording executor**. Reads inside a mutation handler go out immediately as a query
  request (or are served from a per-handler cache). Writes are buffered. When the handler finishes, the SDK
  sends one mutation: buffered writes, plus `assert`/`expect` preconditions derived from what the handler
  read. `get(id)` → `expect.version` of that doc, or `exists:false` for a null read. A `find` read stays
  unguarded in v1 and is documented as such. In v1.1 it can opt in to a range assert.
- On `ConflictError` from a derived precondition, the SDK re-runs the handler (bounded, with jitter, using the
  same `requestId` only when the op list is identical; otherwise a new id). App-level `ConflictError`s, like
  `makeMove`'s explicit `expectedVersion`, still go to the caller.
- `createClient` gains one argument: the functions object (and its schema) as a value, not just a type
  parameter, because the client now runs the handlers. Today `createClient<F>(baseURL, appId, token)` only
  needs `F` as a type. Call sites keep their shape: `client.mutation("makeMove", args)` now runs the handler
  locally against the remote database. Handlers can re-run on conflict, so they must have no side effects
  beyond `db` (no emails or external calls inside a handler). A thin compatibility route on agentd
  (`/apps/:app/...`) is not needed: the SDK is the only client.
- Resolve + ticket + location cache (port of mansio-server `internal/client`): refresh ahead of TTL,
  re-resolve on `wrong_owner`/`fenced` (with `min_epoch`) and `ticket_invalid`, honor `Retry-After`, windowed
  retry budget.

## 4. Server changes (Go, in mansio-server, later `server/` in mansio)

| Piece                     | Package                     | Work                                                                                                                                                          |
| ------------------------- | --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| IR types + decode         | `internal/ir` (new)         | Strict JSON decode (unknown fields rejected, as today), canonical encoding, limits; pure                                                                      |
| Schema model + validation | `internal/docschema` (new)  | Port `core/schema.ts`: node kinds, nullable, enum, fixed-length arrays, integer safety, U+0000 refusal (ADR 0005), reserved names; fingerprint byte-identical |
| Compiler + decode         | `internal/doccompile` (new) | Port `core/compile/{select,write,ddl,decode,dialect}` for SQLite only; `GENERATED_SQL.txt` identical                                                          |
| Executor                  | `internal/docexec` (new)    | Runs a decoded request on an actor connection: savepoint, preconditions, refs, server fields, decode results; request id via the existing `_mansio_requests`  |
| Front route               | `internal/front`            | `/v1/ops` next to `/v1/query`. Raw SQL stays for benches and admin only, behind a perm. Error tags from §2.1                                                  |
| Schema registry           | controld + agentd           | Store, fetch by fingerprint, cache                                                                                                                            |
| Database identity         | controld                    | `(tenant, app)` → database key; created by controld on first use or by an explicit create                                                                     |

The actor needs one addition: a request may carry a Go closure that runs on the connection inside its
savepoint (`docexec` instead of a single SQL string). The closure stays inside the actor's batch, so group
commit, deadlines and shedding apply unchanged.

## 5. Build order

Every step lands with its tests from §6. "Gate" means the tests named there pass in CI.

| Step                   | Work                                                                                                                                                                                     | Gate                                                                                                                                     |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| **IR-0 Spec**          | This doc → ADR; JSON Schema for v1; a shared **test-vector corpus** `ir/vectors/*.json` (request, schema, initial rows → expected result, error or SQL), consumed by both TS and Go      | Vectors run in TS against today's Deno engine and match (via the reference executor `platform/ir/executor.ts`; `Storage` parity is IR-3) |
| **IR-1 Go core**       | `ir`, `docschema`, `doccompile` (pure)                                                                                                                                                   | Vectors (SQL and decode) green in Go; `GENERATED_SQL.txt` byte-identical; differential fuzz vs TS (§6.2) clean for 1M plans              |
| **IR-2 Execution**     | `docexec` + actor closure + `/v1/ops` + schema registry + `(tenant, app)` identity. Built (IR-2a, IR-2b): execute 122/122 on a real actor, `FuzzDocExecModel` clean                      | Vectors (results and errors) green against a real actor; model-based fuzz (§6.3) clean                                                   |
| **IR-3 SDK**           | Recording executor, derived preconditions, handler retry, ops transport, resolve/ticket client. Built: e2e documents/queries green on Go and Deno (both layouts), SDK vectors            | e2e `documents`, `queries` green with the SDK → Go (both storage layouts); same scenarios still green → Deno via a TS shim               |
| **IR-4 Port the rest** | Tenancy/auth, durability, observability phases of [replace-platform-with-go.md](replace-platform-with-go.md) §5 (3–5), over IR                                                           | e2e `auth`, `tenant_isolation`, `admin_lifecycle`, `backup_restore`, `log_analysis` green on Go                                          |
| **IR-5 Multi-node**    | Edge + locator path for `/v1/ops`; controld create/move for `(tenant, app)`                                                                                                              | Cluster DST (§6.4) and the new e2e `scale_out` green                                                                                     |
| **IR-6 Cut over**      | Delete the Deno platform (replace plan phase 7). Built (2026-11-06, `ir/06-cutover`, ADR 0013): `platform/` and its tests deleted; event-log CLI kept in `eventlog/`; e2e 8/8 on Go only | e2e green on Go only                                                                                                                     |

IR-5 carry-over from IR-4a (ADR 0012): controld's locator must accept legacy keys and sessions through
`internal/tenancy`, and `create_api_key.sql` must refuse a deactivated tenant (today it inserts for any
tenant; `api_key.sql` already reads a deactivated tenant's keys as revoked). TODO(IR-5).

IR-0 to IR-2 are server-only and can start now. IR-3 is where users start to see a difference.

## 6. Hardening: tests in layers, added step by step

Builds on mansio-server's testing approach (`knowledge/testing/README.md`: spec tables, fakes with fault
switches, contract suites, model fuzz) and mansio's property suites (`tests/property/`).

### 6.1 Shared test vectors (IR-0)

One corpus, two runners (Deno test, Go test). Each vector: schema, seed rows, request, then the expected
result, error tag and fields, or SQL text and args. Every bug fix adds a vector. This is the cross-language
contract, and the regression suite for local-first later.

### 6.2 Property and differential tests (IR-1)

- **Differential compiler:** fast-check generates schemas and plans in TS (reusing `tests/property`
  arbitraries) and writes a corpus. The Go fuzzer mutates it. Each language compiles every plan; the SQL, the
  args and the `CompileError` reason must match. A nightly job runs both on the same SQLite and compares rows.
- **Properties in Go** (`testing/quick` or `rapid`): decode is the inverse of encode for valid documents;
  validation accepts exactly what the TS validator accepts (on/off points for integer safety, NUL, enum, array
  length); a compiled select's results equal a reference evaluator over the seed rows (ordering with the `_id`
  tie-break, NULLS FIRST/LAST, `countBy` grouping).
- **Go native fuzz targets:** `FuzzDecodeRequest` (never panics; rejects unknown fields; enforces limits),
  `FuzzCompile` (never panics; values only as bind parameters; output always parses in SQLite),
  `FuzzCanonicalJSON` (round trip).

### 6.3 Model-based tests of execution (IR-2)

- `FuzzDocExecModel`: bytes become a script of requests (selects, inserts with and without client ids, patches
  with and without `expect`, deletes, `$ref`s, duplicate request ids, injected statement failures). The script
  runs against a real actor and against a pure Go model (a map of tables to documents). After every request,
  compare results, error tags, `_version`s and the full table contents.
- Invariants: a failed mutation leaves no trace (rows, versions, request-id row); `_version` only goes up by
  one per patch; a duplicate request id returns the first result and changes nothing; preconditions are
  checked before any write in the request is visible.
- Extend mansio-server's `FuzzActorModel`, `FuzzDurableHostModel` and `FuzzOwnershipModel` with IR requests,
  so passivation, write-back and fencing run under document workloads.

### 6.4 Deterministic simulation testing (DST): added in four steps

Goal: run the whole system (SDK client, edge, agentd and actors, controld, placement, S3) in one process from
a seed, with simulated time, network and faults. Any failure then replays exactly from its seed.

1. **DST-0, determinism hygiene (with IR-1).** Inject `Clock`, `Rand` and `IDGen` everywhere state is decided.
   Many seams exist already: breaker clocks, `retry` jitter, owner, placement. A lint check bans `time.Now`,
   `math/rand` globals and `uuid.New` in domain packages. Domain logic stays in pure step functions (most of
   mansio-server already is: `plan`, owner `Table`, `breaker`, `PlanGC`, `Policy.Pick`, locator).
2. **DST-1, single-node sim (with IR-2).** A seeded event-loop harness drives one agent's request path:
   requests arrive, clocks jump, actors step, write-back ticks fire, S3 calls go through `durabletest` and
   `s3test` faults (lost replies, 503 storms, stalls), and "crashes" drop all in-memory state and reopen from
   the last fsynced file plus the S3 manifest. Checker: every acknowledged mutation is either durable or
   within the stated RPO window; nothing is applied twice.
3. **DST-2, cluster sim (with IR-5).** The same harness with several agents, controld logic (controller,
   locator, issuer), a placement fake with linearizable CAS, and a simulated network (seeded delay, drop,
   duplicate, reorder, partition). Operations: create, move, kill, restart, attest-dead/restore, ticket
   rotation, locator cache loss. Invariants:
   - at most one owner serves writes per database per epoch;
   - epochs only rise;
   - no acknowledged write is lost across a move;
   - each database's history is **linearizable**, checked with a Porcupine-style checker over the client-side
     history of IR requests and results.

   Status (2026-11-06, `dst/2-cluster`): built, `internal/sim` `RunCluster`; real agentd path (via
   `internal/agentglue`), controller, locator, issuer rotation and edge over a placement fake, `durabletest`
   and a simulated network; register and bank IR workloads; kill -9 losses judged against a parameterized RPO
   (§9 still open). Note: `server/knowledge/determinism/2026-11-06-dst2-dst3-cluster-sim.md`.
4. **DST-3, swarm.** Run many seeds nightly with fault probabilities varied per run (swarm testing), shrink
   failures to a minimal seed and op script, and commit them as regression vectors. Later option: run the real
   binaries under a deterministic hypervisor (e.g. Antithesis) to cover code that DST can't reach.

   Status (2026-11-06): built: `ClusterConfig.Swarm`, `ShrinkCluster` (op scripts, delta debugging), vectors
   in `server/internal/sim/testdata/cluster`, `server/scripts/sim-swarm.sh` (`make sim-swarm`,
   `deno task sim:swarm`). No CI runs it yet.

Real SQLite stays in the sim (modernc is pure Go and deterministic for a given op order). Crash points are
modeled at fsync boundaries: the sim snapshots the file on each fsync and restores the last snapshot on a
crash.

### 6.5 System tests (real processes)

- The e2e suite (`tests/e2e/platform/`) runs against the Go binaries through the SDK (IR-3 onward).
- **Jepsen-style runs:** real agentd, controld and edge, with `faultnet` between them, `docker pause` on
  SeaweedFS and Postgres, and kill -9 of agents. Many SDK clients run register and bank-transfer style
  workloads on documents. The history is checked for linearizability per database and for conservation (bank
  totals).
- **Soak:** mansio-server's `scripts/soak/` harness, switched to IR workloads, run before calling each step
  done (shortened) and once for 72 h before cut-over.
- **Performance gates:** sqlbench and frontbench gain an IR mode; k6 `mixed.js` through the SDK. Regressions
  are tracked against the Deno baseline from replace-platform phase 0.

### 6.6 What each step must add

| Step | Vectors             | Property/fuzz                                                 | Model                                              | DST                   | System                     |
| ---- | ------------------- | ------------------------------------------------------------- | -------------------------------------------------- | --------------------- | -------------------------- |
| IR-0 | corpus + TS runner  | —                                                             | —                                                  | —                     | —                          |
| IR-1 | Go runner           | differential compiler, validation equivalence, 3 fuzz targets | —                                                  | DST-0                 | —                          |
| IR-2 | results/errors      | `FuzzDecodeRequest` on the live route                         | `FuzzDocExecModel`, extended actor/durable fuzzers | DST-1                 | —                          |
| IR-3 | SDK-generated cases | precondition derivation properties                            | handler retry model                                | DST-1 with SDK client | e2e `documents`, `queries` |
| IR-4 | auth/tenant cases   | key and session parsing fuzz                                  | —                                                  | —                     | remaining e2e              |
| IR-5 | —                   | —                                                             | —                                                  | DST-2, DST-3          | Jepsen-style, soak         |

## 7. Local-first: designed in now, built later

What v1 already does for it:

- **Ops as the sync unit:** small, deterministic, idempotent by request id, ordered per database.
- **Client-generated `_id`s,** so offline inserts never need a server round trip.
- **Versions on every document and `expect` preconditions,** so a replayed op detects conflicts instead of
  overwriting.
- **The data plane is files plus generations:** a device can download the latest generation (the manifest
  names it and its sha256) and query it locally.

Later steps, each on top of the last:

1. **Read replicas:** pull a generation and its WAL tail (OS-13); ticket perm `read` allows the download.
2. **Op log:** an optional `_mansio_oplog` table, written in the same transaction as each mutation, so devices
   can pull "ops since N".
3. **Exclusive checkout:** move ownership to a device (fencing and epochs as built), then check in.
4. **Multi-writer sync:** the device applies ops optimistically to its local copy. The server replays them
   through the same executor, and the device rebases on the server's order. Handlers that run in the SDK are
   already the "same code on both sides" this needs.

## 8. Not in scope now

- **Server-enforced rules for untrusted callers.** Options later: a per-table permission and validation layer
  in Go, or server-side handlers in a sandboxed engine (replace plan §4.2) for the apps that need them.
- **Range preconditions** (v1.1), pagination cursors, schema migrations.
- **Raw SQL from apps.** `/v1/query` stays internal, for benches and admin.

## 9. Decisions this plan makes, and what is still open

Made here (record as ADRs at IR-0):

- the IR is the only app wire;
- handlers run in the SDK with derived preconditions;
- the compiler is ported to Go and the TS compiler stays as the oracle;
- `(tenant, app)` identifies a database;
- client ids are UUIDv7.

Still open:

- leases, and the WAL degraded mode (from mansio-server);
- the durability promise: today `synchronous=FULL`, versus mansio-server's write-back RPO;
- **PAUSED (user, 2026-11-06): S3 backup work is on hold; the code stays working as is, and LiteFS or similar
  will be evaluated instead of full-file copies. Open issues:
  `server/knowledge/writeback/2026-11-06-backup-paused-open-issues.md`. Correction: with the shipped default
  (`write_back_every` 10 s, 1 s tick) a kill -9 can lose about 11 s of acknowledged writes; the 170 ms below
  is the Jepsen test config (200 ms). A rollback also reused `_version` numbers (fixed 2026-11-10 by the
  version floor, `server/knowledge/writeback/2026-11-10-version-floor.md`; the vector is now
  `server/internal/sim/testdata/cluster/version-reuse-after-rollback.json`).** Earlier text: acknowledged
  writes lost to kill -9. A durable agent killed with kill -9 and restarted serves each database from its last
  published generation, so writes acknowledged inside the write-back window are lost (the RPO;
  `server/knowledge/writeback`, DST-1's RPO rule). Measured in the IR-5a Jepsen-style run
  (`server/cmd/controld/jepsen_test.go`, write-back every 200 ms, about 400 register writes/s): about 170 ms
  of acknowledged writes lost in one crash; in a 2.5-minute run, 5 crashes of the register database's owner
  lost an estimated 40, 12, 14, 35 and 0 acknowledged register writes (with S3 paused before a crash, the
  window grows by the pause). Options: accept the RPO (and document it to app developers), shorten the window,
  acknowledge only after a publish, or WAL shipping. The Jepsen checker currently accepts the loss (it checks
  each database's history per crash-free segment of its owner);
- whether `find` reads inside mutations should be guarded by default once v1.1 exists.
