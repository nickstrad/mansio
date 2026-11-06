/**
 * Unit tests for the DuckDB event log (plan items 21 and 35): `core/eventlog/*`, `EventLogIngestLive`
 * and the v1 → v2 schema upgrade, against a real, persistent DuckDB file and a real `LOG_DIR` tree in a
 * temp directory. Every item-35 assertion about the tables goes through `makeEventLogIngest`, the
 * entry the reader uses, so a DDL or mapping change that ingest cannot run fails here.
 *
 * DuckDB runs in this (shared) test process. That is safe here, unlike `reader_test.ts`'s
 * in-process half: the hazard ADR 0001 describes is DuckDB's `sqlite` extension sharing an address
 * space with Node's SQLite, and event-log ingest never loads that extension.
 *
 * Every ingest runs in its own scope, so each run opens and closes the event-log file — which also
 * proves the tables and offsets persist across process-like restarts. Assertions read the file with
 * a separate DuckDB instance after the run's scope has closed.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { join } from "node:path";
import { DuckDBInstance } from "@duckdb/node-api";
import { Effect, Either, Layer, Ref, TestClock, TestContext } from "effect";
import type { IngestError } from "../../core/errors.ts";
import {
  backupEvent,
  type BackupEventRecord,
  type EventEnvelope,
  type EventRecord,
  handlerEvent,
  httpEvent,
  ingestEvent,
  type IngestEventRecord,
  lifecycleEvent,
  queueEvent,
  sqlEvent,
  toJsonLine,
  txEvent,
} from "../../core/events.ts";
import { EVENT_TABLES, EVENTLOG_SCHEMA_VERSION, TYPED_EVENT_TABLES } from "../../core/eventlog/ddl.ts";
import { completeLines, decide, type IngestStateRow, parseSourceFile } from "../../core/eventlog/offsets.ts";
import { ConfigTest } from "../../eventlog/config.ts";
import { EventSink, type IngestReport } from "../../eventlog/services.ts";
import {
  type EventLogConnection,
  type EventLogDriver,
  eventLogDriver,
  type EventLogIngestOptions,
  makeEventLogIngest,
} from "../../eventlog/ingest.ts";
import {
  type EventLogSchemaStatus,
  type EventLogUpgradeReport,
  inspectEventLogSchema,
  repairLoneSurrogates,
  upgradeEventLog,
} from "../../eventlog/tables.ts";

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

const SERVER_FILE = "server/2026-10-10/events-12-4242-001.jsonl";
const READER_FILE = "reader/2026-10-10/events-12-5151-001.jsonl";

interface World {
  readonly root: string;
  readonly logDir: string;
  readonly dbPath: string;
  readonly events: EventRecord[];
}

async function withWorld(body: (world: World) => Promise<void>): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "eventlog-test-" });
  try {
    await body({
      root,
      logDir: join(root, "logs"),
      dbPath: join(root, "logs", "duckdb", "events.duckdb"),
      events: [],
    });
  } finally {
    await Deno.remove(root, { recursive: true });
  }
}

/** One ingest run at a fixed `TestClock` instant, in its own scope. Emitted events land in `world.events`. */
async function ingestEither(
  world: World,
  atMillis: number,
  options: EventLogIngestOptions = {},
): Promise<Either.Either<IngestReport, IngestError>> {
  const program = Effect.gen(function* () {
    const sunk = yield* Ref.make<EventRecord[]>([]);
    const sink = Layer.succeed(EventSink, {
      emit: (event: EventRecord) => Ref.update(sunk, (all) => [...all, event]),
    });
    yield* TestClock.setTime(atMillis);
    const result = yield* Effect.either(
      Effect.scoped(Effect.flatMap(makeEventLogIngest(options), (service) => service.ingest)).pipe(
        Effect.provide(Layer.merge(
          ConfigTest({ logging: { dir: world.logDir }, eventLog: { duckdbPath: world.dbPath } }),
          sink,
        )),
      ),
    );
    world.events.push(...(yield* Ref.get(sunk)));
    return result;
  });
  return await Effect.runPromise(program.pipe(Effect.provide(TestContext.TestContext)));
}

async function ingest(
  world: World,
  atMillis: number,
  options: EventLogIngestOptions = {},
): Promise<IngestReport> {
  const result = await ingestEither(world, atMillis, options);
  if (Either.isLeft(result)) throw new Error(`ingest failed: ${JSON.stringify(result.left)}`);
  return result.right;
}

type Row = Record<string, unknown>;

/** Runs `sql` against the event-log file with a fresh instance; bigints stay bigints. */
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

async function counts(world: World): Promise<Record<string, number>> {
  const out: Record<string, number> = {};
  for (const table of EVENT_TABLES) {
    const [row] = await query(world, `SELECT count(*) AS n FROM ${table}`);
    out[table] = Number(row.n);
  }
  return out;
}

async function stateRows(world: World): Promise<Row[]> {
  return await query(
    world,
    `SELECT source_file, bytes_ingested, prefix_sha256, CAST(last_ingested_at AS VARCHAR) AS at
     FROM _ingest_state ORDER BY source_file`,
  );
}

async function eventIds(world: World): Promise<string[]> {
  const rows = await query(world, "SELECT event_id FROM platform_events ORDER BY source_file, source_offset");
  return rows.map((row) => String(row.event_id));
}

async function writeSource(world: World, relative: string, content: string | Uint8Array): Promise<void> {
  const path = join(world.logDir, relative);
  await Deno.mkdir(join(path, ".."), { recursive: true });
  if (typeof content === "string") await Deno.writeTextFile(path, content);
  else await Deno.writeFile(path, content);
}

async function appendSource(world: World, relative: string, content: string): Promise<void> {
  await Deno.writeTextFile(join(world.logDir, relative), content, { append: true });
}

function envelope(id: string, role: EventEnvelope["role"], second = 0): EventEnvelope {
  return {
    eventId: id,
    ts: `2026-10-10T12:00:${String(second).padStart(2, "0")}.000Z`,
    level: "info",
    role,
    pid: role === "server" ? 4242 : 5151,
    traceId: "trace-1",
    appId: "column",
  };
}

const lines = (records: readonly EventRecord[]): string => records.map((r) => toJsonLine(r) + "\n").join("");

/** One record of every kind with a typed table, plus a handler record (generic table only). */
function serverFixture(): EventRecord[] {
  return [
    httpEvent({
      ...envelope("s-http", "server", 1),
      method: "POST",
      path: "/apps/column/mutation/makeMove",
      handlerKind: "mutation",
      name: "makeMove",
      status: 200,
      durationMs: 4.5,
      bytesIn: 120,
      bytesOut: 340,
    }),
    handlerEvent({
      ...envelope("s-handler", "server", 2),
      handlerKind: "query",
      name: "getGame",
      outcome: "error",
      durationMs: 1.25,
      errorTag: "NotFoundError",
      errorDetail: { table: "games", id: "g-1" },
    }),
    sqlEvent({
      ...envelope("s-sql", "server", 3),
      engine: "sqlite",
      dialect: "sqlite",
      statementKind: "insert",
      tableName: "games",
      sqlText: "INSERT INTO games VALUES (?)",
      bindCount: 1,
      durationMs: 0.5,
      rowCount: 1,
      outcome: "ok",
    }),
    txEvent({
      ...envelope("s-tx", "server", 4),
      engine: "sqlite",
      outcome: "commit",
      operations: 3,
      durationMs: 2,
      queueWaitMs: 0.25,
    }),
    queueEvent({
      ...envelope("s-queue", "server", 5),
      queue: "writer",
      depth: 7,
      cap: 1000,
      event: "high-water",
      dropped: 0,
    }),
    backupEvent({
      ...envelope("s-backup", "server", 6),
      phase: "put-object",
      emitter: "object-store",
      runId: "run-1",
      key: "snapshots/column/1.sqlite",
      bytes: 4096,
      attempts: 1,
      durationMs: 12,
      outcome: "ok",
    }),
    lifecycleEvent({ ...envelope("s-life", "server", 7), phase: "ready", detail: { port: 8000 } }),
  ];
}

function readerFixture(): EventRecord[] {
  return [
    sqlEvent({
      ...envelope("r-sql", "reader", 8),
      engine: "duckdb",
      dialect: "duckdb",
      statementKind: "select",
      tableName: "games",
      sqlText: "SELECT * FROM games",
      bindCount: 0,
      durationMs: 1.5,
      rowCount: 2,
      outcome: "ok",
    }),
    txEvent({
      ...envelope("r-tx", "reader", 9),
      engine: "duckdb",
      outcome: "rollback",
      operations: 1,
      durationMs: 3,
      queueWaitMs: 0,
      errorTag: "ReaderError",
    }),
    ingestEvent({
      ...envelope("r-ingest", "reader", 10),
      sourceFile: SERVER_FILE,
      bytesIngested: 10,
      status: "ok",
    }),
  ];
}

async function writeFixture(world: World): Promise<void> {
  await writeSource(world, SERVER_FILE, lines(serverFixture()));
  await writeSource(world, READER_FILE, lines(readerFixture()));
  // Not sources: ignored by the inventory.
  await writeSource(world, "server/2026-10-10/notes.txt", "not a log\n");
  await writeSource(world, "server/2026-10-08/events-09-4242-001.jsonl.gz", "gzipped history");
  await writeSource(world, "k6/run-1/raw.json", '{"type":"Point"}\n');
}

const T0 = Date.UTC(2026, 9, 10, 13, 0, 0);

// ---------------------------------------------------------------------------
// Acceptance: fixture ingest, then idempotence
// ---------------------------------------------------------------------------

test("ingest of a fixture directory fills each table; a second run inserts nothing and leaves _ingest_state unchanged", async () => {
  await withWorld(async (world) => {
    await writeFixture(world);

    const first = await ingest(world, T0);
    assert.deepEqual(await counts(world), {
      platform_events: 10,
      sql_events: 2,
      tx_events: 2,
      http_events: 1,
      queue_events: 1,
      backup_events: 1,
      lifecycle_events: 1,
      k6_points: 0,
    });
    assert.equal(first.filesScanned, 2);
    assert.equal(first.filesIngested, 2);
    assert.equal(first.linesIngested, 10);
    assert.equal(first.linesRejected, 0);
    const serverBytes = (await Deno.stat(join(world.logDir, SERVER_FILE))).size;
    const readerBytes = (await Deno.stat(join(world.logDir, READER_FILE))).size;
    assert.equal(first.bytesIngested, serverBytes + readerBytes);

    const stateBefore = await stateRows(world);
    assert.deepEqual(stateBefore.map((row) => [row.source_file, Number(row.bytes_ingested)]), [
      [READER_FILE, readerBytes],
      [SERVER_FILE, serverBytes],
    ]);
    assert.equal(stateBefore[0].at, "2026-10-10 13:00:00");

    // An hour later: if the second run touched a row, `last_ingested_at` would show it.
    const second = await ingest(world, T0 + 3_600_000);
    assert.deepEqual(
      {
        filesIngested: second.filesIngested,
        linesIngested: second.linesIngested,
        bytes: second.bytesIngested,
      },
      { filesIngested: 0, linesIngested: 0, bytes: 0 },
    );
    assert.equal((await counts(world)).platform_events, 10);
    assert.deepEqual(await stateRows(world), stateBefore);
    assert.deepEqual(world.events, [], "a clean run emits no ingest events");
  });
});

test("columns follow the documented mapping from event records", async () => {
  await withWorld(async (world) => {
    await writeFixture(world);
    await ingest(world, T0);

    const [http] = await query(
      world,
      "SELECT event_id, CAST(ts AS VARCHAR) AS ts, method, handler_kind, name, status, duration_ms, bytes_in, bytes_out FROM http_events",
    );
    assert.deepEqual(http, {
      event_id: "s-http",
      ts: "2026-10-10 12:00:01",
      method: "POST",
      handler_kind: "mutation",
      name: "makeMove",
      status: 200,
      duration_ms: 4.5,
      bytes_in: 120n,
      bytes_out: 340n,
    });

    const generic = await query(
      world,
      `SELECT event_id, event_kind, handler_kind, name, outcome, error_tag, CAST(attrs AS VARCHAR) AS attrs,
         source_offset, pid
       FROM platform_events WHERE source_file = '${SERVER_FILE}' ORDER BY source_offset`,
    );
    const firstLineBytes = new TextEncoder().encode(toJsonLine(serverFixture()[0]) + "\n").length;
    assert.equal(generic[0].source_offset, 0n);
    assert.equal(generic[1].source_offset, BigInt(firstLineBytes));
    // A handler record keeps `handler` as its event_kind; its query/mutation is handler_kind. attrs
    // carries errorDetail.
    assert.deepEqual(
      {
        eventKind: generic[1].event_kind,
        handlerKind: generic[1].handler_kind,
        name: generic[1].name,
        outcome: generic[1].outcome,
        tag: generic[1].error_tag,
      },
      { eventKind: "handler", handlerKind: "query", name: "getGame", outcome: "error", tag: "NotFoundError" },
    );
    assert.deepEqual(JSON.parse(String(generic[1].attrs)), { id: "g-1", table: "games" });
    assert.equal(generic[0].pid, 4242);
    assert.deepEqual(generic.map((row) => row.event_kind), [
      "http",
      "handler",
      "sql",
      "tx",
      "queue",
      "backup",
      "lifecycle",
    ]);
    assert.deepEqual(generic.map((row) => row.handler_kind), [
      "mutation",
      "query",
      null,
      null,
      null,
      null,
      null,
    ]);
    assert.deepEqual(generic.map((row) => row.name), [
      "makeMove",
      "getGame",
      "insert",
      null,
      "writer",
      "put-object",
      "ready",
    ]);

    const [life] = await query(
      world,
      "SELECT phase, CAST(detail AS VARCHAR) AS detail, pid FROM lifecycle_events",
    );
    assert.deepEqual({ ...life, detail: JSON.parse(String(life.detail)) }, {
      phase: "ready",
      detail: { port: 8000 },
      pid: 4242,
    });
    const [tx] = await query(
      world,
      "SELECT outcome, operations, error_tag FROM tx_events WHERE engine = 'duckdb'",
    );
    assert.deepEqual(tx, { outcome: "rollback", operations: 1, error_tag: "ReaderError" });
    const [ingested] = await query(
      world,
      "SELECT event_kind, handler_kind, name, outcome FROM platform_events WHERE event_id = 'r-ingest'",
    );
    assert.deepEqual(ingested, {
      event_kind: "ingest",
      handler_kind: null,
      name: SERVER_FILE,
      outcome: "ok",
    });
    const [backup] = await query(
      world,
      "SELECT event_id, phase, emitter, run_id, key, bytes, attempts, outcome FROM backup_events",
    );
    assert.deepEqual(backup, {
      event_id: "s-backup",
      phase: "put-object",
      emitter: "object-store",
      run_id: "run-1",
      key: "snapshots/column/1.sqlite",
      bytes: 4096n,
      attempts: 1,
      outcome: "ok",
    });
  });
});

// ---------------------------------------------------------------------------
// Acceptance: appends
// ---------------------------------------------------------------------------

test("appending to an ingested file ingests only the new complete lines; a partial line waits for its newline", async () => {
  await withWorld(async (world) => {
    await writeFixture(world);
    await ingest(world, T0);
    const before = await eventIds(world);

    const more = [
      lifecycleEvent({ ...envelope("s-more-1", "server", 20), phase: "stopping" }),
      lifecycleEvent({ ...envelope("s-more-2", "server", 21), phase: "stopped" }),
    ];
    const partial = toJsonLine(lifecycleEvent({ ...envelope("s-more-3", "server", 22), phase: "exit" }));
    await appendSource(world, SERVER_FILE, lines(more) + partial.slice(0, 25));

    const report = await ingest(world, T0 + 1000);
    assert.equal(report.linesIngested, 2);
    assert.equal(report.filesIngested, 1);
    const after = await eventIds(world);
    assert.deepEqual(after.filter((id) => !before.includes(id)), ["s-more-1", "s-more-2"]);
    assert.equal(after.length, before.length + 2);
    const [row] = await query(
      world,
      `SELECT bytes_ingested FROM _ingest_state WHERE source_file = '${SERVER_FILE}'`,
    );
    const size = (await Deno.stat(join(world.logDir, SERVER_FILE))).size;
    assert.equal(Number(row.bytes_ingested), size - 25, "the partial line is not consumed");

    await appendSource(world, SERVER_FILE, partial.slice(25) + "\n");
    const completed = await ingest(world, T0 + 2000);
    assert.equal(completed.linesIngested, 1);
    assert.deepEqual((await eventIds(world)).slice(-1), ["s-more-3"]);
    assert.equal((await counts(world)).lifecycle_events, 4);
  });
});

// ---------------------------------------------------------------------------
// Acceptance: quarantine
// ---------------------------------------------------------------------------

test("malformed lines are quarantined beside their source with an ingest event; the rest of the batch lands", async () => {
  await withWorld(async (world) => {
    const good = (id: string, second: number) =>
      toJsonLine(lifecycleEvent({ ...envelope(id, "server", second), phase: "tick" }));
    const encoder = new TextEncoder();
    const parts: Uint8Array[] = [
      encoder.encode(good("ok-1", 1) + "\n"),
      encoder.encode("this is not json\n"),
      encoder.encode("[1, 2, 3]\n"),
      encoder.encode('{"ts":"2026-10-10T12:00:00.000Z","kind":"lifecycle"}\n'),
      encoder.encode('{"eventId":"bad-ts","ts":"yesterday"}\n'),
      new Uint8Array([0x7b, 0xff, 0xfe, 0x7d, 0x0a]), // `{` + invalid UTF-8 + `}`
      encoder.encode(good("ok-2", 2) + "\n"),
      encoder.encode("\n\r\n"), // blank lines: consumed, neither rows nor rejects
      encoder.encode(good("ok-3", 3) + "\r\n"), // CRLF
    ];
    const content = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
    let at = 0;
    for (const part of parts) {
      content.set(part, at);
      at += part.length;
    }
    await writeSource(world, SERVER_FILE, content);

    const report = await ingest(world, T0);
    assert.equal(report.linesIngested, 3);
    assert.equal(report.linesRejected, 5);
    assert.deepEqual(await eventIds(world), ["ok-1", "ok-2", "ok-3"]);
    const [state] = await stateRows(world);
    assert.equal(Number(state.bytes_ingested), content.length, "rejected lines are consumed, not retried");

    const quarantine = join(world.logDir, "server/2026-10-10/rejected.jsonl");
    const rejected = (await Deno.readTextFile(quarantine)).trimEnd().split("\n").map((line) =>
      JSON.parse(line)
    );
    assert.deepEqual(rejected.map((r) => [r.reason, r.sourceFile]), [
      ["invalid-json", SERVER_FILE],
      ["not-an-object", SERVER_FILE],
      ["missing-event-id", SERVER_FILE],
      ["invalid-ts", SERVER_FILE],
      ["invalid-utf8", SERVER_FILE],
    ]);
    assert.equal(rejected[0].line, "this is not json");
    assert.equal(rejected[0].sourceOffset, parts[0].length);
    assert.equal(rejected[0].rejectedAt, new Date(T0).toISOString());

    assert.equal(world.events.length, 1);
    const event = world.events[0] as IngestEventRecord;
    assert.equal(event.kind, "ingest");
    assert.equal(event.role, "reader");
    assert.equal(event.sourceFile, SERVER_FILE);
    assert.equal(event.errorTag, "RejectedLines");
    assert.equal((event.errorDetail as { rejected: number }).rejected, 5);
    assert.equal(
      (event.errorDetail as { quarantine: string }).quarantine,
      "server/2026-10-10/rejected.jsonl",
    );

    // Quarantine is not a source, and a second run neither re-quarantines nor re-emits.
    const quarantineBefore = await Deno.readTextFile(quarantine);
    const again = await ingest(world, T0 + 1000);
    assert.equal(again.filesScanned, 1);
    assert.equal(again.linesRejected, 0);
    assert.equal(await Deno.readTextFile(quarantine), quarantineBefore);
    assert.equal(world.events.length, 1);
  });
});

// ---------------------------------------------------------------------------
// Rewrites and truncation
// ---------------------------------------------------------------------------

test("a rewritten file is re-read from byte 0, deduplicated on eventId, and reported", async () => {
  await withWorld(async (world) => {
    const life = (id: string, second: number) =>
      lifecycleEvent({ ...envelope(id, "server", second), phase: id });
    await writeSource(world, SERVER_FILE, lines([life("a", 1), life("b", 2), life("c", 3)]));
    await ingest(world, T0);

    // Same path, different history: one old event survives, two are new.
    await writeSource(world, SERVER_FILE, lines([life("b", 2), life("d", 4), life("e", 5)]));
    const report = await ingest(world, T0 + 1000);
    assert.equal(report.linesIngested, 2);
    assert.deepEqual((await eventIds(world)).sort(), ["a", "b", "c", "d", "e"]);
    const rewrite = world.events.at(-1) as IngestEventRecord;
    assert.equal(rewrite.errorTag, "SourceRewritten");
    assert.deepEqual(rewrite.errorDetail, {
      reason: "prefix-mismatch",
      previousBytesIngested:
        new TextEncoder().encode(lines([life("a", 1), life("b", 2), life("c", 3)])).length,
      size: (await Deno.stat(join(world.logDir, SERVER_FILE))).size,
      deduplicated: 1,
    });

    // Truncated back to its first line: nothing new, and the row now describes the shorter file.
    const firstLine = lines([life("b", 2)]);
    await writeSource(world, SERVER_FILE, firstLine);
    const truncated = await ingest(world, T0 + 2000);
    assert.equal(truncated.linesIngested, 0);
    assert.equal((world.events.at(-1) as IngestEventRecord).errorTag, "SourceRewritten");
    assert.equal(
      ((world.events.at(-1) as IngestEventRecord).errorDetail as { reason: string }).reason,
      "truncated",
    );
    const [state] = await stateRows(world);
    assert.equal(Number(state.bytes_ingested), new TextEncoder().encode(firstLine).length);

    // The new generation is an ordinary append from here on.
    const eventsBefore = world.events.length;
    await appendSource(world, SERVER_FILE, lines([life("f", 6)]));
    assert.equal((await ingest(world, T0 + 3000)).linesIngested, 1);
    assert.equal(world.events.length, eventsBefore, "an append to the new generation is not a rewrite");
    assert.deepEqual((await eventIds(world)).sort(), ["a", "b", "c", "d", "e", "f"]);
  });
});

test("a torn terminator restored later does not insert the old line twice (sticky dedupe)", async () => {
  await withWorld(async (world) => {
    const only = lines([lifecycleEvent({ ...envelope("only", "server", 1), phase: "ready" })]);
    await writeSource(world, SERVER_FILE, only);
    await ingest(world, T0);

    // The file loses its final `\n`: a rewrite with no complete line left to consume.
    await writeSource(world, SERVER_FILE, only.slice(0, -1));
    const torn = await ingest(world, T0 + 1000);
    assert.equal(torn.linesIngested, 0);
    const event = world.events.at(-1) as IngestEventRecord;
    assert.equal(event.errorTag, "SourceRewritten");
    assert.equal((event.errorDetail as { reason: string }).reason, "truncated");
    // The row now describes the new generation — nothing consumed — and is flagged for dedupe.
    const [state] = await query(world, "SELECT bytes_ingested, dedupe FROM _ingest_state");
    assert.deepEqual(state, { bytes_ingested: 0n, dedupe: true });

    // The terminator comes back: the line is complete again, and it is the old line.
    await appendSource(world, SERVER_FILE, "\n");
    const restored = await ingest(world, T0 + 2000);
    assert.equal(restored.linesIngested, 0);
    assert.deepEqual(await eventIds(world), ["only"]);
    // Fresh lines in the flagged file still land.
    await appendSource(
      world,
      SERVER_FILE,
      lines([lifecycleEvent({ ...envelope("next", "server", 2), phase: "x" })]),
    );
    assert.equal((await ingest(world, T0 + 3000)).linesIngested, 1);
    assert.deepEqual(await eventIds(world), ["only", "next"]);
  });
});

test("records the platform's serializer writes always parse: lone surrogates, non-finite numbers, bare ts", async () => {
  await withWorld(async (world) => {
    // `toJsonLine` writes a lone surrogate as an escape that DuckDB's json_valid would reject.
    const lone = toJsonLine(handlerEvent({
      ...envelope("lone", "server", 1),
      handlerKind: "query",
      name: "getGame",
      outcome: "error",
      durationMs: 1,
      errorTag: "SchemaError",
      errorDetail: { actual: "x\ud800y", pair: "\ud83d\ude00" },
    }));
    assert.ok(lone.includes("\\ud800"));
    const raw = (id: string, extra: string) =>
      `{"eventId":"${id}","ts":"2026-10-10T12:00:00","level":"info","role":"server","pid":1,"traceId":"t",` +
      `"kind":"tx","engine":"sqlite","outcome":"commit","operations":1,"queueWaitMs":0${extra}}\n`;
    await writeSource(
      world,
      SERVER_FILE,
      lone + "\n" + raw("inf", ',"durationMs":1e999') + raw("nan", ',"durationMs":NaN') +
        raw("ok", ',"durationMs":2'),
    );
    const report = await ingest(world, T0);
    assert.equal(report.linesRejected, 0);
    assert.deepEqual(await eventIds(world), ["lone", "inf", "nan", "ok"]);
    const [attrs] = await query(
      world,
      "SELECT CAST(attrs AS VARCHAR) AS a FROM platform_events WHERE event_id = 'lone'",
    );
    assert.deepEqual(JSON.parse(String(attrs.a)), { actual: "x\ufffdy", pair: "\ud83d\ude00" });
    const tx = await query(
      world,
      "SELECT duration_ms, CAST(ts AS VARCHAR) AS ts FROM tx_events ORDER BY ts, duration_ms NULLS FIRST",
    );
    assert.deepEqual(tx.map((row) => row.duration_ms), [null, null, 2]);
    // An offset-less timestamp is read as UTC, whatever the host's zone.
    assert.equal(tx[0].ts, "2026-10-10 12:00:00");
  });
});

test("repairLoneSurrogates fixes only unpaired surrogate escapes", () => {
  assert.equal(repairLoneSurrogates('{"a":"\\ud800"}'), '{"a":"\\ufffd"}');
  assert.equal(repairLoneSurrogates('{"a":"\\udc00x\\ud800"}'), '{"a":"\\ufffdx\\ufffd"}');
  assert.equal(repairLoneSurrogates('{"a":"\\ud83d\\ude00"}'), '{"a":"\\ud83d\\ude00"}');
  assert.equal(repairLoneSurrogates('{"a":"\\ud83d\\u0041"}'), '{"a":"\\ufffd\\u0041"}');
  // An escaped backslash followed by `ud800` is literal text, not an escape.
  assert.equal(repairLoneSurrogates('{"a":"\\\\ud800"}'), '{"a":"\\\\ud800"}');
  assert.equal(repairLoneSurrogates('{"a":"plain \\n"}'), '{"a":"plain \\n"}');
  // Every repaired line is still valid JSON with the same structure.
  assert.deepEqual(JSON.parse(repairLoneSurrogates(JSON.stringify({ k: "\ud800", n: 1 }))), {
    k: "\ufffd",
    n: 1,
  });
});

test("two copies of one event in a rewritten file land once", async () => {
  await withWorld(async (world) => {
    const life = (id: string) => lifecycleEvent({ ...envelope(id, "server", 1), phase: id });
    await writeSource(world, SERVER_FILE, lines([life("a")]));
    await ingest(world, T0);
    await writeSource(world, SERVER_FILE, lines([life("b"), life("c"), life("c"), life("a")]));
    const report = await ingest(world, T0 + 1000);
    assert.equal(report.linesIngested, 2);
    assert.deepEqual((await eventIds(world)).sort(), ["a", "b", "c"]);
  });
});

/** Wraps the real driver and records every statement that builds `ingest_rows`. */
function rowsRecordingDriver(seen: string[]): EventLogDriver {
  return {
    open: async (path) => {
      const instance = await eventLogDriver.open(path);
      return {
        closeSync: () => instance.closeSync(),
        connect: async () => {
          const connection = await instance.connect();
          return {
            run: (sql, values) => {
              if (sql.includes("TEMP TABLE ingest_rows")) seen.push(sql);
              return connection.run(sql, values);
            },
            runAndReadAll: (sql) => connection.runAndReadAll(sql),
            closeSync: () => connection.closeSync(),
          };
        },
      };
    },
  };
}

test("only a batch with a rewritten file anti-joins against platform_events", async () => {
  await withWorld(async (world) => {
    const life = (id: string) => lifecycleEvent({ ...envelope(id, "server", 1), phase: id });
    const seen: string[] = [];
    const driver = rowsRecordingDriver(seen);
    await writeSource(world, SERVER_FILE, lines([life("a")]));
    await ingest(world, T0, { driver });
    await appendSource(world, SERVER_FILE, lines([life("b")]));
    await ingest(world, T0 + 1000, { driver });
    assert.equal(seen.length, 2);
    // A new file and a plain append trust their lines: the batch never reads history.
    assert.ok(seen.every((sql) => !sql.includes("platform_events")), seen.join("\n---\n"));

    await writeSource(world, SERVER_FILE, lines([life("a"), life("c")]));
    assert.equal((await ingest(world, T0 + 2000, { driver })).linesIngested, 1);
    assert.equal(seen.length, 3);
    assert.ok(seen[2].includes("platform_events"));
    assert.deepEqual((await eventIds(world)).sort(), ["a", "b", "c"]);
  });
});

/** Wraps the real driver; the first `COMMIT` and the first `ROLLBACK` issued both fail. */
function failingRollbackDriver(): EventLogDriver {
  const failed = new Set<string>();
  return {
    open: async (path) => {
      const instance = await eventLogDriver.open(path);
      return {
        closeSync: () => instance.closeSync(),
        connect: async () => {
          const connection = await instance.connect();
          return {
            run: (sql, values) => {
              if ((sql === "COMMIT" || sql === "ROLLBACK") && !failed.has(sql)) {
                failed.add(sql);
                return Promise.reject(new Error(`injected ${sql} failure`));
              }
              return connection.run(sql, values);
            },
            runAndReadAll: (sql) => connection.runAndReadAll(sql),
            closeSync: () => connection.closeSync(),
          };
        },
      };
    },
  };
}

test("a failed ROLLBACK leaves the service usable: the next run reconnects and ingests exactly once", async () => {
  await withWorld(async (world) => {
    await writeFixture(world);
    const sink = Layer.succeed(EventSink, { emit: () => Effect.void });
    const [first, second] = await Effect.runPromise(
      Effect.scoped(Effect.gen(function* () {
        const service = yield* makeEventLogIngest({ driver: failingRollbackDriver() });
        // Both runs share one service, and so one connection until it is replaced.
        return [yield* Effect.either(service.ingest), yield* Effect.either(service.ingest)] as const;
      })).pipe(
        Effect.provide(Layer.merge(
          ConfigTest({ logging: { dir: world.logDir }, eventLog: { duckdbPath: world.dbPath } }),
          sink,
        )),
      ),
    );
    assert.ok(Either.isLeft(first) && first.left.phase === "commit");
    assert.ok(Either.isRight(second), JSON.stringify(second));
    assert.equal(second.right.linesIngested, 10);
    assert.equal((await counts(world)).platform_events, 10);
  });
});

// ---------------------------------------------------------------------------
// Transactions
// ---------------------------------------------------------------------------

/** Wraps the real driver; the first `COMMIT` issued fails. */
function failingCommitDriver(): EventLogDriver {
  let failed = false;
  return {
    open: async (path) => {
      const instance = await eventLogDriver.open(path);
      return {
        closeSync: () => instance.closeSync(),
        connect: async () => {
          const connection = await instance.connect();
          const wrapped: EventLogConnection = {
            run: (sql, values) => {
              if (sql === "COMMIT" && !failed) {
                failed = true;
                return Promise.reject(new Error("injected commit failure"));
              }
              return connection.run(sql, values);
            },
            runAndReadAll: (sql) => connection.runAndReadAll(sql),
            closeSync: () => connection.closeSync(),
          };
          return wrapped;
        },
      };
    },
  };
}

test("a failed commit rolls back rows and offsets together; the next run ingests everything exactly once", async () => {
  await withWorld(async (world) => {
    await writeFixture(world);

    const failed = await ingestEither(world, T0, { driver: failingCommitDriver() });
    assert.ok(Either.isLeft(failed));
    assert.equal(failed.left.phase, "commit");
    assert.deepEqual(Object.values(await counts(world)), EVENT_TABLES.map(() => 0));
    assert.deepEqual(await stateRows(world), []);
    assert.equal((world.events.at(-1) as IngestEventRecord).errorTag, "IngestError");

    await ingest(world, T0 + 1000);
    assert.equal((await counts(world)).platform_events, 10);
    assert.equal(new Set(await eventIds(world)).size, 10);
  });
});

test("small batches commit separately and produce the same tables as one batch", async () => {
  await withWorld(async (world) => {
    await writeFixture(world);
    await writeSource(
      world,
      "server/2026-10-10/events-13-4242-001.jsonl",
      lines([lifecycleEvent({ ...envelope("later", "server", 30), phase: "later" })]),
    );
    const report = await ingest(world, T0, { batchBytes: 1 });
    assert.equal(report.filesIngested, 3);
    assert.equal((await counts(world)).platform_events, 11);
    assert.equal((await ingest(world, T0 + 1000, { batchBytes: 1 })).linesIngested, 0);
  });
});

// ---------------------------------------------------------------------------
// Large integers and timestamps (the item-13 json_extract concern)
// ---------------------------------------------------------------------------

test("integers are exact or NULL, never silently rounded; timestamp offsets are honoured", async () => {
  await withWorld(async (world) => {
    // `durationMs` doubles as the row's position, since the typed tables carry no offset.
    const cases: [bytes: string, expected: bigint | null][] = [
      ["9007199254740993", 9007199254740993n], // > 2^53 as an integer literal: exact
      ["9223372036854775807", 9223372036854775807n], // int64 max
      ["9223372036854775808", null], // past int64: NULL, not wrapped
      ["9007199254740993.0", null], // already rounded to ...992 by the JSON parser: NULL
      ["1e+21", null], // JSON.stringify's form for >= 1e21: NULL
      ["12.5", null], // TRY_CAST would round to 13: NULL
      ["4096.0", 4096n], // an integral double below 2^53 is exact
      ['"123"', 123n], // how core/events.ts serialises a bigint
      ['"12x"', null],
    ];
    const backup = (i: number, bytes: string, ts = "2026-10-10T12:00:00.000Z") =>
      `{"eventId":"b-${i}","ts":"${ts}","level":"info","role":"server","pid":1,"traceId":"t","kind":"backup",` +
      `"phase":"hash","bytes":${bytes},"durationMs":${i},"outcome":"ok"}\n`;
    await writeSource(
      world,
      SERVER_FILE,
      cases.map(([bytes], i) => backup(i, bytes)).join("") +
        backup(cases.length, "1", "2026-10-10T12:00:00.000+02:00"),
    );
    await ingest(world, T0);
    const rows = await query(
      world,
      "SELECT bytes, CAST(ts AS VARCHAR) AS ts FROM backup_events ORDER BY duration_ms",
    );
    assert.deepEqual(rows.map((row) => row.bytes), [...cases.map(([, expected]) => expected), 1n]);
    assert.equal(rows[0].ts, "2026-10-10 12:00:00");
    assert.equal(rows[cases.length].ts, "2026-10-10 10:00:00");
  });
});

// ---------------------------------------------------------------------------
// Item 35: event identity — exact typed-to-canonical joins
// ---------------------------------------------------------------------------

/**
 * Per typed table: its rows; those whose `event_id` matches exactly one `platform_events` row, of the
 * table's kind; the canonical rows of that kind; and those with exactly one typed row.
 */
async function joinReport(world: World): Promise<Record<string, number[]>> {
  const out: Record<string, number[]> = {};
  for (const [table, kind] of Object.entries(TYPED_EVENT_TABLES)) {
    const [row] = await query(
      world,
      `SELECT
        (SELECT count(*) FROM ${table}) AS typed,
        (SELECT count(*) FROM ${table} t WHERE t.event_id IS NOT NULL
          AND (SELECT count(*) FROM platform_events p WHERE p.event_id = t.event_id) = 1
          AND (SELECT any_value(p.event_kind) FROM platform_events p WHERE p.event_id = t.event_id) = '${kind}'
        ) AS typed_exact,
        (SELECT count(*) FROM platform_events WHERE event_kind = '${kind}') AS canonical,
        (SELECT count(*) FROM platform_events p WHERE p.event_kind = '${kind}'
          AND (SELECT count(*) FROM ${table} t WHERE t.event_id = p.event_id) = 1) AS canonical_exact`,
    );
    out[table] = [
      Number(row.typed),
      Number(row.typed_exact),
      Number(row.canonical),
      Number(row.canonical_exact),
    ];
  }
  return out;
}

/** Asserts every typed row joins one canonical row and vice versa; returns the typed row counts. */
async function assertExactJoins(world: World): Promise<Record<string, number>> {
  const report = await joinReport(world);
  const counts: Record<string, number> = {};
  for (const [table, [typed, typedExact, canonical, canonicalExact]] of Object.entries(report)) {
    assert.deepEqual([typedExact, canonical, canonicalExact], [typed, typed, typed], table);
    counts[table] = typed;
  }
  const duplicates = await query(
    world,
    "SELECT event_id FROM platform_events GROUP BY 1 HAVING count(*) > 1",
  );
  assert.deepEqual(duplicates, [], "no event_id is canonical twice");
  return counts;
}

test("every typed row joins exactly one canonical row on event_id; ts and trace_id are not join keys", async () => {
  await withWorld(async (world) => {
    // Two SQL events with the same ts, trace and app: only event_id tells them apart.
    const twin = (id: string, table: string) =>
      sqlEvent({
        ...envelope(id, "server", 40),
        engine: "sqlite",
        dialect: "sqlite",
        statementKind: "select",
        tableName: table,
        sqlText: `SELECT * FROM ${table}`,
        bindCount: 0,
        durationMs: 1,
        outcome: "ok",
      });
    await writeSource(
      world,
      SERVER_FILE,
      lines([...serverFixture(), twin("twin-a", "games"), twin("twin-b", "moves")]),
    );
    await writeSource(world, READER_FILE, lines(readerFixture()));
    await ingest(world, T0);

    assert.deepEqual(await assertExactJoins(world), {
      sql_events: 4,
      tx_events: 2,
      http_events: 1,
      queue_events: 1,
      backup_events: 1,
      lifecycle_events: 1,
    });
    const [byTime] = await query(
      world,
      `SELECT count(*) AS n FROM sql_events s JOIN platform_events p
         ON p.ts = s.ts AND p.trace_id = s.trace_id AND p.event_kind = 'sql' WHERE s.event_id = 'twin-a'`,
    );
    assert.equal(byTime.n, 2n, "ts + trace_id matches both twins");
    const [byId] = await query(
      world,
      `SELECT p.source_offset, s.table_name FROM sql_events s JOIN platform_events p USING (event_id)
       WHERE s.event_id = 'twin-a'`,
    );
    assert.equal(byId.table_name, "games");

    // Duplicate ingestion: an unchanged re-run reads nothing; a rewrite that brings back old lines, and
    // repeats a new one inside the batch, lands each event once.
    assert.equal((await ingest(world, T0 + 1000)).linesIngested, 0);
    const again = lifecycleEvent({ ...envelope("s-again", "server", 41), phase: "again" });
    await writeSource(world, SERVER_FILE, lines([serverFixture()[2], again, twin("twin-a", "games"), again]));
    assert.equal((await ingest(world, T0 + 2000)).linesIngested, 1);
    assert.equal((await assertExactJoins(world)).lifecycle_events, 2);
    assert.equal((await counts(world)).platform_events, 13);
  });
});

test("a record of an unknown kind, or with no kind, lands in platform_events raw with no typed row", async () => {
  await withWorld(async (world) => {
    const raw = (id: string, kind?: string) =>
      JSON.stringify({ ...envelope(id, "server", 42), ...(kind === undefined ? {} : { kind }), value: 7 }) +
      "\n";
    await writeSource(
      world,
      SERVER_FILE,
      lines(serverFixture()) + raw("m-metrics", "metrics") + raw("m-bare"),
    );
    await writeSource(world, READER_FILE, lines(readerFixture()));
    const report = await ingest(world, T0);
    assert.equal(report.linesRejected, 0, "an unknown or missing kind is not a reject");

    // The envelope lands; nothing kind-specific (`name`) is guessed.
    const landed = await query(
      world,
      `SELECT event_id, event_kind, handler_kind, role, trace_id, name, outcome FROM platform_events
       WHERE event_id IN ('m-metrics', 'm-bare') ORDER BY event_id`,
    );
    assert.deepEqual(landed, [
      {
        event_id: "m-bare",
        event_kind: null,
        handler_kind: null,
        role: "server",
        trace_id: "trace-1",
        name: null,
        outcome: null,
      },
      {
        event_id: "m-metrics",
        event_kind: "metrics",
        handler_kind: null,
        role: "server",
        trace_id: "trace-1",
        name: null,
        outcome: null,
      },
    ]);
    for (const table of Object.keys(TYPED_EVENT_TABLES)) {
      const [row] = await query(
        world,
        `SELECT count(*) AS n FROM ${table} WHERE event_id IN ('m-metrics', 'm-bare')`,
      );
      assert.equal(row.n, 0n, table);
    }
    // The typed kinds still join exactly.
    assert.equal((await assertExactJoins(world)).sql_events, 2);
  });
});

// ---------------------------------------------------------------------------
// Item 35: event_kind / handler_kind taxonomy and outcomes
// ---------------------------------------------------------------------------

test("event_kind and handler_kind are separate columns, mapped explicitly; rollback and error outcomes survive", async () => {
  await withWorld(async (world) => {
    const handler = (
      id: string,
      handlerKind: "query" | "mutation",
      outcome: "ok" | "error",
      errorTag?: string,
    ) =>
      handlerEvent({ ...envelope(id, "server", 1), handlerKind, name: id, outcome, durationMs: 1, errorTag });
    const http = (id: string, handlerKind: "query" | "mutation", status: number, errorTag?: string) =>
      httpEvent({
        ...envelope(id, "server", 2),
        method: handlerKind === "query" ? "GET" : "POST",
        path: `/apps/column/${handlerKind}/${id}`,
        handlerKind,
        name: id,
        status,
        durationMs: 1,
        bytesIn: 0,
        bytesOut: 0,
        errorTag,
      });
    const tx = (id: string, outcome: "commit" | "rollback" | "interrupted", errorTag?: string) =>
      txEvent({
        ...envelope(id, "server", 3),
        engine: "sqlite",
        outcome,
        operations: 1,
        durationMs: 1,
        queueWaitMs: 0,
        errorTag,
      });
    const raw = (id: string, fields: string) =>
      `{"eventId":"${id}","ts":"2026-10-10T12:00:04.000Z","level":"info","role":"server","pid":1,"traceId":"t",${fields}}\n`;
    await writeSource(
      world,
      SERVER_FILE,
      lines([
        handler("h-query", "query", "ok"),
        handler("h-mutation", "mutation", "error", "ConflictError"),
        http("w-query", "query", 200),
        http("w-mutation", "mutation", 409, "ConflictError"),
        tx("t-commit", "commit"),
        tx("t-rollback", "rollback", "ConflictError"),
        tx("t-interrupted", "interrupted"),
      ]) +
        // Not a handler kind the platform knows: kept as a handler event, handler_kind NULL.
        raw(
          "h-odd",
          `"kind":"handler","handlerKind":"subscription","name":"h-odd","outcome":"ok","durationMs":1`,
        ) +
        // Legacy JSONL shaped after §4.1's original field list: the handler's kind in `kind` itself.
        raw(
          "h-legacy",
          `"kind":"mutation","name":"h-legacy","outcome":"error","durationMs":1,"errorTag":"SchemaError"`,
        ),
    );
    await ingest(world, T0);

    const canonical = await query(
      world,
      `SELECT event_id, event_kind, handler_kind, name, outcome, error_tag FROM platform_events ORDER BY source_offset`,
    );
    assert.deepEqual(
      canonical.map((r) => [r.event_id, r.event_kind, r.handler_kind, r.outcome, r.error_tag]),
      [
        ["h-query", "handler", "query", "ok", null],
        ["h-mutation", "handler", "mutation", "error", "ConflictError"],
        ["w-query", "http", "query", "ok", null],
        ["w-mutation", "http", "mutation", "error", "ConflictError"],
        ["t-commit", "tx", null, "commit", null],
        ["t-rollback", "tx", null, "rollback", "ConflictError"],
        ["t-interrupted", "tx", null, "interrupted", null],
        ["h-odd", "handler", null, "ok", null],
        ["h-legacy", "handler", "mutation", "error", "SchemaError"],
      ],
    );
    assert.equal(canonical.find((r) => r.event_id === "h-legacy")?.name, "h-legacy");
    assert.deepEqual(
      await query(
        world,
        "SELECT event_id, handler_kind, status, error_tag FROM http_events ORDER BY event_id",
      ),
      [
        { event_id: "w-mutation", handler_kind: "mutation", status: 409, error_tag: "ConflictError" },
        { event_id: "w-query", handler_kind: "query", status: 200, error_tag: null },
      ],
    );
    assert.deepEqual(
      (await query(world, "SELECT outcome FROM tx_events ORDER BY event_id")).map((r) => r.outcome),
      ["commit", "interrupted", "rollback"],
    );
    // The per-handler question item 23 asks, answered on handler_kind alone — no kind overloading.
    const perKind = await query(
      world,
      `SELECT handler_kind, count(*) AS n, count(*) FILTER (WHERE outcome <> 'ok') AS failed
       FROM platform_events WHERE event_kind IN ('handler', 'http') AND handler_kind IS NOT NULL
       GROUP BY 1 ORDER BY 1`,
    );
    assert.deepEqual(perKind, [
      { handler_kind: "mutation", n: 3n, failed: 3n },
      { handler_kind: "query", n: 2n, failed: 0n },
    ]);
    // Every failure, across kinds: the outcome vocabularies item 23 must treat as failed.
    const [failures] = await query(
      world,
      "SELECT count(*) AS n FROM platform_events WHERE outcome IN ('error', 'rollback', 'interrupted')",
    );
    assert.equal(failures.n, 5n);
  });
});

// ---------------------------------------------------------------------------
// Item 35: backup records — emitter, run and attempts, old and new side by side
// ---------------------------------------------------------------------------

test("mixed pre-item-35 and current backup records: emitter is explicit or inferred, and one run counts once", async () => {
  await withWorld(async (world) => {
    // What items 18 and 19 wrote: no emitter, no runId, no attempts.
    const legacy = (id: string, phase: string, extra = "") =>
      `{"eventId":"${id}","ts":"2026-10-09T12:00:00.000Z","level":"info","role":"server","pid":1,"traceId":"old-trace",` +
      `"appId":"column","kind":"backup","phase":"${phase}","key":null,"bytes":null,"sha256":null,"durationMs":1,` +
      `"outcome":"ok","errorTag":null,"errorDetail":null${extra}}\n`;
    const legacyLines = [
      "snapshot",
      "verify",
      "hash",
      "put-object",
      "put-object",
      "put-manifest",
      "put-object",
      "publish-latest",
    ]
      .map((phase, i) => legacy(`old-${i}`, phase)).join("") +
      // Backups' own record of an interrupted upload, which item 19 marked `written: "unknown"`.
      legacy("old-interrupted", "put-object").replace(
        `"errorTag":null,"errorDetail":null`,
        `"errorTag":"Interrupted","errorDetail":{"written":"unknown"}`,
      );
    const current = (
      id: string,
      phase: BackupEventRecord["phase"],
      emitter: "backups" | "object-store",
      attempts?: number,
    ) =>
      backupEvent({
        ...envelope(id, "server", 5),
        phase,
        emitter,
        runId: "run-new",
        key: `k/${id}`,
        attempts,
        durationMs: 1,
        outcome: "ok",
      });
    await writeSource(
      world,
      SERVER_FILE,
      legacyLines +
        lines([
          current("new-snapshot", "snapshot", "backups"),
          current("new-put", "put-object", "backups"),
          current("new-transfer", "put-object", "object-store", 3),
        ]) +
        // An emitter this build does not know is not guessed at.
        legacy("odd-emitter", "hash", `,"emitter":"robot"`),
    );
    const report = await ingest(world, T0);
    assert.equal(report.linesRejected, 0);
    const rows = await query(
      world,
      "SELECT event_id, phase, emitter, run_id, attempts FROM backup_events ORDER BY event_id",
    );
    const emitters = Object.fromEntries(rows.map((r) => [r.event_id, r.emitter]));
    assert.deepEqual(emitters, {
      "new-put": "backups",
      "new-snapshot": "backups",
      "new-transfer": "object-store",
      "odd-emitter": null,
      "old-0": "backups",
      "old-1": "backups",
      "old-2": "backups",
      "old-3": "object-store",
      "old-4": "object-store",
      "old-5": "backups",
      "old-6": "object-store",
      "old-7": "backups",
      "old-interrupted": "backups",
    });
    assert.ok(rows.filter((r) => String(r.event_id).startsWith("old")).every((r) => r.run_id === null));
    assert.deepEqual(rows.find((r) => r.event_id === "new-transfer"), {
      event_id: "new-transfer",
      phase: "put-object",
      emitter: "object-store",
      run_id: "run-new",
      attempts: 3,
    });
    assert.deepEqual(await assertExactJoins(world), {
      sql_events: 0,
      tx_events: 0,
      http_events: 0,
      queue_events: 0,
      backup_events: 13,
      lifecycle_events: 0,
    });
    // The new run: one run, two phases, one transfer that took three tries.
    const [run] = await query(
      world,
      `SELECT count(DISTINCT run_id) AS runs,
         count(*) FILTER (WHERE emitter = 'backups') AS phases,
         count(*) FILTER (WHERE emitter = 'object-store') AS transfers,
         sum(attempts - 1) FILTER (WHERE emitter = 'object-store') AS retries
       FROM backup_events WHERE run_id IS NOT NULL`,
    );
    assert.deepEqual(run, { runs: 1n, phases: 2n, transfers: 1n, retries: 2n });
  });
});

// ---------------------------------------------------------------------------
// Item 35: schema versions — the v1 guard and the v1 → v2 rebuild
// ---------------------------------------------------------------------------

/** Item 21's DDL, frozen: what every event-log file written before item 35 holds. */
const V1_DDL = [
  `CREATE TABLE platform_events (event_id VARCHAR, ts TIMESTAMP, level VARCHAR, role VARCHAR, pid INTEGER,
    trace_id VARCHAR, span_id VARCHAR, parent_span_id VARCHAR, app_id VARCHAR, kind VARCHAR, name VARCHAR,
    outcome VARCHAR, error_tag VARCHAR, duration_ms DOUBLE, attrs JSON, source_file VARCHAR, source_offset BIGINT)`,
  `CREATE TABLE sql_events (ts TIMESTAMP, trace_id VARCHAR, app_id VARCHAR, engine VARCHAR, dialect VARCHAR,
    statement_kind VARCHAR, table_name VARCHAR, sql_text VARCHAR, bind_count INTEGER, duration_ms DOUBLE,
    row_count BIGINT, outcome VARCHAR, error_tag VARCHAR)`,
  `CREATE TABLE tx_events (ts TIMESTAMP, trace_id VARCHAR, app_id VARCHAR, engine VARCHAR, outcome VARCHAR,
    operations INTEGER, duration_ms DOUBLE, queue_wait_ms DOUBLE, error_tag VARCHAR)`,
  `CREATE TABLE http_events (ts TIMESTAMP, trace_id VARCHAR, method VARCHAR, path VARCHAR, app_id VARCHAR,
    kind VARCHAR, name VARCHAR, status INTEGER, duration_ms DOUBLE, bytes_in BIGINT, bytes_out BIGINT,
    error_tag VARCHAR)`,
  `CREATE TABLE queue_events (ts TIMESTAMP, queue VARCHAR, depth INTEGER, cap INTEGER, event VARCHAR,
    dropped BIGINT)`,
  `CREATE TABLE backup_events (ts TIMESTAMP, app_id VARCHAR, phase VARCHAR, key VARCHAR, bytes BIGINT,
    sha256 VARCHAR, duration_ms DOUBLE, outcome VARCHAR, error_tag VARCHAR)`,
  `CREATE TABLE lifecycle_events (ts TIMESTAMP, role VARCHAR, pid INTEGER, phase VARCHAR, detail JSON)`,
  `CREATE TABLE k6_points (ts TIMESTAMP, run_id VARCHAR, metric VARCHAR, value DOUBLE, scenario VARCHAR,
    status VARCHAR, trace_id VARCHAR, tags JSON)`,
  `CREATE TABLE _ingest_state (source_file VARCHAR PRIMARY KEY, bytes_ingested BIGINT, prefix_sha256 VARCHAR,
    last_ingested_at TIMESTAMP, dedupe BOOLEAN)`,
];

const sha256Hex = (bytes: Uint8Array) => createHash("sha256").update(bytes).digest("hex");
const fileSha = async (path: string) => sha256Hex(await Deno.readFile(path));

/**
 * Writes `sources` as JSONL and builds the v1 file item 21's ingest would have left for them: one
 * canonical row per record (`kind` the handler's query/mutation for handler records), one typed row
 * per typed record (no `event_id`), an `_ingest_state` row covering each whole file, and `k6Points`
 * k6 rows. Closed cleanly, so no WAL remains.
 */
async function writeV1World(
  world: World,
  sources: Record<string, EventRecord[]>,
  k6Points = 0,
): Promise<void> {
  await Deno.mkdir(join(world.dbPath, ".."), { recursive: true });
  const instance = await DuckDBInstance.create(world.dbPath);
  const connection = await instance.connect();
  try {
    for (const ddl of V1_DDL) await connection.run(ddl);
    for (const [file, records] of Object.entries(sources)) {
      const text = lines(records);
      await writeSource(world, file, text);
      let offset = 0;
      for (const record of records) {
        const ts = `CAST(${`'${record.ts}'`} AS TIMESTAMPTZ) AT TIME ZONE 'UTC'`;
        const kind = record.kind === "handler" ? record.handlerKind : record.kind;
        await connection.run(
          `INSERT INTO platform_events (event_id, ts, trace_id, kind, source_file, source_offset)
           VALUES ($1, ${ts}, $2, $3, $4, $5)`,
          [record.eventId, record.traceId, kind, file, offset],
        );
        const typed = Object.entries(TYPED_EVENT_TABLES).find(([, k]) => k === record.kind)?.[0];
        if (typed !== undefined) await connection.run(`INSERT INTO ${typed} (ts) VALUES (${ts})`);
        offset += new TextEncoder().encode(toJsonLine(record) + "\n").length;
      }
      const bytes = new TextEncoder().encode(text);
      await connection.run(
        "INSERT INTO _ingest_state VALUES ($1, $2, $3, TIMESTAMP '2026-10-10 13:00:00', false)",
        [file, bytes.length, sha256Hex(bytes)],
      );
    }
    for (let i = 0; i < k6Points; i++) {
      await connection.run(
        `INSERT INTO k6_points VALUES (TIMESTAMP '2026-10-10 12:00:00', 'k6-run', 'http_req_duration', ${i}, 'mixed',
          '200', NULL, '{"i":${i}}')`,
      );
    }
  } finally {
    connection.closeSync();
    instance.closeSync();
  }
}

async function upgradeEither(
  world: World,
  atMillis: number,
  options: Parameters<typeof upgradeEventLog>[0] = {},
): Promise<Either.Either<EventLogUpgradeReport, IngestError>> {
  const program = Effect.gen(function* () {
    const sunk = yield* Ref.make<EventRecord[]>([]);
    const sink = Layer.succeed(EventSink, {
      emit: (event: EventRecord) => Ref.update(sunk, (all) => [...all, event]),
    });
    yield* TestClock.setTime(atMillis);
    const result = yield* Effect.either(upgradeEventLog(options)).pipe(
      Effect.provide(Layer.merge(
        ConfigTest({ logging: { dir: world.logDir }, eventLog: { duckdbPath: world.dbPath } }),
        sink,
      )),
    );
    world.events.push(...(yield* Ref.get(sunk)));
    return result;
  });
  return await Effect.runPromise(program.pipe(Effect.provide(TestContext.TestContext)));
}

async function schemaStatus(world: World): Promise<EventLogSchemaStatus> {
  return await Effect.runPromise(
    inspectEventLogSchema().pipe(
      Effect.provide(ConfigTest({ logging: { dir: world.logDir }, eventLog: { duckdbPath: world.dbPath } })),
    ),
  );
}

/** The event-log directory's file names: the live file and any copies or side files beside it. */
async function dbDirectory(world: World): Promise<string[]> {
  const names: string[] = [];
  for await (const entry of Deno.readDir(join(world.dbPath, ".."))) names.push(entry.name);
  return names.sort();
}

const V1_SOURCES = () => ({ [SERVER_FILE]: serverFixture(), [READER_FILE]: readerFixture() });

test("ingest refuses a pre-change (v1) database before touching it, rather than reuse v1 tables", async () => {
  await withWorld(async (world) => {
    await writeV1World(world, V1_SOURCES());
    const before = await fileSha(world.dbPath);
    assert.equal(await schemaStatus(world), "v1");

    const refused = await ingestEither(world, T0);
    assert.ok(Either.isLeft(refused));
    assert.equal(refused.left.phase, "inventory");
    assert.match(refused.left.detail ?? "", /schema v1 \(unversioned\).*upgradeEventLog/);
    assert.equal(await fileSha(world.dbPath), before, "the v1 file is byte-for-byte unchanged");
    assert.deepEqual(await dbDirectory(world), ["events.duckdb"]);
    // Its offsets are exactly item 21's.
    assert.deepEqual((await stateRows(world)).map((r) => r.source_file), [READER_FILE, SERVER_FILE]);
  });
});

test("upgrade refuses when the retained JSONL cannot rebuild every v1 event; the v1 file and offsets stay as they were", async () => {
  await withWorld(async (world) => {
    await writeV1World(world, V1_SOURCES());
    // Retention gzipped the reader's file after v1 ingested it: `.gz` is not a source.
    await Deno.rename(join(world.logDir, READER_FILE), join(world.logDir, `${READER_FILE}.gz`));
    const before = await fileSha(world.dbPath);

    const refused = await upgradeEither(world, T0);
    assert.ok(Either.isLeft(refused));
    assert.equal(refused.left.phase, "plan");
    assert.match(
      refused.left.detail ?? "",
      /3 of 10 v1 platform_events rows have no event_id in the rebuild/,
    );
    assert.ok(refused.left.detail?.includes(READER_FILE), refused.left.detail);
    assert.match(refused.left.detail ?? "", /1 v1 'sql' events have no row in the rebuilt sql_events/);
    assert.match(refused.left.detail ?? "", /1 v1 'tx' events have no row in the rebuilt tx_events/);
    assert.equal(await fileSha(world.dbPath), before);
    assert.deepEqual(await dbDirectory(world), ["events.duckdb"], "no side file or copy is left behind");
    const event = world.events.at(-1);
    assert.ok(event?.kind === "lifecycle" && event.phase === "eventlog-schema-upgrade-refused");
    assert.equal(await schemaStatus(world), "v1");
    assert.ok(Either.isLeft(await ingestEither(world, T0)), "ingest still refuses the v1 file");

    // The source comes back (gunzipped in place): the same upgrade now succeeds.
    await Deno.rename(join(world.logDir, `${READER_FILE}.gz`), join(world.logDir, READER_FILE));
    const upgraded = await upgradeEither(world, T0 + 1000);
    assert.ok(Either.isRight(upgraded) && upgraded.right.status === "upgraded", JSON.stringify(upgraded));
    assert.equal(upgraded.right.eventsVerified, 10);
  });
});

/** Wraps the real driver; every `COMMIT` fails. */
function alwaysFailingCommitDriver(): EventLogDriver {
  return {
    open: async (path) => {
      const instance = await eventLogDriver.open(path);
      return {
        closeSync: () => instance.closeSync(),
        connect: async () => {
          const connection = await instance.connect();
          return {
            run: (sql, values) =>
              sql === "COMMIT"
                ? Promise.reject(new Error("injected commit failure"))
                : connection.run(sql, values),
            runAndReadAll: (sql) => connection.runAndReadAll(sql),
            closeSync: () => connection.closeSync(),
          };
        },
      };
    },
  };
}

test("a rebuild that fails part-way leaves the v1 file untouched and nothing beside it", async () => {
  await withWorld(async (world) => {
    await writeV1World(world, V1_SOURCES());
    const before = await fileSha(world.dbPath);
    const failed = await upgradeEither(world, T0, { driver: alwaysFailingCommitDriver() });
    assert.ok(Either.isLeft(failed));
    assert.equal(failed.left.phase, "commit");
    assert.equal(await fileSha(world.dbPath), before);
    assert.deepEqual(await dbDirectory(world), ["events.duckdb"]);
    assert.equal(await schemaStatus(world), "v1");
  });
});

test("upgrade rebuilds v1 through the real ingest, proves it complete, switches, and keeps the original", async () => {
  await withWorld(async (world) => {
    await writeV1World(world, V1_SOURCES(), 2);
    const original = await fileSha(world.dbPath);

    const result = await upgradeEither(world, T0);
    assert.ok(Either.isRight(result), JSON.stringify(result));
    const report = result.right;
    assert.ok(report.status === "upgraded");
    assert.deepEqual(
      [
        report.fromVersion,
        report.version,
        report.eventsVerified,
        report.k6PointsCopied,
        report.rebuild.linesIngested,
      ],
      [1, EVENTLOG_SCHEMA_VERSION, 10, 2, 10],
    );
    const backupName = "events.duckdb.v1-2026-10-10T13-00-00.000Z.duckdb";
    assert.equal(report.backupPath, join(world.dbPath, "..", backupName));
    assert.deepEqual(await dbDirectory(world), ["events.duckdb", backupName]);
    assert.equal(await fileSha(report.backupPath), original, "the original is kept byte-for-byte");

    // The live file is v2: stamped, exact joins, the v1 handler event re-mapped, k6 carried over.
    assert.equal(await schemaStatus(world), "current");
    const versions = await query(
      world,
      `SELECT version, method, CAST(detail AS VARCHAR) AS detail FROM _eventlog_schema ORDER BY method`,
    );
    assert.deepEqual(versions.map((r) => [r.version, r.method]), [[2, "create"], [2, "rebuild-from-v1"]]);
    assert.equal(JSON.parse(String(versions[1].detail)).eventsVerified, 10);
    assert.deepEqual(await assertExactJoins(world), {
      sql_events: 2,
      tx_events: 2,
      http_events: 1,
      queue_events: 1,
      backup_events: 1,
      lifecycle_events: 1,
    });
    const [handler] = await query(
      world,
      "SELECT event_kind, handler_kind FROM platform_events WHERE event_id = 's-handler'",
    );
    assert.deepEqual(handler, { event_kind: "handler", handler_kind: "query" });
    assert.equal((await counts(world)).k6_points, 2);
    const done = world.events.at(-1);
    assert.ok(done?.kind === "lifecycle" && done.phase === "eventlog-schema-upgraded");

    // The rebuild's offsets cover every source: the next ingest reads nothing, then only appends.
    assert.equal((await ingest(world, T0 + 1000)).linesIngested, 0);
    await appendSource(
      world,
      SERVER_FILE,
      lines([lifecycleEvent({ ...envelope("after", "server", 50), phase: "after" })]),
    );
    assert.equal((await ingest(world, T0 + 2000)).linesIngested, 1);
    // Upgrading again is a no-op.
    const again = await upgradeEither(world, T0 + 3000);
    assert.ok(Either.isRight(again));
    assert.deepEqual(again.right, { status: "current", version: EVENTLOG_SCHEMA_VERSION });
  });
});

test("an upgrade killed before its switch leaves no timestamped orphan; the next attempt clears its scratch", async () => {
  await withWorld(async (world) => {
    await writeV1World(world, V1_SOURCES());
    const original = await fileSha(world.dbPath);
    // What a process killed mid-copy and mid-rebuild leaves: a torn pending copy and a partial rebuild.
    const v1Bytes = await Deno.readFile(world.dbPath);
    await Deno.writeFile(`${world.dbPath}.v1-pending.duckdb`, v1Bytes.slice(0, v1Bytes.length >> 1));
    await Deno.writeTextFile(`${world.dbPath}.v2-rebuild.duckdb`, "torn");
    await Deno.writeTextFile(`${world.dbPath}.v2-rebuild.duckdb.wal`, "torn");

    const result = await upgradeEither(world, T0);
    assert.ok(Either.isRight(result) && result.right.status === "upgraded", JSON.stringify(result));
    const backupName = "events.duckdb.v1-2026-10-10T13-00-00.000Z.duckdb";
    assert.deepEqual(await dbDirectory(world), ["events.duckdb", backupName]);
    assert.equal(await fileSha(join(world.dbPath, "..", backupName)), original);
    assert.equal(await schemaStatus(world), "current");
  });
});

test("a fresh file is stamped v2 once, a missing one is absent, and a file of another version is refused", async () => {
  await withWorld(async (world) => {
    assert.equal(await schemaStatus(world), "absent");
    const absent = await upgradeEither(world, T0);
    assert.ok(Either.isRight(absent) && absent.right.status === "absent");

    await ingest(world, T0);
    await ingest(world, T0 + 1000);
    assert.deepEqual(
      (await query(world, "SELECT version, method FROM _eventlog_schema")).map((r) => [r.version, r.method]),
      [[EVENTLOG_SCHEMA_VERSION, "create"]],
    );
    for (const table of Object.keys(TYPED_EVENT_TABLES)) {
      const [first] = await query(
        world,
        `SELECT column_name FROM duckdb_columns() WHERE table_name = '${table}' ORDER BY column_index LIMIT 1`,
      );
      assert.equal(first.column_name, "event_id", table);
    }
    assert.equal(await schemaStatus(world), "current");

    await query(
      world,
      "INSERT INTO _eventlog_schema VALUES (3, TIMESTAMP '2027-01-01 00:00:00', 'create', NULL)",
    );
    const refused = await ingestEither(world, T0 + 2000);
    assert.ok(Either.isLeft(refused));
    assert.match(refused.left.detail ?? "", /schema v3 found; this build reads v2 only/);
    const upgrade = await upgradeEither(world, T0 + 3000);
    assert.ok(Either.isLeft(upgrade));
    assert.match(upgrade.left.detail ?? "", /schema v3/);
  });
});

// ---------------------------------------------------------------------------
// Pure helpers and source-level invariants
// ---------------------------------------------------------------------------

test("decide: new, append, skip, truncated and prefix-mismatch", () => {
  const state: IngestStateRow = {
    sourceFile: "f",
    bytesIngested: 10,
    prefixSha256: "h",
    dedupe: false,
    lastIngestedAt: "",
  };
  assert.deepEqual(decide("f", undefined, 0, undefined), { kind: "skip", sourceFile: "f" });
  assert.equal(decide("f", undefined, 5, undefined).kind, "read");
  assert.deepEqual(decide("f", state, 10, "h"), { kind: "skip", sourceFile: "f" });
  const append = decide("f", state, 15, "h");
  assert.ok(
    append.kind === "read" && append.mode === "append" && append.from === 10 && !append.dedupe,
  );
  const truncated = decide("f", state, 9, undefined);
  assert.ok(
    truncated.kind === "read" && truncated.rewrite === "truncated" && truncated.from === 0 &&
      truncated.dedupe,
  );
  const mismatch = decide("f", state, 10, "other");
  assert.ok(
    mismatch.kind === "read" && mismatch.rewrite === "prefix-mismatch" && mismatch.dedupe,
  );
  // An append to a file that was once rewritten stays deduplicated.
  const sticky = decide("f", { ...state, dedupe: true }, 20, "h");
  assert.ok(sticky.kind === "read" && sticky.mode === "append" && sticky.dedupe);
});

test("completeLines stops at the last newline and strips CR", () => {
  const bytes = new TextEncoder().encode("ab\r\n\ncd\nef");
  assert.deepEqual(completeLines(bytes, 0), { lines: [{ start: 0, end: 2 }, { start: 5, end: 7 }], end: 8 });
  assert.deepEqual(completeLines(bytes, 8), { lines: [], end: 8 });
});

test("parseSourceFile accepts only event-log sources", () => {
  assert.deepEqual(parseSourceFile("server/2026-10-10/events-14-31887-001.jsonl"), {
    path: "server/2026-10-10/events-14-31887-001.jsonl",
    role: "server",
    date: "2026-10-10",
    hour: 14,
    pid: 31887,
    seq: 1,
  });
  for (
    const path of [
      "server/2026-10-10/events-14-31887-001.jsonl.gz",
      "server/2026-10-10/rejected.jsonl",
      "k6/2026-10-10/events-14-1-001.jsonl",
      "server/events-14-1-001.jsonl",
      "../server/2026-10-10/events-14-1-001.jsonl",
    ]
  ) {
    assert.equal(parseSourceFile(path), undefined, path);
  }
});

const repoRoot = new URL("../../", import.meta.url);

test("ingest.ts and tables.ts change no existing row outside _ingest_state and the temp tables", async () => {
  const mutating = /\b(UPDATE|DELETE|TRUNCATE|DROP|ALTER)\b|INSERT OR REPLACE/;
  const allowed = /_ingest_state|UPSERT_INGEST_STATE|\$\{(ROWS|STAGE)_TABLE\}/;
  for (const file of ["eventlog/ingest.ts", "eventlog/tables.ts"]) {
    const source = await Deno.readTextFile(new URL(file, repoRoot));
    const offending = source.split("\n").filter((line) => mutating.test(line) && !allowed.test(line));
    assert.deepEqual(offending, [], file);
  }
});

test("core/eventlog is pure and the event log never loads node:sqlite", async () => {
  for (const file of ["core/eventlog/ddl.ts", "core/eventlog/offsets.ts"]) {
    const source = await Deno.readTextFile(new URL(file, repoRoot));
    assert.deepEqual(source.match(/^import .*$/gm) ?? [], [], file);
    assert.ok(!/\bDeno\./.test(source), `${file} touches Deno.*`);
  }
  for (const file of ["eventlog/ingest.ts", "eventlog/tables.ts"]) {
    assert.ok(!(await Deno.readTextFile(new URL(file, repoRoot))).includes("node:sqlite"), file);
  }
});
