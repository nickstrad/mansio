import { apps, assert, client, Effect, play, players, run, scenarioDir, test } from "../harness.ts";
import { type Backup, dbtool, keyOf, startDurableAgent } from "../durable.ts";
import { moves } from "../seed.ts";
import type { Game } from "../../../../app/rules.ts";
async function checksum(path: string) {
  const digest = await crypto.subtle.digest("SHA-256", await Deno.readFile(path));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/**
 * Backups over generations (IR-4b): `/admin/backup?key=` forces a write-back and answers the manifest; the
 * shutdown writes back what came after; `dbtool restore -gen` brings the backup's generation back (its
 * sha256), without `-gen` the newer one; `dbtool import` makes the restored file a new agent's database,
 * which opens with no schema drift and serves the snapshot.
 */
const durable = () =>
  Effect.gen(function* () {
    const dir = yield* scenarioDir("backup_restore");
    const first = yield* startDurableAgent(`${dir}/first`);
    const snapshots = yield* Effect.promise(async () => {
      const snapshots: { app: string; game: Game; backup: Backup; advanced: Game }[] = [];
      for (const app of apps) {
        await first.claim(app, true);
        const c = client(first, app);
        const game = await play(c, moves.X.slice(0, 2));
        const backup = await first.backup(app);
        assert.equal(backup.key, keyOf(app));
        assert.ok(backup.generation >= 1 && backup.wrote, JSON.stringify(backup));
        const verified = await dbtool(first.store, ["verify", keyOf(app)]);
        assert.match(
          verified.stdout,
          new RegExp(`generation ${backup.generation} .*sha256 ${backup.sha256}`),
        );
        const advanced = await run(
          c.mutation("makeMove", {
            gameId: game._id,
            playerId: players.xPlayer,
            cell: moves.X[2],
            expectedVersion: game._version,
          }),
        );
        assert.equal(advanced!._version, game._version + 1);
        snapshots.push({ app, game, backup, advanced: advanced! });
      }
      await first.stop(); // writes back the advanced moves
      for (const { app, backup } of snapshots) {
        const target = `${dir}/restored/${app}.db`;
        const out = await dbtool(first.store, [
          "restore",
          "-gen",
          String(backup.generation),
          keyOf(app),
          target,
        ]);
        assert.equal(JSON.parse(out.stdout).generation, backup.generation);
        assert.equal(await checksum(target), backup.sha256);
        const latest = JSON.parse(
          (await dbtool(first.store, ["restore", keyOf(app), `${dir}/latest/${app}.db`])).stdout,
        );
        assert.ok(
          latest.generation > backup.generation,
          "the shutdown published the move made after the backup",
        );
        const again = await dbtool(first.store, ["restore", keyOf(app), target], false);
        assert.equal(again.code, 1, "restore never overwrites");
      }
      return snapshots;
    });

    const second = yield* startDurableAgent(`${dir}/second`);
    yield* Effect.promise(async () => {
      for (const { app, game, advanced } of snapshots) {
        const imported = await second.importFile(app, `${dir}/restored/${app}.db`);
        assert.equal(imported.manifest.generation, 1);
        assert.deepEqual(imported.placement, { owner: "vm-local", epoch: 1, gen: 1 });
        await second.claim(app, false);
        const c = client(second, app);
        const read = await run(c.query("getGame", { gameId: game._id }));
        assert.deepEqual(read, game);
        assert.notEqual(read!._version, advanced._version);
        assert.deepEqual(await run(c.query("listGames", { status: "playing" })), [game]);
        assert.equal((await run(c.query("moveHistory", { gameId: game._id }))).length, 2);
      }
      await second.stop();
    });
  });

test(
  "e2e/3-backup-restore: manifests, checksums and restored DuckDB reads preserve the snapshot",
  durable,
);
