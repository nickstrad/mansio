/**
 * Scoped server composition (plan item 32): every resource the server acquires is released, in
 * order, however startup or serving ends.
 *
 * In process, over the real composition (`makeServer` → `makePlatformLive`: real SQLite handles, a
 * real reader child with DuckDB, the real JSONL sink), with the stop signal and an `afterStep` hook
 * injected:
 *
 * - a failure injected after **each** startup step releases exactly what was acquired before it;
 * - a real later-app open failure and a real HTTP bind failure (occupied port) do the same;
 * - a stop with a request held in flight waits for it (the drain), and it commits;
 * - a stop during startup (right after a step acquires, or while the startup ping waits on a frozen
 *   reader), a clean stop and the reader child dying while serving all end with the
 *   finalizers in order: HTTP drain → backup fiber → ingest/sweep fiber → reader exit → writers
 *   closed (apps in reverse) → `shutdown` as the log's last line;
 * - the ingest/sweep fiber reaches the reader through its existing `ingest` RPC.
 *
 * After each run: termination within a bound, no file descriptor left open under `DATA_DIR`
 * (`/proc/self/fd`), and no live reader process (`/proc/<pid>`).
 *
 * As spawned processes (real signals, real exit codes): SIGTERM with a mutation held in flight, an
 * occupied port and a killed reader child.
 *
 * And statically: the server's runtime module graph never reaches DuckDB, and no module in it reads
 * the environment except `platform/config.ts`.
 *
 * The reader child inherits this process's environment, so in-process runs set `Deno.env` (and
 * restore it) rather than `ConfigTest`: both processes then read one configuration, as in production.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { Cause, Deferred, Duration, Effect, Exit, Option } from "effect";
import type { StartupStep } from "../../platform/layers.ts";
import { makeServer, type ServerOptions } from "../../platform/server.ts";
import { inspect } from "../helpers/sqlite_inspect.ts";

const repoRoot = fileURLToPath(new URL("../../", import.meta.url));
const ADMIN = "admin-token";
const COLUMNS = { id: "tic-tac-toe", token: "columns-token" } as const;
const JSON_APP = { id: "tic-tac-toe-json", token: "json-token" } as const;
/** Every run must end within this, start included. */
const BOUND_MS = 30_000;

// ===========================================================================
// Harness
// ===========================================================================

interface LogRecord {
  readonly kind: string;
  readonly phase?: string;
  readonly level?: string;
  readonly traceId?: string;
  readonly detail?: Record<string, unknown>;
  readonly [key: string]: unknown;
}

interface Scratch {
  readonly root: string;
  readonly dataDir: string;
  readonly logDir: string;
  readonly port: number;
  readonly env: Record<string, string>;
}

function freePort(): number {
  const probe = Deno.listen({ hostname: "127.0.0.1", port: 0 });
  const port = (probe.addr as Deno.NetAddr).port;
  probe.close();
  return port;
}

async function scratch(prefix: string): Promise<Scratch> {
  const root = await Deno.makeTempDir({ prefix: `mansio-scope-${prefix}-` });
  const dataDir = join(root, "data");
  const logDir = join(root, "logs");
  const port = freePort();
  return {
    root,
    dataDir,
    logDir,
    port,
    env: {
      DATA_DIR: dataDir,
      LOG_DIR: logDir,
      LOCAL_OBJECT_DIR: join(root, "objects"),
      EVENT_FLUSH_MS: "20",
      PORT: String(port),
      BIND_HOST: "127.0.0.1",
      ADMIN_TOKEN: ADMIN,
      COLUMN_APP_TOKEN: COLUMNS.token,
      JSON_APP_TOKEN: JSON_APP.token,
      S3_BASE_URL: "",
      // Started (so its shutdown is observable) but never due during a test.
      BACKUP_INTERVAL_MS: "3600000",
    },
  };
}

/** Runs `body` with `env` set in this process (the reader child inherits it), then restores it. */
async function withEnv<T>(env: Record<string, string>, body: () => Promise<T>): Promise<T> {
  const previous = new Map(Object.keys(env).map((key) => [key, Deno.env.get(key)] as const));
  for (const [key, value] of Object.entries(env)) Deno.env.set(key, value);
  try {
    return await body();
  } finally {
    for (const [key, value] of previous) {
      if (value === undefined) Deno.env.delete(key);
      else Deno.env.set(key, value);
    }
  }
}

async function jsonlUnder(dir: string): Promise<string[]> {
  const files: string[] = [];
  const walk = async (at: string) => {
    let entries: Deno.DirEntry[];
    try {
      entries = await Array.fromAsync(Deno.readDir(at));
    } catch {
      return;
    }
    for (const entry of entries) {
      const path = join(at, entry.name);
      if (entry.isDirectory) await walk(path);
      else if (/^events-.*\.jsonl$/.test(entry.name)) files.push(path);
    }
  };
  await walk(dir);
  // events-<HH>-<pid>-<seq>.jsonl under <date>/: date, hour, then seq numerically.
  const key = (file: string) => {
    const match = file.match(/(\d{4}-\d{2}-\d{2})\/events-(\d+)-(\d+)-(\d+)\.jsonl$/);
    return match ? [match[1], Number(match[2]), Number(match[4])] as const : [file, 0, 0] as const;
  };
  return files.sort((a, b) => {
    const [ka, kb] = [key(a), key(b)];
    return ka[0] < kb[0] ? -1 : ka[0] > kb[0] ? 1 : ka[1] - kb[1] || ka[2] - kb[2];
  });
}

/** The server role's records under `logDir`, in write order. `pid` narrows to one process's files. */
async function serverLog(logDir: string, pid?: number): Promise<LogRecord[]> {
  const files = (await jsonlUnder(join(logDir, "server"))).filter((file) =>
    pid === undefined || file.includes(`-${pid}-`)
  );
  const texts = await Promise.all(files.map((file) => Deno.readTextFile(file)));
  return texts.flatMap((text) => text.split("\n").filter((line) => line !== "")).map((line) =>
    JSON.parse(line) as LogRecord
  );
}

const lifecycle = (records: readonly LogRecord[]) => records.filter((record) => record.kind === "lifecycle");

/** Index of the first lifecycle record matching `phase` (and `appId`, for storage events), or -1. */
function at(records: readonly LogRecord[], phase: string, appId?: string): number {
  return records.findIndex((record) =>
    record.kind === "lifecycle" && record.phase === phase &&
    (appId === undefined || record.detail?.appId === appId)
  );
}

/** Asserts the listed phases (those that occur) occur in this order. */
function assertOrder(records: readonly LogRecord[], phases: readonly (string | [string, string])[]): void {
  let previous = -1;
  let previousName = "start";
  for (const entry of phases) {
    const [phase, appId] = typeof entry === "string" ? [entry, undefined] : entry;
    const index = at(records, phase, appId);
    if (index === -1) continue;
    const name = appId === undefined ? phase : `${phase}(${appId})`;
    assert.ok(
      index > previous,
      `${name} (#${index}) must come after ${previousName} (#${previous}): ${
        lifecycle(records).map((r) => r.phase).join(", ")
      }`,
    );
    previous = index;
    previousName = name;
  }
}

/** The pid `platform/ipc.ts` logged when it spawned the reader, if it did. */
function readerPid(records: readonly LogRecord[]): number | undefined {
  const spawned = records.find((record) => record.kind === "lifecycle" && record.phase === "reader-spawned");
  const pid = spawned?.detail?.pid;
  return typeof pid === "number" ? pid : undefined;
}

/** Whether `pid` is a live (not zombie) process. */
async function isAlive(pid: number): Promise<boolean> {
  try {
    const stat = await Deno.readTextFile(`/proc/${pid}/stat`);
    return stat.slice(stat.lastIndexOf(")") + 2, stat.lastIndexOf(")") + 3) !== "Z";
  } catch {
    return false;
  }
}

async function assertGone(pid: number): Promise<void> {
  for (let attempt = 0; attempt < 50 && await isAlive(pid); attempt++) {
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  assert.ok(!(await isAlive(pid)), `reader process ${pid} is still running`);
}

/** File descriptors this process holds on anything under `dir` (SQLite files, WAL, SHM, ...). */
async function openFilesUnder(dir: string): Promise<string[]> {
  const open: string[] = [];
  for await (const entry of Deno.readDir("/proc/self/fd")) {
    try {
      const target = await Deno.readLink(`/proc/self/fd/${entry.name}`);
      if (target.startsWith(dir)) open.push(target);
    } catch { /* closed while listing */ }
  }
  return open;
}

/** `makeServer(options)` to its exit, failing the test if it takes longer than `BOUND_MS`. */
async function runServer(options: ServerOptions): Promise<Exit.Exit<never, unknown>> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const bound = new Promise<never>((_, reject) => {
    timer = setTimeout(
      () => reject(new Error(`server did not terminate within ${BOUND_MS} ms`)),
      BOUND_MS,
    );
  });
  try {
    return await Promise.race([Effect.runPromiseExit(makeServer(options)), bound]);
  } finally {
    clearTimeout(timer);
  }
}

/** A stop source the test completes, plus a promise for the `http` step (the server is serving). */
function controls() {
  const stop = Effect.runSync(Deferred.make<string>());
  let serving!: () => void;
  const ready = new Promise<void>((resolve) => (serving = resolve));
  return {
    stop,
    ready,
    serving: () => serving(),
    requestStop: (reason = "test-stop") => Deferred.unsafeDone(stop, Effect.succeed(reason)),
  };
}

/**
 * What every run must leave behind, however it ended: no descriptor under `DATA_DIR`, no live
 * reader, one `storage-closed` per `storage-opened` (apps closed in reverse), and the `shutdown`
 * event as the log's last line. Returns the log.
 */
async function assertReleased(s: Scratch, expectedExit: "success" | "interrupted" | "failure") {
  assert.deepEqual(await openFilesUnder(s.dataDir), [], "a file under DATA_DIR is still open");
  const records = await serverLog(s.logDir, Deno.pid);
  const pid = readerPid(records);
  if (pid !== undefined) {
    await assertGone(pid);
    assert.ok(at(records, "reader-exited") >= 0, "reader-spawned without reader-exited");
  }
  const opened = lifecycle(records).filter((r) => r.phase === "storage-opened").map((r) => r.detail?.appId);
  const closed = lifecycle(records).filter((r) => r.phase === "storage-closed").map((r) => r.detail?.appId);
  assert.deepEqual(closed, [...opened].reverse(), "every opened app closes, in reverse order");
  const last = records.at(-1);
  assert.equal(last?.kind, "lifecycle");
  assert.equal(last?.phase, "shutdown", `last record: ${JSON.stringify(last)}`);
  assert.equal(last?.detail?.exit, expectedExit);
  return records;
}

/** The shutdown order every run that got far enough must show. */
const SHUTDOWN_ORDER: readonly (string | [string, string])[] = [
  "stopping",
  "http-drained",
  "backup-timer-stopped",
  "ingest-sweep-stopped",
  "reader-exited",
  ["storage-closed", JSON_APP.id],
  ["storage-closed", COLUMNS.id],
  "shutdown",
];

/**
 * When the reader's death is what stops the server, `platform/ipc.ts` logs `reader-exited` as soon
 * as it sees the exit, which races the program's `stopping`; it must still precede the writers.
 */
function assertChildExitOrder(records: readonly LogRecord[]): void {
  assertOrder(records, ["ready", ...SHUTDOWN_ORDER.filter((phase) => phase !== "reader-exited")]);
  assertOrder(records, ["ready", "reader-exited", ["storage-closed", JSON_APP.id]]);
}

/** One in-process run in a fresh scratch tree, removed afterwards. */
async function inProcess(
  prefix: string,
  body: (s: Scratch) => Promise<void>,
  envOverrides: Record<string, string> = {},
): Promise<void> {
  const s = await scratch(prefix);
  try {
    await withEnv({ ...s.env, ...envOverrides }, () => body(s));
  } finally {
    await Deno.remove(s.root, { recursive: true });
  }
}

function dieMessage(exit: Exit.Exit<never, unknown>): string {
  assert.ok(Exit.isFailure(exit), "expected a failure");
  const defect = Option.getOrUndefined(Cause.dieOption(exit.cause));
  return defect instanceof Error ? defect.message : String(defect);
}

function failureOf(exit: Exit.Exit<never, unknown>): { _tag?: string } & Record<string, unknown> {
  assert.ok(Exit.isFailure(exit), "expected a failure");
  const failure = Option.getOrUndefined(Cause.failureOption(exit.cause));
  assert.ok(failure !== undefined, `expected a typed failure: ${Cause.pretty(exit.cause)}`);
  return failure as { _tag?: string } & Record<string, unknown>;
}

// ===========================================================================
// Partial startup: a failure after each acquisition
// ===========================================================================

/** Every startup step, in order, with what must have been acquired once it has run. */
const STEPS: readonly {
  readonly step: StartupStep;
  readonly apps: readonly string[];
  readonly reader: boolean;
  readonly stopped: readonly string[];
}[] = [
  { step: "logging", apps: [], reader: false, stopped: [] },
  { step: "object-store", apps: [], reader: false, stopped: [] },
  { step: `storage:${COLUMNS.id}`, apps: [COLUMNS.id], reader: false, stopped: [] },
  { step: `storage:${JSON_APP.id}`, apps: [COLUMNS.id, JSON_APP.id], reader: false, stopped: [] },
  { step: "reader", apps: [COLUMNS.id, JSON_APP.id], reader: true, stopped: [] },
  { step: "reader-ready", apps: [COLUMNS.id, JSON_APP.id], reader: true, stopped: [] },
  { step: "ingest-sweep", apps: [COLUMNS.id, JSON_APP.id], reader: true, stopped: ["ingest-sweep-stopped"] },
  {
    step: "backup-timer",
    apps: [COLUMNS.id, JSON_APP.id],
    reader: true,
    stopped: ["ingest-sweep-stopped", "backup-timer-stopped"],
  },
  {
    step: "http",
    apps: [COLUMNS.id, JSON_APP.id],
    reader: true,
    stopped: ["ingest-sweep-stopped", "backup-timer-stopped", "http-drained"],
  },
];

for (const expected of STEPS) {
  test(
    `partial startup: a failure right after "${expected.step}" releases everything acquired before it`,
    { timeout: 60_000 },
    () =>
      inProcess("partial", async (s) => {
        const exit = await runServer({
          stop: Deferred.make<string>(),
          afterStep: (step) =>
            step === expected.step ? Effect.die(new Error(`injected after ${step}`)) : Effect.void,
        });
        assert.equal(dieMessage(exit), `injected after ${expected.step}`);

        const records = await assertReleased(s, "failure");
        const opened = lifecycle(records).filter((r) => r.phase === "storage-opened").map((r) =>
          r.detail?.appId
        );
        assert.deepEqual(opened, expected.apps, "acquired exactly the steps before the failure");
        assert.equal(readerPid(records) !== undefined, expected.reader, "reader spawned iff its step ran");
        for (const phase of ["ingest-sweep-stopped", "backup-timer-stopped", "http-drained"]) {
          assert.equal(at(records, phase) >= 0, expected.stopped.includes(phase), phase);
        }
        assert.equal(at(records, "ready"), -1, "never served");
        assertOrder(records, SHUTDOWN_ORDER);
      }),
  );
}

test(
  "partial startup: a later app that cannot open closes the earlier app's handle; no reader is spawned",
  { timeout: 60_000 },
  () =>
    inProcess("app-open", async (s) => {
      // A directory where the second app's SQLite file should be: SQLite cannot open it.
      await Deno.mkdir(join(s.dataDir, `${JSON_APP.id}.sqlite`), { recursive: true });
      const exit = await runServer({ stop: Deferred.make<string>() });
      const failure = failureOf(exit);
      assert.equal(failure._tag, "StorageError");
      assert.equal(failure.operation, "open");

      const records = await assertReleased(s, "failure");
      assert.ok(at(records, "storage-opened", COLUMNS.id) >= 0);
      assert.equal(at(records, "storage-opened", JSON_APP.id), -1);
      assert.ok(at(records, "storage-closed", COLUMNS.id) >= 0);
      assert.equal(readerPid(records), undefined, "the reader is acquired after every app");
    }),
);

test(
  "partial startup: an occupied port fails with ListenError after the reader, fibers and apps, and releases them",
  { timeout: 60_000 },
  () =>
    inProcess("port", async (s) => {
      const holder = Deno.listen({ hostname: "127.0.0.1", port: s.port });
      try {
        const exit = await runServer({ stop: Deferred.make<string>() });
        const failure = failureOf(exit);
        assert.equal(failure._tag, "ListenError");
        assert.equal(failure.port, s.port);
        assert.match(String(failure.detail), /AddrInUse/);

        const records = await assertReleased(s, "failure");
        assert.ok(readerPid(records) !== undefined);
        assert.ok(at(records, "ingest-sweep-stopped") >= 0);
        assert.ok(at(records, "backup-timer-stopped") >= 0);
        assert.equal(at(records, "http-drained"), -1, "the listener was never acquired");
        assertOrder(records, SHUTDOWN_ORDER);
      } finally {
        holder.close();
      }
    }),
);

for (const step of ["object-store", `storage:${JSON_APP.id}`, "reader"] as const) {
  test(
    `partial startup: a stop signal right after "${step}" acquires interrupts startup and releases it`,
    { timeout: 60_000 },
    () =>
      inProcess("stop-startup", async (s) => {
        const c = controls();
        const exit = await runServer({
          stop: Effect.succeed(c.stop),
          // The stop arrives once this step has acquired, while the rest of the layer is still
          // building; nothing after it may start.
          afterStep: (current) =>
            current === step
              ? Effect.zipRight(Effect.sync(() => c.requestStop()), Effect.never)
              : Effect.void,
        });
        assert.ok(Exit.isFailure(exit) && Cause.isInterruptedOnly(exit.cause), String(exit));
        const records = await assertReleased(s, "interrupted");
        assert.equal(at(records, "ready"), -1);
      }),
  );
}

test(
  "partial startup: a stop signal while the startup ping waits for the reader ends startup and releases it",
  { timeout: 60_000 },
  () =>
    inProcess("stop-ping", async (s) => {
      const c = controls();
      let frozen!: (pid: number) => void;
      const readerFrozen = new Promise<number>((resolve) => (frozen = resolve));
      // Freeze the reader as soon as it is spawned, so the program's ping can never be answered.
      const run = runServer({
        stop: Effect.succeed(c.stop),
        afterStep: (step) =>
          step === "reader"
            ? Effect.promise(async () => {
              const pid = readerPid(await waitForPhase(s.logDir, Deno.pid, "reader-spawned"));
              assert.ok(pid !== undefined);
              Deno.kill(pid, "SIGSTOP");
              frozen(pid);
            })
            : Effect.void,
      });
      const pid = await readerFrozen;
      let stopping: LogRecord[];
      try {
        // `reader` is the last layer step: the build is done and the program is waiting on the ping.
        await new Promise((resolve) => setTimeout(resolve, 500));
        c.requestStop();
        stopping = await waitForPhase(s.logDir, Deno.pid, "stopping");
      } finally {
        // Let the reader see stdin close and exit gracefully, as in any other stop.
        Deno.kill(pid, "SIGCONT");
      }
      // Only the program's ping race logs `stopping` before `ready`; a stop during the build does not.
      assert.equal(stopping[at(stopping, "stopping")].detail?.reason, "test-stop");

      const exit = await run;
      assert.ok(Exit.isFailure(exit) && Cause.isInterruptedOnly(exit.cause), String(exit));
      const records = await assertReleased(s, "interrupted");
      assert.equal(at(records, "ready"), -1, "never served");
      for (const phase of ["ingest-sweep-stopped", "backup-timer-stopped", "http-drained"]) {
        assert.equal(at(records, phase), -1, `${phase}: nothing after the ping may start`);
      }
      assertOrder(records, SHUTDOWN_ORDER);
    }),
);

// ===========================================================================
// Serving, then stopping
// ===========================================================================

async function post(
  s: Scratch,
  path: string,
  token: string,
  body: unknown,
  traceId?: string,
): Promise<Response> {
  return await fetch(`http://127.0.0.1:${s.port}${path}`, {
    method: "POST",
    headers: { authorization: `Bearer ${token}`, ...(traceId ? { "x-trace-id": traceId } : {}) },
    body: JSON.stringify(body),
  });
}

/**
 * A mutation held in flight: its headers are sent and its handler is reading the body, but the body
 * is incomplete until `finish()`. Nothing can complete it early, so whatever the server does while
 * it is held (a stop, a drain) happens with this request in flight.
 */
function heldMutation(s: Scratch, appId: string, token: string, traceId: string) {
  const json = JSON.stringify({ xPlayer: "held", oPlayer: "o" });
  const encoder = new TextEncoder();
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const body = new ReadableStream<Uint8Array>({
    start: (c) => {
      controller = c;
      c.enqueue(encoder.encode(json.slice(0, 5)));
    },
  });
  const response = fetch(`http://127.0.0.1:${s.port}/apps/${appId}/mutation/createGame`, {
    method: "POST",
    headers: { authorization: `Bearer ${token}`, "x-trace-id": traceId },
    body,
  });
  return {
    response,
    finish: () => {
      controller.enqueue(encoder.encode(json.slice(5)));
      controller.close();
    },
  };
}

/** Polls the log until a lifecycle `phase` appears; returns the log at that point. */
async function waitForPhase(logDir: string, pid: number, phase: string): Promise<LogRecord[]> {
  for (let attempt = 0; attempt < 300; attempt++) {
    const records = await serverLog(logDir, pid);
    if (at(records, phase) >= 0) return records;
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  assert.fail(`no lifecycle ${phase} event`);
}

/**
 * The held request's evidence once the server has gone: it was answered 200, its `http` event lies
 * between `stopping` and `http-drained` (it finished during the drain, not before the stop), and the
 * game it created is in the SQLite file.
 */
async function assertDrained(
  records: readonly LogRecord[],
  answered: Response,
  traceId: string,
  file: string,
): Promise<void> {
  assert.equal(answered.status, 200);
  const { gameId } = (await answered.json()).result as { gameId: string };
  const event = records.findIndex((r) => r.kind === "http" && r.traceId === traceId);
  assert.ok(event >= 0, "no http event for the held request");
  assert.equal(records[event].status, 200);
  assert.ok(at(records, "stopping") < event, "the held request finished before the stop");
  assert.ok(event < at(records, "http-drained"), "the drain finished before the held request");
  const db = inspect(file);
  try {
    assert.equal(db.get("PRAGMA quick_check")?.quick_check, "ok");
    assert.ok(db.get("SELECT 1 FROM games WHERE _id = ?", [gameId]), `held game ${gameId} was lost`);
  } finally {
    db.close();
  }
}

test(
  "clean stop: requests are served, then drain → fibers → reader → writers → shutdown, in order",
  { timeout: 60_000 },
  () =>
    inProcess("clean", async (s) => {
      const c = controls();
      const run = runServer({
        stop: Effect.succeed(c.stop),
        afterStep: (step) => step === "http" ? Effect.sync(c.serving) : Effect.void,
      });
      await c.ready;
      const created = await post(s, `/apps/${COLUMNS.id}/mutation/createGame`, COLUMNS.token, {
        xPlayer: "a",
        oPlayer: "b",
      }, "scope-clean-1");
      assert.equal(created.status, 200);
      const { gameId } = (await created.json()).result;
      const read = await post(s, `/apps/${COLUMNS.id}/query/getGame`, COLUMNS.token, { gameId });
      assert.equal(read.status, 200);
      assert.equal((await read.json()).result._id, gameId);

      c.requestStop();
      const exit = await run;
      assert.ok(Exit.isFailure(exit) && Cause.isInterruptedOnly(exit.cause), String(exit));
      const records = await assertReleased(s, "interrupted");
      assert.ok(at(records, "ready") >= 0);
      assert.equal(records[at(records, "stopping")].detail?.reason, "test-stop");
      assertOrder(records, ["ready", ...SHUTDOWN_ORDER]);
      for (const phase of ["http-drained", "backup-timer-stopped", "ingest-sweep-stopped", "reader-exited"]) {
        assert.ok(at(records, phase) >= 0, `no ${phase}`);
      }
      assert.ok(
        records.some((r) => r.kind === "http" && r.traceId === "scope-clean-1" && r.status === 200),
        "the request's http event is in the server JSONL",
      );
    }),
);

test(
  "a stop with a mutation in flight: the drain waits for it, it commits, then fibers, reader, writers close",
  { timeout: 60_000 },
  () =>
    inProcess("inflight", async (s) => {
      const c = controls();
      const run = runServer({
        stop: Effect.succeed(c.stop),
        afterStep: (step) => step === "http" ? Effect.sync(c.serving) : Effect.void,
      });
      await c.ready;
      const held = heldMutation(s, COLUMNS.id, COLUMNS.token, "scope-held");
      // Let the server accept the connection and enter the handler (it is now reading the body).
      await new Promise((resolve) => setTimeout(resolve, 300));
      c.requestStop();
      const stopping = await waitForPhase(s.logDir, Deno.pid, "stopping");
      assert.equal(at(stopping, "http-drained"), -1, "drained with a request still in flight");
      // Still held: nothing past the drain has started.
      await new Promise((resolve) => setTimeout(resolve, 200));
      const during = await serverLog(s.logDir, Deno.pid);
      for (const phase of ["http-drained", "backup-timer-stopped", "reader-exited", "storage-closed"]) {
        assert.equal(at(during, phase), -1, `${phase} while a request was in flight`);
      }
      held.finish();
      const answered = await held.response;

      const exit = await run;
      assert.ok(Exit.isFailure(exit) && Cause.isInterruptedOnly(exit.cause), String(exit));
      const records = await assertReleased(s, "interrupted");
      assertOrder(records, SHUTDOWN_ORDER);
      await assertDrained(records, answered, "scope-held", join(s.dataDir, `${COLUMNS.id}.sqlite`));
    }),
);

test(
  "the reader child dies while serving: the supervisor restarts it and the server stops normally",
  { timeout: 60_000 },
  () =>
    inProcess("child-exit", async (s) => {
      const c = controls();
      const run = runServer({
        stop: Effect.succeed(c.stop),
        afterStep: (step) => step === "http" ? Effect.sync(c.serving) : Effect.void,
      });
      await c.ready;
      let pid: number | undefined;
      for (let attempt = 0; attempt < 100 && pid === undefined; attempt++) {
        pid = readerPid(await serverLog(s.logDir, Deno.pid));
        if (pid === undefined) await new Promise((resolve) => setTimeout(resolve, 50));
      }
      assert.ok(pid !== undefined, "reader-spawned never logged");
      Deno.kill(pid, "SIGKILL");

      let settled = false;
      void run.then(() => {
        settled = true;
      });
      let replacementPid: number | undefined;
      try {
        const restarted = await waitForPhase(s.logDir, Deno.pid, "reader-restarted");
        assertOrder(restarted, ["reader-restarting", "reader-restarted"]);
        assert.ok(at(restarted, "reader-restarting") >= 0);
        replacementPid = readerPid(restarted.filter((record) => record.detail?.pid !== pid));
        assert.ok(replacementPid !== undefined && replacementPid !== pid, "a new reader was spawned");
        assert.equal(restarted[at(restarted, "reader-restarted")].detail?.pid, replacementPid);
        assert.ok(await isAlive(replacementPid));
        assert.equal(settled, false, "runServer resolved after a single reader death");
        assert.equal(at(restarted, "stopping"), -1);
      } finally {
        c.requestStop();
      }
      const exit = await run;
      assert.ok(Exit.isFailure(exit) && Cause.isInterruptedOnly(exit.cause), String(exit));
      const records = await assertReleased(s, "interrupted");
      assert.equal(records[at(records, "stopping")].detail?.reason, "test-stop");
      // The killed child's exit precedes stopping; check the replacement's shutdown instead.
      const shutdown = records.slice(at(records, "stopping"));
      for (const entry of SHUTDOWN_ORDER) {
        const [phase, appId] = typeof entry === "string" ? [entry, undefined] : entry;
        assert.ok(at(shutdown, phase, appId) >= 0, `${phase} finalizer is present`);
      }
      assertOrder(shutdown, SHUTDOWN_ORDER);
      if (replacementPid !== undefined) await assertGone(replacementPid);
    }),
);

test(
  "the ingest/sweep fiber triggers the reader's existing ingest RPC; the server holds no DuckDB file",
  { timeout: 60_000 },
  () =>
    inProcess("ingest", async (s) => {
      const c = controls();
      const run = runServer({
        stop: Effect.succeed(c.stop),
        ingestSweepEvery: Duration.millis(200),
        afterStep: (step) => step === "http" ? Effect.sync(c.serving) : Effect.void,
      });
      await c.ready;
      let runs: LogRecord[] = [];
      for (let attempt = 0; attempt < 200 && runs.length < 2; attempt++) {
        runs = lifecycle(await serverLog(s.logDir, Deno.pid)).filter((r) =>
          r.phase === "ingest-sweep" || r.phase === "ingest-sweep-failed"
        );
        if (runs.length < 2) await new Promise((resolve) => setTimeout(resolve, 50));
      }
      assert.ok(runs.length >= 2, "the fiber ran at least twice");
      for (const record of runs) {
        // Until item 33 hosts the coordinator, the reader answers this RPC with a typed IngestError;
        // either way the answer came from the reader, over the RPC, as data.
        if (record.phase === "ingest-sweep-failed") {
          assert.equal(record.detail?.errorTag, "IngestError", JSON.stringify(record.detail));
          assert.equal(record.level, "warn");
        } else {
          assert.equal(typeof record.detail?.filesScanned, "number", JSON.stringify(record.detail));
        }
      }
      // A runtime spot check only: until item 33 hosts the coordinator, nothing opens the event-log
      // file at all. The import-graph test below is the guard that the server cannot load DuckDB.
      const duckdb = (await openFilesUnder(s.root)).filter((path) => path.includes(".duckdb"));
      assert.deepEqual(duckdb, [], "the server process opened an event-log DuckDB file");

      c.requestStop();
      const exit = await run;
      assert.ok(Exit.isFailure(exit) && Cause.isInterruptedOnly(exit.cause), String(exit));
      assertOrder(await assertReleased(s, "interrupted"), SHUTDOWN_ORDER);
    }),
);

// ===========================================================================
// As a process: real signals and exit codes
// ===========================================================================

interface Spawned {
  readonly child: Deno.ChildProcess;
  readonly stderr: Promise<string>;
  readonly exited: Promise<Deno.CommandStatus>;
}

function spawnServer(s: Scratch): Spawned {
  const child = new Deno.Command(Deno.execPath(), {
    args: ["run", "-A", "platform/server.ts"],
    cwd: repoRoot,
    env: s.env,
    stdout: "null",
    stderr: "piped",
  }).spawn();
  return { child, stderr: new Response(child.stderr).text(), exited: child.status };
}

async function waitReady(s: Scratch, spawned: Spawned): Promise<void> {
  let exited = false;
  void spawned.exited.then(() => (exited = true));
  for (let attempt = 0; attempt < 600; attempt++) {
    if (exited) assert.fail(`server exited before becoming ready: ${await spawned.stderr}`);
    try {
      const response = await fetch(`http://127.0.0.1:${s.port}/health`, { signal: AbortSignal.timeout(500) });
      await response.body?.cancel();
      if (response.ok) return;
    } catch { /* starting */ }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  assert.fail("server never became ready");
}

/** Waits for the child's exit, failing if it takes longer than `ms`. */
async function exitWithin(spawned: Spawned, ms: number): Promise<Deno.CommandStatus> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      spawned.exited,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(`server did not exit within ${ms} ms`)), ms);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

async function spawnedRun(prefix: string, body: (s: Scratch, spawned: Spawned) => Promise<void>) {
  const s = await scratch(prefix);
  const spawned = spawnServer(s);
  try {
    await body(s, spawned);
  } finally {
    try {
      spawned.child.kill("SIGKILL");
    } catch { /* already exited */ }
    await spawned.exited;
    await spawned.stderr;
    const pid = readerPid(await serverLog(s.logDir).catch(() => []));
    if (pid !== undefined && await isAlive(pid)) Deno.kill(pid, "SIGKILL");
    await Deno.remove(s.root, { recursive: true });
  }
}

/**
 * The process's log: shutdown last, reader gone, finalizers present and in order. `assertOrder`
 * skips missing phases, so presence is checked first: both fibers always stop, and the listener
 * drains iff it was acquired (`served`).
 */
async function assertProcessReleased(
  s: Scratch,
  spawned: Spawned,
  exit: string,
  { childExit = false, served = true }: { childExit?: boolean; served?: boolean } = {},
): Promise<LogRecord[]> {
  const records = await serverLog(s.logDir, spawned.child.pid);
  const pid = readerPid(records);
  assert.ok(pid !== undefined, "no reader-spawned event");
  await assertGone(pid);
  assert.equal(records.at(-1)?.phase, "shutdown", JSON.stringify(records.at(-1)));
  assert.equal(records.at(-1)?.detail?.exit, exit);
  const phases = lifecycle(records).map((r) => r.phase).join(", ");
  for (const phase of ["backup-timer-stopped", "ingest-sweep-stopped"]) {
    assert.ok(at(records, phase) >= 0, `no ${phase} event: ${phases}`);
  }
  assert.equal(
    at(records, "http-drained") >= 0,
    served,
    `http-drained iff the listener was acquired: ${phases}`,
  );
  if (childExit) assertChildExitOrder(records);
  else assertOrder(records, SHUTDOWN_ORDER);
  return records;
}

test(
  "process: SIGTERM with a mutation in flight drains it, then exits 0 within the bound, finalizers in order",
  { timeout: 120_000 },
  () =>
    spawnedRun("sigterm", async (s, spawned) => {
      await waitReady(s, spawned);
      const held = heldMutation(s, JSON_APP.id, JSON_APP.token, "process-held");
      await new Promise((resolve) => setTimeout(resolve, 300));
      spawned.child.kill("SIGTERM");
      const stopping = await waitForPhase(s.logDir, spawned.child.pid, "stopping");
      assert.equal(stopping[at(stopping, "stopping")].detail?.reason, "SIGTERM");
      assert.equal(at(stopping, "http-drained"), -1, "drained with a request still in flight");
      held.finish();
      const answered = await held.response;
      const status = await exitWithin(spawned, 15_000);
      assert.equal(status.code, 0, await spawned.stderr);

      const records = await assertProcessReleased(s, spawned, "interrupted");
      await assertDrained(records, answered, "process-held", join(s.dataDir, `${JSON_APP.id}.sqlite`));
    }),
);

test(
  "process: an occupied port exits 1 with ListenError on stderr and leaves no reader behind",
  { timeout: 120_000 },
  async () => {
    const s = await scratch("port-process");
    const holder = Deno.listen({ hostname: "127.0.0.1", port: s.port });
    const spawned = spawnServer(s);
    try {
      const status = await exitWithin(spawned, BOUND_MS);
      assert.equal(status.code, 1);
      const stderr = await spawned.stderr;
      assert.match(stderr, /mansio server failed: ListenError/);
      assert.match(stderr, /AddrInUse/);
      await assertProcessReleased(s, spawned, "failure", { served: false });
    } finally {
      holder.close();
      try {
        spawned.child.kill("SIGKILL");
      } catch { /* already exited */ }
      await spawned.exited;
      await Deno.remove(s.root, { recursive: true });
    }
  },
);

test(
  "process: a killed reader child is replaced, then SIGTERM exits 0 with finalizers in order",
  { timeout: 120_000 },
  () =>
    spawnedRun("child-exit-process", async (s, spawned) => {
      await waitReady(s, spawned);
      let pid: number | undefined;
      for (let attempt = 0; attempt < 100 && pid === undefined; attempt++) {
        pid = readerPid(await serverLog(s.logDir, spawned.child.pid));
        if (pid === undefined) await new Promise((resolve) => setTimeout(resolve, 50));
      }
      assert.ok(pid !== undefined);
      Deno.kill(pid, "SIGKILL");
      let exited = false;
      void spawned.exited.then(() => {
        exited = true;
      });
      const restarted = await waitForPhase(s.logDir, spawned.child.pid, "reader-restarted");
      assert.ok(at(restarted, "reader-restarting") >= 0);
      assertOrder(restarted, ["reader-restarting", "reader-restarted"]);
      const replacementPid = readerPid(restarted.filter((record) => record.detail?.pid !== pid));
      assert.ok(replacementPid !== undefined && replacementPid !== pid, "a new reader was spawned");
      assert.equal(restarted[at(restarted, "reader-restarted")].detail?.pid, replacementPid);
      assert.ok(await isAlive(replacementPid));
      // Observe the entire former failure bound, including time after the successful restart.
      await new Promise((resolve) => setTimeout(resolve, 15_000));
      assert.equal(exited, false, "server exited after a single reader death");
      assert.ok(await isAlive(spawned.child.pid));
      spawned.child.kill("SIGTERM");
      const status = await exitWithin(spawned, 15_000);
      assert.equal(status.code, 0, await spawned.stderr);
      assert.doesNotMatch(await spawned.stderr, /mansio server failed: ReaderExitedError/);
      // Exclude the killed child's exit when checking the replacement's finalizer order.
      const records = await serverLog(s.logDir, spawned.child.pid);
      await assertProcessReleased(s, spawned, "interrupted", { childExit: true });
      assert.equal(records[at(records, "stopping")].detail?.reason, "SIGTERM");
      const shutdown = records.slice(at(records, "stopping"));
      for (const entry of SHUTDOWN_ORDER) {
        const [phase, appId] = typeof entry === "string" ? [entry, undefined] : entry;
        assert.ok(at(shutdown, phase, appId) >= 0, `${phase} finalizer is present`);
      }
      assertOrder(shutdown, SHUTDOWN_ORDER);
      await assertGone(replacementPid);
    }),
);

// ===========================================================================
// The import graph
// ===========================================================================

/** Every module `platform/server.ts` loads at runtime (`import type` edges are erased). */
async function serverRuntimeGraph(): Promise<Set<string>> {
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
  return reached;
}

test("import graph: DuckDB is not reachable from the server at runtime", async () => {
  const reached = await serverRuntimeGraph();
  const local = (path: string) => new URL(path, `file://${repoRoot}`).href;
  for (
    const module of ["platform/layers.ts", "platform/registry.ts", "platform/sqlite.ts", "platform/ipc.ts"]
  ) {
    assert.ok(reached.has(local(module)), `${module} not in the graph: the walk is broken`);
  }
  const duckdb = [...reached].filter((specifier) => /duckdb|platform\/eventlog\//i.test(specifier));
  assert.deepEqual(duckdb, [], `server reaches DuckDB or the event-log modules: ${duckdb.join(", ")}`);
});

test("import graph: no module the server loads reads the environment except platform/config.ts", async () => {
  const reached = await serverRuntimeGraph();
  const root = `file://${repoRoot}`;
  const local = [...reached].filter((specifier) =>
    specifier.startsWith(root) && !specifier.includes("/node_modules/")
  );
  for (const module of ["platform/server.ts", "platform/registry.ts", "platform/config.ts"]) {
    assert.ok(local.includes(new URL(module, root).href), `${module} not in the graph`);
  }
  const offenders: string[] = [];
  for (const specifier of local) {
    if (specifier === new URL("platform/config.ts", root).href) continue;
    const source = await Deno.readTextFile(new URL(specifier));
    // Comments may name the rule; only code counts.
    const code = source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[^:"'`])\/\/.*$/gm, "$1");
    if (/\bDeno\s*\.\s*env\b|\bprocess\s*\.\s*env\b|["']node:process["']/.test(code)) {
      offenders.push(specifier.slice(root.length));
    }
  }
  assert.deepEqual(offenders, [], `modules reading the environment directly: ${offenders.join(", ")}`);
});
