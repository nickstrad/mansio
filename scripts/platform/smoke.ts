/**
 * The `make platform-up` proof (scripts/platform/up.sh): against the running dev platform, registers the
 * tic-tac-toe schema at controld, creates a tenant and an API key through controld's tenancy admin API, then
 * does one SDK write (`createGame`) and one SDK read (`getGame`) on the locator route — resolve at controld,
 * every request through the edge with the ticket — and checks the document read back is the one written.
 *
 * Input: PLATFORM_DIR/smoke.json ({api, edge, admin_token}); the dev CA through DENO_CERT. Exit 0 and one
 * `platform smoke OK` line on success; nonzero with the reason otherwise. Same calls as
 * tests/e2e/platform/cluster.ts (registerSchema, tenant, client).
 */
import { Effect } from "effect";
import { functions } from "../../app/functions.ts";
import { schema } from "../../app/schema.ts";
import { fingerprintText } from "../../core/ir/schema.ts";
import { createClient } from "../../sdk/client.ts";
import { schemaFingerprint } from "../../sdk/fingerprint.ts";
import { withStorage } from "../../sdk/schema.ts";

const APP = "tic-tac-toe";

interface Smoke {
  api: string;
  edge: string;
  admin_token: string;
}

async function admin(cfg: Smoke, method: string, path: string, body: unknown) {
  const response = await fetch(`${cfg.api}${path}`, {
    method,
    headers: { authorization: `Bearer ${cfg.admin_token}`, "content-type": "application/json" },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(30_000),
  });
  const out = await response.json();
  if (response.status !== 200) {
    throw new Error(`${method} ${path}: ${response.status} ${JSON.stringify(out)}`);
  }
  return out;
}

async function main() {
  const dir = Deno.env.get("PLATFORM_DIR");
  if (!dir) throw new Error("PLATFORM_DIR is required (run via make platform-up)");
  const cfg = JSON.parse(await Deno.readTextFile(`${dir}/smoke.json`)) as Smoke;

  const s = withStorage(schema, "columns");
  const registered = await admin(cfg, "PUT", `/v1/apps/${APP}/schema`, {
    fingerprint: await schemaFingerprint(s),
    schema: fingerprintText(s),
  });
  console.log(`smoke: schema ${APP} registered (fingerprint ${registered.fingerprint})`);

  const slug = `smoke-${Date.now().toString(36)}-${crypto.randomUUID().slice(0, 8)}`;
  const created = await admin(cfg, "POST", "/admin/tenants", { name: slug, slug });
  const tenant = created.result.tenant.id as string;
  const apiKey = created.result.apiKey.plaintext as string;
  console.log(`smoke: tenant ${slug} (${tenant}) and an API key created`);

  const client = createClient({
    functions,
    schema: s,
    tenant,
    app: APP,
    timeoutMs: 20_000,
    locator: { url: cfg.api, apiKey, create: true, edge: cfg.edge },
  });
  const xPlayer = `alice-${crypto.randomUUID().slice(0, 8)}`;
  const { gameId } = await Effect.runPromise(client.mutation("createGame", { xPlayer, oPlayer: "bob" }));
  console.log(`smoke: SDK write through the edge: createGame → ${gameId} (xPlayer ${xPlayer})`);
  const game = await Effect.runPromise(client.query("getGame", { gameId }));
  if (!game || game._id !== gameId || game.xPlayer !== xPlayer || game.oPlayer !== "bob") {
    throw new Error(`read back ${JSON.stringify(game)}, want game ${gameId} with xPlayer ${xPlayer}`);
  }
  console.log(
    `smoke: SDK read through the edge: getGame(${gameId}) → xPlayer ${game.xPlayer}, oPlayer ${game.oPlayer}`,
  );
  console.log(
    `platform smoke OK: wrote and read back game ${gameId} (xPlayer ${xPlayer}) via ${cfg.edge} for ${tenant}:${APP}`,
  );
}

try {
  await main();
} catch (e) {
  console.error(`platform smoke FAILED: ${e instanceof Error ? e.stack ?? e.message : String(e)}`);
  Deno.exit(1);
}
