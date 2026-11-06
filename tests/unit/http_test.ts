/**
 * The HTTP surface, in process (plan item 22): `platform/http/router.ts`'s `handle` over test doubles
 * — `StorageTest` + `ReaderTest` (one shared in-memory store per app), `ObjectStoreTest`,
 * `EventSinkTest`, `IdGenTest`, and a `ReaderClient` that answers in this process instead of a child.
 * **No subprocess and no DuckDB**; every request is a real `Request` and every answer a real
 * `Response`. The one exception to the doubles is the last section, which swaps `EventSinkTest`
 * for the real server `LoggingLive` to show the events reach the JSONL file. What only the real
 * composition can show (partial startup, SIGTERM, the reader child, finalizer order) is in
 * `tests/unit/server_scope_test.ts` and `tests/unit/server_process_test.ts`.
 */
import { UnusedTenantRegistry } from "../layers/tenant_registry_test_layer.ts";
import { test } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import { Context, Deferred, Effect, Exit, Fiber, Layer, Option, Ref, type Scope, TestContext } from "effect";
import {
  HandlerKindMismatch,
  IngestError,
  IpcError,
  ReaderExitedError,
  StatementDeadlineError,
  toHttpStatus,
  UnknownAnalysisQuery,
  UnknownAppError,
  UnknownHandlerError,
} from "../../core/errors.ts";
import type { HttpEventRecord } from "../../core/events.ts";
import { BackupsLive } from "../../platform/backup.ts";
import { ConfigTest } from "../../platform/config.ts";
import { errorMessage, handle, matchRoute, type RouterRequirements } from "../../platform/http/router.ts";
import type { CallError } from "../../platform/ipc.ts";
import { LoggingLive } from "../../platform/logging/logger.ts";
import {
  AppServices,
  appsFromConfig,
  type AppStack,
  makeAppRegistry,
  makeAppServices,
} from "../../platform/registry.ts";
import {
  type App,
  AppRegistry,
  Backups,
  Config,
  Reader,
  ReaderClient,
  type ReaderClientService,
  type ReaderRequest,
  type ReaderService,
  Storage,
} from "../../platform/services.ts";
import {
  DocumentStoreTest,
  EventSinkTest,
  eventsOfKind,
  IdGenTest,
  ObjectStoreTest,
  type TestEvents,
} from "../layers/index.ts";
import { functions } from "../../app/functions.ts";
import { inspect } from "../helpers/sqlite_inspect.ts";

// ===========================================================================
// The in-process platform
// ===========================================================================

const ADMIN = "admin-token";
const COLUMNS = { id: "tic-tac-toe", token: "columns-token", storage: "columns" } as const;
const JSON_APP = { id: "tic-tac-toe-json", token: "json-token", storage: "json" } as const;

/** Holds `Storage.snapshot` inside the writer queue while armed, so a test can fill the queue. */
interface Held {
  readonly entered: Deferred.Deferred<void>;
  readonly release: Deferred.Deferred<void>;
}
interface SnapshotGate {
  readonly armed: Ref.Ref<Option.Option<Held>>;
}
class Gate extends Context.Tag("test/http/Gate")<Gate, SnapshotGate>() {}

/** Every request the in-process reader received, oldest first. */
class ReaderCalls
  extends Context.Tag("test/http/ReaderCalls")<ReaderCalls, Ref.Ref<readonly ReaderRequest[]>>() {}

/** While `Some`, every reader call fails with it: the child exited, timed out, or broke the protocol. */
class ReaderFault
  extends Context.Tag("test/http/ReaderFault")<ReaderFault, Ref.Ref<Option.Option<CallError>>>() {}

/** A real SQLite file, so `BackupsLive`'s `quick_check` has something to check. */
function writeValidSnapshot(path: string): void {
  const db = inspect(path, { write: true });
  try {
    db.exec("CREATE TABLE t (v TEXT)");
  } finally {
    db.close();
  }
}

/**
 * The reader process, in this process: the same lookups and failures `platform/reader.ts` answers
 * with, over each app's `ReaderTest`. `ingest` and `analysis` are canned — they belong to the
 * reader, and here only their routing is under test. The analysis name `boom` dies (a defect, not a
 * typed failure), and an armed `ReaderFault` fails every call as `ReaderClientLive` would.
 */
function readerClientTest(
  apps: readonly App[],
  readers: ReadonlyMap<string, ReaderService>,
  calls: Ref.Ref<readonly ReaderRequest[]>,
  fault: Ref.Ref<Option.Option<CallError>>,
): ReaderClientService {
  const answer = (request: ReaderRequest): ReturnType<ReaderClientService["call"]> => {
    switch (request.kind) {
      case "ping":
        return Effect.succeed("ready");
      case "query": {
        const app = apps.find((candidate) => candidate.id === request.appId);
        if (!app) return Effect.fail(new UnknownAppError({ appId: request.appId }));
        if (!Object.hasOwn(app.functions, request.name)) {
          return Effect.fail(new UnknownHandlerError({ appId: app.id, name: request.name }));
        }
        const definition = app.functions[request.name];
        if (definition.kind !== "query") {
          return Effect.fail(
            new HandlerKindMismatch({
              appId: app.id,
              name: request.name,
              expected: "query",
              actual: "mutation",
            }),
          );
        }
        return readers.get(app.id)!.query(definition, request.args);
      }
      case "ingest":
        return Effect.fail(new IngestError({ phase: "read", detail: "/srv/secret/logs unreadable" }));
      case "analysis":
        if (request.name === "boom") return Effect.die(new Error("analysis handler blew up"));
        return request.name === "errorsByTag"
          ? Effect.succeed([{ error_tag: "ConflictError", n: 1, params: request.params }])
          : Effect.fail(new UnknownAnalysisQuery({ name: request.name, known: ["errorsByTag"] }));
    }
  };
  return {
    call: (request) =>
      Effect.gen(function* () {
        yield* Ref.update(calls, (all) => [...all, request]);
        const failure = yield* Ref.get(fault);
        if (Option.isSome(failure)) return yield* Effect.fail(failure.value);
        return yield* answer(request);
      }),
  };
}

/**
 * `AppRegistry`, `AppServices` (one `DocumentStoreTest` + `BackupsLive` per app) and the in-process
 * `ReaderClient`, over the config below. Mirrors `AppServicesLive` in `platform/layers.ts`.
 */
const PlatformTest = Layer.scopedContext(
  Effect.gen(function* () {
    const apps = appsFromConfig(yield* Config);
    const gate: SnapshotGate = { armed: yield* Ref.make(Option.none<Held>()) };
    const stacks: AppStack[] = [];
    const readers = new Map<string, ReaderService>();
    for (const app of apps) {
      const stack = BackupsLive.pipe(
        Layer.provideMerge(DocumentStoreTest({
          schema: app.schema,
          appId: app.id,
          onSnapshot: (path) =>
            Effect.gen(function* () {
              writeValidSnapshot(path);
              const armed = yield* Ref.get(gate.armed);
              if (Option.isSome(armed)) {
                yield* Deferred.succeed(armed.value.entered, undefined);
                yield* Deferred.await(armed.value.release);
              }
            }),
        })),
      );
      const context = yield* Layer.build(stack);
      stacks.push({ app, storage: Context.get(context, Storage), backups: Context.get(context, Backups) });
      readers.set(app.id, Context.get(context, Reader));
    }
    const calls = yield* Ref.make<readonly ReaderRequest[]>([]);
    const fault = yield* Ref.make(Option.none<CallError>());
    return Context.make(AppRegistry, makeAppRegistry(apps)).pipe(
      Context.add(AppServices, makeAppServices(stacks)),
      Context.add(ReaderClient, readerClientTest(apps, readers, calls, fault)),
      Context.add(ReaderCalls, calls),
      Context.add(ReaderFault, fault),
      Context.add(Gate, gate),
    );
  }),
);

const TEST_CONFIG = { server: { adminToken: ADMIN }, apps: [COLUMNS, JSON_APP] };

const TestStack = PlatformTest.pipe(
  Layer.provideMerge(
    Layer.mergeAll(
      ConfigTest(TEST_CONFIG),
      UnusedTenantRegistry,
      IdGenTest,
      EventSinkTest,
      ObjectStoreTest(),
      TestContext.TestContext,
    ),
  ),
);

type TestServices = Layer.Layer.Success<typeof TestStack>;

/** Registers one in-process test. */
function httpTest(
  name: string,
  body: Effect.Effect<unknown, unknown, TestServices | RouterRequirements | TestEvents | Scope.Scope>,
) {
  test(name, async () => {
    const exit = await Effect.runPromiseExit(Effect.provide(Effect.scoped(body), TestStack));
    if (Exit.isFailure(exit)) throw new Error(String(exit.cause), { cause: exit.cause });
  });
}

interface Sent {
  readonly status: number;
  readonly body: { result?: unknown; error?: { tag: string; message: string } };
  readonly traceId: string | null;
}

/** One request through `handle`. */
function send(
  method: string,
  path: string,
  options: { token?: string; body?: unknown; raw?: BodyInit; headers?: Record<string, string> } = {},
): Effect.Effect<Sent, never, RouterRequirements> {
  return Effect.gen(function* () {
    const headers = new Headers(options.headers);
    if (options.token !== undefined) headers.set("authorization", `Bearer ${options.token}`);
    const body = options.raw ?? (options.body === undefined ? undefined : JSON.stringify(options.body));
    const response = yield* handle(new Request(`http://test.local${path}`, { method, headers, body }));
    const parsed = yield* Effect.promise(() => response.json());
    return { status: response.status, body: parsed, traceId: response.headers.get("x-trace-id") };
  });
}

const app = (kind: "query" | "mutation", name: string, body: unknown, token: string = COLUMNS.token) =>
  send("POST", `/apps/${COLUMNS.id}/${kind}/${name}`, { token, body });

/** The `error` body a failure must carry: tag + message, under the key `sdk/client.ts` reads. */
function assertError(sent: Sent, status: number, tag: string): void {
  assert.equal(sent.status, status, JSON.stringify(sent.body));
  assert.equal(sent.body.error?.tag, tag);
  assert.equal(typeof sent.body.error?.message, "string");
  assert.ok(sent.body.error!.message.length > 0);
  assert.equal(sent.body.result, undefined);
}

const lastHttpEvent = Effect.map(eventsOfKind("http"), (all) => all.at(-1)!);

// ===========================================================================
// Routing
// ===========================================================================

test("matchRoute keeps the pre-port route shapes and adds the admin log routes", () => {
  const name = (method: string, path: string) => Option.getOrUndefined(matchRoute(method, path))?.name;
  assert.equal(name("GET", "/health"), "health");
  assert.equal(name("POST", "/health"), undefined);
  assert.equal(name("POST", "/apps/a/query/getGame"), "getGame");
  assert.equal(name("POST", "/apps/a/mutation/make_move2"), "make_move2");
  assert.equal(name("GET", "/apps/a/query/getGame"), undefined);
  assert.equal(name("POST", "/apps/a/delete/getGame"), undefined);
  assert.equal(name("POST", "/apps/a/query/2bad"), undefined);
  assert.equal(name("POST", "/apps/a/b/query/x"), undefined);
  assert.equal(name("POST", "/admin/backup/a"), "admin.backup");
  assert.equal(name("POST", "/admin/logs/ingest"), "admin.logs.ingest");
  assert.equal(name("POST", "/admin/logs/analysis/errorsByTag"), "admin.logs.analysis.errorsByTag");
  assert.equal(name("POST", "/admin/logs/analysis/x;DROP"), undefined);
  assert.equal(name("POST", "/admin/logs/analysis/slowest-sql"), "admin.logs.analysis.slowest-sql");
  assert.equal(
    name("POST", "/admin/logs/analysis/error-rate-by-handler"),
    "admin.logs.analysis.error-rate-by-handler",
  );
  assert.equal(name("POST", "/admin/logs/analysis/k6-vs-platform"), "admin.logs.analysis.k6-vs-platform");
  assert.equal(name("POST", "/admin/logs/analysis/-leading"), undefined);
  assert.equal(name("POST", "/admin/logs/analysis/a..b"), undefined);
  assert.equal(name("POST", "/admin/logs/analysis/a/b"), undefined);
});

httpTest(
  "GET /health answers the reader's ping and the served apps",
  Effect.gen(function* () {
    const sent = yield* send("GET", "/health");
    assert.equal(sent.status, 200);
    assert.deepEqual(sent.body, { result: { ok: true, apps: [COLUMNS.id, JSON_APP.id] } });
    assert.deepEqual(yield* Ref.get(yield* ReaderCalls), [{ kind: "ping" }]);
  }),
);

httpTest(
  "an unmatched route or method is a 404 RouteNotFound",
  Effect.gen(function* () {
    assertError(yield* send("GET", "/nope"), 404, "RouteNotFound");
    assertError(yield* send("GET", `/apps/${COLUMNS.id}/query/getGame`), 404, "RouteNotFound");
    assertError(yield* send("POST", "/health"), 404, "RouteNotFound");
  }),
);

// ===========================================================================
// Mutations and queries
// ===========================================================================

httpTest(
  "a mutation commits to the app's storage and a query reads it back, per app",
  Effect.gen(function* () {
    const created = yield* app("mutation", "createGame", { xPlayer: "alice", oPlayer: "bob" });
    assert.equal(created.status, 200, JSON.stringify(created.body));
    const { gameId } = created.body.result as { gameId: string };

    const game = yield* app("query", "getGame", { gameId });
    assert.equal(game.status, 200);
    assert.equal((game.body.result as { _version: number })._version, 1);

    const moved = yield* app("mutation", "makeMove", {
      gameId,
      playerId: "alice",
      cell: 4,
      expectedVersion: 1,
    });
    assert.equal(moved.status, 200, JSON.stringify(moved.body));
    assert.equal((moved.body.result as { _version: number })._version, 2);
    assert.deepEqual((yield* app("query", "getGame", { gameId })).body.result, moved.body.result);

    // App isolation: the other app's store has no such game.
    const other = yield* send("POST", `/apps/${JSON_APP.id}/query/getGame`, {
      token: JSON_APP.token,
      body: { gameId },
    });
    assert.equal(other.status, 200);
    assert.equal(other.body.result, null);
  }),
);

httpTest(
  "an absent or empty body is {}; malformed JSON is a 400 SchemaError",
  Effect.gen(function* () {
    const empty = yield* send("POST", `/apps/${COLUMNS.id}/query/winsByMark`, { token: COLUMNS.token });
    assert.equal(empty.status, 200, JSON.stringify(empty.body));
    assert.deepEqual(empty.body.result, []);
    assertError(
      yield* send("POST", `/apps/${COLUMNS.id}/query/winsByMark`, { token: COLUMNS.token, raw: "{nope" }),
      400,
      "SchemaError",
    );
  }),
);

// ===========================================================================
// The status mapping
// ===========================================================================

httpTest(
  "unknown app → 404, unknown handler → 404, kind mismatch → 400",
  Effect.gen(function* () {
    assertError(
      yield* send("POST", "/apps/nope/query/getGame", { token: COLUMNS.token }),
      404,
      "UnknownAppError",
    );
    assertError(yield* app("query", "noSuchHandler", {}), 404, "UnknownHandlerError");
    assertError(
      yield* app("query", "createGame", { xPlayer: "a", oPlayer: "b" }),
      400,
      "HandlerKindMismatch",
    );
    assertError(yield* app("mutation", "getGame", { gameId: "x" }), 400, "HandlerKindMismatch");
  }),
);

httpTest(
  "missing, malformed or wrong tokens → 401, on app and admin routes alike",
  Effect.gen(function* () {
    const path = `/apps/${COLUMNS.id}/query/listGames`;
    assertError(yield* send("POST", path, { body: { status: "won" } }), 401, "AuthError");
    assertError(yield* app("query", "listGames", { status: "won" }, "wrong"), 401, "AuthError");
    assertError(yield* app("query", "listGames", { status: "won" }, JSON_APP.token), 401, "AuthError");
    assertError(
      yield* send("POST", path, { headers: { authorization: `Basic ${COLUMNS.token}` } }),
      401,
      "AuthError",
    );
    assertError(
      yield* send("POST", `/admin/backup/${COLUMNS.id}`, { token: COLUMNS.token }),
      401,
      "AuthError",
    );
    assertError(yield* send("POST", "/admin/logs/ingest"), 401, "AuthError");
    assertError(yield* send("POST", "/admin/logs/analysis/errorsByTag", { token: "x" }), 401, "AuthError");
    // The handler never ran: no reader call was made for any of these.
    assert.deepEqual(yield* Ref.get(yield* ReaderCalls), []);
  }),
);

httpTest(
  "a body over the 64 KiB cap → 413 RequestTooLarge, checked after auth",
  Effect.gen(function* () {
    const big = JSON.stringify({ xPlayer: "a".repeat(70_000), oPlayer: "b" });
    assertError(
      yield* send("POST", `/apps/${COLUMNS.id}/mutation/createGame`, { token: COLUMNS.token, raw: big }),
      413,
      "RequestTooLarge",
    );
    // Unauthenticated, the same body is a 401: the server does not read it.
    assertError(
      yield* send("POST", `/apps/${COLUMNS.id}/mutation/createGame`, { raw: big }),
      401,
      "AuthError",
    );
    const event = yield* lastHttpEvent;
    assert.equal(event.bytesIn, 0);
  }),
);

httpTest(
  "a stale _version → 409 ConflictError (was a 400 before the port)",
  Effect.gen(function* () {
    const { gameId } = (yield* app("mutation", "createGame", { xPlayer: "a", oPlayer: "b" })).body.result as {
      gameId: string;
    };
    assert.equal(
      (yield* app("mutation", "makeMove", { gameId, playerId: "a", cell: 0, expectedVersion: 1 })).status,
      200,
    );
    const stale = yield* app("mutation", "makeMove", { gameId, playerId: "b", cell: 1, expectedVersion: 1 });
    assertError(stale, 409, "ConflictError");
    assert.match(stale.body.error!.message, /expected 1, found 2/);
  }),
);

httpTest(
  "a validation failure → 400 SchemaError; an app rule → 400 RuleError; a missing document → 404",
  Effect.gen(function* () {
    assertError(yield* app("mutation", "createGame", { xPlayer: 5, oPlayer: "b" }), 400, "SchemaError");
    assertError(yield* app("query", "listGames", { status: "sideways" }), 400, "SchemaError");
    assertError(yield* app("mutation", "createGame", { xPlayer: "a", oPlayer: "a" }), 400, "RuleError");
    assertError(
      yield* app("mutation", "makeMove", { gameId: "missing", playerId: "a", cell: 0, expectedVersion: 1 }),
      404,
      "NotFoundError",
    );
  }),
);

httpTest(
  "a full writer queue → 503 QueueFullError (was a 400 before the port)",
  Effect.gen(function* () {
    const stack = yield* (yield* AppServices).get(COLUMNS.id);
    const gate = yield* Gate;
    const entered = yield* Deferred.make<void>();
    const release = yield* Deferred.make<void>();
    yield* Ref.set(gate.armed, Option.some({ entered, release }));

    // One snapshot holds the writer; 999 mutations queue behind it — 1000 admitted, the cap.
    const dir = yield* Effect.acquireRelease(
      Effect.promise(() => Deno.makeTempDir({ prefix: "mansio-http-queue-" })),
      (path) => Effect.promise(() => Deno.remove(path, { recursive: true })),
    );
    const holder = yield* Effect.fork(stack.storage.snapshot(join(dir, "held.sqlite")));
    yield* Deferred.await(entered);
    const queued = yield* Effect.forEach(
      Array.from({ length: 999 }, (_, i) => i),
      (i) => Effect.fork(stack.storage.mutation(functions.createGame, { xPlayer: `x${i}`, oPlayer: "o" })),
    );
    let sent = yield* app("mutation", "createGame", { xPlayer: "late", oPlayer: "o" });
    for (let attempt = 0; sent.status !== 503 && attempt < 20; attempt++) {
      yield* Effect.yieldNow();
      sent = yield* app("mutation", "createGame", { xPlayer: "late", oPlayer: "o" });
    }
    assertError(sent, 503, "QueueFullError");

    yield* Deferred.succeed(release, undefined);
    yield* Fiber.join(holder);
    for (const fiber of queued) yield* Fiber.join(fiber);
    // Once drained, the queue admits again.
    assert.equal((yield* app("mutation", "createGame", { xPlayer: "after", oPlayer: "o" })).status, 200);
  }),
);

httpTest(
  "a 5xx names the tag and trace but not the detail; the detail goes to the http event",
  Effect.gen(function* () {
    const sent = yield* send("POST", "/admin/logs/ingest", {
      token: ADMIN,
      headers: { "x-trace-id": "t-500" },
    });
    assertError(sent, 500, "IngestError");
    assert.ok(!sent.body.error!.message.includes("/srv/secret"), sent.body.error!.message);
    assert.match(sent.body.error!.message, /t-500/);
    const event = yield* lastHttpEvent;
    assert.equal(event.errorTag, "IngestError");
    assert.equal((event.errorDetail as { detail?: string }).detail, "/srv/secret/logs unreadable");
    assert.equal(event.level, "error");
  }),
);

test("FFI-05: a statement deadline → 504 with a message naming the deadline; the reader call timeout stays 502", () => {
  const deadline = new StatementDeadlineError({
    deadlineMs: 5000,
    elapsedMs: 5000.6,
    table: "games",
    statementKind: "count",
  });
  assert.equal(toHttpStatus(deadline), 504);
  assert.equal(
    errorMessage(deadline, 504, "t-504"),
    "Statement deadline exceeded: count on games was stopped after 5000 ms; see the server log for trace t-504",
  );
  const callTimeout = new IpcError({ reason: "timeout", detail: "Document query deadline exceeded" });
  assert.equal(toHttpStatus(callTimeout), 502);
  assert.equal(errorMessage(callTimeout, 502, "t-502"), "IpcError; see the server log for trace t-502");
});

httpTest(
  "a reader that exited, timed out or broke the protocol → 502 per call; the writer is unaffected",
  Effect.gen(function* () {
    const fault = yield* ReaderFault;
    yield* Ref.set(fault, Option.some<CallError>(new ReaderExitedError({ code: 137, signal: "SIGKILL" })));
    assertError(yield* send("GET", "/health"), 502, "ReaderExitedError");
    assertError(yield* app("query", "getGame", { gameId: "g" }), 502, "ReaderExitedError");

    yield* Ref.set(
      fault,
      Option.some<CallError>(new IpcError({ reason: "timeout", detail: "no reply to query after 30s" })),
    );
    const timedOut = yield* send("POST", `/apps/${COLUMNS.id}/query/getGame`, {
      token: COLUMNS.token,
      body: { gameId: "g" },
      headers: { "x-trace-id": "t-502" },
    });
    assertError(timedOut, 502, "IpcError");
    assert.ok(!timedOut.body.error!.message.includes("no reply"), timedOut.body.error!.message);
    const event = yield* lastHttpEvent;
    assert.equal(event.traceId, "t-502");
    assert.equal(event.errorTag, "IpcError");
    assert.equal((event.errorDetail as { reason?: string }).reason, "timeout");
    assert.equal(event.level, "error");

    yield* Ref.set(fault, Option.some<CallError>(new IpcError({ reason: "malformed-frame", detail: "{" })));
    assertError(yield* send("POST", "/admin/logs/ingest", { token: ADMIN }), 502, "IpcError");

    // The writer is in this process: a mutation still commits while reader calls fail. (A real child
    // exit also stops the whole server — `platform/server.ts`, `tests/unit/server_scope_test.ts`.)
    const created = yield* app("mutation", "createGame", { xPlayer: "a", oPlayer: "b" });
    assert.equal(created.status, 200, JSON.stringify(created.body));
  }),
);

httpTest(
  "a handler defect → 500 Defect with the trace id; the cause is logged under that trace",
  Effect.gen(function* () {
    const sent = yield* send("POST", "/admin/logs/analysis/boom", {
      token: ADMIN,
      headers: { "x-trace-id": "t-defect" },
    });
    assertError(sent, 500, "Defect");
    assert.match(sent.body.error!.message, /t-defect/);
    assert.ok(!sent.body.error!.message.includes("blew up"), sent.body.error!.message);
    const event = yield* lastHttpEvent;
    assert.equal(event.status, 500);
    assert.equal(event.errorTag, "Defect");
    assert.equal(event.traceId, "t-defect");
  }),
);

// ===========================================================================
// Admin routes
// ===========================================================================

httpTest(
  "POST /admin/backup/:appId takes a backup under the request's trace id",
  Effect.gen(function* () {
    yield* app("mutation", "createGame", { xPlayer: "a", oPlayer: "b" });
    const sent = yield* send("POST", `/admin/backup/${COLUMNS.id}`, {
      token: ADMIN,
      headers: { "x-trace-id": "backup-trace-1" },
    });
    assert.equal(sent.status, 200, JSON.stringify(sent.body));
    const manifest = sent.body.result as { appId: string; key: string };
    assert.equal(manifest.appId, COLUMNS.id);
    // The backup's own events carry the request's trace id: `Effect.annotateLogs` reached them.
    const phases = yield* eventsOfKind("backup");
    assert.ok(phases.length > 0);
    for (const event of phases) assert.equal(event.traceId, "backup-trace-1");
    assertError(yield* send("POST", "/admin/backup/nope", { token: ADMIN }), 404, "UnknownAppError");
  }),
);

httpTest(
  "POST /admin/logs/analysis/:name passes a catalog name and scalar params, never SQL",
  Effect.gen(function* () {
    const ok = yield* send("POST", "/admin/logs/analysis/errorsByTag", { token: ADMIN, body: { days: 7 } });
    assert.equal(ok.status, 200, JSON.stringify(ok.body));
    assert.deepEqual((yield* Ref.get(yield* ReaderCalls)).at(-1), {
      kind: "analysis",
      name: "errorsByTag",
      params: { days: 7 },
    });
    assertError(
      yield* send("POST", "/admin/logs/analysis/nope", { token: ADMIN }),
      404,
      "UnknownAnalysisQuery",
    );
    assertError(
      yield* send("POST", "/admin/logs/analysis/errorsByTag", { token: ADMIN, body: { where: { or: 1 } } }),
      400,
      "SchemaError",
    );
    assertError(
      yield* send("POST", "/admin/logs/analysis/errorsByTag", { token: ADMIN, body: ["SELECT 1"] }),
      400,
      "SchemaError",
    );
  }),
);

// ===========================================================================
// Tracing
// ===========================================================================

httpTest(
  "X-Trace-Id is accepted or generated, echoed, and stamped on one http event per request",
  Effect.gen(function* () {
    const supplied = yield* send("POST", `/apps/${COLUMNS.id}/mutation/createGame`, {
      token: COLUMNS.token,
      body: { xPlayer: "a", oPlayer: "b" },
      headers: { "x-trace-id": "caller-trace-42" },
    });
    assert.equal(supplied.traceId, "caller-trace-42");
    const event: HttpEventRecord = yield* lastHttpEvent;
    assert.equal(event.traceId, "caller-trace-42");
    assert.equal(event.appId, COLUMNS.id);
    assert.equal(event.method, "POST");
    assert.equal(event.path, `/apps/${COLUMNS.id}/mutation/createGame`);
    assert.equal(event.handlerKind, "mutation");
    assert.equal(event.name, "createGame");
    assert.equal(event.status, 200);
    assert.equal(event.bytesIn, JSON.stringify({ xPlayer: "a", oPlayer: "b" }).length);
    assert.equal(event.bytesOut, new TextEncoder().encode(JSON.stringify(supplied.body)).length);
    assert.equal(event.level, "info");

    // Absent or implausible ids are replaced by a fresh one from IdGen.
    const generated = yield* send("GET", "/health");
    assert.ok(generated.traceId && /^id-\d+$/.test(generated.traceId), String(generated.traceId));
    const junk = yield* send("GET", "/health", { headers: { "x-trace-id": "has spaces\tand tabs" } });
    assert.notEqual(junk.traceId, "has spaces\tand tabs");

    const failed = yield* app("query", "noSuchHandler", {});
    const failedEvent = yield* lastHttpEvent;
    assert.equal(failedEvent.traceId, failed.traceId);
    assert.equal(failedEvent.status, 404);
    assert.equal(failedEvent.errorTag, "UnknownHandlerError");
    assert.equal(failedEvent.level, "warn");
    assert.equal((yield* eventsOfKind("http")).length, 4);
  }),
);

// ===========================================================================
// The server JSONL file
// ===========================================================================

/** All records in the server role's JSONL files under `dir`. */
async function serverJsonl(dir: string): Promise<Record<string, unknown>[]> {
  const records: Record<string, unknown>[] = [];
  const walk = async (at: string): Promise<void> => {
    for await (const entry of Deno.readDir(at)) {
      const path = join(at, entry.name);
      if (entry.isDirectory) await walk(path);
      else if (entry.name.endsWith(".jsonl")) {
        for (const line of (await Deno.readTextFile(path)).split("\n")) {
          if (line !== "") records.push(JSON.parse(line));
        }
      }
    }
  };
  await walk(join(dir, "server"));
  return records;
}

test("the server's LoggingLive writes one http event per request, and defect logs, to its JSONL file", async () => {
  const dir = await Deno.makeTempDir({ prefix: "mansio-http-jsonl-" });
  try {
    const JsonlStack = PlatformTest.pipe(
      Layer.provideMerge(Layer.mergeAll(LoggingLive, IdGenTest, ObjectStoreTest(), UnusedTenantRegistry)),
      Layer.provideMerge(ConfigTest({ ...TEST_CONFIG, logging: { dir, eventFlushMs: 10 } })),
    );
    const statuses = await Effect.runPromise(
      Effect.gen(function* () {
        const headers = (traceId: string) => ({ "x-trace-id": traceId });
        const out = [
          (yield* send("GET", "/health", { headers: headers("jsonl-200") })).status,
          (yield* send("GET", "/nope", { headers: headers("jsonl-404") })).status,
          (yield* send("POST", "/admin/logs/analysis/boom", { token: ADMIN, headers: headers("jsonl-500") }))
            .status,
        ];
        yield* Ref.set(yield* ReaderFault, Option.some<CallError>(new ReaderExitedError({ code: 1 })));
        out.push((yield* send("GET", "/health", { headers: headers("jsonl-502") })).status);
        return out;
      }).pipe(Effect.scoped, Effect.provide(JsonlStack)),
    );
    assert.deepEqual(statuses, [200, 404, 500, 502]);

    // The scope has closed, so the sink has drained and closed the file.
    const records = await serverJsonl(dir);
    const http = records.filter((record) => record.kind === "http");
    assert.deepEqual(
      http.map((record) => [record.traceId, record.status, record.role]),
      [
        ["jsonl-200", 200, "server"],
        ["jsonl-404", 404, "server"],
        ["jsonl-500", 500, "server"],
        ["jsonl-502", 502, "server"],
      ],
    );
    assert.deepEqual(http.map((record) => record.errorTag ?? null), [
      null,
      "RouteNotFound",
      "Defect",
      "ReaderExitedError",
    ]);
    const defect = records.find((record) =>
      record.kind === "lifecycle" &&
      (record.detail as { message?: string } | undefined)?.message === "request handler defect"
    );
    assert.ok(defect, "the defect's Effect.logError line is in the file");
    assert.equal(defect.traceId, "jsonl-500");
    assert.equal(defect.level, "error");
    assert.match(String((defect.detail as { cause?: unknown }).cause), /analysis handler blew up/);
  } finally {
    await Deno.remove(dir, { recursive: true });
  }
});
