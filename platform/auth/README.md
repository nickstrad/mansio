# Headless email/password authentication (MT-08)

**Scope integration pending:** the brief excludes the normal migration directory, `deno.lock`, and
`docs/knowledge/decisions.md` from ownership while requiring changes there. The tested migration pair
currently lives here as `002_auth.up.sql` / `002_auth.down.sql`; the standard runner does not discover it
here. `deno.lock.patch` and `decision.md` are prepared for the explicitly requested destinations. These
artifacts must be installed before this item can be marked complete.

2026-10-11: **GO** on Deno 2.9.5, Better Auth 1.7.7 and Postgres 17. The executable [spike](spike.ts) uses the
vanilla `createAuthClient`, signs up, signs in, captures the body token and calls a tenant endpoint. It
exits 0. Without `bearer()` that body token returns 401; with the plugin it authenticates. Wrong passwords and
unknown users return 401 (or 429 after the rate limit).

## Configuration and use

Set `TENANCY=better-auth`, `DATABASE_URL`, `BETTER_AUTH_URL` (the canonical externally reachable origin) and a
random `BETTER_AUTH_SECRET` of at least 32 characters. Use HTTPS outside local development. Apply the numbered
migrations before starting. The router mounts `/api/auth/*` only in this mode. API-key mode and tenancy-off
keep their previous behavior.

Use `better-auth/client` to call `signUp.email` and `signIn.email`, then pass `data.token` as
`Authorization: Bearer <token>` to normal `/apps/:appId/...` endpoints. Public sign-up creates an unlinked
account: only an operator can grant membership. No email verification is required by this item; possession of
an email string alone never grants a tenant. Password resets and email delivery are not configured.

```sh
# Supply the password as one line on stdin, from your secret store.
deno run -A scripts/auth.ts users create person@example.test --tenant <tenant-uuid>
deno run -A scripts/auth.ts users link person@example.test <tenant-uuid>
```

The CLI rejects interactive stdin to prevent echoing passwords. Passwords are neither command-line arguments
nor output. Creation and optional linking share a transaction; a failed link leaves no partial account.
Linking is idempotent and does not promote existing roles. Roles are membership metadata for this item; they
do not grant platform admin privileges.

A session must resolve to **exactly one membership**, and that tenant must currently be active. Zero or
multiple memberships return 401. No header/body tenant selector can choose among memberships. Removing
membership, expiring/deleting a session, signing out, or deactivating a tenant prevents the next
authentication; already admitted requests can finish. Session cookie caching is disabled. Only the bearer
header is forwarded for tenant authentication, so an ambient cookie cannot rescue an invalid token. `mansio_`
credentials continue through MT-07's API-key path and its existing cache policy.

The built-in process-local rate limiter is enabled. This router has no trusted socket address, so forwarded-IP
headers are ignored and requests share a per-path bucket (three sign-ins per ten seconds). This is
conservative and can limit unrelated users. Production ingress needs its own trusted per-client and
distributed rate limits; this item does not claim a distributed limiter.

## Driver and resource decision

The pinned Better Auth built-in PostgreSQL adapter recognizes `pg.Pool` (`connect`), not the callable
postgres.js client. A raw postgres.js client is interpreted as a custom adapter factory, so passing it
directly is not supported. The
[upstream PostgreSQL adapter documentation](https://www.better-auth.com/docs/adapters/postgresql) describes
the built-in Kysely path.

Runtime uses Better Auth's **minimal entrypoint** and a [custom adapter](adapter.ts) backed directly by pinned
`npm:postgres@3.4.9`. All SQL is handwritten; identifiers are allowlisted against the four pinned auth tables,
and all values are bound. Better Auth handles password hashing and session semantics. Unsupported adapter
models/operators fail closed. This is deliberately limited to this email/password configuration; adding
plugins or upgrading requires adapter/schema regression tests. No ORM or query-builder runs in the product
auth path.

`pg@8.23.1` and Better Auth's built-in migration tooling are used only by the independent spike to capture the
upstream schema in [schema.generated.sql](schema.generated.sql). Production never runs Better Auth migrations
on startup. The numbered migration must contain this captured SQL plus memberships; down must drop memberships
before the referenced auth tables.

Each HTTP/CLI operation owns its postgres.js client (`max: 1`) and closes it in a finalizer, including on
failure. There is no detached/global database pool. This favors explicit lifetime safety within MT-08's
ownership boundary; connection pooling across requests is a future scoped-layer change. Database errors become
generic 500 responses without driver payloads or tokens. Auth HTTP events record status, timing and byte
counts, with a constant route label and no credential content.

## Verification

```sh
deno run -A platform/auth/spike.ts
deno test -A platform/auth/integration_test.ts
deno task ci
```

Both auth programs create a unique scratch schema in the configured database, then drop only their own schema
in `finally`. They do not alter application tables. The integration test exercises the real router mount and
middleware against Postgres, including membership ambiguity, tenant deactivation, API-key coexistence, session
expiry/sign-out, invalid credentials and adapter transaction rollback.

Lockfile capture for this build: the proposed `deno.lock` delta adds exact root specifiers
`npm:better-auth@1.7.7` and `npm:pg@8.23.1`, and 36 npm resolution entries. It changes no existing resolution.
Better Auth declares its built-in adapters and Kysely as package dependencies; those entries appear in the
lock even though product code uses the minimal/custom-adapter entrypoints.

Migration capture validation used the existing runner against a private schema: apply creates journal versions
`001` and `002`; replay is a no-op; empty down succeeds; reapply succeeds; nonempty down fails and retains
version `002`. The integration test stages the candidate SQL in a temporary migration directory and changes
only the copied runner's import/directory locations.
