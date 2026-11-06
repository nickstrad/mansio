/**
 * Tenant domain types and id parsing (MT-03).
 *
 * Pure: no I/O, no Postgres. `store.ts` implements `TenantRegistryService` against these
 * types; the storage router (MT-04) and the request path (MT-07) reuse `parseTenantId`, so
 * the strict UUID discipline is defined once, here.
 */
import { Brand, Effect } from "effect";
import { MalformedTenantId } from "../../core/errors.ts";
import type { ConflictError, StorageError, TenantError } from "../../core/errors.ts";

/**
 * A tenant id: a strictly-parsed canonical-lowercase UUID. The brand is the guarantee —
 * only `parseTenantId` (or a freshly generated UUID) can produce one, so anything holding
 * a `TenantId` has already passed the strict parse. In particular a `TenantId` can never be
 * a path traversal (`../x`), an uppercase variant, a `urn:uuid:` form, or carry NUL,
 * whitespace, or percent-encoding: all of those fail the parse with `MalformedTenantId`.
 */
export type TenantId = Brand.Branded<string, "TenantId">;

/** An API key id: the same strict UUID discipline as `TenantId`, a distinct brand. */
export type ApiKeyId = Brand.Branded<string, "ApiKeyId">;

/**
 * Canonical lowercase UUID: 8-4-4-4-12 lowercase hex. Rejects uppercase, `urn:uuid:`
 * forms, NUL, whitespace, percent-encoded forms, and separator-less forms.
 */
const CANONICAL_UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * Strictly parse a tenant id. Anything that is not a canonical-lowercase UUID fails with
 * `MalformedTenantId` — never a path, never a case-folded id.
 */
export const parseTenantId = (input: string): Effect.Effect<TenantId, MalformedTenantId> =>
  CANONICAL_UUID.test(input)
    ? Effect.succeed(input as TenantId)
    : Effect.fail(new MalformedTenantId({ input }));

/** Strictly parse an API key id (same discipline as `parseTenantId`). */
export const parseApiKeyId = (input: string): Effect.Effect<ApiKeyId, MalformedTenantId> =>
  CANONICAL_UUID.test(input)
    ? Effect.succeed(input as ApiKeyId)
    : Effect.fail(new MalformedTenantId({ input }));

/** Lifecycle states a tenant can be in. The `tenants.status` column only ever holds these. */
export type TenantStatus = "active" | "deactivated";

/** One row of the `tenants` table. */
export interface Tenant {
  readonly id: TenantId;
  readonly slug: string;
  readonly name: string;
  readonly status: TenantStatus;
  readonly createdAt: Date;
}

/** Patch for `updateTenant`: name and/or status. The slug is immutable. */
export interface TenantPatch {
  readonly name?: string;
  readonly status?: TenantStatus;
}

/**
 * A freshly-issued API key. `plaintext` is returned exactly once, at issuance — it is never
 * persisted (only the SHA-256 hash is stored) and no interface method can read it back.
 */
export interface IssuedApiKey {
  readonly id: ApiKeyId;
  readonly tenantId: TenantId;
  readonly name: string;
  /** Key scheme prefix. Constant today; stored in `api_keys.prefix` for support identification. */
  readonly prefix: "mansio_";
  readonly plaintext: string;
  readonly createdAt: Date;
}

/**
 * The tenant registry contract (MT-03), in the `platform/services.ts` style. Ids are
 * branded: callers parse raw strings with `parseTenantId`/`parseApiKeyId` first (the admin
 * endpoints do this at the HTTP boundary), so the store never sees an unparsed id.
 *
 * Error channels: `TenantError` for domain failures (malformed id at the parse boundary,
 * unknown tenant, deactivated tenant), `ConflictError` for a duplicate slug on create
 * (→ HTTP 409), `StorageError` for Postgres/driver failures (→ HTTP 500).
 */
export interface TenantRegistryService {
  /**
   * Insert a tenant. A duplicate slug fails `ConflictError` (`expected: 0, actual: 1`,
   * `table: "tenants"`, `id: <slug>`) — the caller assumed the slug was free and the
   * stored state says otherwise.
   */
  createTenant(input: {
    readonly name: string;
    readonly slug: string;
  }): Effect.Effect<Tenant, TenantError | ConflictError | StorageError>;
  /** Fetch a tenant by id. Missing → `UnknownTenant`. */
  getTenant(id: TenantId): Effect.Effect<Tenant, TenantError | StorageError>;
  /** All tenants, oldest first. */
  listTenants(): Effect.Effect<readonly Tenant[], StorageError>;
  /**
   * Patch name and/or status. An empty patch is a no-op returning the tenant. Setting
   * status to `deactivated` revokes the tenant's keys (same as `deactivateTenant`);
   * reactivating flips the status back but does NOT resurrect revoked keys — issue new ones.
   */
  updateTenant(id: TenantId, patch: TenantPatch): Effect.Effect<Tenant, TenantError | StorageError>;
  /**
   * Deactivate a tenant and revoke all its API keys, atomically. Revocation is real
   * (`revoked_at` is set): a reactivated tenant's old keys stay dead (MT-05/MT-06
   * reactivation semantics).
   */
  deactivateTenant(id: TenantId): Effect.Effect<Tenant, TenantError | StorageError>;
  /**
   * Issue an API key: `mansio_` + 32 random bytes (base64url). The SHA-256 hash of the full
   * plaintext is stored; the plaintext is returned exactly once in `IssuedApiKey`. Atomic:
   * the tenant row is locked and re-checked inside the insert transaction, so a key can
   * never be issued for a tenant deactivated concurrently with issuance. Unknown tenant →
   * `UnknownTenant`, inactive tenant → `TenantDeactivated`.
   */
  issueApiKey(
    tenantId: TenantId,
    name: string,
  ): Effect.Effect<IssuedApiKey, TenantError | StorageError>;
  /** Revoke a key, optionally restricted to its owning tenant. Idempotent: unknown or already-revoked ids succeed silently. */
  revokeApiKey(keyId: ApiKeyId, tenantId?: TenantId): Effect.Effect<void, StorageError>;
  /**
   * Resolve a tenant from the SHA-256 hex of a presented API key. Joins `api_keys` →
   * `tenants`; revoked keys and inactive tenants are rejected. Deliberately one error
   * (`UnknownTenant`) for unknown key, revoked key, and inactive tenant — callers must not
   * distinguish them (no key-guessing oracle).
   */
  resolveTenantByKeyHash(hash: string): Effect.Effect<Tenant, TenantError | StorageError>;
}
