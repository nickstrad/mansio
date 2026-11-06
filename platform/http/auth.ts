/**
 * Request authentication. TENANCY=off keeps legacy app tokens; enabled tenancy hashes
 * API keys and provides request-local TenantContext. Admin credentials stay separate.
 */
import { Clock, Effect, Either } from "effect";
import { AuthError, type StorageError, type TenantError } from "../../core/errors.ts";
import { type App, Config, type ConfigService, TenantRegistry } from "../services.ts";
import type { Tenant, TenantRegistryService } from "../tenants/registry.ts";
import { sessionTenant } from "../auth/service.ts";
import { TenantContext } from "../tenants/routing.ts";

/** The documented default for every token (`platform/config.ts`). */
export const DEV_TOKEN = "dev-only";

/** Who a request authenticated as. */
export type Principal =
  | { readonly scope: "app"; readonly appId: string }
  | { readonly scope: "admin" };

const encoder = new TextEncoder();

/**
 * Equality that does not stop at the first differing byte. Length still leaks (the loop runs over the
 * longer input); token lengths are not secret here.
 */
export function constantTimeEqual(a: string, b: string): boolean {
  const left = encoder.encode(a);
  const right = encoder.encode(b);
  let diff = left.length ^ right.length;
  const length = Math.max(left.length, right.length);
  for (let i = 0; i < length; i++) diff |= (left[i] ?? 0) ^ (right[i] ?? 0);
  return diff === 0;
}

type BearerResult =
  | { readonly _tag: "token"; readonly token: string }
  | { readonly _tag: "missing" }
  | { readonly _tag: "malformed" };

/** Reads `Authorization: Bearer <token>`. The scheme is case-sensitive, as it was before the port. */
export function bearerToken(headers: Headers): BearerResult {
  const header = headers.get("authorization");
  if (header === null || header === "") return { _tag: "missing" };
  if (!header.startsWith("Bearer ") || header.length === "Bearer ".length) return { _tag: "malformed" };
  return { _tag: "token", token: header.slice("Bearer ".length) };
}

function check(
  headers: Headers,
  expected: string,
  fail: (reason: AuthError["reason"]) => AuthError,
): Effect.Effect<void, AuthError> {
  const presented = bearerToken(headers);
  if (presented._tag !== "token") return Effect.fail(fail(presented._tag));
  return constantTimeEqual(presented.token, expected) ? Effect.void : Effect.fail(fail("mismatch"));
}

/** `Bearer <app.token>`, or `AuthError({ scope: "app" })`. */
export function authenticateApp(headers: Headers, app: App): Effect.Effect<Principal, AuthError> {
  return Effect.as(
    check(headers, app.token, (reason) => new AuthError({ scope: "app", appId: app.id, reason })),
    { scope: "app", appId: app.id } as const,
  );
}

/** `Bearer <ADMIN_TOKEN>`, or `AuthError({ scope: "admin" })`. */
export function authenticateAdmin(
  headers: Headers,
  config: ConfigService,
): Effect.Effect<Principal, AuthError> {
  return Effect.as(
    check(headers, config.server.adminToken, (reason) => new AuthError({ scope: "admin", reason })),
    { scope: "admin" } as const,
  );
}

/**
 * Which configured tokens still hold the `dev-only` default, by env var. Empty in a configured
 * deployment; the server emits one `lifecycle` warning at startup when it is not.
 */
export function devTokensInUse(config: ConfigService): readonly string[] {
  const inUse: string[] = [];
  if (config.server.adminToken === DEV_TOKEN) inUse.push("ADMIN_TOKEN");
  for (const app of config.apps) {
    if (app.token === DEV_TOKEN) inUse.push(`token for ${app.id}`);
  }
  return inUse;
}

/**
 * Scope-owned key cache. Only SHA-256 hashes enter this service. Expiry is fixed at the
 * start of each lookup, never extended on a hit; both successful and negative lookups
 * count toward the bound. Driver failures are never cached. Other server instances see
 * revocation/deactivation within one TTL. Local mutations invalidate even on failure
 * (a connection can fail after the database commits).
 */
export const makeCachedTenantRegistry = (
  registry: TenantRegistryService,
  ttlMs: number,
  capacity = 1024,
): Effect.Effect<TenantRegistryService> =>
  Effect.gen(function* () {
    if (
      !Number.isSafeInteger(ttlMs) || ttlMs <= 0 || ttlMs > 300_000 ||
      !Number.isSafeInteger(capacity) || capacity <= 0
    ) {
      return yield* Effect.die(new Error("Invalid tenant key cache limits"));
    }
    const clock = yield* Effect.clock;
    const lock = yield* Effect.makeSemaphore(1);
    const entries = new Map<string, {
      expires: number;
      result: Either.Either<Tenant, TenantError>;
    }>();
    const invalidate = Effect.sync(() => entries.clear());
    const change = <A, E>(effect: Effect.Effect<A, E>) =>
      lock.withPermits(1)(effect.pipe(Effect.ensuring(invalidate)));
    return {
      ...registry,
      updateTenant: (id, patch) => change(registry.updateTenant(id, patch)),
      deactivateTenant: (id) => change(registry.deactivateTenant(id)),
      revokeApiKey: (id, tenantId) => change(registry.revokeApiKey(id, tenantId)),
      issueApiKey: (id, name) => change(registry.issueApiKey(id, name)),
      resolveTenantByKeyHash: (hash) =>
        lock.withPermits(1)(Effect.gen(function* () {
          const now = clock.unsafeCurrentTimeMillis();
          const cached = entries.get(hash);
          if (cached && now < cached.expires) return yield* cached.result;
          entries.delete(hash);
          const result = yield* registry.resolveTenantByKeyHash(hash).pipe(
            Effect.map((tenant) => Either.right(tenant)),
            Effect.catchIf(
              (error): error is TenantError => error._tag !== "StorageError",
              (error) => Effect.succeed(Either.left(error)),
            ),
          );
          if (entries.size >= capacity) entries.delete(entries.keys().next().value!);
          entries.set(hash, { expires: now + ttlMs, result });
          return yield* result;
        })),
    };
  });

// One constant-size rate limiter per scope-owned registry, no caller-controlled log content.
const warningTimes = new WeakMap<TenantRegistryService, number>();
function warnTenantIdentity(registry: TenantRegistryService): Effect.Effect<void> {
  return Effect.gen(function* () {
    const now = yield* Clock.currentTimeMillis;
    if (now < (warningTimes.get(registry) ?? -Infinity)) return;
    warningTimes.set(registry, now + 60_000);
    yield* Effect.logWarning("Ignored client-supplied tenant identity; credentials determine tenant");
  });
}

/** Never passes the plaintext key to the registry or includes it in an error/log. */
export function resolveRequestTenant(headers: Headers, appId: string) {
  return Effect.gen(function* () {
    const registry = yield* TenantRegistry;
    if (headers.has("x-tenant-id")) yield* warnTenantIdentity(registry);
    const presented = bearerToken(headers);
    const fail = (reason: AuthError["reason"]) => new AuthError({ scope: "app", appId, reason });
    if (presented._tag !== "token") return yield* fail(presented._tag);
    if (!/^[^\s]+$/.test(presented.token)) return yield* fail("malformed");
    const configured = yield* Effect.serviceOption(Config);
    const config = configured._tag === "Some" ? configured.value : undefined;
    if (config?.tenancy.mode === "better-auth" && !presented.token.startsWith("mansio_")) {
      const id = yield* sessionTenant(config.tenancy, presented.token, appId);
      const tenant = yield* registry.getTenant(id).pipe(
        Effect.catchIf((error) => error._tag !== "StorageError", () => Effect.fail(fail("mismatch"))),
      );
      if (tenant.status !== "active") return yield* fail("mismatch");
      return tenant.id;
    }
    const hash = yield* Effect.promise(async () => {
      const digest = await crypto.subtle.digest("SHA-256", encoder.encode(presented.token));
      return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
    });
    const tenant = yield* registry.resolveTenantByKeyHash(hash).pipe(
      Effect.catchIf((error) => error._tag !== "StorageError", () => Effect.fail(fail("mismatch"))),
    );
    if (tenant.status !== "active") return yield* fail("mismatch");
    return tenant.id;
  });
}

/**
 * Authenticate before reading the body; provide tenant context only to this request's
 * continuation. TENANCY=off preserves the legacy app-token path and body unchanged.
 * Body identity fields are ignored, not forwarded as application arguments.
 */
export function withAppAuthentication<A, E, R, E2, R2>(
  headers: Headers,
  app: App,
  body: Effect.Effect<unknown, E2, R2>,
  run: (args: unknown) => Effect.Effect<A, E, R>,
): Effect.Effect<
  A,
  E | E2 | AuthError | StorageError | TenantError,
  R | R2 | Config | TenantRegistry
> {
  return Effect.gen(function* () {
    const config = yield* Config;
    if (config.tenancy.mode === "off") {
      yield* authenticateApp(headers, app);
      return yield* Effect.flatMap(body, run);
    }
    const tenantId = yield* resolveRequestTenant(headers, app.id);
    return yield* Effect.gen(function* () {
      const args = yield* body;
      // Raw bytes (the ops route) are not an argument object: nothing to strip.
      if (args !== null && typeof args === "object" && !Array.isArray(args) && !ArrayBuffer.isView(args)) {
        const fields = ["tenant", "tenantId", "tenant_id"];
        if (fields.some((field) => Object.hasOwn(args, field))) {
          yield* warnTenantIdentity(yield* TenantRegistry);
          return yield* run(
            Object.fromEntries(Object.entries(args).filter(([key]) => !fields.includes(key))),
          );
        }
      }
      return yield* run(args);
    }).pipe(Effect.provideService(TenantContext, { tenantId }));
  });
}
