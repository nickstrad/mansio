/**
 * The reader's `ingest` RPC, connected to the event log (plan item 33).
 *
 * Most tests drive the real `platform/reader.ts` over stdin/stdout with isolated `DATA_DIR`,
 * `LOG_DIR`, `TMPDIR` and event-log paths: ingest a fixed, closed JSONL source; repeat, and restart,
 * with zero new rows for that source; append a tail and see only its records; fail ingest at the
 * handle open and inside a run; and stop the process with SIGTERM in the middle of an ingest. Every
 * run checks that stdout carried nothing but reply frames.
 *
 * Between runs the test opens the event-log file itself, read-only, to count rows per source. The
 * reader's own log files are ingest sources too, so each run's ingest/lifecycle records land in the
 * tables as well; they are counted apart (`reader/…` sources), never mixed into the fixture's counts.
 *
 * The last test builds the reader's event-log service in this process over a probe driver, to see
 * the order native handles open and close in. That loads DuckDB here, which is safe for the same
 * reason as in `tests/unit/eventlog_test.ts`: event-log ingest never loads DuckDB's `sqlite`
 * extension. The query fixture's SQLite files are written with the FFI binding
 * (`tests/helpers/sqlite_inspect.ts`), imported dynamically.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { Deferred, Effect, Either, Exit, Fiber, Layer, Scope } from "effect";
import type { IngestError } from "../../core/errors.ts";
import {
  type EventRecord,
  lifecycleEvent,
  type LifecycleEventRecord,
  toJsonLine,
} from "../../core/events.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileInsert } from "../../core/compile/write.ts";
import { dateBucketOf } from "../../core/logging/rotation.ts";
import { schema } from "../../app/schema.ts";
import { withStorage } from "../../sdk/schema.ts";
import { ConfigTest } from "../../platform/config.ts";
import { EventSink, type ReaderReply } from "../../platform/services.ts";

/** Unwrap a right, failing the test with the rendered left on a left. */
function expectRight<A, E>(either: Either.Either<A, E>): A {
  if (Either.isRight(either)) return either.right;
  assert.fail(`expected a right, got left ${JSON.stringify(either.left)}`);
}

import type {
  EventLogConnection,
  EventLogDriver,
  IngestThenSweepReport,
} from "../../platform/eventlog/ingest.ts";

const repoRoot = fileURLToPath(new URL("../../", import.meta.url));
const DAY_MS = 24 * 60 * 60 * 1000;
/** Every reader run must end within this; a hang is a failure, not a slow pass. */
const EXIT_BOUND_MS = 60_000;

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

interface World {
  readonly root: string;
  readonly dataDir: string;
  readonly logDir: string;
  readonly tmpDir: string;
  readonly dbPath: string;
}

async function withWorld(body: (world: World) => Promise<void>): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "mansio-reader-ingest-" });
  const world = {
    root,
    dataDir: join(root, "data"),
    logDir: join(root, "logs"),
    tmpDir: join(root, "tmp"),
    dbPath: join(root, "data", "eventlog.duckdb"),
  };
  try {
    await Deno.mkdir(world.dataDir, { recursive: true });
    await Deno.mkdir(world.tmpDir, { recursive: true });
    await writeAppFixture(world.dataDir);
    await body(world);
  } finally {
    await Deno.remove(root, { recursive: true });
  }
}

/** Both apps' SQLite files, each holding one game, so the reader can answer queries. */
async function writeAppFixture(dir: string): Promise<void> {
  const { inspect } = await import("../helpers/sqlite_inspect.ts");
  const layouts = [
    ["tic-tac-toe.sqlite", schema],
    ["tic-tac-toe-json.sqlite", withStorage(schema, "json")],
  ] as const;
  const game = {
    xPlayer: "alice",
    oPlayer: "bob",
    board: [null, null, null, null, null, null, null, null, null],
    turn: "X",
    status: "playing",
    winner: null,
  };
  for (const [file, layout] of layouts) {
    const db = inspect(join(dir, file), { write: true });
    try {
      for (const statement of expectRight(compileDDL(layout))) db.exec(statement);
      const insert = expectRight(
        compileInsert(layout, "games", game, { _id: "game-1", _creationTime: 1 }),
      );
      db.run(insert.text, insert.values);
    } finally {
      db.close();
    }
  }
}

/** A dated directory `days` before today, in the local-time buckets the log tree uses. */
function daysAgo(days: number): { readonly date: string; readonly mtime: Date } {
  const mtime = new Date(Date.now() - days * DAY_MS);
  return { date: dateBucketOf(mtime), mtime };
}

/** One ingestible line: a real `lifecycle` record from a server process, with a known `eventId`. */
function eventLine(eventId: string, pad = 0): string {
  return toJsonLine(lifecycleEvent({
    eventId,
    ts: "2026-10-01T09:00:00.000Z",
    level: "info",
    role: "server",
    pid: 4242,
    traceId: "fixture",
    phase: "fixture",
    detail: pad > 0 ? { pad: "x".repeat(pad) } : { eventId },
  }));
}

/** A closed server source `days` old (its mtime is that old too), relative to LOG_DIR. */
async function putSource(
  world: World,
  days: number,
  seq: number,
  ids: readonly string[],
  pad = 0,
): Promise<string> {
  const { date, mtime } = daysAgo(days);
  const rel = `server/${date}/events-09-4242-${String(seq).padStart(3, "0")}.jsonl`;
  const path = join(world.logDir, rel);
  await Deno.mkdir(dirname(path), { recursive: true });
  await Deno.writeTextFile(path, ids.map((id) => eventLine(id, pad) + "\n").join(""));
  await Deno.utime(path, mtime, mtime);
  return rel;
}

const range = (prefix: string, n: number) => Array.from({ length: n }, (_, i) => `${prefix}-${i}`);

async function exists(path: string): Promise<boolean> {
  try {
    await Deno.lstat(path);
    return true;
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return false;
    throw cause;
  }
}

// ---------------------------------------------------------------------------
// The reader process
// ---------------------------------------------------------------------------

type Reply = ReaderReply & {
  readonly result?: IngestThenSweepReport & Record<string, unknown>;
  readonly error?: {
    readonly _tag: string;
    readonly message: string;
    readonly fields: Record<string, unknown>;
  };
};

/** One running `platform/reader.ts`, with every byte of its stdout kept for the purity check. */
class ReaderSession {
  readonly child: Deno.ChildProcess;
  readonly pid: number;
  #stdin: WritableStreamDefaultWriter<Uint8Array>;
  #stdout: ReadableStreamDefaultReader<string>;
  #stderr: Promise<string>;
  #buffered = "";
  raw = "";

  constructor(world: World, env: Record<string, string> = {}) {
    this.child = new Deno.Command(Deno.execPath(), {
      args: ["run", "-A", "platform/reader.ts"],
      cwd: repoRoot,
      env: {
        DATA_DIR: world.dataDir,
        LOG_DIR: world.logDir,
        TMPDIR: world.tmpDir,
        EVENTLOG_DUCKDB: world.dbPath,
        ...env,
      },
      stdin: "piped",
      stdout: "piped",
      stderr: "piped",
    }).spawn();
    this.pid = this.child.pid;
    this.#stdin = this.child.stdin.getWriter();
    this.#stdout = this.child.stdout.pipeThrough(new TextDecoderStream()).getReader();
    this.#stderr = new Response(this.child.stderr).text();
  }

  async send(...frames: readonly Record<string, unknown>[]): Promise<void> {
    await this.#stdin.write(new TextEncoder().encode(frames.map((f) => JSON.stringify(f) + "\n").join("")));
  }

  async next(): Promise<Reply> {
    while (!this.#buffered.includes("\n")) {
      const chunk = await this.#stdout.read();
      assert.ok(!chunk.done, `reader closed stdout early; stdout so far: ${this.raw}`);
      this.#buffered += chunk.value;
      this.raw += chunk.value;
    }
    const line = this.#buffered.slice(0, this.#buffered.indexOf("\n"));
    this.#buffered = this.#buffered.slice(line.length + 1);
    return JSON.parse(line) as Reply;
  }

  async ask(frame: Record<string, unknown>): Promise<Reply> {
    await this.send(frame);
    return await this.next();
  }

  /** Reads the rest of stdout and waits for the exit, within `EXIT_BOUND_MS`. */
  async #drain(): Promise<{ code: number; stdout: string; stderr: string }> {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const bound = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        try {
          this.child.kill("SIGKILL");
        } catch { /* already gone */ }
        reject(new Error(`reader ${this.pid} did not exit within ${EXIT_BOUND_MS} ms`));
      }, EXIT_BOUND_MS);
    });
    try {
      return await Promise.race([
        (async () => {
          while (true) {
            const chunk = await this.#stdout.read();
            if (chunk.done) break;
            this.raw += chunk.value;
          }
          const status = await this.child.status;
          return { code: status.code, stdout: this.raw, stderr: await this.#stderr };
        })(),
        bound,
      ]);
    } finally {
      clearTimeout(timer);
    }
  }

  /** Closes stdin (a normal stop) and waits for the exit. */
  async finish() {
    await this.#stdin.close();
    return await this.#drain();
  }

  /** Sends `signal` and waits for the exit; stdin stays open, so only the signal can stop it. */
  async stop(signal: Deno.Signal) {
    this.child.kill(signal);
    const result = await this.#drain();
    await this.#stdin.close().catch(() => undefined);
    return result;
  }
}

/** stdout carried only whole reply frames: one JSON object per line, `id` plus `result` or `error`. */
function assertPureProtocol(stdout: string): Reply[] {
  const lines = stdout.split("\n");
  assert.equal(lines.pop(), "", `stdout ends with a newline: ${JSON.stringify(stdout.slice(-200))}`);
  return lines.map((line) => {
    const reply = JSON.parse(line) as Reply;
    const keys = JSON.stringify(Object.keys(reply).sort());
    assert.ok(
      keys === '["id","result"]' || keys === '["error","id"]',
      `not a reply frame: ${line.slice(0, 200)}`,
    );
    assert.equal(typeof reply.id, "number");
    return reply;
  });
}

function resultOf(reply: Reply): IngestThenSweepReport {
  assert.ok(reply.result !== undefined, `expected a result: ${JSON.stringify(reply).slice(0, 2000)}`);
  return reply.result;
}

function assertGame(reply: Reply): void {
  assert.equal(
    (reply.result as unknown as { _id?: string } | undefined)?._id,
    "game-1",
    JSON.stringify(reply),
  );
}

const QUERY = { kind: "query", appId: "tic-tac-toe", name: "getGame", args: { gameId: "game-1" } };

/** Every record the reader processes wrote under `LOG_DIR/reader`. */
async function readerRecords(logDir: string): Promise<EventRecord[]> {
  const records: EventRecord[] = [];
  const base = join(logDir, "reader");
  if (!(await exists(base))) return records;
  for await (const day of Deno.readDir(base)) {
    for await (const file of Deno.readDir(join(base, day.name))) {
      if (!/^events-.*\.jsonl$/.test(file.name)) continue;
      const text = await Deno.readTextFile(join(base, day.name, file.name));
      for (const line of text.split("\n")) if (line !== "") records.push(JSON.parse(line) as EventRecord);
    }
  }
  return records.sort((a, b) => a.ts.localeCompare(b.ts));
}

function lifecyclePhases(records: readonly EventRecord[], pid: number): string[] {
  return records.flatMap((r) =>
    r.kind === "lifecycle" && r.pid === pid ? [(r as LifecycleEventRecord).phase] : []
  );
}

// ---------------------------------------------------------------------------
// The event-log file, read between runs
// ---------------------------------------------------------------------------

type Row = Record<string, unknown>;

async function eventLog(dbPath: string, sql: string): Promise<Row[]> {
  const { DuckDBInstance } = await import("@duckdb/node-api");
  const instance = await DuckDBInstance.create(dbPath, { access_mode: "READ_ONLY" });
  try {
    const connection = await instance.connect();
    try {
      return (await connection.runAndReadAll(sql)).getRowObjects() as Row[];
    } finally {
      connection.closeSync();
    }
  } finally {
    instance.closeSync();
  }
}

/** `platform_events` rows per source file: event ids and offsets. */
async function rowsBySource(dbPath: string): Promise<Map<string, { id: string; offset: number }[]>> {
  const rows = await eventLog(
    dbPath,
    "SELECT source_file, event_id, source_offset FROM platform_events ORDER BY source_file, source_offset",
  );
  const out = new Map<string, { id: string; offset: number }[]>();
  for (const row of rows) {
    const list = out.get(String(row.source_file)) ?? [];
    list.push({ id: String(row.event_id), offset: Number(row.source_offset) });
    out.set(String(row.source_file), list);
  }
  return out;
}

async function ingestState(dbPath: string): Promise<Map<string, Row>> {
  const rows = await eventLog(
    dbPath,
    "SELECT source_file, bytes_ingested, prefix_sha256, CAST(last_ingested_at AS VARCHAR) AS at FROM _ingest_state",
  );
  return new Map(
    rows.map((row) => [String(row.source_file), { ...row, bytes_ingested: Number(row.bytes_ingested) }]),
  );
}

/** No event id appears twice in `platform_events`. */
async function assertNoDuplicates(dbPath: string): Promise<void> {
  const dups = await eventLog(
    dbPath,
    "SELECT event_id, count(*) AS n FROM platform_events GROUP BY event_id HAVING count(*) > 1",
  );
  assert.deepEqual(dups, [], "an event was ingested twice");
}

/**
 * The rows the operation itself contributed: every source under `reader/` holds this suite's own
 * reader processes' records, and nothing else. Returns them, for counting apart from the fixture.
 */
function readerOwn(
  bySource: ReadonlyMap<string, readonly { id: string }[]>,
  pids: readonly number[],
): number {
  let count = 0;
  for (const [source, rows] of bySource) {
    if (!source.startsWith("reader/")) continue;
    const pid = Number(/events-\d{2}-(\d+)-\d+\.jsonl$/.exec(source)?.[1]);
    assert.ok(pids.includes(pid), `${source} is not from one of this test's readers (${pids.join(", ")})`);
    count += rows.length;
  }
  return count;
}

// ===========================================================================
// The import graph
// ===========================================================================

/** Every module `entry` loads at runtime (`import type` edges are erased by `deno info`). */
async function runtimeGraph(entry: string): Promise<Set<string>> {
  const { stdout, code } = await new Deno.Command(Deno.execPath(), {
    args: ["info", "--json", entry],
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

const local = (path: string) => new URL(path, `file://${repoRoot}`).href;

// The same walks in `tests/unit/boundaries_test.ts` also pin the other SQLite driver out of both graphs.
test("import graph: the reader entry reaches the event log and retention, never the writer's SQLite", async () => {
  const reached = await runtimeGraph("platform/reader.ts");
  for (
    const module of [
      "platform/reader_layers.ts",
      "platform/reader_context.ts",
      "platform/eventlog/ingest.ts",
      "platform/eventlog/tables.ts",
      "platform/logging/retention.ts",
      "platform/duckdb.ts",
    ]
  ) {
    assert.ok(reached.has(local(module)), `${module} is not reachable from platform/reader.ts at runtime`);
  }
  assert.ok(
    [...reached].some((s) => /@duckdb\/node-api/.test(s)),
    "the walk never reached DuckDB: it is broken",
  );
  const sqlite = [...reached].filter((s) => /platform\/(sqlite\.ts|sqlite_ffi\/)/.test(s));
  assert.deepEqual(sqlite, [], `the reader reaches the writer's SQLite: ${sqlite.join(", ")}`);
});

test("import graph: the server never reaches the reader root or its context module", async () => {
  const reached = await runtimeGraph("platform/server.ts");
  assert.ok(
    reached.has(local("platform/sqlite_ffi/database.ts")),
    "the walk never reached the SQLite binding: it is broken",
  );
  const offenders = [...reached].filter((s) => /platform\/reader_(layers|context)\.ts$|duckdb/i.test(s));
  assert.deepEqual(offenders, [], `the server reaches reader-only modules: ${offenders.join(", ")}`);
});

// ===========================================================================
// Ingest via RPC
// ===========================================================================

test("analysis RPC: catalog validates names and parameters and sees subsequent ingest commits", async () => {
  await withWorld(async (world) => {
    await putSource(world, 3, 1, ["analysis-first"]);
    const reader = new ReaderSession(world);
    try {
      const unknown = await reader.ask({ id: 1, kind: "analysis", name: "noSuchQuery", params: {} });
      assert.equal(unknown.error?._tag, "UnknownAnalysisQuery");
      assert.ok((unknown.error?.fields.known as string[]).includes("trace"));
      const invalid = await reader.ask({ id: 2, kind: "analysis", name: "trace", params: {} });
      assert.equal(invalid.error?._tag, "AnalysisError");
      resultOf(await reader.ask({ id: 3, kind: "ingest" }));
      const timeline = async (id: number) => {
        const reply = await reader.ask({
          id,
          kind: "analysis",
          name: "trace",
          params: { traceId: "fixture" },
        });
        assert.equal(reply.error, undefined, JSON.stringify(reply));
        assert.ok(Array.isArray(reply.result));
        return (reply.result as unknown as Row[]).map((row) => row.event_id).sort();
      };
      assert.deepEqual(await timeline(4), ["analysis-first"]);
      // The analysis handle is already open: a later ingest must be visible without reopening it.
      await putSource(world, 3, 2, ["analysis-second"]);
      resultOf(await reader.ask({ id: 5, kind: "ingest" }));
      assert.deepEqual(await timeline(6), ["analysis-first", "analysis-second"]);
    } finally {
      const stopped = await reader.finish();
      assert.equal(stopped.code, 0, stopped.stderr);
      assertPureProtocol(stopped.stdout);
    }
  });
});

test(
  "ingest RPC: a closed source lands once, survives restarts with zero new rows, then only its tail; retention runs after",
  { timeout: 180_000 },
  () =>
    withWorld(async (world) => {
      const SOURCE = await putSource(world, 3, 1, range("src", 40));
      // Older than LOG_RETENTION_DAYS (default 30): the sweep may delete it, but only once ingested.
      const OLD = await putSource(world, 40, 1, range("old", 5));
      const pids: number[] = [];

      // --- run 1: ingest, then a query on the same connection ---------------------------------
      const first = new ReaderSession(world);
      pids.push(first.pid);
      assert.equal((await first.ask({ id: 1, kind: "ping" })).result, "ready");
      const report = resultOf(await first.ask({ id: 2, kind: "ingest" }));
      assert.equal(report.linesRejected, 0);
      assert.ok(report.linesIngested >= 45, JSON.stringify(report));
      assert.equal(report.retention.status, "swept", JSON.stringify(report.retention));
      if (report.retention.status === "swept") {
        assert.deepEqual(report.retention.sweep.deleted, [OLD], "the ingested, expired source was swept");
        assert.ok(!report.retention.sweep.deleted.includes(SOURCE));
      }
      assert.ok(report.budgets !== undefined && report.budgets.derived.platformEventRows >= 45);
      assert.ok(
        !(await exists(join(world.logDir, OLD))),
        "retention deleted the old source after ingesting it",
      );
      assert.ok(
        await exists(join(world.logDir, SOURCE)),
        "a 3-day-old source is neither gzipped nor deleted",
      );
      assertGame(await first.ask({ id: 3, ...QUERY }));
      const run1 = await first.finish();
      assert.equal(run1.code, 0, run1.stderr);
      assert.equal(run1.stderr, "");
      assert.deepEqual(assertPureProtocol(run1.stdout).map((r) => r.id), [1, 2, 3]);

      const after1 = await rowsBySource(world.dbPath);
      assert.deepEqual(after1.get(SOURCE)?.map((r) => r.id), range("src", 40));
      assert.deepEqual(
        after1.get(OLD)?.map((r) => r.id),
        range("old", 5),
        "the deleted source's rows remain",
      );
      const own1 = readerOwn(after1, pids);
      // The only sources are the fixture, the swept file, and the readers' own logs.
      assert.equal([...after1.values()].reduce((n, rows) => n + rows.length, 0), 45 + own1);
      const state1 = (await ingestState(world.dbPath)).get(SOURCE);
      assert.equal(state1?.bytes_ingested, (await Deno.stat(join(world.logDir, SOURCE))).size);
      await assertNoDuplicates(world.dbPath);

      // --- run 2: restart; ingest twice around a query, sent in one write ----------------------
      const second = new ReaderSession(world);
      pids.push(second.pid);
      await second.send({ id: 1, kind: "ingest" }, { id: 2, ...QUERY }, { id: 3, kind: "ingest" });
      const replies2 = [await second.next(), await second.next(), await second.next()];
      assert.deepEqual(replies2.map((r) => r.id), [1, 2, 3], "served strictly in order");
      for (const reply of [replies2[0], replies2[2]]) assert.equal(resultOf(reply).linesRejected, 0);
      assertGame(replies2[1]);
      const run2 = await second.finish();
      assert.equal(run2.code, 0, run2.stderr);
      assert.equal(run2.stderr, "");
      assertPureProtocol(run2.stdout);

      const after2 = await rowsBySource(world.dbPath);
      assert.deepEqual(after2.get(SOURCE), after1.get(SOURCE), "zero new rows for the fixed source");
      assert.deepEqual(
        (await ingestState(world.dbPath)).get(SOURCE),
        state1,
        "its committed offset is unchanged",
      );
      const own2 = readerOwn(after2, pids);
      assert.ok(own2 > own1, "run 2's ingest picked up the readers' own records, and only those");
      assert.equal([...after2.values()].reduce((n, rows) => n + rows.length, 0), 45 + own2);

      // --- run 3: append a tail; only its records appear -------------------------------------
      const sizeBefore = (await Deno.stat(join(world.logDir, SOURCE))).size;
      await Deno.writeTextFile(
        join(world.logDir, SOURCE),
        range("tail", 7).map((id) => eventLine(id) + "\n").join(""),
        { append: true },
      );
      const third = new ReaderSession(world);
      pids.push(third.pid);
      const tailReport = resultOf(await third.ask({ id: 1, kind: "ingest" }));
      assert.equal(tailReport.linesRejected, 0);
      const run3 = await third.finish();
      assert.equal(run3.code, 0, run3.stderr);
      assertPureProtocol(run3.stdout);

      const after3 = await rowsBySource(world.dbPath);
      const sourceRows = after3.get(SOURCE) ?? [];
      assert.deepEqual(sourceRows.map((r) => r.id), [...range("src", 40), ...range("tail", 7)]);
      assert.deepEqual(
        sourceRows.filter((r) => r.offset >= sizeBefore).map((r) => r.id),
        range("tail", 7),
        "the new rows are exactly the appended tail",
      );
      readerOwn(after3, pids);
      await assertNoDuplicates(world.dbPath);

      // Each run opened the handle on its first ingest and closed it after it stopped serving.
      const records = await readerRecords(world.logDir);
      for (const pid of pids) {
        const phases = lifecyclePhases(records, pid);
        assert.deepEqual(
          phases.filter((p) => ["ready", "eventlog-opened", "stopping", "eventlog-closed"].includes(p)),
          ["ready", "eventlog-opened", "stopping", "eventlog-closed"],
          `reader ${pid}: ${phases.join(", ")}`,
        );
      }
    }),
);

test(
  "ingest failures are typed replies: a failed open is retried, a failed run commits nothing, queries keep working",
  { timeout: 120_000 },
  () =>
    withWorld(async (world) => {
      const SOURCE = await putSource(world, 3, 1, range("src", 10));
      // The event-log file's parent is a regular file, so the handle cannot open; and TMPDIR does
      // not exist, so a run cannot stage. Both are repaired between requests.
      const blocker = join(world.root, "blocker");
      await Deno.writeTextFile(blocker, "not a directory");
      const dbPath = join(blocker, "eventlog.duckdb");
      const tmpDir = join(world.root, "tmp-missing");
      const session = new ReaderSession(world, { EVENTLOG_DUCKDB: dbPath, TMPDIR: tmpDir });

      const openFailed = await session.ask({ id: 1, kind: "ingest" });
      assert.equal(openFailed.error?._tag, "IngestError", JSON.stringify(openFailed));
      assert.equal(openFailed.error?.fields.phase, "inventory");
      assertGame(await session.ask({ id: 2, ...QUERY }));

      await Deno.remove(blocker);
      const runFailed = await session.ask({ id: 3, kind: "ingest" });
      assert.equal(runFailed.error?._tag, "IngestError", JSON.stringify(runFailed));
      assert.equal(runFailed.error?.fields.phase, "read", "the open was retried; the run itself failed");
      assertGame(await session.ask({ id: 4, ...QUERY }));

      await Deno.mkdir(tmpDir);
      const recovered = resultOf(await session.ask({ id: 5, kind: "ingest" }));
      assert.equal(recovered.linesRejected, 0);
      assert.ok(recovered.linesIngested >= 10);
      assert.equal((await session.ask({ id: 6, kind: "ping" })).result, "ready");
      const run = await session.finish();
      assert.equal(run.code, 0, run.stderr);
      assert.equal(run.stderr, "", "a recoverable ingest failure never reaches stderr");
      assert.deepEqual(assertPureProtocol(run.stdout).map((r) => r.id), [1, 2, 3, 4, 5, 6]);

      const bySource = await rowsBySource(join(blocker, "eventlog.duckdb"));
      assert.deepEqual(
        bySource.get(SOURCE)?.map((r) => r.id),
        range("src", 10),
        "the failed run left nothing",
      );

      // The operation's own evidence, apart from the fixture: one failed open, then one opened
      // handle that stayed open across the failed run; and the failed run's `ingest` event.
      const records = await readerRecords(world.logDir);
      const phases = lifecyclePhases(records, session.pid);
      assert.deepEqual(
        phases.filter((p) => p.startsWith("eventlog-")),
        ["eventlog-open-failed", "eventlog-opened", "eventlog-closed"],
      );
      const failedRuns = records.filter((r) =>
        r.kind === "ingest" && r.pid === session.pid &&
        (r as { errorTag?: string }).errorTag === "IngestError"
      );
      assert.equal(failedRuns.length, 1, "the failed run emitted one ingest error event");
      await assertNoDuplicates(join(blocker, "eventlog.duckdb"));
    }),
);

test(
  "SIGTERM in the middle of an ingest: no reply, no half-committed file, handles closed; a restart ingests the rest once",
  { timeout: 240_000 },
  () =>
    withWorld(async (world) => {
      // ~15 MB in ten files: one batch, a few seconds of work, so the signal lands inside it.
      const files = 10;
      const perFile = 4000;
      const sources: string[] = [];
      for (let f = 0; f < files; f++) {
        sources.push(await putSource(world, 3, f + 1, range(`big${f}`, perFile), 150));
      }

      const session = new ReaderSession(world);
      assert.equal((await session.ask({ id: 1, kind: "ping" })).result, "ready");
      await session.send({ id: 2, kind: "ingest" });
      // The run stages into a temp directory under TMPDIR: its appearance means ingest is under way.
      let staging = false;
      for (let attempt = 0; attempt < 2000 && !staging; attempt++) {
        for await (const entry of Deno.readDir(world.tmpDir)) {
          staging ||= entry.name.startsWith("mansio-ingest-");
        }
        if (!staging) await new Promise((resolve) => setTimeout(resolve, 5));
      }
      assert.ok(staging, "ingest never started staging");
      const run = await session.stop("SIGTERM");
      assert.equal(run.code, 0, `a signal is a normal stop: ${run.stderr}`);
      assert.equal(run.stderr, "");
      assert.deepEqual(
        assertPureProtocol(run.stdout).map((r) => r.id),
        [1],
        "the interrupted ingest wrote no reply, and no partial frame",
      );

      const records = await readerRecords(world.logDir);
      const phases = lifecyclePhases(records, session.pid);
      const stopping = records.find((r) =>
        r.kind === "lifecycle" && r.pid === session.pid && (r as LifecycleEventRecord).phase === "stopping"
      ) as LifecycleEventRecord | undefined;
      assert.deepEqual(stopping?.detail, { reason: "SIGTERM" });
      assert.deepEqual(
        phases.slice(phases.indexOf("stopping")),
        ["stopping", "eventlog-closed"],
        "the event-log handle closed (connection, then instance) after serving stopped",
      );
      const leftovers: string[] = [];
      for await (const entry of Deno.readDir(world.tmpDir)) leftovers.push(entry.name);
      assert.deepEqual(leftovers, [], "the run's scoped staging directory was removed");

      // Whatever the interrupt caught, every source is all-or-nothing: rows and offset agree.
      const state = await ingestState(world.dbPath);
      const partial = await rowsBySource(world.dbPath);
      for (const source of sources) {
        const rows = partial.get(source)?.length ?? 0;
        const committed = state.get(source);
        if (committed === undefined) assert.equal(rows, 0, `${source}: rows without an offset`);
        else {
          assert.equal(committed.bytes_ingested, (await Deno.stat(join(world.logDir, source))).size);
          assert.equal(rows, perFile, `${source}: offset without all its rows`);
        }
      }

      const restart = new ReaderSession(world);
      const report = resultOf(await restart.ask({ id: 1, kind: "ingest" }));
      assert.equal(report.linesRejected, 0);
      const run2 = await restart.finish();
      assert.equal(run2.code, 0, run2.stderr);
      assertPureProtocol(run2.stdout);
      const after = await rowsBySource(world.dbPath);
      sources.forEach((source, f) =>
        assert.deepEqual(after.get(source)?.map((r) => r.id), range(`big${f}`, perFile), source)
      );
      await assertNoDuplicates(world.dbPath);
    }),
);

// ===========================================================================
// The handle's lifetime, in process, over a probe driver
// ===========================================================================

interface Probe {
  readonly log: string[];
  failOpen: number;
  failDdl: number;
  /**
   * When set, a statement matching it waits on this gate and then fails *without* reaching DuckDB:
   * the interrupted run abandons it, and a native statement still running after the scope closed
   * the connection would be a use-after-close in this process.
   */
  hold?: {
    readonly match: RegExp;
    readonly reached: Deferred.Deferred<void>;
    readonly release: Promise<void>;
  };
}

function probeDriver(probe: Probe, real: EventLogDriver): EventLogDriver {
  let instances = 0;
  return {
    open: async (path) => {
      if (probe.failOpen > 0) {
        probe.failOpen -= 1;
        probe.log.push("open failed");
        throw new Error("injected: cannot open the event log");
      }
      const instance = await real.open(path);
      const i = ++instances;
      let connections = 0;
      probe.log.push(`open instance ${i}`);
      return {
        connect: async (): Promise<EventLogConnection> => {
          const connection = await instance.connect();
          const c = `${i}.${++connections}`;
          probe.log.push(`connect ${c}`);
          return {
            run: async (sql, values) => {
              if (probe.failDdl > 0 && sql.startsWith("CREATE TABLE")) {
                probe.failDdl -= 1;
                throw new Error("injected: DDL failed");
              }
              const hold = probe.hold;
              if (hold !== undefined && hold.match.test(sql)) {
                probe.hold = undefined;
                Effect.runSync(Deferred.succeed(hold.reached, undefined));
                await hold.release;
                throw new Error("injected: statement abandoned by an interrupted run");
              }
              return await connection.run(sql, values);
            },
            runAndReadAll: (sql) => connection.runAndReadAll(sql),
            closeSync: () => {
              probe.log.push(`close connection ${c}`);
              connection.closeSync();
            },
          };
        },
        closeSync: () => {
          probe.log.push(`close instance ${i}`);
          instance.closeSync();
        },
      };
    },
  };
}

test(
  "the event-log handle opens on first ingest, is not cached when its open fails, and closes connection before instance even mid-ingest",
  { timeout: 120_000 },
  async () => {
    const { eventLogDriver } = await import("../../platform/eventlog/ingest.ts");
    const { makeReaderEventLog } = await import("../../platform/reader_layers.ts");
    await withWorld(async (world) => {
      const SOURCE = await putSource(world, 3, 1, range("src", 20));
      const probe: Probe = { log: [], failOpen: 1, failDdl: 0 };
      const emitted: EventRecord[] = [];
      const services = Layer.mergeAll(
        ConfigTest({ logging: { dir: world.logDir }, eventLog: { duckdbPath: world.dbPath } }),
        Layer.succeed(EventSink, { emit: (event) => Effect.sync(() => void emitted.push(event)) }),
      );
      const phases = () =>
        emitted.flatMap((e) => e.kind === "lifecycle" ? [(e as LifecycleEventRecord).phase] : []);

      const program = Effect.gen(function* () {
        const scope = yield* Scope.make();
        const service = yield* Scope.extend(
          makeReaderEventLog({ driver: probeDriver(probe, eventLogDriver) }),
          scope,
        );
        const ingest = Effect.either(service.ingest) as Effect.Effect<
          Either.Either<IngestThenSweepReport, IngestError>
        >;
        assert.deepEqual(probe.log, [], "nothing opens before the first ingest");

        // 1. The driver refuses: a typed failure, and nothing is cached.
        const refused = yield* ingest;
        assert.ok(Either.isLeft(refused) && refused.left.phase === "inventory", String(refused));
        // 2. The instance opens but the schema step fails: the connection, then the instance, close.
        probe.failDdl = 1;
        const ddl = yield* ingest;
        assert.ok(Either.isLeft(ddl), "a failed DDL fails that ingest");
        assert.deepEqual(probe.log, [
          "open failed",
          "open instance 1",
          "connect 1.1",
          "close connection 1.1",
          "close instance 1",
        ]);
        assert.deepEqual(phases(), ["eventlog-open-failed", "eventlog-open-failed"]);
        // 3. Opens, ingests, and stays open for the next run.
        const first = yield* ingest;
        assert.ok(Either.isRight(first) && first.right.linesIngested === 20, String(first));
        const second = yield* ingest;
        assert.ok(Either.isRight(second) && second.right.linesIngested === 0, "nothing new the second time");
        assert.deepEqual(probe.log.slice(5), ["open instance 2", "connect 2.1"], "one persistent handle");

        // 4. Interrupted inside a run's transaction (at the staging statement), then the scope closes.
        yield* Effect.promise(() =>
          Deno.writeTextFile(
            join(world.logDir, SOURCE),
            range("tail", 5).map((id) => eventLine(id) + "\n").join(""),
            {
              append: true,
            },
          )
        );
        const reached = yield* Deferred.make<void>();
        let release!: () => void;
        probe.hold = {
          match: /CREATE OR REPLACE TEMP TABLE/,
          reached,
          release: new Promise((r) => (release = r)),
        };
        const held = yield* Effect.fork(service.ingest);
        yield* Deferred.await(reached);
        const interrupting = yield* Effect.fork(Fiber.interrupt(held));
        release();
        const exit = yield* Fiber.join(interrupting);
        assert.ok(Exit.isInterrupted(exit), `the held ingest was interrupted: ${String(exit)}`);
        yield* Scope.close(scope, Exit.void);
        assert.deepEqual(
          probe.log.slice(5),
          ["open instance 2", "connect 2.1", "close connection 2.1", "close instance 2"],
          "connection before instance, every handle closed",
        );
        assert.equal(phases().at(-1), "eventlog-closed");
      });
      await Effect.runPromise(Effect.provide(program, services));

      // The interrupted batch rolled back; a fresh process-equivalent scope ingests the tail once.
      const bySource = await rowsBySource(world.dbPath);
      assert.deepEqual(bySource.get(SOURCE)?.map((r) => r.id), range("src", 20));
      const again = await Effect.runPromise(
        Effect.scoped(Effect.flatMap(makeReaderEventLog(), (s) => s.ingest)).pipe(Effect.provide(services)),
      );
      assert.equal(again.linesIngested, 5);
      assert.deepEqual((await rowsBySource(world.dbPath)).get(SOURCE)?.map((r) => r.id), [
        ...range("src", 20),
        ...range("tail", 5),
      ]);
    });
  },
);
