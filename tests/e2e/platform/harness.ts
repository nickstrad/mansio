/** Owned process groups, bounded commands and clean environments shared by all scenarios. */
import assert from "node:assert/strict";
import { kill } from "node:process";
import { resolve } from "node:path";
import { Effect, Either, Exit, type Scope } from "effect";
import { OPTIONS } from "../../../platform/config.ts";
import { createClient, createTenantClient } from "../../../sdk/client.ts";
import type { functions } from "../../../app/functions.ts";
import type { ClientError } from "../../../core/errors.ts";
import { names, players, titled } from "./seed.ts";
export { assert, Effect, players };
export const apps = ["tic-tac-toe", "tic-tac-toe-json"] as const;
export const keep = Deno.env.get("E2E_KEEP") === "1";
export const root = () => {
  const value = Deno.env.get("E2E_ROOT");
  assert.ok(value, "Run via deno task e2e; E2E_ROOT is required");
  return value;
};
export const cleanEnv = (): Record<string, string> => {
  const env: Record<string, string> = {};
  for (const key of ["PATH", "HOME", "DENO_DIR"]) {
    const value = Deno.env.get(key);
    if (value !== undefined) env[key] = value;
  }
  assert.ok(env.HOME, "HOME must be set for DuckDB extensions");
  return env;
};
export const delay = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));
export async function exists(path: string): Promise<boolean> {
  try {
    await Deno.stat(path);
    return true;
  } catch (e) {
    if (e instanceof Deno.errors.NotFound) return false;
    throw e;
  }
}
export async function groupPids(group: number): Promise<number[]> {
  const pids: number[] = [];
  for await (const entry of Deno.readDir("/proc")) {
    if (!/^\d+$/.test(entry.name)) continue;
    try {
      const stat = await Deno.readTextFile(`/proc/${entry.name}/stat`);
      const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
      if (Number(fields[2]) === group && fields[0] !== "Z") pids.push(Number(entry.name));
    } catch (e) {
      if (!(e instanceof Deno.errors.NotFound)) throw e;
    }
  }
  return pids;
}
async function waitEmpty(group: number, ms: number): Promise<boolean> {
  const end = Date.now() + ms;
  while ((await groupPids(group)).length) {
    if (Date.now() >= end) return false;
    await delay(50);
  }
  return true;
}
export async function stopGroup(group: number): Promise<void> {
  if (!(await groupPids(group)).length) return;
  // Let the server close the reader's stdin and await it before terminating the group.
  try {
    Deno.kill(group, "SIGTERM");
  } catch (e) {
    if (!(e instanceof Deno.errors.NotFound)) throw e;
  }
  if (await waitEmpty(group, 8_000)) return;
  try {
    kill(-group, "SIGTERM");
  } catch { /* group exited */ }
  if (await waitEmpty(group, 2_000)) return;
  try {
    kill(-group, "SIGKILL");
  } catch { /* group exited */ }
  assert.ok(await waitEmpty(group, 2_000), `Process group ${group} survived teardown`);
}
export function processResource(args: string[], env: Record<string, string>, options: {
  command?: string;
  inherit?: boolean;
  trackRoot?: string;
  preserve?: boolean;
} = {}) {
  return Effect.uninterruptible(Effect.gen(function* () {
    const child = yield* Effect.acquireRelease(
      Effect.sync(() =>
        new Deno.Command("setsid", {
          args: [options.command ?? Deno.execPath(), ...args],
          env,
          clearEnv: true,
          stdin: "null",
          stdout: options.inherit ? "inherit" : "piped",
          stderr: options.inherit ? "inherit" : "piped",
        }).spawn()
      ),
      (child) =>
        Effect.promise(async () => {
          if (keep && options.preserve) return;
          await stopGroup(child.pid);
          await child.status;
        }),
    );
    // Synchronous registration inside the acquisition mask, after installing the finalizer.
    const ledger = options.trackRoot ?? Deno.env.get("E2E_ROOT");
    if (ledger) Deno.writeTextFileSync(`${ledger}/pids/${child.pid}`, String(child.pid));
    return child;
  }));
}
const activeCommands = new Set<Promise<unknown>>();
export async function drainCommands() {
  await Promise.allSettled([...activeCommands]);
}

export async function command(
  command: string,
  args: string[],
  env = cleanEnv(),
  timeout = 60_000,
  signal: AbortSignal = abort.signal,
) {
  const pending = Effect.runPromise(
    Effect.scoped(Effect.gen(function* () {
      const child = yield* processResource(args, env, { command });
      let timer: ReturnType<typeof setTimeout> | undefined;
      try {
        const out = yield* Effect.promise(() =>
          Promise.race([
            child.output(),
            new Promise<never>((_, reject) => {
              timer = setTimeout(() => reject(new Error(`${command} timed out`)), timeout);
            }),
          ])
        );
        const decode = (b: Uint8Array) => new TextDecoder().decode(b);
        return { code: out.code, stdout: decode(out.stdout), stderr: decode(out.stderr) };
      } finally {
        clearTimeout(timer);
      }
    })),
    { signal },
  );
  activeCommands.add(pending);
  try {
    return await pending;
  } finally {
    activeCommands.delete(pending);
  }
}

export function scenarioDir(name: string) {
  return Effect.acquireRelease(
    Effect.promise(async () => {
      const dir = resolve(root(), name);
      await Deno.mkdir(dir, { recursive: true });
      return dir;
    }),
    (dir) =>
      Effect.promise(async () => {
        if (!keep) await Deno.remove(dir, { recursive: true });
      }),
  );
}
export function startServer(dir: string, databaseUrl: string, extra: Record<string, string> = {}) {
  return Effect.gen(function* () {
    const probe = Deno.listen({ hostname: "127.0.0.1", port: 0 });
    const port = (probe.addr as Deno.NetAddr).port;
    probe.close();
    const base = `http://127.0.0.1:${port}`;
    const options: Record<string, string> = {
      PORT: String(port),
      BIND_HOST: "127.0.0.1",
      DATA_DIR: `${dir}/data`,
      LOG_DIR: `${dir}/logs`,
      LOCAL_OBJECT_DIR: `${dir}/objects`,
      EVENTLOG_DUCKDB: `${dir}/eventlog.duckdb`,
      S3_BASE_URL: "",
      BACKUP_INTERVAL_MS: "0",
      ADMIN_TOKEN: crypto.randomUUID(),
      COLUMN_APP_TOKEN: crypto.randomUUID(),
      JSON_APP_TOKEN: crypto.randomUUID(),
      TENANCY: "off",
      DATABASE_URL: databaseUrl,
      TENANTS_DIR: `${dir}/tenants`,
      BETTER_AUTH_SECRET: crypto.randomUUID() + crypto.randomUUID(),
      BETTER_AUTH_URL: base,
      ...extra,
    };
    const allowed = new Set<string>(OPTIONS.map((o) => o.envVar));
    for (const key of Object.keys(options)) assert.ok(allowed.has(key), `Unknown server option ${key}`);
    const env = { ...cleanEnv(), ...options };
    const child = yield* processResource(["run", "-A", "platform/server.ts"], env, {
      trackRoot: root(),
      preserve: true,
    });
    let exited = false;
    const output = child.output(); // Drain both streams throughout startup and requests.
    yield* Effect.addFinalizer((exit) =>
      Effect.promise(async () => {
        if (keep) return;
        await stopGroup(child.pid);
        const out = await output;
        if (Exit.isFailure(exit)) {
          const stderr = new TextDecoder().decode(out.stderr);
          if (stderr.trim()) console.error(`Server diagnostics: ${stderr.slice(-4000)}`);
          if (await exists(`${env.LOG_DIR}/server`)) {
            for await (const date of Deno.readDir(`${env.LOG_DIR}/server`)) {
              if (!date.isDirectory) continue;
              for await (const file of Deno.readDir(`${env.LOG_DIR}/server/${date.name}`)) {
                if (!file.name.endsWith(".jsonl")) continue;
                for (
                  const line of (await Deno.readTextFile(`${env.LOG_DIR}/server/${date.name}/${file.name}`))
                    .trim().split("\n")
                ) {
                  const row = JSON.parse(line);
                  if (row.errorTag === "StorageError" || row.errorTag === "ReaderError") {
                    console.error(
                      `Server error: ${
                        JSON.stringify({
                          tag: row.errorTag,
                          detail: row.errorDetail,
                          name: row.name,
                          status: row.status,
                        })
                      }`,
                    );
                  }
                }
              }
            }
          }
        }
      })
    );
    void child.status.then(() => {
      exited = true;
    });
    const deadline = Date.now() + 30_000;
    while (true) {
      if (exited) {
        const out = yield* Effect.promise(() => output);
        throw new Error(`Server exited ${out.code}: ${new TextDecoder().decode(out.stderr)}`);
      }
      const healthy = yield* Effect.promise(async () => {
        try {
          const response = await fetch(`${base}/health`, { signal: AbortSignal.timeout(500) });
          await response.body?.cancel();
          return response.ok;
        } catch {
          return false;
        }
      });
      if (healthy) break;
      assert.ok(Date.now() < deadline, "Server health timeout");
      yield* Effect.sleep("50 millis");
    }
    return {
      base,
      env,
      dir,
      stop: async () => {
        await stopGroup(child.pid);
        const out = await output;
        assert.equal(out.code, 0, `Server shutdown failed: ${new TextDecoder().decode(out.stderr)}`);
      },
    };
  });
}
export type Server = Effect.Effect.Success<ReturnType<typeof startServer>>;
export type Client = ReturnType<typeof createClient<typeof functions>>;
export function client(server: Server, app: string = apps[0], token?: string): Client {
  return (token ? createTenantClient : createClient)<typeof functions>(
    server.base,
    app,
    token ?? server.env[app === apps[0] ? "COLUMN_APP_TOKEN" : "JSON_APP_TOKEN"],
  );
}
export const run = Effect.runPromise;
export async function remoteError<A>(effect: Effect.Effect<A, ClientError>, status: number, tag: string) {
  const result = await run(Effect.either(effect));
  assert.ok(Either.isLeft(result), `Expected ${status} ${tag}`);
  assert.equal(result.left._tag, "RemoteError");
  assert.equal(result.left.status, status);
  assert.equal(result.left.remoteTag, tag);
  return { status: result.left.status, tag: result.left.remoteTag };
}
export async function request(
  server: Server,
  path: string,
  body: unknown = {},
  token = server.env.ADMIN_TOKEN,
  headers: Record<string, string> = {},
) {
  const response = await fetch(server.base + path, {
    method: "POST",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json", ...headers },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(10_000),
  });
  return { status: response.status, body: await response.json() };
}
export async function admin(server: Server, path: string, body: unknown = {}) {
  const response = await request(server, path, body);
  assert.equal(response.status, 200, `${path}: ${JSON.stringify(response.body)}`);
  return response.body.result;
}
export async function cli(
  server: Server,
  script: string,
  args: string[],
  extra: Record<string, string> = {},
  success = true,
) {
  const out = await command(Deno.execPath(), ["run", "-A", `scripts/${script}.ts`, ...args], {
    ...server.env,
    BASE_URL: server.base,
    ...extra,
  });
  if (success) assert.equal(out.code, 0, `${script} ${args[0]}: ${out.stderr}`);
  return out;
}
export async function tenant(server: Server, index: number) {
  const out = await cli(server, "tenants", ["create", names[index], "--slug", names[index]]);
  const keys = out.stdout.match(/mansio_[A-Za-z0-9_-]+/g) ?? [];
  assert.equal(keys.length, 1, "create must print the plaintext key exactly once");
  assert.match(out.stdout, /Save this API key now/);
  const ids = out.stdout.match(/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}/g)!;
  assert.ok(ids?.length >= 3, "tenant and key metadata must be printed");
  return { id: ids[0], keyId: ids[1], key: keys[0] };
}
export async function play(c: Client, cells: readonly number[], suffix = "") {
  const people = { xPlayer: players.xPlayer + suffix, oPlayer: players.oPlayer + suffix };
  const { gameId } = await run(c.mutation("createGame", people));
  let game = await run(c.query("getGame", { gameId }));
  assert.ok(game);
  for (const [i, cell] of cells.entries()) {
    const written: import("../../../app/rules.ts").Game | null = await run(c.mutation("makeMove", {
      gameId,
      playerId: i % 2 === 0 ? people.xPlayer : people.oPlayer,
      cell,
      expectedVersion: game._version,
    }));
    game = await run(c.query("getGame", { gameId }));
    assert.deepEqual(game, written, "read-after-write through DuckDB");
    assert.ok(game);
  }
  return game;
}
const abort = new AbortController();
let signals = false;
export function test(name: string, body: () => Effect.Effect<void, unknown, Scope.Scope>) {
  if (!signals) {
    for (const signal of ["SIGINT", "SIGTERM"] as const) Deno.addSignalListener(signal, () => abort.abort());
    signals = true;
  }
  Deno.test(titled(name), async () => {
    abort.signal.throwIfAborted();
    await Effect.runPromise(Effect.scoped(body()), { signal: abort.signal });
  });
}
