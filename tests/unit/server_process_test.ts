/**
 * The server as a process (plan item 22): what `tests/unit/http_test.ts` cannot show in process.
 * These tests spawn subprocesses, and the shutdown test runs the real reader child with DuckDB.
 *
 * - SIGTERM during in-flight mutations: HTTP drains before the reader child exits, the child is gone
 *   afterwards, the final line of the log file is the `lifecycle` `shutdown` event, and caller
 *   `X-Trace-Id`s land in that file.
 * - The server's runtime module graph never reaches DuckDB; only its reader child loads it.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import { inspect } from "../helpers/sqlite_inspect.ts";

const ADMIN = "admin-token";
const COLUMNS = { id: "tic-tac-toe", token: "columns-token", storage: "columns" } as const;
const JSON_APP = { id: "tic-tac-toe-json", token: "json-token", storage: "json" } as const;

// ===========================================================================
// The real process: SIGTERM, shutdown order, and the log file
// ===========================================================================

const repoRoot = new URL("../../", import.meta.url);

function freePort(): number {
  const probe = Deno.listen({ hostname: "127.0.0.1", port: 0 });
  const port = (probe.addr as Deno.NetAddr).port;
  probe.close();
  return port;
}

async function jsonlUnder(dir: string): Promise<string[]> {
  const files: string[] = [];
  const walk = async (at: string) => {
    for await (const entry of Deno.readDir(at)) {
      const path = join(at, entry.name);
      if (entry.isDirectory) await walk(path);
      else if (entry.name.endsWith(".jsonl")) files.push(path);
    }
  };
  await walk(dir);
  return files.sort();
}

test(
  "server process (spawned; DuckDB): SIGTERM mid-mutations drains, stops the reader, and logs shutdown last",
  { timeout: 120_000 },
  async () => {
    const root = await Deno.makeTempDir({ prefix: "mansio-http-sigterm-" });
    const port = freePort();
    const base = `http://127.0.0.1:${port}`;
    const env = {
      DATA_DIR: join(root, "data"),
      LOCAL_OBJECT_DIR: join(root, "objects"),
      LOG_DIR: join(root, "logs"),
      EVENT_FLUSH_MS: "50",
      PORT: String(port),
      ADMIN_TOKEN: ADMIN,
      TENANCY: "off",
      DATABASE_URL: "postgres://mansio:mansio@127.0.0.1:1/unavailable?connect_timeout=1",
      COLUMN_APP_TOKEN: COLUMNS.token,
      JSON_APP_TOKEN: JSON_APP.token,
      S3_BASE_URL: "",
      BACKUP_INTERVAL_MS: "0",
    };
    const child = new Deno.Command(Deno.execPath(), {
      args: ["run", "-A", "platform/server.ts"],
      cwd: repoRoot,
      env,
      stdout: "null",
      stderr: "piped",
    }).spawn();
    const stderr = new Response(child.stderr).text();
    try {
      let ready = false;
      for (let attempt = 0; attempt < 600 && !ready; attempt++) {
        try {
          const response = await fetch(`${base}/health`, { signal: AbortSignal.timeout(500) });
          await response.body?.cancel();
          ready = response.ok;
        } catch { /* starting */ }
        if (!ready) await new Promise((resolve) => setTimeout(resolve, 100));
      }
      assert.ok(ready, "server never became ready");

      // MT-05: off-mode startup and ordinary app traffic do not depend on Postgres.
      const unauthorized = await fetch(`${base}/admin/tenants`);
      assert.equal(unauthorized.status, 401);
      await unauthorized.body?.cancel();
      const unavailable = await fetch(`${base}/admin/tenants`, {
        headers: { authorization: `Bearer ${ADMIN}` },
      });
      assert.equal(unavailable.status, 500);
      assert.equal((await unavailable.json()).error.tag, "StorageError");

      const post = (name: string, body: unknown, traceId: string) =>
        fetch(`${base}/apps/${COLUMNS.id}/mutation/${name}`, {
          method: "POST",
          headers: { authorization: `Bearer ${COLUMNS.token}`, "x-trace-id": traceId },
          body: JSON.stringify(body),
        });

      // Caller-supplied trace ids, to find in the server's log afterwards.
      const traceIds = [0, 1, 2].map((i) => `sigterm-test-${crypto.randomUUID()}-${i}`);
      for (const traceId of traceIds) {
        const response = await post("createGame", { xPlayer: "a", oPlayer: "b" }, traceId);
        assert.equal(response.headers.get("x-trace-id"), traceId);
        assert.equal(response.status, 200);
        await response.body?.cancel();
      }

      // A burst of mutations, and SIGTERM as soon as the first is acknowledged: the rest are in flight.
      const burst = Array.from(
        { length: 200 },
        (_, i) =>
          post("createGame", { xPlayer: `x${i}`, oPlayer: "o" }, `burst-${i}`).then(
            async (response) => response.ok ? ((await response.json()).result.gameId as string) : undefined,
            () => undefined,
          ),
      );
      await Promise.race(burst);
      child.kill("SIGTERM");
      const acknowledged = (await Promise.all(burst)).filter((id): id is string => id !== undefined);
      const status = await child.status;
      assert.equal(status.code, 0, await stderr);

      // Every acknowledged mutation is durable; the file is consistent; nothing is half-written.
      const db = inspect(join(env.DATA_DIR, `${COLUMNS.id}.sqlite`));
      try {
        assert.equal(db.get("PRAGMA quick_check")?.quick_check, "ok");
        const ids = new Set(db.all("SELECT _id FROM games").map((row) => row._id as string));
        for (const id of acknowledged) assert.ok(ids.has(id), `acknowledged game ${id} was lost`);
        assert.ok(ids.size >= acknowledged.length + traceIds.length);
        assert.ok(ids.size <= burst.length + traceIds.length);
      } finally {
        db.close();
      }

      // The server's log: the trace ids are there, the reader child exited, and shutdown is the last line.
      const files = (await jsonlUnder(join(env.LOG_DIR, "server"))).filter((file) =>
        file.includes(`-${child.pid}-`)
      );
      assert.ok(files.length > 0, "no server log file");
      const records = (await Promise.all(files.map((file) => Deno.readTextFile(file))))
        .flatMap((text) => text.split("\n").filter((line) => line !== ""))
        .map((line) => JSON.parse(line));
      for (const traceId of traceIds) {
        assert.ok(
          records.some((record) => record.kind === "http" && record.traceId === traceId),
          `trace ${traceId} not in the server log`,
        );
      }
      const lifecycle = records.filter((record) => record.kind === "lifecycle").map((record) => record.phase);
      for (const phase of ["ready", "stopping", "http-drained", "reader-exited", "shutdown"]) {
        assert.ok(lifecycle.includes(phase), `no lifecycle ${phase} event: ${lifecycle.join(", ")}`);
      }
      assert.ok(lifecycle.indexOf("http-drained") < lifecycle.indexOf("reader-exited"));
      const lastFile = await Deno.readTextFile(files.at(-1)!);
      const last = JSON.parse(lastFile.trimEnd().split("\n").at(-1)!);
      assert.equal(last.kind, "lifecycle");
      assert.equal(last.phase, "shutdown");

      // The reader child is gone too.
      const spawned = records.find((record) =>
        record.kind === "lifecycle" && record.phase === "reader-spawned"
      );
      const readerPid = spawned?.detail?.pid as number;
      assert.ok(Number.isInteger(readerPid));
      let gone = false;
      for (let attempt = 0; attempt < 50 && !gone; attempt++) {
        gone = await Deno.stat(`/proc/${readerPid}`).then(() => false, () => true);
        if (!gone) await new Promise((resolve) => setTimeout(resolve, 100));
      }
      assert.ok(gone, `reader process ${readerPid} is still running`);
    } finally {
      try {
        child.kill("SIGKILL");
      } catch { /* already exited */ }
      await child.status;
      await stderr;
      await Deno.remove(root, { recursive: true });
    }
  },
);

test("server process: its runtime module graph never loads DuckDB", async () => {
  const { stdout, code } = await new Deno.Command(Deno.execPath(), {
    args: ["info", "--json", "platform/server.ts"],
    cwd: repoRoot,
    stdout: "piped",
    stderr: "inherit",
  }).output();
  assert.equal(code, 0);
  type Dependency = { code?: { specifier?: string } };
  const graph = JSON.parse(new TextDecoder().decode(stdout)) as {
    roots: string[];
    modules: { specifier: string; dependencies?: Dependency[] }[];
  };
  const bySpecifier = new Map(graph.modules.map((module) => [module.specifier, module]));
  // Runtime edges only (`code`, static or dynamic): `import type` edges are erased before execution.
  const reached = new Set<string>();
  const pending = [...graph.roots];
  while (pending.length > 0) {
    const specifier = pending.pop()!;
    if (reached.has(specifier)) continue;
    reached.add(specifier);
    for (const dependency of bySpecifier.get(specifier)?.dependencies ?? []) {
      if (dependency.code?.specifier) pending.push(dependency.code.specifier);
    }
  }
  assert.ok(reached.size > 1, "module graph walk found nothing");
  const duckdb = [...reached].filter((specifier) => /duckdb/i.test(specifier));
  assert.deepEqual(duckdb, [], `server reaches DuckDB: ${duckdb.join(", ")}`);
});

// ===========================================================================
// Supervised reader restart (segfault mitigation): kill the reader, expect
// 502s during the restart window, then recovery with no server restart
// ===========================================================================

test(
  "server process (spawned): killing the reader child leaves document reads up while ingest waits for restart",
  { timeout: 180_000 },
  async () => {
    const root = await Deno.makeTempDir({ prefix: "mansio-reader-restart-" });
    const port = freePort();
    const base = `http://127.0.0.1:${port}`;
    const env = {
      DATA_DIR: join(root, "data"),
      LOCAL_OBJECT_DIR: join(root, "objects"),
      LOG_DIR: join(root, "logs"),
      EVENT_FLUSH_MS: "50",
      PORT: String(port),
      ADMIN_TOKEN: ADMIN,
      TENANCY: "off",
      DATABASE_URL: "postgres://mansio:mansio@127.0.0.1:1/unavailable?connect_timeout=1",
      COLUMN_APP_TOKEN: COLUMNS.token,
      JSON_APP_TOKEN: JSON_APP.token,
      S3_BASE_URL: "",
      BACKUP_INTERVAL_MS: "0",
    };
    const child = new Deno.Command(Deno.execPath(), {
      args: ["run", "-A", "platform/server.ts"],
      cwd: repoRoot,
      env,
      stdout: "null",
      stderr: "piped",
    }).spawn();
    const stderr = new Response(child.stderr).text();
    const serverPid = child.pid;
    try {
      let ready = false;
      for (let attempt = 0; attempt < 600 && !ready; attempt++) {
        try {
          const response = await fetch(`${base}/health`, { signal: AbortSignal.timeout(500) });
          await response.body?.cancel();
          ready = response.ok;
        } catch { /* starting */ }
        if (!ready) await new Promise((resolve) => setTimeout(resolve, 100));
      }
      assert.ok(ready, "server never became ready");

      const query = () =>
        fetch(`${base}/apps/${COLUMNS.id}/query/getGame`, {
          method: "POST",
          headers: { authorization: `Bearer ${COLUMNS.token}` },
          body: JSON.stringify({ gameId: "no-such-game" }),
        });

      const ingest = () =>
        fetch(`${base}/admin/logs/ingest`, {
          method: "POST",
          headers: { authorization: `Bearer ${ADMIN}` },
        });

      // Baseline: the local document query works.
      const before = await query();
      assert.equal(before.status, 200, "query did not work before the kill");
      await before.body?.cancel();

      // Find the reader child's pid from the server's `ready` lifecycle event.
      const logDir = join(env.LOG_DIR, "server");
      let readerPid = 0;
      for (let attempt = 0; attempt < 50 && readerPid === 0; attempt++) {
        for (const file of await jsonlUnder(logDir)) {
          for (const line of (await Deno.readTextFile(file)).split("\n")) {
            if (line === "") continue;
            const record = JSON.parse(line);
            if (record.kind === "lifecycle" && record.phase === "ready" && record.detail?.readerPid) {
              readerPid = record.detail.readerPid;
            }
          }
        }
        if (readerPid === 0) await new Promise((resolve) => setTimeout(resolve, 100));
      }
      assert.ok(readerPid > 0, "no readerPid in the server log");
      assert.notEqual(readerPid, serverPid);

      // Kill the reader child. The server must stay up.
      Deno.kill(readerPid, "SIGKILL");

      // Document reads stay local while ingest still reports the reader restart window.
      let saw502 = false;
      const windowEnd = Date.now() + 1500;
      while (Date.now() < windowEnd && !saw502) {
        const document = await query();
        await document.body?.cancel();
        assert.equal(document.status, 200, "document read depended on the child");
        try {
          const response = await ingest();
          await response.body?.cancel();
          if (response.status === 502) saw502 = true;
        } catch { /* connection reset is also a down reader */ }
      }
      assert.ok(saw502, "no 502 during the reader restart window");

      // The supervisor respawns the child: ingest recovers with no server restart.
      let recovered = false;
      for (let attempt = 0; attempt < 300 && !recovered; attempt++) {
        try {
          const response = await ingest();
          await response.body?.cancel();
          recovered = response.status === 200;
        } catch { /* still restarting */ }
        if (!recovered) await new Promise((resolve) => setTimeout(resolve, 100));
      }
      assert.ok(recovered, "ingest never recovered after the reader was killed");
      assert.equal(child.pid, serverPid, "server process changed");

      // The log shows the supervision, not a shutdown.
      const records = (await Promise.all(
        (await jsonlUnder(logDir)).map((file) => Deno.readTextFile(file)),
      ))
        .flatMap((text) => text.split("\n").filter((line) => line !== ""))
        .map((line) => JSON.parse(line));
      const phases = records.filter((r) => r.kind === "lifecycle").map((r) => r.phase);
      assert.ok(phases.includes("reader-restarting"), `no reader-restarting event: ${phases.join(", ")}`);
      assert.ok(phases.includes("reader-restarted"), `no reader-restarted event: ${phases.join(", ")}`);
      assert.ok(!phases.includes("stopping"), `server stopped instead of restarting: ${phases.join(", ")}`);
    } finally {
      child.kill("SIGTERM");
      await child.status;
      await stderr;
    }
  },
);
