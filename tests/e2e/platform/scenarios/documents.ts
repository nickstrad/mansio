import {
  apps,
  assert,
  client,
  Effect,
  players,
  remoteError,
  run,
  scenarioDir,
  startBackend,
  tenant,
  test,
} from "../harness.ts";
import { database } from "../postgres.ts";
import { moves } from "../seed.ts";

test("e2e/1-documents: typed CRUD, versions and argument errors in both layouts and auth modes (SDK, /v1/ops)", () =>
  Effect.gen(function* () {
    const dir = yield* scenarioDir("documents");
    const url = yield* database("documents");
    // On Go the off-mode half uses minted tickets, the api-key half tenant keys (IR-4a).
    for (const mode of ["off", "api-key"]) {
      yield* Effect.scoped(Effect.gen(function* () {
        const server = yield* startBackend(`${dir}/${mode}`, url, { TENANCY: mode });
        yield* Effect.promise(async () => {
          for (const [index, app] of apps.entries()) {
            // A tenant file has one layout; exercise each app with a fresh tenant.
            const key = mode === "api-key" ? (await tenant(server, 2 + index)).key : undefined;
            const c = client(server, app, key);
            const { gameId } = await run(c.mutation("createGame", players));
            let game = await run(c.query("getGame", { gameId }));
            assert.ok(game);
            assert.equal(game._version, 1);
            assert.equal(typeof game._creationTime, "number");
            for (const [i, cell] of moves.X.slice(0, 3).entries()) {
              const written: import("../../../../app/rules.ts").Game | null = await run(
                c.mutation("makeMove", {
                  gameId,
                  playerId: i % 2 === 0 ? players.xPlayer : players.oPlayer,
                  cell,
                  expectedVersion: game._version,
                }),
              );
              game = await run(c.query("getGame", { gameId }));
              assert.deepEqual(game, written);
              assert.ok(game);
              assert.equal(game._version, i + 2);
            }
            await remoteError(
              c.mutation("makeMove", {
                gameId,
                playerId: players.oPlayer,
                cell: moves.X[3],
                expectedVersion: 1,
              }),
              409,
              "ConflictError",
            );
            // Deliberately cross the public argument boundary with an invalid scalar.
            await remoteError(
              c.mutation("createGame", { ...players, xPlayer: 42 as unknown as string }),
              400,
              "SchemaError",
            );
            assert.deepEqual(await run(c.mutation("deleteGame", { gameId })), { deleted: true });
            assert.equal(await run(c.query("getGame", { gameId })), null);
          }
        });
      }));
    }
  }));
