/** MT-07: real HTTP, Postgres, SQLite and reader IPC, including CLI-created tenants. */
import { test } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import postgres from "npm:postgres@3.4.9";

const baseUrl = Deno.env.get("DATABASE_URL") ?? "postgres://mansio:mansio@localhost:5432/mansio";

test("tenant HTTP: CLI keys, reads, forged identity, concurrent mutations, revocation and deactivation", {
  timeout: 120_000,
}, async () => {
  const root = await Deno.makeTempDir({ prefix: "mansio-mt07-" });
  const name = `mt07_${crypto.randomUUID().replaceAll("-", "")}`;
  const admin = postgres(baseUrl);
  let sql: ReturnType<typeof postgres> | undefined;
  let child: Deno.ChildProcess | undefined;
  let stderr: Promise<string> | undefined;
  const portProbe = Deno.listen({ hostname: "127.0.0.1", port: 0 });
  const port = (portProbe.addr as Deno.NetAddr).port;
  portProbe.close();
  const base = `http://127.0.0.1:${port}`;
  const adminToken = "mt07-test-admin";
  try {
    await admin.unsafe(`CREATE DATABASE ${name}`);
    const url = new URL(baseUrl);
    url.pathname = `/${name}`;
    sql = postgres(url.toString());
    await sql.unsafe(await Deno.readTextFile(new URL("../../platform/tenants/schema.sql", import.meta.url)));
    child = new Deno.Command(Deno.execPath(), {
      args: ["run", "-A", "platform/server.ts"],
      cwd: new URL("../../", import.meta.url),
      env: {
        TENANCY: "api-key",
        TENANT_ROUTER: "local",
        DATABASE_URL: url.toString(),
        PORT: String(port),
        BIND_HOST: "127.0.0.1",
        ADMIN_TOKEN: adminToken,
        DATA_DIR: join(root, "data"),
        TENANTS_DIR: join(root, "tenants"),
        LOG_DIR: join(root, "logs"),
        LOCAL_OBJECT_DIR: join(root, "objects"),
        EVENTLOG_DUCKDB: join(root, "events.duckdb"),
        BACKUP_INTERVAL_MS: "0",
        S3_BASE_URL: "",
        EVENT_FLUSH_MS: "20",
      },
      stdout: "null",
      stderr: "piped",
    }).spawn();
    stderr = new Response(child.stderr).text();
    let ready = false;
    for (let i = 0; i < 300 && !ready; i++) {
      try {
        const response = await fetch(`${base}/health`, { signal: AbortSignal.timeout(500) });
        ready = response.ok;
        await response.body?.cancel();
      } catch { /* starting */ }
      if (!ready) await new Promise((resolve) => setTimeout(resolve, 100));
    }
    assert.ok(ready, "server became ready");
    // MT-06's tenant CLI is not on this base yet. curl exercises the same admin HTTP contract.
    const createTenant = async (slug: string) => {
      const result = await new Deno.Command("curl", {
        args: [
          "--silent",
          "--show-error",
          "--fail-with-body",
          "-X",
          "POST",
          `${base}/admin/tenants`,
          "-H",
          `Authorization: Bearer ${adminToken}`,
          "-H",
          "Content-Type: application/json",
          "--data",
          JSON.stringify({ name: slug, slug }),
        ],
        stdout: "piped",
        stderr: "piped",
      }).output();
      assert.equal(result.code, 0, new TextDecoder().decode(result.stderr));
      return JSON.parse(new TextDecoder().decode(result.stdout)).result;
    };
    const a = await createTenant("alpha");
    const b = await createTenant("beta");
    const send = async (key: string | null, kind: string, fn: string, args: unknown, forged?: string) => {
      const response = await fetch(`${base}/apps/tic-tac-toe/${kind}/${fn}`, {
        method: "POST",
        headers: {
          ...(key === null ? {} : { authorization: `Bearer ${key}` }),
          ...(forged ? { "x-tenant-id": forged } : {}),
        },
        body: JSON.stringify(args),
      });
      return { status: response.status, body: await response.json() };
    };
    const keyA = a.apiKey.plaintext;
    const keyB = b.apiKey.plaintext;
    for (const key of [null, "bad-key", "dev-only", adminToken]) {
      assert.equal((await send(key, "query", "listGames", { status: "playing" })).status, 401);
    }
    const forgedFlood = "spoofed-identity-".repeat(100);
    const rejected = await Promise.all(
      Array.from({ length: 16 }, () => send(null, "query", "listGames", { status: "playing" }, forgedFlood)),
    );
    assert.ok(rejected.every((response) => response.status === 401));
    const firstRead = await send(keyA, "query", "listGames", { status: "playing" });
    assert.equal(firstRead.status, 200, JSON.stringify(firstRead));
    assert.deepEqual(firstRead.body.result, []);
    const writes = await Promise.all(
      Array.from({ length: 16 }, (_, i) =>
        send(i % 2 ? keyB : keyA, "mutation", "createGame", {
          xPlayer: `${i % 2 ? "B" : "A"}-${i}`,
          oPlayer: "opponent",
          tenant: i % 2 ? a.tenant.id : b.tenant.id,
          tenantId: i % 2 ? a.tenant.id : b.tenant.id,
          tenant_id: i % 2 ? a.tenant.id : b.tenant.id,
        }, i % 2 ? a.tenant.id : b.tenant.id)),
    );
    for (const write of writes) assert.equal(write.status, 200, JSON.stringify(write));
    const aId = writes[0].body.result.gameId;
    const bId = writes[1].body.result.gameId;
    for (
      const [key, own, other, prefix, forged] of [
        [keyA, aId, bId, "A-", b.tenant.id],
        [keyB, bId, aId, "B-", a.tenant.id],
      ]
    ) {
      const found = await send(key, "query", "getGame", { gameId: own }, forged);
      assert.equal(found.status, 200, JSON.stringify(found));
      assert.equal(found.body.result._id, own);
      const absent = await send(key, "query", "getGame", { gameId: other }, forged);
      const missing = await send(key, "query", "getGame", { gameId: crypto.randomUUID() });
      assert.deepEqual(absent, missing);
      assert.equal(absent.body.result, null);
      assert.equal((await send(key, "mutation", "deleteGame", { gameId: other }, forged)).status, 404);
      const list = await send(key, "query", "listGames", { status: "playing" }, forged);
      assert.equal(list.status, 200, JSON.stringify(list));
      assert.equal(list.body.result.length, 8);
      assert.ok(list.body.result.every((row: { xPlayer: string }) => row.xPlayer.startsWith(prefix)));
    }
    const adminCall = async (path: string) => {
      const response = await fetch(`${base}${path}`, {
        method: "DELETE",
        headers: { authorization: `Bearer ${adminToken}` },
      });
      assert.equal(response.status, 200, await response.text());
    };
    // Simulate revocation by another server instance: this bypasses the local wrapper.
    const issuedResponse = await fetch(`${base}/admin/tenants/${a.tenant.id}/keys`, {
      method: "POST",
      headers: { authorization: `Bearer ${adminToken}` },
      body: JSON.stringify({ name: "external-revocation" }),
    });
    assert.equal(issuedResponse.status, 200);
    const externalKey = (await issuedResponse.json()).result;
    assert.equal((await send(externalKey.plaintext, "query", "getGame", { gameId: aId })).status, 200);
    await sql`UPDATE api_keys SET revoked_at = now() WHERE id = ${externalKey.id}`;
    // The fixed default TTL is 30 seconds. Cached authentication may succeed inside that window.
    assert.equal((await send(externalKey.plaintext, "query", "getGame", { gameId: aId })).status, 200);
    await new Promise((resolve) => setTimeout(resolve, 30_050));
    assert.equal((await send(externalKey.plaintext, "query", "getGame", { gameId: aId })).status, 401);

    assert.equal(
      (await send(keyA, "query", "getGame", { gameId: aId })).status,
      200,
      "rewarm the local key after the external revocation TTL elapsed",
    );
    await adminCall(`/admin/tenants/${a.tenant.id}/keys/${a.apiKey.id}`);
    assert.equal(
      (await send(keyA, "query", "getGame", { gameId: aId })).status,
      401,
      "in-process revocation evicts a warm key immediately",
    );
    assert.equal((await send(keyB, "query", "getGame", { gameId: bId })).status, 200);
    await adminCall(`/admin/tenants/${b.tenant.id}`);
    assert.equal((await send(keyB, "query", "getGame", { gameId: bId })).status, 401);
    child.kill("SIGTERM");
    assert.equal((await child.status).code, 0, await stderr);
    child = undefined;
    const logTexts: string[] = [];
    const walk = async (dir: string) => {
      for await (const entry of Deno.readDir(dir)) {
        const path = join(dir, entry.name);
        if (entry.isDirectory) await walk(path);
        else if (entry.name.endsWith(".jsonl")) logTexts.push(await Deno.readTextFile(path));
      }
    };
    await walk(join(root, "logs"));
    const logs = logTexts.join("\n");
    assert.ok(
      [keyA, keyB, externalKey.plaintext].every((key) => !logs.includes(key)),
      "plaintext keys never enter logs",
    );
    assert.ok(!logs.includes(forgedFlood), "forged identity content is never logged");
    assert.equal(
      logs.split("Ignored client-supplied tenant identity").length - 1,
      1,
      "forged header/body warnings share one bounded rate limiter",
    );
  } finally {
    if (child) {
      try {
        child.kill("SIGTERM");
      } catch { /* already stopped */ }
      await child.status;
      await stderr;
    }
    await sql?.end();
    await admin.unsafe(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
    await admin.end();
    await Deno.remove(root, { recursive: true });
  }
});

test("tenant reader IPC rejects absent and malformed tenant context without opening legacy storage", async () => {
  const root = await Deno.makeTempDir({ prefix: "mansio-mt07-reader-" });
  const child = new Deno.Command(Deno.execPath(), {
    args: ["run", "-A", "platform/reader.ts"],
    cwd: new URL("../../", import.meta.url),
    env: {
      TENANCY: "api-key",
      DATA_DIR: join(root, "missing-data"),
      LOG_DIR: join(root, "logs"),
      EVENTLOG_DUCKDB: join(root, "events.duckdb"),
    },
    stdin: "piped",
    stdout: "piped",
    stderr: "piped",
  }).spawn();
  const output = child.output();
  try {
    const requests = [undefined, {}, { id: "../../escape", file: "/tmp/legacy.sqlite" }, {
      id: "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa",
      file: "relative.db",
    }];
    const writer = child.stdin.getWriter();
    await writer.write(new TextEncoder().encode(
      requests.map((tenant, id) =>
        JSON.stringify({
          id,
          kind: "query",
          appId: "tic-tac-toe",
          name: "getGame",
          args: { gameId: "unknown" },
          tenant,
        })
      ).join("\n") + "\n",
    ));
    await writer.close();
    const result = await output;
    assert.equal(result.code, 0, new TextDecoder().decode(result.stderr));
    const replies = new TextDecoder().decode(result.stdout).trim().split("\n").map((line) =>
      JSON.parse(line)
    );
    assert.equal(replies.length, requests.length);
    for (const reply of replies) {
      assert.equal(reply.error._tag, "ReaderError");
      assert.match(reply.error.fields.detail, /tenant context/);
    }
    await assert.rejects(() => Deno.stat(join(root, "missing-data")), Deno.errors.NotFound);
  } finally {
    try {
      child.kill("SIGTERM");
    } catch { /* EOF already stopped it */ }
    await output;
    await Deno.remove(root, { recursive: true });
  }
});
