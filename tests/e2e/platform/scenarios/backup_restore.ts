import {
  admin,
  apps,
  assert,
  cli,
  Effect,
  legacyClient,
  play,
  players,
  run,
  scenarioDir,
  startServer,
  test,
} from "../harness.ts";
import { database } from "../postgres.ts";
import { moves } from "../seed.ts";
import type { Manifest } from "../../../../platform/backup.ts";
async function checksum(path: string) {
  const digest = await crypto.subtle.digest("SHA-256", await Deno.readFile(path));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

test(
  "e2e/3-backup-restore: manifests, checksums and restored DuckDB reads preserve the snapshot",
  () =>
    Effect.gen(function* () {
      const dir = yield* scenarioDir("backup_restore");
      const url = yield* database("backup_restore");
      const server = yield* startServer(dir, url);
      const snapshots = yield* Effect.promise(async () => {
        const snapshots = [];
        for (const app of apps) {
          const c = legacyClient(server, app);
          const game = await play(c, moves.X.slice(0, 2));
          const manifest = await admin(server, `/admin/backup/${app}`) as Manifest;
          const latest = JSON.parse(
            await Deno.readTextFile(`${server.env.LOCAL_OBJECT_DIR}/apps/${app}/latest.json`),
          );
          assert.deepEqual(latest, manifest);
          assert.equal(await checksum(`${server.env.LOCAL_OBJECT_DIR}/${manifest.key}`), manifest.sha256);
          const advanced = await run(
            c.mutation("makeMove", {
              gameId: game._id,
              playerId: players.xPlayer,
              cell: moves.X[2],
              expectedVersion: game._version,
            }),
          );
          assert.equal(advanced!._version, game._version + 1);
          snapshots.push({ app, game, manifest, advanced });
        }
        await server.stop();
        for (const { app, manifest } of snapshots) {
          const target = `${dir}/restored/${app}.sqlite`;
          await cli(server, "restore", [app, target]);
          assert.equal(await checksum(target), manifest.sha256);
        }
        return snapshots;
      });
      const restored = yield* startServer(`${dir}/second`, url, { DATA_DIR: `${dir}/restored` });
      yield* Effect.promise(async () => {
        for (const { app, game, advanced } of snapshots) {
          const c = legacyClient(restored, app);
          const read = await run(c.query("getGame", { gameId: game._id }));
          assert.deepEqual(read, game);
          assert.notEqual(read!._version, advanced!._version);
          assert.deepEqual(await run(c.query("listGames", { status: "playing" })), [game]);
          assert.equal((await run(c.query("moveHistory", { gameId: game._id }))).length, 2);
        }
      });
    }),
  { go: "IR-4 ports durability (backups, restore) to Go" },
);
