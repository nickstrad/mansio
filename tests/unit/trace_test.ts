import { UnusedTenantRegistry } from "../layers/tenant_registry_test_layer.ts";
/**
 * Trace context in the server process (plan item 34), in process: the HTTP ingress contract
 * (`platform/http/trace.ts`), the router held to it, the handler span, and what the real SQLite
 * writer and the reader client see under it.
 *
 * The stack is the router over a **real** `makeStorage` on a temp SQLite file (so the `sql`/`tx`
 * events are the writer's own), `EventSinkTest`, and a `ReaderClient` double that records the trace
 * context of the fiber that called it — the context `platform/ipc.ts` captures at submission (that
 * capture itself is proven against a child in `tests/unit/ipc_test.ts`). The whole two-process path
 * is `tests/unit/trace_e2e_test.ts`.
 */
import { after, test } from "node:test";
import assert from "node:assert/strict";
import { Context, Effect, Exit, Fiber, HashMap, Layer, Logger, Option, Ref, type Scope } from "effect";
import fc from "fast-check";
import { ConflictError, ReaderError } from "../../core/errors.ts";
import type { EventRecord, HandlerEventRecord, HttpEventRecord } from "../../core/events.ts";
import { ConfigTest } from "../../platform/config.ts";
import { handle, TRACE_HEADER as ROUTER_TRACE_HEADER } from "../../platform/http/router.ts";
import {
  currentTraceContext,
  ingressTraceId,
  isTraceToken,
  TRACE_HEADER,
  TRACE_TOKEN,
  type TraceContext,
  withChildSpan,
} from "../../platform/http/trace.ts";
import { TRACE_TOKEN as READER_TRACE_TOKEN } from "../../platform/reader_context.ts";
import { AppServices, appsFromConfig, makeAppRegistry, makeAppServices } from "../../platform/registry.ts";
import { makeStorage } from "../../platform/sqlite.ts";
import {
  type App,
  AppRegistry,
  Config,
  ReaderClient,
  type ReaderClientService,
  type ReaderRequest,
} from "../../platform/services.ts";
import { defineFunctions } from "../../sdk/functions.ts";
import { EventSinkTest, IdGenTest, TestEvents } from "../layers/index.ts";

/** Unwrap a `Some`, failing the test on `None`. */
function expectSome<A>(option: Option.Option<A>): A {
  assert.ok(Option.isSome(option), "expected a Some, got None");
  return option.value;
}

// ===========================================================================
// The ingress contract
// ===========================================================================

test("one token bound everywhere: trace.ts, the reader receiver and the router's header", () => {
  assert.equal(
    TRACE_TOKEN.source,
    READER_TRACE_TOKEN.source,
    "HTTP ingress and the reader wire share one bound",
  );
  assert.equal(TRACE_TOKEN.flags, READER_TRACE_TOKEN.flags);
  assert.equal(TRACE_HEADER, ROUTER_TRACE_HEADER);
  assert.equal(TRACE_HEADER, TRACE_HEADER.toLowerCase());
});

test("ingressTraceId: 1-128 of [A-Za-z0-9._:-] is kept; absent or anything else is None", () => {
  const kept = ["a", "x".repeat(128), "A-z_0.9:-", crypto.randomUUID(), "1-0-k6-smoke"];
  for (const value of kept) assert.deepEqual(ingressTraceId(value), Option.some(value), value);
  const refused = [
    null,
    "",
    "x".repeat(129),
    "has space",
    "a,b",
    "a, b",
    "tab\there",
    "naïve",
    "a/b",
    "{json}",
    "é",
  ];
  for (const value of refused) assert.ok(Option.isNone(ingressTraceId(value)), String(value));
  assert.equal(isTraceToken(42), false);
});

// ===========================================================================
// Spans in a fiber
// ===========================================================================

test("withChildSpan: nested spans link to their parent; a root span has none; the trace is inherited", async () => {
  const seen = await Effect.runPromise(
    Effect.gen(function* () {
      const outside = yield* currentTraceContext;
      const root = yield* withChildSpan("root", currentTraceContext);
      const nested = yield* withChildSpan(
        "outer",
        Effect.zip(currentTraceContext, withChildSpan("inner", currentTraceContext)),
      );
      // A stale parent annotation does not survive into a fresh root span.
      const restarted = yield* withChildSpan("fresh", currentTraceContext).pipe(
        Effect.annotateLogs({ parentSpanId: "stale" }),
      );
      return { outside, root, nested, restarted };
    }).pipe(Effect.annotateLogs({ traceId: "trace-1" })),
  );
  assert.ok(Option.isSome(seen.outside));
  assert.deepEqual(seen.outside.value, { traceId: "trace-1" });
  assert.deepEqual(expectSome(seen.root), { traceId: "trace-1", spanId: "root" });
  assert.deepEqual(expectSome(seen.nested[0]), { traceId: "trace-1", spanId: "outer" });
  assert.deepEqual(expectSome(seen.nested[1]), {
    traceId: "trace-1",
    spanId: "inner",
    parentSpanId: "outer",
  });
  assert.deepEqual(expectSome(seen.restarted), { traceId: "trace-1", spanId: "fresh" });
});

test("currentTraceContext: None outside a trace; non-token annotations are not propagated", async () => {
  const [none, filtered] = await Effect.runPromise(Effect.all([
    currentTraceContext,
    currentTraceContext.pipe(Effect.annotateLogs({ traceId: "ok", spanId: "bad span", parentSpanId: 7 })),
  ]));
  assert.ok(Option.isNone(none));
  assert.deepEqual(expectSome(filtered), { traceId: "ok" });
  const invalid = await Effect.runPromise(currentTraceContext.pipe(Effect.annotateLogs({ traceId: "a b" })));
  assert.ok(Option.isNone(invalid));
});

test("concurrent fibers never see each other's span", async () => {
  const seen = await Effect.runPromise(Effect.forEach(
    Array.from({ length: 50 }, (_, i) => i),
    (i) =>
      withChildSpan(
        `span-${i}`,
        Effect.zipRight(Effect.sleep(`${(i * 7) % 11} millis`), currentTraceContext),
      ).pipe(Effect.annotateLogs({ traceId: `trace-${i}` })),
    { concurrency: "unbounded" },
  ));
  seen.forEach((context, i) =>
    assert.deepEqual(expectSome(context), { traceId: `trace-${i}`, spanId: `span-${i}` })
  );
});

// ===========================================================================
// The router and handlers over a real SQLite writer
// ===========================================================================

const APP = { id: "tic-tac-toe", token: "columns-token", storage: "columns" } as const;
const dataDir = await Deno.makeTempDir({ prefix: "mansio-trace-" });
after(() => Deno.remove(dataDir, { recursive: true }));

/** What the reader double saw: each request and the trace context of the fiber that sent it. */
interface Sent {
  readonly request: ReaderRequest;
  readonly context: Option.Option<TraceContext>;
}

/** Extra handlers on the real app, to put a log line, a rollback and a defect inside a handler. */
function withProbeHandlers(app: App): App {
  const { mutation } = defineFunctions(app.schema);
  return {
    ...app,
    functions: {
      ...app.functions,
      logThenConflict: mutation({
        args: {},
        handler: ({ db }) =>
          Effect.gen(function* () {
            yield* Effect.logInfo("inside the handler");
            yield* db.insert("games", {
              xPlayer: "rolled",
              oPlayer: "back",
              board: Array(9).fill(null),
              turn: "X",
              status: "playing",
              winner: null,
            });
            return yield* new ConflictError({ expected: 1, actual: 2 });
          }),
      }),
      dies: mutation({ args: {}, handler: () => Effect.die(new Error("handler bug")) }),
    },
  };
}

class ReaderSent extends Context.Tag("test/trace/ReaderSent")<ReaderSent, Ref.Ref<readonly Sent[]>>() {}

/**
 * One app over a real SQLite writer, and a reader double that records each request with the trace
 * context of its caller's fiber. `moveHistory` fails as the reader would on a broken query.
 */
const PlatformTrace = Layer.scopedContext(Effect.gen(function* () {
  const [app] = appsFromConfig(yield* Config).map(withProbeHandlers);
  const storage = yield* makeStorage({ file: app.file, schema: app.schema, appId: app.id });
  const sent = yield* Ref.make<readonly Sent[]>([]);
  const reader: ReaderClientService = {
    call: (request) =>
      Effect.gen(function* () {
        yield* Ref.update(sent, (all) => [...all, { request, context: Option.none() }]);
        const context = yield* currentTraceContext;
        yield* Ref.update(sent, (all) => [...all.slice(0, -1), { request, context }]);
        if (request.kind === "query" && request.name === "moveHistory") {
          return yield* new ReaderError({ operation: "select", table: "moves", detail: "disk on fire" });
        }
        return request.kind === "ping" ? "ready" : { answered: request.kind };
      }),
  };
  const backups = { take: () => Effect.die(new Error("no backups in this test")) };
  return Context.make(AppRegistry, makeAppRegistry([app])).pipe(
    Context.add(AppServices, makeAppServices([{ app, storage, backups }])),
    Context.add(ReaderClient, reader),
    Context.add(ReaderSent, sent),
  );
}));

/** The stack over its own data directory: `IdGenTest` restarts per build, so files are not shared. */
const traceStack = (dir: string) =>
  PlatformTrace.pipe(
    Layer.provideMerge(Layer.mergeAll(
      ConfigTest({
        server: { dataDir: dir, adminToken: "admin-token" },
        apps: [APP],
        logging: { includeBindValues: true, traceSql: true },
      }),
      UnusedTenantRegistry,
      IdGenTest,
      EventSinkTest,
    )),
  );

type Services = Layer.Layer.Success<ReturnType<typeof traceStack>>;

/** One log line as the logger sees it: message and annotations. */
interface LogLine {
  readonly message: unknown;
  readonly annotations: Readonly<Record<string, unknown>>;
}

/** Registers one test over a fresh stack and SQLite file. */
function traceTest(
  name: string,
  body: (logs: LogLine[]) => Effect.Effect<unknown, unknown, Services | Scope.Scope>,
) {
  test(name, async () => {
    const logs: LogLine[] = [];
    const capture = Logger.replace(
      Logger.defaultLogger,
      Logger.make(({ message, annotations }) => {
        logs.push({ message, annotations: Object.fromEntries(HashMap.toEntries(annotations)) });
      }),
    );
    const exit = await Effect.runPromiseExit(
      Effect.scoped(body(logs)).pipe(
        Effect.provide(traceStack(await Deno.makeTempDir({ dir: dataDir }))),
        Effect.provide(capture),
      ),
    );
    if (Exit.isFailure(exit)) throw new Error(String(exit.cause), { cause: exit.cause });
  });
}

interface Answer {
  readonly status: number;
  readonly body: { result?: unknown; error?: { tag: string; message: string } };
  readonly traceId: string | null;
}

function post(path: string, body: unknown, headers: Record<string, string>) {
  return Effect.gen(function* () {
    const response = yield* handle(
      new Request(`http://test.local${path}`, { method: "POST", headers, body: JSON.stringify(body) }),
    );
    const parsed = yield* Effect.promise(() => response.json());
    return { status: response.status, body: parsed, traceId: response.headers.get(TRACE_HEADER) } as Answer;
  });
}

const auth = (traceId?: string) => ({
  authorization: `Bearer ${APP.token}`,
  ...(traceId === undefined ? {} : { [TRACE_HEADER]: traceId }),
});

const appPath = (kind: "query" | "mutation", name: string) => `/apps/${APP.id}/${kind}/${name}`;

/** Every recorded event of trace `traceId`. */
const traced = (traceId: string) =>
  Effect.flatMap(TestEvents, (t) => t.where((event) => event.traceId === traceId));

function only<E extends EventRecord>(events: readonly EventRecord[], kind: E["kind"]): E {
  const matching = events.filter((event) => event.kind === kind);
  assert.equal(matching.length, 1, `exactly one ${kind} event, got ${matching.length}`);
  return matching[0] as E;
}

traceTest(
  "ingress: the router accepts exactly what ingressTraceId accepts and always echoes",
  () =>
    Effect.gen(function* () {
      const fixed = ["", "x".repeat(128), "x".repeat(129), "has space", "a, b", "ok-trace", "a/b", "é"];
      const generated = fc.sample(fc.string({ maxLength: 140 }), { numRuns: 200, seed: 34 });
      for (const presented of [...fixed, ...generated]) {
        let headers: Headers;
        try {
          headers = new Headers({ [TRACE_HEADER]: presented });
        } catch {
          continue; // not a legal header value at all; fetch would refuse to send it
        }
        // `Headers` trims surrounding whitespace; ingress sees what `get` returns.
        const seen = headers.get(TRACE_HEADER);
        const answer = yield* post("/no/such/route", {}, Object.fromEntries(headers));
        assert.equal(answer.status, 404);
        assert.ok(answer.traceId !== null && isTraceToken(answer.traceId), "the echo is always a valid id");
        if (Option.isSome(ingressTraceId(seen))) assert.equal(answer.traceId, seen);
        else assert.notEqual(answer.traceId, seen);
      }
    }),
);

traceTest(
  "a mutation: http, handler, sql and tx events share the trace; the handler's span covers SQL",
  (logs) =>
    Effect.gen(function* () {
      const answer = yield* post(
        appPath("mutation", "createGame"),
        { xPlayer: "ann", oPlayer: "bo" },
        auth("t-mutation"),
      );
      assert.equal(answer.status, 200);
      assert.equal(answer.traceId, "t-mutation");
      const events = yield* traced("t-mutation");
      const http = only<HttpEventRecord>(events, "http");
      const handler = only<HandlerEventRecord>(events, "handler");
      assert.equal(http.status, 200);
      assert.deepEqual(
        { kind: handler.handlerKind, name: handler.name, outcome: handler.outcome, appId: handler.appId },
        { kind: "mutation", name: "createGame", outcome: "ok", appId: APP.id },
      );
      assert.ok(handler.spanId && isTraceToken(handler.spanId));
      assert.equal(handler.parentSpanId, undefined, "the handler span is the request's root span");
      const sql = events.filter((event) => event.kind === "sql");
      const tx = events.filter((event) => event.kind === "tx");
      assert.ok(sql.some((event) => event.kind === "sql" && event.statementKind === "insert"));
      assert.deepEqual(tx.map((event) => event.kind === "tx" && event.outcome), ["commit"]);
      for (const event of [...sql, ...tx]) {
        assert.equal(event.spanId, handler.spanId, `${event.kind} in the handler span`);
      }
      assert.equal(logs.length, 0);
    }),
);

traceTest(
  "a rollback: the tx event, the handler's log line and its failure all carry the trace and span",
  (logs) =>
    Effect.gen(function* () {
      const answer = yield* post(appPath("mutation", "logThenConflict"), {}, auth("t-rollback"));
      assert.equal(answer.status, 409, JSON.stringify(answer.body));
      assert.equal(answer.traceId, "t-rollback");
      const events = yield* traced("t-rollback");
      const handler = only<HandlerEventRecord>(events, "handler");
      assert.deepEqual(
        { outcome: handler.outcome, errorTag: handler.errorTag, level: handler.level },
        { outcome: "error", errorTag: "ConflictError", level: "warn" },
      );
      const tx = only(events, "tx");
      assert.ok(tx.kind === "tx" && tx.outcome === "rollback" && tx.errorTag === "ConflictError");
      assert.equal(tx.spanId, handler.spanId);
      const insert = events.find((event) => event.kind === "sql" && event.statementKind === "insert");
      assert.equal(insert?.spanId, handler.spanId, "the rolled-back insert is in the handler span");
      const line = logs.find((log) => String(log.message).includes("inside the handler"));
      assert.ok(line, "the handler's own log call ran");
      assert.deepEqual(
        { traceId: line.annotations.traceId, spanId: line.annotations.spanId, appId: line.annotations.appId },
        { traceId: "t-rollback", spanId: handler.spanId, appId: APP.id },
      );
      assert.equal(only<HttpEventRecord>(events, "http").errorTag, "ConflictError");
    }),
);

traceTest(
  "a defect and an auth failure are still recorded under the request's trace",
  () =>
    Effect.gen(function* () {
      const defect = yield* post(appPath("mutation", "dies"), {}, auth("t-defect"));
      assert.equal(defect.status, 500);
      assert.equal(defect.traceId, "t-defect");
      const handler = only<HandlerEventRecord>(yield* traced("t-defect"), "handler");
      assert.deepEqual({ outcome: handler.outcome, errorTag: handler.errorTag }, {
        outcome: "error",
        errorTag: "Defect",
      });

      const denied = yield* post(appPath("mutation", "createGame"), {}, { [TRACE_HEADER]: "t-denied" });
      assert.equal(denied.status, 401);
      assert.equal(denied.traceId, "t-denied");
      const deniedEvents = yield* traced("t-denied");
      assert.equal(only<HandlerEventRecord>(deniedEvents, "handler").errorTag, "AuthError");
      assert.equal(only<HttpEventRecord>(deniedEvents, "http").status, 401);
    }),
);

traceTest(
  "a query: the reader call is made from inside the handler span; a reader error keeps the trace",
  () =>
    Effect.gen(function* () {
      const ok = yield* post(appPath("query", "listGames"), { status: "playing" }, auth("t-query"));
      assert.equal(ok.status, 200);
      const failed = yield* post(appPath("query", "moveHistory"), { gameId: "g" }, auth("t-reader-error"));
      assert.equal(failed.status, 500);
      assert.equal(failed.traceId, "t-reader-error");
      assert.match(failed.body.error!.message, /t-reader-error/, "a 5xx names the trace to look up");

      const sent = yield* Ref.get(yield* ReaderSent);
      const byTrace = (traceId: string) =>
        sent.find((s) => Option.getOrUndefined(s.context)?.traceId === traceId);
      for (const traceId of ["t-query", "t-reader-error"]) {
        const call = byTrace(traceId);
        assert.ok(call, `the reader call of ${traceId} ran under it`);
        const handler = only<HandlerEventRecord>(yield* traced(traceId), "handler");
        assert.equal(expectSome(call.context).spanId, handler.spanId);
        assert.deepEqual(
          Object.keys(call.request).filter((key) => key.endsWith("Id") && key !== "appId"),
          [],
          "handlers leave the wire trace to ReaderClient",
        );
      }
      const readerError = only<HandlerEventRecord>(yield* traced("t-reader-error"), "handler");
      assert.deepEqual(
        { outcome: readerError.outcome, errorTag: readerError.errorTag, level: readerError.level },
        { outcome: "error", errorTag: "ReaderError", level: "error" },
      );
    }),
);

traceTest("generated and replaced trace ids are what the events carry", () =>
  Effect.gen(function* () {
    const generated = yield* post(appPath("mutation", "createGame"), { xPlayer: "g", oPlayer: "h" }, auth());
    const replaced = yield* post(
      appPath("mutation", "createGame"),
      { xPlayer: "i", oPlayer: "j" },
      auth("bad trace!"),
    );
    for (const answer of [generated, replaced]) {
      assert.equal(answer.status, 200);
      assert.ok(answer.traceId && isTraceToken(answer.traceId));
      const events = yield* traced(answer.traceId);
      assert.deepEqual(
        [...new Set(events.map((event) => event.kind))].sort(),
        ["handler", "http", "sql", "tx"],
      );
    }
    assert.notEqual(generated.traceId, replaced.traceId);
    const all = yield* Effect.flatMap(TestEvents, (t) => t.all);
    assert.ok(!all.some((event) => event.traceId === "bad trace!"), "an invalid id is never recorded");
  }));

traceTest(
  "overlapping mutations: every sql/tx event carries its own caller's trace, span and values",
  () =>
    Effect.gen(function* () {
      const callers = Array.from(
        { length: 16 },
        (_, i) => ({ traceId: `overlap-${i}`, player: `player-${i}` }),
      );
      const fibers = yield* Effect.forEach(
        callers,
        (c) =>
          Effect.fork(
            post(appPath("mutation", "createGame"), { xPlayer: c.player, oPlayer: "o" }, auth(c.traceId)),
          ),
      );
      const answers = yield* Effect.forEach(fibers, Fiber.join);
      answers.forEach((answer, i) => assert.equal(answer.traceId, callers[i].traceId));
      for (const caller of callers) {
        const events = yield* traced(caller.traceId);
        const handler = only<HandlerEventRecord>(events, "handler");
        const insert = events.filter((event) => event.kind === "sql" && event.statementKind === "insert");
        assert.equal(insert.length, 1);
        assert.ok(
          insert[0].kind === "sql" && JSON.stringify(insert[0].bindValues).includes(`"${caller.player}"`),
          `${caller.traceId}'s insert binds ${caller.player}`,
        );
        for (const event of events.filter((e) => e.kind === "sql" || e.kind === "tx")) {
          assert.equal(event.spanId, handler.spanId);
        }
      }
    }),
);
