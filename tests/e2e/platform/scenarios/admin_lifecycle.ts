import {
  apps,
  assert,
  client,
  delay,
  Effect,
  mansioctl,
  OPS_PATH,
  opsQuery,
  play,
  request,
  scenarioDir,
  startBackend,
  tenant,
  test,
} from "../harness.ts";
import { database } from "../postgres.ts";
import { moves } from "../seed.ts";

test(
  "e2e/7-admin-lifecycle: issue, revoke, deactivate and secret-safe CLI output",
  () =>
    Effect.gen(function* () {
      const dir = yield* scenarioDir("admin_lifecycle");
      const url = yield* database("admin_lifecycle");
      const server = yield* startBackend(dir, url, "api-key");
      // The agent's key cache_ttl (startGoTenancy: 1 s); its admin API invalidates synchronously too.
      const ttl = 1_000;
      yield* Effect.promise(async () => {
        for (const [index, app] of apps.entries()) {
          const first = await tenant(server, 10 + index);
          await play(client(server, app, first.key), moves.X.slice(0, 1));
          const issue = await mansioctl(server, ["tenants", "issue-key", first.id]);
          const keys = issue.stdout.match(/mansio_[A-Za-z0-9_-]+/g) ?? [];
          assert.equal(keys.length, 1);
          assert.match(issue.stderr, /Save this API key now/);
          const second = keys[0];
          await play(client(server, app, second), moves.O.slice(0, 2));
          const status = async (key: string, app: string) =>
            (await request(server, OPS_PATH, await opsQuery(app, "any-game"), key)).status;
          const refused = async (key: string) => {
            const end = Date.now() + ttl + 1_000;
            while (await status(key, app) !== 401) {
              assert.ok(Date.now() < end, "Credential still authorized beyond configured cache TTL + 1 s");
              await delay(25);
            }
          };
          const reads = [["tenants", "list"], ["tenants", "show", first.id], [
            "-format",
            "table",
            "tenants",
            "list",
          ]];
          for (const command of reads) {
            const out = await mansioctl(server, command);
            assert.ok(!out.stdout.includes(first.key) && !out.stdout.includes(second));
            assert.doesNotMatch(out.stdout + out.stderr, /mansio_[A-Za-z0-9_-]+/);
          }
          await mansioctl(server, ["tenants", "revoke-key", first.keyId]);
          await refused(first.key);
          assert.equal(await status(second, app), 200);
          await mansioctl(server, ["tenants", "deactivate", first.id]);
          await refused(first.key);
          await refused(second);
          const show = await mansioctl(server, ["-format", "table", "tenants", "show", first.id]);
          assert.match(show.stdout, /deactivated/);
          assert.doesNotMatch(show.stdout + show.stderr, /mansio_[A-Za-z0-9_-]+/);
        }
        const wrong = await mansioctl(server, ["tenants", "list"], { ADMIN_TOKEN: "wrong" }, false);
        assert.notEqual(wrong.code, 0);
        assert.match(wrong.stderr, /401/);
      });
    }),
);
