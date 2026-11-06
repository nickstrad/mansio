/** All Compose calls use this file explicitly. Never invoke the developer Compose project. */
import postgres from "npm:postgres@3.4.9";
import { Effect } from "effect";
import { assert, cleanEnv, command, drainCommands, keep } from "./harness.ts";
import { SEED } from "./seed.ts";
export const composeFile = new URL("./compose.yml", import.meta.url).pathname;
export const project = `mansio-e2e-${Deno.pid}-${String(SEED).replace("-", "n")}`;
export async function compose(args: string[], signal?: AbortSignal) {
  const out = await command(
    "docker",
    ["compose", "-f", composeFile, "-p", project, ...args],
    cleanEnv(),
    60_000,
    signal,
  );
  assert.equal(out.code, 0, `Docker Compose ${args[0]} failed: ${out.stderr}`);
  return out.stdout.trim();
}
export async function docker(args: string[], signal?: AbortSignal) {
  const out = await command("docker", args, cleanEnv(), 60_000, signal);
  assert.equal(out.code, 0, `Docker dependency unavailable: ${out.stderr}`);
  return out.stdout.trim().split("\n").filter(Boolean).sort();
}
export function startPostgres() {
  return Effect.gen(function* () {
    const source = yield* Effect.promise(() => Deno.readTextFile("docker-compose.yml"));
    const isolated = yield* Effect.promise(() => Deno.readTextFile(composeFile));
    const image = (text: string) => text.match(/^ {2}postgres:\s*\n\s+image:\s*(\S+)/m)?.[1];
    assert.ok(image(source), "Cannot identify root Postgres image");
    assert.equal(image(isolated), image(source), "E2E Postgres image drift");
    // Register cleanup BEFORE up, including partial startup and failed health checks.
    yield* Effect.acquireRelease(Effect.void, () =>
      Effect.promise(async () => {
        await drainCommands();
        if (!keep) await compose(["down", "--remove-orphans", "--volumes", "--timeout", "10"]);
      }));
    yield* Effect.promise(async (signal) => {
      try {
        await docker(["info", "--format", "{{.ServerVersion}}"], signal);
      } catch (error) {
        throw new Error(`Docker is required and must be healthy within 60 s: ${error}`);
      }
      await compose(["up", "-d", "--wait", "--wait-timeout", "60"], signal);
    });
    const address = yield* Effect.promise(() => compose(["port", "postgres", "5432"]));
    assert.match(address, /^127\.0\.0\.1:\d+$/);
    return `postgres://mansio:mansio@${address}/postgres`;
  });
}
export function database(name: string) {
  return Effect.gen(function* () {
    assert.match(name, /^[a-z_]+$/);
    const base = Deno.env.get("E2E_POSTGRES_URL");
    assert.ok(base, "Missing E2E_POSTGRES_URL");
    const sql = yield* Effect.acquireRelease(
      Effect.sync(() => postgres(base, { max: 1, onnotice() {} })),
      (sql) => Effect.promise(() => sql.end()),
    );
    yield* Effect.promise(() => sql.unsafe(`CREATE DATABASE "${name}"`));
    const url = new URL(base);
    url.pathname = "/" + name;
    const result = yield* Effect.promise(() =>
      command(Deno.execPath(), ["run", "-A", "scripts/db/migrate.ts"], {
        ...cleanEnv(),
        DATABASE_URL: url.href,
      })
    );
    assert.equal(result.code, 0, result.stderr);
    // MT-08 is open: the normal migration runner must actually install its staged artifacts.
    if (name === "auth") {
      const auth = yield* Effect.acquireRelease(
        Effect.sync(() => postgres(url.href, { max: 1, onnotice() {} })),
        (sql) => Effect.promise(() => sql.end()),
      );
      const rows = yield* Effect.promise(() => auth`SELECT to_regclass('public.session') AS name`);
      assert.equal(rows[0].name, "session", "MT-08 migration was not applied by scripts/db/migrate.ts");
    }
    return url.href;
  });
}
