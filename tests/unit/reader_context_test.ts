/**
 * Request context reception in the reader (plan item 33, `platform/reader_context.ts`).
 *
 * The pure half checks `receiveFrame`, `resolveRequestContext` and `withRequestContext` directly:
 * optional `traceId` / `spanId` / `parentSpanId` are validated on the unknown JSON before the frame
 * is narrowed, a valid caller trace is never replaced, an invalid one is ignored (never a protocol
 * violation), and an old sender keeps the reader-local fallback.
 *
 * The process half drives the real `platform/reader.ts` with the three kinds of sender and reads its
 * JSONL log: the request's log records and its DuckDB `tx` event carry the caller's trace, or the
 * fallback. This prepares item 34's end-to-end contract; it does not claim it — no real caller sends
 * these properties until item 34 extends the wire type and the IPC client.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { Effect, Either, Exit, HashMap, Layer, Logger, Ref } from "effect";
import type { EventRecord, LifecycleEventRecord, TxEventRecord } from "../../core/events.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileInsert } from "../../core/compile/write.ts";
import { schema } from "../../app/schema.ts";
import { IdGen, type ReaderReply } from "../../platform/services.ts";

/** Unwrap a right, failing the test with the rendered left on a left. */
function expectRight<A, E>(either: Either.Either<A, E>): A {
  if (Either.isRight(either)) return either.right;
  assert.fail(`expected a right, got left ${JSON.stringify(either.left)}`);
}

import {
  fallbackTraceId,
  readTraceContext,
  receiveFrame,
  resolveRequestContext,
  TRACE_TOKEN,
  withRequestContext,
} from "../../platform/reader_context.ts";

const repoRoot = fileURLToPath(new URL("../../", import.meta.url));

/** `span-1`, `span-2`, … */
function idGenTest() {
  let n = 0;
  return Layer.succeed(IdGen, { next: Effect.sync(() => `span-${++n}`) });
}

const receive = (value: unknown) => Effect.runSync(Effect.either(receiveFrame(JSON.stringify(value))));

function received(value: unknown) {
  const result = receive(value);
  assert.ok(Either.isRight(result), `expected a frame: ${JSON.stringify(value)}`);
  return result.right;
}

// ===========================================================================
// Reception: optional trace properties on unknown JSON
// ===========================================================================

test("receiveFrame: an old sender's frame has no trace context and is otherwise unchanged", () => {
  const frame = { id: 7, kind: "query", appId: "a", name: "getGame", args: { gameId: "g" } };
  const { frame: narrowed, trace } = received(frame);
  assert.deepEqual(narrowed, frame);
  assert.deepEqual(trace, { ignored: [] });
});

test("receiveFrame: valid traceId, spanId and parentSpanId are read beside every request kind", () => {
  const context = { traceId: "trace-ABC.1:2_x", spanId: "s-1", parentSpanId: "p-0" };
  for (
    const frame of [
      { id: 1, kind: "ping" },
      { id: 2, kind: "ingest" },
      { id: 3, kind: "query", appId: "a", name: "n", args: {} },
      { id: 4, kind: "analysis", name: "n", params: {} },
    ]
  ) {
    const result = received({ ...frame, ...context });
    assert.deepEqual(result.trace, { ...context, ignored: [] });
    assert.equal(result.frame.id, frame.id);
    assert.equal(result.frame.kind, frame.kind);
  }
});

test("receiveFrame: invalid trace properties are ignored and named, never a malformed frame", () => {
  const invalid: unknown[] = [42, "", "x".repeat(129), "has space", "semi;colon", "ünï", null, true, {}, [
    "t",
  ]];
  for (const value of invalid) {
    const result = received({ id: 1, kind: "ping", traceId: value, spanId: value, parentSpanId: value });
    assert.deepEqual(result.trace, { ignored: ["traceId", "spanId", "parentSpanId"] }, JSON.stringify(value));
  }
  // The bound is the HTTP ingress's: 128 characters of [A-Za-z0-9._:-].
  assert.ok(TRACE_TOKEN.test("x".repeat(128)));
  const mixed = received({ id: 1, kind: "ping", traceId: "ok", spanId: 5 });
  assert.deepEqual(mixed.trace, { traceId: "ok", ignored: ["spanId"] });
});

test("receiveFrame: a malformed frame still fails as one, with or without a valid trace", () => {
  for (
    const line of [
      { traceId: "t", id: "1", kind: "ping" },
      { traceId: "t", id: 1, kind: "nope" },
      { traceId: "t", id: 1, kind: "query", appId: 1, name: "n" },
      { traceId: "t", id: 1, kind: "analysis", name: "n" },
      ["t"],
      null,
    ]
  ) {
    const result = receive(line);
    assert.ok(Either.isLeft(result), JSON.stringify(line));
    assert.equal(result.left._tag, "IpcError");
    assert.equal(result.left.reason, "malformed-frame");
  }
  const notJson = Effect.runSync(Effect.either(receiveFrame("{")));
  assert.ok(Either.isLeft(notJson) && notJson.left.reason === "malformed-frame");
});

test("readTraceContext: own properties only; an explicit undefined is absent", () => {
  const inherited = Object.create({ traceId: "from-prototype" }) as Record<string, unknown>;
  assert.deepEqual(readTraceContext(inherited), { ignored: [] });
  assert.deepEqual(readTraceContext({ traceId: undefined }), { ignored: [] });
});

// ===========================================================================
// Resolution
// ===========================================================================

const resolve = (value: unknown, pid = 99) =>
  Effect.runSync(resolveRequestContext(received(value), pid).pipe(Effect.provide(idGenTest())));

test("resolveRequestContext: a valid caller trace is kept as is, span and parent too", () => {
  assert.deepEqual(
    resolve({ id: 3, kind: "ping", traceId: "caller", spanId: "s", parentSpanId: "p" }),
    { traceId: "caller", spanId: "s", parentSpanId: "p", origin: "caller", ignored: [] },
  );
  // Without a caller span, the reader names one; the caller's trace still stands.
  assert.deepEqual(
    resolve({ id: 3, kind: "ping", traceId: "caller", spanId: "bad span" }),
    { traceId: "caller", spanId: "span-1", origin: "caller", ignored: ["spanId"] },
  );
});

test("resolveRequestContext: an old sender, or an invalid trace, gets the reader-local fallback", () => {
  assert.equal(fallbackTraceId(99, 3), "reader-99-3");
  assert.deepEqual(
    resolve({ id: 3, kind: "ping" }),
    { traceId: "reader-99-3", spanId: "span-1", origin: "reader", ignored: [] },
  );
  // Span ids without a usable trace cannot be placed in it: they are not carried over.
  assert.deepEqual(
    resolve({ id: 4, kind: "ping", traceId: "", spanId: "s", parentSpanId: "p" }),
    { traceId: "reader-99-4", spanId: "span-1", origin: "reader", ignored: ["traceId"] },
  );
});

// ===========================================================================
// Annotation
// ===========================================================================

/** Runs `effect` with a logger that captures each record's annotations. */
function annotationsOf<A, E>(effect: Effect.Effect<A, E, IdGen>) {
  return Effect.runSync(Effect.gen(function* () {
    const seen = yield* Ref.make<Record<string, unknown>[]>([]);
    const logger = Logger.make(({ annotations }) => {
      Effect.runSync(Ref.update(seen, (all) => [...all, Object.fromEntries(HashMap.toEntries(annotations))]));
    });
    const exit = yield* Effect.exit(
      effect.pipe(Effect.provide(Layer.merge(idGenTest(), Logger.replace(Logger.defaultLogger, logger)))),
    );
    return { exit, records: yield* Ref.get(seen) };
  }));
}

test("withRequestContext: every log record of the request carries its context, including nested fibers", () => {
  const request = received({
    id: 5,
    kind: "query",
    appId: "app-1",
    name: "n",
    args: {},
    traceId: "caller-trace",
    spanId: "caller-span",
    parentSpanId: "caller-parent",
  });
  const { exit, records } = annotationsOf(
    withRequestContext(
      request,
      99,
      Effect.zipRight(
        Effect.logInfo("one"),
        Effect.fork(Effect.logInfo("two")).pipe(Effect.flatMap((f) => f.await)),
      ),
    ),
  );
  assert.ok(Exit.isSuccess(exit));
  assert.equal(records.length, 2);
  for (const annotations of records) {
    assert.deepEqual(annotations, {
      traceId: "caller-trace",
      spanId: "caller-span",
      parentSpanId: "caller-parent",
      traceOrigin: "caller",
      appId: "app-1",
    });
  }
});

test("withRequestContext: a caller trace is not replaced by an outer annotation; old senders get the fallback", () => {
  const outer = (effect: Effect.Effect<void, never, IdGen>) =>
    annotationsOf(Effect.annotateLogs(effect, { traceId: "outer" }));
  const caller = outer(
    withRequestContext(received({ id: 1, kind: "ingest", traceId: "kept" }), 99, Effect.log("x")),
  );
  assert.equal(caller.records[0].traceId, "kept");
  const legacy = outer(withRequestContext(received({ id: 8, kind: "ingest" }), 99, Effect.log("x")));
  assert.equal(legacy.records[0].traceId, "reader-99-8");
  assert.equal(legacy.records[0].traceOrigin, "reader");
  const invalid = outer(
    withRequestContext(received({ id: 9, kind: "ping", traceId: 7 }), 99, Effect.log("x")),
  );
  assert.equal(invalid.records[0].traceId, "reader-99-9");
  assert.deepEqual(invalid.records[0].ignoredTraceFields, ["traceId"]);
  // The request's failure channel is untouched.
  const failing = annotationsOf(
    withRequestContext(received({ id: 1, kind: "ping" }), 99, Effect.fail("boom")),
  );
  assert.ok(Exit.isFailure(failing.exit));
});

// ===========================================================================
// The real reader process
// ===========================================================================

async function withFixture(run: (dir: string) => Promise<void>): Promise<void> {
  const dir = await Deno.makeTempDir({ prefix: "mansio-reader-context-" });
  try {
    const { inspect } = await import("../helpers/sqlite_inspect.ts");
    const db = inspect(join(dir, "tic-tac-toe.sqlite"), { write: true });
    try {
      for (const statement of expectRight(compileDDL(schema))) db.exec(statement);
      const game = {
        xPlayer: "alice",
        oPlayer: "bob",
        board: [null, null, null, null, null, null, null, null, null],
        turn: "X",
        status: "playing",
        winner: null,
      };
      const insert = expectRight(
        compileInsert(schema, "games", game, { _id: "game-1", _creationTime: 1 }),
      );
      db.run(insert.text, insert.values);
    } finally {
      db.close();
    }
    await run(dir);
  } finally {
    await Deno.remove(dir, { recursive: true });
  }
}

async function recordsUnder(logDir: string): Promise<EventRecord[]> {
  const out: EventRecord[] = [];
  for await (const day of Deno.readDir(join(logDir, "reader"))) {
    for await (const file of Deno.readDir(join(logDir, "reader", day.name))) {
      const text = await Deno.readTextFile(join(logDir, "reader", day.name, file.name));
      for (const line of text.split("\n")) if (line !== "") out.push(JSON.parse(line) as EventRecord);
    }
  }
  return out;
}

test("reader process: query records carry the caller's trace, or the fallback for old and invalid senders", async () => {
  await withFixture(async (dir) => {
    const logDir = join(dir, "logs");
    const query = { kind: "query", appId: "tic-tac-toe", name: "getGame", args: { gameId: "game-1" } };
    const requests = [
      {
        id: 1,
        ...query,
        traceId: "caller-trace-1",
        spanId: "caller-span-1",
        parentSpanId: "caller-parent-1",
      },
      { id: 2, ...query },
      { id: 3, ...query, traceId: "not valid!", spanId: "caller-span-3" },
      { id: 4, ...query, traceId: "caller-trace-4" },
    ];
    const child = new Deno.Command(Deno.execPath(), {
      args: ["run", "-A", "platform/reader.ts"],
      cwd: repoRoot,
      env: { DATA_DIR: dir, LOG_DIR: logDir, EVENTLOG_DUCKDB: join(dir, "eventlog.duckdb") },
      stdin: "piped",
      stdout: "piped",
      stderr: "piped",
    }).spawn();
    const writer = child.stdin.getWriter();
    await writer.write(new TextEncoder().encode(requests.map((r) => JSON.stringify(r) + "\n").join("")));
    await writer.close();
    const output = await child.output();
    const stdout = new TextDecoder().decode(output.stdout);
    assert.equal(output.code, 0, new TextDecoder().decode(output.stderr));
    assert.equal(new TextDecoder().decode(output.stderr), "");

    // stdout: one reply frame per request, all answered — an invalid trace is not a protocol error.
    const lines = stdout.split("\n");
    assert.equal(lines.pop(), "");
    const replies = lines.map((line) => JSON.parse(line) as ReaderReply & { result?: { _id?: string } });
    assert.deepEqual(replies.map((r) => r.id), [1, 2, 3, 4]);
    for (const reply of replies) assert.equal(reply.result?._id, "game-1", JSON.stringify(reply));

    const records = await recordsUnder(logDir);
    const pid = child.pid;
    const txs = records.filter((r): r is TxEventRecord => r.kind === "tx");
    assert.deepEqual(
      txs.map((r) => r.traceId),
      ["caller-trace-1", `reader-${pid}-2`, `reader-${pid}-3`, "caller-trace-4"],
      "each request's tx event carries its own trace, in order",
    );
    // The first query opens the app's reader and logs it inside the request: the whole context.
    // `Effect.log*` records are `lifecycle` records whose `detail.message` is the log message.
    type Logged = LifecycleEventRecord & {
      detail?: { message?: string; annotations?: Record<string, unknown> };
    };
    const opened = records.find((r): r is Logged =>
      r.kind === "lifecycle" && (r as Logged).detail?.message === "reader opened"
    );
    assert.ok(opened, "the reader-opened log record was written");
    assert.equal(opened.traceId, "caller-trace-1");
    assert.equal(opened.spanId, "caller-span-1");
    const annotations = opened.detail?.annotations;
    assert.equal(annotations?.parentSpanId, "caller-parent-1");
    assert.equal(annotations?.traceOrigin, "caller");
  });
});
