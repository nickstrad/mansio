/**
 * The `make platform-up` proof (scripts/platform/up.sh): against the running dev platform, registers the
 * tic-tac-toe schema at controld, creates a tenant and an API key through controld's tenancy admin API, then
 * does one SDK write (`createGame`) and one SDK read (`getGame`) on the locator route — resolve at controld,
 * every request through the edge with the ticket — and checks the document read back is the one written.
 * Then it plays four moves and counts the `/v1/ops` requests each `makeMove` cost (the read path): one when
 * the client's cache holds the game, two when it does not, two when the cache is stale, and the game must end
 * at the version four moves make.
 *
 * Input: PLATFORM_DIR/smoke.json ({api, edge, admin_token}); the dev CA through DENO_CERT. Exit 0 and one
 * `platform smoke OK` line on success; nonzero with the reason otherwise. Same calls as
 * tests/e2e/platform/cluster.ts (registerSchema, tenant, client).
 */
import { Effect } from "effect";
import { functions } from "../../examples/tic-tac-toe/functions.ts";
import { schema } from "../../examples/tic-tac-toe/schema.ts";
import { fingerprintText } from "../../core/intermediate-representation/schema.ts";
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

  // Counts the /v1/ops requests the server decided (200, or 409 for a refused precondition); a request
  // the platform turned away and the transport sent again is not a round trip of the handler.
  let trips = 0;
  const counting: typeof fetch = async (input, init) => {
    const response = await fetch(input, init);
    if (String(input).endsWith("/v1/ops") && (response.status === 200 || response.status === 409)) trips++;
    return response;
  };
  const newClient = () =>
    createClient({
      functions,
      schema: s,
      tenant,
      app: APP,
      timeoutMs: 20_000,
      fetch: counting,
      locator: { url: cfg.api, apiKey, create: true, edge: cfg.edge },
    });
  const client = newClient();
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

  // The game's version as the last answer gave it. Versions only rise; the numbers are the server's (a durable
  // agent counts from its version floor, spec §6.3), so each move expects the version the one before answered.
  let version = game._version;
  const move = async (who: ReturnType<typeof newClient>, playerId: string, cell: number) => {
    trips = 0;
    const after = await Effect.runPromise(
      who.mutation("makeMove", { gameId, playerId, cell, expectedVersion: version }),
    );
    if (!after || after._version <= version) {
      throw new Error(
        `makeMove(cell ${cell}) answered version ${after?._version}, want one above ${version}`,
      );
    }
    version = after._version;
    return trips;
  };
  const other = newClient(); // a second caller, with its own (empty) cache
  const counts = {
    warm: [await move(client, xPlayer, 0), await move(client, "bob", 1)], // getGame cached the game
    cold: await move(other, xPlayer, 2),
    stale: await move(client, "bob", 3), // this client's cache still holds the game before `other` moved
  };
  console.log(
    `smoke: makeMove round trips through the edge: warm cache ${
      counts.warm.join(" and ")
    }, cold ${counts.cold}, ` +
      `stale cache ${counts.stale}`,
  );
  if (counts.warm.some((n) => n !== 1) || counts.cold !== 2 || counts.stale !== 2) {
    throw new Error(`makeMove round trips ${JSON.stringify(counts)}, want warm 1, cold 2, stale 2`);
  }
  const final = await Effect.runPromise(client.query("getGame", { gameId }));
  if (final?._version !== version || final.board.filter((cell) => cell !== null).length !== 4) {
    throw new Error(`after four moves the game is ${JSON.stringify(final)}`);
  }
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
