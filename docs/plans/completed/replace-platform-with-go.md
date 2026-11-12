# Replace mansio's control and data plane with mansio-server

Status: plan · Date: 2026-11-05 · Baselines: mansio `main` at `addda3d` (plus uncommitted docs), mansio-server
`main` at `1eac7d4` · Supersedes the build order in
[scale-out-go-greenfield.md](../../architecture/scale-out-go-greenfield.md) §4, which was written before
mansio-server had a control plane. Changes no code.

> **Status (2026-11-06): done.** Phase 7 landed as IR-6 on `ir/06-cutover` (ADR 0013): the Deno platform is
> deleted, e2e runs on Go only (8/8), and the TS event-log ingest stays as an offline CLI (`eventlog/`,
> `scripts/logs.ts`) because Go has no ingest. The rest of this document is the plan as written.

> **Update (2026-11-05):** the handler-engine track (D2, D3, phases 0 and 2) is replaced by
> [ir-protocol.md](ir-protocol.md): the SDK sends document operations (IR) and runs handlers itself; the Go
> backend runs no TypeScript. Phases 1 and 3–7 below still apply, over IR.

## 1. Goal and end state

Delete the Deno server (`platform/`, 16k lines, plus its 30k lines of platform tests) and run mansio on the Go
backend in mansio-server. TypeScript stays where users touch it: the SDK, the schema DSL, the handler API and
the sample app. Users see the same SDK, the same wire contract (`{"result"}` / `{"error":{tag,message}}`, the
same statuses and tags) and the same document semantics.

```
TODAY                                         END STATE
sdk/client.ts ──HTTP──► Deno platform/         sdk/client.ts ──resolve──► controld (locator, tickets, tenants, auth)
                         router, auth,                       ──HTTP────► edge ──► agentd (Go)
                         Storage (FFI),                                             front: /apps/:app/(query|mutation)/:name
                         handlers (in-process),                                     JS engine: app + sdk + core handlers
                         reader child (DuckDB),                                     actor per database (SQLite, group commit)
                         backups, Postgres                                          vactor: S3 generations, GC, cache
                                                                                    owner: fencing, epochs
```

## 2. What mansio-server already covers, and the gaps

mansio-server today (see its `docs/control-plane-plan.md`) has: actor per database with group commit,
savepoints, request ids and deadlines; S3 durability (generations, conditional manifest, GC, generation cache,
`dbtool`); single-owner fencing; agentd with TLS, admission, metrics, readiness, bounded shutdown; controld
with Postgres placement, tenants/databases/VM registries, create/move/drop/restore, locator, Ed25519 tickets,
API keys; an edge; a Go client. It does **not** speak mansio's language: its wire is raw SQL on `/v1/query`.

| mansio today (file)                                                                                                             | Replacement                                                                                                                                                                       | Status                                     |
| ------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------ |
| Router, wire format, status map (`platform/http/router.ts`, `core/errors.ts:554-620`)                                           | New **document front** in agentd serving `/apps/:appId/(query\|mutation)/:name`, `{result}`/`{error:{tag,message}}`, `X-Trace-Id`, 64 KiB cap, check order of `handlers.ts:12-17` | **Build**                                  |
| Handlers in-process inside `BEGIN IMMEDIATE` (`platform/sqlite.ts:683-765`)                                                     | JS engine inside the actor goroutine; one `SAVEPOINT` per handler inside a group-commit batch (greenfield §3)                                                                     | **Build — the gate**                       |
| Compiler, decode, schema validation (`core/compile/*`, `core/schema.ts`)                                                        | Run them **inside the engine**, unchanged (§4.2); port to Go later only if profiles demand                                                                                        | Reuse                                      |
| Per-file writer `Semaphore(1)`, 64-handle LRU, global locks (`sqlite.ts:147, 1005-1145`)                                        | `internal/actor` + `internal/vactor`                                                                                                                                              | Done                                       |
| Statement deadline + interrupt (`sqlite_ffi/classify.ts`)                                                                       | Actor `StmtTimeout`, ctx-bounded `Do`                                                                                                                                             | Done (map to `StatementDeadlineError` 504) |
| Read-only query path (`document_reader.ts`)                                                                                     | Actor read pool (read-only connections)                                                                                                                                           | Done (enforce `QueryWriteAttemptError`)    |
| Schema fingerprint `_platform_schema` (`sqlite.ts:916-947`)                                                                     | Produced by the in-engine DDL path, byte-identical                                                                                                                                | Build (small)                              |
| Tenants + API keys (`platform/tenants/*`, migration `001`)                                                                      | controld `tenants`, `api_keys` (CP-4/5)                                                                                                                                           | Done, **formats differ** (§4.4)            |
| Better Auth sessions + memberships (`platform/auth/*`, migration `002`)                                                         | Go auth in controld (scrypt-compatible verify)                                                                                                                                    | **Build**                                  |
| `appId` + static handler import (`platform/registry.ts:22-23`)                                                                  | App bundles loaded by agentd; database key = `(tenant, app)`                                                                                                                      | **Build**                                  |
| Backups `VACUUM INTO` → S3 + `latest.json`; offline restore (`backup.ts`, `scripts/restore.ts`)                                 | Write-back generations + manifest, `dbtool` restore, GC                                                                                                                           | Done (and fixes the tenant-backup gap)     |
| Admin tenant HTTP + CLI (`/admin/tenants*`, `scripts/tenants.ts`)                                                               | controld API + CLI                                                                                                                                                                | Done, **shapes differ**                    |
| JSONL events, DuckDB ingest, analysis catalog, logs CLI, k6 ingest (`core/events.ts`, `platform/eventlog`, `platform/analysis`) | Go emits the `core/events.ts` envelope; ingest + catalog become an offline CLI                                                                                                    | **Build** (emitter) / move (ingest)        |
| `/health`                                                                                                                       | agentd `/livez`, `/readyz`                                                                                                                                                        | Done (keep `/health` alias)                |
| SQLite FFI binding, reader child, IPC, Effect layers                                                                            | —                                                                                                                                                                                 | Deleted                                    |

## 3. Decisions to make first

| #  | Decision                      | Recommendation                                                                                                                                                                                                                                                                    |
| -- | ----------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| D1 | Repo layout                   | Move mansio-server into mansio as `server/` with `git subtree` (history kept). One repo means the e2e suite, SDK and server change in one commit. mansio-server stays read-only afterwards.                                                                                       |
| D2 | Handler engine                | Decided by the spike (§5 phase 0): goja vs QuickJS-on-wazero vs V8/cgo. If none passes, fall back to greenfield option B (Deno sidecar, optimistic commit).                                                                                                                       |
| D3 | Where the compiler runs       | Inside the engine with the handler (`db.*` → `core/compile` → host `exec(sql, args)` → `decode`). Zero port, exact parity with `GENERATED_SQL.txt`. Port to Go only for a measured reason.                                                                                        |
| D4 | App model                     | A database is `(tenant, app)`. Fixes today's un-namespaced tenant files (both apps sharing `<tenantId>.db`). Off mode: tenant `default`.                                                                                                                                          |
| D5 | Auth                          | Go auth in controld: email/password with Better Auth's scrypt parameters (N=16384, r=16, p=1, 64-byte key, NFKC, `salt:hex`) so accounts migrate without resets — verify against a real hash first. Sessions as opaque bearer tokens; memberships unchanged.                      |
| D6 | API keys                      | Keep accepting existing `mansio_<base64url>` keys (SHA-256 lookup, as today) alongside mansio-server's `mansio_<id>_<secret>`. Migrate rows; issue new keys in the new format.                                                                                                    |
| D7 | Observability                 | Go writes mansio's event envelope (`http`, `handler`, `tx`, `sql`, `queue`, `backup`, `lifecycle`) as JSONL with the same redaction, so ingest and the analysis catalog keep working. Ingest moves to an offline CLI (ADR 0004/0007 still hold). Prometheus metrics stay as well. |
| D8 | Wire for multi-node           | Single URL first: the edge routes `/apps/...` requests. Phase 6 adds resolve + tickets to the TS SDK (`createClient(baseURL, ...)` keeps working with `baseURL` = locator).                                                                                                       |
| D9 | Still open from mansio-server | Leases (`mansio-server/knowledge/fencing/2026-11-05-leases-options.md`), WAL-shipping degraded mode. Neither blocks phases 0–5.                                                                                                                                                   |

## 4. Design notes

### 4.1 Document front (agentd)

`POST /apps/:appId/(query|mutation)/:name`, auth (§4.4) → resolve `(tenant, app)` database → admission →
actor. Same check order, size cap, trace-id rules and error rendering as `router.ts` (5xx messages carry only
the tag and trace id; 504 names the deadline). Tags come from the handler (engine) or from Go
(`QueueFullError` 503, `StatementDeadlineError` 504, `RequestTooLarge` 413, `AuthError` 401, `unavailable` →
503). The status map is generated from `core/errors.ts` so both sides can't drift (a test diffs them).

### 4.2 Handlers in the engine

```
mailbox → batch of N mutations
  BEGIN IMMEDIATE
    each: SAVEPOINT h_i → engine.invoke(app, name, args, db)   db.* → compile (JS) → host exec on the actor conn
          → result must JSON round-trip (NonSerializableResultError) → RELEASE h_i | ROLLBACK TO h_i
  COMMIT → reply to all N
queries: read-pool connection, BEGIN (read) … COMMIT, writes refused (QueryWriteAttemptError)
```

The bundle is `app/` + `sdk/{db,functions,schema}` + `core/{schema,plan,errors,compile}` + `effect`, built
with esbuild per app. Runtimes are pooled per app and lane, not per database. No ambient I/O in the engine
(sandbox tests). Request ids (mansio-server CP-2) give mutations exactly-once retries. That's new for mansio:
add an optional `Idempotency-Key` header to the SDK.

### 4.3 Data and file migration

Existing SQLite files (`<DATA_DIR>/<appId>.sqlite`, `<TENANTS_DIR>/<tenantId>.db`) become first generations:
`dbtool import <file> --tenant --app` checks the `_platform_schema` fingerprint, uploads the file as a
generation, writes the manifest, and creates the placement row. Postgres: one migration maps `tenants` and
`api_keys` into controld's schema, and moves Better Auth `user`, `account`, `session` and `memberships` into
the Go auth tables. Dry-run it against a copy first.

### 4.4 Auth on the request path

Single node (phases 2–5): agentd verifies credentials itself through a small auth port. Off mode uses the app
token. api-key mode does a SHA-256 lookup with a cache, as `auth.ts` does today but without the global lock.
Session mode needs exactly one membership. Multi-node (phase 6): controld authenticates once per resolve and
issues the ticket; agentd only verifies tickets. The rule "the tenant always comes from the credential; a
forged header is ignored" carries over in both modes.

## 5. Phases (each ends with a gate, run by the existing e2e suite)

The e2e harness (`tests/e2e/platform/harness.ts`) gains `BACKEND=deno|go`. It translates its env options into
a Go config file instead of rejecting unknown names. Until phase 7, every scenario runs against both backends.
The Deno backend is the reference.

| Phase                                   | Work                                                                                                                                                                                                                                                                                                                | Gate                                                                                                                                                                              |
| --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **0. Spike + baseline**                 | Bundle `app/functions.ts` with Effect; run `createGame`/`makeMove` in each engine inside a mansio-server actor with a minimal `db` binding; measure p50/p99 overhead, memory per runtime, savepoint-batch throughput. Baseline mansio on the same workload (k6 `mixed.js`). Verify one real Better Auth hash in Go. | An engine runs the bundle unmodified; handler overhead well under SQLite cost; D1–D8 recorded as ADRs                                                                             |
| **1. One repo**                         | `git subtree add --prefix=server` mansio-server; Go CI tasks in `deno.json`/Makefile; harness `BACKEND=go` that boots agentd (single node, file placement, local S3 = SeaweedFS from `docker-compose.yml`)                                                                                                          | Harness starts and health-checks the Go binary                                                                                                                                    |
| **2. Single node, TENANCY=off**         | Document front (§4.1), engine pool and actor integration (§4.2), fingerprint, app bundles for both storage layouts, error/status parity, `X-Trace-Id`                                                                                                                                                               | e2e `documents`, `queries` green on Go; `GENERATED_SQL.txt` identical; differential property test (fast-check plans → same rows from Deno and Go)                                 |
| **3. Tenancy + auth**                   | `(tenant, app)` databases; auth port (app token, API keys with the old format, sessions); controld admin endpoints compatible with `scripts/tenants.ts` or the CLI switched to controld; Go auth with `/api/auth` sign-up/sign-in; Postgres migration (§4.3)                                                        | e2e `auth` (Better Auth client calls replaced by plain HTTP), `tenant_isolation`, `admin_lifecycle` green on Go                                                                   |
| **4. Durability**                       | `/admin/backup/:appId` → force write-back and return a manifest-shaped answer; `scripts/restore.ts` → `dbtool restore`; `dbtool import` for existing files; backups now cover tenant databases                                                                                                                      | e2e `backup_restore` (rewritten for generations) green; an import of a Deno-written file opens with no drift                                                                      |
| **5. Observability**                    | Go JSONL emitter for the event envelope with redaction; offline `logs ingest` (keep the TS ingest as a CLI, or port it); `/admin/logs/*` on controld or the CLI; k6 scripts against Go                                                                                                                              | e2e `log_analysis` (rewritten: no direct reader IPC) and `observability_test.ts` green; the catalog's queries return the same shapes                                              |
| **6. Multi-node**                       | TS SDK: resolve + ticket + location cache + re-resolve rules (port mansio-server `internal/client`), request ids; edge in front; controld `create/move/drop` for `(tenant, app)`                                                                                                                                    | New e2e `scale_out`: two agents, a move under load with no lost or duplicate mutation, a VM restored after an operator attestation; the locator down doesn't stop running traffic |
| **7. Cut over and delete** (done, IR-6) | Remove `platform/`, `core/{events,eventlog,logging,ipc}` (if ingest was ported), the Deno server tasks, the FFI and reader tests, Better Auth deps, `TENANCY` env plumbing; update the C4 diagrams, README, CLAUDE/AGENTS, `docs/testing.md`                                                                        | e2e green on Go only; `deno task ci` covers SDK, core and app; one ADR records the switch                                                                                         |

Phases 2–4 fix the Deno platform's scaling issues 2–5 on a single node before any multi-node work, the same
"in-node first" order as before.

## 6. What survives in mansio after phase 7

`sdk/` (client gains resolve and tickets), `app/`, `core/{schema,plan,errors,compile}` (bundled into the
engine and used by the SDK), `tests/{unit,property}` for those, the e2e suite (now the server's conformance
suite), `perf/` k6 scripts, and `server/` (Go). Gone: about 16k lines of platform code and about 25k lines of
platform-only tests (`storage`, `sqlite_ffi_*`, `reader*`, `ipc`, `eventlog`, `retention*`, `tenant_*`,
`http`, `server_*`, `backup`, `object_store`, ...).

## 7. Risks

- **The engine gate.** Effect generators and fibers in goja or QuickJS are unproven. If performance fails,
  option B (sidecar plus optimistic commit) adds read-set validation and retries on hot databases.
- **Byte-level compatibility:** the schema fingerprint, the JSON layout's `CAST(... AS REAL)`, integer safety,
  U+0000 refusal (ADR 0005), and `bigint` reads. Mitigated by running the compiler in the engine (D3) plus the
  differential tests.
- **Auth migration:** scrypt compatibility is unverified until phase 0. Session semantics (exactly one
  membership) must hold.
- **Two backends during the transition:** freeze features on the Deno side once phase 2 starts.
- **Observability parity** is wide (10 catalog queries, 8 event kinds). Keep the TS ingest as a CLI rather
  than porting it, unless Deno must go entirely.
- **Durability defaults:** mansio runs `synchronous=FULL` today. mansio-server defaults to `NORMAL` plus
  write-back RPO, and WAL shipping is still a design. Decide the RPO mansio promises before phase 4 (D9).

## 8. Size (estimate, unverified)

New Go: front plus engine integration (~2–3k lines), auth (~1.5k), event emitter (~0.8k), import/admin glue
(~1k). New TS: SDK resolve and ticket client (~0.5k), harness changes (~0.3k), e2e rewrites (~0.5k). Deleted:
~16k platform lines and ~25k test lines. The largest single uncertainty is phase 0.
