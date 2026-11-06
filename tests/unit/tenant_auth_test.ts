/** MT-07: cache lifetimes and credentials at the real request seam. */
import { test } from "node:test";
import assert from "node:assert/strict";
import { Deferred, Effect, Either, Exit, Layer, Scope, TestClock, TestContext } from "effect";
import { StorageError, UnknownTenant } from "../../core/errors.ts";
import { makeCachedTenantRegistry, resolveRequestTenant } from "../../platform/http/auth.ts";
import { ConfigTest } from "../../platform/config.ts";
import { handle } from "../../platform/http/router.ts";
import { makeAppServicesLive } from "../../platform/layers.ts";
import { AppRegistryTest } from "../../platform/registry.ts";
import { defineFunctions } from "../../sdk/functions.ts";
import { schema } from "../../app/schema.ts";
import { EventSinkTest, IdGenTest, ObjectStoreTest } from "../layers/index.ts";
import { ReaderClient, TenantRegistry } from "../../platform/services.ts";
import { parseTenantId, type TenantRegistryService } from "../../platform/tenants/registry.ts";

const tenantId = Effect.runSync(parseTenantId("aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"));
const tenant = { id: tenantId, name: "A", slug: "a", status: "active" as const, createdAt: new Date(0) };
const unused = () => Effect.die("unexpected registry operation");
function stub(resolve: TenantRegistryService["resolveTenantByKeyHash"]): TenantRegistryService {
  return {
    createTenant: unused,
    getTenant: unused,
    listTenants: unused,
    updateTenant: unused,
    deactivateTenant: () => Effect.succeed(tenant),
    issueApiKey: unused,
    revokeApiKey: () => Effect.void,
    resolveTenantByKeyHash: resolve,
  };
}

test("tenant auth hashes keys before lookup and never resolves missing/malformed credentials", async () => {
  const hashes: string[] = [];
  const registry = stub((hash) => {
    hashes.push(hash);
    return hash === "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
      ? Effect.succeed(tenant)
      : Effect.fail(new UnknownTenant({}));
  });
  const run = (header?: string) =>
    Effect.runPromise(
      Effect.either(resolveRequestTenant(
        new Headers(header ? { authorization: header } : {}),
        "app",
      )).pipe(Effect.provideService(TenantRegistry, registry)),
    );
  assert.deepEqual(await run("Bearer abc"), Either.right(tenantId));
  for (const header of [undefined, "", "Basic abc", "Bearer", "Bearer a b"]) {
    const result = await run(header);
    assert.ok(Either.isLeft(result));
    assert.equal(result.left._tag, "AuthError");
  }
  assert.equal(hashes.length, 1);
  assert.ok(Either.isLeft(await run("Bearer invalid")));
  assert.equal(hashes.length, 2);
  assert.ok(hashes.every((hash) => /^[a-f0-9]{64}$/.test(hash)));
});

test("tenant key cache: fixed positive/negative TTL, capacity, local invalidation, no cached outages", async () => {
  await Effect.runPromise(
    Effect.gen(function* () {
      let calls = 0;
      let active = true;
      let unavailable = false;
      const registry = stub((hash) =>
        Effect.suspend((): ReturnType<TenantRegistryService["resolveTenantByKeyHash"]> => {
          calls++;
          if (unavailable) return Effect.fail(new StorageError({ operation: "open", detail: "offline" }));
          return active && hash === "valid" ? Effect.succeed(tenant) : Effect.fail(new UnknownTenant({}));
        })
      );
      const cached = yield* makeCachedTenantRegistry(registry, 100, 2);
      const lookup = (key: string) => Effect.either(cached.resolveTenantByKeyHash(key));
      yield* lookup("valid");
      active = false; // another instance revoked: visible by the original fixed deadline
      yield* TestClock.adjust(60);
      assert.ok(Either.isRight(yield* lookup("valid")));
      yield* TestClock.adjust(40);
      assert.ok(Either.isLeft(yield* lookup("valid")));
      assert.equal(calls, 2);
      active = true;
      yield* TestClock.adjust(60);
      assert.ok(Either.isLeft(yield* lookup("valid")), "negative hit does not slide expiry");
      yield* TestClock.adjust(40);
      assert.ok(Either.isRight(yield* lookup("valid")));
      assert.equal(calls, 3);
      yield* cached.deactivateTenant(tenantId);
      yield* lookup("valid");
      assert.equal(calls, 4, "local mutations clear the same cache used for authentication");
      yield* lookup("negative-1");
      yield* lookup("negative-2");
      yield* lookup("valid");
      assert.equal(calls, 7, "negative entries count toward capacity and evict oldest");
      unavailable = true;
      yield* lookup("outage");
      yield* lookup("outage");
      assert.equal(calls, 9, "driver failures are never cached");
    }).pipe(Effect.provide(TestContext.TestContext)),
  );
});

test("tenant HTTP query does not re-prepare SQLite while a mutation holds its transaction", async () => {
  const root = await Deno.makeTempDir({ prefix: "mansio-tenant-overlap-" });
  const scope = Effect.runSync(Scope.make());
  const entered = Effect.runSync(Deferred.make<void>());
  const release = Effect.runSync(Deferred.make<void>());
  const { mutation, query } = defineFunctions(schema);
  const app = {
    id: "test",
    file: `${root}/legacy.sqlite`,
    schema,
    token: "unused",
    functions: {
      hold: mutation({
        args: {},
        handler: () =>
          Effect.zipRight(
            Deferred.succeed(entered, undefined),
            Deferred.await(release).pipe(Effect.as(null)),
          ),
      }),
      read: query({ args: {}, handler: () => Effect.succeed("ok") }),
    },
  };
  const layer = makeAppServicesLive().pipe(Layer.provideMerge(Layer.mergeAll(
    ConfigTest({ tenancy: { mode: "api-key", tenantsDir: `${root}/tenants` } }),
    AppRegistryTest([app]),
    EventSinkTest,
    IdGenTest,
    ObjectStoreTest(),
    Layer.succeed(TenantRegistry, stub(() => Effect.succeed(tenant))),
    Layer.succeed(ReaderClient, {
      call: (frame) => {
        assert.equal(frame.kind, "query");
        if (frame.kind === "query") assert.equal(frame.tenant?.id, tenantId);
        return Effect.succeed("ok");
      },
    }),
  )));
  let writing: Promise<Response> | undefined;
  try {
    const context = await Effect.runPromise(Layer.buildWithScope(layer, scope));
    const request = (kind: string, name: string) =>
      Effect.runPromise(
        handle(
          new Request(
            `http://localhost/apps/test/${kind}/${name}`,
            {
              method: "POST",
              headers: { authorization: "Bearer abc" },
              body: "{}",
            },
          ),
        ).pipe(Effect.provide(context)),
      );
    writing = request("mutation", "hold");
    await Effect.runPromise(Deferred.await(entered).pipe(Effect.timeout("5 seconds")));
    const reading = await request("query", "read");
    assert.equal(reading.status, 200, await reading.text());
    await Effect.runPromise(Deferred.succeed(release, undefined));
    assert.equal((await writing).status, 200);
  } finally {
    await Effect.runPromise(Deferred.succeed(release, undefined));
    await writing;
    await Effect.runPromise(Scope.close(scope, Exit.void));
    await Deno.remove(root, { recursive: true });
  }
});
