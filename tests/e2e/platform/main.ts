/** One command; own Postgres, test subprocess, process groups and scratch paths through every exit. */
import { Effect } from "effect";
import { assert, cleanEnv, command, exists, keep, processResource, stopGroup } from "./harness.ts";
import { docker, project, startPostgres } from "./postgres.ts";
import { names, order, players, SEED } from "./seed.ts";

const started = performance.now();
const abort = new AbortController();
let interrupted = false;
const onSignal = () => {
  interrupted = true;
  abort.abort();
};
for (const signal of ["SIGINT", "SIGTERM"] as const) Deno.addSignalListener(signal, onSignal);
const timeout = setTimeout(() => {
  console.error("E2E exceeded 230 s runtime bound");
  onSignal();
}, 230_000);
let temp: string | undefined;
let beforeStatus: string | undefined;
let beforeContainers: string[] = [];
let beforePaths: boolean[] = [];
const defaultPaths = ["data", "logs", "object-store"];
let failed = false;
let dockerReady = false;
const gitStatus = async () => {
  const out = await command("git", ["status", "--porcelain"]);
  assert.equal(out.code, 0, out.stderr);
  return out.stdout;
};
/**
 * A Postgres the caller already runs (`E2E_POSTGRES_URL`, e.g. `work-scripts/bash/e2e-linux.sh`'s sibling
 * container): no Compose project is started, and the Docker hermeticity checks are skipped.
 */
const external = Deno.env.get("E2E_POSTGRES_URL") || undefined;
const backend = Deno.env.get("BACKEND") ?? "deno";
/** Scenarios that run on the Go backend; the rest are skipped there with a reason (harness.ts `test`). */
const onGo = ["documents", "queries", "backup_restore", "auth", "tenant_isolation", "admin_lifecycle"];
const skipped = backend === "go" ? order.filter((name) => !onGo.includes(name)) : [];
function forwarded(keys: string[]): Record<string, string> {
  return Object.fromEntries(keys.flatMap((key) => {
    const value = Deno.env.get(key);
    return value === undefined ? [] : [[key, value]];
  }));
}
console.log(`order=${order.join(",")} seed=${SEED} backend=${backend}`);
console.log(`players=${JSON.stringify(players)} tenant-slugs=${names.slice(2).join(",")}`);
try {
  beforeStatus = await gitStatus();
  beforePaths = await Promise.all(defaultPaths.map(exists));
  if (!external) {
    try {
      beforeContainers = await docker(["ps", "-a", "-q", "--filter", "name=mansio-e2e-"]);
      dockerReady = true;
    } catch (error) {
      throw new Error(`Docker dependency missing or unhealthy: ${error}`);
    }
  }
  await Effect.runPromise(
    Effect.scoped(Effect.gen(function* () {
      temp = yield* Effect.acquireRelease(
        Effect.promise(() => Deno.makeTempDir({ prefix: "mansio-e2e-" })),
        (path) =>
          Effect.promise(async () => {
            if (keep) return;
            const failures: unknown[] = [];
            if (await exists(`${path}/pids`)) {
              for await (const entry of Deno.readDir(`${path}/pids`)) {
                try {
                  await stopGroup(Number(entry.name));
                } catch (e) {
                  failures.push(e);
                }
              }
            }
            assert.deepEqual(failures, [], "Failed to reclaim child processes");
            await Deno.remove(path, { recursive: true });
          }),
      );
      yield* Effect.promise(() => Deno.mkdir(`${temp}/pids`));
      const url = external ?? (yield* startPostgres());
      const args = Deno.args[0] === "--" ? Deno.args.slice(1) : Deno.args;
      // Reject flags that can silently execute zero scenarios while claiming acceptance.
      assert.ok(
        !args.some((arg) => ["--no-run", "--shuffle"].some((flag) => arg.split("=")[0] === flag)),
        "--no-run/--shuffle conflict with seeded E2E execution",
      );
      const child = yield* processResource(
        ["test", "-A", ...args, "tests/e2e/platform/suite_test.ts"],
        {
          ...cleanEnv(),
          E2E_ROOT: temp!,
          E2E_POSTGRES_URL: url,
          PROP_SEED: String(SEED),
          E2E_KEEP: keep ? "1" : "0",
          // The backend under test and how to get a Go agent (harness.ts `startBackend`).
          ...forwarded(["BACKEND", "AGENTD_BIN", "GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS"]),
          // Durability on Go (durable.ts): dbtool and the S3 the durable agents use.
          ...forwarded([
            "DBTOOL_BIN",
            "E2E_S3_ENDPOINT",
            "E2E_S3_BUCKET",
            "E2E_S3_ACCESS_KEY",
            "E2E_S3_SECRET_KEY",
          ]),
        },
        { inherit: true, trackRoot: temp },
      );
      const status = yield* Effect.promise(() => child.status);
      assert.equal(status.code, 0, `deno test exited ${status.code}`);
    })),
    { signal: abort.signal },
  );
} catch (error) {
  failed = true;
  console.error(error instanceof Error ? error.stack : String(error));
} finally {
  clearTimeout(timeout);
  for (const signal of ["SIGINT", "SIGTERM"] as const) Deno.removeSignalListener(signal, onSignal);
  if (keep) {
    console.error(
      `E2E_KEEP=1 retained project=${project} root=${temp ?? "(not acquired)"}; PID records in root/pids`,
    );
  } else {
    try {
      if (temp) assert.equal(await exists(temp), false, `Temp root leaked: ${temp}`);
      if (dockerReady) {
        for (const kind of ["container", "network", "volume"]) {
          assert.deepEqual(
            await docker([
              kind,
              "ls",
              "-q",
              ...(kind === "container" ? ["-a"] : []),
              "--filter",
              `label=com.docker.compose.project=${project}`,
            ]),
            [],
            `${kind} leak in ${project}`,
          );
        }
        const after = await docker(["ps", "-a", "-q", "--filter", "name=mansio-e2e-"]);
        // Other concurrent suites may start/finish; only this project's resources are ours to reclaim.
        console.log(
          `hermeticity: baseline-e2e-containers=${beforeContainers.length} current=${after.length}`,
        );
      }
      if (beforeStatus !== undefined) assert.equal(await gitStatus(), beforeStatus, "git status changed");
      if (beforePaths.length) {
        assert.deepEqual(
          await Promise.all(defaultPaths.map(exists)),
          beforePaths,
          "Created default data/logs/object-store path",
        );
      }
    } catch (error) {
      failed = true;
      console.error(`Hermeticity failure: ${error}`);
    }
  }
}
if (interrupted) failed = true;
console.log(`E2E duration=${((performance.now() - started) / 1000).toFixed(2)}s`);
if (failed) console.error(`Replay: PROP_SEED=${SEED} BACKEND=${backend} deno task e2e`);
console.log(
  `E2E ${failed ? "FAIL" : "PASS"} seed=${SEED} backend=${backend} scenarios=${order.length} ` +
    `skipped=${skipped.length ? skipped.join(",") : "none"}`,
);
Deno.exitCode = failed ? 1 : 0;
