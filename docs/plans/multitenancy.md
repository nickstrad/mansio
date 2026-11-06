# Plan: Multitenancy — Postgres central DB, per-tenant SQLite, admin CLI

Status: proposed · Date: 2026-10-10 · Target branch: feature branches off `main`, one commit per item.

Builds on: `docs/plans/effect-port-and-observability.md` items 01–29 complete. This plan is additive — it does
not modify that plan. Where this plan touches files owned by earlier items, it extends them (new config rows,
new routes, new service methods), never rewrites them.

## 0. Where the code is today (multitenancy-relevant surface)

Verified by reading the repo on 2026-10-10.

| Surface                | Today                                                                                                                                                    |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sdk/client.ts`        | `createClient(baseURL, appId, token)` → `POST {baseURL}/apps/{appId}/{kind}/{name}` with `authorization: Bearer <token>` and `x-trace-id`                |
| `platform/config.ts`   | Options table (`name`, env var, type, default, description): `adminToken`, `dataDir`, `s3BaseUrl`, `awsRegion`, … — the ONLY place that reads `Deno.env` |
| `platform/services.ts` | `Context.Tag` declarations: `Config`, `Storage`, `Reader`, `ObjectStore`, `Backups`, …                                                                   |
| `platform/server.ts`   | Regex routing; `/admin/*` guarded by `ADMIN_TOKEN` bearer check                                                                                          |
| Item 15 `Storage`      | `Semaphore(1)` serialized writer queue over a **single** SQLite file, `BEGIN IMMEDIATE`                                                                  |
| `docker-compose.yml`   | SeaweedFS only, plus a commented template block for adding services; `Makefile`: `up/down/restart/ps/logs/clean`                                         |
| Auth                   | Per-app static tokens (`columnAppToken`, `jsonAppToken`); no users, no sessions                                                                          |

## 1. Target architecture

```text
Postgres (docker-compose, new)          Per-tenant SQLite files
┌──────────────────────────┐           ┌─────────────────────────────────┐
│ tenants(id, slug, name,   │           │ <TENANTS_DIR>/<tenant-id>.db    │
│   status, created_at)     │           │ (one file per tenant, same DDL) │
│ api_keys(id, tenant_id,   │           │                                 │
│   key_hash, prefix, name, │           │ routed via TenantStorageRouter │
│   revoked_at, created_at) │           │ (local folder now; control-plane│
│ users/session/account/   │           │  pluggable later)               │
│   (Better Auth tables)   │           └─────────────────────────────────┘
└──────────────────────────┘                        ▲
         ▲                                          │ Storage depends only
         │ TenantRegistry (Effect service)          │ on the router tag
         │                                          │
Client ──┼── Authorization: Bearer <api-key> ───────┘
         │         │
         │         ▼
         │   server resolves tenant server-side
         │   from the key hash (cached); the tenant
         │   id is NEVER trusted from client params
         │
Admin ───┴── scripts/tenants.ts → /admin/tenants/* (ADMIN_TOKEN)
```

**The load-bearing rule** (from the Convex/Auth0 research in §2): tenant identity is derived server-side from
the credential on every request. A client-supplied tenant id is never trusted — a cross-tenant read must
behave exactly like reading a row that does not exist.

**Tenancy model:** database-per-tenant at the SQLite layer (one file per tenant — the human's explicit
choice), single shared Postgres for the central registry. This matches the platform's existing shape (SQLite
files are already the unit of backup/restore in item 19) and keeps tenant data physically separable. Routing
to those files goes through the `TenantStorageRouter` interface (MT-04) — never hardcoded paths — so a future
control-plane router plugs in as a different layer with no changes to `Storage`, the tenant middleware, or the
client.

## 2. Research

### 2.1 How Convex and other BaaS clients pass tenant context

- **Convex.** The deployment URL is the tenant boundary for the platform itself — the client connects to a
  deployment URL and Convex routes by it. _Within_ one deployment, multi-tenant SaaS is app-level: every
  tenant row carries an `organizationId` (e.g. the Clerk organization ID), the server derives
  `{ userId, orgId, role }` from the auth JWT via `getAuthContext(ctx)`, and queries filter on an indexed
  `by_company`-style index. Tenant context comes from the **token**, never from client parameters. (Sources:
  Convex project-configuration docs — `CONVEX_DEPLOYMENT` env + deployment URL; dev.to Convex Champions
  multi-tenant walkthrough — `companyId` scoping pattern.)
- **Auth0 Organizations.** The client passes `organization=<org_id>` at authorize time; the org lands in the
  token claims and the API trusts the claims, not a request field. (Source: Auth0 multi-tenancy Organizations
  guide.)
- **App-layer isolation pattern** (sveltekit-cf-template `docs/multi-tenancy.md`): the Actor is derived from
  the session by the route, never from the request body; services scope every query by `organization_id`; a
  cross-tenant read returns nothing.
- **Decision for mansio:** the existing `Authorization: Bearer <token>` header carries a **tenant-scoped API
  key** (or, after MT-08, a Better Auth session token). The server hashes the presented key, looks up the
  tenant in Postgres, and injects the tenant id into the Effect layer. The TS client's `token` parameter
  becomes that key — no new client field is needed, which keeps the `createClient(baseURL, appId, token)`
  signature stable.

### 2.2 Better Auth headless (no webapp): VIABLE

Researched against current docs (better-auth.com, Oct 2026) and field reports:

1. **Server is framework-agnostic.** Official docs: "Better Auth supports any backend framework with standard
   Request and Response objects." `auth.handler` is a plain web handler mountable at `/api/auth/*` on
   `Deno.serve` or the Effect `HttpRouter`. Field evidence: an Effect-TS codebase mounts it via
   `HttpRouter.add("*", "/api/auth/*", ...)` with zero Next.js involved.
2. **Vanilla JS client exists.** Official docs: "Better Auth comes with a client for all the popular web
   frameworks, including vanilla JavaScript" — `createAuthClient` from `better-auth/client` is fetch-based and
   runs in a Deno CLI.
3. **Email/password is built-in:** `emailAndPassword: { enabled: true }`; no email sending is needed when
   `requireEmailVerification: false`.
4. **Known CLI wrinkle (not a blocker):** sessions are cookie-based by default; a headless CLI must capture
   the session token from the sign-in response and send it back explicitly. MT-08 therefore starts with a
   **verification spike**: if the token cannot be captured and used headlessly, the item stops and the plan
   falls back to API-keys-only (auth deferred) — the human's stated punt condition, documented in the item.

**Auth decision: proceed with Better Auth email/password headless, with the MT-08 spike as the go/no-go
gate.** If the spike fails, MT-08 closes as headless-infeasible and opaque API keys remain the identity story;
no other item depends on MT-08.

### 2.3 Postgres in docker-compose

Standard pattern, following the file's existing commented template: `postgres:17` image,
`POSTGRES_DB/USER/PASSWORD` env (dev defaults documented, never committed secrets), `pg_isready` healthcheck,
named volume `pg-data`. `make up` already waits on healthchecks, so no Makefile logic changes are needed —
only a `db-shell` convenience target.

### 2.4 New npm dependencies under the `deno.json` freeze

MT-03 needs a Postgres driver (`npm:postgres@3.4.9` — pinned per the sq-10 bake-off, choice justified in a
code comment) and MT-08 needs `better-auth`. **No ORM, ever:** MT-03 (and any other item touching Postgres)
uses the pinned `npm:postgres` client directly with hand-written SQL, leveraging SQL syntax — not an ORM or
query builder generating SQL internally. Direct hand-written SQL is tunable and has no hidden query
generation. The main plan's §2.2 freezes `deno.json` after item 01, so neither item may add import-map
entries.

**Decision:** builders use **inline pinned `npm:` specifiers** (e.g.
`import postgres from "npm:postgres@3.4.5"`) — exact versions, no ranges — which Deno resolves without
touching `deno.json`. The resulting `deno.lock` entries are owned by the item that first introduces each
package: **MT-03** owns the lock entries for the Postgres driver, **MT-08** owns the lock entries for
`better-auth`; each item regenerates the lock (via the test run) and commits the lockfile delta in the same
commit. Reviewers reject unpinned specifiers.

## 3. Batch table

| Batch | Items        | Scope                                             | Gate                                                             |
| ----- | ------------ | ------------------------------------------------- | ---------------------------------------------------------------- |
| MT-A  | MT-01, MT-02 | Postgres service + schema; config options         | concurrent (disjoint files)                                      |
| MT-B  | MT-03, MT-04 | TenantRegistry service; per-tenant SQLite routing | concurrent (MT-04 takes tenant id as a plain param)              |
| MT-C  | MT-05, MT-06 | Admin CRUD endpoints; admin CLI                   | MT-06 after MT-05 (CLI dogfoods the endpoints)                   |
| MT-D  | MT-07, MT-08 | Tenant resolution in request path; Better Auth    | MT-08 after MT-07 (needs the tenant-context seam)                |
| MT-E  | MT-09        | Property tests for tenant routing invariants      | after MT-03 and MT-04 (needs the registry and router interfaces) |
| MT-F  | MT-10        | End-to-end platform test suite (`deno task e2e`)  | **SYNC**, alone; after MT-06, MT-07, MT-08 land; before item 27  |

Model policy (same as the main plan): **Opus** for security boundaries and resource lifetimes; **Sonnet** for
bounded, mechanical items with checkable acceptance.

**Build order with the main plan** (from the ordering analysis — do not reorder):
`22 → 24 → batch 11: (30+35) → 31 → 36 → 32 → 33 → 34 → 37 →
(23+25+MT-01+MT-02) → (26+MT-03+MT-01b) → (MT-04+MT-05) → (MT-07+MT-09) → (MT-06+MT-08) → MT-10 → 27 →
(28+29)`.
**Non-reorderable reflection gate:** all eight main-plan batch-11 items (30–37) land after 22 and before 23 or
any MT work. Item 24 moves forward as 36's prerequisite, not into the MT stack. Main-plan §3 records the
limited deferral of 24's CLI check until 23 creates the CLI; 36 supplies direct ingest/retention evidence.
Within batch 11 only 30 and 35 run concurrently; all other arrows are landing barriers. Parenthesized items
run concurrently, subject to the batch gates above; MT-06's CLI work still starts only after MT-05's endpoints
exist. MT-10 (added 2026-10-11) runs alone in its slot: it needs every tenancy and auth item landed, and item
27 documents it, so it can neither move earlier nor run beside 27.

---

#### MT-01 — Postgres in docker-compose + central schema

- **Scoping:** CONCURRENT with MT-02.
- **Model:** **Sonnet / sol.** Mechanical: one service block following the file's template, one SQL file.
- **Owns:** `docker-compose.yml` (additive `postgres` service block + `pg-data` volume),
  `platform/tenants/schema.sql`, `Makefile` (additive `db-shell`, `db-migrate` targets only).
- **Reads:** `docker-compose.yml` (template block), `Makefile`.
- **Work:**
  1. `postgres:17` service: `POSTGRES_DB/USER/PASSWORD` from env with dev defaults, `pg_isready -U <user>`
     healthcheck, named volume `pg-data`, resource limits matching the file's style.
  2. `platform/tenants/schema.sql`:
     `tenants(id uuid PK, slug text UNIQUE, name text, status text DEFAULT 'active', created_at timestamptz)`;
     `api_keys(id uuid PK, tenant_id uuid REFERENCES tenants, key_hash text UNIQUE, prefix text, name text, revoked_at timestamptz, created_at timestamptz)`;
     index on `api_keys(key_hash)`.
  3. `make db-migrate`: applies `schema.sql` via psql (idempotent — `CREATE TABLE IF NOT EXISTS`);
     `make db-shell`: psql into the DB.
- **Acceptance criteria:**
  - `make up` → postgres reports healthy; `make db-migrate` → `\dt` lists `tenants`, `api_keys`.
  - `make down && make up` → tables persist (volume).

#### MT-02 — Config options for multitenancy

- **Scoping:** CONCURRENT with MT-01.
- **Model:** **Sonnet / sol.** Follows the item-08 options-table pattern exactly.
- **Owns:** `platform/config.ts` (additive rows only), `README.md` "Configuration" section (additive),
  `tests/unit/config_test.ts` (additive rows only — owned by main-plan item 08; listed here as an allowed
  edit).
- **Reads:** `platform/config.ts` (options table).
- **Work:** Add `tenancyMode` (env `TENANCY`, default `"off"`, validated against
  `off | api-key |
  better-auth`), `databaseUrl` (default `postgres://mansio:mansio@localhost:5432/mansio`),
  `tenantsDir` (default `<dataDir>/tenants`), `tenantRouter` (default `"local"`; validated against the known
  router kinds — `"local"` today, `"control-plane"` reserved for the future), `betterAuthSecret` (no default —
  required when `tenancyMode` is `better-auth`), `betterAuthUrl` (default `http://localhost:3000`-style
  platform base URL), `tenantKeyCacheTtlMs` (env `TENANT_KEY_CACHE_TTL_MS`, default `30000`, validated `> 0`
  and `<= 300000` — the fixed, non-sliding TTL for MT-07's tenant-key cache). Semantics: `off` = pre-tenancy
  behavior (single SQLite file, existing app-token auth) — MT-07's middleware must be a no-op in this mode so
  the default app-token path keeps working; `api-key` = tenant API-key resolution enforced on `/apps/*`;
  `better-auth` = session tokens accepted alongside API keys. Keep the README section in sync (reviewer
  verifies parity, per the item-08 precedent).
- **Acceptance criteria:**
  - `ConfigLive` decodes the new options; missing `betterAuthSecret` fails with `ConfigError` only when
    `tenancyMode` is `better-auth`; an unknown `TENANCY` value fails with `ConfigError`.
  - `tenantKeyCacheTtlMs` decodes with its default; `0` and values above `300000` fail with `ConfigError`.
  - With `tenancyMode: "off"`, the pre-tenancy default path is preserved (no tenant resolution, no new auth
    behavior) — covered by the new rows in `tests/unit/config_test.ts`.
  - Reviewer verifies README lists exactly the new options.

#### MT-01b — Postgres migration runner (follow-up, MT-01 ownership)

- **Scoping:** CONCURRENT with item 26 and MT-03 (disjoint files).
- **Model:** **Sonnet / sol.** Mechanical: one Deno script, SQL files, a Makefile recipe change.
- **Owns:** `scripts/db/migrate.ts`, `platform/tenants/migrations/*.sql`, `Makefile` (the `db-migrate` recipe
  only — supersedes MT-01's psql-based recipe), `deno.json` (additive `db:migrate` task only).
- **Reads:** `platform/tenants/schema.sql` (becomes migration 001), `Makefile`.
- **Work:**
  1. Hand-rolled Deno migration runner at `scripts/db/migrate.ts` (no migration framework, no ORM — standing
     rule: plain SQL files, hand-written). Discovers `platform/tenants/migrations/` files named
     `<NNN>_<slug>.up.sql` / `<NNN>_<slug>.down.sql`, ordered by `<NNN>`.
  2. `schema_migrations(version text PK, applied_at timestamptz)` journal table, created by the runner itself
     when absent. Takes a Postgres advisory lock (`pg_advisory_xact_lock`) before planning so concurrent
     runners serialize; each migration applies inside its own transaction.
  3. Migration 001 = MT-01's `schema.sql` verbatim (baseline). `make db-migrate` runs the runner, which
     applies only pending migrations in order and is a no-op when the journal is current — replacing the old
     "apply schema.sql via psql" recipe. `make db-migrate down` (or `deno task db:migrate down`) rolls back
     the most recent migration via its `.down.sql`; refuses when the journal is empty.
  4. Better Auth tables (MT-08): generated once with Better Auth's own tooling, then captured verbatim as a
     numbered migration pair — never regenerated in place afterwards.
  5. Down migrations are best-effort and must never silently drop a table with live rows: a `.down.sql` that
     would drop a non-empty table fails loudly instead.
- **Acceptance criteria:**
  - Fresh DB: `make db-migrate` → `\dt` lists `tenants`, `api_keys`, `schema_migrations`; journal has one row
    (001). Second run → no-op, exit 0.
  - Add a scratch migration 002 (additive column), migrate up → column present; migrate down → column gone,
    journal row removed; delete the scratch files afterwards.
  - Two concurrent `make db-migrate` runs → exactly one applies, the other waits and exits 0 (advisory lock,
    no double-apply).
  - `deno task db:migrate` behaves identically to `make db-migrate`.

#### MT-03 — TenantRegistry service (Effect)

- **Scoping:** CONCURRENT with MT-04.
- **Model:** **Opus / astra.** Security boundary: key generation, hashing, uniqueness.
- **Owns:** `platform/tenants/registry.ts`, `platform/tenants/store.ts`, `tests/unit/tenants_test.ts`,
  `core/errors.ts` (additive only: `TenantError` tagged union with `MalformedTenantId`, `UnknownTenant`,
  `TenantDeactivated` — no other edits), `platform/services.ts` (additive only: the `TenantRegistry`
  `Context.Tag` appended to the tag-declaration section — the `TenantStorageRouter` tag belongs to MT-04;
  existing lines are never reordered so the concurrent MT-04 append merges cleanly).
- **Reads:** `platform/config.ts`.
- **Work:**
  1. `TenantRegistry` `Context.Tag` + interface in the `services.ts` style: `createTenant({name, slug})`,
     `getTenant(id)`, `listTenants()`, `updateTenant(id, patch)`, `deactivateTenant(id)`,
     `issueApiKey(tenantId, name)`, `revokeApiKey(keyId)`, `resolveTenantByKeyHash(hash)`.
  2. `store.ts`: Postgres access via the pinned `npm:postgres@3.4.9` driver (sq-10 bake-off; justify in a code
     comment with the citable paragraph from `docs/haiku-experiments/findings/sq-10.md`). **No ORM, ever**:
     the store uses the pinned `npm:postgres` client directly with hand-written SQL, leveraging SQL syntax —
     not an ORM or query builder generating SQL internally; direct hand-written SQL is tunable and has no
     hidden query generation. API keys: `mansio_` prefix + 32 random bytes; **SHA-256 hash stored**, plaintext
     returned exactly once at issuance; `resolveTenantByKeyHash` joins `api_keys` → `tenants` and rejects
     revoked keys and inactive tenants. Use `sql.begin` for `issueApiKey`-style atomic writes; match errors on
     `PostgresError` + `code === "23505"` (constraint `tenants_slug_key` in real Postgres); keep prepared
     statements (the default); re-measure if `prepare: false` is ever used (PgBouncer transaction mode).
  3. Unit tests with a test layer (ephemeral Postgres via `make up` in CI — MT-01's compose `postgres:17`;
     pg-mem is NO: its ROLLBACK silently does nothing across separate statements, it misnames the
     duplicate-slug constraint, and it does not parse the plan's bare `REFERENCES tenants` DDL. Do NOT change
     the bare `REFERENCES tenants` DDL to suit pg-mem — it is valid Postgres.)
- **Acceptance criteria:**
  - `deno task test` passes; tests cover: duplicate slug rejected, key plaintext shown once (second read
    impossible), revoked key rejected, inactive tenant's keys rejected, `resolveTenantByKeyHash` round-trip.

#### MT-04 — Tenant storage routing: interface + local-folder implementation

- **Scoping:** CONCURRENT with MT-03.
- **Model:** **Opus / astra.** Interface design plus resource lifetimes (one SQLite handle per tenant, bounded
  cache, idle eviction).
- **Owns:** `platform/tenants/routing.ts`, `platform/sqlite.ts` (additive),
  `tests/unit/tenant_routing_test.ts`, `platform/services.ts` (additive only: the `TenantStorageRouter`
  `Context.Tag` appended to the tag-declaration section — disjoint from MT-03's `TenantRegistry` tag; existing
  lines are never reordered so the concurrent MT-03 append merges cleanly).
- **Reads:** `platform/sqlite.ts` (item 15).
- **Work:**
  1. **Define the interface** in `routing.ts` — `TenantStorageRouter` as a `Context.Tag`, in the `services.ts`
     style. Routing goes through this interface; paths are never hardcoded in `Storage` or anywhere else:
     - `resolve(tenantId: TenantId): Effect<StorageLocation, TenantError>` — the contract: map a tenant to
       _where its data lives_. Performs no storage I/O itself.
     - `StorageLocation` is a tagged union: `{ _tag: "SqliteFile", path: string }` today; future kinds (e.g.
       `{ _tag: "ControlPlane", endpoint: string }`) extend the union without touching the interface.
     - `prepare(location: StorageLocation): Effect<void, TenantError>` — idempotent: ensure the location
       exists and carries the schema DDL (creates the file on first use for the local implementation).
       Re-check containment with `realpath` at open; refuse symlinks at `<tenantsDir>/<id>.db`.
       `StorageLocation` is constructible only by `resolve` (opaque/branded) — `prepare` never accepts a
       caller-constructed path.
     - Contract guarantees: `resolve` is deterministic per tenant id; implementations must reject malformed
       ids so path traversal is impossible by construction; `Storage` depends **only** on the
       `TenantStorageRouter` tag — never on paths, env vars, or a concrete router.
  2. **Local-folder implementation now:** `LocalFolderRouter` — `resolve` returns
     `{ _tag: "SqliteFile", path: "<tenantsDir>/<tenantId>.db" }`; the tenant id is a strictly parsed,
     canonical-lowercase UUID — a branded `TenantId` type. Anything else (uppercase, `urn:uuid:` forms, NUL,
     whitespace, percent-encoded forms, separators) is rejected with `MalformedTenantId`, never a path.
     Selected via the `tenantRouter: "local"` config value.
  3. **`sqlite.ts`:** `Storage` gains tenant-scoped access through the router — a bounded
     `Map<tenantId, handle>` with single-flight get-or-create (concurrent first opens share one handle) and a
     lease/refcount so idle eviction never closes an in-flight handle; **each tenant file keeps item 15's
     guarantee** (own `Semaphore(1)` queue, `BEGIN IMMEDIATE`). The single-file path remains for the
     pre-tenancy default (back-compat for existing deployments) — it is chosen once at startup by
     `tenancyMode`, never as a per-call fallback. **The `Storage` service interface is unchanged** — the
     tenant id arrives via a `TenantContext` tag in the Effect layer (injected per request by MT-07's
     middleware), never as a new method parameter. `TenantContext` is fiber-local (provided via
     `Effect.provideService`/`FiberRef`, never a process-global `Ref`); `Storage` reads it with `R = never`
     (e.g. `Effect.serviceOption(TenantContext)` or a `FiberRef`). In `api-key` and `better-auth` modes a
     missing context is a typed failure, never the single-file path. Item 20's `StorageTest` and the
     conformance suite keep running against the existing interface.
  4. **Document the extension point** in `routing.ts` header comments: a future `ControlPlaneRouter` (central
     routing service, remote files, whatever comes later) implements `TenantStorageRouter`, is selected via
     `tenantRouter: "control-plane"` + endpoint config, and plugs in as a different layer — no changes to
     `Storage`, the tenant middleware, or the client wiring.
- **Acceptance criteria:**
  - A test builds `Storage` against a stub `TenantStorageRouter` pointed at a temp dir and proves `Storage`
    never reads paths or env directly — the router is the only seam.
  - Two tenants write the same document id → fully isolated reads.
  - 50 concurrent writes across 5 tenants → no cross-talk, no lost updates (extends the sq-06 stress
    approach).
  - Path-traversal tenant id (`../x`) rejected by `resolve`; non-canonical ids (uppercase UUID, `urn:uuid:`
    form, NUL, whitespace, percent-encoded) rejected with `MalformedTenantId`.
  - Concurrent first requests to a new tenant open exactly one handle; eviction under load causes no failed
    request.
  - `Storage` call with no `TenantContext` in api-key mode → typed error (never the single-file path).
  - Item 20's `StorageTest` and conformance suite pass unmodified (the `Storage` interface is unchanged).

#### MT-05 — Admin CRUD HTTP endpoints

- **Scoping:** After MT-03 (needs the registry). MT-06 follows MT-05.
- **Model:** **Sonnet / sol.** CRUD over the registry behind the existing admin-token pattern.
- **Owns:** the admin route surface — `platform/server.ts` `/admin/*` section today, or `platform/http/*` if
  item 22 has landed (builder checks and targets whichever exists; exactly one).
- **Reads:** `platform/tenants/registry.ts`, `platform/server.ts`.
- **Work:** `POST /admin/tenants` (create, returns api key once), `GET /admin/tenants`,
  `GET /admin/tenants/:id`, `PATCH /admin/tenants/:id` (name/status), `DELETE /admin/tenants/:id` (soft →
  deactivate), `POST /admin/tenants/:id/keys` (issue), `DELETE /admin/tenants/:id/keys/:keyId` (revoke). All
  behind the existing `ADMIN_TOKEN` bearer check. Reactivation semantics: deactivation revokes the tenant's
  keys, so a reactivated tenant's old keys stay dead — reactivation requires issuing new keys.
- **Acceptance criteria:**
  - `curl` each endpoint: 200s with valid admin token, 401 without.
  - `POST /admin/tenants` response contains the plaintext key exactly once; a second `GET` never does.

#### MT-06 — Admin CLI (`scripts/tenants.ts`)

- **Scoping:** After MT-05 (dogfoods its endpoints).
- **Model:** **Sonnet / sol.** Small CLI, checkable by running.
- **Owns:** `scripts/tenants.ts`.
- **Reads:** MT-05's routes.
- **Work:** `deno run -A scripts/tenants.ts create <name> [--slug]`, `list`, `show <id>`, `deactivate <id>`,
  `issue-key <id> [--name]`, `revoke-key <key-id>`; talks to the admin endpoints using `ADMIN_TOKEN` env;
  table output; prints the API key once on create/issue-key with a "save this" warning.
- **Acceptance criteria:**
  - Full cycle against a live server: create → key works on `/apps/*`; revoke-key → key 401s; deactivate → all
    keys 401 (keys are revoked, not merely gated — reactivating the tenant does not resurrect them; new keys
    must be issued).

#### MT-07 — Tenant resolution in the request path + TS client tenant config

- **Scoping:** After MT-04 (needs routing). MT-08 follows MT-07.
- **Model:** **Opus / astra.** Security-critical: the tenant-context seam every request flows through.
- **Owns:** `platform/http/auth.ts` (new; or the auth section of `platform/server.ts` if item 22 hasn't landed
  — builder checks), `sdk/client.ts` (docs + `createTenantClient` alias only, no behavior change),
  `platform/layers.ts` (additive only: `PlatformLive` gains the `TenantStorageRouter` and `TenantRegistry`
  layers — the file belongs to main-plan item 22; listed here as an allowed edit).
- **Reads:** `platform/tenants/registry.ts`, `platform/tenants/routing.ts`, `sdk/client.ts`.
- **Work:**
  1. Request middleware: extract `Authorization: Bearer <key>` → `SHA-256` →
     `TenantRegistry.resolveTenantByKeyHash` (with a fixed, non-sliding TTL cache from `tenantKeyCacheTtlMs`
     (MT-02); negative lookups cached; cache size capped) → inject `TenantContext { tenantId }` into the
     Effect layer per request (fiber-local via `Effect.provideService`/`FiberRef`, never a global `Ref`).
     Every `Storage` access resolves through it. Revocation and deactivation evict the cached entry
     in-process; other instances see the change within one TTL (no cross-instance invalidation — the window is
     stated, not hidden).
  2. **Never trust client tenant identity:** ignore (and log at warn) any `x-tenant-id` header or tenant field
     in the body — the credential is the single source of truth (§2.1). Cap and rate-limit the warn line (a
     flood of unauthenticated `x-tenant-id` requests must not fill `LOG_DIR`).
  3. `sdk/client.ts`: document that `token` is now the tenant API key; add
     `createTenantClient(baseURL, appId, apiKey)` as a named alias for discoverability. No wire-format change.
     (2026-11-06, IR-3: the alias is removed; the IR `createClient` takes the API key as `token`.)
- **Acceptance criteria:**
  - Tenant A's key reading tenant B's document id returns not-found (indistinguishable from missing) — test
    with two CLI-created tenants.
  - Missing/invalid key → 401; revoked key → 401 within `tenantKeyCacheTtlMs` of revocation.
  - A request carrying a forged `x-tenant-id` is still scoped to the key's tenant (test asserts this).
  - Interleaved-concurrency test through the real HTTP middleware: two tenant keys, concurrent mutations —
    each tenant sees only its own writes.

#### MT-08 — Better Auth email/password, headless

- **Scoping:** After MT-07 (needs the tenant-context seam). The plan's last item.
- **Model:** **Opus / astra.** Auth is a security boundary; the spike needs judgment.
- **Owns:** `platform/auth/*`, `scripts/auth.ts` (admin user commands), `platform/http/auth.ts` (additive:
  session-token branch in MT-07's middleware), `platform/http/router.ts` (additive: `/api/auth/*` mount — or
  `platform/server.ts` if item 22 hasn't landed, builder checks; exactly one), `platform/tenants/schema.sql`
  (additive only: `memberships(user_id, tenant_id, role)` table — disjoint from MT-01's `tenants`/`api_keys`
  DDL).
- **Reads:** `platform/tenants/registry.ts`, MT-07's middleware, Better Auth docs.
- **Work:**
  1. **SPIKE FIRST (go/no-go):** mount
     `betterAuth({ database: <pg>, emailAndPassword: { enabled: true, requireEmailVerification: false } }).handler`
     at `/api/auth/*`; from a Deno script, `createAuthClient` (vanilla `better-auth/client`) → `signUp.email`
     → `signIn.email` → capture the session token → call a tenant endpoint with it. **If the token cannot be
     captured and used headlessly, STOP:** close the item as headless-infeasible, document API-keys as the
     auth story, and do not proceed to step 2.
  2. If the spike passes: Better Auth tables in Postgres (via `auth generate`/`migrate`); users↔tenants
     membership (`memberships(user_id, tenant_id, role)`); extend MT-07's middleware to accept session tokens
     (session → user → tenant); `scripts/auth.ts`: `users create <email> [--tenant]`,
     `users link <email> <tenant>`.
  3. **Decide token transport before build** (sq-09): `plugins: [bearer()]` iff the tenant middleware consumes
     `Authorization: Bearer <session token>` (without the plugin the body token 401s); else forward the signed
     cookie. Re-verify at the pinned better-auth version (record the `deno.lock` delta). Better Auth vs `pg`
     Pool compat is an open item: check whether Better Auth accepts a postgres.js client or needs its own `pg`
     Pool, and record the driver decision here. MT-08 depends on MT-01 landing `postgres:17` in compose.
- **Acceptance criteria:**
  - Headless sign-up/sign-in round-trip from a Deno script, exit 0.
  - Session token authenticates tenant-scoped requests; wrong password → 401; unknown user → 401.
  - If spiked infeasible: the spike script and a `docs.md`-style note recording the failure mode and the
    API-keys fallback (no silent punt).

#### MT-09 — Property tests for tenant routing invariants

- **Scoping:** After MT-03 and MT-04 (needs the registry and `TenantStorageRouter` interfaces). Batch MT-E.
- **Model:** **Sonnet / sol.** Bounded, mechanical, checked by running.
- **Owns:** `tests/property/tenant_routing_props.ts` (new file only — disjoint from MT-04's
  `tests/unit/tenant_routing_test.ts`).
- **Reads:** `platform/tenants/routing.ts`, `platform/tenants/registry.ts`, `core/errors.ts`,
  `tests/property/arbitraries.ts` (seed harness), `work-scripts/deno/tx-conflict-stress.ts` (sq-06
  concurrent-load approach).
- **Work:**
  1. **Seed-replay pattern (reuse the item-12/13 convention):** import the seed harness from
     `tests/property/arbitraries.ts` — `PROP_SEED` env var → `fc.configureGlobal({ seed, numRuns })`,
     `titled()` suffixes every test name with `[seed=..., runs=...]` so the seed appears in `deno test`
     output. A failure therefore prints the shrunk counterexample AND the seed; re-running with
     `PROP_SEED=<n>` reproduces the exact failing case. Do not invent a second seed mechanism.
  2. **Arbitraries:** tenant ids (valid canonical-lowercase uuids), malformed ids (`../x`, separators,
     unicode, empty strings, uppercase UUIDs, `urn:uuid:` forms, NUL, whitespace, percent-encoded forms — all
     of which must be rejected with `MalformedTenantId`), registry states per tenant
     (active/deactivated/unknown), doc ids, op sequences (put/get/del).
  3. **Properties over the router + registry seam:**
     - **Isolation:** for arbitrary distinct parsed canonical tenant ids `a ≠ b`, `resolve(a)` and
       `resolve(b)` never produce the same `StorageLocation.path`. (The property is over parsed ids: a
       canonicalizing resolver must not be penalized for mapping case variants to one file.)
     - **Determinism:** `resolve(id)` called twice yields deep-equal locations.
     - **Traversal impossibility:** for arbitrary string ids, `resolve` either returns a typed `TenantError`
       (`MalformedTenantId`) or a path strictly contained in `tenantsDir` after normalization — it never
       throws, and no produced path escapes the tenants dir.
     - **Registry-state gating (composed):** with a stub registry holding arbitrary per-tenant states, the
       `authorize → resolve → prepare` path yields typed `UnknownTenant`/`TenantDeactivated` errors for
       unknown/deactivated tenants and correct locations for active ones.
     - **Concurrent multi-tenant load (extends the sq-06 approach):** arbitrary tenant sets × concurrent
       put/get op sequences run in parallel; each tenant's reads see exactly its own writes — no cross-talk,
       no lost updates, and every write lands in the right tenant's file.
  4. `numRuns >= 200` per property, following the repo convention. Report coverage assertions separately from
     correctness assertions so a coverage miss is never triaged as an invariant break; baseline expected
     per-seed failure rates (pre-fix): rules ≈6.4%, offsets ≈1%, rotation ≈0.2% (→≈0 after the item-12/13
     rules fix).
- **Acceptance criteria:**
  - `deno task test:prop` passes and includes the new file (it matches the existing `tests/property/**/*.ts`
    glob).
  - Each property's test name prints its seed (`[seed=...]` in `deno test` output).
  - Seed replay: deliberately breaking one invariant (e.g. making `resolve` return a constant path) makes the
    suite fail with a minimal counterexample; `PROP_SEED=<the printed seed> deno task test:prop` reproduces
    the exact same failure.
  - `deno task test:prop` completes in under 60 s on this droplet.

#### MT-10 — End-to-end platform test suite (`deno task e2e`)

- **Scoping:** **SYNC. Single item in batch MT-F.** Runs after `(MT-06+MT-08)` and before main-plan item 27:
  `… → (MT-06+MT-08) → MT-10 → 27 → (28+29)`. Must have landed first: main-plan 19 (backups), 22 (server,
  admin routes, error mapping), 23 (analysis catalog, `scripts/logs.ts`), 25 (Effect-ported integration test,
  `tests/e2e/observability_test.ts`), 26 (wrapper-free client API); MT-01 (Postgres service, `schema.sql`),
  MT-02 (tenancy config rows), MT-05 (admin endpoints), MT-06 (admin CLI), MT-07 (tenant resolution,
  `createTenantClient`) and MT-08 (Better Auth, or its recorded headless-infeasible outcome — the
  auth-scenario gate below needs to know which). **Placement justification:** the suite exercises tenant
  isolation, the admin CLI lifecycle and Better Auth, so its hard dependencies are MT-05 through MT-08 — the
  last MT items in the build order. A main-plan item 30 would make the main plan depend on this plan,
  inverting the recorded direction (this plan builds on the main plan, never the reverse), and extending item
  25 is impossible because 25 runs in the batch right after 22, before any tenancy code exists. MT-10
  therefore appends to this plan with no renumbering, and the interleaved build order that only this plan's §3
  records gains exactly one slot. The slot is before item 27 rather than concurrent with it: 27's
  `docs/testing.md` describes the real-process test tier and must execute every command it quotes, so the
  suite has to exist (and be green) before 27 documents it; and running it as the platform's final acceptance
  gate means a defect it finds is fixed before 27, 28 and 29 describe the platform as finished.
- **Model:** **Opus / astra.** The item owns resource lifetimes across three kinds of process — a Docker
  Compose project, a server per scenario with its reader child, and CLI subprocesses — with teardown that must
  survive failures and signals, and its core assertions are security properties (cross-tenant reads and writes
  behave as missing, revocation and deactivation are enforced). Like item 25, diagnosing a red run means
  reasoning across every layer.
- **Owns:** `package.json` (additive only: a `scripts.e2e` entry — no other key changes, no dependencies),
  `tests/e2e/platform/main.ts`, `tests/e2e/platform/harness.ts`, `tests/e2e/platform/postgres.ts`,
  `tests/e2e/platform/compose.yml`, `tests/e2e/platform/seed.ts`, `tests/e2e/platform/suite_test.ts`, and
  `tests/e2e/platform/scenarios/{documents,queries,backup_restore,tenant_isolation,auth,log_analysis,admin_lifecycle}.ts`.
  Disjoint from item 25's `tests/integration.ts` and `tests/e2e/observability_test.ts`, and from MT-07's
  tenancy tests (including any `tests/e2e/tenancy_test.ts`); the suite may import exported helpers from those
  files but never edits them. Not `deno.json`, not `deno.lock`, not `docker-compose.yml`, not `Makefile`.
- **Reads:** `platform/config.ts` (`OPTIONS`, including the MT-02 rows and the `tenantKeyCacheTtlMs` option),
  `platform/http/router.ts`, `platform/http/auth.ts`, `core/errors.ts` (`toHttpStatus`), `sdk/client.ts`
  (`createClient`, `createTenantClient`), `app/schema.ts`, `app/functions.ts`, `platform/analysis/catalog.ts`,
  `scripts/logs.ts`, `scripts/restore.ts`, `scripts/tenants.ts`, `scripts/auth.ts` and `platform/auth/*` (if
  MT-08 passed its spike), `platform/tenants/schema.sql`, `docker-compose.yml` (the `postgres` image tag),
  `Makefile` (the `db-migrate` recipe), `tests/property/arbitraries.ts` (`SEED`, `titled` — the seed harness),
  `tests/integration.ts`, `tests/e2e/observability_test.ts`, `docs/observability.md`, `deno.lock` (existing
  pins only), and the main-plan items 22, 23, 25 and 28 plus MT-02 through MT-08 in this plan.
- **Work:**
  1. **One command without touching `deno.json`.** Main-plan §2.2 freezes `deno.json`, so the task lives in
     `package.json`, which no item owns and which Deno 2's `deno task` already reads: `deno task` lists
     package.json scripts next to the deno.json table (verified with deno 2.9.5 on this droplet, 2026-10-11).
     Add `"scripts": { "e2e": "deno run -A tests/e2e/platform/main.ts" }` and nothing else, so the root import
     map and `deno.lock` still apply. Arguments after `--` pass through to `deno test` (e.g.
     `deno task e2e -- --filter e2e/4`, `-- --reporter=junit`). Docs that must satisfy item 29's
     deno.json-only task check cite the direct `deno run -A tests/e2e/platform/main.ts` form.
  2. **Dependencies: Postgres only.** The suite needs Postgres for the registry (scenario 1's tenant-key leg
     and scenarios 4, 5 and 7). It does **not** need SeaweedFS: backups go to `LOCAL_OBJECT_DIR` (the same
     layer the integration test uses), and the S3 layer is already covered by
     `tests/integration/object_store_s3_test.ts`. The suite does not call `make up`, because that stack has
     fixed host ports (5432, 8333) and persistent named volumes shared with the developer — not hermetic, and
     `docker compose down -v` against the root compose file could delete `seaweedfs-data`. Instead
     `tests/e2e/platform/compose.yml` declares a single `postgres` service: the same image tag as the root
     `docker-compose.yml` (a check in `postgres.ts` fails the run on drift), data on `tmpfs` (no volumes at
     all), the host port published as `127.0.0.1::5432` (ephemeral, read back with `docker compose port`), and
     the same `pg_isready` healthcheck. `postgres.ts` runs it under a unique project name
     (`mansio-e2e-<pid>-<seed>`) with `docker compose up -d --wait`, creates one database per scenario, runs
     the migration runner (`scripts/db/migrate.ts`, MT-01b) against each with `DATABASE_URL` pointed at the
     scenario database (the `make db-migrate` recipe, re-pointed — no Makefile edit), applies MT-08's Better
     Auth migration when the auth gate is open, and tears down with
     `docker compose down --remove-orphans --volumes`. Docker absent or unhealthy within 60 s → the run fails
     with a clear message; it never skips to green.
  3. **Harness (`harness.ts`), one server per scenario.** Each scenario gets its own subdirectory of one run
     temp root (`Deno.makeTempDir({ prefix: "mansio-e2e-" })`) and its own server on a free `127.0.0.1` port,
     spawned with the command the `serve` task runs. Spawn with `clearEnv: true` and an explicit env: `PATH`,
     `HOME` and `DENO_DIR` from the parent, plus only option names from the `OPTIONS` table — `PORT`,
     `BIND_HOST`, `DATA_DIR`, `LOG_DIR`, `LOCAL_OBJECT_DIR`, `EVENTLOG_DUCKDB`, `S3_BASE_URL=""`,
     `BACKUP_INTERVAL_MS=0`, per-run random admin and app tokens, and the MT-02 rows (`TENANCY`, database URL,
     tenants dir, Better Auth secret) as each scenario needs. A developer's `AWS_*` or `S3_BASE_URL` can
     therefore never leak in. Poll `GET /health` with a bounded timeout, and fail fast if the child exits.
     Every resource is acquired with `Effect.acquireRelease`: SIGTERM, await the server and its reader child,
     remove the scenario dir. CLI subprocesses (`scripts/tenants.ts`, `scripts/logs.ts`, `scripts/restore.ts`,
     `scripts/auth.ts`) get the same clean env plus the scenario server's URL in whatever form MT-06 accepts.
     If MT-06's CLI cannot target a non-default port, stop and report rather than editing it.
  4. **Seed and ordering (`seed.ts`).** Reuse the repo's single seed mechanism: `SEED` from
     `tests/property/arbitraries.ts` (`PROP_SEED` env, default `DEFAULT_SEED`) and `titled()` for every test
     name. Do not add a second seed variable. The seed drives the scenario execution order (a seeded
     permutation, applied by `suite_test.ts` dynamically importing the scenario modules in that order) and all
     generated data: player names, tenant names and slugs, move sequences, and which games are left
     unfinished, via `fc.sample(arb, { seed })`. `main.ts` prints `order=<scenario list> seed=<n>` before the
     run, and on failure it prints `Replay: PROP_SEED=<n> deno task e2e`. Every test is named
     `e2e/<n>-<slug>: <case> [seed=<n>]` so `--filter e2e/<n>` runs a single scenario.
  5. **Scenarios.** Each asserts outcomes rather than printing them, and none depends on another's state.
     - **1 documents:** through the typed Effect client, on both `tic-tac-toe` (columns) and
       `tic-tac-toe-json` (json), first in `TENANCY=off` with the app token, then in `TENANCY=api-key` with a
       tenant key via `createTenantClient`. Cover `createGame`, then `getGame` (`_version` 1, numeric
       `_creationTime`), seeded `makeMove` updates with read-after-write equality, `deleteGame` then `getGame`
       → `null`, a stale `expectedVersion` → typed `ConflictError` (409), and a bad argument → typed
       `SchemaError` (400).
     - **2 queries:** for a seeded set of finished and unfinished games, `listGames` per status, `moveHistory`
       length, and `winsByMark` (`countBy`) equal a reference computed in the test from the moves it sent.
       Both layouts return identical results, ignoring `_id` and `_creationTime`.
     - **3 backup/restore** (`TENANCY=off`; per-tenant backup is deferred, see §4): play a game, then call
       `POST /admin/backup/<app>` for both apps. Each `latest.json` manifest's object has the manifest's
       SHA-256. Make one more move after the backup, stop the server, and run `scripts/restore.ts` into
       `<scenario>/restored/`; the restored file's checksum equals the manifest's. Start a second server on
       the restored `DATA_DIR`: the game has the backed-up `_version`, not the post-backup one, and queries
       through the DuckDB reader succeed on the restored data.
     - **4 tenant isolation** (`TENANCY=api-key`, two tenants created with the admin CLI): tenant A creates a
       game. Tenant B, addressing that same document id, gets `getGame` → `null`, `makeMove` → 404, and
       `deleteGame` → 404. Each matches the status and error tag returned for a never-existing id, and A's
       document is unchanged at its version afterwards. Each tenant's `listGames` and `winsByMark` count only
       its own games. A request with B's key and a forged `x-tenant-id: <A's id>` header is still scoped to B.
       The literal case of two tenant files holding the same `_id` stays covered at the storage layer by MT-04
       (no HTTP route lets a client choose `_id`).
     - **5 auth:** **(a) API keys, always run:** a missing `Authorization` header, a malformed bearer, an
       unknown `mansio_…` key and a wrong admin token are each 401 on `/apps/*` and `/admin/*`; in
       `TENANCY=off`, one app's token is 401 on the other app. **(b) Better Auth, gated:** the gate is open
       iff MT-08's step-2 deliverables exist (`scripts/auth.ts` and `platform/auth/`). Open → run headless
       `signUp.email`/`signIn.email` through `better-auth/client` at the version MT-08 pinned (no new
       specifier), link the user to tenant A with `scripts/auth.ts`, then (presenting the session token per
       MT-08's bearer-or-cookie transport decision): the session token reads A's documents; it reads B's
       documents as missing; a wrong password is 401; an unknown user is 401. Any failure while the gate is
       open fails the run. Closed → register these cases with `ignore: true` and a name carrying
       `[skipped: MT-08 headless-infeasible]`, so `deno test` reports them as ignored, never passed.
     - **6 log ingest → DuckDB analysis:** run a seeded workload with a caller-supplied `X-Trace-Id` on one
       request, one deliberate 409, one unknown-handler 404 and one backup. Call `POST /admin/logs/ingest`,
       retrying for at most 10 s until the trace id is queryable (the sink flushes asynchronously). Then
       through `POST /admin/logs/analysis/:name`: `handler-throughput` lists the handlers the workload called;
       `error-rate-by-handler` shows the 409 and the 404 with their error tags; `slowest-sql` and
       `tx-outcomes` return rows; `backup-history` shows the backup; and `trace` for the supplied id returns
       an ordered timeline containing both `server` and `reader` roles.
       `scripts/logs.ts query <name> --format
       json`, run against the same `LOG_DIR`/`EVENTLOG_DUCKDB`,
       returns the same rows as the endpoint for at least `handler-throughput`. An unknown query name is
       rejected with `UnknownAnalysisQuery`. No k6 — the k6 join stays item 25's.
     - **7 admin CLI tenant lifecycle:** `scripts/tenants.ts create` prints the key exactly once (with the
       "save this" warning), and the key works on `/apps/*`. `issue-key` adds a second working key. After
       `revoke-key` on the first, it is 401 while the second still works. After `deactivate`, both are 401,
       and `show` reports the tenant deactivated. Neither `list` nor `show` ever prints a plaintext key. The
       CLI with a wrong `ADMIN_TOKEN` exits non-zero. If MT-07's tenant-key cache delays revocation, each "→
       401" step polls up to the `tenantKeyCacheTtlMs` option (MT-02) plus 1 s and fails past that bound; the
       bound comes from config, not a hardcoded sleep.
  6. **Runner (`main.ts`) and hermeticity self-check.** Record `git status --porcelain` and the set of
     `mansio-e2e-*` containers, start Postgres, and run `deno test -A tests/e2e/platform/suite_test.ts` (plus
     passthrough args) with the connection details in env. Tear down on exit, on failure, and on
     SIGINT/SIGTERM. Then re-check: no container, network or volume of this run's project; no live child pid
     this run spawned; the temp root gone; `git status --porcelain` unchanged; no `./data`, `./logs` or
     `./object-store` created. Any violation fails the run even when every test passed. `E2E_KEEP=1` skips
     teardown for debugging and prints what it left. The last line is
     `E2E PASS seed=<n> scenarios=7 skipped=<none|5b(better-auth: MT-08 headless-infeasible)>` or `E2E FAIL …`
     with the replay line. CI wiring: the repo has no CI configuration, and `deno task ci` deliberately
     excludes the suite (it needs Docker). The suite is CI-ready as is — one command, no TTY or prompts,
     non-zero exit on failure, junit via passthrough, bounded runtime — and adding a CI workflow is out of
     scope.
  7. **A red scenario that exposes a platform defect is reported, not worked around.** Stop with the replay
     seed and the failing assertion. The coordinator opens a fix against the owning item. Never weaken an
     assertion or mark a case ignored to get green (the only ignore is the MT-08 gate in scenario 5b).
- **Acceptance criteria:**
  - `deno task` lists `e2e (package.json)`. From a state with no mansio containers running (`make down`),
    `deno task e2e` exits 0 and its last line is `E2E PASS seed=20261010 scenarios=7 skipped=…`.
    `deno run -A tests/e2e/platform/main.ts` behaves identically.
  - With a fresh developer stack up (`make up && make db-migrate`, holding host ports 5432 and 8333),
    `deno task e2e` again exits 0, and the dev stack is untouched: `docker compose ps`, `docker volume ls` and
    the dev Postgres's `psql -l` are identical before and after.
  - Coverage: the `deno test` output has `ok` lines for every one of `e2e/1-documents`, `e2e/2-queries`,
    `e2e/3-backup-restore`, `e2e/4-tenant-isolation`, `e2e/5-auth` (5a), `e2e/6-log-analysis` and
    `e2e/7-admin-lifecycle`. The 5b cases are `ok` if MT-08 passed its spike and `ignored` with the
    `[skipped: MT-08 headless-infeasible]` reason if it did not — the reviewer confirms the state matches
    MT-08's recorded outcome — and the final line's `skipped=` field agrees.
  - The suite asserts. The reviewer makes one local break per check and confirms `E2E FAIL` each time: MT-07's
    middleware honoring `x-tenant-id` → scenario 4 fails; `resolveTenantByKeyHash` ignoring `revoked_at` →
    scenario 7 fails; `EventSinkLive` swapped for a no-op sink → scenario 6 fails; the restored-`_version`
    expectation flipped → scenario 3 fails.
  - Hermetic, after a passing run, after a failing run (one injected failed assertion), and after an
    interrupted run (`timeout -s INT 30 deno task e2e`): `docker ps -a -q --filter name=mansio-e2e` and
    `docker volume ls -q --filter name=mansio-e2e` print nothing; `ls -d /tmp/mansio-e2e-*` matches nothing;
    `pgrep -f "platform/(server|reader).ts"` prints nothing (no other mansio server running);
    `git status --porcelain` is unchanged; no `./data`, `./logs` or `./object-store` exists that did not exist
    before.
  - Two concurrent runs (`deno task e2e & PROP_SEED=7 deno task e2e & wait`) both exit 0 — the run projects,
    ports and directories are independent.
  - Seed replay: every test name carries `[seed=<n>]`. Two runs with the same `PROP_SEED` print the same
    `order=…` line and the same generated tenant slugs and player names; a different `PROP_SEED` changes the
    order. A failing run prints `Replay: PROP_SEED=<n> deno task e2e`, and running that command reproduces the
    same failure in the same scenario order.
  - Docker stopped (or `docker` off `PATH`): `deno task e2e` exits non-zero with a message naming the missing
    dependency and never prints `E2E PASS`.
  - `deno task e2e` completes in under 240 s on this droplet, excluding the first `postgres` image pull.
  - `deno check tests/e2e/platform/*.ts tests/e2e/platform/scenarios/*.ts`, `deno task fmt:check`,
    `deno task lint` and `deno task ci` pass.
  - `git diff --name-only <base>..HEAD` lists only `package.json` and paths under `tests/e2e/platform/`;
    `git diff <base>..HEAD -- deno.json deno.lock` is empty; and the `package.json` diff adds only
    `scripts.e2e`.

## 4. Out of scope / deferred

- Postgres HA, PITR.
- Per-tenant DuckDB/event-log separation (tenants share the central event log in this phase; tenant id is a
  column).
- Billing, plans, quotas.
- OAuth/social providers (Better Auth supports them; not in this phase).
- If MT-08's spike fails: full user auth defers to a later phase; API keys remain the identity story.
- Per-tenant backup/restore: item 19 covers the single-file path only. No MT item backs up or restores
  per-tenant SQLite files — **deferred** to a follow-up item (per-tenant `snapshot()` → object store, restore
  by tenant id, retention per tenant). Until that item lands, `POST /admin/backup/<app>` in `api-key` /
  `better-auth` mode must refuse (or document legacy-file-only) — it must never report success while tenant
  data is excluded.
