// Package tenancy is mansio's tenants, API keys, users and sessions on the Go backend (ADR 0012): who a bearer
// credential is, and the admin and sign-in operations behind /admin/tenants and /api/auth. Domain code: no I/O and no
// logging. It reaches Postgres through the Store port (internal/placement/pg implements it); tenancyhttp is its HTTP
// adapter.
//
// # The credential decides the tenant
//
// Authenticate maps a bearer credential and the app it is used for to a Principal: off mode, the app's own
// token (tenant OffTenant); api-key mode, a tenant API key; session mode, an API key or a session token whose
// user has exactly one membership. Nothing the caller says about its tenant (a header, the IR body's
// db.tenant) is consulted: the database is Principal.Tenant ':' app.
//
// # Keys and sessions at rest
//
// API keys come in two formats, both accepted: "mansio_<16 hex id>_<secret>" (new keys; locator.NewAPIKey;
// SHA-256 of the secret stored under the id) and the Deno platform's "mansio_<43 base64url>" (legacy; SHA-256
// of the whole key stored, found by that hash). Session tokens are stored as SHA-256 too. Every comparison of a
// secret is constant-time. Passwords use Better Auth's scrypt (password.go), so migrated accounts sign in
// unchanged.
//
// # Caching
//
// Authenticate caches its answers (allowed and refused) by SHA-256 of the credential, for CacheTTL. Local
// changes (revoke, deactivate, issue, link) clear the cache at once; another process sees them within one TTL.
// A store that cannot answer (ErrUnavailable) is never cached.
//
// Errors are module "tenancy". Sentinels: ErrUnauthenticated (refuse 401, with a Reason), ErrNotFound,
// ErrConflict, ErrDeactivated, ErrInvalid (bad input), ErrUnavailable (the store is down: 503, retry).
package tenancy
