# ADR 0012 — Single node: agentd checks credentials itself; multi-node: controld at resolve

- **Status:** accepted
- **Date:** 2026-11-06
- **Deciders:** [`docs/plans/replace-platform-with-go.md`](../plans/replace-platform-with-go.md) §3 D5, D6,
  §4.4; recorded by IR-4a ([`docs/plans/ir-protocol.md`](../plans/ir-protocol.md) §5 IR-4)
- **Related:** [ADR 0009](0009-ir-is-the-app-wire.md), [ADR 0011](0011-database-identity-and-client-ids.md)
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
7. **Migration** is `controld import-mansio [-dry-run]`: reads the Deno tables (read-only session), writes
   every table in one statement in one transaction, skips rows already present (re-runs are safe), moves only
   unexpired sessions and only `credential` accounts.

## Why

- The plan (§4.4) already set this split: on one node there is no controld to resolve through, and making
  every request mint a ticket from a local issuer would only move the same check elsewhere.
- One `tenancy` package for both placements keeps one meaning of "who is this credential" (D5, D6) and lets
  IR-5 mount it in controld without a second implementation.
- Keeping the legacy key hash and the Better Auth hash format means the cut-over (IR-6) needs no key rotation
  and no password reset.

## Consequences

- Revocation is seen at once by the process that made it (local changes clear the credential cache) and within
  `cache_ttl` (1 s) by any other process sharing the store.
- Tenancy is static-mode only for now (`create_dbs` creates `<tenant>:<app>.db` on first use at
  `default_epoch`); durable single-node mode would need the claim path to create databases (IR-5).
- Not ported: Better Auth's other endpoints (sign-out, session refresh/rolling expiry, email verification,
  OAuth), `PATCH /admin/tenants/:id`. Expired sessions are reaped per user at that user's next sign-in.
- `scripts/auth.ts users link` still writes Postgres directly on Deno; on Go the e2e uses
  `POST /admin/users/link`. A CLI over that endpoint replaces it at IR-6.
