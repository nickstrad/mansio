/**
 * IR-4b: a Go agent in durable mode (databases are S3 generations behind the virtual-actor host) and the
 * `dbtool` CLI, for the scenarios that need durability (`backup_restore`).
 *
 * S3 comes from the environment: `E2E_S3_ENDPOINT` (host:port, plain HTTP), `E2E_S3_ACCESS_KEY`,
 * `E2E_S3_SECRET_KEY`, `E2E_S3_BUCKET` (`work-scripts/bash/e2e-linux.sh` starts a SeaweedFS and sets them).
 * Every agent gets its own prefix in the bucket, so runs and agents never see each other's objects.
 */
import { Effect, Exit } from "effect";
import { schema } from "../../../app/schema.ts";
import { withStorage } from "../../../sdk/schema.ts";
import { generateIssuer, mintTicket, PERM_READ, PERM_WRITE } from "../../../sdk/ticket.ts";
import {
  apps,
  assert,
  cleanEnv,
  command,
  keep,
  layoutOf,
  processResource,
  root,
  stopGroup,
  TENANT,
} from "./harness.ts";

/** Where the agents' generations go: one bucket, one prefix per agent. */
export interface S3Store {
  readonly endpoint: string;
  readonly bucket: string;
  readonly accessKey: string;
  readonly secretKey: string;
  readonly prefix: string;
}

/** The S3 named by the environment, under a fresh prefix. */
export function s3Store(): S3Store {
  const env = (name: string) => {
    const value = Deno.env.get(name);
    assert.ok(
      value,
      `${name} is required for durability (work-scripts/bash/e2e-linux.sh sets it)`,
    );
    return value;
  };
  return {
    endpoint: env("E2E_S3_ENDPOINT"),
    bucket: env("E2E_S3_BUCKET"),
    accessKey: env("E2E_S3_ACCESS_KEY"),
    secretKey: env("E2E_S3_SECRET_KEY"),
    prefix: `e2e-${crypto.randomUUID()}/`,
  };
}

/** The database key of `app` (ADR 0011: tenant:app). */
export const keyOf = (app: string) => `${TENANT}:${app}`;

/** The manifest `POST /admin/backup` answers (`cmd/agentd/backup.go`; contract 1 in `internal/durable`). */
export interface Backup {
  readonly key: string;
  readonly generation: number;
  readonly epoch: number;
  readonly object: string;
  readonly size: number;
  readonly sha256: string;
  readonly written_at: string;
  readonly version: string;
  readonly wrote: boolean;
}

/** A Go command from `server/cmd`: `<NAME>_BIN` if set (e2e-linux.sh cross-compiles them), else built once. */
const built = new Map<string, Promise<string>>();
export function goBinary(name: "agentd" | "dbtool" | "controld" | "edge"): Promise<string> {
  let bin = built.get(name);
  if (!bin) {
    bin = (async () => {
      const given = Deno.env.get(`${name.toUpperCase()}_BIN`);
      if (given) return given;
      const out = `${root()}/${name}-durable`;
      const goEnv = ["GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS"].flatMap((k) => {
        const v = Deno.env.get(k);
        return v === undefined ? [] : [[k, v]];
      });
      const result = await command(
        "go",
        ["build", "-o", out, `./cmd/${name}`],
        { ...cleanEnv(), ...Object.fromEntries(goEnv) },
        300_000,
        undefined,
        "server",
      );
      assert.equal(result.code, 0, `go build ${name}: ${result.stderr}`);
      return out;
    })();
    built.set(name, bin);
  }
  return bin;
}

/** Runs `dbtool` against `store`; a non-zero exit fails unless `success` is false. */
export async function dbtool(store: S3Store, args: string[], success = true) {
  const bin = await goBinary("dbtool");
  const out = await command(bin, [
    "-endpoint",
    store.endpoint,
    "-bucket",
    store.bucket,
    "-root",
    store.prefix,
    ...args,
  ], {
    ...cleanEnv(),
    DBTOOL_S3_ACCESS_KEY: store.accessKey,
    DBTOOL_S3_SECRET_KEY: store.secretKey,
  });
  if (success) assert.equal(out.code, 0, `dbtool ${args[0]}: ${out.stderr}`);
  return out;
}

/**
 * agentd in durable mode for both apps, on one node: S3 under `store.prefix`, a file placement authority
 * (`placement_dir`), each app's schema in `schemas_dir`, and `-dev` (plain-HTTP S3; the claim's create
 * stand-in for controld). Databases are served once `claim`ed. `stop` is a clean shutdown: every dirty
 * database is written back.
 */
export function startDurableAgent(dir: string, store: S3Store = s3Store()) {
  return Effect.gen(function* () {
    const bin = yield* Effect.promise(() => goBinary("agentd"));
    const vm = "vm-local";
    const controlToken = crypto.randomUUID();
    const issuer = yield* Effect.promise(async () => {
      for (const sub of ["keys", "logs", "placement"]) await Deno.mkdir(`${dir}/${sub}`, { recursive: true });
      for (const app of apps) {
        await Deno.mkdir(`${dir}/schemas/${app}`, { recursive: true });
        await Deno.writeTextFile(
          `${dir}/schemas/${app}/v1.json`,
          JSON.stringify(withStorage(schema, layoutOf(app))),
        );
      }
      const issuer = await generateIssuer();
      await Deno.writeTextFile(`${dir}/keys/keys.json`, issuer.keysFile);
      await Deno.writeTextFile(
        `${dir}/keys/secrets.json`,
        JSON.stringify({
          s3_access_key: store.accessKey,
          s3_secret_key: store.secretKey,
          control_token: controlToken,
        }),
        { mode: 0o600 },
      );
      await Deno.writeTextFile(
        `${dir}/agentd.json`,
        JSON.stringify({
          listen: "127.0.0.1:0",
          health_listen: "127.0.0.1:0",
          admin_listen: "127.0.0.1:0",
          vm,
          keys_file: `${dir}/keys/keys.json`,
          schemas_dir: `${dir}/schemas`,
          s3: { endpoint: store.endpoint, bucket: store.bucket, prefix: store.prefix },
          secrets_file: `${dir}/keys/secrets.json`,
          cache_dir: `${dir}/cache`,
          owner_dir: `${dir}/owner`,
          placement_dir: `${dir}/placement`,
          clock_skew: "5s",
          max_ticket_ttl: "1h",
          default_timeout: "5s",
          max_timeout: "30s",
          drain_timeout: "2s",
          shutdown_budget: "20s",
          log_file: `${dir}/logs/agentd.log`,
        }),
      );
      return issuer;
    });
    const addrFile = `${dir}/agentd.addr`;
    const child = yield* processResource(
      ["-config", `${dir}/agentd.json`, "-addr-file", addrFile, "-dev"],
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
    const diagnostics = async () => {
      const out = await output;
      const log = await Deno.readTextFile(`${dir}/logs/agentd.log`).catch(() => "");
      return `exit ${out.code}: ${new TextDecoder().decode(out.stderr).slice(-2000)}${log.slice(-4000)}`;
    };
    yield* Effect.addFinalizer((exit) =>
      Effect.promise(async () => {
        if (keep) return;
        await stopGroup(child.pid);
        if (Exit.isFailure(exit)) console.error(`agentd (durable) diagnostics: ${await diagnostics()}`);
      })
    );
    // The addr-file's lines: query, lifecycle, admin listener.
    const deadline = Date.now() + 30_000;
    let lines: string[] = [];
    while (lines.length < 3) {
      if (exited) throw new Error(`agentd exited: ${yield* Effect.promise(diagnostics)}`);
      const text = yield* Effect.promise(() => Deno.readTextFile(addrFile).catch(() => ""));
      lines = text.split("\n").map((l) => l.trim()).filter(Boolean);
      if (lines.length < 3) {
        assert.ok(Date.now() < deadline, "agentd start timeout");
        yield* Effect.sleep("50 millis");
      }
    }
    const [base, life, adminBase] = lines.map((l) => `http://${l}`);
    // Ready once the bucket exists: the agent creates it in the background, and the S3 probe passes after.
    while (true) {
      const metrics = yield* Effect.promise(() =>
        fetch(`${life}/metrics`, { signal: AbortSignal.timeout(1_000) }).then((r) => r.text()).catch(() => "")
      );
      if (/^mansio_s3_probe_ok 1$/m.test(metrics)) break;
      assert.ok(Date.now() < deadline, `agentd: the S3 probe never passed (${store.endpoint})`);
      yield* Effect.sleep("100 millis");
    }
    const post = async (url: string, body?: unknown, token?: string) => {
      const response = await fetch(url, {
        method: "POST",
        headers: token ? { authorization: `Bearer ${token}`, "content-type": "application/json" } : {},
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(20_000),
      });
      const answer = await response.json();
      assert.equal(response.status, 200, `${url}: ${JSON.stringify(answer)}`);
      return answer;
    };
    /** A ticket for `app`'s database: read for a query, write for a mutation (write does not imply read). */
    const mint = (app: string, kind: "query" | "mutation") => {
      const iat = Math.floor(Date.now() / 1000) - 1;
      return mintTicket(issuer.key, {
        kid: issuer.kid,
        tid: TENANT,
        dbid: keyOf(app),
        vm,
        epoch: 1,
        sub: "e2e",
        perms: kind === "query" ? PERM_READ : PERM_WRITE,
        iat,
        exp: iat + 600,
      });
    };
    return {
      base,
      dir,
      env: {} as Record<string, string>,
      mint,
      store,
      /** Serve `app`'s database here; `create` writes its placement row first (a new database). */
      claim: (app: string, create: boolean) =>
        post(`${life}/v1/lifecycle/claim`, { db: keyOf(app), ...(create ? { create } : {}) }, controlToken),
      /** Write `app`'s database back now and answer its manifest. */
      backup: async (app: string) =>
        (await post(`${adminBase}/admin/backup?key=${encodeURIComponent(keyOf(app))}`)) as Backup,
      /** `dbtool import` of a database file as `app`'s, with this agent as the owner in its placement row. */
      importFile: async (app: string, file: string) =>
        JSON.parse(
          (await dbtool(store, [
            "import",
            file,
            "-tenant",
            TENANT,
            "-app",
            app,
            "-schema",
            `${dir}/schemas/${app}/v1.json`,
            "-placement-dir",
            `${dir}/placement`,
            "-vm",
            vm,
          ])).stdout,
        ),
      /** SIGTERM and wait: the shutdown writes back every dirty database; it must exit 0. */
      stop: async () => {
        await stopGroup(child.pid);
        const out = await output;
        assert.equal(out.code, 0, `agentd shutdown: ${await diagnostics()}`);
      },
    };
  });
}
