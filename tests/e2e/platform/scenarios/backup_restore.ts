import {
  admin,
  apps,
  assert,
  backend,
  cli,
  client,
  Effect,
  legacyClient,
  play,
  players,
  run,
  scenarioDir,
  startServer,
  test,
} from "../harness.ts";
import { type Backup, dbtool, keyOf, startDurableAgent } from "../durable.ts";
import { database } from "../postgres.ts";
import { moves } from "../seed.ts";
import type { Game } from "../../../../app/rules.ts";
import type { Manifest } from "../../../../platform/backup.ts";
async function checksum(path: string) {
  const digest = await crypto.subtle.digest("SHA-256", await Deno.readFile(path));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** Deno: `/admin/backup/:appId` writes a snapshot object and `latest.json`; `scripts/restore.ts` restores it. */
const onDeno = () =>
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
  });

/**
 * Go (IR-4b), over generations: `/admin/backup?key=` forces a write-back and answers the manifest; the
 * shutdown writes back what came after; `dbtool restore -gen` brings the backup's generation back (its
 * sha256), without `-gen` the newer one; `dbtool import` makes the restored file a new agent's database, and
 * a Deno-written file a third's: both open with no schema drift and serve the snapshot.
 */
const onGo = () =>
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

    // A file the Deno platform wrote (<DATA_DIR>/<app>.sqlite) imports with no drift and takes writes.
    const url = yield* database("backup_restore");
    const deno = yield* startServer(`${dir}/deno`, url);
    const written = yield* Effect.promise(async () => {
      const written: { app: string; game: Game }[] = [];
      for (const app of apps) {
        written.push({ app, game: await play(legacyClient(deno, app), moves.X.slice(0, 3)) });
      }
      await deno.stop();
      return written;
    });
    const third = yield* startDurableAgent(`${dir}/third`);
    yield* Effect.promise(async () => {
      for (const { app, game } of written) {
        await third.importFile(app, `${deno.env.DATA_DIR}/${app}.sqlite`);
        await third.claim(app, false);
        const c = client(third, app);
        assert.deepEqual(await run(c.query("getGame", { gameId: game._id })), game);
        assert.equal((await run(c.query("moveHistory", { gameId: game._id }))).length, 3);
        const next = await run(
          c.mutation("makeMove", {
            gameId: game._id,
            playerId: players.oPlayer,
            cell: moves.X[3],
            expectedVersion: game._version,
          }),
        );
        assert.equal(next!._version, game._version + 1);
      }
      await third.stop();
    });
  });

test(
  "e2e/3-backup-restore: manifests, checksums and restored DuckDB reads preserve the snapshot",
  () => backend === "go" ? onGo() : onDeno(),
);
