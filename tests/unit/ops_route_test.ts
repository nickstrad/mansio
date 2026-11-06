/**
 * The Deno `/v1/ops` shim (IR-3), in process: `platform/http/router.ts`'s `handle` over a real SQLite
 * `Storage` (the IR rules themselves are the vectors': `ir_vectors_test.ts` runs every execute vector
 * through `Storage.ir`). What this pins is the route: app-token auth before the body, the body cap and
 * strict decode, `db.app` against the route's app, `X-Timeout-Ms`, the wire form of answers and errors,
 * and the SDK driving it end to end.
 */
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Context, Effect, Layer } from "effect";
import { canonical } from "../../core/ir/json.ts";
import { ConfigTest } from "../../platform/config.ts";
import { handle } from "../../platform/http/router.ts";
import { AppServices, appsFromConfig, makeAppRegistry, makeAppServices } from "../../platform/registry.ts";
import { AppRegistry, Config, ReaderClient } from "../../platform/services.ts";
import { makeStorage } from "../../platform/sqlite.ts";
import { createClient, schemaFingerprint } from "../../sdk/client.ts";
import { withStorage } from "../../sdk/schema.ts";
import { functions } from "../../app/functions.ts";
import { schema } from "../../app/schema.ts";
import { EventSinkTest, IdGenTest } from "../layers/index.ts";
import { UnusedTenantRegistry } from "../layers/tenant_registry_test_layer.ts";

const TOKEN = "columns-token";

async function withRoute(
  body: (send: (path: string, init?: RequestInit) => Promise<Response>) => Promise<void>,
): Promise<void> {
  const dir = mkdtempSync(join(tmpdir(), "ops-route-"));
  try {
    const config = ConfigTest({
      server: { dataDir: dir },
      apps: [{ id: "tic-tac-toe", token: TOKEN, storage: "columns" }],
    });
    const platform = Layer.scopedContext(Effect.gen(function* () {
      const apps = appsFromConfig(yield* Config);
      const storage = yield* makeStorage({ file: apps[0].file, schema: apps[0].schema, appId: apps[0].id });
      return Context.make(AppRegistry, makeAppRegistry(apps)).pipe(
        Context.add(AppServices, makeAppServices([{ app: apps[0], storage, backups: undefined as never }])),
        Context.add(ReaderClient, { call: () => Effect.die("the ops route never calls the reader") }),
      );
    }));
    const layer = platform.pipe(
      Layer.provideMerge(Layer.mergeAll(config, UnusedTenantRegistry, IdGenTest, EventSinkTest)),
    );
    await Effect.runPromise(Effect.scoped(Effect.gen(function* () {
      const context = yield* Layer.build(layer);
      const send = (path: string, init: RequestInit = {}) =>
        Effect.runPromise(Effect.provide(handle(new Request(`http://test.local${path}`, init)), context));
      yield* Effect.promise(() => body(send));
    })));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

const post = (body: unknown, headers: Record<string, string> = {}): RequestInit => ({
  method: "POST",
  headers: { authorization: `Bearer ${TOKEN}`, ...headers },
  body: typeof body === "string" ? body : canonical(body),
});

Deno.test("ops route: mutations, duplicates, queries and the wire errors of the shim", async () => {
  const fp = await schemaFingerprint(withStorage(schema, "columns"));
  const request = (over: Record<string, unknown>) => ({
    v: 1,
    db: { tenant: "local", app: "tic-tac-toe" },
    kind: "mutation",
    requestId: "r1",
    schema: fp,
    ops: [{
      op: "insert",
      table: "moves",
      id: "01890a5d-ac96-774b-bcce-b302099a8057",
      value: { gameId: "g", playerId: "p", mark: "X", cell: 0, moveNumber: 1 },
    }],
    ...over,
  });
  await withRoute(async (send) => {
    const path = "/apps/tic-tac-toe/v1/ops";
    const first = await send(path, post(request({}), { "x-trace-id": "trace-ops" }));
    assert.equal(first.status, 200);
    assert.equal(first.headers.get("x-trace-id"), "trace-ops");
    assert.deepEqual(await first.json(), { result: ["01890a5d-ac96-774b-bcce-b302099a8057"] });
    const again = await send(path, post(request({})));
    assert.deepEqual(await again.json(), {
      result: ["01890a5d-ac96-774b-bcce-b302099a8057"],
      duplicate: true,
    });

    const query = await send(
      path,
      post(
        request({
          kind: "query",
          requestId: undefined,
          ops: [{ op: "select", table: "moves", filters: [], limit: 5, aggregate: { kind: "count" } }],
        }),
      ),
    );
    assert.deepEqual(await query.json(), { result: [1] });

    const error = async (
      init: RequestInit,
      status: number,
      tag: string,
      fields?: Record<string, unknown>,
    ) => {
      const response = await send(path, init);
      const body = await response.json();
      assert.equal(response.status, status, JSON.stringify(body));
      assert.equal(body.error.tag, tag);
      for (const [k, v] of Object.entries(fields ?? {})) assert.deepEqual(body.error.fields[k], v, k);
      return body.error;
    };
    await error({ method: "POST", body: canonical(request({})) }, 401, "AuthError");
    await error(post(request({}), { "x-timeout-ms": "0" }), 400, "SchemaError", {
      path: "header:X-Timeout-Ms",
    });
    await error(post(request({ db: { tenant: "local", app: "other" } })), 400, "SchemaError", {
      path: "db.app",
    });
    await error(
      post(request({ requestId: "r2", schema: `sha256:${"0".repeat(64)}` })),
      409,
      "SchemaDriftError",
    );
    await error(post("{"), 400, "SchemaError", { path: "body" });
    await error(post(" ".repeat(65_537)), 413, "RequestTooLarge", { limit: 65_536 });
    const conflict = await error(
      post(
        request({
          requestId: "r3",
          ops: [{ op: "assert", table: "moves", id: "nope", expect: { exists: true } }],
        }),
      ),
      409,
      "ConflictError",
      { check: "exists" },
    );
    assert.equal(conflict.op, 0);
    const unknown = await send("/apps/nope/v1/ops", post(request({})));
    assert.equal(unknown.status, 404);
  });
});

Deno.test("ops route: the SDK runs the sample app against the shim", async () => {
  await withRoute(async (send) => {
    const client = createClient({
      functions,
      schema: withStorage(schema, "columns"),
      tenant: "local",
      app: "tic-tac-toe",
      endpoint: "http://test.local/apps/tic-tac-toe",
      token: TOKEN,
      fetch: ((url: string, init: RequestInit) => send(new URL(url).pathname, init)) as typeof fetch,
    });
    const { gameId } = await Effect.runPromise(
      client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }),
    );
    const game = await Effect.runPromise(
      client.mutation("makeMove", { gameId, playerId: "ada", cell: 4, expectedVersion: 1 }),
    );
    assert.equal(game?._version, 2);
    assert.deepEqual(await Effect.runPromise(client.query("getGame", { gameId })), game);
    assert.deepEqual(
      (await Effect.runPromise(client.query("moveHistory", { gameId }))).map((m) => m.cell),
      [4],
    );
    assert.deepEqual(await Effect.runPromise(client.mutation("deleteGame", { gameId })), { deleted: true });
    assert.equal(await Effect.runPromise(client.query("getGame", { gameId })), null);
  });
});
