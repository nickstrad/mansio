/** MT-05 HTTP tests over TenantRegistryLive and an isolated real Postgres database. */
import { test } from "node:test";
import assert from "node:assert/strict";
import postgres from "npm:postgres@3.4.9";
import { Context, Effect, Exit, Layer, Scope } from "effect";
import { ConfigTest } from "../../platform/config.ts";
import { handle, type RouterRequirements } from "../../platform/http/router.ts";
import { TenantRegistry } from "../../platform/services.ts";
import { TenantRegistryLazy, TenantRegistryLive } from "../../platform/tenants/store.ts";
import { EventSinkTest, IdGenTest } from "../layers/index.ts";

const ADMIN = "mt05-admin-token";
const baseUrl = Deno.env.get("DATABASE_URL") ?? "postgres://mansio:mansio@localhost:5432/mansio";

async function hash(key: string): Promise<string> {
  const bytes = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(key)));
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

test("admin tenant HTTP: CRUD, one-time keys, authentication, validation and isolation", async () => {
  const dbName = `mt05_http_${crypto.randomUUID().replaceAll("-", "")}`;
  const admin = postgres(baseUrl);
  const scope = Effect.runSync(Scope.make());
  let sql: ReturnType<typeof postgres> | undefined;
  try {
    // Only internally-generated alphanumeric names enter this DDL; no caller input.
    await admin.unsafe(`CREATE DATABASE ${dbName}`);
    const url = new URL(baseUrl);
    url.pathname = `/${dbName}`;
    sql = postgres(url.toString());
    await sql.unsafe(await Deno.readTextFile(new URL("../../platform/tenants/schema.sql", import.meta.url)));
    const config = ConfigTest({ server: { adminToken: ADMIN }, tenancy: { databaseUrl: url.toString() } });
    const layer = TenantRegistryLive.pipe(
      Layer.provideMerge(Layer.mergeAll(config, EventSinkTest, IdGenTest)),
    );
    const context = await Effect.runPromise(Layer.buildWithScope(layer, scope));
    const registry = Context.get(context, TenantRegistry);
    // First acquisition fails, then the same lazy service recovers when Postgres is reachable.
    const probe = Deno.listen({ hostname: "127.0.0.1", port: 0 });
    const proxyPort = (probe.addr as Deno.NetAddr).port;
    probe.close();
    const proxyUrl = new URL(url);
    proxyUrl.hostname = "127.0.0.1";
    proxyUrl.port = String(proxyPort);
    proxyUrl.searchParams.set("connect_timeout", "1");
    const lazyLayer = TenantRegistryLazy.pipe(Layer.provide(ConfigTest({
      tenancy: { databaseUrl: proxyUrl.toString() },
    })));
    const lazy = Context.get(await Effect.runPromise(Layer.buildWithScope(lazyLayer, scope)), TenantRegistry);
    assert.equal((await Effect.runPromiseExit(lazy.listTenants()))._tag, "Failure");
    const listener = Deno.listen({ hostname: "127.0.0.1", port: proxyPort });
    const connections: Deno.Conn[] = [];
    const tunnels: Promise<unknown>[] = [];
    const accepting = (async () => {
      try {
        for await (const incoming of listener) {
          const outgoing = await Deno.connect({ hostname: url.hostname, port: Number(url.port || 5432) });
          connections.push(incoming, outgoing);
          tunnels.push(Promise.allSettled([
            incoming.readable.pipeTo(outgoing.writable),
            outgoing.readable.pipeTo(incoming.writable),
          ]));
        }
      } catch (error) {
        if (!(error instanceof Deno.errors.BadResource)) throw error;
      }
    })();
    try {
      assert.deepEqual(await Effect.runPromise(lazy.listTenants()), []);
      assert.deepEqual(await Effect.runPromise(lazy.listTenants()), []);
      assert.equal(connections.length, 2, "successful acquisition shares its live client");
    } finally {
      listener.close();
      for (const connection of connections) {
        try {
          connection.close();
        } catch { /* already closed */ }
      }
      await accepting;
      await Promise.all(tunnels);
    }

    // These routes use only Config, TenantRegistry, EventSink and IdGen.
    const send = async (method: string, path: string, body?: unknown, token: string | null = ADMIN) => {
      const request = new Request(`http://localhost${path}`, {
        method,
        headers: token === null ? {} : { authorization: `Bearer ${token}` },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      const response = await Effect.runPromise(Effect.provide(
        handle(request),
        context as Context.Context<RouterRequirements>,
      ));
      return { status: response.status, data: await response.json() };
    };
    const missing = crypto.randomUUID();
    const routes = [
      ["POST", "/admin/tenants"],
      ["GET", "/admin/tenants"],
      ["GET", `/admin/tenants/${missing}`],
      ["PATCH", `/admin/tenants/${missing}`],
      ["DELETE", `/admin/tenants/${missing}`],
      ["POST", `/admin/tenants/${missing}/keys`],
      ["DELETE", `/admin/tenants/${missing}/keys/${crypto.randomUUID()}`],
    ];
    for (const [method, path] of routes) {
      for (const token of [null, "wrong", "app-token"]) {
        assert.equal((await send(method, path, undefined, token)).status, 401, `${method} ${path}`);
      }
    }
    assert.equal((await send("POST", "/admin/tenants", { name: "First", slug: "first" })).status, 200);
    const created = await send("POST", "/admin/tenants", { name: "Acme", slug: "acme" });
    assert.equal(created.status, 200);
    const { tenant, apiKey } = created.data.result;
    assert.match(apiKey.plaintext, /^mansio_[A-Za-z0-9_-]{43}$/);
    assert.equal(
      (await Effect.runPromise(registry.resolveTenantByKeyHash(await hash(apiKey.plaintext)))).id,
      tenant.id,
    );
    const stored = await sql`SELECT * FROM api_keys WHERE id = ${apiKey.id}`;
    assert.equal(stored[0].key_hash, await hash(apiKey.plaintext));
    assert.ok(!Object.values(stored[0]).includes(apiKey.plaintext));
    const list = await send("GET", "/admin/tenants");
    assert.equal(list.status, 200);
    assert.equal(list.data.result.length, 2);
    const get = await send("GET", `/admin/tenants/${tenant.id}`);
    assert.equal(get.status, 200);
    assert.deepEqual(get.data.result, tenant);
    assert.ok(!JSON.stringify([get.data, list.data]).includes(apiKey.plaintext));
    assert.equal((await send("POST", "/admin/tenants", { name: "Dup", slug: "acme" })).status, 409);
    const path = `/admin/tenants/${tenant.id}`;
    const renamed = await send("PATCH", path, { name: "Renamed" });
    assert.equal(renamed.status, 200);
    assert.equal(renamed.data.result.name, "Renamed");
    assert.equal((await send("PATCH", path, {})).status, 200);
    const issued = await send("POST", `${path}/keys`, { name: "second" });
    assert.equal(issued.status, 200);
    const second = issued.data.result;
    const other = list.data.result.find((t: { id: string }) => t.id !== tenant.id);
    assert.equal((await send("DELETE", `/admin/tenants/${other.id}/keys/${second.id}`)).status, 200);
    await Effect.runPromise(registry.resolveTenantByKeyHash(await hash(second.plaintext)));
    assert.equal((await send("DELETE", `${path}/keys/${second.id}`)).status, 200);
    assert.equal((await send("DELETE", `${path}/keys/${second.id}`)).status, 200);
    assert.equal(
      (await Effect.runPromiseExit(registry.resolveTenantByKeyHash(await hash(second.plaintext))))._tag,
      "Failure",
    );
    assert.equal((await send("DELETE", path)).data.result.status, "deactivated");
    const forbidden = await send("POST", `${path}/keys`, { name: "blocked" });
    assert.equal(forbidden.status, 403);
    assert.equal(forbidden.data.error.tag, "TenantDeactivated");
    assert.equal((await send("PATCH", path, { status: "active" })).status, 200);
    assert.equal(
      (await Effect.runPromiseExit(registry.resolveTenantByKeyHash(await hash(apiKey.plaintext))))._tag,
      "Failure",
    );
    const fresh = await send("POST", `${path}/keys`, { name: "fresh" });
    assert.equal(fresh.status, 200);
    await Effect.runPromise(registry.resolveTenantByKeyHash(await hash(fresh.data.result.plaintext)));
    assert.equal((await send("PATCH", path, { status: "deactivated" })).status, 200);
    assert.equal((await send("PATCH", path, { status: "active" })).status, 200);
    assert.equal(
      (await Effect.runPromiseExit(registry.resolveTenantByKeyHash(await hash(fresh.data.result.plaintext))))
        ._tag,
      "Failure",
    );
    for (const [method, target] of routes.slice(2)) {
      const response = await send(
        method,
        target,
        method === "PATCH" || method === "POST" ? { name: "x" } : undefined,
      );
      assert.equal(response.status, 404);
      assert.equal(response.data.error.tag, "UnknownTenant");
    }
    for (const id of ["not-a-uuid", tenant.id.toUpperCase(), "%61bad"]) {
      assert.equal((await send("GET", `/admin/tenants/${id}`)).status, 400);
    }
    assert.equal((await send("DELETE", `${path}/keys/bad`)).status, 400);
    for (
      const body of [null, [], {}, { name: 1, slug: "x" }, { name: " ", slug: "x" }, {
        name: "a",
        slug: "b",
        extra: true,
      }]
    ) {
      assert.equal((await send("POST", "/admin/tenants", body)).status, 400);
    }
    for (const body of [{ status: "invalid" }, { name: null }, { slug: "immutable" }]) {
      assert.equal((await send("PATCH", path, body)).status, 400);
    }
    assert.equal((await send("PUT", path)).status, 404);
  } finally {
    await Effect.runPromise(Scope.close(scope, Exit.void));
    await sql?.end();
    await admin.unsafe(`DROP DATABASE IF EXISTS ${dbName}`);
    await admin.end();
  }
});
