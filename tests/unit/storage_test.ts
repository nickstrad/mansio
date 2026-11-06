/**
 * Unit tests for the `Storage` service (plan item 15).
 *
 * These run the real `StorageLive` over a real SQLite file in a temp directory: the point is
 * the transaction and queue semantics, which only the real driver can show. Side connections on the
 * live file go through `tests/helpers/sqlite_inspect.ts`, the server's own FFI binding. Every dependency the
 * layer takes is a local double — `EventSink` collects into an array, `IdGen` counts `id-1`,
 * `id-2`, …, `Config` is `ConfigTest` — so each test can assert on the events it caused.
 *
 * The two behaviours the pre-port writer did not have get the most attention: an interrupted
 * mutation fiber rolls its transaction back and leaves the connection usable, and the writer queue
 * rejects job 1001 with a typed `QueueFullError` instead of an untyped rejection.
 *
 * The last section (plan item 31) shows where the U+0000 text storage contract is enforced, from the
 * events: before the statement on writes, and by a second, guarded select on reads.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  Cause,
  Deferred,
  Duration,
  Effect,
  Exit,
  Fiber,
  FiberId,
  Layer,
  Scope,
  TestClock,
  TestContext,
} from "effect";
import type { EventRecord, QueueEventRecord, SqlEventRecord, TxEventRecord } from "../../core/events.ts";
import { type StatementDeadlineError, toHttpStatus } from "../../core/errors.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { withStorage } from "../../core/schema.ts";
import { ConfigTest } from "../../platform/config.ts";
import {
  Config,
  EventSink,
  IdGen,
  type LoggingConfig,
  Storage,
  type StorageService,
} from "../../platform/services.ts";
import {
  ConnectionGate,
  makeStorage,
  POISONED_DETAIL,
  StorageLive,
  WRITER_QUEUE_CAP,
} from "../../platform/sqlite.ts";
import { allAsync, Database, isSqliteFfiError, prepareCached } from "../../platform/sqlite_ffi/mod.ts";
import { defineFunctions } from "../../sdk/functions.ts";
import { defineSchema, defineTable, v } from "../../sdk/schema.ts";
import { schema } from "../../app/schema.ts";
import { functions } from "../../app/functions.ts";
import { inspect as inspectFile } from "../helpers/sqlite_inspect.ts";

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

interface Harness {
  readonly file: string;
  readonly events: EventRecord[];
  readonly deps: Layer.Layer<EventSink | IdGen | Config>;
}

function harness(logging: Partial<LoggingConfig> = {}): Harness & { cleanup: () => void } {
  const dir = mkdtempSync(join(tmpdir(), "mansio-storage-"));
  const events: EventRecord[] = [];
  let id = 0;
  const deps = Layer.mergeAll(
    Layer.succeed(EventSink, { emit: (event) => Effect.sync(() => void events.push(event)) }),
    Layer.succeed(IdGen, { next: Effect.sync(() => `id-${++id}`) }),
    ConfigTest({ logging }),
  );
  return {
    file: join(dir, "app.sqlite"),
    events,
    deps,
    cleanup: () => rmSync(dir, { recursive: true, force: true }),
  };
}

/** Run `program` against a fresh `StorageLive` over the app schema; the layer's scope closes after. */
async function withStorageLayer<A, E>(
  program: (storage: StorageService, h: Harness) => Effect.Effect<A, E>,
  logging: Partial<LoggingConfig> = {},
): Promise<A> {
  const h = harness(logging);
  try {
    return await Effect.runPromise(
      Effect.flatMap(Storage, (storage) => program(storage, h)).pipe(
        Effect.provide(StorageLive({ file: h.file, schema, appId: "test-app" }).pipe(Layer.provide(h.deps))),
      ),
    );
  } finally {
    h.cleanup();
  }
}

const app = defineFunctions(schema);

const inspect = app.mutation({
  args: {},
  handler: ({ db }) =>
    Effect.gen(function* () {
      return {
        games: yield* db.find("games").limit(1000).collect(),
        moves: yield* db.find("moves").limit(1000).collect(),
      };
    }),
});

const ofKind = <K extends EventRecord["kind"]>(events: EventRecord[], kind: K) =>
  events.filter((e): e is Extract<EventRecord, { kind: K }> => e.kind === kind);

/**
 * A gate for blocking handlers. Callers open it in `Effect.ensuring`, so a failed assertion cannot
 * leave a handler parked forever — closing the layer waits for in-flight work, and would hang.
 */
const openedOnExit = () => Deferred.unsafeMake<void>(FiberId.none);

const failureOf = <A, E>(exit: Exit.Exit<A, E>): unknown => {
  assert.ok(Exit.isFailure(exit), "expected a failure");
  return Cause.squash(exit.cause);
};

// ---------------------------------------------------------------------------
// Interruption: the behaviour the pre-port try/catch did not have
// ---------------------------------------------------------------------------

test("an interrupted mutation fiber rolls back and leaves the connection usable", async () => {
  await withStorageLayer((storage, h) =>
    Effect.gen(function* () {
      const { gameId } = (yield* storage.mutation(functions.createGame, { xPlayer: "a", oPlayer: "b" })) as {
        gameId: string;
      };
      const wrote = yield* Deferred.make<void>();
      const hang = app.mutation({
        args: { gameId: v.string() },
        handler: ({ db }, { gameId }) =>
          Effect.gen(function* () {
            yield* db.patch("games", gameId, {
              board: ["X", null, null, null, null, null, null, null, null],
            });
            yield* db.insert("moves", { gameId, playerId: "a", mark: "X", cell: 0, moveNumber: 1 });
            yield* Deferred.succeed(wrote, undefined);
            return yield* Effect.never;
          }),
      });
      const fiber = yield* Effect.fork(storage.mutation(hang, { gameId }));
      yield* Deferred.await(wrote);
      const exit = yield* Fiber.interrupt(fiber);
      assert.ok(Exit.isInterrupted(exit), "the mutation fiber ended interrupted");

      // Rolled back: neither the patch nor the insert is visible.
      const state = (yield* storage.mutation(inspect, {})) as {
        games: { board: unknown[]; _version: number }[];
        moves: unknown[];
      };
      assert.deepEqual(state.games[0].board, Array(9).fill(null));
      assert.equal(state.games[0]._version, 1);
      assert.equal(state.moves.length, 0);

      // Still usable: a real write commits, and the writer slot was released.
      const move = (yield* storage.mutation(functions.makeMove, {
        gameId,
        playerId: "a",
        cell: 4,
        expectedVersion: 1,
      })) as { _version: number; board: unknown[] };
      assert.equal(move._version, 2);
      assert.equal(move.board[4], "X");

      const outcomes = ofKind(h.events, "tx").map((e: TxEventRecord) => e.outcome);
      assert.deepEqual(outcomes, ["commit", "commit", "interrupted", "commit", "commit"]);
      const interrupted = ofKind(h.events, "tx").find((e) => e.outcome === "interrupted")!;
      assert.equal(interrupted.operations, 2, "the patch and the insert inside the interrupted handler");
      assert.equal(interrupted.level, "warn");
    })
  );
});

// ---------------------------------------------------------------------------
// The writer queue
// ---------------------------------------------------------------------------

test(`the writer queue rejects job ${WRITER_QUEUE_CAP + 1} with a typed QueueFullError`, async () => {
  const gate = openedOnExit();
  await withStorageLayer((storage, h) =>
    Effect.gen(function* () {
      const blocked = app.mutation({ args: {}, handler: () => Deferred.await(gate) });
      // One job runs (and blocks on the gate); the remaining 999 wait for the permit.
      const fibers = yield* Effect.forEach(
        Array.from({ length: WRITER_QUEUE_CAP }),
        () => Effect.fork(storage.mutation(blocked, {})),
      );
      yield* Effect.sleep(Duration.millis(50)); // let every forked fiber enter the queue

      const rejected = failureOf(yield* Effect.exit(storage.mutation(blocked, {})));
      assert.equal((rejected as { _tag: string })._tag, "QueueFullError");
      assert.deepEqual(
        { ...(rejected as object) },
        { _tag: "QueueFullError", queue: "writer", depth: WRITER_QUEUE_CAP, cap: WRITER_QUEUE_CAP },
      );
      // A snapshot shares the queue; its channel is `StorageError`, with the queue failure as cause.
      const snapshot = failureOf(yield* Effect.exit(storage.snapshot(join(h.file, "..", "snap.sqlite"))));
      assert.equal((snapshot as { _tag: string })._tag, "StorageError");
      assert.equal((snapshot as { cause: { _tag: string } }).cause._tag, "QueueFullError");

      const queue = ofKind(h.events, "queue");
      assert.equal(queue.length, 2);
      assert.deepEqual(
        queue.map((e: QueueEventRecord) => [e.queue, e.event, e.depth, e.cap, e.level, e.appId]),
        [
          ["writer", "rejected", WRITER_QUEUE_CAP, WRITER_QUEUE_CAP, "warn", "test-app"],
          ["writer", "rejected", WRITER_QUEUE_CAP, WRITER_QUEUE_CAP, "warn", "test-app"],
        ],
      );

      // Open the gate: all 1000 admitted jobs commit, and the queue admits work again.
      yield* Deferred.succeed(gate, undefined);
      const exits = yield* Effect.forEach(fibers, Fiber.await);
      assert.ok(exits.every(Exit.isSuccess));
      yield* storage.mutation(functions.createGame, { xPlayer: "a", oPlayer: "b" });
    }).pipe(Effect.ensuring(Deferred.succeed(gate, undefined)))
  );
});

test("an interrupted waiter gives its queue slot back", async () => {
  const gate = openedOnExit();
  await withStorageLayer((storage) =>
    Effect.gen(function* () {
      const blocked = app.mutation({ args: {}, handler: () => Deferred.await(gate) });
      const head = yield* Effect.fork(storage.mutation(blocked, {}));
      const waiters = yield* Effect.forEach(
        Array.from({ length: WRITER_QUEUE_CAP - 1 }),
        () => Effect.fork(storage.mutation(blocked, {})),
      );
      yield* Effect.sleep(Duration.millis(50));
      // Full: interrupting one waiter must free exactly one slot.
      yield* Fiber.interrupt(waiters[0]);
      const admitted = yield* Effect.fork(storage.mutation(blocked, {}));
      yield* Effect.sleep(Duration.millis(10));
      const rejected = failureOf(yield* Effect.exit(storage.mutation(blocked, {})));
      assert.equal((rejected as { _tag: string })._tag, "QueueFullError");
      yield* Deferred.succeed(gate, undefined);
      assert.ok(Exit.isSuccess(yield* Fiber.await(head)));
      assert.ok(Exit.isSuccess(yield* Fiber.await(admitted)));
      yield* Effect.forEach(waiters.slice(1), Fiber.join);
    }).pipe(Effect.ensuring(Deferred.succeed(gate, undefined)))
  );
});

// ---------------------------------------------------------------------------
// Transactions
// ---------------------------------------------------------------------------

test("a defect in the handler rolls back like a failure", async () => {
  await withStorageLayer((storage, h) =>
    Effect.gen(function* () {
      const crash = app.mutation({
        args: {},
        handler: ({ db }) =>
          Effect.gen(function* () {
            yield* db.insert("games", {
              xPlayer: "a",
              oPlayer: "b",
              board: Array(9).fill(null),
              turn: "X",
              status: "playing",
              winner: null,
            });
            return yield* Effect.sync(() => {
              throw new Error("handler bug");
            });
          }),
      });
      const failure = failureOf(yield* Effect.exit(storage.mutation(crash, {})));
      assert.match((failure as Error).message, /handler bug/);
      const state = (yield* storage.mutation(inspect, {})) as { games: unknown[] };
      assert.equal(state.games.length, 0);
      const rollback = ofKind(h.events, "tx").find((e) => e.outcome === "rollback")!;
      assert.equal(rollback.errorTag, "Defect");
    })
  );
});

test("a non-JSON result fails NonSerializableResultError before COMMIT", async () => {
  await withStorageLayer((storage) =>
    Effect.gen(function* () {
      const bad = app.mutation({
        args: {},
        handler: ({ db }) =>
          Effect.gen(function* () {
            yield* db.insert("games", {
              xPlayer: "a",
              oPlayer: "b",
              board: Array(9).fill(null),
              turn: "X",
              status: "playing",
              winner: null,
            });
            return { n: 1n };
          }),
      });
      const failure = failureOf(yield* Effect.exit(storage.mutation(bad, {})));
      assert.equal((failure as { _tag: string })._tag, "NonSerializableResultError");
      assert.equal((failure as { appId: string }).appId, "test-app");
      const state = (yield* storage.mutation(inspect, {})) as { games: unknown[] };
      assert.equal(state.games.length, 0, "the insert was rolled back");
    })
  );
});

test("a Documents handle used after its transaction fails ContextExpiredError", async () => {
  await withStorageLayer((storage) =>
    Effect.gen(function* () {
      let leaked: { find: (t: "games") => { count: () => Effect.Effect<number, unknown> } } | undefined;
      const leak = app.mutation({
        args: {},
        handler: ({ db }) =>
          Effect.sync(() => {
            leaked = db;
            return null;
          }),
      });
      yield* storage.mutation(leak, {});
      const failure = failureOf(yield* Effect.exit(leaked!.find("games").count()));
      assert.equal((failure as { _tag: string })._tag, "ContextExpiredError");
      assert.equal((failure as { context: string }).context, "mutation");
    })
  );
});

test("a forked write switched out mid-operation cannot land after COMMIT", async () => {
  await withStorageLayer((storage, h) =>
    Effect.gen(function* () {
      let expired = 0;
      // The yield threshold decides where the forked patch is switched out: before its select,
      // between the select and the update, or between COMMIT and the release. Sweep them all.
      for (let threshold = 10; threshold <= 60; threshold++) {
        const { gameId } =
          (yield* storage.mutation(functions.createGame, { xPlayer: "a", oPlayer: "b" })) as {
            gameId: string;
          };
        let forked: Fiber.Fiber<unknown, unknown> | undefined;
        const leak = app.mutation({
          args: { gameId: v.string() },
          handler: ({ db }, { gameId }) =>
            Effect.gen(function* () {
              forked = yield* Effect.forkDaemon(db.patch("games", gameId, { turn: "O" }));
              return null;
            }),
        });
        const from = h.events.length;
        yield* storage.mutation(leak, { gameId }).pipe(Effect.withMaxOpsBeforeYield(threshold));
        const exit = yield* Fiber.await(forked!);
        const events = h.events.slice(from);
        const commit = events.findIndex((e) => e.kind === "tx");
        const update = events.findIndex((e) => e.kind === "sql" && e.statementKind === "update");
        const games =
          ((yield* storage.mutation(inspect, {})) as { games: { _id: string; turn: string }[] }).games;
        const turn = games.find((g) => g._id === gameId)!.turn;
        if (Exit.isSuccess(exit)) {
          // Only acceptable if the whole patch ran inside the transaction.
          assert.ok(update !== -1 && update < commit, `threshold ${threshold}: update ran after COMMIT`);
          assert.equal(turn, "O");
        } else {
          expired++;
          assert.equal(
            (failureOf(exit) as { _tag: string })._tag,
            "ContextExpiredError",
            `threshold ${threshold}`,
          );
          assert.equal(update, -1, `threshold ${threshold}: no update statement ran`);
          assert.equal(turn, "X", `threshold ${threshold}: the patch did not persist`);
        }
      }
      assert.ok(expired > 0, "some threshold switched the forked patch out past COMMIT");
    })
  );
});

test("typed handler failures keep their tag and roll back", async () => {
  await withStorageLayer((storage) =>
    Effect.gen(function* () {
      const missing = failureOf(
        yield* Effect.exit(storage.mutation(functions.makeMove, {
          gameId: "nope",
          playerId: "a",
          cell: 0,
          expectedVersion: 1,
        })),
      );
      assert.equal((missing as { _tag: string })._tag, "NotFoundError");
      const schemaFailure = failureOf(
        yield* Effect.exit(storage.mutation(functions.createGame, { xPlayer: "a", oPlayer: "b", extra: 1 })),
      );
      assert.equal((schemaFailure as { _tag: string })._tag, "SchemaError");
      const patchMissing = app.mutation({
        args: {},
        handler: ({ db }) => db.patch("games", "absent", { status: "won" }),
      });
      const notFound = failureOf(yield* Effect.exit(storage.mutation(patchMissing, {})));
      assert.deepEqual({ ...(notFound as object) }, { _tag: "NotFoundError", table: "games", id: "absent" });
    })
  );
});

// ---------------------------------------------------------------------------
// Lifetime
// ---------------------------------------------------------------------------

test("schema drift fails the layer with SchemaDriftError and closes the handle", async () => {
  const h = harness();
  try {
    const build = (s: typeof schema) =>
      Effect.runPromiseExit(
        Effect.scoped(Layer.build(StorageLive({ file: h.file, schema: s }).pipe(Layer.provide(h.deps)))),
      );
    assert.ok(Exit.isSuccess(await build(schema)));
    const drift = failureOf(await build(withStorage(schema, "json")));
    assert.equal((drift as { _tag: string })._tag, "SchemaDriftError");
    assert.match((drift as { detail: string }).detail, /refuses automatic migration/);
    // The failed acquire rolled back and closed: another connection can take the write lock now.
    const db = inspectFile(h.file, { write: true });
    try {
      db.exec("PRAGMA busy_timeout=0; BEGIN IMMEDIATE; ROLLBACK");
      assert.equal(
        db.get('SELECT definition FROM "_platform_schema"')!.definition,
        JSON.stringify(schema),
      );
    } finally {
      db.close();
    }
    const tx = ofKind(h.events, "tx");
    assert.deepEqual(tx.map((e) => e.outcome), ["commit", "rollback"]);
    assert.equal(tx[1].errorTag, "SchemaDriftError");
  } finally {
    h.cleanup();
  }
});

test("closing the scope waits for in-flight work, then refuses new work", async () => {
  const h = harness();
  const gate = openedOnExit();
  try {
    await Effect.runPromise(
      Effect.gen(function* () {
        const scope = yield* Scope.make();
        const storage = yield* makeStorage({ file: h.file, schema }).pipe(
          Scope.extend(scope),
          Effect.provide(h.deps),
        );
        const slow = app.mutation({
          args: {},
          handler: ({ db }) =>
            Effect.gen(function* () {
              yield* Deferred.await(gate);
              return yield* db.insert("games", {
                xPlayer: "a",
                oPlayer: "b",
                board: Array(9).fill(null),
                turn: "X",
                status: "playing",
                winner: null,
              });
            }),
        });
        const inFlight = yield* Effect.fork(storage.mutation(slow, {}));
        yield* Effect.sleep(Duration.millis(10));
        const closing = yield* Effect.fork(Scope.close(scope, Exit.void));
        yield* Effect.sleep(Duration.millis(10));
        assert.equal(closing.unsafePoll(), null, "close is waiting for the in-flight mutation");
        const refused = failureOf(yield* Effect.exit(storage.mutation(inspect, {})));
        assert.deepEqual(
          [(refused as { _tag: string })._tag, (refused as { operation: string }).operation],
          ["StorageError", "close"],
        );
        yield* Deferred.succeed(gate, undefined);
        assert.equal(yield* Fiber.join(inFlight), "id-1");
        yield* Fiber.join(closing);
      }).pipe(Effect.ensuring(Deferred.succeed(gate, undefined))),
    );
    const db = inspectFile(h.file);
    try {
      assert.equal(
        db.get("SELECT COUNT(*) AS n FROM games")!.n,
        1,
        "the drained mutation committed",
      );
    } finally {
      db.close();
    }
  } finally {
    h.cleanup();
  }
});

// ---------------------------------------------------------------------------
// Clock and IdGen come from services
// ---------------------------------------------------------------------------

test("_creationTime comes from Clock (TestClock) and _id from IdGen", async () => {
  const h = harness();
  try {
    const game = await Effect.runPromise(
      Effect.gen(function* () {
        yield* TestClock.setTime(1_700_000_000_000);
        const storage = yield* Storage;
        const { gameId } =
          (yield* storage.mutation(functions.createGame, { xPlayer: "a", oPlayer: "b" })) as {
            gameId: string;
          };
        const state = (yield* storage.mutation(inspect, {})) as {
          games: { _id: string; _creationTime: number }[];
        };
        assert.equal(gameId, "id-1");
        return state.games[0];
      }).pipe(
        Effect.provide(StorageLive({ file: h.file, schema }).pipe(Layer.provide(h.deps))),
        Effect.provide(TestContext.TestContext),
      ),
    );
    assert.deepEqual([game._id, game._creationTime], ["id-1", 1_700_000_000_000]);
    assert.equal(ofKind(h.events, "tx").at(-1)!.ts, new Date(1_700_000_000_000).toISOString());
  } finally {
    h.cleanup();
  }
});

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

test("each statement emits a sql event; each transaction a tx event", async () => {
  await withStorageLayer((storage, h) =>
    Effect.gen(function* () {
      const startup = ofKind(h.events, "sql").map((e) => e.statementKind);
      assert.deepEqual(startup.slice(0, 3), ["pragma", "pragma", "pragma"]);
      assert.ok(startup.includes("ddl"));
      h.events.length = 0;

      yield* storage.mutation(functions.createGame, { xPlayer: "a", oPlayer: "b" }).pipe(
        Effect.annotateLogs({ traceId: "trace-123" }),
      );
      const sql = ofKind(h.events, "sql");
      assert.equal(sql.length, 1);
      const insert: SqlEventRecord = sql[0];
      assert.equal(insert.engine, "sqlite");
      assert.equal(insert.statementKind, "insert");
      assert.equal(insert.tableName, "games");
      assert.equal(insert.rowCount, 1);
      assert.equal(insert.outcome, "ok");
      assert.match(insert.sqlText, /^INSERT INTO "games"/);
      assert.ok(insert.bindCount > 0);
      assert.equal(insert.bindValues, undefined, "bind values are off by default");
      assert.equal(insert.level, "debug", "SQL tracing is opt-in");
      assert.equal(insert.traceId, "trace-123");
      assert.equal(insert.appId, "test-app");
      assert.equal(typeof insert.durationMs, "number");

      const [tx] = ofKind(h.events, "tx");
      assert.deepEqual(
        [tx.outcome, tx.operations, tx.engine, tx.traceId, tx.level],
        ["commit", 1, "sqlite", "trace-123", "info"],
      );
      assert.ok(tx.queueWaitMs >= 0 && tx.durationMs >= 0);
    })
  );
});

test("LOG_INCLUDE_BIND_VALUES and TRACE_SQL change what a sql event carries", async () => {
  await withStorageLayer(
    (storage, h) =>
      Effect.gen(function* () {
        h.events.length = 0;
        yield* storage.mutation(functions.createGame, { xPlayer: "alice", oPlayer: "bob" });
        const [insert] = ofKind(h.events, "sql");
        assert.ok(insert.bindValues?.includes("alice"));
        assert.equal(insert.level, "info");
      }),
    { includeBindValues: true, traceSql: true },
  );
});

test("a failing statement emits an error sql event and a StorageError", async () => {
  await withStorageLayer((storage, h) =>
    Effect.gen(function* () {
      // Corrupt the stored schema under the layer's feet so the next insert hits a driver error.
      const side = inspectFile(h.file, { write: true });
      side.exec('DROP TABLE "moves"');
      side.close();
      h.events.length = 0;
      const insertMove = app.mutation({
        args: {},
        handler: ({ db }) =>
          db.insert("moves", { gameId: "g", playerId: "a", mark: "X", cell: 0, moveNumber: 1 }),
      });
      const failure = failureOf(yield* Effect.exit(storage.mutation(insertMove, {})));
      assert.deepEqual(
        [(failure as { _tag: string })._tag, (failure as { operation: string }).operation],
        ["StorageError", "insert"],
      );
      const [sql] = ofKind(h.events, "sql");
      assert.deepEqual([sql.outcome, sql.errorTag, sql.level], ["error", "StorageError", "warn"]);
      assert.equal(ofKind(h.events, "tx")[0].outcome, "rollback");
    })
  );
});

// ---------------------------------------------------------------------------
// The text storage contract (plan item 31, docs/adr/0005-text-storage-contract.md)
//
// `tests/unit/nul_contract_test.ts` covers the contract on all four backends; these two tests read
// the live layer's events to show *where* it is enforced.
// ---------------------------------------------------------------------------

const newGame = (xPlayer: string) => ({
  xPlayer,
  oPlayer: "b",
  board: Array(9).fill(null),
  turn: "X" as const,
  status: "playing" as const,
  winner: null,
});

test("a U+0000 rejection runs no statement for the rejected write and rolls back the earlier ones", async () => {
  await withStorageLayer((storage, h) =>
    Effect.gen(function* () {
      h.events.length = 0;
      const twoInserts = app.mutation({
        args: {},
        handler: ({ db }) =>
          Effect.gen(function* () {
            yield* db.insert("games", newGame("a"));
            return yield* db.insert("games", newGame("a\u0000b"));
          }),
      });
      const failure = failureOf(yield* Effect.exit(storage.mutation(twoInserts, {})));
      assert.deepEqual(
        { ...(failure as object) },
        {
          _tag: "SchemaError",
          reason: "type-mismatch",
          path: "xPlayer",
          expected: "text without U+0000",
          actual: "a\u0000b",
        },
      );
      // Only the first insert reached SQLite; the transaction then rolled it back.
      assert.deepEqual(ofKind(h.events, "sql").map((e) => e.statementKind), ["insert"]);
      const [tx] = ofKind(h.events, "tx");
      assert.deepEqual([tx.outcome, tx.operations, tx.errorTag], ["rollback", 2, "SchemaError"]);
      const state = (yield* storage.mutation(inspect, {})) as { games: unknown[] };
      assert.equal(state.games.length, 0);
    })
  );
});

test("a read of pre-existing U+0000 text fails StorageError via a second, guarded select", async () => {
  await withStorageLayer((storage, h) =>
    Effect.gen(function* () {
      const { gameId } = (yield* storage.mutation(functions.createGame, { xPlayer: "a", oPlayer: "b" })) as {
        gameId: string;
      };
      // What a pre-contract build could have committed: SQLite holds all three bytes.
      const side = inspectFile(h.file, { write: true });
      side.run('UPDATE "games" SET "xPlayer" = ? WHERE "_id" = ?', ["a\u0000b", gameId]);
      side.close();
      const read = (definition: Parameters<StorageService["mutation"]>[0]) =>
        Effect.gen(function* () {
          h.events.length = 0;
          const exit = yield* Effect.exit(storage.mutation(definition, {}));
          return { exit, sql: ofKind(h.events, "sql") };
        });

      const get = app.mutation({ args: {}, handler: ({ db }) => db.get("games", gameId) });
      const { exit, sql } = yield* read(get);
      assert.deepEqual({ ...(failureOf(exit) as object) }, {
        _tag: "StorageError",
        operation: "select",
        table: "games",
        detail: 'Stored text in "xPlayer" contains U+0000, which the text storage contract rejects',
      });
      assert.deepEqual(sql.map((e) => [e.statementKind, e.outcome]), [["select", "ok"], ["select", "ok"]]);
      assert.match(sql[1].sqlText, /instr\(q\."xPlayer", char\(0\)\) > 0/);
      assert.equal(sql[1].rowCount, 1, "the probe found the stored NUL");
      assert.equal(ofKind(h.events, "tx")[0].errorTag, "StorageError");

      // No rows, or no text in the result: no probe.
      const none = app.mutation({ args: {}, handler: ({ db }) => db.get("games", "absent") });
      assert.deepEqual((yield* read(none)).sql.length, 1);
      const count = app.mutation({ args: {}, handler: ({ db }) => db.find("games").count() });
      const counted = yield* read(count);
      assert.ok(Exit.isSuccess(counted.exit) && counted.exit.value === 1);
      assert.equal(counted.sql.length, 1);
    })
  );
});

// ---------------------------------------------------------------------------
// sq-06 regression: the writer queue's Semaphore(1) exclusivity
// ---------------------------------------------------------------------------

test("sq-06: 50 concurrent read-modify-write increments commit 50/50 with zero lost updates", async () => {
  const counterSchema = defineSchema({
    counters: defineTable({ name: v.string(), value: v.integer() }),
  });
  const counterApp = defineFunctions(counterSchema);
  const seed = counterApp.mutation({
    args: {},
    handler: ({ db }) => db.insert("counters", { name: "hits", value: 0 }),
  });
  const read = counterApp.mutation({
    args: { id: v.string() },
    handler: ({ db }, { id }) => db.get("counters", id),
  });
  // Read-modify-write inside one mutation. The yieldNow between read and write is the widest
  // window a handler has: without the writer queue's Semaphore(1) serialization these 50
  // concurrent increments would race and lose updates.
  const increment = counterApp.mutation({
    args: { id: v.string() },
    handler: ({ db }, { id }) =>
      Effect.gen(function* () {
        const doc = (yield* db.get("counters", id)) as { value: number };
        yield* Effect.yieldNow();
        yield* db.patch("counters", id, { value: doc.value + 1 });
      }),
  });

  const h = harness();
  try {
    await Effect.runPromise(
      Effect.gen(function* () {
        const storage = yield* Storage;
        const id = (yield* storage.mutation(seed, {})) as string;
        const exits = yield* Effect.forEach(
          Array.from({ length: 50 }),
          () => Effect.exit(storage.mutation(increment, { id })),
          { concurrency: "unbounded" },
        );
        assert.ok(exits.every(Exit.isSuccess), "every increment committed");
        const final = (yield* storage.mutation(read, { id })) as { value: number };
        assert.equal(final.value, 50, "no lost updates: 50 increments, final value 50");
      }).pipe(
        Effect.provide(
          StorageLive({ file: h.file, schema: counterSchema, appId: "test-app" }).pipe(
            Layer.provide(h.deps),
          ),
        ),
      ),
    );
  } finally {
    h.cleanup();
  }
});

// ---------------------------------------------------------------------------
// FFI-05: unbounded reads inside a mutation run nonblocking under the statement deadline
// ---------------------------------------------------------------------------

test("FFI-05: a mutation whose find runs past the deadline rolls back, fails 504, and the next mutation succeeds", async () => {
  const dir = mkdtempSync(join(tmpdir(), "mansio-ffi05-writer-"));
  const file = join(dir, "app.sqlite");
  const jsonSchema = withStorage(schema, "json");
  try {
    // 200 000 games, seeded before the writer opens the file. JSON layout: a non-key filter is a
    // `json_extract` per row, so the scan below takes about 200 ms on vm-1, four deadlines.
    const fixture = inspectFile(file, { write: true });
    try {
      fixture.exec("PRAGMA journal_mode=WAL");
      const ddl = compileDDL(jsonSchema);
      assert.ok(ddl._tag === "Right");
      for (const text of ddl.right) fixture.exec(text);
      fixture.run(
        `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 199999)
         INSERT INTO "games" ("_id", "_creationTime", "_version", "doc") SELECT 'seed-' || i, i, 1, ? FROM n`,
        [JSON.stringify({
          xPlayer: "alice",
          oPlayer: "bob",
          board: Array(9).fill(null),
          turn: "X",
          status: "playing",
          winner: null,
        })],
      );
    } finally {
      fixture.close();
    }

    const jsonApp = defineFunctions(jsonSchema);
    const game = (xPlayer: string) => ({
      xPlayer,
      oPlayer: "bob",
      board: Array(9).fill(null),
      turn: "X" as const,
      status: "playing" as const,
      winner: null,
    });
    // Writes a row, then reads unboundedly: the read must stop at the deadline and take the
    // write with it.
    const writeThenScan = jsonApp.mutation({
      args: {},
      handler: ({ db }) =>
        Effect.gen(function* () {
          yield* db.insert("games", game("marker"));
          return yield* db.find("games", { xPlayer: "alice", oPlayer: "nobody" }).collect();
        }),
    });
    const create = jsonApp.mutation({
      args: {},
      handler: ({ db }) => db.insert("games", game("after")),
    });
    // A bounded read inside a mutation stays on the blocking path.
    const byId = jsonApp.mutation({
      args: { id: v.string() },
      handler: ({ db }, { id }) => db.get("games", id),
    });

    const events: EventRecord[] = [];
    let id = 0;
    const deadlineMs = 50;
    const deps = Layer.mergeAll(
      Layer.succeed(EventSink, { emit: (event) => Effect.sync(() => void events.push(event)) }),
      Layer.succeed(IdGen, { next: Effect.sync(() => `id-${++id}`) }),
      ConfigTest({ sqlite: { statementDeadlineMs: deadlineMs } }),
    );
    await Effect.runPromise(
      Effect.gen(function* () {
        const storage = yield* Storage;
        events.length = 0;
        const started = performance.now();
        const failed = yield* Effect.flip(storage.mutation(writeThenScan, {}));
        const wallMs = performance.now() - started;
        assert.equal(failed._tag, "StatementDeadlineError");
        const deadline = failed as StatementDeadlineError;
        assert.equal(deadline.statementKind, "select");
        assert.equal(deadline.table, "games");
        assert.equal(deadline.deadlineMs, deadlineMs);
        assert.equal(toHttpStatus(deadline), 504);
        console.log(
          `FFI-05 writer deadline: statement ${deadline.elapsedMs.toFixed(1)} ms, mutation ${
            wallMs.toFixed(1)
          } ms; load1 ${Deno.loadavg()[0].toFixed(2)}`,
        );

        const sql = ofKind(events, "sql") as SqlEventRecord[];
        assert.deepEqual(
          sql.map((e) => [e.statementKind, e.path, e.outcome, e.errorTag ?? null]),
          [
            ["insert", "blocking", "ok", null],
            ["select", "nonblocking", "error", "StatementDeadlineError"],
          ],
        );
        const tx = ofKind(events, "tx") as TxEventRecord[];
        assert.equal(tx.length, 1);
        assert.equal(tx[0].outcome, "rollback");
        assert.equal(tx[0].errorTag, "StatementDeadlineError");
        assert.equal(ofKind(events, "lifecycle").length, 0, "no stuck-statement event");

        // The same connection takes the next mutation, and a get-by-id inside one is blocking.
        events.length = 0;
        const created = (yield* storage.mutation(create, {})) as string;
        assert.equal(created, "id-2");
        const found = (yield* storage.mutation(byId, { id: created })) as { xPlayer: string };
        assert.equal(found.xPlayer, "after");
        assert.deepEqual(
          (ofKind(events, "sql") as SqlEventRecord[]).map((e) => [e.statementKind, e.path, e.outcome]),
          // The get and its U+0000 probe: both bounded, both blocking.
          [["insert", "blocking", "ok"], ["select", "blocking", "ok"], ["select", "blocking", "ok"]],
        );
        assert.deepEqual((ofKind(events, "tx") as TxEventRecord[]).map((e) => e.outcome), [
          "commit",
          "commit",
        ]);
      }).pipe(
        Effect.provide(
          StorageLive({ file, schema: jsonSchema, appId: "test-app" }).pipe(Layer.provide(deps)),
        ),
      ),
    );

    // Nothing of the failed mutation was committed; the next one was.
    const after = inspectFile(file);
    try {
      assert.equal(
        after.get(`SELECT count(*) AS n FROM "games" WHERE json_extract("doc", '$.xPlayer') = 'marker'`)?.n,
        0,
      );
      assert.equal(after.get(`SELECT count(*) AS n FROM "games" WHERE "_id" = 'id-2'`)?.n, 1);
      assert.equal(after.get(`SELECT count(*) AS n FROM "games"`)?.n, 200_001);
    } finally {
      after.close();
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("FFI-05: a concurrent statement waits for an in-flight nonblocking read instead of failing busy", async () => {
  const dir = mkdtempSync(join(tmpdir(), "mansio-ffi05-gate-"));
  const db = Database.open(join(dir, "gate.sqlite"));
  try {
    db.exec("CREATE TABLE t (x INTEGER)");
    db.exec("INSERT INTO t VALUES (1)");
    const gate = new ConnectionGate(db);
    // A read of about 100 ms on the pool (a recursive CTE that counts to 3e5; well under the 5 s deadline).
    const slow =
      "WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 300000) SELECT count(*) AS c FROM n";
    const order: string[] = [];
    await Effect.runPromise(Effect.gen(function* () {
      const read = yield* Effect.fork(
        gate.nonblocking({
          sql: slow,
          execute: (statement) => allAsync(statement, [], { timeoutMs: 5000 }),
          limits: { deadlineMs: 5000, graceMs: 1000 },
          onStuck: Effect.void,
        }).pipe(Effect.tap(() => Effect.sync(() => void order.push("read")))),
      );
      yield* Effect.sleep("2 millis");
      assert.ok(db.inFlight, "the read is on the pool");
      // Without the gate this would throw SqliteUsageError("busy") from the main thread. The native
      // call runs inside `start`, and only its result is returned as an Effect (the gate's contract).
      const value = yield* gate.blocking(() => {
        order.push("blocking");
        return Effect.succeed(db.prepare("SELECT x FROM t").get()?.x);
      });
      assert.equal(value, 1);
      assert.equal((yield* Fiber.join(read))[0]?.c, 300001);
      assert.deepEqual(order, ["read", "blocking"]);
    }));
  } finally {
    db.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test("FFI-05: a blocking start does its native work before returning, so a competing nonblocking claim cannot take the connection first", async () => {
  // The gate's contract (`ConnectionGate.blocking`): once `start` returns, nothing holds the
  // connection for it, so a nonblocking claimant may take it before the returned Effect runs. A start
  // that defers its native call into the Effect (`Effect.sync(() => nativeCall())`) then finds the
  // connection busy. `handOff` is that window made deterministic: it lets the competitor claim the
  // connection and waits until it has, standing in for any yield between `start` and its Effect.
  const dir = mkdtempSync(join(tmpdir(), "mansio-ffi05-claim-"));
  const db = Database.open(join(dir, "claim.sqlite"));
  try {
    db.exec("CREATE TABLE t (x INTEGER)");
    db.exec("INSERT INTO t VALUES (1)");
    const gate = new ConnectionGate(db);
    const slow =
      "WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 300000) SELECT count(*) AS c FROM n";
    const race = (start: (handOff: Effect.Effect<void>) => Effect.Effect<unknown, unknown>) =>
      Effect.gen(function* () {
        const go = yield* Deferred.make<void>();
        const claimed = yield* Deferred.make<void>();
        const competitor = yield* Effect.fork(Effect.zipRight(
          Deferred.await(go),
          gate.nonblocking({
            sql: slow,
            // Called in the claiming step, after the claim: `claimed` means the connection is taken.
            execute: (statement) =>
              Effect.zipRight(
                Deferred.succeed(claimed, undefined),
                allAsync(statement, [], { timeoutMs: 5000 }),
              ),
            limits: { deadlineMs: 5000, graceMs: 1000 },
            onStuck: Effect.void,
          }),
        ));
        const handOff = Effect.zipRight(Deferred.succeed(go, undefined), Deferred.await(claimed));
        const outcome = yield* Effect.exit(gate.blocking(() => start(handOff)));
        const read = yield* Fiber.join(competitor);
        return { outcome, inFlightAfter: db.inFlight, count: read[0]?.c };
      });

    // Contract-following start: the native call has returned before `start` does.
    const immediate = await Effect.runPromise(race((handOff) => {
      const x = db.prepare("SELECT x FROM t").get()?.x;
      return Effect.as(handOff, x);
    }));
    assert.ok(Exit.isSuccess(immediate.outcome), String(immediate.outcome));
    assert.equal(immediate.outcome.value, 1);
    assert.equal(immediate.count, 300001);
    assert.equal(immediate.inFlightAfter, false);

    // The deferred pattern the contract forbids: the competitor claims first and the native call,
    // made later, is refused busy. This is what the fixtures above must never do.
    const deferred = await Effect.runPromise(race((handOff) =>
      Effect.zipRight(
        handOff,
        Effect.try({ try: () => db.prepare("SELECT x FROM t").get()?.x, catch: (cause) => cause }),
      )
    ));
    assert.ok(Exit.isFailure(deferred.outcome), "a deferred native call lost the connection to the claim");
    const failure = Cause.squash(deferred.outcome.cause);
    assert.ok(isSqliteFfiError(failure), String(failure));
    assert.equal(failure._tag, "SqliteUsageError");
    assert.equal((failure as { reason?: string }).reason, "busy");
    assert.equal(deferred.count, 300001);
  } finally {
    db.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test("FFI-05 grace period: a step that outlives its interrupt poisons the connection, which closes when the step returns", async () => {
  // `sqlite3_interrupt` cannot cut a busy-handler wait (measured on vm-1's 3.45.1: a step blocked on
  // another connection's EXCLUSIVE lock returned SQLITE_BUSY only when its busy timeout ran out), so
  // a read queued behind a lock is a real step that does not return after its interrupt.
  const dir = mkdtempSync(join(tmpdir(), "mansio-ffi05-grace-"));
  const file = join(dir, "grace.sqlite"); // rollback journal: readers wait for an EXCLUSIVE writer
  const holder = Database.open(file);
  const victim = Database.open(file, { busyTimeoutMs: 600 });
  try {
    holder.exec("CREATE TABLE t (x INTEGER)");
    const sql = "SELECT count(*) AS n FROM t";
    const gate = new ConnectionGate(victim);
    // Prepare (and cache) while unlocked: preparing reads the schema, which would wait on the main thread.
    assert.equal(
      Effect.runSync(gate.blocking(() => Effect.succeed(prepareCached(victim, sql).all())))[0]?.n,
      0,
    );
    holder.exec("BEGIN EXCLUSIVE");
    holder.exec("INSERT INTO t VALUES (1)");

    const limits = { deadlineMs: 30, graceMs: 100 };
    const started = performance.now();
    let stuckAt: number | undefined;
    const refused: string[] = [];
    const exit = await Effect.runPromise(Effect.gen(function* () {
      const read = yield* Effect.fork(Effect.exit(gate.nonblocking<unknown, string>({
        sql,
        guard: () => gate.poisoned ? "poisoned" : undefined,
        execute: (statement) => allAsync(statement, [], { timeoutMs: limits.deadlineMs }),
        limits,
        onStuck: Effect.sync(() => void (stuckAt = performance.now() - started)),
      })));
      // Queued behind the stuck read: it runs only after the step returns, and then refuses.
      yield* Effect.sleep("60 millis");
      const queued = yield* Effect.fork(gate.blocking(() => {
        if (gate.poisoned) refused.push(victim.isOpen ? "open" : "closed");
        else prepareCached(victim, sql).all();
        return Effect.void;
      }));
      const result = yield* Fiber.join(read);
      yield* Fiber.join(queued);
      return result;
    }));
    const returnedMs = performance.now() - started;
    console.log(
      `FFI-05 grace: deadline ${limits.deadlineMs} ms + grace ${limits.graceMs} ms, stuck event at ${
        stuckAt?.toFixed(1)
      } ms, step returned at ${returnedMs.toFixed(1)} ms (busy timeout 600 ms); load1 ${
        Deno.loadavg()[0].toFixed(2)
      }`,
    );
    assert.ok(stuckAt !== undefined, "onStuck ran");
    assert.ok(stuckAt >= limits.deadlineMs + limits.graceMs, `stuck event at ${stuckAt} ms`);
    assert.ok(stuckAt < 600, "before the step returned");
    assert.ok(returnedMs >= 600, `the step returned at ${returnedMs} ms`);
    assert.ok(gate.poisoned);
    // The step's own outcome: it was cancelled at the deadline but returned SQLITE_BUSY, which the
    // binding reports as the step's error.
    assert.ok(Exit.isFailure(exit));
    const failure = Cause.squash(exit.cause);
    assert.ok(isSqliteFfiError(failure), String(failure));
    assert.equal(failure._tag, "SqliteError");
    // Closed by the gate as soon as the step returned; the queued statement saw that and refused.
    assert.equal(victim.isOpen, false);
    assert.deepEqual(refused, ["closed"]);
    assert.match(POISONED_DETAIL, /poisoned/);
  } finally {
    if (holder.inTransaction) holder.exec("ROLLBACK");
    holder.close();
    await victim.closeAsync();
    rmSync(dir, { recursive: true, force: true });
  }
});
