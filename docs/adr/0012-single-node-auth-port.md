# ADR 0012 — Single node: agentd checks credentials itself; multi-node: controld at resolve

- **Status:** accepted
- **Date:** 2026-11-06
- **Deciders:** `docs/plans/completed/replace-platform-with-go.md` (removed; see git history) §3 D5, D6, §4.4;
  recorded by IR-4a (`docs/plans/completed/intermediate-representation-protocol.md` (removed; see git history)
  §5 IR-4)
- **Related:** [ADR 0009](0009-intermediate-representation-is-the-app-wire.md),
  [ADR 0011](0011-database-identity-and-client-ids.md)
- **Implements:** `server/internal/tenancy`, `server/internal/tenancy/tenancyhttp`, `server/internal/front`
  (`Credentials`), `server/cmd/agentd/tenancy.go`, `server/internal/placement/pg` (schema 0007, `Tenancy()`,
  `ImportMansio`), `controld import-mansio`

## Decision

1. **Single node (now): agentd verifies credentials itself** through an auth port (`front.Credentials`), on
   `POST /v1/ops` beside tickets. A bearer that is ticket-shaped is a ticket (and fails as `ticket_invalid`);
   any other bearer goes to the port. Modes, as the Deno platform's `TENANCY`: `off` (each app's own token,
   tenant `default`), `api-key` (tenant API keys), `session` (API keys and email/password sessions; the user
   needs exactly one membership). A refused credential is `AuthError` 401 with `fields.reason`
   (`missing`/`malformed`/`mismatch`); a store outage is `unavailable` 503.
2. **The tenant always comes from the credential.** The database is `<credential tenant>:<db.app>`; the body's
   `db.tenant` and any `x-tenant-id` header are ignored, never compared. The SDK keeps sending whatever tenant
   it was configured with.
3. **Multi-node (IR-5): controld authenticates once per resolve and issues the ticket**; agents verify tickets
   only. The same `internal/tenancy` service (and `tenancyhttp` routes) is what controld mounts then; its
   locator will accept legacy keys and sessions through it.
4. **One store, the control plane's Postgres schema** (`placement/pg` migration `0007_tenancy.sql`): tenants
   gain slug/name/status; `api_keys` keep both formats (new `mansio_<16 hex>_<secret>`, SHA-256 of the secret
   under the id; legacy `mansio_<43 base64url>`, SHA-256 of the whole key, found by hash, `legacy = true`);
   Better Auth's user/account/session/memberships become `auth_users`, `auth_passwords`, `auth_sessions`
   (SHA-256 of the token, never the token) and `memberships`. New keys are issued in the new format.
5. **Passwords are Better Auth's scrypt**, byte-compatible (N=16384, r=16, p=1, 64-byte key over the NFKC
   password, the salt's hex text as the salt; verified against two real Better Auth 1.7.7 hashes in
   `password_test.go`), so migrated accounts sign in unchanged; no forced reset.
6. **The admin API keeps the Deno shapes** (`/admin/tenants…` as `scripts/tenants.ts` calls it; plus
   `POST /admin/users/link` for `scripts/auth.ts users link`), and `/api/auth/sign-up/email`,
   `/api/auth/sign-in/email` answer Better Auth's `{token, user}` / `{code, message}`. On a single node they
   are served by agentd's query listener; every `/admin/` path checks the admin token before routing.
7. **Migration** is `controld import-mansio [-dry-run]`: reads the Deno tables in one read-only snapshot,
   writes every table in one statement in one transaction, adds what is new and carries one-way changes on a
   re-run (below), moves only unexpired sessions and only `credential` accounts.

## Why

- The plan (§4.4) already set this split: on one node there is no controld to resolve through, and making
  every request mint a ticket from a local issuer would only move the same check elsewhere.
- One `tenancy` package for both placements keeps one meaning of "who is this credential" (D5, D6) and lets
  IR-5 mount it in controld without a second implementation.
- Keeping the legacy key hash and the Better Auth hash format means the cut-over (IR-6) needs no key rotation
  and no password reset.

## Consequences

- **Accepted risks (Better Auth parity):** sign-up answers `USER_ALREADY_EXISTS` (422) for a registered email,
  so it reveals which emails have accounts; sign-in and sign-up are rate-limited per source (client IP, IPv6
  /64), not per account, so a distributed guesser is slowed only by scrypt and the 4 hash slots.
- **What a credential can create:** only apps in `tenancy.apps` (default: the apps in `schemas_dir`) and only
  for a registered schema, checked before anything is admitted; `create_dbs` needs `schemas_dir` and is capped
  by `max_dbs` (10 000) and `max_dbs_per_tenant` (100), counting databases found at start. Past a cap:
  `QuotaExceeded` 429 (`fields.scope`, `fields.limit`), nothing created.
- **Readiness:** the agent is not ready until the tenancy schema has migrated once, so a Postgres that is down
  at start keeps it not ready (ticket traffic is still served meanwhile). A Postgres outage after that never
  fails readiness (the `tenancy` breaker and metrics show it); credential requests answer `unavailable` 503.
- **Body before credential:** agentd reads and decodes the body (at most 64 KiB) before checking a credential,
  because the app is in the body; the Deno shim checks first (its app is in the path). Kept: the body is
  bounded and decoding does no I/O.
- **Import re-runs** carry only one-way changes: deactivations (also revoking the tenant's new-format keys
  here), key revocations and changed password hashes. They never reactivate, unrevoke or delete: a membership,
  session or user removed at the source stays here until removed by hand.

- Revocation is seen at once by the process that made it (local changes clear the credential cache) and within
  `cache_ttl` (1 s) by any other process sharing the store.
- Tenancy was static-mode only at first (`create_dbs` creates `<tenant>:<app>.db` on first use at
  `default_epoch`). IR-5a: durable single-node mode (`placement_dir`) runs a credential at its owner-table
  epoch and the claim path creates databases; with `placement_pg`, agents only verify tickets.
- **Multi-node (IR-5a):** controld authenticates at resolve with the same `internal/tenancy` service (new and
  legacy keys, sessions) and issues the ticket; the tenant always comes from the credential. A legacy key or a
  session reaches, by an app resolve, any database `tenant:<name>` of its own tenant, including raw databases
  an operator made with `POST /v1/databases` under a name that is not an app (ADR 0011: apps and raw names
  share one namespace per tenant). That is the tenant's own data, never another tenant's.
- Not ported: Better Auth's other endpoints (sign-out, session refresh/rolling expiry, email verification,
  OAuth), `PATCH /admin/tenants/:id`. Expired sessions are reaped per user at that user's next sign-in.
- `scripts/auth.ts users link` still writes Postgres directly on Deno; on Go the e2e uses
  `POST /admin/users/link`. A CLI over that endpoint replaces it at IR-6.
