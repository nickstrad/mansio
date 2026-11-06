/** Owned process groups, bounded commands and clean environments shared by all scenarios. */
import assert from "node:assert/strict";
import { kill } from "node:process";
import { resolve } from "node:path";
import { Effect, Either, Exit, type Scope } from "effect";
import { OPTIONS } from "../../../platform/config.ts";
import { createClient, createLegacyClient, schemaFingerprint } from "../../../sdk/client.ts";
import postgres from "npm:postgres@3.4.9";
import { functions } from "../../../app/functions.ts";
import { schema } from "../../../app/schema.ts";
import { withStorage } from "../../../sdk/schema.ts";
import { generateIssuer, mintTicket, PERM_READ, PERM_WRITE } from "../../../sdk/ticket.ts";
import type { ClientError } from "../../../core/errors.ts";
import { names, players, titled } from "./seed.ts";
export { assert, Effect, players };
export const apps = ["tic-tac-toe", "tic-tac-toe-json"] as const;
/** The storage layout each app is configured with (`platform/config.ts`). */
export const layoutOf = (app: string) => app === "tic-tac-toe-json" ? "json" as const : "columns" as const;
/**
 * Which backend the scenarios run against: `deno` (the platform server; app handlers through the SDK's
 * `/v1/ops` shim) or `go` (agentd in single-node static mode). `BACKEND=go deno task e2e`.
 */
export const backend: "deno" | "go" = (() => {
  const value = Deno.env.get("BACKEND") ?? "deno";
  assert.ok(value === "deno" || value === "go", `BACKEND must be deno or go, got ${value}`);
  return value;
})();
/** The tenant SDK clients name. The Deno shim ignores it (credentials decide); agentd serves `local:<app>`. */
export const TENANT = "local";
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
  cwd?: string;
} = {}) {
  return Effect.uninterruptible(Effect.gen(function* () {
    const child = yield* Effect.acquireRelease(
      Effect.sync(() =>
        new Deno.Command("setsid", {
          args: [options.command ?? Deno.execPath(), ...args],
          env,
          ...(options.cwd ? { cwd: options.cwd } : {}),
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
  cwd?: string,
) {
  const pending = Effect.runPromise(
    Effect.scoped(Effect.gen(function* () {
      const child = yield* processResource(args, env, { command, cwd });
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
      backend: "deno" as const,
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
/** Where a Go agent is: the built `agentd` (`AGENTD_BIN`, else `go build` into the run's root once). */
let agentd: Promise<string> | undefined;
function agentdBinary(): Promise<string> {
  return agentd ??= (async () => {
    const given = Deno.env.get("AGENTD_BIN");
    if (given) return given;
    const out = `${root()}/agentd`;
    const built = await command(
      "go",
      ["build", "-o", out, "./cmd/agentd"],
      {
        ...cleanEnv(),
        ...Object.fromEntries(
          ["GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS"].flatMap((k) => {
            const v = Deno.env.get(k);
            return v === undefined ? [] : [[k, v]];
          }),
        ),
      },
      300_000,
      undefined,
      "server",
    );
    assert.equal(built.code, 0, `go build agentd: ${built.stderr}`);
    return out;
  })();
}

/**
 * agentd in single-node static mode for both apps (IR-3): a fresh Ed25519 issuer (`keys_file`), an empty
 * `local:<app>.db` per app in `db_dir` (static mode serves only files it finds at start), and each app's
 * schema in its layout under `schemas_dir/<app>/v1.json`, so the first request creates the tables. Tickets
 * are minted per request kind by `mint`. `extra` is merged into the agent config.
 */
export function startGoAgent(dir: string, extra: Record<string, unknown> = {}) {
  return Effect.gen(function* () {
    const bin = yield* Effect.promise(agentdBinary);
    const issuer = yield* Effect.promise(async () => {
      for (const sub of ["dbs", "keys", "logs"]) await Deno.mkdir(`${dir}/${sub}`, { recursive: true });
      for (const app of apps) {
        await Deno.writeFile(`${dir}/dbs/${TENANT}:${app}.db`, new Uint8Array());
        await Deno.mkdir(`${dir}/schemas/${app}`, { recursive: true });
        await Deno.writeTextFile(
          `${dir}/schemas/${app}/v1.json`,
          JSON.stringify(withStorage(schema, layoutOf(app))),
        );
      }
      const issuer = await generateIssuer();
      await Deno.writeTextFile(`${dir}/keys/keys.json`, issuer.keysFile);
      await Deno.writeTextFile(
        `${dir}/agentd.json`,
        JSON.stringify({
          listen: "127.0.0.1:0",
          vm: "vm-local",
          db_dir: `${dir}/dbs`,
          keys_file: `${dir}/keys/keys.json`,
          schemas_dir: `${dir}/schemas`,
          clock_skew: "5s",
          max_ticket_ttl: "1h",
          default_timeout: "5s",
          max_timeout: "30s",
          drain_timeout: "2s",
          shutdown_budget: "10s",
          log_file: `${dir}/logs/agentd.log`,
          ...extra,
        }),
      );
      return issuer;
    });
    const addrFile = `${dir}/agentd.addr`;
    const child = yield* processResource(
      ["-config", `${dir}/agentd.json`, "-addr-file", addrFile],
      cleanEnv(),
      {
        command: bin,
        trackRoot: root(),
        preserve: true,
      },
    );
    const output = child.output();
    let exited = false;
    void child.status.then(() => void (exited = true));
    yield* Effect.addFinalizer((exit) =>
      Effect.promise(async () => {
        if (keep) return;
        await stopGroup(child.pid);
        const out = await output;
        if (Exit.isFailure(exit)) {
          const log = await Deno.readTextFile(`${dir}/logs/agentd.log`).catch(() => "");
          console.error(
            `agentd diagnostics: ${new TextDecoder().decode(out.stderr).slice(-2000)}${log.slice(-4000)}`,
          );
        }
      })
    );
    const deadline = Date.now() + 30_000;
    let base = "";
    while (!base) {
      if (exited) {
        const out = yield* Effect.promise(() => output);
        throw new Error(`agentd exited ${out.code}: ${new TextDecoder().decode(out.stderr)}`);
      }
      const line = yield* Effect.promise(() => Deno.readTextFile(addrFile).catch(() => ""));
      if (line.includes("\n")) base = `http://${line.split("\n")[0].trim()}`;
      else {
        assert.ok(Date.now() < deadline, "agentd start timeout");
        yield* Effect.sleep("50 millis");
      }
    }
    /** A ticket for `app`'s database: read for a query, write for a mutation (write does not imply read). */
    const mint = (app: string, kind: "query" | "mutation") => {
      const iat = Math.floor(Date.now() / 1000) - 1;
      return mintTicket(issuer.key, {
        kid: issuer.kid,
        tid: TENANT,
        dbid: `${TENANT}:${app}`,
        vm: "vm-local",
        epoch: 1,
        sub: "e2e",
        perms: kind === "query" ? PERM_READ : PERM_WRITE,
        iat,
        exp: iat + 600,
      });
    };
    return { backend: "go" as const, base, dir, env: {} as Record<string, string>, mint };
  });
}

export type DenoServer = Effect.Effect.Success<ReturnType<typeof startServer>>;
export type GoAgent = Effect.Effect.Success<ReturnType<typeof startGoAgent>>;
export type GoTenancy = Effect.Effect.Success<ReturnType<typeof startGoTenancy>>;
export type Server = DenoServer | GoAgent | GoTenancy;

/** A tenancy mode by the Deno server's `TENANCY` name. */
export type Tenancy = "off" | "api-key" | "better-auth";

/**
 * agentd with a `tenancy` section (IR-4a, ADR 0012): credentials on `/v1/ops`, `/admin/tenants*`, and in
 * session mode (`better-auth`) `/api/auth` and `/admin/users/link`. Keys, users and sessions live in the
 * scenario's Postgres database, schema `control` (the Deno platform's tables stay in `public`). `env` holds
 * the same `ADMIN_TOKEN`/`*_APP_TOKEN` names as the Deno server's, so `request`, `cli` and `client` work
 * unchanged. Databases `<tenant>:<app>` are created on first use (`create_dbs`).
 */
export function startGoTenancy(dir: string, databaseUrl: string, mode: Tenancy) {
  return Effect.gen(function* () {
    const env = {
      ADMIN_TOKEN: crypto.randomUUID(),
      COLUMN_APP_TOKEN: crypto.randomUUID(),
      JSON_APP_TOKEN: crypto.randomUUID(),
    };
    yield* Effect.promise(async () => {
      const sql = postgres(databaseUrl, { max: 1, onnotice() {} });
      try {
        await sql.unsafe("CREATE SCHEMA IF NOT EXISTS control");
      } finally {
        await sql.end();
      }
      await Deno.mkdir(`${dir}/secrets`, { recursive: true, mode: 0o700 });
      await Deno.writeTextFile(
        `${dir}/secrets/tenancy.json`,
        JSON.stringify({
          admin_token: env.ADMIN_TOKEN,
          app_tokens: { [apps[0]]: env.COLUMN_APP_TOKEN, [apps[1]]: env.JSON_APP_TOKEN },
          // An explicit sslmode: pgx's default (prefer) adds a fallback config, which the adapter refuses.
          pg_dsn: `${databaseUrl}${databaseUrl.includes("?") ? "&" : "?"}sslmode=disable`,
        }),
        { mode: 0o600 },
      );
    });
    const agent = yield* startGoAgent(dir, {
      create_dbs: true,
      tenancy: {
        mode: mode === "better-auth" ? "session" : mode,
        secrets_file: `${dir}/secrets/tenancy.json`,
        pg_schema: "control",
        cache_ttl: "1s",
      },
    });
    const deadline = Date.now() + 30_000;
    while (true) { // the tenancy schema migrates in the background: /readyz says when
      const ready = yield* Effect.promise(async () => {
        try {
          const response = await fetch(`${agent.base}/readyz`, { signal: AbortSignal.timeout(500) });
          await response.body?.cancel();
          return response.ok;
        } catch {
          return false;
        }
      });
      if (ready) break;
      assert.ok(Date.now() < deadline, "agentd tenancy readiness timeout");
      yield* Effect.sleep("50 millis");
    }
    return { ...agent, env, tenancy: mode };
  });
}

/**
 * The backend this run targets (`BACKEND`): the Deno server (`databaseUrl` and `extra` are its options)
 * or a Go agent: with `TENANCY` api-key or better-auth, `startGoTenancy`; otherwise tickets only.
 */
export function startBackend(dir: string, databaseUrl: string, extra: Record<string, string> = {}) {
  if (backend === "deno") return startServer(dir, databaseUrl, extra);
  const mode = extra.TENANCY as Tenancy | undefined;
  return mode === "api-key" || mode === "better-auth"
    ? startGoTenancy(dir, databaseUrl, mode)
    : startGoAgent(dir);
}

/** A backend in `mode` on either side (`TENANCY=off` on Go: app tokens, not tickets). */
export function startTenancy(dir: string, databaseUrl: string, mode: Tenancy) {
  return backend === "go"
    ? startGoTenancy(dir, databaseUrl, mode)
    : startServer(dir, databaseUrl, { TENANCY: mode });
}

/** Where a backend serves `/v1/ops` for `app`: agentd's one route, or the Deno shim's per-app one. */
export const opsPath = (server: Server, app: string) =>
  server.backend === "go" ? "/v1/ops" : `/apps/${encodeURIComponent(app)}/v1/ops`;

/** A valid IR query body for `app` (one `get`), naming `tenant` as its database's tenant. */
export async function opsQuery(app: string, id: string, tenant: string = TENANT) {
  return {
    v: 1,
    db: { tenant, app },
    kind: "query",
    schema: await schemaFingerprint(withStorage(schema, layoutOf(app))),
    ops: [{ op: "get", table: "games", id }],
  };
}

export type Client = ReturnType<typeof createClient<typeof functions>>;
/**
 * An IR-3 SDK client for `app`: the handlers run here, their reads and commits go to `/v1/ops`. On Deno
 * the bearer is the app token (TENANCY=off) or `token` (an API key or session); on Go `token`, the app token
 * of a tenancy agent, or else a minted ticket. `tenant` is what the client claims (credentials decide).
 */
export function client(
  server: Server,
  app: string = apps[0],
  token?: string,
  tenant: string = TENANT,
): Client {
  const common = {
    functions,
    schema: withStorage(schema, layoutOf(app)),
    tenant,
    app,
    timeoutMs: 15_000,
  };
  if (server.backend === "go") {
    const credential = token ?? server.env[app === apps[0] ? "COLUMN_APP_TOKEN" : "JSON_APP_TOKEN"];
    if (credential !== undefined) {
      return createClient({ ...common, endpoint: server.base, token: credential });
    }
    return createClient({ ...common, endpoint: server.base, token: (kind) => server.mint(app, kind) });
  }
  return createClient({
    ...common,
    endpoint: `${server.base}/apps/${encodeURIComponent(app)}`,
    token: token ?? server.env[app === apps[0] ? "COLUMN_APP_TOKEN" : "JSON_APP_TOKEN"],
  });
}
/** The pre-IR client (handlers run on the Deno server), for the scenarios IR-4 ports. */
export function legacyClient(server: DenoServer, app: string = apps[0], token?: string): Client {
  return createLegacyClient<typeof functions>(
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
  const id = out.stdout.match(/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}/)?.[0];
  assert.ok(id, "tenant metadata must be printed");
  // The key row: its id (a uuid on Deno, 16 hex digits on Go), then its tenant.
  const keyId = out.stdout.match(new RegExp(`^([0-9a-f-]{16,36})\\s+${id}\\s`, "m"))?.[1];
  assert.ok(keyId, "key metadata must be printed");
  return { id, keyId, key: keys[0] };
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
/**
 * One scenario. `options.go` names why a scenario does not run against the Go backend yet (it is then
 * ignored, with the reason in its title, when `BACKEND=go`); `options.deno` the same for the Deno backend.
 */
export function test(
  name: string,
  body: () => Effect.Effect<void, unknown, Scope.Scope>,
  options: { go?: string; deno?: string } = {},
) {
  if (!signals) {
    for (const signal of ["SIGINT", "SIGTERM"] as const) Deno.addSignalListener(signal, () => abort.abort());
    signals = true;
  }
  const skip = backend === "go" ? options.go : options.deno;
  Deno.test({
    name: skip ? `${titled(name)} [skipped on ${backend}: ${skip}]` : titled(name),
    ignore: skip !== undefined,
  }, async () => {
    abort.signal.throwIfAborted();
    await Effect.runPromise(Effect.scoped(body()), { signal: abort.signal });
  });
}
