/**
 * Postgres-backed `TenantRegistry` (MT-03).
 *
 * Driver pin: `npm:postgres@3.4.9` (porsager/postgres), not `npm:pg`. sq-10 measured both on the
 * registry's own workload (PK SELECT, api_keys→tenants join by key_hash, UPDATE/DELETE by PK, 100-row
 * batch INSERT, BEGIN/COMMIT) and latency was within run-to-run noise for prepared statements (PK SELECT
 * median 0.46–1.17 ms for both drivers across four runs; see `docs/haiku-experiments/findings/sq-10.md`).
 * The choice therefore rests on the transaction and error surface: `sql.begin` rolled back a thrown error
 * with the row absent (verified), the equivalent `npm:pg` path needed a hand-written BEGIN/ROLLBACK/release
 * wrapper, and server errors arrive as a named `PostgresError` with the SQLSTATE in `code` (23505 on
 * duplicate slug, verified). Pinned exactly; a version bump must re-run the bake-off. Note: on unprepared
 * statements pg was consistently faster (4/4 runs: postgres.js +30–64% on PK SELECT, +32–116% on 100-row
 * INSERT); postgres.js prepares by default, so the recommendation stands on the default path — re-measure
 * if `prepare: false` is ever used.
 *
 * Deno note: under Deno's npm compat the package exposes no named ESM export for the error class —
 * `PostgresError` hangs off the default export (`postgres.PostgresError`, verified 2026-10-11) — so
 * error matching goes through that reference, not a named import.
 *
 * **No ORM, ever** (plan §2.4): hand-written SQL through the client's tagged template, which
 * parameterizes every interpolated value — no string concatenation, no query builder.
 */
import postgres from "npm:postgres@3.4.9";
import type { Sql } from "npm:postgres@3.4.9";
import { Context, Effect, Layer, Scope } from "effect";
import { Config, TenantRegistry } from "../services.ts";
import { ConflictError, StorageError, TenantDeactivated, UnknownTenant } from "../../core/errors.ts";
import type { TenantError } from "../../core/errors.ts";
import type {
  ApiKeyId,
  IssuedApiKey,
  Tenant,
  TenantId,
  TenantPatch,
  TenantRegistryService,
} from "./registry.ts";

/** The driver's error class, via the default export (see the Deno note above). */
const PostgresError = postgres.PostgresError;

/** A `tenants` row as postgres.js returns it (`timestamptz` → `Date`, `uuid` → `string`). */
interface TenantRow {
  readonly id: string;
  readonly slug: string;
  readonly name: string;
  readonly status: string;
  readonly created_at: Date;
}

/** An `api_keys` insert `RETURNING` row. */
interface ApiKeyRow {
  readonly id: string;
  readonly tenant_id: string;
  readonly name: string;
  readonly created_at: Date;
}

const isTenantStatus = (status: string): status is Tenant["status"] =>
  status === "active" || status === "deactivated";

/**
 * Decode a `tenants` row. This store is the sole writer of the status column, so anything
 * outside the two known values is store corruption → `StorageError`, never a silent misread.
 */
const decodeTenant = (row: TenantRow): Effect.Effect<Tenant, StorageError> =>
  isTenantStatus(row.status)
    ? Effect.succeed({
      id: row.id as TenantId,
      slug: row.slug,
      name: row.name,
      status: row.status,
      createdAt: row.created_at,
    })
    : Effect.fail(
      new StorageError({
        operation: "select",
        table: "tenants",
        detail: `unexpected status ${JSON.stringify(row.status)} for tenant ${row.id}`,
      }),
    );

/** SHA-256 hex of the full key plaintext — what `api_keys.key_hash` stores. */
const sha256Hex = (plaintext: string): Effect.Effect<string, StorageError> =>
  Effect.tryPromise({
    try: async () => {
      const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(plaintext));
      return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
    },
    catch: (cause) => new StorageError({ operation: "insert", detail: "sha256(plaintext)", cause }),
  });

/** 32 random bytes, base64url — the secret part of a `mansio_` API key. */
const randomKeySecret = (): string => {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
};

/**
 * Build the service over an already-open client. Exported so tests can drive the registry
 * without a layer; `TenantRegistryLive` is the scoped layer over `Config`.
 */
export const makeTenantRegistry = (sql: Sql): TenantRegistryService => {
  /** Run one statement against `client` (the pool client or a `sql.begin` transaction). */
  const statement = <A>(
    client: Sql,
    operation: StorageError["operation"],
    table: string,
    run: (client: Sql) => Promise<A>,
  ): Effect.Effect<A, StorageError> =>
    Effect.tryPromise({
      try: () => run(client),
      catch: (cause) => new StorageError({ operation, table, cause }),
    });

  const getTenant = (id: TenantId): Effect.Effect<Tenant, TenantError | StorageError> =>
    Effect.gen(function* () {
      const rows = yield* statement(
        sql,
        "select",
        "tenants",
        (client) => client`SELECT id, slug, name, status, created_at FROM tenants WHERE id = ${id}`,
      );
      if (rows.length === 0) return yield* new UnknownTenant({ tenantId: id });
      return yield* decodeTenant(rows[0] as TenantRow);
    });

  const createTenant: TenantRegistryService["createTenant"] = (input) =>
    Effect.gen(function* () {
      // `crypto.randomUUID()` emits canonical lowercase — the `TenantId` brand holds by construction.
      const id = crypto.randomUUID() as TenantId;
      const rows = yield* Effect.tryPromise({
        try: () =>
          sql`INSERT INTO tenants (id, slug, name, status)
              VALUES (${id}, ${input.slug}, ${input.name}, 'active')
              RETURNING id, slug, name, status, created_at`,
        catch: (cause) =>
          cause instanceof PostgresError && cause.code === "23505"
            ? new ConflictError({
              // The caller assumed no tenant holds this slug (0); the stored state holds one (1).
              expected: 0,
              actual: 1,
              table: "tenants",
              id: input.slug,
            })
            : new StorageError({ operation: "insert", table: "tenants", cause }),
      });
      return yield* decodeTenant(rows[0] as TenantRow);
    });

  const listTenants: TenantRegistryService["listTenants"] = () =>
    Effect.gen(function* () {
      const rows = yield* statement(sql, "select", "tenants", (client) =>
        client`SELECT id, slug, name, status, created_at FROM tenants ORDER BY created_at ASC, id ASC`);
      return yield* Effect.forEach(rows as unknown as TenantRow[], decodeTenant);
    });

  const updateTenant: TenantRegistryService["updateTenant"] = (id, patch: TenantPatch) =>
    Effect.gen(function* () {
      if (patch.name === undefined && patch.status === undefined) return yield* getTenant(id);
      const rows = yield* Effect.tryPromise({
        try: () =>
          sql.begin(async (tx) => {
            const found = await tx`SELECT id FROM tenants WHERE id = ${id} FOR UPDATE`;
            if (found.length === 0) throw new UnknownTenant({ tenantId: id });
            if (patch.status === "deactivated") {
              // Deactivation revokes the tenant's keys (same semantics as `deactivateTenant`):
              // a reactivated tenant's old keys stay dead.
              await tx`UPDATE api_keys SET revoked_at = now()
                       WHERE tenant_id = ${id} AND revoked_at IS NULL`;
            }
            return await tx`UPDATE tenants
                            SET name = COALESCE(${patch.name ?? null}, name),
                                status = COALESCE(${patch.status ?? null}, status)
                            WHERE id = ${id}
                            RETURNING id, slug, name, status, created_at`;
          }),
        catch: (cause) =>
          cause instanceof UnknownTenant
            ? cause
            : new StorageError({ operation: "update", table: "tenants", cause }),
      });
      return yield* decodeTenant(rows[0] as TenantRow);
    });

  const deactivateTenant: TenantRegistryService["deactivateTenant"] = (id) =>
    updateTenant(id, { status: "deactivated" });

  const issueApiKey: TenantRegistryService["issueApiKey"] = (tenantId, name) =>
    Effect.gen(function* () {
      const keyId = crypto.randomUUID() as ApiKeyId;
      const plaintext = `mansio_${randomKeySecret()}`;
      const hash = yield* sha256Hex(plaintext);
      const rows = yield* Effect.tryPromise({
        try: () =>
          sql.begin(async (tx) => {
            // Lock the tenant row and re-check liveness inside the transaction: a key must
            // never be issued for a tenant deactivated concurrently with issuance. Throwing
            // rolls the transaction back; the typed error is re-raised below.
            const tenants = await tx`SELECT status FROM tenants WHERE id = ${tenantId} FOR UPDATE`;
            if (tenants.length === 0) throw new UnknownTenant({ tenantId });
            if ((tenants[0] as { status: string }).status !== "active") {
              throw new TenantDeactivated({ tenantId });
            }
            return await tx`INSERT INTO api_keys (id, tenant_id, key_hash, prefix, name)
                            VALUES (${keyId}, ${tenantId}, ${hash}, 'mansio_', ${name})
                            RETURNING id, tenant_id, name, created_at`;
          }),
        catch: (cause) =>
          cause instanceof UnknownTenant || cause instanceof TenantDeactivated
            ? cause
            : new StorageError({ operation: "insert", table: "api_keys", cause }),
      });
      const row = rows[0] as ApiKeyRow;
      const issued: IssuedApiKey = {
        id: row.id as ApiKeyId,
        tenantId: row.tenant_id as TenantId,
        name: row.name,
        prefix: "mansio_",
        // Returned exactly once, here. Never persisted: only the hash is stored.
        plaintext,
        createdAt: row.created_at,
      };
      return issued;
    });

  const revokeApiKey: TenantRegistryService["revokeApiKey"] = (keyId, tenantId) =>
    Effect.asVoid(
      statement(sql, "update", "api_keys", (client) =>
        client`UPDATE api_keys SET revoked_at = now() WHERE id = ${keyId} AND revoked_at IS NULL
                AND (${tenantId ?? null}::uuid IS NULL OR tenant_id = ${tenantId ?? null}::uuid)`),
    );

  const resolveTenantByKeyHash: TenantRegistryService["resolveTenantByKeyHash"] = (hash) =>
    Effect.gen(function* () {
      const rows = yield* statement(
        sql,
        "select",
        "api_keys",
        (client) =>
          client`SELECT t.id, t.slug, t.name, t.status, t.created_at
                FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
                WHERE k.key_hash = ${hash} AND k.revoked_at IS NULL AND t.status = 'active'`,
      );
      if (rows.length === 0) {
        // One error for unknown key, revoked key, and inactive tenant: distinguishing them
        // would hand attackers a key-guessing oracle.
        return yield* new UnknownTenant({});
      }
      return yield* decodeTenant(rows[0] as TenantRow);
    });

  return {
    createTenant,
    getTenant,
    listTenants,
    updateTenant,
    deactivateTenant,
    issueApiKey,
    revokeApiKey,
    resolveTenantByKeyHash,
  };
};

/**
 * Live layer: a scoped postgres.js client over `Config.tenancy.databaseUrl`. Acquire opens
 * the client and fails fast with a `SELECT 1` (a bad URL or refused connection fails the
 * layer, not the first query); release drains the pool with `sql.end()`.
 */
export const TenantRegistryLive: Layer.Layer<TenantRegistry, StorageError, Config> = Layer.scoped(
  TenantRegistry,
  Effect.gen(function* () {
    const { tenancy } = yield* Config;
    const sql = yield* Effect.acquireRelease(
      Effect.tryPromise({
        try: async () => {
          const client = postgres(tenancy.databaseUrl);
          try {
            await client`SELECT 1`;
            return client;
          } catch (cause) {
            await client.end();
            throw cause;
          }
        },
        catch: (cause) => new StorageError({ operation: "open", detail: "postgres connect", cause }),
      }),
      (client) => Effect.promise(() => client.end()),
    );
    return makeTenantRegistry(sql);
  }),
);

/**
 * Share one live registry for the server scope, acquired only on the first admin tenant call.
 * Default (tenancy-off) app traffic and startup therefore do not require Postgres.
 */
export const TenantRegistryLazy = Layer.scoped(
  TenantRegistry,
  Effect.gen(function* () {
    const config = yield* Config;
    const scope = yield* Scope.Scope;
    const lock = yield* Effect.makeSemaphore(1);
    const live = TenantRegistryLive.pipe(Layer.provide(Layer.succeed(Config, config)));
    let acquired: TenantRegistryService | undefined;
    const acquire = lock.withPermits(1)(Effect.gen(function* () {
      if (acquired !== undefined) return acquired;
      const context = yield* Layer.buildWithScope(live, scope);
      acquired = Context.get(context, TenantRegistry);
      return acquired;
    }));
    const call = <A, E>(f: (registry: TenantRegistryService) => Effect.Effect<A, E>) =>
      Effect.flatMap(acquire, f);
    return {
      createTenant: (input) => call((registry) => registry.createTenant(input)),
      getTenant: (id) => call((registry) => registry.getTenant(id)),
      listTenants: () => call((registry) => registry.listTenants()),
      updateTenant: (id, patch) => call((registry) => registry.updateTenant(id, patch)),
      deactivateTenant: (id) => call((registry) => registry.deactivateTenant(id)),
      issueApiKey: (id, name) => call((registry) => registry.issueApiKey(id, name)),
      revokeApiKey: (id, tenantId) => call((registry) => registry.revokeApiKey(id, tenantId)),
      resolveTenantByKeyHash: (hash) => call((registry) => registry.resolveTenantByKeyHash(hash)),
    } satisfies TenantRegistryService;
  }),
);
