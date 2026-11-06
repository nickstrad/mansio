import {
  apps,
  assert,
  client,
  Effect,
  OPS_PATH,
  opsQuery,
  play,
  players,
  remoteError,
  request,
  run,
  scenarioDir,
  startBackend,
  tenant,
  test,
} from "../harness.ts";
import { database } from "../postgres.ts";
import { moves } from "../seed.ts";

test(
  "e2e/4-tenant-isolation: foreign ids behave as missing and forged tenant identity confers no access",
  () =>
    Effect.gen(function* () {
      const dir = yield* scenarioDir("tenant_isolation");
      const url = yield* database("tenant_isolation");
      const server = yield* startBackend(dir, url, "api-key");
      yield* Effect.promise(async () => {
        for (const [index, app] of apps.entries()) {
          const a = await tenant(server, 3 + index * 2), b = await tenant(server, 4 + index * 2);
          const ca = client(server, app, a.key), cb = client(server, app, b.key);
          const game = await play(ca, moves.X.slice(0, 1));
          const missing = "never-existing-game";
          assert.equal(await run(cb.query("getGame", { gameId: game._id })), null);
          assert.equal(await run(cb.query("getGame", { gameId: missing })), null);
          const attemptMove = (gameId: string) =>
            remoteError(
              cb.mutation("makeMove", {
                gameId,
                playerId: players.oPlayer,
                cell: moves.X[1],
                expectedVersion: game._version,
              }),
              404,
              "NotFoundError",
            );
          assert.deepEqual(await attemptMove(game._id), await attemptMove(missing));
          const attemptDelete = (gameId: string) =>
            remoteError(cb.mutation("deleteGame", { gameId }), 404, "NotFoundError");
          assert.deepEqual(await attemptDelete(game._id), await attemptDelete(missing));
          assert.deepEqual(await run(ca.query("getGame", { gameId: game._id })), game);
          const aw = await play(ca, moves.X, "-a"), bw = await play(cb, moves.O, "-b");
          assert.deepEqual(await run(ca.query("listGames", { status: "playing" })), [game]);
          assert.deepEqual(await run(cb.query("listGames", { status: "playing" })), []);
          assert.deepEqual(await run(ca.query("listGames", { status: "won" })), [aw]);
          assert.deepEqual(await run(cb.query("listGames", { status: "won" })), [bw]);
          assert.deepEqual(await run(ca.query("winsByMark", {})), [{ value: "X", count: 1 }]);
          assert.deepEqual(await run(cb.query("winsByMark", {})), [{ value: "O", count: 1 }]);
          // B's key, claiming A's tenant in the body (db.tenant) and in a header: B's database answers.
          const forge = { "x-tenant-id": a.id };
          const forged = await request(
            server,
            OPS_PATH,
            await opsQuery(app, game._id, a.id),
            b.key,
            forge,
          );
          assert.equal(forged.status, 200, JSON.stringify(forged.body));
          assert.deepEqual(forged.body.result, [null]);
          const own = await request(
            server,
            OPS_PATH,
            await opsQuery(app, bw._id, a.id),
            b.key,
            forge,
          );
          assert.equal(own.status, 200, JSON.stringify(own.body));
          assert.deepEqual(own.body.result, [bw], "a forged tenant must still use B's database");
          const claiming = client(server, app, b.key, a.id);
          assert.equal(await run(claiming.query("getGame", { gameId: game._id })), null);
          assert.deepEqual(await run(claiming.query("getGame", { gameId: bw._id })), bw);
        }
      });
    }),
);
