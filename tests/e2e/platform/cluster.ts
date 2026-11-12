/**
 * A multi-node Go cluster for `scale_out` — controld (Postgres placement, the locator and ticket issuer,
 * tenancy in api-key mode), two agentd in durable mode (S3 generations; ticket keys and app schemas fetched
 * from controld), and the edge (routes from controld's VM registry). Clients use the SDK's locator route:
 * resolve at controld, every request through the edge with the ticket.
 *
 * Postgres is the scenario's database (schema `control`); S3 is `durable.ts`'s (`E2E_S3_*`), one prefix per
 * cluster. Every process runs with `-dev` (plain HTTP). Binaries: `<NAME>_BIN`, else built once.
 */
import { Effect, Exit } from "effect";
import postgres from "npm:postgres@3.4.9";
import { schema } from "../../../examples/tic-tac-toe/schema.ts";
import { fingerprintText } from "../../../core/ir/schema.ts";
import { createClient } from "../../../sdk/client.ts";
import { schemaFingerprint } from "../../../sdk/fingerprint.ts";
import type { Functions } from "../../../sdk/functions.ts";
import { withStorage } from "../../../sdk/schema.ts";
import { goBinary, type S3Store, s3Store } from "./durable.ts";
import { assert, cleanEnv, keep, layoutOf, processResource, root, stopGroup } from "./harness.ts";

export const VMS = ["vm-a", "vm-b"] as const;
export type VM = typeof VMS[number];

/** A free loopback port (a tiny race with other processes; the cluster is the only one binding in a run). */
function freeAddr(): string {
  const ln = Deno.listen({ hostname: "127.0.0.1", port: 0 });
  const port = (ln.addr as Deno.NetAddr).port;
  ln.close();
  return `127.0.0.1:${port}`;
}

const token = () => `e2e-${crypto.randomUUID()}`;

/** Reads an addr-file until it has `lines` lines; fails with the process's diagnostics if it exits first. */
async function addresses(file: string, lines: number, exited: () => boolean, why: () => Promise<string>) {
  const deadline = Date.now() + 30_000;
  for (;;) {
    const text = await Deno.readTextFile(file).catch(() => "");
    const got = text.split("\n").map((l) => l.trim()).filter(Boolean);
    if (got.length >= lines) return got;
    if (exited()) throw new Error(`${file}: process exited: ${await why()}`);
    if (Date.now() >= deadline) throw new Error(`${file}: start timeout`);
    await new Promise((r) => setTimeout(r, 50));
  }
}

async function waitOk(url: string, what: string, ms = 30_000) {
  const deadline = Date.now() + ms;
  for (;;) {
    const ok = await fetch(url, { signal: AbortSignal.timeout(1_000) }).then(async (r) => {
      await r.body?.cancel();
      return r.ok;
    }).catch(() => false);
    if (ok) return;
    assert.ok(Date.now() < deadline, `${what} not ready (${url})`);
    await new Promise((r) => setTimeout(r, 100));
  }
}

/** One managed process of the cluster: started in the caller's scope, stoppable and killable early. */
function daemon(name: string, bin: string, args: string[], dir: string, log: string) {
  return Effect.gen(function* () {
    const child = yield* processResource(args, cleanEnv(), {
      command: bin,
      trackRoot: root(),
      preserve: true,
      cwd: dir,
    });
    const output = child.output();
    let exited = false;
    void child.status.then(() => void (exited = true));
    const diagnostics = async () => {
      const out = await output;
      const text = await Deno.readTextFile(log).catch(() => "");
      return `${name} exit ${out.code}: ${new TextDecoder().decode(out.stderr).slice(-2000)}${
        text.slice(-4000)
      }`;
    };
    yield* Effect.addFinalizer((exit) =>
      Effect.promise(async () => {
        if (keep) return;
        await stopGroup(child.pid);
        if (Exit.isFailure(exit)) console.error(`${name} diagnostics: ${await diagnostics()}`);
      })
    );
    return {
      pid: child.pid,
      exited: () => exited,
      diagnostics,
      /** kill -9 of the whole process group. */
      kill: async () => {
        try {
          Deno.kill(-child.pid, "SIGKILL");
        } catch { /* gone */ }
        await child.status;
      },
      /** SIGTERM and wait (stopGroup escalates to SIGKILL after its grace). */
      stop: async () => {
        await stopGroup(child.pid);
        await child.status;
      },
    };
  });
}

type Daemon = Effect.Effect.Success<ReturnType<typeof daemon>>;

export interface Agent {
  readonly vm: VM;
  readonly query: string;
  readonly life: string;
  readonly admin: string;
  readonly process: Daemon;
}

/** Starts the cluster in the caller's scope. `databaseUrl` is the scenario's Postgres database. */
export function startCluster(dir: string, databaseUrl: string, store: S3Store = s3Store()) {
  return Effect.gen(function* () {
    const [agentd, controld, edge] = yield* Effect.promise(() =>
      Promise.all([goBinary("agentd"), goBinary("controld"), goBinary("edge")])
    );
    const pgSchema = "control";
    const dsn = `${databaseUrl}${databaseUrl.includes("?") ? "&" : "?"}sslmode=disable`;
    const secrets = { control: token(), admin: token(), edge: token() };
    const controldAddr = freeAddr();
    const api = `http://${controldAddr}`;
    yield* Effect.promise(async () => {
      const sql = postgres(databaseUrl, { max: 1, onnotice() {} });
      try {
        await sql.unsafe(`CREATE SCHEMA IF NOT EXISTS ${pgSchema}`);
      } finally {
        await sql.end();
      }
      for (const sub of ["controld", "edge", ...VMS]) await Deno.mkdir(`${dir}/${sub}`, { recursive: true });
    });
    const s3 = { endpoint: store.endpoint, bucket: store.bucket, prefix: store.prefix };
    const writeSecret = (path: string, value: unknown) =>
      Deno.writeTextFile(path, typeof value === "string" ? value : JSON.stringify(value), { mode: 0o600 });

    const agents = new Map<VM, Agent>();
    const startAgent = (vm: VM) =>
      Effect.gen(function* () {
        const at = `${dir}/${vm}`;
        yield* Effect.promise(async () => {
          await writeSecret(`${at}/secrets.json`, {
            s3_access_key: store.accessKey,
            s3_secret_key: store.secretKey,
            control_token: secrets.control,
            pg_dsn: dsn,
          });
          await Deno.writeTextFile(
            `${at}/agentd.json`,
            JSON.stringify({
              listen: "127.0.0.1:0",
              health_listen: "127.0.0.1:0",
              admin_listen: "127.0.0.1:0",
              vm,
              keys_url: `${api}/v1/keys`,
              keys_file: "ticket-keys.json",
              keys_refresh: "500ms",
              keys_timeout: "400ms",
              schemas_url: api,
              s3,
              secrets_file: "secrets.json",
              cache_dir: "cache",
              owner_dir: "owner",
              placement_pg: { schema: pgSchema },
              clock_skew: "5s",
              default_timeout: "5s",
              max_timeout: "30s",
              lifecycle_timeout: "20s",
              drain_timeout: "2s",
              shutdown_budget: "20s",
              maintenance: { interval: "50ms", write_back_every: "200ms" },
              log_file: "agentd.log",
            }),
          );
          await Deno.remove(`${at}/addr`).catch(() => {});
        });
        const process = yield* daemon(
          vm,
          agentd,
          ["-dev", "-config", `${at}/agentd.json`, "-addr-file", `${at}/addr`],
          at,
          `${at}/agentd.log`,
        );
        const [query, life, admin] = yield* Effect.promise(() =>
          addresses(`${at}/addr`, 3, process.exited, process.diagnostics)
        );
        const agent: Agent = { vm, query, life, admin, process };
        agents.set(vm, agent);
        return agent;
      });
    for (const vm of VMS) yield* startAgent(vm);

    // controld: the agents' endpoints are fixed now, and its own address was fixed before they started.
    const cdir = `${dir}/controld`;
    yield* Effect.promise(async () => {
      await writeSecret(`${cdir}/secrets.json`, {
        pg_dsn: dsn,
        control_token: secrets.control,
        admin_token: secrets.admin,
        edge_token: secrets.edge,
        s3_access_key: store.accessKey,
        s3_secret_key: store.secretKey,
      });
      await writeSecret(`${dir}/edge/edge-token`, secrets.edge);
      await Deno.writeTextFile(
        `${cdir}/controld.json`,
        JSON.stringify({
          listen: controldAddr,
          secrets_file: "secrets.json",
          log_file: "controld.log",
          pg: { schema: pgSchema },
          s3,
          agents: VMS.map((vm) => ({
            vm,
            endpoint: agents.get(vm)!.query,
            lifecycle: `http://${agents.get(vm)!.life}`,
          })),
          ticket_keys: { dir: "ticket-keys" },
          tenancy: { mode: "api-key", cache_ttl: "1s" },
          locator: { ticket_ttl: "60s" },
          heartbeat_interval: "200ms",
          heartbeat_timeout: "150ms",
          resume_interval: "500ms",
          resume_after: "2s",
          agent_timeout: "20s",
          per_key_timeout: "30s",
        }),
      );
    });
    let ctl: Daemon | undefined;
    const startControld = () =>
      Effect.gen(function* () {
        ctl = yield* daemon(
          "controld",
          controld,
          ["-dev", "-config", `${cdir}/controld.json`],
          cdir,
          `${cdir}/controld.log`,
        );
        yield* Effect.promise(() => waitOk(`${api}/readyz`, "controld"));
        yield* Effect.promise(async () => { // the placement listener: the locator's cache is in use
          const deadline = Date.now() + 15_000;
          while (
            !/^controld_locator_listening 1$/m.test(await fetch(`${api}/metrics`).then((r) => r.text()))
          ) {
            assert.ok(Date.now() < deadline, "controld's placement listener never connected");
            await new Promise((r) => setTimeout(r, 50));
          }
        });
      });
    yield* startControld();
    for (const a of agents.values()) yield* Effect.promise(() => waitOk(`http://${a.life}/readyz`, a.vm));

    const edir = `${dir}/edge`;
    yield* Effect.promise(() =>
      Deno.writeTextFile(
        `${edir}/edge.json`,
        JSON.stringify({
          listen: "127.0.0.1:0",
          health_listen: "127.0.0.1:0",
          keys_url: `${api}/v1/keys`,
          keys_file: "edge-ticket-keys.json",
          keys_refresh: "500ms",
          keys_timeout: "400ms",
          registry: {
            url: `${api}/v1/agents`,
            token_file: "edge-token",
            refresh: "300ms",
            timeout: "250ms",
            agent_scheme: "http",
          },
          rate_limit: { verifies_per_second: 1000, burst: 1000 },
          default_timeout: "5s",
          max_timeout: "30s",
          log_file: "edge.log",
        }),
      )
    );
    const edgeProc = yield* daemon(
      "edge",
      edge,
      ["-dev", "-config", `${edir}/edge.json`, "-addr-file", `${edir}/addr`],
      edir,
      `${edir}/edge.log`,
    );
    const [edgeAddr, edgeLife] = yield* Effect.promise(() =>
      addresses(`${edir}/addr`, 2, edgeProc.exited, edgeProc.diagnostics)
    );
    yield* Effect.promise(() => waitOk(`http://${edgeLife}/readyz`, "edge"));

    /** One controld API call with the admin token; answers status and body. */
    const call = async (method: string, path: string, body?: unknown) => {
      const response = await fetch(`${api}${path}`, {
        method,
        headers: { authorization: `Bearer ${secrets.admin}`, "content-type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(60_000),
      });
      return { status: response.status, body: await response.json() };
    };
    const must = async (method: string, path: string, body?: unknown) => {
      const r = await call(method, path, body);
      assert.equal(r.status, 200, `${method} ${path}: ${JSON.stringify(r.body)}`);
      return r.body;
    };

    return {
      api,
      edge: `http://${edgeAddr}`,
      agents,
      call,
      must,
      startAgent,
      startControld,
      /** controld down: SIGTERM and wait. */
      stopControld: () => ctl!.stop(),
      /** Registers `app`'s schema (its layout's fingerprint text, ADR 0011). */
      registerSchema: async (app: string) => {
        const s = withStorage(schema, layoutOf(app));
        return await must("PUT", `/v1/apps/${encodeURIComponent(app)}/schema`, {
          fingerprint: await schemaFingerprint(s),
          schema: fingerprintText(s),
        });
      },
      /** A tenant through controld's tenancy admin API: its id and API key. */
      tenant: async (slug: string) => {
        const out = await must("POST", "/admin/tenants", { name: slug, slug });
        return { id: out.result.tenant.id as string, key: out.result.apiKey.plaintext as string };
      },
      /** The registry row of `tenant:app` (state, target). */
      database: async (key: string) => (await must("GET", `/v1/databases/${encodeURIComponent(key)}`)),
      /** The placement owner of `key` (the registry view is the authority's, through the locator store). */
      owner: async (key: string, apiKey: string, tenant: string, app: string) => {
        const response = await fetch(`${api}/v1/resolve`, {
          method: "POST",
          headers: { authorization: `Bearer ${apiKey}`, "content-type": "application/json" },
          body: JSON.stringify({ tenant, app }),
          signal: AbortSignal.timeout(5_000),
        });
        const out = await response.json();
        assert.equal(response.status, 200, `resolve ${key}: ${JSON.stringify(out)}`);
        return { vm: out.vm as VM, epoch: out.epoch as number };
      },
      /** Writes `key` back on its owner now (`POST /admin/backup`): the generation S3 holds. */
      backup: async (vm: VM, key: string) => {
        const response = await fetch(
          `http://${agents.get(vm)!.admin}/admin/backup?key=${encodeURIComponent(key)}`,
          {
            method: "POST",
            signal: AbortSignal.timeout(20_000),
          },
        );
        const out = await response.json();
        assert.equal(response.status, 200, `backup ${key} on ${vm}: ${JSON.stringify(out)}`);
        return out;
      },
      /** An SDK client for `app` of `tenant` through the locator and the edge, creating the database on first use. */
      client: <F extends Functions>(functions: F, app: string, tenant: string, apiKey: string) =>
        createClient({
          functions,
          schema: withStorage(schema, layoutOf(app)),
          tenant,
          app,
          timeoutMs: 20_000,
          locator: { url: api, apiKey, create: true, edge: `http://${edgeAddr}` },
        }),
    };
  });
}

export type Cluster = Effect.Effect.Success<ReturnType<typeof startCluster>>;
