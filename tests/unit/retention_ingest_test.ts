/**
 * Retention follows committed ingest (plan item 36, ADR 0007), against a real `LOG_DIR` tree and a
 * real, persistent DuckDB event-log file, through the reader's coordinator:
 * `makeEventLogIngest(...).ingestThenSweep` — ingest, seal closed torn tails, then sweep with the
 * committed state as the gate's ledger. No item-23 CLI is involved.
 *
 * Every run opens and closes its own scope, so every run is also a restart: offsets, quarantine and
 * the tree are all that carry over. Fault injection wraps the real DuckDB driver (as
 * `tests/unit/eventlog_test.ts` does) to fail a `COMMIT`, or to change the tree at an exact point
 * inside a run — after ingest committed, or after the gate read a file but before it was deleted.
 *
 * The last test is the bounded sustained-write run: the real logger (`eventSinkLayer`, with its
 * dropping queue) writes through many hour, date and size rotations under a virtual clock while a
 * foreign writer adds malformed lines, and ingest-then-sweep runs once per virtual day. It prints
 * input, ingested, rejected and dropped counts with the JSONL and DuckDB sizes after every sweep, and
 * asserts the JSONL cap only as far as closed-file safety lets the plan enforce it.
 *
 * DuckDB runs in this test process; that is safe because event-log ingest never loads DuckDB's
 * `sqlite` extension (see `tests/unit/eventlog_test.ts`).
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { dirname, join, relative } from "node:path";
import { gzipSync } from "node:zlib";
import { DuckDBInstance } from "@duckdb/node-api";
import { Clock, Effect, Either, Layer, Ref, TestClock, TestContext } from "effect";
import type { IngestError } from "../../core/errors.ts";
import { type EventRecord, lifecycleEvent, toJsonLine } from "../../core/events.ts";
import { ConfigTest } from "../../eventlog/config.ts";
import { EventSink } from "../../eventlog/services.ts";
import {
  type EventLogConnection,
  type EventLogDriver,
  eventLogDriver,
  type IngestThenSweepReport,
  makeEventLogIngest,
  type RetentionBudgets,
  type RetentionCoordinatorOptions,
} from "../../eventlog/ingest.ts";
import { eventSinkLayer } from "../../eventlog/logger.ts";
import type { SweepReport } from "../../eventlog/retention.ts";

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

/** "Now" for every run but the sustained one: 2026-10-11 12:00 local. */
const NOW = new Date(2026, 9, 11, 12, 0, 0);
const DAY_MS = 24 * 60 * 60 * 1000;
/** `keepDays` 7 and `gzipAfterDays` 2: a file 10 days old is deleted, one 4 days old is gzipped. */
const LOGGING = { retentionDays: 7, gzipAfterDays: 2, maxTotalBytes: Number.MAX_SAFE_INTEGER };
const OLD = "server/2026-10-01/events-09-100-001.jsonl"; // age 10: delete
const MID = "server/2026-10-07/events-09-100-001.jsonl"; // age 4: gzip
const TODAY = "server/2026-10-11/events-09-100-001.jsonl"; // today: never touched

interface World {
  readonly root: string;
  readonly logDir: string;
  readonly dbPath: string;
  /** Every event a run emitted, in order. */
  readonly events: EventRecord[];
}

async function withWorld(body: (world: World) => Promise<void>): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "retention-ingest-test-" });
  try {
    // The event-log file sits outside LOG_DIR, so a snapshot of the tree is exactly the JSONL.
    await body({
      root,
      logDir: join(root, "logs"),
      dbPath: join(root, "duckdb", "events.duckdb"),
      events: [],
    });
  } finally {
    await Deno.remove(root, { recursive: true });
  }
}

let nextId = 0;

/** One ingestible line: a real `lifecycle` record with a unique `eventId`. */
function eventLine(id = `ev-${++nextId}`): string {
  return toJsonLine(lifecycleEvent({
    eventId: id,
    ts: "2026-10-01T09:00:00.000Z",
    level: "info",
    role: "server",
    pid: 100,
    traceId: "t-1",
    phase: "fixture",
    detail: { id },
  }));
}

const idOf = (line: string) => (JSON.parse(line) as { eventId: string }).eventId;

/** Writes `content` at `rel` under LOG_DIR with an mtime of `mtime` (default: the file's own day, settled). */
async function put(world: World, rel: string, content: string | Uint8Array, mtime?: Date): Promise<string> {
  const path = join(world.logDir, rel);
  await Deno.mkdir(dirname(path), { recursive: true });
  await Deno.writeFile(path, typeof content === "string" ? new TextEncoder().encode(content) : content);
  const [, date] = rel.split("/");
  const stamp = mtime ?? new Date(`${date}T09:30:00`);
  await Deno.utime(path, stamp, stamp);
  return path;
}

async function exists(path: string): Promise<boolean> {
  try {
    await Deno.lstat(path);
    return true;
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return false;
    throw cause;
  }
}

/** Type, size, mtime and hash of every file under LOG_DIR. */
async function snapshot(dir: string): Promise<string[]> {
  const rows: string[] = [];
  const walk = async (at: string): Promise<void> => {
    for await (const entry of Deno.readDir(at)) {
      const full = join(at, entry.name);
      const info = await Deno.lstat(full);
      if (info.isDirectory) {
        rows.push(`d ${relative(dir, full)}`);
        await walk(full);
      } else {
        const hash = createHash("sha256").update(await Deno.readFile(full)).digest("hex");
        rows.push(`f ${relative(dir, full)} ${info.size} ${info.mtime?.getTime()} ${hash}`);
      }
    }
  };
  await walk(dir);
  return rows.sort();
}

/** Wraps the real driver so a test can act around any statement. */
interface Hooks {
  /** Runs before the statement; throwing fails it as DuckDB would. */
  readonly before?: (sql: string) => Promise<void> | void;
  /** Runs after the statement succeeded. */
  readonly after?: (sql: string) => Promise<void> | void;
}

function hookedDriver(hooks: Hooks): EventLogDriver {
  return {
    open: async (path) => {
      const instance = await eventLogDriver.open(path);
      return {
        closeSync: () => instance.closeSync(),
        connect: async (): Promise<EventLogConnection> => {
          const connection = await instance.connect();
          const around = async <A>(sql: string, run: () => Promise<A>): Promise<A> => {
            await hooks.before?.(sql);
            const result = await run();
            await hooks.after?.(sql);
            return result;
          };
          return {
            closeSync: () => connection.closeSync(),
            run: (sql, values) => around(sql, () => connection.run(sql, values)),
            runAndReadAll: (sql) => around(sql, () => connection.runAndReadAll(sql)),
          };
        },
      };
    },
  };
}

interface RunOptions {
  readonly driver?: EventLogDriver;
  readonly dryRun?: boolean;
  readonly retention?: RetentionCoordinatorOptions;
  /** Plain `ingest` instead of `ingestThenSweep`. */
  readonly ingestOnly?: boolean;
  readonly at?: Date;
}

const configFor = (world: World) =>
  ConfigTest({ logging: { dir: world.logDir, ...LOGGING }, eventLog: { duckdbPath: world.dbPath } });

/** One coordinator run at `NOW` (by `TestClock`), in its own scope — so each run is a restart. */
async function runEither(
  world: World,
  options: RunOptions = {},
): Promise<Either.Either<IngestThenSweepReport, IngestError>> {
  const program = Effect.gen(function* () {
    const sunk = yield* Ref.make<EventRecord[]>([]);
    const sink = Layer.succeed(EventSink, {
      emit: (event: EventRecord) => Ref.update(sunk, (all) => [...all, event]),
    });
    yield* TestClock.setTime((options.at ?? NOW).getTime());
    const result = yield* Effect.either(
      Effect.scoped(Effect.flatMap(
        makeEventLogIngest({ driver: options.driver, retention: options.retention }),
        (control): Effect.Effect<IngestThenSweepReport, IngestError> =>
          options.ingestOnly === true
            ? Effect.map(control.ingest, (report) => report as IngestThenSweepReport)
            : control.ingestThenSweep({ dryRun: options.dryRun }),
      )).pipe(Effect.provide(Layer.merge(configFor(world), sink))),
    );
    world.events.push(...(yield* Ref.get(sunk)));
    return result;
  });
  return await Effect.runPromise(program.pipe(Effect.provide(TestContext.TestContext)));
}

async function run(world: World, options: RunOptions = {}): Promise<IngestThenSweepReport> {
  const result = await runEither(world, options);
  if (Either.isLeft(result)) throw new Error(`run failed: ${JSON.stringify(result.left)}`);
  return result.right;
}

function swept(report: IngestThenSweepReport): SweepReport {
  if (report.retention.status !== "swept") {
    throw new Error(`sweep failed: ${JSON.stringify(report.retention)}`);
  }
  return report.retention.sweep;
}

function budgetsOf(report: IngestThenSweepReport): RetentionBudgets {
  if (report.budgets === undefined) throw new Error(`budgets not measured: ${report.budgetsError}`);
  return report.budgets;
}

type Row = Record<string, unknown>;

async function query(world: World, sql: string): Promise<Row[]> {
  const instance = await DuckDBInstance.create(world.dbPath);
  try {
    const connection = await instance.connect();
    try {
      return (await connection.runAndReadAll(sql)).getRowObjects();
    } finally {
      connection.closeSync();
    }
  } finally {
    instance.closeSync();
  }
}

/** Every `event_id` in `platform_events`, with how many times it is there. */
async function ingestedIds(world: World): Promise<Map<string, number>> {
  const rows = await query(world, "SELECT event_id, count(*) AS n FROM platform_events GROUP BY 1");
  return new Map(rows.map((r) => [String(r.event_id), Number(r.n)]));
}

/** Asserts each id is ingested exactly once. */
async function assertIngestedOnce(world: World, ids: readonly string[]): Promise<void> {
  const found = await ingestedIds(world);
  for (const id of ids) assert.equal(found.get(id), 1, `${id} must be ingested exactly once`);
}

/** The quarantine records under `rel`'s directory. */
async function quarantined(world: World, rel: string): Promise<Array<Record<string, unknown>>> {
  const path = join(world.logDir, dirname(rel), "rejected.jsonl");
  if (!(await exists(path))) return [];
  return (await Deno.readTextFile(path)).split("\n").filter((l) => l !== "").map((l) => JSON.parse(l));
}

const lines = (n: number) => Array.from({ length: n }, () => eventLine());
const jsonl = (ls: readonly string[]) => ls.map((l) => `${l}\n`).join("");

/** The standard three-file tree: old (delete), mid (gzip) and today, each fully formed. */
async function standardTree(world: World) {
  const old = lines(5);
  const mid = lines(5);
  const today = lines(5);
  await put(world, OLD, jsonl(old));
  await put(world, MID, jsonl(mid));
  await put(world, TODAY, jsonl(today), new Date(NOW.getTime() - 60 * 60 * 1000));
  return { old, mid, today, all: [...old, ...mid, ...today].map(idOf) };
}

const sweepEvents = (world: World) =>
  world.events.flatMap((e) => e.kind === "lifecycle" && e.phase.startsWith("retention-sweep") ? [e] : []);

// ---------------------------------------------------------------------------
// The basic contract
// ---------------------------------------------------------------------------

test("ingest, then sweep: what is committed goes; today stays; nothing is ingested twice", async () => {
  await withWorld(async (world) => {
    const tree = await standardTree(world);

    const first = await run(world);
    const sweep = swept(first);

    assert.equal(first.linesIngested, 15);
    assert.deepEqual(sweep.deleted, [OLD]);
    assert.deepEqual(sweep.gzipped, [MID]);
    assert.deepEqual(sweep.blocked, []);
    assert.equal(await exists(join(world.logDir, `${MID}.gz`)), true);
    assert.equal(await exists(join(world.logDir, TODAY)), true);
    await assertIngestedOnce(world, tree.all);

    // A second run (and a restart) finds nothing new and leaves the tree exactly as it is.
    const before = await snapshot(world.logDir);
    const second = await run(world);
    assert.equal(second.linesIngested, 0);
    assert.deepEqual([swept(second).deleted, swept(second).gzipped], [[], []]);
    assert.deepEqual(await snapshot(world.logDir), before);
    await assertIngestedOnce(world, tree.all);

    // The budgets: JSONL is what is left on disk; the derived file is measured, not capped.
    assert.equal(budgetsOf(second).derived.platformEventRows, 15);
    assert.equal(budgetsOf(second).derived.boundedByLogMaxTotalBytes, false);
    assert.ok(budgetsOf(second).derived.duckdbBytes > 0);
    assert.equal(budgetsOf(second).jsonl.files, 2);
  });
});

test("restart: a sweep in a fresh process trusts only offsets that were committed before it", async () => {
  await withWorld(async (world) => {
    const tree = await standardTree(world);
    // Process 1 only ingests; process 2 only plans; process 3 sweeps; process 4 re-runs.
    await run(world, { ingestOnly: true });
    const planned = swept(await run(world, { dryRun: true }));
    assert.deepEqual([planned.plan.delete, planned.plan.gzip], [[OLD], [MID]]);
    assert.equal(await exists(join(world.logDir, OLD)), true, "a dry run touches nothing");

    const third = await run(world);
    assert.equal(third.linesIngested, 0, "the restart re-read nothing it had committed");
    assert.deepEqual(swept(third).deleted, [OLD]);
    const fourth = await run(world);
    assert.equal(fourth.linesIngested, 0);
    await assertIngestedOnce(world, tree.all);
  });
});

test("dry run: nothing is ingested, sealed, touched or emitted; the plan is from committed state", async () => {
  await withWorld(async (world) => {
    await standardTree(world);
    // Nothing committed yet: a dry run sees every candidate blocked for lack of a watermark.
    const cold = swept(await run(world, { dryRun: true }));
    assert.equal(cold.dryRun, true);
    assert.deepEqual(cold.blocked.map((b) => [b.path, b.reason]), [[OLD, "no-watermark"], [
      MID,
      "no-watermark",
    ]]);

    await run(world, { ingestOnly: true });
    // A torn tail on a closed file is something a real run would seal; a dry run must not.
    await Deno.writeTextFile(join(world.logDir, OLD), '{"torn', { append: true });
    await Deno.utime(
      join(world.logDir, OLD),
      new Date("2026-10-01T10:00:00"),
      new Date("2026-10-01T10:00:00"),
    );
    const before = await snapshot(world.logDir);
    world.events.length = 0;

    const warm = await run(world, { dryRun: true });

    assert.deepEqual(await snapshot(world.logDir), before);
    assert.equal(warm.linesIngested, 0);
    assert.equal(warm.retention.sealed.length, 0);
    assert.deepEqual(swept(warm).plan.gzip, [MID]);
    assert.deepEqual(swept(warm).blocked.map((b) => [b.path, b.reason]), [[OLD, "unread-tail"]]);
    assert.deepEqual(world.events, [], "a dry run emits nothing");
  });
});

// ---------------------------------------------------------------------------
// What blocks destructive action
// ---------------------------------------------------------------------------

test("append during the sweep: a file that grows after the gate read it is skipped, then swept", async () => {
  await withWorld(async (world) => {
    const tree = await standardTree(world);
    const late = eventLine();
    let appended = false;
    // `acceptedOffsets` runs after the gate read every candidate and before anything is removed.
    const driver = hookedDriver({
      after: async (sql) => {
        if (appended || !sql.includes("SELECT source_file, source_offset FROM platform_events")) return;
        appended = true;
        await Deno.writeTextFile(join(world.logDir, OLD), `${late}\n`, { append: true });
      },
    });

    const report = swept(await run(world, { driver }));

    assert.equal(appended, true);
    assert.deepEqual(report.deleted, []);
    assert.deepEqual(report.gzipped, [MID]);
    assert.deepEqual(report.skipped.map((s) => [s.path, s.action, s.reason]), [
      [OLD, "delete", "changed since inventory"],
    ]);
    assert.equal(await exists(join(world.logDir, OLD)), true, "the appended bytes were not deleted");

    // The next run ingests the late line, then the file goes.
    await Deno.utime(
      join(world.logDir, OLD),
      new Date("2026-10-01T10:00:00"),
      new Date("2026-10-01T10:00:00"),
    );
    const next = await run(world);
    assert.equal(next.linesIngested, 1);
    assert.deepEqual(swept(next).deleted, [OLD]);
    await assertIngestedOnce(world, [...tree.all, idOf(late)]);
  });
});

test("append after ingest committed: the stale watermark blocks the sweep until the next ingest", async () => {
  await withWorld(async (world) => {
    const tree = await standardTree(world);
    const late = eventLine();
    let appended = false;
    const driver = hookedDriver({
      after: async (sql) => {
        if (appended || sql !== "COMMIT") return;
        appended = true;
        await Deno.writeTextFile(join(world.logDir, OLD), `${late}\n`, { append: true });
        await Deno.utime(
          join(world.logDir, OLD),
          new Date("2026-10-01T10:00:00"),
          new Date("2026-10-01T10:00:00"),
        );
      },
    });

    const report = swept(await run(world, { driver }));

    assert.deepEqual(report.blocked.map((b) => [b.path, b.reason]), [[OLD, "unread-tail"]]);
    assert.equal(await exists(join(world.logDir, OLD)), true);
    const next = await run(world);
    assert.equal(next.linesIngested, 1);
    assert.deepEqual(swept(next).deleted, [OLD]);
    await assertIngestedOnce(world, [...tree.all, idOf(late)]);
  });
});

test("an unread partial tail blocks the sweep; once the file is closed it is sealed, then swept", async () => {
  await withWorld(async (world) => {
    const complete = lines(3);
    const torn = '{"eventId":"never-finished","ts":"2026-10-01T09:';
    // Modified a minute ago: a write may still be landing, so the file is not yet closed.
    const path = await put(world, OLD, `${jsonl(complete)}${torn}`, new Date(NOW.getTime() - 60_000));

    const open = await run(world);
    assert.equal(open.linesIngested, 3);
    assert.deepEqual(open.retention.sealed, []);
    assert.deepEqual(swept(open).blocked.map((b) => [b.path, b.reason]), [[OLD, "unsettled"]]);
    assert.equal(await exists(path), true);
    assert.deepEqual(await quarantined(world, OLD), []);

    // Settled: no sink will ever finish that line. It is quarantined, then the file is swept.
    await Deno.utime(path, new Date("2026-10-01T10:00:00"), new Date("2026-10-01T10:00:00"));
    const closed = await run(world);
    assert.deepEqual(closed.retention.sealed, [OLD]);
    assert.deepEqual(swept(closed).deleted, [OLD, "server/2026-10-01/rejected.jsonl"]);
    assert.equal(await exists(path), false);
    await assertIngestedOnce(world, complete.map(idOf));
    const sealed = world.events.filter((e) => e.kind === "ingest" && e.errorTag === "TornTailSealed");
    assert.equal(sealed.length, 1);
    assert.equal((sealed[0] as { sourceFile: string }).sourceFile, OLD);
  });
});

test("a sealed torn tail is durable quarantine evidence; a tail with complete lines is never sealed", async () => {
  await withWorld(async (world) => {
    const complete = lines(2);
    const torn = '{"eventId":"half';
    // A gzip-age directory keeps its quarantine file, so the evidence can be inspected after.
    const path = await put(world, MID, `${jsonl(complete)}${torn}`);
    const report = await run(world);

    assert.deepEqual(report.retention.sealed, [MID]);
    const records = await quarantined(world, MID);
    assert.equal(records.length, 1);
    assert.equal(records[0].reason, "torn-tail");
    assert.equal(records[0].line, torn);
    assert.equal(records[0].sourceOffset, new TextEncoder().encode(jsonl(complete)).length);
    // Fully accounted for (two rows + one reject), so it is compressed, byte offsets intact.
    assert.deepEqual(swept(report).gzipped, [MID]);
    assert.equal(await exists(path), false);
    const [state] = await query(
      world,
      `SELECT bytes_ingested FROM _ingest_state WHERE source_file = '${MID}'`,
    );
    assert.equal(Number(state.bytes_ingested), new TextEncoder().encode(`${jsonl(complete)}${torn}`).length);

    // A file whose unread part holds a newline is ingest's to read, not a torn write to seal: here a
    // complete line and a partial one land after ingest committed, just before sealing looks.
    const other = "server/2026-10-07/events-10-100-001.jsonl";
    const otherPath = await put(world, other, jsonl(lines(1)));
    const next = eventLine();
    let appended = false;
    const driver = hookedDriver({
      after: async (sql) => {
        if (appended || sql !== "COMMIT") return;
        appended = true;
        await Deno.writeTextFile(otherPath, `${next}\n{"part`, { append: true });
        await Deno.utime(otherPath, new Date("2026-10-07T10:00:00"), new Date("2026-10-07T10:00:00"));
      },
    });
    const racing = await run(world, { driver });
    assert.equal(appended, true);
    assert.deepEqual(racing.retention.sealed, []);
    assert.deepEqual(swept(racing).blocked.map((b) => [b.path, b.reason]), [[other, "unread-tail"]]);
    assert.deepEqual((await quarantined(world, other)).filter((r) => r.sourceFile === other), []);
    // The next run reads the complete line; only then is the partial one a torn tail to seal.
    const after = await run(world);
    assert.equal(after.linesIngested, 1);
    assert.deepEqual(after.retention.sealed, [other]);
    assert.deepEqual(swept(after).gzipped, [other]);
    await assertIngestedOnce(world, [...complete.map(idOf), idOf(next)]);
  });
});

test("a failed commit sweeps nothing and loses nothing; the retry ingests and sweeps", async () => {
  await withWorld(async (world) => {
    const tree = await standardTree(world);
    const before = await snapshot(world.logDir);
    const failing = hookedDriver({
      before: (sql) => {
        if (sql === "COMMIT") throw new Error("injected commit failure");
      },
    });

    const failed = await runEither(world, { driver: failing });

    assert.equal(Either.isLeft(failed), true);
    if (Either.isLeft(failed)) assert.equal(failed.left.phase, "commit");
    assert.deepEqual(await snapshot(world.logDir), before, "no file touched");
    assert.deepEqual(sweepEvents(world), [], "no sweep ran");
    assert.equal((await query(world, "SELECT count(*) AS n FROM platform_events"))[0].n, 0n);

    const retry = await run(world);
    assert.equal(retry.linesIngested, 15);
    assert.deepEqual(swept(retry).deleted, [OLD]);
    await assertIngestedOnce(world, tree.all);
  });
});

test("the active file and today's directory are never sealed, gzipped or deleted", async () => {
  await withWorld(async (world) => {
    const tree = await standardTree(world);
    // A sink still holds the old file open (idle since before midnight), with a torn tail.
    await Deno.writeTextFile(join(world.logDir, OLD), '{"in-flight', { append: true });
    await Deno.utime(
      join(world.logDir, OLD),
      new Date("2026-10-01T10:00:00"),
      new Date("2026-10-01T10:00:00"),
    );
    // Today's file has a torn tail too; today is never sealed or swept, whatever its age.
    await Deno.writeTextFile(join(world.logDir, TODAY), '{"in-flight', { append: true });
    const openPaths = Effect.succeed([join(world.logDir, OLD)]);

    const report = await run(world, { retention: { openPaths, settleMs: 0 } });

    assert.deepEqual(report.retention.sealed, []);
    assert.deepEqual(swept(report).blocked.map((b) => [b.path, b.reason]), [[OLD, "open"]]);
    assert.deepEqual(swept(report).gzipped, [MID]);
    assert.equal(await exists(join(world.logDir, OLD)), true);
    assert.equal(await exists(join(world.logDir, TODAY)), true);
    assert.ok(!swept(report).plan.delete.includes(TODAY) && !swept(report).plan.gzip.includes(TODAY));
    await assertIngestedOnce(world, tree.all);
  });
});

// ---------------------------------------------------------------------------
// Rejected lines
// ---------------------------------------------------------------------------

test("rejected lines are reconciled against durable quarantine before their source goes", async () => {
  await withWorld(async (world) => {
    const good = lines(3);
    const content = `${good[0]}\nnot json at all\n${good[1]}\n{"no":"event id"}\n${good[2]}\n`;
    await put(world, OLD, content);
    const otherDay = "server/2026-10-02/events-09-100-001.jsonl";
    await put(world, otherDay, `${eventLine()}\nbroken {\n`);
    await run(world, { ingestOnly: true });

    // The quarantine of 10-02 is lost: its rejected line is no longer accounted for, so that source
    // stays; 10-01's two rejects are on record, so it goes — quarantine file last.
    const lostEvidence = join(world.logDir, "server/2026-10-02/rejected.jsonl");
    assert.equal((await quarantined(world, OLD)).length, 2);
    await Deno.remove(lostEvidence);
    const report = swept(await run(world));

    assert.deepEqual(report.deleted, [OLD, "server/2026-10-01/rejected.jsonl"]);
    assert.deepEqual(report.blocked.map((b) => [b.path, b.reason]), [[otherDay, "unreconciled"]]);
    assert.equal(await exists(join(world.logDir, otherDay)), true);
    assert.equal(await exists(join(world.logDir, "server/2026-10-01")), false);
    await assertIngestedOnce(world, good.map(idOf));
  });
});

test("a quarantine file left with a torn record still takes whole records after it", async () => {
  await withWorld(async (world) => {
    const good = lines(2);
    await put(world, OLD, `${good[0]}\nnot json\n${good[1]}\n`);
    // A quarantine write torn by a full disk: a fragment with no terminator, then the batch rolled back.
    await put(world, "server/2026-10-01/rejected.jsonl", '{"rejectedAt":"2026-');

    const report = swept(await run(world));

    // The retried record starts on its own line, so it counts as evidence and the source can go.
    assert.deepEqual(report.blocked, []);
    assert.deepEqual(report.deleted, [OLD, "server/2026-10-01/rejected.jsonl"]);
    await assertIngestedOnce(world, good.map(idOf));
  });
});

test("an empty closed file gets an empty-prefix row, so it no longer pins its directory", async () => {
  await withWorld(async (world) => {
    const good = lines(2);
    const empty = "server/2026-10-01/events-10-100-001.jsonl";
    await put(world, OLD, `${good[0]}\nnot json\n${good[1]}\n`);
    await put(world, empty, "");

    // Plain ingest never writes a row for a 0-byte file.
    await run(world, { ingestOnly: true });
    const rowOf = () =>
      query(world, `SELECT bytes_ingested, prefix_sha256 FROM _ingest_state WHERE source_file = '${empty}'`);
    assert.deepEqual(await rowOf(), []);
    world.events.length = 0;

    const report = await run(world);

    // The coordinator commits a complete empty-prefix watermark for it, so the gate passes it, and
    // the directory's quarantine file and the directory go with it.
    const [state] = await rowOf();
    assert.equal(Number(state.bytes_ingested), 0);
    assert.equal(state.prefix_sha256, createHash("sha256").update(new Uint8Array()).digest("hex"));
    assert.deepEqual(report.retention.sealed, []);
    assert.deepEqual(swept(report).blocked, []);
    assert.deepEqual(
      [...swept(report).deleted].sort(),
      [OLD, empty, "server/2026-10-01/rejected.jsonl"].sort(),
    );
    assert.deepEqual(swept(report).removedDirs, ["server/2026-10-01"]);
    assert.deepEqual(world.events.filter((e) => e.level === "warn"), [], "nothing left to warn about");
    await assertIngestedOnce(world, good.map(idOf));
  });
});

// ---------------------------------------------------------------------------
// Gzip crash and retry
// ---------------------------------------------------------------------------

test("gzip retry: a crash after the rename, or before it, is finished by the next run", async () => {
  await withWorld(async (world) => {
    const tree = await standardTree(world);
    await run(world, { ingestOnly: true });
    const mid = join(world.logDir, MID);
    // Crash after the rename: a complete `.gz` beside its source.
    const gz = `${mid}.gz`;
    await Deno.writeFile(gz, gzipSync(await Deno.readFile(mid)));
    await Deno.utime(gz, new Date("2026-10-07T09:30:00"), new Date("2026-10-07T09:30:00"));
    // Crash before the rename, by another process, long ago: its temp file.
    const tmp = `${mid}.gz.999.tmp`;
    await Deno.writeTextFile(tmp, "partial");
    await Deno.utime(tmp, new Date("2026-10-07T09:30:00"), new Date("2026-10-07T09:30:00"));

    // While both exist, ingest sees only the plain source, which it has fully read.
    const report = await run(world);

    assert.equal(report.linesIngested, 0);
    assert.deepEqual(swept(report).gzipped, [MID]);
    assert.ok(swept(report).deleted.includes(relative(world.logDir, tmp)));
    assert.deepEqual(swept(report).skipped, []);
    assert.equal(await exists(mid), false);
    assert.equal(await exists(gz), true);
    // Gunzipped back in place, the source is recognised by its kept offset row: nothing re-ingested.
    await Deno.writeFile(
      mid,
      new Uint8Array(
        await new Response(
          (await Deno.open(gz)).readable.pipeThrough(new DecompressionStream("gzip")),
        ).arrayBuffer(),
      ),
    );
    await Deno.remove(gz);
    assert.equal((await run(world, { ingestOnly: true })).linesIngested, 0);
    await assertIngestedOnce(world, tree.all);
  });
});

// ---------------------------------------------------------------------------
// Schema compatibility (item 35)
// ---------------------------------------------------------------------------

test("an incompatible event-log schema stops ingest at startup, so nothing is swept", async () => {
  await withWorld(async (world) => {
    await standardTree(world);
    await Deno.mkdir(dirname(world.dbPath), { recursive: true });
    // A v1 file: event-log tables and no version table.
    const instance = await DuckDBInstance.create(world.dbPath);
    const connection = await instance.connect();
    await connection.run(
      "CREATE TABLE _ingest_state (source_file VARCHAR PRIMARY KEY, bytes_ingested BIGINT)",
    );
    connection.closeSync();
    instance.closeSync();
    const before = await snapshot(world.logDir);

    const result = await runEither(world);

    assert.equal(Either.isLeft(result), true);
    assert.deepEqual(await snapshot(world.logDir), before);
  });
});

// ---------------------------------------------------------------------------
// The bounded sustained-write run
// ---------------------------------------------------------------------------

/** A clock whose time is set by the test, and whose sleeps are real: the logger's fiber keeps flushing. */
function virtualClock(start: number) {
  const real = Clock.make();
  const state = { ms: start };
  const clock: Clock.Clock = {
    [Clock.ClockTypeId]: Clock.ClockTypeId,
    unsafeCurrentTimeMillis: () => state.ms,
    currentTimeMillis: Effect.sync(() => state.ms),
    unsafeCurrentTimeNanos: () => BigInt(state.ms) * 1_000_000n,
    currentTimeNanos: Effect.sync(() => BigInt(state.ms) * 1_000_000n),
    sleep: (duration) => real.sleep(duration),
  };
  return { clock, state };
}

const realSleep = (ms: number) => Effect.promise(() => new Promise((resolve) => setTimeout(resolve, ms)));

test("sustained writes over many rotations and sweeps: every accepted event ingested once, drops apart", async () => {
  await withWorld(async (world) => {
    const DAYS = 7;
    const HOURS = [8, 12, 16, 20];
    const BURSTS = 2;
    const BURST = 100;
    // About two ordinary days of writes. Day 3 writes twice as much, so the sweeps around it must
    // prune by size as well as by age, while ordinary days are gzipped and kept.
    const capBytes = 384 * 1024;
    const logging = {
      dir: world.logDir,
      retentionDays: 3,
      gzipAfterDays: 1,
      maxTotalBytes: capBytes,
      maxFileBytes: 16 * 1024,
      eventQueueCapacity: 64,
      eventFlushMs: 5,
    };
    const config = ConfigTest({ logging, eventLog: { duckdbPath: world.dbPath } });
    // Days ahead of the real clock, so every real mtime is long settled by virtual time.
    const start = new Date(Date.now() + 2 * DAY_MS);
    start.setHours(0, 0, 0, 0);
    const { clock, state } = virtualClock(start.getTime());
    const held: string[] = [];

    let emitted = 0;
    let foreignGood = 0;
    let foreignBad = 0;
    let rejectedReported = 0;
    let sweeps = 0;
    let blockedSeen = 0;
    let capPruned = 0;
    let gzipped = 0;

    // The coordinator holds the event-log file open; a second instance on it would conflict, so the
    // test reads through a second connection to the coordinator's own instance.
    let instance: { connect(): Promise<EventLogConnection> } | undefined;
    const driver: EventLogDriver = {
      open: async (path) => {
        const opened = await eventLogDriver.open(path);
        instance = opened;
        return opened;
      },
    };
    const sideQuery = (sql: string) =>
      Effect.promise(async () => {
        const connection = await instance!.connect();
        try {
          return (await connection.runAndReadAll(sql)).getRowObjects();
        } finally {
          connection.closeSync();
        }
      });

    const program = Effect.gen(function* () {
      const sink = yield* EventSink;
      const control = yield* makeEventLogIngest({
        driver,
        retention: { openPaths: Effect.sync(() => [...held]) },
      });
      const appRows = () =>
        Effect.map(
          sideQuery(
            "SELECT count(*) AS n, count(DISTINCT event_id) AS d FROM platform_events WHERE name = 'sustained-load'",
          ),
          ([row]) => row,
        );

      for (let day = 0; day < DAYS + 2; day++) {
        const writing = day < DAYS;
        for (const hour of writing ? HOURS : []) {
          state.ms = start.getTime() + day * DAY_MS + hour * 60 * 60 * 1000;
          const now = new Date(state.ms);
          for (let burst = 0; burst < (day === 3 ? 2 * BURSTS : BURSTS); burst++) {
            // One synchronous burst: the consumer cannot run inside it, so what does not fit is dropped.
            yield* Effect.sync(() => {
              for (let i = 0; i < BURST; i++) {
                emitted += 1;
                Effect.runSync(sink.emit(lifecycleEvent({
                  eventId: `load-${emitted}`,
                  ts: now.toISOString(),
                  level: "info",
                  role: "server",
                  pid: Deno.pid,
                  traceId: "sustained",
                  phase: "sustained-load",
                  detail: { n: emitted, pad: "x".repeat(120) },
                })));
              }
            });
            yield* realSleep(40);
          }
          // A foreign writer following the sink's protocol: one file per hour, whole lines only.
          const bucket = dayBucket(state.ms);
          const foreign = join(
            world.logDir,
            "cli",
            bucket,
            `events-${String(hour).padStart(2, "0")}-777777-001.jsonl`,
          );
          yield* Effect.promise(async () => {
            await Deno.mkdir(dirname(foreign), { recursive: true });
            await Deno.writeTextFile(foreign, `${eventLine()}\n${eventLine()}\nmalformed {\n`, {
              append: true,
            });
          });
          foreignGood += 2;
          foreignBad += 1;
          // Day 2's first foreign file is held "open" for two sweeps: unsafe backlog, then released.
          if (day === 2 && hour === HOURS[0]) held.push(foreign);
        }

        // End of the virtual day: let the sink flush, then ingest and sweep.
        state.ms = start.getTime() + day * DAY_MS + 23 * 60 * 60 * 1000;
        yield* realSleep(60);
        const todayBefore = (yield* Effect.promise(() => treeBytes(world.logDir, new Date(state.ms)))).today;
        const report = yield* control.ingestThenSweep();
        sweeps += 1;
        rejectedReported += report.linesRejected;
        const sweep = swept(report);
        if (day === 5) held.length = 0;
        blockedSeen += sweep.blocked.length;
        if (writing) gzipped += sweep.gzipped.length;
        // A directory one day old is far inside `keepDays`: only the cap can have deleted it.
        if (sweep.deleted.some((p) => p.includes(dayBucket(start.getTime() + (day - 1) * DAY_MS)))) {
          capPruned += 1;
        }
        const rows = yield* appRows();
        const { jsonl, derived, logger } = budgetsOf(report);
        console.log(
          `sweep ${sweeps} (day ${day}): input=${emitted} ingested=${rows.n} rejected=${rejectedReported} ` +
            `dropped=${logger.dropsRecorded} | jsonl=${jsonl.retainedBytes}B in ${jsonl.files} files ` +
            `(today ${jsonl.todayBytes}B, cap ${jsonl.capBytes}B, unsafe backlog ${sweep.blocked.length} files ` +
            `${sweep.blockedBytes}B) | duckdb=${derived.duckdbBytes}B wal=${derived.walBytes}B ` +
            `rows=${derived.platformEventRows} | gzipped=${sweep.gzipped.length} deleted=${sweep.deleted.length}`,
        );
        // Exactly once so far, and never more ingested than emitted minus dropped.
        assert.equal(Number(rows.n), Number(rows.d), "no accepted event is ingested twice");
        assert.ok(Number(rows.n) + logger.dropsRecorded <= emitted);
        // The cap holds wherever closed-file safety lets the plan act: what stays outside today, net
        // of the backlog the gate kept, fits in what today (at least as large as before the sweep)
        // left of the cap. Today's own growth during the sweep is the coordinator's own events.
        const nonToday = jsonl.retainedBytes - jsonl.todayBytes;
        assert.ok(
          nonToday - sweep.blockedBytes <= Math.max(0, capBytes - todayBefore - sweep.blockedBytes),
          `JSONL outside today (${nonToday}B, ${sweep.blockedBytes}B blocked) exceeds what the cap allows`,
        );
        assert.equal(derived.boundedByLogMaxTotalBytes, false);
      }

      const drops = budgetsOf(yield* control.ingestThenSweep()).logger.dropsRecorded;
      const rows = yield* appRows();
      const foreign = yield* sideQuery("SELECT count(*) AS n FROM platform_events WHERE name = 'fixture'");
      return { ingested: Number(rows.n), drops, foreignIngested: Number(foreign[0].n) };
    });

    const { ingested, drops, foreignIngested } = await Effect.runPromise(
      Effect.scoped(program).pipe(
        Effect.provide(Layer.provideMerge(eventSinkLayer("server"), config)),
        Effect.withClock(clock),
      ),
    );

    console.log(
      `sustained: input=${emitted} ingested=${ingested} dropped=${drops} ` +
        `foreign accepted=${foreignGood} rejected=${rejectedReported}/${foreignBad} sweeps=${sweeps}`,
    );
    assert.ok(drops > 0, "the run must overflow the logger's queue to say anything about drops");
    assert.ok(blockedSeen > 0, "the run must exercise the unsafe-to-delete backlog");
    assert.ok(capPruned > 0, "the run must exercise the size cap, not only age");
    assert.ok(gzipped > 0, "the run must exercise gzip");
    // Every emitted record was either flushed and ingested exactly once, or counted as dropped.
    assert.equal(ingested + drops, emitted);
    assert.equal(rejectedReported, foreignBad, "each malformed line quarantined exactly once");
    assert.equal(foreignIngested, foreignGood);
  });
});

/** The local `YYYY-MM-DD` bucket of an instant. */
function dayBucket(ms: number): string {
  const d = new Date(ms);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${
    String(d.getDate()).padStart(2, "0")
  }`;
}

/** JSONL bytes under `logDir`, and how many of them are in `now`'s date bucket. */
async function treeBytes(logDir: string, now: Date): Promise<{ total: number; today: number }> {
  const bucket = dayBucket(now.getTime());
  let total = 0;
  let today = 0;
  for (const role of ["server", "reader", "restore", "cli"]) {
    let days: Deno.DirEntry[] = [];
    try {
      days = await Array.fromAsync(Deno.readDir(join(logDir, role)));
    } catch {
      continue;
    }
    for (const day of days) {
      for await (const file of Deno.readDir(join(logDir, role, day.name))) {
        if (!/^(events-.*\.jsonl(\.gz)?|rejected\.jsonl)$/.test(file.name)) continue;
        const size = (await Deno.stat(join(logDir, role, day.name, file.name))).size;
        total += size;
        if (day.name >= bucket) today += size;
      }
    }
  }
  return { total, today };
}
