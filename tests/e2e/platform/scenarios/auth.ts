import { createAuthClient } from "npm:better-auth@1.7.7/client";
import {
  apps,
  assert,
  cli,
  Effect,
  exists,
  legacyClient,
  play,
  request,
  run,
  scenarioDir,
  startServer,
  tenant,
  test,
} from "../harness.ts";
import { database } from "../postgres.ts";
import { moves, names } from "../seed.ts";

test(
  "e2e/5-auth: 5a API-key refusals and 5b headless Better Auth tenant sessions",
  () =>
    Effect.gen(function* () {
      const dir = yield* scenarioDir("auth");
      const url = yield* database("auth");
      yield* Effect.scoped(Effect.gen(function* () {
        const server = yield* startServer(`${dir}/off`, url);
        yield* Effect.promise(async () => {
          for (const [i, app] of apps.entries()) {
            const wrong = server.env[i === 0 ? "JSON_APP_TOKEN" : "COLUMN_APP_TOKEN"];
            const response = await request(
              server,
              `/apps/${app}/query/listGames`,
              { status: "playing" },
              wrong,
            );
            assert.equal(response.status, 401, "tokens must be app-specific in TENANCY=off");
          }
        });
      }));
      yield* Effect.scoped(Effect.gen(function* () {
        const server = yield* startServer(`${dir}/keys`, url, { TENANCY: "api-key" });
        yield* Effect.promise(async () => {
          for (const path of ["/apps/tic-tac-toe/query/listGames", "/admin/logs/ingest"]) {
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
                body: JSON.stringify({ status: "playing" }),
                signal: AbortSignal.timeout(10_000),
              });
              const body = await response.json();
              assert.equal(response.status, 401, `${path}: ${JSON.stringify(body)}`);
              assert.equal(body.error.tag, "AuthError");
            }
          }
        });
      }));
      assert.ok(yield* Effect.promise(() => exists("scripts/auth.ts")), "MT-08 auth gate must remain open");
      assert.ok(yield* Effect.promise(() => exists("platform/auth")), "MT-08 auth gate must remain open");
      // Fresh processes also isolate Better Auth's shared per-path rate-limit bucket.
      for (const [index, app] of apps.entries()) {
        yield* Effect.scoped(Effect.gen(function* () {
          const server = yield* startServer(`${dir}/sessions-${index}`, url, { TENANCY: "better-auth" });
          yield* Effect.promise(async () => {
            const a = await tenant(server, 5 + index * 2), b = await tenant(server, 6 + index * 2);
            const auth = createAuthClient({ baseURL: server.base });
            const email = `${names[7]}-${index}@example.test`, password = "E2e-password-" + names[8];
            const signup = await auth.signUp.email({ name: names[7], email, password });
            assert.equal(signup.error, null, JSON.stringify(signup.error));
            assert.ok(signup.data?.token, "headless sign-up must capture a token");
            await cli(server, "auth", ["users", "link", email, a.id]);
            const signin = await auth.signIn.email({ email, password });
            assert.equal(signin.error, null, JSON.stringify(signin.error));
            assert.ok(signin.data?.token, "headless sign-in must capture a token");
            const gameA = await play(legacyClient(server, app, a.key), moves.X.slice(0, 1));
            const gameB = await play(legacyClient(server, app, b.key), moves.O.slice(0, 2));
            for (const token of [signup.data.token, signin.data.token]) {
              const session = legacyClient(server, app, token);
              assert.deepEqual(await run(session.query("getGame", { gameId: gameA._id })), gameA);
              assert.equal(await run(session.query("getGame", { gameId: gameB._id })), null);
              assert.deepEqual(await run(session.query("listGames", { status: "playing" })), [gameA]);
            }
            const wrong = await auth.signIn.email({ email, password: password + "-wrong" });
            assert.equal(wrong.error?.status, 401, "wrong password");
            const unknown = await auth.signIn.email({ email: `unknown-${email}`, password });
            assert.equal(unknown.error?.status, 401, "unknown user");
          });
        }));
      }
    }),
  { go: "IR-4 ports API keys and sessions to Go" },
);
