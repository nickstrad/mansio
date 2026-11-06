/**
 * Unit tests for the `Reader` service and the reader process (plan item 16).
 *
 * **Two processes, on purpose.** `deno task test` runs every test module in one OS process, and the
 * regression and property suites load the writer's SQLite there (the FFI binding over the system
 * `libsqlite3`). DuckDB's `sqlite` extension is a second copy of the SQLite library, and the two must
 * never share an address space — the rule that makes the reader a separate process in production
 * applies to tests as well. So this module has two halves, selected by `MANSIO_READER_FIXTURE`:
 *
 * - **Parent** (the variable is unset): never loads DuckDB. It writes the SQLite fixture files with
 *   the FFI binding (`tests/helpers/sqlite_inspect.ts`) and the pure compiler, then (1) runs this
 *   same module again as a child `deno test` pointed at the fixture, and (2) drives the real
 *   `platform/reader.ts` over stdin/stdout.
 * - **Child** (the variable names the fixture directory): never loads the writer's SQLite. It exercises
 *   `ReaderLive` in-process against real DuckDB, with a probe driver recording the order native
 *   handles are opened and closed in.
 *
 * Both halves import the native modules dynamically, so neither half's import graph pulls in the
 * other's SQLite.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { Cause, Deferred, Effect, Either, Exit, Fiber, Layer, Option, TestClock, TestContext } from "effect";
import { RuleError } from "../../core/errors.ts";
import type { EventRecord, SqlEventRecord, TxEventRecord } from "../../core/events.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileInsert } from "../../core/compile/write.ts";
import { schema } from "../../app/schema.ts";
import { functions } from "../../app/functions.ts";
import { defineFunctions } from "../../sdk/functions.ts";
import type { ReadDB, WriteDB } from "../../sdk/db.ts";
import { withStorage } from "../../sdk/schema.ts";
import { ConfigTest } from "../../platform/config.ts";
import { EventSink, Reader, type ReaderReply } from "../../platform/services.ts";
import type { DuckDriver, ReaderApp } from "../../platform/duckdb.ts";

/** Unwrap a right, failing the test with the rendered left on a left. */
function expectRight<A, E>(either: Either.Either<A, E>): A {
  if (Either.isRight(either)) return either.right;
  assert.fail(`expected a right, got left ${JSON.stringify(either.left)}`);
}

const FIXTURE_ENV = "MANSIO_READER_FIXTURE";
const fixture = Deno.env.get(FIXTURE_ENV);
const repoRoot = fileURLToPath(new URL("../../", import.meta.url));

const GAME = {
  xPlayer: "alice",
  oPlayer: "bob",
  board: [null, null, null, null, null, null, null, null, null],
  turn: "X",
  status: "playing",
  winner: null,
};

// ===========================================================================
// Parent half: no DuckDB in this process
// ===========================================================================

/** Writes both apps' SQLite files the way `loadApps` names them, each holding one game. */
async function writeFixture(dir: string): Promise<void> {
  const { inspect } = await import("../helpers/sqlite_inspect.ts");
  const layouts = [
    ["tic-tac-toe.sqlite", schema],
    ["tic-tac-toe-json.sqlite", withStorage(schema, "json")],
  ] as const;
  for (const [file, layout] of layouts) {
    const db = inspect(join(dir, file), { write: true });
    try {
      for (const statement of expectRight(compileDDL(layout))) db.exec(statement);
      const insert = expectRight(
        compileInsert(layout, "games", GAME, { _id: "game-1", _creationTime: 1 }),
      );
      db.run(insert.text, insert.values);
    } finally {
      db.close();
    }
  }
}

async function withFixture(run: (dir: string) => Promise<void>): Promise<void> {
  const dir = await Deno.makeTempDir({ prefix: "mansio-reader-" });
  try {
    await writeFixture(dir);
    await run(dir);
  } finally {
    await Deno.remove(dir, { recursive: true });
  }
}

/** Every file under `dir`, as paths relative to it. */
async function filesUnder(dir: string, prefix = ""): Promise<string[]> {
  const found: string[] = [];
  for await (const entry of Deno.readDir(join(dir, prefix))) {
    const relative = prefix ? `${prefix}/${entry.name}` : entry.name;
    if (entry.isDirectory) found.push(...await filesUnder(dir, relative));
    else found.push(relative);
  }
  return found.sort();
}

/** Runs `platform/reader.ts` to completion with `input` on stdin. */
async function runReader(dataDir: string, logDir: string, input: string) {
  const child = new Deno.Command(Deno.execPath(), {
    args: ["run", "-A", "platform/reader.ts"],
    cwd: repoRoot,
    env: { DATA_DIR: dataDir, LOG_DIR: logDir },
    stdin: "piped",
    stdout: "piped",
    stderr: "piped",
  }).spawn();
  const writer = child.stdin.getWriter();
  await writer.write(new TextEncoder().encode(input));
  await writer.close();
  const output = await child.output();
  return {
    code: output.code,
    stdout: new TextDecoder().decode(output.stdout),
    stderr: new TextDecoder().decode(output.stderr),
  };
}

if (fixture === undefined) {
  test("Reader service suite passes in its own process (DuckDB never shares one with the writer's SQLite)", async () => {
    await withFixture(async (dir) => {
      const output = await new Deno.Command(Deno.execPath(), {
        args: ["test", "-A", "--no-check", fileURLToPath(import.meta.url)],
        cwd: repoRoot,
        env: { [FIXTURE_ENV]: dir },
        stdout: "piped",
        stderr: "piped",
      }).output();
      const text = new TextDecoder().decode(output.stdout) + new TextDecoder().decode(output.stderr);
      assert.equal(output.code, 0, text);
      // Guard against the child silently selecting the parent half and passing vacuously.
      assert.match(text, /failed ATTACH fails the layer/);
    });
  });

  test("reader process: stdout carries only RPC reply lines; every log record lands under logs/reader", async () => {
    await withFixture(async (dir) => {
      const logDir = join(dir, "logs");
      const requests = [
        { id: 1, kind: "ping" },
        { id: 2, kind: "query", appId: "tic-tac-toe", name: "getGame", args: { gameId: "game-1" } },
        { id: 3, kind: "query", appId: "tic-tac-toe-json", name: "listGames", args: { status: "playing" } },
        { id: 4, kind: "query", appId: "nope", name: "getGame", args: {} },
        { id: 5, kind: "query", appId: "tic-tac-toe", name: "nope", args: {} },
        { id: 6, kind: "query", appId: "tic-tac-toe", name: "createGame", args: {} },
        { id: 7, kind: "query", appId: "tic-tac-toe", name: "getGame", args: { gameId: 7 } },
        { id: 8, kind: "ingest" },
        { id: 9, kind: "query", appId: "tic-tac-toe", name: "getGame", args: { gameId: "missing" } },
      ];
      // Frames split across chunks are the framing tests' job; one write here still delivers
      // several frames per chunk.
      const run = await runReader(dir, logDir, requests.map((r) => JSON.stringify(r) + "\n").join(""));
      assert.equal(run.code, 0, run.stderr);
      assert.equal(run.stderr, "", "a clean run writes nothing to stderr");

      const lines = run.stdout.split("\n");
      assert.equal(lines.pop(), "", "stdout ends with a newline");
      const replies = lines.map((line) => JSON.parse(line) as ReaderReply);
      assert.deepEqual(
        replies.map((r) => r.id),
        requests.map((r) => r.id),
        "one reply per request, in order",
      );
      for (const reply of replies) {
        const keys = Object.keys(reply).sort();
        assert.ok(
          JSON.stringify(keys) === '["id","result"]' || JSON.stringify(keys) === '["error","id"]',
          `reply shape: ${JSON.stringify(reply)}`,
        );
      }
      const result = (id: number) => (replies[id - 1] as { result: unknown }).result;
      const tag = (id: number) => (replies[id - 1] as { error?: { _tag: string } }).error?._tag;
      assert.equal(result(1), "ready");
      assert.equal((result(2) as { _id: string; xPlayer: string })._id, "game-1");
      assert.equal((result(2) as { xPlayer: string }).xPlayer, "alice");
      assert.equal((result(3) as unknown[]).length, 1);
      assert.equal(tag(4), "UnknownAppError");
      assert.equal(tag(5), "UnknownHandlerError");
      assert.equal(tag(6), "HandlerKindMismatch");
      assert.equal(tag(7), "SchemaError");
      // Item 33: `ingest` is answered by the event-log ingest-then-sweep coordinator.
      assert.equal(typeof (result(8) as { filesScanned?: unknown }).filesScanned, "number");
      assert.equal(result(9), null);
      const frame = (replies[6] as { error: { message: string; fields: Record<string, unknown> } }).error;
      assert.equal(frame.fields.path, "gameId");
      assert.match(frame.message, /^SchemaError: /);

      const files = await filesUnder(logDir);
      assert.ok(files.length > 0, "the reader wrote a log");
      for (const file of files) assert.match(file, /^reader\/\d{4}-\d{2}-\d{2}\/events-.*\.jsonl$/);
      const records = (await Promise.all(files.map((file) => Deno.readTextFile(join(logDir, file)))))
        .join("")
        .split("\n")
        .filter((line) => line !== "")
        .map((line) => JSON.parse(line) as EventRecord);
      assert.ok(records.every((r) => r.role === "reader"));
      const phases = records.flatMap((r) => r.kind === "lifecycle" ? [r.phase] : []);
      assert.equal(phases[0], "ready");
      // Item 33: the event-log handle opened by the ingest closes after serving stops.
      assert.deepEqual(phases.slice(-2), ["stopping", "eventlog-closed"]);
      const txs = records.filter((r): r is TxEventRecord => r.kind === "tx");
      assert.ok(
        txs.some((r) => r.outcome === "commit" && r.engine === "duckdb" && r.appId === "tic-tac-toe"),
      );
      assert.ok(txs.some((r) => r.traceId.endsWith("-2")), "tx events carry the request's trace id");
    });
  });

  test("reader process: a failed reader open is not cached; the next request retries it", async () => {
    const dir = await Deno.makeTempDir({ prefix: "mansio-reader-" });
    try {
      const child = new Deno.Command(Deno.execPath(), {
        args: ["run", "-A", "platform/reader.ts"],
        cwd: repoRoot,
        env: { DATA_DIR: dir, LOG_DIR: join(dir, "logs") },
        stdin: "piped",
        stdout: "piped",
        stderr: "piped",
      }).spawn();
      const stdin = child.stdin.getWriter();
      const stdout = child.stdout.pipeThrough(new TextDecoderStream()).getReader();
      let buffered = "";
      const ask = async (id: number) => {
        const request = {
          id,
          kind: "query",
          appId: "tic-tac-toe",
          name: "getGame",
          args: { gameId: "game-1" },
        };
        await stdin.write(new TextEncoder().encode(JSON.stringify(request) + "\n"));
        while (!buffered.includes("\n")) {
          const chunk = await stdout.read();
          assert.ok(!chunk.done, "reader closed stdout early");
          buffered += chunk.value;
        }
        const line = buffered.slice(0, buffered.indexOf("\n"));
        buffered = buffered.slice(line.length + 1);
        return JSON.parse(line) as ReaderReply & {
          error?: { _tag: string; fields: Record<string, unknown> };
        };
      };
      const first = await ask(1);
      assert.equal(first.error?._tag, "ReaderError");
      assert.equal(first.error?.fields.operation, "attach");
      await writeFixture(dir);
      const second = await ask(2);
      assert.equal((second as { result: { _id: string } }).result._id, "game-1", JSON.stringify(second));
      await stdin.close();
      await stdout.cancel();
      const status = await child.status;
      await child.stderr.cancel();
      assert.equal(status.code, 0);
    } finally {
      await Deno.remove(dir, { recursive: true });
    }
  });

  test("reader process: a malformed frame ends the process with an error on stderr, not stdout", async () => {
    await withFixture(async (dir) => {
      const run = await runReader(
        dir,
        join(dir, "logs"),
        '{"id":1,"kind":"ping"}\nnot json\n{"id":2,"kind":"ping"}\n',
      );
      assert.equal(run.code, 1);
      assert.equal(run.stdout, '{"id":1,"result":"ready"}\n', "requests before the bad frame were answered");
      assert.match(run.stderr, /malformed-frame/);
    });
  });
}

// ===========================================================================
// Child half: real DuckDB, no writer SQLite
// ===========================================================================

if (fixture !== undefined) {
  const { duckDriver, emitLifecycle, makeReaderPool, ReaderLive } = await import(
    "../../platform/duckdb.ts"
  );

  /**
   * Wraps the real driver, logging each native open and close in the order they happen. `fail`
   * "begin" makes the first connection's `BEGIN TRANSACTION` throw, as a transaction left open by a
   * failed `ROLLBACK` would; later connections are healthy.
   */
  const probeDriver = (log: string[], fail?: "open" | "connect" | "begin"): DuckDriver => {
    let connections = 0;
    return {
      open: async () => {
        if (fail === "open") throw new Error("probe: instance refused");
        const instance = await duckDriver.open();
        log.push("instance:open");
        return {
          connect: async () => {
            if (fail === "connect") throw new Error("probe: connection refused");
            const connection = await instance.connect();
            const wedged = fail === "begin" && connections++ === 0;
            log.push("connection:open");
            return {
              run: (sql) => {
                if (wedged && sql === "BEGIN TRANSACTION") {
                  return Promise.reject(new Error("probe: cannot start a transaction within a transaction"));
                }
                return connection.run(sql);
              },
              runAndReadAll: (sql, values) => connection.runAndReadAll(sql, values),
              closeSync: () => {
                log.push("connection:close");
                connection.closeSync();
              },
            };
          },
          closeSync: () => {
            log.push("instance:close");
            instance.closeSync();
          },
        };
      },
    };
  };

  const columns: ReaderApp = { id: "tic-tac-toe", file: join(fixture, "tic-tac-toe.sqlite"), schema };
  const json: ReaderApp = {
    id: "tic-tac-toe-json",
    file: join(fixture, "tic-tac-toe-json.sqlite"),
    schema: withStorage(schema, "json"),
  };

  /** Config + an `EventSink` that records into an array. */
  const deps = (events: EventRecord[], logging: { traceSql?: boolean; includeBindValues?: boolean } = {}) =>
    Layer.merge(
      ConfigTest({ logging: { traceSql: true, ...logging } }),
      Layer.succeed(EventSink, { emit: (event) => Effect.sync(() => void events.push(event)) }),
    );

  /** Runs `program` against a fresh reader over `app`; returns the exit and everything emitted. */
  const withReader = async <A, E>(
    app: ReaderApp,
    program: Effect.Effect<A, E, Reader>,
    options: { log?: string[]; traceSql?: boolean; includeBindValues?: boolean } = {},
  ) => {
    const events: EventRecord[] = [];
    const layer = ReaderLive(app, probeDriver(options.log ?? [])).pipe(Layer.provide(deps(events, options)));
    const exit = await Effect.runPromiseExit(Effect.provide(program, layer));
    return { exit, events };
  };

  const failureOf = <A, E>(exit: Exit.Exit<A, E>): E => {
    assert.ok(Exit.isFailure(exit), "expected a failure");
    const failure = Cause.failureOption(exit.cause);
    assert.ok(Option.isSome(failure), `expected a typed failure, got ${Cause.pretty(exit.cause)}`);
    return failure.value;
  };

  const txs = (events: EventRecord[]) => events.filter((e): e is TxEventRecord => e.kind === "tx");
  const sqls = (events: EventRecord[]) => events.filter((e): e is SqlEventRecord => e.kind === "sql");

  const { query, mutation } = defineFunctions(schema);
  let leaked: ReadDB<typeof schema> | undefined;
  const probes = {
    write: query({
      args: {},
      // The cast is the attack: a query handler that reaches past `ReadDB` to write.
      handler: ({ db }) => (db as unknown as WriteDB<typeof schema>).insert("games", GAME as never),
    }),
    leak: query({
      args: {},
      handler: ({ db }) =>
        Effect.sync(() => {
          leaked = db;
          return null;
        }),
    }),
    rule: query({
      args: {},
      handler: ({ db }) => Effect.flatMap(db.get("games", "game-1"), () => new RuleError({ rule: "probe" })),
    }),
    defect: query({ args: {}, handler: () => Effect.die(new Error("probe defect")) }),
    bigint: query({ args: {}, handler: () => Effect.succeed(1n) }),
    mutate: mutation({ args: {}, handler: () => Effect.succeed(null) }),
  };
  const getGame = Effect.flatMap(Reader, (r) => r.query(functions.getGame, { gameId: "game-1" }));

  test("a failed ATTACH fails the layer with ReaderError and closes connection, then instance", async () => {
    const log: string[] = [];
    const missing = join(fixture, "does-not-exist.sqlite");
    const { exit } = await withReader({ ...columns, file: missing }, getGame, { log });
    const error = failureOf(exit);
    assert.equal(error._tag, "ReaderError");
    assert.equal(error._tag === "ReaderError" && error.operation, "attach");
    assert.deepEqual(log, ["instance:open", "connection:open", "connection:close", "instance:close"]);
    await assert.rejects(
      Deno.stat(missing),
      Deno.errors.NotFound,
      "READ_ONLY attach must not create the file",
    );
  });

  test("a failed connect closes the instance it opened; a failed open closes nothing", async () => {
    for (
      const [fail, expected] of [["connect", ["instance:open", "instance:close"]], ["open", []]] as const
    ) {
      const log: string[] = [];
      const events: EventRecord[] = [];
      const layer = ReaderLive(columns, probeDriver(log, fail)).pipe(Layer.provide(deps(events)));
      const error = failureOf(await Effect.runPromiseExit(Effect.provide(getGame, layer)));
      assert.equal(error._tag === "ReaderError" && error.operation, "open");
      assert.deepEqual(log, expected);
    }
  });

  test("a query reads the attached file, commits, and emits duckdb sql + tx events", async () => {
    const log: string[] = [];
    for (const app of [columns, json]) {
      log.length = 0;
      const { exit, events } = await withReader(app, getGame, { log });
      assert.ok(Exit.isSuccess(exit), Exit.isFailure(exit) ? Cause.pretty(exit.cause) : "");
      const game = exit.value as { _id: string; board: unknown[]; winner: unknown };
      assert.equal(game._id, "game-1");
      assert.equal(game.board.length, 9);
      assert.equal(game.winner, null);
      assert.deepEqual(log, ["instance:open", "connection:open", "connection:close", "instance:close"]);

      const [sql] = sqls(events);
      assert.equal(sql.engine, "duckdb");
      assert.equal(sql.dialect, "duckdb");
      assert.equal(sql.statementKind, "select");
      assert.equal(sql.outcome, "ok");
      assert.equal(sql.rowCount, 1);
      assert.equal(sql.appId, app.id);
      assert.equal(sql.bindValues, undefined, "bind values stay out unless opted in");
      const [tx] = txs(events);
      assert.equal(tx.engine, "duckdb");
      assert.equal(tx.outcome, "commit");
      assert.equal(tx.operations, 1);
    }
  });

  test("sql events follow TRACE_SQL; tx events are always emitted", async () => {
    const { events } = await withReader(columns, getGame, { traceSql: false });
    assert.equal(sqls(events).length, 0);
    assert.equal(txs(events).length, 1);
  });

  test("a write in a query context fails QueryWriteAttemptError and rolls back", async () => {
    const { exit, events } = await withReader(
      columns,
      Effect.flatMap(Reader, (r) => r.query(probes.write, {})),
    );
    const error = failureOf(exit);
    assert.equal(error._tag, "QueryWriteAttemptError");
    assert.equal(error._tag === "QueryWriteAttemptError" && error.operation, "insert");
    assert.equal(error._tag === "QueryWriteAttemptError" && error.table, "games");
    const [tx] = txs(events);
    assert.equal(tx.outcome, "rollback");
    assert.equal(tx.errorTag, "QueryWriteAttemptError");
  });

  test("a Documents handle used after its query ends fails ContextExpiredError", async () => {
    leaked = undefined;
    const program = Effect.gen(function* () {
      const reader = yield* Reader;
      yield* reader.query(probes.leak, {});
      assert.ok(leaked);
      return yield* leaked.get("games", "game-1");
    });
    const { exit } = await withReader(columns, program);
    const error = failureOf(exit);
    assert.equal(error._tag, "ContextExpiredError");
    assert.equal(error._tag === "ContextExpiredError" && error.context, "query");
  });

  test("handler failures and defects roll back and leave the connection usable", async () => {
    const program = Effect.gen(function* () {
      const reader = yield* Reader;
      const rule = yield* Effect.exit(reader.query(probes.rule, {}));
      const defect = yield* Effect.exit(reader.query(probes.defect, {}));
      const bigint = yield* Effect.exit(reader.query(probes.bigint, {}));
      const after = yield* reader.query(functions.getGame, { gameId: "game-1" });
      return { rule, defect, bigint, after };
    });
    const { exit, events } = await withReader(columns, program);
    assert.ok(Exit.isSuccess(exit), Exit.isFailure(exit) ? Cause.pretty(exit.cause) : "");
    const { rule, defect, bigint, after } = exit.value;
    assert.equal(failureOf(rule)._tag, "RuleError");
    assert.ok(Exit.isFailure(defect) && Cause.isDie(defect.cause), "a handler defect stays a defect");
    const serial = failureOf(bigint);
    assert.equal(serial._tag === "ReaderError" && serial.operation, "commit");
    assert.equal((after as { _id: string })._id, "game-1");
    assert.deepEqual(txs(events).map((t) => [t.outcome, t.errorTag]), [
      ["rollback", "RuleError"],
      ["rollback", "Defect"],
      ["rollback", "ReaderError"],
      ["commit", undefined],
    ]);
  });

  test("a mutation definition is refused as a defect, before any transaction", async () => {
    const { exit, events } = await withReader(
      columns,
      Effect.flatMap(Reader, (r) => r.query(probes.mutate, {})),
    );
    assert.ok(Exit.isFailure(exit) && Cause.isDie(exit.cause));
    const defect = Cause.squash(exit.cause) as { _tag?: string };
    assert.equal(defect._tag, "HandlerKindMismatch");
    assert.equal(txs(events).length, 0);
  });

  test("interrupting a query mid-handler rolls back; the next query still runs", async () => {
    const program = Effect.gen(function* () {
      const reader = yield* Reader;
      const started = yield* Deferred.make<void>();
      const { query: q } = defineFunctions(schema);
      const hang = q({
        args: {},
        handler: ({ db }) =>
          Effect.flatMap(
            db.get("games", "game-1"),
            () => Effect.zipRight(Deferred.succeed(started, void 0), Effect.never),
          ),
      });
      const fiber = yield* Effect.fork(reader.query(hang, {}));
      yield* Deferred.await(started);
      yield* Fiber.interrupt(fiber);
      return yield* reader.query(functions.getGame, { gameId: "game-1" });
    });
    const { exit, events } = await withReader(columns, program);
    assert.ok(Exit.isSuccess(exit), Exit.isFailure(exit) ? Cause.pretty(exit.cause) : "");
    assert.deepEqual(txs(events).map((t) => t.outcome), ["interrupted", "commit"]);
  });

  test("concurrent queries on one reader are serialized, never interleaved in one transaction", async () => {
    const program = Effect.flatMap(Reader, (reader) =>
      Effect.all(
        Array.from({ length: 5 }, () => reader.query(functions.listGames, { status: "playing" })),
        { concurrency: "unbounded" },
      ));
    const { exit, events } = await withReader(columns, program);
    assert.ok(Exit.isSuccess(exit), Exit.isFailure(exit) ? Cause.pretty(exit.cause) : "");
    assert.ok(exit.value.every((rows) => (rows as unknown[]).length === 1));
    assert.deepEqual(txs(events).map((t) => t.outcome), Array(5).fill("commit"));
  });

  test("a failed BEGIN drops the cached reader; the next query opens a fresh connection", async () => {
    const log: string[] = [];
    const events: EventRecord[] = [];
    const program = Effect.gen(function* () {
      const reader = (yield* makeReaderPool(probeDriver(log, "begin")))(columns);
      const first = yield* Effect.exit(reader.query(functions.getGame, { gameId: "game-1" }));
      const afterFirst = [...log];
      const second = yield* reader.query(functions.getGame, { gameId: "game-1" });
      const third = yield* reader.query(functions.getGame, { gameId: "game-1" });
      return { first, afterFirst, second, third };
    }).pipe(Effect.scoped, Effect.provide(deps(events)));
    const exit = await Effect.runPromiseExit(program);
    assert.ok(Exit.isSuccess(exit), Exit.isFailure(exit) ? Cause.pretty(exit.cause) : "");
    const { first, afterFirst, second, third } = exit.value;

    const error = failureOf(first);
    assert.equal(error._tag === "ReaderError" && error.operation, "begin");
    assert.deepEqual(
      afterFirst,
      ["instance:open", "connection:open", "connection:close", "instance:close"],
      "the wedged reader is closed, connection then instance, as soon as its BEGIN fails",
    );
    assert.equal((second as { _id: string })._id, "game-1");
    assert.equal((third as { _id: string })._id, "game-1");
    assert.deepEqual(
      log,
      [...afterFirst, "instance:open", "connection:open", "connection:close", "instance:close"],
      "one fresh reader serves the later queries and closes with the pool",
    );
    assert.deepEqual(txs(events).map((t) => t.outcome), ["commit", "commit"]);
  });

  test("the pool keeps a reader after an ordinary query failure and caches it across queries", async () => {
    const log: string[] = [];
    const events: EventRecord[] = [];
    const program = Effect.gen(function* () {
      const reader = (yield* makeReaderPool(probeDriver(log)))(columns);
      const rule = yield* Effect.exit(reader.query(probes.rule, {}));
      yield* reader.query(functions.getGame, { gameId: "game-1" });
      return { rule, afterQueries: [...log] };
    }).pipe(Effect.scoped, Effect.provide(deps(events)));
    const exit = await Effect.runPromiseExit(program);
    assert.ok(Exit.isSuccess(exit), Exit.isFailure(exit) ? Cause.pretty(exit.cause) : "");
    assert.equal(failureOf(exit.value.rule)._tag, "RuleError");
    assert.deepEqual(exit.value.afterQueries, ["instance:open", "connection:open"]);
  });

  test("interrupting a query mid-statement: ROLLBACK queues behind the running SELECT; the connection stays usable", async () => {
    // A real, slow DuckDB statement stands in for the compiled SELECT of the first query only.
    const SLOW = "SELECT sum(hash(range)) AS n FROM range(20000000)";
    const log: string[] = [];
    const at: Record<string, number> = {};
    const mark = (event: string) => {
      log.push(event);
      at[event] = performance.now();
    };
    let started!: () => void;
    const selectStarted = new Promise<void>((resolve) => (started = resolve));
    let slowed = false;
    const inner = probeDriver([]);
    const driver: DuckDriver = {
      open: async () => {
        const instance = await inner.open();
        return {
          closeSync: () => instance.closeSync(),
          connect: async () => {
            const connection = await instance.connect();
            return {
              closeSync: () => connection.closeSync(),
              run: async (sql) => {
                const tracked = sql === "ROLLBACK";
                if (tracked) mark("rollback:start");
                const result = await connection.run(sql);
                if (tracked) mark("rollback:done");
                return result;
              },
              runAndReadAll: async (sql, values) => {
                if (slowed) return await connection.runAndReadAll(sql, values);
                slowed = true;
                mark("select:start");
                started();
                try {
                  const rows = await connection.runAndReadAll(SLOW, {});
                  mark("select:ok");
                  return rows;
                } catch (error) {
                  mark("select:failed");
                  throw error;
                }
              },
            };
          },
        };
      },
    };

    const events: EventRecord[] = [];
    const program = Effect.gen(function* () {
      const reader = (yield* makeReaderPool(driver))(columns);
      const { query: q } = defineFunctions(schema);
      const slow = q({ args: {}, handler: ({ db }) => db.get("games", "game-1") });
      const fiber = yield* Effect.fork(reader.query(slow, {}));
      yield* Effect.promise(() => selectStarted);
      // Let the native call actually begin: a ROLLBACK issued before it starts could overtake it.
      yield* Effect.promise(() => new Promise((resolve) => setTimeout(resolve, 100)));
      yield* Fiber.interrupt(fiber);
      return yield* reader.query(functions.getGame, { gameId: "game-1" });
    }).pipe(Effect.scoped, Effect.provide(deps(events)));
    const exit = await Effect.runPromiseExit(program);
    assert.ok(Exit.isSuccess(exit), Exit.isFailure(exit) ? Cause.pretty(exit.cause) : "");
    assert.equal((exit.value as { _id: string })._id, "game-1");
    assert.deepEqual(log.slice(0, 2), ["select:start", "rollback:start"], "ROLLBACK is issued mid-statement");
    assert.ok(at["rollback:start"] - at["select:start"] >= 100, "the SELECT was running natively");
    // The abandoned SELECT ran to completion and the ROLLBACK succeeded; their native completions
    // land a few ms apart in either microtask order, so the check is "not materially earlier".
    assert.deepEqual(log.slice(2).sort(), ["rollback:done", "select:ok"]);
    assert.ok(
      at["rollback:done"] >= at["select:ok"] - 25,
      `ROLLBACK finished ${at["select:ok"] - at["rollback:done"]}ms before the SELECT it overlapped`,
    );
    assert.deepEqual(txs(events).map((t) => t.outcome), ["interrupted", "commit"]);
    assert.equal(txs(events)[0].errorTag, undefined);
  });

  test("lifecycle events are stamped from the Effect Clock, so TestClock controls them", async () => {
    const events: EventRecord[] = [];
    const sink = { emit: (event: EventRecord) => Effect.sync(() => void events.push(event)) };
    await Effect.runPromise(
      Effect.gen(function* () {
        yield* TestClock.setTime(Date.UTC(2026, 0, 2, 3, 4, 5));
        yield* emitLifecycle(sink, "ready", { apps: ["a"] });
        yield* TestClock.adjust("1 hour");
        yield* emitLifecycle(sink, "stopping");
      }).pipe(Effect.provide(TestContext.TestContext)),
    );
    assert.deepEqual(events.map((e) => [e.kind, e.ts]), [
      ["lifecycle", "2026-01-02T03:04:05.000Z"],
      ["lifecycle", "2026-01-02T04:04:05.000Z"],
    ]);
  });
}
