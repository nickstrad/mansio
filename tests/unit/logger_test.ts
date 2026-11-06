/**
 * Unit tests for the logging service (plan item 11).
 *
 * Every test runs against a fresh temp `LOG_DIR` and asserts on the bytes that actually landed
 * there, because the contract this item has to keep is a file contract: one JSON object per line, in
 * the dated/hour-bucketed file `core/logging/rotation.ts` names, with nothing lost that was not
 * counted and reported.
 *
 * Two mechanics are worth knowing before reading on:
 *
 * - **Waiting.** The batching fiber writes asynchronously, so assertions wait on the filesystem
 *   rather than on a fixed sleep. `waitFor` polls a predicate in *real* time (`setTimeout`, not
 *   `Effect.sleep`), which is what makes the same helper usable under `TestClock`, where virtual
 *   time only moves when a test moves it.
 * - **Virtual time.** Under `TestClock` the consumer's flush timer only fires when the test advances
 *   the clock, and `TestClock.adjust` replays every wake-up inside the window it advances. The
 *   date-boundary test is built around both facts; see the comment there.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import { Cause, Duration, Effect, Exit, Logger, LogLevel, TestClock, TestContext } from "effect";
import { type EventRecord, handlerEvent, type Level, lifecycleEvent, type Role } from "../../core/events.ts";
import { ConfigTest } from "../../platform/config.ts";
import { EventSink, type LoggingConfig } from "../../platform/services.ts";
import {
  eventSink,
  eventSinkLayer,
  type EventSinkRecordService,
  loggingLayer,
  recordFromLogEntry,
} from "../../platform/logging/logger.ts";

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

async function withTempDir(run: (dir: string) => Promise<void>): Promise<void> {
  const dir = await Deno.makeTempDir({ prefix: "mansio-logger-" });
  try {
    await run(dir);
  } finally {
    await Deno.remove(dir, { recursive: true });
  }
}

/** Every `*.jsonl` under `<dir>/<role>/`, sorted by path (so dated dirs and seqs read in order). */
async function logFiles(dir: string, role: Role = "server"): Promise<string[]> {
  const found: string[] = [];
  const walk = async (path: string): Promise<void> => {
    for await (const entry of Deno.readDir(path)) {
      const full = join(path, entry.name);
      if (entry.isDirectory) await walk(full);
      else if (entry.name.endsWith(".jsonl")) found.push(full);
    }
  };
  try {
    await walk(join(dir, role));
  } catch (cause) {
    // No tree at all (nothing logged yet) or an unwritable root (the degradation test points
    // `LOG_DIR` at a regular file) both mean "no files", not a broken test.
    const missing = cause instanceof Deno.errors.NotFound ||
      cause instanceof Deno.errors.NotADirectory;
    if (!missing) throw cause;
  }
  return found.sort();
}

/** Raw lines across every log file. Asserts the file-level invariant: no blank or partial lines. */
async function readLines(dir: string, role: Role = "server"): Promise<string[]> {
  const lines: string[] = [];
  for (const file of await logFiles(dir, role)) {
    const text = await Deno.readTextFile(file);
    if (text === "") continue;
    assert.ok(text.endsWith("\n"), `${file} does not end with a newline`);
    for (const line of text.slice(0, -1).split("\n")) {
      assert.notEqual(line, "", `${file} contains a blank line`);
      lines.push(line);
    }
  }
  return lines;
}

/** Every record on disk. Parsing here is the "one JSON object per line" assertion. */
async function readRecords(
  dir: string,
  role: Role = "server",
): Promise<Record<string, unknown>[]> {
  return (await readLines(dir, role)).map((line, index) => {
    const parsed = JSON.parse(line) as unknown;
    assert.equal(typeof parsed, "object", `line ${index} is not an object`);
    assert.notEqual(parsed, null, `line ${index} is null`);
    return parsed as Record<string, unknown>;
  });
}

const realSleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/**
 * Polls `predicate` in real time until it holds, up to three seconds. Returns whether it held;
 * callers assert on that rather than throwing here, so a failure reads as the assertion that failed.
 */
function waitFor(predicate: () => Promise<boolean>, attempts = 300): Effect.Effect<boolean> {
  return Effect.gen(function* () {
    for (let attempt = 0; attempt < attempts; attempt++) {
      if (yield* Effect.promise(predicate)) return true;
      yield* Effect.promise(() => realSleep(10));
    }
    return yield* Effect.promise(predicate);
  });
}

/** Runs `program` against a live `EventSink` over a temp `LOG_DIR`, then closes its scope. */
function runWithSink<A>(
  dir: string,
  logging: Partial<LoggingConfig>,
  program: Effect.Effect<A, never, EventSink>,
  role: Role = "server",
): Promise<A> {
  return Effect.runPromise(
    program.pipe(
      Effect.provide(eventSinkLayer(role)),
      Effect.provide(ConfigTest({ logging: { dir, ...logging } })),
    ),
  );
}

let nextId = 0;

function event(level: Level = "info", detail: Record<string, unknown> = {}): EventRecord {
  nextId += 1;
  return lifecycleEvent({
    eventId: `e-${nextId}`,
    ts: "2026-10-10T14:00:00.000Z",
    level,
    role: "server",
    pid: 4242,
    traceId: `t-${nextId}`,
    phase: "unit-test",
    detail: { seq: nextId, ...detail },
  });
}

const emitAll = (records: readonly EventRecord[]) =>
  Effect.gen(function* () {
    const sink = yield* eventSink;
    for (const record of records) yield* sink.emit(record);
  });

/** Emits every record and reports whether each `emit` succeeded — `emit` may never fail. */
const emitAllChecked = (records: readonly EventRecord[]) =>
  Effect.gen(function* () {
    const sink: EventSinkRecordService = yield* eventSink;
    let failures = 0;
    for (const record of records) {
      const exit = yield* Effect.exit(sink.emit(record));
      if (!Exit.isSuccess(exit)) failures += 1;
    }
    return failures;
  });

function dateBucketOf(ms: number): string {
  const date = new Date(ms);
  const pad = (n: number) => n.toString().padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

// ---------------------------------------------------------------------------
// Record layout on disk
// ---------------------------------------------------------------------------

test("records land as one JSON object per line, in emission order", async () => {
  await withTempDir(async (dir) => {
    const records = [event(), event(), event("error")];
    await runWithSink(dir, { eventFlushMs: 10 }, emitAll(records));

    const files = await logFiles(dir);
    assert.equal(files.length, 1);
    assert.match(files[0], new RegExp(`/events-\\d{2}-${Deno.pid}-001\\.jsonl$`));
    assert.equal(files[0].startsWith(join(dir, "server", dateBucketOf(Date.now()))), true);

    const parsed = await readRecords(dir);
    assert.equal(parsed.length, 3);
    assert.deepEqual(parsed.map((r) => r["eventId"]), records.map((r) => r.eventId));
    assert.deepEqual(parsed.map((r) => r["kind"]), ["lifecycle", "lifecycle", "lifecycle"]);
    assert.deepEqual(parsed.map((r) => r["level"]), ["info", "info", "error"]);
    assert.equal(parsed[0]["role"], "server");
    assert.equal(parsed[0]["phase"], "unit-test");
  });
});

test("the scope finalizer flushes buffered records before the temp dir is removed", async () => {
  await withTempDir(async (dir) => {
    // A flush period longer than the test can ever take: nothing reaches disk via the timer, so
    // every line below was written by the drain finalizer — and written before the handle it uses
    // was closed, which is the finalizer ordering this asserts.
    const records = [event(), event(), event(), event(), event()];
    await runWithSink(dir, { eventFlushMs: 600_000 }, emitAll(records));

    const parsed = await readRecords(dir);
    assert.equal(parsed.length, 5);
    assert.deepEqual(parsed.map((r) => r["eventId"]), records.map((r) => r.eventId));
  });
});

test("records below the configured level are not written; the rest are", async () => {
  await withTempDir(async (dir) => {
    const records = [event("debug"), event("info"), event("warn"), event("error")];
    await runWithSink(dir, { level: "warn", eventFlushMs: 10 }, emitAll(records));

    const parsed = await readRecords(dir);
    assert.deepEqual(parsed.map((r) => r["level"]), ["warn", "error"]);
  });
});

test("every level is written when the gate is open at debug", async () => {
  await withTempDir(async (dir) => {
    const records = [event("debug"), event("info"), event("warn"), event("error")];
    await runWithSink(dir, { level: "debug", eventFlushMs: 10 }, emitAll(records));

    const parsed = await readRecords(dir);
    assert.deepEqual(parsed.map((r) => r["level"]), ["debug", "info", "warn", "error"]);
  });
});

// ---------------------------------------------------------------------------
// Rotation
// ---------------------------------------------------------------------------

test("a date boundary driven by TestClock opens a new dated directory", async () => {
  await withTempDir(async (dir) => {
    // Virtual time is parked two seconds before a local midnight, and the crossing is two one-second
    // flushes. The jump to midnight happens *before* the sink layer is built, while no timer is
    // registered: `TestClock` replays every wake-up inside the window it advances, so jumping a day
    // against a one-second timer would cost 86 400 of them.
    const flushMs = 1_000;
    const midnightMs = new Date(2026, 9, 11, 0, 0, 0).getTime();
    const flush = TestClock.adjust(Duration.millis(flushMs));
    // Real time for the batching fiber to get back to its (virtual) sleep, so the `adjust` that
    // follows is the wake-up that flushes rather than a jump past an unregistered timer.
    const settle = Effect.promise(() => realSleep(50));

    const body = Effect.gen(function* () {
      const sink = yield* eventSink;
      yield* settle;

      yield* sink.emit(event());
      yield* settle;
      yield* flush;
      const firstDay = dateBucketOf(yield* TestClock.currentTimeMillis);
      const firstOpened = yield* waitFor(async () => (await logFiles(dir)).length >= 1);

      yield* sink.emit(event());
      yield* settle;
      yield* flush;
      const secondDay = dateBucketOf(yield* TestClock.currentTimeMillis);
      const secondOpened = yield* waitFor(async () => (await logFiles(dir)).length >= 2);
      return { firstDay, secondDay, firstOpened, secondOpened };
    });

    const program = Effect.gen(function* () {
      yield* TestClock.setTime(midnightMs - 2 * flushMs);
      return yield* body.pipe(
        Effect.provide(eventSinkLayer("server")),
        Effect.provide(ConfigTest({ logging: { dir, eventFlushMs: flushMs } })),
      );
    });

    const result = await Effect.runPromise(
      program.pipe(Effect.provide(TestContext.TestContext)),
    );

    assert.notEqual(result.firstDay, result.secondDay);
    assert.ok(result.firstOpened, `no log directory for ${result.firstDay}`);
    assert.ok(result.secondOpened, `no log directory for ${result.secondDay}`);

    const files = await logFiles(dir);
    assert.equal(files.length, 2, `expected one file per day, got ${files.join(", ")}`);
    assert.ok(files[0].includes(result.firstDay));
    assert.ok(files[1].includes(result.secondDay));
    assert.equal((await readRecords(dir)).length, 2);
  });
});

test("shutdown after a timer flush drains into the open file, then closes it", async () => {
  await withTempDir(async (dir) => {
    // One timer flush opens the file; the second record is emitted with virtual time frozen, so only
    // the drain finalizer can write it. Drain-before-close appends it to the same `-001` file; the
    // reverse order (close, then drain) would reopen onto a second `-002` file and leak its handle.
    const flushMs = 1_000;
    const settle = Effect.promise(() => realSleep(50));
    const first = event();
    const second = event();

    const body = Effect.gen(function* () {
      const sink = yield* eventSink;
      yield* settle;
      yield* sink.emit(first);
      yield* settle;
      yield* TestClock.adjust(Duration.millis(flushMs));
      const flushed = yield* waitFor(async () => (await readLines(dir)).length === 1);
      yield* sink.emit(second);
      return flushed;
    });

    const flushed = await Effect.runPromise(
      body.pipe(
        Effect.provide(eventSinkLayer("server")),
        Effect.provide(ConfigTest({ logging: { dir, eventFlushMs: flushMs } })),
        Effect.provide(TestContext.TestContext),
      ),
    );

    assert.ok(flushed, "the timer flush never reached disk");
    const files = await logFiles(dir);
    assert.equal(files.length, 1, `expected one data file, got ${files.join(", ")}`);
    assert.match(files[0], /-001\.jsonl$/);
    const parsed = await readRecords(dir);
    assert.deepEqual(parsed.map((r) => r["eventId"]), [first.eventId, second.eventId]);
  });
});

test("exceeding maxFileBytes rolls onto -002 within the same hour", async () => {
  await withTempDir(async (dir) => {
    const first = [event(), event(), event()];
    const second = [event(), event(), event()];
    const program = Effect.gen(function* () {
      const sink = yield* eventSink;
      for (const record of first) yield* sink.emit(record);
      const wrote = yield* waitFor(async () => (await readLines(dir)).length >= first.length);
      for (const record of second) yield* sink.emit(record);
      const rolled = yield* waitFor(async () =>
        (await logFiles(dir)).some((file) => file.endsWith("-002.jsonl"))
      );
      return { wrote, rolled };
    });

    // Small enough that the first batch alone passes it, so the next append must roll.
    const result = await runWithSink(dir, { maxFileBytes: 128, eventFlushMs: 10 }, program);
    assert.ok(result.wrote, "the first batch never reached disk");
    assert.ok(result.rolled, "maxFileBytes was exceeded but no -002 file was opened");

    const files = await logFiles(dir);
    assert.ok(files.some((file) => file.endsWith("-001.jsonl")));
    assert.ok(files.some((file) => file.endsWith("-002.jsonl")));
    const parsed = await readRecords(dir);
    assert.equal(parsed.length, 6);
    assert.deepEqual(
      parsed.map((r) => r["eventId"]),
      [...first, ...second].map((r) => r.eventId),
    );
  });
});

test("server and reader write separate trees", async () => {
  await withTempDir(async (dir) => {
    await runWithSink(dir, { eventFlushMs: 10 }, emitAll([event()]), "server");
    await runWithSink(dir, { eventFlushMs: 10 }, emitAll([event(), event()]), "reader");

    assert.equal((await logFiles(dir, "server")).length, 1);
    assert.equal((await logFiles(dir, "reader")).length, 1);
    assert.equal((await readRecords(dir, "server")).length, 1);
    assert.equal((await readRecords(dir, "reader")).length, 2);
  });
});

// ---------------------------------------------------------------------------
// Backpressure
// ---------------------------------------------------------------------------

test("overflowing the queue drops records visibly: emit never fails and the drops are reported", async () => {
  await withTempDir(async (dir) => {
    const EMITTED = 400;
    const CAPACITY = 8;
    const records = Array.from({ length: EMITTED }, () => event());

    // A flush period the test never reaches, so the consumer cannot drain the queue while the
    // producer floods it: the overflow is deterministic.
    const failures = await runWithSink(
      dir,
      { eventQueueCapacity: CAPACITY, eventFlushMs: 600_000 },
      emitAllChecked(records),
    );
    assert.equal(failures, 0, "emit must never fail, even when every offer is rejected");

    const parsed = await readRecords(dir);
    const queueEvents = parsed.filter((r) => r["kind"] === "queue");
    const dataLines = parsed.filter((r) => r["kind"] !== "queue");
    const dropped = queueEvents.reduce((sum, r) => sum + Number(r["dropped"]), 0);

    assert.ok(queueEvents.length > 0, "a dropped record must be reported as a queue event");
    assert.ok(dropped > 0, "the queue event must carry a non-zero drop count");
    assert.equal(
      dataLines.length + dropped,
      EMITTED,
      `${dataLines.length} lines + ${dropped} dropped != ${EMITTED} emitted`,
    );
    for (const report of queueEvents) {
      assert.equal(report["queue"], "events");
      assert.equal(report["event"], "dropped");
      assert.equal(report["cap"], CAPACITY);
      assert.equal(report["level"], "warn");
      assert.ok(Number(report["depth"]) <= CAPACITY);
    }
  });
});

test("drop reports are written even when the level gate would suppress a warning", async () => {
  await withTempDir(async (dir) => {
    const records = Array.from({ length: 100 }, () => event("error"));
    const failures = await runWithSink(
      dir,
      { level: "error", eventQueueCapacity: 4, eventFlushMs: 600_000 },
      emitAllChecked(records),
    );
    assert.equal(failures, 0);

    const parsed = await readRecords(dir);
    const queueEvents = parsed.filter((r) => r["kind"] === "queue");
    assert.ok(queueEvents.length > 0, "loss must stay visible regardless of LOG_LEVEL");
    const dropped = queueEvents.reduce((sum, r) => sum + Number(r["dropped"]), 0);
    assert.equal(parsed.length - queueEvents.length + dropped, 100);
  });
});

test("an unwritable LOG_DIR degrades instead of failing emit or shutdown", async () => {
  await withTempDir(async (dir) => {
    // A regular file where the log root should be: every mkdir under it fails. The one stderr line
    // this prints is the documented degradation path (platform/logging/logger.ts: noteFailure).
    const blocked = join(dir, "blocked");
    await Deno.writeTextFile(blocked, "not a directory\n");

    const failures = await runWithSink(
      blocked,
      { eventFlushMs: 10 },
      emitAllChecked([event(), event(), event()]),
    );
    assert.equal(failures, 0, "a broken sink must not surface in emit's error channel");
    assert.equal((await logFiles(blocked)).length, 0);
  });
});

// ---------------------------------------------------------------------------
// The Effect Logger path
// ---------------------------------------------------------------------------

function runWithLogger<A>(
  dir: string,
  logging: Partial<LoggingConfig>,
  program: Effect.Effect<A, never, EventSink>,
): Promise<A> {
  return Effect.runPromise(
    program.pipe(
      Effect.provide(loggingLayer("server")),
      Effect.provide(ConfigTest({ logging: { dir, ...logging } })),
    ),
  );
}

test("Effect.log* flows into the same JSONL records, with annotations and log spans", async () => {
  await withTempDir(async (dir) => {
    await runWithLogger(
      dir,
      { eventFlushMs: 10 },
      Effect.gen(function* () {
        yield* Effect.logInfo("hello logger").pipe(
          Effect.annotateLogs({ traceId: "t-log", appId: "tic-tac-toe", attempt: 2 }),
          Effect.withLogSpan("boot"),
        );
        yield* Effect.logDebug("below the info gate");
        yield* Effect.logError("boom");
      }),
    );

    const parsed = await readRecords(dir);
    assert.equal(parsed.length, 2, `expected the debug line to be gated, got ${parsed.length}`);

    const [info, error] = parsed;
    assert.equal(info["kind"], "lifecycle");
    assert.equal(info["level"], "info");
    assert.equal(info["traceId"], "t-log");
    assert.equal(info["appId"], "tic-tac-toe");
    assert.equal(info["phase"], "boot");
    const detail = info["detail"] as Record<string, unknown>;
    assert.equal(detail["message"], "hello logger");
    assert.deepEqual(detail["spans"], ["boot"]);
    assert.deepEqual(detail["annotations"], { attempt: 2 });
    assert.match(String(detail["fiber"]), /^#\d+$/);

    assert.equal(error["level"], "error");
    assert.equal(error["phase"], "log");
    assert.equal((error["detail"] as Record<string, unknown>)["message"], "boom");
  });
});

test("the configured level gates Effect.log* too", async () => {
  await withTempDir(async (dir) => {
    await runWithLogger(
      dir,
      { level: "warn", eventFlushMs: 10 },
      Effect.gen(function* () {
        yield* Effect.logInfo("suppressed");
        yield* Effect.logWarning("kept");
      }),
    );

    const parsed = await readRecords(dir);
    assert.equal(parsed.length, 1);
    assert.equal(parsed[0]["level"], "warn");
    assert.equal((parsed[0]["detail"] as Record<string, unknown>)["message"], "kept");
  });
});

test("recordFromLogEntry maps every Effect log level onto an event level, and keeps the cause", async () => {
  const seen: Record<string, unknown>[] = [];
  const capture = Logger.make<unknown, void>((entry) => {
    seen.push(recordFromLogEntry(entry, "cli", 7) as unknown as Record<string, unknown>);
  });
  await Effect.runPromise(
    Effect.gen(function* () {
      yield* Effect.logFatal("fatal");
      yield* Effect.logError("error");
      yield* Effect.logWarning("warning");
      yield* Effect.logInfo("info");
      yield* Effect.logDebug("debug");
      yield* Effect.logTrace("trace");
      yield* Effect.logError(Cause.fail("nope"), "while playing");
    }).pipe(
      Logger.withMinimumLogLevel(LogLevel.All),
      Effect.provide(Logger.replace(Logger.defaultLogger, capture)),
    ),
  );

  assert.deepEqual(seen.slice(0, 6).map((r) => r["level"]), [
    "error",
    "error",
    "warn",
    "info",
    "debug",
    "debug",
  ]);
  assert.equal(seen[0]["role"], "cli");
  assert.equal(seen[0]["pid"], 7);
  assert.equal((seen[0]["detail"] as Record<string, unknown>)["cause"], undefined);

  const withCause = (seen[6]["detail"] as Record<string, unknown>)["cause"];
  assert.equal(typeof withCause, "string");
  assert.match(String(withCause), /nope/);
});

// ---------------------------------------------------------------------------
// Throughput sanity
// ---------------------------------------------------------------------------

test("appending 10 000 events completes in under 2 s", async () => {
  await withTempDir(async (dir) => {
    const records = Array.from({ length: 10_000 }, () => event());
    const started = performance.now();
    await runWithSink(
      dir,
      { eventQueueCapacity: 20_000, eventFlushMs: 10 },
      emitAll(records),
    );
    const elapsedMs = performance.now() - started;

    const lines = await readLines(dir);
    assert.equal(lines.length, 10_000, "every event must reach disk when the queue is deep enough");
    assert.ok(elapsedMs < 2_000, `10 000 events took ${elapsedMs.toFixed(0)}ms`);
  });
});

// ---------------------------------------------------------------------------
// Emit accepts the kinds other items build
// ---------------------------------------------------------------------------

test("every event kind an item emits serializes to a line", async () => {
  await withTempDir(async (dir) => {
    const handler = handlerEvent({
      eventId: "h-1",
      ts: "2026-10-10T14:00:00.000Z",
      level: "info",
      role: "server",
      pid: 4242,
      traceId: "t-h",
      handlerKind: "mutation",
      name: "play",
      outcome: "ok",
      durationMs: 7,
    });
    await runWithSink(dir, { eventFlushMs: 10 }, emitAll([handler, event()]));

    const parsed = await readRecords(dir);
    assert.deepEqual(parsed.map((r) => r["kind"]), ["handler", "lifecycle"]);
    assert.equal(parsed[0]["name"], "play");
    assert.equal(parsed[0]["durationMs"], 7);
  });
});
