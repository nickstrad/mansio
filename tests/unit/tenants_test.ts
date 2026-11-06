/**
 * TenantRegistry tests (MT-03): `TenantRegistryLive` against ephemeral Postgres — the MT-01
 * compose service (`make up` before running; `DATABASE_URL` overrides the dev default).
 * pg-mem is explicitly NO for these tests (`docs/haiku-experiments/findings/sq-10.md`):
 * its ROLLBACK silently keeps rows, it misnames the duplicate-slug constraint, and it cannot
 * parse the schema's bare `REFERENCES tenants` DDL. Tables are truncated before each test;
 * no other test file touches them.
 */
import { after, before, beforeEach, test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { Cause, Context, Effect, Exit, Layer, Scope } from "effect";
import postgres from "npm:postgres@3.4.9";
import type { Sql } from "npm:postgres@3.4.9";
import { ConflictError, MalformedTenantId, TenantDeactivated, UnknownTenant } from "../../core/errors.ts";
import { ConfigTest } from "../../platform/config.ts";
import { TenantRegistry } from "../../platform/services.ts";
import { TenantRegistryLive } from "../../platform/tenants/store.ts";
import { parseApiKeyId, parseTenantId, type TenantRegistryService } from "../../platform/tenants/registry.ts";

const DATABASE_URL = Deno.env.get("DATABASE_URL") ?? "postgres://mansio:mansio@localhost:5432/mansio";

let scope: Scope.CloseableScope;
let sql: Sql;
let registry: TenantRegistryService;

const run = <A, E>(effect: Effect.Effect<A, E>): Promise<A> => Effect.runPromise(effect);

/** Run an effect that must fail; return its typed failure (defects fail the test). */
async function failWith<E>(effect: Effect.Effect<unknown, E>): Promise<E> {
  const exit = await Effect.runPromiseExit(effect);
  assert.equal(exit._tag, "Failure", "expected the effect to fail, but it succeeded");
  if (exit._tag === "Failure") {
    const failure = Cause.failureOption(exit.cause);
    assert.ok(failure._tag === "Some", "expected a typed failure, got a defect");
    return failure.value;
  }
  throw new Error("unreachable");
}

const sha256Hex = async (plaintext: string): Promise<string> => {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(plaintext));
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
};

before(async () => {
  sql = postgres(DATABASE_URL, { max: 5 });
  // Idempotent schema apply — the same file `make db-migrate` applies.
  const ddl = readFileSync(new URL("../../platform/tenants/schema.sql", import.meta.url), "utf8");
  await sql.unsafe(ddl);
  const layer = Layer.provide(
    TenantRegistryLive,
    ConfigTest({ tenancy: { databaseUrl: DATABASE_URL } }),
  );
  // The layer is scoped: build it against an explicit scope held for the whole file,
  // otherwise the pool client would be released when the `before` effect ends.
  scope = Effect.runSync(Scope.make());
  const context = await run(Layer.buildWithScope(layer, scope));
  registry = Context.get(context, TenantRegistry);
});

beforeEach(async () => {
  await sql`TRUNCATE api_keys, tenants`;
});

after(async () => {
  await run(Scope.close(scope, Exit.void));
  await sql.end();
});

test("createTenant → getTenant round trip", async () => {
  const created = await run(registry.createTenant({ name: "Acme", slug: "acme" }));
  assert.equal(created.slug, "acme");
  assert.equal(created.name, "Acme");
  assert.equal(created.status, "active");
  assert.match(created.id, /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
  assert.ok(created.createdAt instanceof Date);
  const fetched = await run(registry.getTenant(created.id));
  assert.deepEqual(fetched, created);
});

test("getTenant on an unknown id fails UnknownTenant", async () => {
  const id = await run(parseTenantId(crypto.randomUUID()));
  const error = await failWith(registry.getTenant(id));
  assert.ok(error instanceof UnknownTenant);
  assert.equal(error.tenantId, id);
});

test("duplicate slug is rejected with ConflictError", async () => {
  await run(registry.createTenant({ name: "First", slug: "dup" }));
  const error = await failWith(registry.createTenant({ name: "Second", slug: "dup" }));
  assert.ok(error instanceof ConflictError);
  assert.equal(error.table, "tenants");
  assert.equal(error.id, "dup");
  // The failed insert left no second row.
  const tenants = await run(registry.listTenants());
  assert.equal(tenants.length, 1);
});

test("listTenants returns every tenant, oldest first", async () => {
  await run(registry.createTenant({ name: "A", slug: "lista" }));
  await run(registry.createTenant({ name: "B", slug: "listb" }));
  const tenants = await run(registry.listTenants());
  assert.deepEqual(tenants.map((t) => t.slug), ["lista", "listb"]);
});

test("updateTenant patches the name; an empty patch is a no-op", async () => {
  const created = await run(registry.createTenant({ name: "Old", slug: "patchme" }));
  const updated = await run(registry.updateTenant(created.id, { name: "New" }));
  assert.equal(updated.name, "New");
  assert.equal(updated.slug, "patchme");
  assert.equal(updated.status, "active");
  const same = await run(registry.updateTenant(created.id, {}));
  assert.deepEqual(same, updated);
});

test("updateTenant on an unknown id fails UnknownTenant", async () => {
  const id = await run(parseTenantId(crypto.randomUUID()));
  const error = await failWith(registry.updateTenant(id, { name: "x" }));
  assert.ok(error instanceof UnknownTenant);
});

test("key plaintext is shown exactly once — the store keeps only the hash", async () => {
  const tenant = await run(registry.createTenant({ name: "KeyOnce", slug: "keyonce" }));
  const issued = await run(registry.issueApiKey(tenant.id, "admin"));
  assert.ok(issued.plaintext.startsWith("mansio_"));
  assert.equal(issued.prefix, "mansio_");
  assert.equal(issued.tenantId, tenant.id);

  const hash = await sha256Hex(issued.plaintext);
  const rows = await sql`SELECT * FROM api_keys WHERE id = ${issued.id}`;
  assert.equal(rows.length, 1);
  assert.equal(rows[0].key_hash, hash);
  // No column anywhere in the row carries the plaintext: a second read is impossible.
  for (const value of Object.values(rows[0] as Record<string, unknown>)) {
    assert.notEqual(value, issued.plaintext);
  }

  // Resolving by hash yields the tenant, never key material.
  const resolved = await run(registry.resolveTenantByKeyHash(hash));
  assert.equal(resolved.id, tenant.id);
  assert.ok(!("plaintext" in resolved));
});

test("resolveTenantByKeyHash round trip; unknown hash fails UnknownTenant", async () => {
  const tenant = await run(registry.createTenant({ name: "RoundTrip", slug: "roundtrip" }));
  const issued = await run(registry.issueApiKey(tenant.id, "k"));
  const resolved = await run(registry.resolveTenantByKeyHash(await sha256Hex(issued.plaintext)));
  assert.equal(resolved.id, tenant.id);
  assert.equal(resolved.slug, "roundtrip");

  const error = await failWith(registry.resolveTenantByKeyHash("0".repeat(64)));
  assert.ok(error instanceof UnknownTenant);
});

test("revoked key is rejected; revoking twice is a no-op", async () => {
  const tenant = await run(registry.createTenant({ name: "Revoke", slug: "revoke" }));
  const issued = await run(registry.issueApiKey(tenant.id, "k"));
  const hash = await sha256Hex(issued.plaintext);
  await run(registry.resolveTenantByKeyHash(hash)); // resolves before revocation

  await run(registry.revokeApiKey(issued.id));
  const error = await failWith(registry.resolveTenantByKeyHash(hash));
  assert.ok(error instanceof UnknownTenant);

  await run(registry.revokeApiKey(issued.id)); // idempotent
});

test("deactivateTenant revokes keys for real; inactive tenants reject keys and issuance", async () => {
  const tenant = await run(registry.createTenant({ name: "Deact", slug: "deact" }));
  const issued = await run(registry.issueApiKey(tenant.id, "k"));
  const hash = await sha256Hex(issued.plaintext);

  const deactivated = await run(registry.deactivateTenant(tenant.id));
  assert.equal(deactivated.status, "deactivated");

  // Revocation is real (`revoked_at` set), not mere status gating: reactivating the tenant
  // must not resurrect the key (MT-05/MT-06 reactivation semantics).
  const keyRows = await sql`SELECT revoked_at FROM api_keys WHERE id = ${issued.id}`;
  assert.ok(keyRows[0].revoked_at !== null);

  const resolveError = await failWith(registry.resolveTenantByKeyHash(hash));
  assert.ok(resolveError instanceof UnknownTenant);

  const issueError = await failWith(registry.issueApiKey(tenant.id, "k2"));
  assert.ok(issueError instanceof TenantDeactivated);

  const reactivated = await run(registry.updateTenant(tenant.id, { status: "active" }));
  assert.equal(reactivated.status, "active");
  const afterReactivate = await failWith(registry.resolveTenantByKeyHash(hash));
  assert.ok(afterReactivate instanceof UnknownTenant, "old key stays dead after reactivation");
});

test("issueApiKey is atomic: unknown tenant fails without leaving a key row", async () => {
  const id = await run(parseTenantId(crypto.randomUUID()));
  const error = await failWith(registry.issueApiKey(id, "k"));
  assert.ok(error instanceof UnknownTenant);
  const count = await sql`SELECT count(*)::int AS n FROM api_keys`;
  assert.equal(count[0].n, 0);
});

test("parseTenantId accepts only canonical-lowercase UUIDs", async () => {
  const good = "c04df567-11e4-4388-98af-2e9daaee060b";
  assert.equal(await run(parseTenantId(good)), good);
  assert.equal(await run(parseApiKeyId(good)), good);

  const bad = [
    good.toUpperCase(),
    `urn:uuid:${good}`,
    ` ${good}`,
    `${good} `,
    `${good}\n`,
    `${good}\t`,
    good.replace(/-/g, ""),
    `${good}\0`,
    `%63${good.slice(1)}`,
    `../${good}`,
    "",
    "not-a-uuid",
    "c04df567-11e4-4388-98af-2e9daaee060", // too short
    "g04df567-11e4-4388-98af-2e9daaee060b", // non-hex
  ];
  for (const input of bad) {
    const error = await failWith(parseTenantId(input));
    assert.ok(error instanceof MalformedTenantId, `expected MalformedTenantId for ${JSON.stringify(input)}`);
    assert.equal(error.input, input);
  }
});
