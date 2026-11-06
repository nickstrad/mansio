// Deliberately private to this demo: copy this directory without another demo dependency.
import { Cause, Effect, Exit } from "effect";

export function assert(condition: unknown, name: string): asserts condition {
  if (!condition) throw new Error(`Assertion failed: ${name}`);
}

export const temporaryRoot = Effect.acquireRelease(
  Effect.promise(() => Deno.makeTempDir({ prefix: "mansio-demo-" })),
  (root) => Effect.promise(() => Deno.remove(root, { recursive: true })),
);

// Ignore ambient deployment settings (S3, tenancy, tokens, event-log paths, etc.).
export function environment(root: string, dataDir = `${root}/data`) {
  return {
    HOME: Deno.env.get("HOME") || "/root",
    PATH: Deno.env.get("PATH") ?? "",
    DATA_DIR: dataDir,
    LOG_DIR: `${root}/logs`,
    LOCAL_OBJECT_DIR: `${root}/object-store`,
    ADMIN_TOKEN: "demo-admin",
    COLUMN_APP_TOKEN: "demo-app",
    JSON_APP_TOKEN: "demo-app",
    TENANCY: "off",
    TRACE_SQL: "true",
    BACKUP_INTERVAL_MS: "0",
  };
}

export function server(root: string, dataDir = `${root}/data`) {
  return Effect.gen(function* () {
    const running = yield* Effect.acquireRelease(
      Effect.sync(() => {
        const socket = Deno.listen({ hostname: "127.0.0.1", port: 0 });
        const port = (socket.addr as Deno.NetAddr).port;
        socket.close();
        const child = new Deno.Command(Deno.execPath(), {
          args: ["run", "-A", "platform/server.ts"],
          clearEnv: true,
          env: { ...environment(root, dataDir), PORT: String(port), BIND_HOST: "127.0.0.1" },
          stdout: "null",
          stderr: "inherit",
        }).spawn();
        return { child, base: `http://127.0.0.1:${port}` };
      }),
      ({ child }) =>
        Effect.promise(async () => {
          try {
            child.kill("SIGTERM");
          } catch (error) {
            if (!(error instanceof Deno.errors.NotFound)) throw error;
          }
          await child.status;
        }),
    );
    yield* Effect.promise(async () => {
      const deadline = Date.now() + 30_000;
      while (Date.now() < deadline) {
        try {
          const response = await fetch(`${running.base}/health`, { signal: AbortSignal.timeout(500) });
          await response.arrayBuffer();
          if (response.ok) return;
        } catch { /* startup is asynchronous */ }
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      throw new Error("Assertion failed: server healthy within 30 seconds");
    });
    return running.base;
  });
}

export function post(base: string, path: string, body: unknown = {}) {
  return Effect.promise(async () => {
    const response = await fetch(`${base}${path}`, {
      method: "POST",
      headers: { authorization: "Bearer demo-admin", "content-type": "application/json" },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(10_000),
    });
    const payload = await response.json();
    assert(response.ok, `${path}: HTTP ${response.status} ${JSON.stringify(payload)}`);
    return payload.result;
  });
}

export async function run<A, E>(name: string, program: Effect.Effect<A, E>) {
  const result = await Effect.runPromiseExit(program);
  if (Exit.isFailure(result)) {
    console.error(Cause.pretty(result.cause));
    Deno.exitCode = 1;
  } else console.log(`DEMO OK: ${name}`);
}
