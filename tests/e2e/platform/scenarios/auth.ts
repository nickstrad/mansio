import {
  admin,
  apps,
  assert,
  backend,
  cli,
  client,
  Effect,
  exists,
  opsPath,
  opsQuery,
  play,
  request,
  run,
  scenarioDir,
  type Server,
  startTenancy,
  tenant,
  test,
} from "../harness.ts";
import { database } from "../postgres.ts";
import { moves, names } from "../seed.ts";

/** A Better Auth email/password call as plain HTTP (what its client sends). */
async function authCall(server: Server, path: string, body: Record<string, string>) {
  const response = await fetch(`${server.base}/api/auth/${path}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(10_000),
  });
  return { status: response.status, body: await response.json() };
}

/** `scripts/auth.ts users link` on Deno (it writes Postgres directly); the admin API on Go. */
async function link(server: Server, email: string, tenantId: string) {
  if (backend === "go") await admin(server, "/admin/users/link", { email, tenantId });
  else await cli(server, "auth", ["users", "link", email, tenantId]);
}

test(
  "e2e/5-auth: 5a app-token and API-key refusals and 5b headless email/password tenant sessions",
  () =>
    Effect.gen(function* () {
      const dir = yield* scenarioDir("auth");
      const url = yield* database("auth");
      yield* Effect.scoped(Effect.gen(function* () {
        const server = yield* startTenancy(`${dir}/off`, url, "off");
        yield* Effect.promise(async () => {
          for (const [i, app] of apps.entries()) {
            const wrong = server.env[i === 0 ? "JSON_APP_TOKEN" : "COLUMN_APP_TOKEN"];
            const response = await request(server, opsPath(server, app), await opsQuery(app, "x"), wrong);
            assert.equal(response.status, 401, "tokens must be app-specific in TENANCY=off");
            assert.equal(response.body.error.tag, "AuthError");
            const right = server.env[i === 0 ? "COLUMN_APP_TOKEN" : "JSON_APP_TOKEN"];
            assert.equal(
              (await request(server, opsPath(server, app), await opsQuery(app, "x"), right)).status,
              200,
            );
          }
        });
      }));
      yield* Effect.scoped(Effect.gen(function* () {
        const server = yield* startTenancy(`${dir}/keys`, url, "api-key");
        yield* Effect.promise(async () => {
          for (const path of [opsPath(server, apps[0]), "/admin/logs/ingest"]) {
            const body = path.startsWith("/admin") ? {} : await opsQuery(apps[0], "x");
            for (
              const authorization of [
                undefined,
                "Bearer",
                "Bearer mansio_unknown",
                "Bearer wrong-admin-token",
              ]
            ) {
              const response = await fetch(server.base + path, {
                method: "POST",
                headers: { "content-type": "application/json", ...(authorization ? { authorization } : {}) },
                body: JSON.stringify(body),
                signal: AbortSignal.timeout(10_000),
              });
              const answer = await response.json();
              assert.equal(response.status, 401, `${path}: ${JSON.stringify(answer)}`);
              assert.equal(answer.error.tag, "AuthError");
            }
          }
        });
      }));
      assert.ok(yield* Effect.promise(() => exists("scripts/auth.ts")), "MT-08 auth gate must remain open");
      assert.ok(yield* Effect.promise(() => exists("platform/auth")), "MT-08 auth gate must remain open");
      // Fresh processes also isolate Better Auth's shared per-path rate-limit bucket.
      for (const [index, app] of apps.entries()) {
        yield* Effect.scoped(Effect.gen(function* () {
          const server = yield* startTenancy(`${dir}/sessions-${index}`, url, "better-auth");
          yield* Effect.promise(async () => {
            const a = await tenant(server, 5 + index * 2), b = await tenant(server, 6 + index * 2);
            const email = `${names[7]}-${index}@example.test`, password = "E2e-password-" + names[8];
            const signup = await authCall(server, "sign-up/email", { name: names[7], email, password });
            assert.equal(signup.status, 200, JSON.stringify(signup.body));
            assert.ok(signup.body.token, "headless sign-up must capture a token");
            await link(server, email, a.id);
            const signin = await authCall(server, "sign-in/email", { email, password });
            assert.equal(signin.status, 200, JSON.stringify(signin.body));
            assert.ok(signin.body.token, "headless sign-in must capture a token");
            const gameA = await play(client(server, app, a.key), moves.X.slice(0, 1));
            const gameB = await play(client(server, app, b.key), moves.O.slice(0, 2));
            for (const token of [signup.body.token, signin.body.token]) {
              const session = client(server, app, token);
              assert.deepEqual(await run(session.query("getGame", { gameId: gameA._id })), gameA);
              assert.equal(await run(session.query("getGame", { gameId: gameB._id })), null);
              assert.deepEqual(await run(session.query("listGames", { status: "playing" })), [gameA]);
            }
            const wrong = await authCall(server, "sign-in/email", { email, password: password + "-wrong" });
            assert.equal(wrong.status, 401, "wrong password");
            const unknown = await authCall(server, "sign-in/email", { email: `unknown-${email}`, password });
            assert.equal(unknown.status, 401, "unknown user");
          });
        }));
      }
    }),
);
