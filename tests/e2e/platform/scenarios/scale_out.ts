/**
 * IR-5 `scale_out` (replace-platform plan §5 phase 6): two durable agents, controld and the edge; the SDK
 * resolves at controld (an API key from controld's tenancy admin API; the database created on first use)
 * and sends every request through the edge.
 *
 * 1. A move under load, twice (vm → the other → back): concurrent writers never see a definite failure,
 *    and afterwards every acknowledged mutation is there exactly once, nothing else is (request ids make
 *    the SDK's retries across the move safe), and the owner is where the move put it.
 * 2. The locator down doesn't stop running traffic: with controld stopped, cached routes and tickets, the
 *    edge's cached registry and keys, and the agents' cached keys carry on.
 * 3. A VM restored after an operator attestation: the owner is written back and killed (kill -9), attested
 *    dead and restored; the other agent serves every acknowledged write, and the same client writes again
 *    once the edge drops the dead VM.
 */
import { Effect, Either } from "effect";
import { Unavailable } from "../../../../core/errors.ts";
import type { createClient } from "../../../../sdk/client.ts";
import { defineFunctions } from "../../../../sdk/functions.ts";
import { v } from "../../../../sdk/schema.ts";
import { schema } from "../../../../examples/tic-tac-toe/schema.ts";
import { functions as app } from "../../../../examples/tic-tac-toe/functions.ts";
import { apps, assert, delay, run, scenarioDir, test } from "../harness.ts";
import { database } from "../postgres.ts";
import { startCluster, VMS } from "../cluster.ts";

const { query } = defineFunctions(schema);
const functions = {
  createGame: app.createGame,
  /** Every game (the ledger check; a run stays far below the cap). */
  allGames: query({
    args: { cap: v.integer() },
    handler: ({ db }, { cap }) => db.find("games", {}).limit(cap).collect(),
  }),
};
const CAP = 1000;

/** What each writer saw: a definite answer, or none (the commit may or may not have landed). */
class Ledger {
  readonly acked = new Map<string, string>(); // player → game id
  readonly ambiguous = new Set<string>();
  readonly failed: string[] = [];
}

type C = ReturnType<typeof createClient<typeof functions>>;

async function write(c: C, ledger: Ledger, player: string) {
  const out = await run(Effect.either(c.mutation("createGame", { xPlayer: player, oPlayer: "o" })));
  if (Either.isRight(out)) ledger.acked.set(player, (out.right as { gameId: string }).gameId);
  else if (out.left instanceof Unavailable && out.left.mayHaveApplied) ledger.ambiguous.add(player);
  else ledger.failed.push(`${player}: ${out.left._tag} ${JSON.stringify(out.left)}`);
}

/** Every acknowledged write exactly once, with its id; nothing that was not (maybe) written. */
async function check(c: C, ledger: Ledger, what: string) {
  const games = await run(c.query("allGames", { cap: CAP })) as { _id: string; xPlayer: string }[];
  assert.ok(games.length < CAP, `${what}: the check's cap was reached`);
  const seen = new Map<string, string[]>();
  for (const g of games) seen.set(g.xPlayer, [...(seen.get(g.xPlayer) ?? []), g._id]);
  for (const [player, ids] of seen) {
    assert.equal(ids.length, 1, `${what}: ${player} written ${ids.length} times (duplicate mutation)`);
    assert.ok(
      ledger.acked.has(player) || ledger.ambiguous.has(player),
      `${what}: ${player} is stored but was never acknowledged or in doubt`,
    );
  }
  for (const [player, id] of ledger.acked) {
    assert.deepEqual(seen.get(player), [id], `${what}: acknowledged ${player} (${id}) is lost`);
  }
  return games.length;
}

test("scale_out", () =>
  Effect.gen(function* () {
    const dir = yield* scenarioDir("scale_out");
    const url = yield* database("scale_out");
    const cluster = yield* startCluster(dir, url);
    const name = apps[0];
    const t0 = Date.now();
    const phase = (what: string) => console.log(`scale_out +${Date.now() - t0} ms: ${what}`);
    phase("cluster up");
    const { tenant, key, c, ledger } = yield* Effect.promise(async () => {
      await cluster.registerSchema(name);
      const t = await cluster.tenant("scale-out");
      const c = cluster.client(functions, name, t.id, t.key);
      const ledger = new Ledger();
      await write(c, ledger, "first-use"); // resolve with create: the database is made on first use
      phase(`first use: ${JSON.stringify(ledger.failed)}`);
      assert.deepEqual(ledger.failed, []);
      return { tenant: t, key: `${t.id}:${name}`, c, ledger };
    });
    const ownerOf = () => cluster.owner(key, tenant.key, tenant.id, name);
    const other = (vm: string) => VMS.find((x) => x !== vm)!;

    // 1. Moves under load.
    yield* Effect.promise(async () => {
      let stop = false;
      const writers = [0, 1, 2, 3].map(async (w) => {
        for (let i = 0; !stop; i++) {
          await write(c, ledger, `w${w}-${i}`);
          await delay(15); // paced: the load spans the moves and stays under the check's cap
        }
      });
      await delay(400);
      let epoch = (await ownerOf()).epoch;
      for (let move = 0; move < 2; move++) {
        const from = (await ownerOf()).vm;
        phase(`move ${move} from ${from}; ${ledger.acked.size} acked`);
        const before = ledger.acked.size;
        const moved = await cluster.mustCtl(["databases", "move", "-db", key, "-to", other(from)]);
        assert.equal(moved.State ?? moved.state, "active", `move ${move}: ${JSON.stringify(moved)}`);
        const now = await ownerOf();
        assert.equal(now.vm, other(from), `move ${move}: owner`);
        assert.ok(now.epoch > epoch, `move ${move}: epoch ${now.epoch} after ${epoch}`);
        epoch = now.epoch;
        // Writers resume after the move (a request that met it re-resolves after the locator's moving hint).
        const deadline = Date.now() + 10_000;
        while (ledger.acked.size < before + 20) {
          assert.ok(Date.now() < deadline, `move ${move}: writes did not resume after it`);
          await delay(50);
        }
      }
      stop = true;
      await Promise.all(writers);
      phase("writers done");
      assert.deepEqual(ledger.failed, [], "a definite failure during the moves");
      assert.ok(ledger.acked.size >= 20, `only ${ledger.acked.size} writes acknowledged under load`);
      const n = await check(c, ledger, "after the moves");
      console.log(
        `scale_out: moves: ${ledger.acked.size} acked, ${ledger.ambiguous.size} in doubt, ${n} stored`,
      );
    });

    // 2. The locator down: running traffic goes on (cached route, ticket, edge registry, keys).
    yield* Effect.promise(() => cluster.stopControld());
    phase("controld stopped");
    yield* Effect.promise(async () => {
      for (let i = 0; i < 20; i++) await write(c, ledger, `down-${i}`);
      assert.deepEqual(ledger.failed, [], "a write failed while the locator was down");
      await check(c, ledger, "locator down");
    });
    yield* cluster.startControld();
    phase("controld restarted");

    // 3. Restore after an operator attestation.
    yield* Effect.promise(async () => {
      const dead = (await ownerOf()).vm;
      const live = other(dead);
      await cluster.backup(dead, key); // the generation the restore serves: every acknowledged write so far
      await cluster.agents.get(dead)!.process.kill();
      phase(`killed ${dead}`);
      // Without an attestation nothing moves.
      const refused = await cluster.mansioctl(["vms", "restore", "-vm", dead]);
      assert.equal(refused.code, 1, `restore before attest-dead: ${refused.stdout}`);
      assert.match(refused.stderr, /HTTP 409/);
      await cluster.mustCtl(["vms", "attest-dead", "-vm", dead, "-by", "e2e operator"]);
      const rep = await cluster.mustCtl(["vms", "restore", "-vm", dead]);
      assert.deepEqual(rep.moved, { [key]: live }, `restore: ${JSON.stringify(rep)}`);
      assert.equal((await ownerOf()).vm, live);
      // The same client: its route names the dead VM until the edge's registry drops it (then wrong_owner →
      // re-resolve). Until then the edge answers unavailable, nothing ran: a definite failure, retried here.
      const deadline = Date.now() + 15_000;
      for (let i = 0; !ledger.acked.has(`restored-${i - 1}`); i++) {
        // Every iteration (an in-doubt answer adds no failure): a write that never lands must not hang the run.
        assert.ok(Date.now() < deadline, `no write after the restore: ${ledger.failed.at(-1) ?? "in doubt"}`);
        await write(c, ledger, `restored-${i}`);
        if (ledger.failed.length > 0) {
          ledger.failed.pop();
          await delay(200);
        }
      }
      const n = await check(c, ledger, "after the restore");
      console.log(`scale_out: restored ${key} from ${dead} to ${live}; ${n} stored`);
    });
  }));
