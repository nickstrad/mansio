import {
  apps,
  assert,
  client,
  delay,
  Effect,
  play,
  remoteError,
  run,
  scenarioDir,
  startGoAgent,
  TENANT,
  test,
} from "../harness.ts";
import { createClient } from "../../../../sdk/client.ts";
import { withStorage } from "../../../../sdk/schema.ts";
import { functions } from "../../../../examples/tic-tac-toe/functions.ts";
import { schema } from "../../../../examples/tic-tac-toe/schema.ts";
import { moves, names } from "../seed.ts";

type Line = Record<string, unknown>;

/**
 * The keys of each record kind, in the order agentd writes them (`server/internal/eventlog/record.go`,
 * documented in `docs/observability.md`). Every key is always present; an absent value is `null`.
 */
const ENVELOPE = [
  "eventId",
  "ts",
  "level",
  "role",
  "pid",
  "traceId",
  "spanId",
  "parentSpanId",
  "appId",
  "kind",
];
const KEYS: Record<string, readonly string[]> = {
  http: [
    "method",
    "path",
    "handlerKind",
    "name",
    "status",
    "durationMs",
    "bytesIn",
    "bytesOut",
    "errorTag",
    "errorDetail",
  ],
  handler: ["handlerKind", "name", "outcome", "durationMs", "errorTag", "errorDetail"],
  sql: [
    "engine",
    "dialect",
    "statementKind",
    "path",
    "tableName",
    "sqlText",
    "bindCount",
    "bindValues",
    "durationMs",
    "rowCount",
    "outcome",
    "errorTag",
    "errorDetail",
  ],
  tx: ["engine", "outcome", "operations", "durationMs", "queueWaitMs", "errorTag", "errorDetail"],
};

/** Every record in agentd's request log: `<dir>/server/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl`. */
async function readLog(dir: string): Promise<Line[]> {
  const out: Line[] = [];
  const server = `${dir}/server`;
  let days: Deno.DirEntry[] = [];
  try {
    days = await Array.fromAsync(Deno.readDir(server));
  } catch (e) {
    if (e instanceof Deno.errors.NotFound) return out;
    throw e;
  }
  for (const day of days) {
    assert.match(day.name, /^\d{4}-\d{2}-\d{2}$/, `unexpected entry ${server}/${day.name}`);
    for await (const file of Deno.readDir(`${server}/${day.name}`)) {
      assert.match(file.name, /^events-\d{2}-\d+-\d{3,}\.jsonl$/, `unexpected file ${day.name}/${file.name}`);
      const text = await Deno.readTextFile(`${server}/${day.name}/${file.name}`);
      // The writer flushes whole lines; a line still being written has no newline yet.
      for (const line of text.split("\n").slice(0, -1)) out.push(JSON.parse(line));
    }
  }
  return out;
}

/** A record's keys are exactly the envelope then its kind's keys, in order. */
function assertShape(record: Line) {
  const kind = String(record.kind);
  assert.deepEqual(Object.keys(record), [...ENVELOPE, ...KEYS[kind]], `${kind} record keys`);
  assert.equal(record.role, "server");
  assert.equal(typeof record.pid, "number");
  assert.match(
    String(record.eventId),
    /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
  );
  assert.match(String(record.ts), /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/);
  assert.equal(typeof record.durationMs, "number");
}

/**
 * The records of one traced SDK call that made one request: an http record (no span), and a handler, a tx
 * and its sql records sharing one span.
 */
function assertTrace(
  log: Line[],
  traceId: string,
  expect: { handlerKind: string; name: string; statement: string; table: string },
) {
  const records = log.filter((r) => r.traceId === traceId);
  const of = (kind: string) => records.filter((r) => r.kind === kind);
  records.forEach(assertShape);
  const [http] = of("http");
  assert.ok(http, `no http record for ${traceId}`);
  assert.equal(of("http").length, 1, `${traceId}: one request`);
  assert.equal(http.method, "POST");
  assert.equal(http.path, "/v1/ops");
  assert.equal(http.handlerKind, expect.handlerKind);
  assert.equal(http.name, expect.name, "the SDK names the handler (X-Mansio-Handler)");
  assert.equal(http.status, 200);
  assert.equal(http.level, "info");
  assert.equal(http.appId, apps[0]);
  assert.equal(http.spanId, null);
  assert.equal(http.errorTag, null);
  assert.ok(Number(http.bytesIn) > 0 && Number(http.bytesOut) > 0, "request and answer sizes");

  const [handler] = of("handler");
  assert.ok(handler, `no handler record for ${traceId}`);
  assert.equal(handler.handlerKind, expect.handlerKind);
  assert.equal(handler.name, expect.name);
  assert.equal(handler.outcome, "ok");
  assert.equal(typeof handler.spanId, "string");

  const [tx] = of("tx");
  assert.ok(tx, `no tx record for ${traceId}`);
  assert.equal(tx.engine, "sqlite");
  assert.equal(tx.outcome, "commit");
  assert.equal(typeof tx.queueWaitMs, "number");

  const sql = of("sql");
  assert.ok(sql.length > 0, `no sql records for ${traceId} (trace_sql is on)`);
  assert.equal(tx.operations, sql.length, "one sql record per statement of the transaction");
  for (const s of sql) {
    assert.equal(s.engine, "sqlite");
    assert.equal(s.outcome, "ok");
    assert.equal(s.bindValues, null, "bind values are never logged");
    assert.equal(typeof s.sqlText, "string");
    assert.equal(typeof s.bindCount, "number");
  }
  assert.ok(
    sql.some((s) => s.statementKind === expect.statement && s.tableName === expect.table),
    `${traceId}: a ${expect.statement} on ${expect.table} in ${JSON.stringify(sql.map((s) => s.sqlText))}`,
  );
  for (const r of [tx, ...sql]) assert.equal(r.spanId, handler.spanId, "handler, tx and sql share one span");
  for (const r of records) assert.equal(r.appId, apps[0]);
}

/**
 * The SDK runs the handlers; agentd writes its request log (`event_log`, JSON lines under `<dir>/logs`,
 * `trace_sql` on). The scenario reads those files as an operator or a log collector would: a traced query
 * and a traced mutation each leave their http, handler, tx and sql records under the call's trace id, and a
 * request the server refuses leaves its error. An app conflict never reaches the server (the SDK fails
 * `makeMove` locally), so the server-side failure is a schema drift: a client compiled for the other layout.
 */
const scenario = (dir: string) =>
  Effect.gen(function* () {
    const logDir = `${dir}/logs`;
    const server = yield* startGoAgent(dir, { event_log: { dir: logDir, trace_sql: true } });
    yield* Effect.promise(async () => {
      const c = client(server);
      const game = await play(c, moves.X.slice(0, 2));
      const readTrace = `e2e-read-${names[9]}`;
      const writeTrace = `e2e-write-${names[9]}`;
      await run(c.query("getGame", { gameId: game._id }, readTrace));
      await run(c.mutation("createGame", { xPlayer: names[10], oPlayer: names[11] }, writeTrace));
      await run(c.query("listGames", { status: "playing" }));
      await run(c.query("winsByMark", {}));
      const drifted = createClient({
        functions,
        schema: withStorage(schema, "json"),
        tenant: TENANT,
        app: apps[0],
        endpoint: server.base,
        token: (kind) => server.mint(apps[0], kind),
        timeoutMs: 15_000,
      });
      await remoteError(drifted.query("listGames", { status: "won" }), 409, "SchemaDriftError");

      // The writer flushes asynchronously: poll the files until the whole workload is there.
      const handlers = ["createGame", "getGame", "makeMove", "listGames", "winsByMark"];
      const drift = (r: Line) => r.kind === "http" && r.name === "listGames" && r.status === 409;
      const complete = (log: Line[]) =>
        [readTrace, writeTrace].every((t) =>
          ["http", "handler", "tx", "sql"].every((k) => log.some((r) => r.traceId === t && r.kind === k))
        ) &&
        handlers.every((name) => log.some((r) => r.kind === "http" && r.name === name)) &&
        log.some(drift) &&
        log.some((r) => r.kind === "handler" && r.name === "listGames" && r.outcome === "error");
      const deadline = Date.now() + 15_000;
      let log = await readLog(logDir);
      while (!complete(log)) {
        assert.ok(Date.now() < deadline, `incomplete request log within 15 s: ${JSON.stringify(log)}`);
        await delay(100);
        log = await readLog(logDir);
      }

      assertTrace(log, readTrace, {
        handlerKind: "query",
        name: "getGame",
        statement: "select",
        table: "games",
      });
      assertTrace(log, writeTrace, {
        handlerKind: "mutation",
        name: "createGame",
        statement: "insert",
        table: "games",
      });

      const refused = log.find(drift)!;
      assertShape(refused);
      assert.equal(refused.errorTag, "SchemaDriftError");
      assert.equal(refused.level, "warn");
      assert.equal(
        (refused.errorDetail as Line | null)?.tag,
        "SchemaDriftError",
        "the error the client was told",
      );
      const failed = log.find((r) =>
        r.kind === "handler" && r.traceId === refused.traceId && r.outcome === "error"
      );
      assert.ok(failed, "the refused request's handler record");
      assert.equal(failed.errorTag, "SchemaDriftError");

      assert.ok(
        log.some((r) => r.kind === "lifecycle" && r.phase === "startup" && r.traceId === "lifecycle"),
        "agentd's startup record",
      );
      for (const r of log) assert.equal(r.role, "server");
    });
  });

test(
  "e2e/6-log-analysis: agentd's request log holds each traced request's http, handler, tx and sql records",
  () =>
    Effect.gen(function* () {
      const dir = yield* scenarioDir("log_analysis");
      yield* scenario(dir);
    }),
);
