/**
 * Caller trace context end to end (plan item 34): the real SDK client, the real server process and
 * its real reader child (DuckDB), in isolated temp directories. Assertions read the JSONL logs both
 * processes wrote, after a SIGTERM has drained and closed them.
 *
 * The SDK is called exactly as an app would call it. Its answers do not expose response headers, so
 * a `fetch` spy records each request's `X-Trace-Id` and the one echoed back.
 *
 * What it shows:
 * - one explicit trace is echoed and identifies the `http`, `handler`, SQLite `sql`/`tx` events in the
 *   server across a mutation and a query, each under its own handler span;
 * - a generated trace (no argument) and an invalid one (replaced at ingress, echoed) do the same;
 * - a rolled-back mutation (409) and a reader-side error (400) keep the trace on every event;
 * - two callers overlapping many requests never swap
 *   traces: every SQL statement bound a caller's own values under that caller's trace;
 * - a legacy frame with no trace properties, sent straight to a reader, runs under the reader's
 *   nonempty `reader-<pid>-<id>` fallback.
 *
 * Cancellation and late replies over the IPC connection are `tests/unit/ipc_test.ts`'s; the in-process
 * handler/storage cases are `tests/unit/trace_test.ts`'s.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import { Effect, Either } from "effect";
import type { RemoteError } from "../../core/errors.ts";
import { createLegacyClient } from "../../sdk/client.ts";
import type { functions } from "../../app/functions.ts";

const repoRoot = new URL("../../", import.meta.url);
const APP = { id: "tic-tac-toe", token: "columns-token" } as const;
const TOKEN_SHAPE = /^[A-Za-z0-9._:-]{1,128}$/;

type LogRecord = Record<string, unknown> & {
  readonly kind: string;
  readonly role: string;
  readonly traceId: string;
  readonly spanId?: string;
  readonly parentSpanId?: string;
};

function freePort(): number {
  const probe = Deno.listen({ hostname: "127.0.0.1", port: 0 });
  const port = (probe.addr as Deno.NetAddr).port;
  probe.close();
  return port;
}

/** Every JSONL record under `dir`. */
async function records(dir: string): Promise<LogRecord[]> {
  const out: LogRecord[] = [];
  const walk = async (at: string): Promise<void> => {
    for await (const entry of Deno.readDir(at)) {
      const path = join(at, entry.name);
      if (entry.isDirectory) await walk(path);
      else if (entry.name.endsWith(".jsonl")) {
        for (const line of (await Deno.readTextFile(path)).split("\n")) if (line) out.push(JSON.parse(line));
      }
    }
  };
  await walk(dir);
  return out;
}

/** One request the SDK made, as the `fetch` spy saw it. */
interface Exchange {
  readonly path: string;
  readonly sent: string | null;
  readonly echoed: string | null;
  readonly status: number;
}

/** Wraps `globalThis.fetch` for the duration of `body`, recording every exchange. */
async function spyingOnFetch<A>(body: (log: Exchange[]) => Promise<A>): Promise<A> {
  const log: Exchange[] = [];
  const real = globalThis.fetch;
  globalThis.fetch = async (input, init) => {
    const response = await real(input, init);
    log.push({
      path: new URL(String(input)).pathname,
      sent: new Headers(init?.headers).get("x-trace-id"),
      echoed: response.headers.get("x-trace-id"),
      status: response.status,
    });
    return response;
  };
  try {
    return await body(log);
  } finally {
    globalThis.fetch = real;
  }
}

test("end to end: caller traces through the SDK, server and reader child", { timeout: 180_000 }, async () => {
  const root = await Deno.makeTempDir({ prefix: "mansio-trace-e2e-" });
  const port = freePort();
  const base = `http://127.0.0.1:${port}`;
  const env = {
    DATA_DIR: join(root, "data"),
    LOCAL_OBJECT_DIR: join(root, "objects"),
    LOG_DIR: join(root, "logs"),
    EVENTLOG_DUCKDB: join(root, "events.duckdb"),
    EVENT_FLUSH_MS: "50",
    PORT: String(port),
    BIND_HOST: "127.0.0.1",
    ADMIN_TOKEN: "admin-token",
    COLUMN_APP_TOKEN: APP.token,
    JSON_APP_TOKEN: "json-token",
    S3_BASE_URL: "",
    BACKUP_INTERVAL_MS: "0",
    // Successful statements are recorded (document reads record only failures otherwise), with their
    // values, which is what lets the overlap check prove whose statement ran under whose trace.
    TRACE_SQL: "true",
    LOG_INCLUDE_BIND_VALUES: "true",
  };
  const server = new Deno.Command(Deno.execPath(), {
    args: ["run", "-A", "platform/server.ts"],
    cwd: repoRoot,
    env,
    stdout: "null",
    stderr: "piped",
  }).spawn();
  const serverStderr = new Response(server.stderr).text();
  let serverStopped = false;

  try {
    let ready = false;
    for (let attempt = 0; attempt < 600 && !ready; attempt++) {
      try {
        const response = await fetch(`${base}/health`, { signal: AbortSignal.timeout(500) });
        await response.body?.cancel();
        ready = response.ok;
      } catch { /* starting */ }
      if (!ready) await new Promise((resolve) => setTimeout(resolve, 100));
    }
    assert.ok(ready, "server never became ready");

    const client = createLegacyClient<typeof functions>(base, APP.id, APP.token);
    const run = <A>(effect: Effect.Effect<A, RemoteError>) => Effect.runPromise(Effect.either(effect));
    const explicit = `e2e-explicit-${crypto.randomUUID()}`;
    const rollback = `e2e-rollback-${crypto.randomUUID()}`;
    const readerFailure = `e2e-reader-error-${crypto.randomUUID()}`;
    const callers = ["A", "B"].map((name) => ({
      name,
      traceId: `e2e-overlap-${name}-${crypto.randomUUID()}`,
    }));

    const outcome = await spyingOnFetch(async (exchanges) => {
      // One explicit trace across a mutation and a query (the run's first query, see below).
      const created = await run(client.mutation("createGame", { xPlayer: "xe", oPlayer: "oe" }, explicit));
      assert.ok(Either.isRight(created), String(created));
      const gameId = created.right.gameId;
      const fetched = await run(client.query("getGame", { gameId }, explicit));
      assert.ok(Either.isRight(fetched) && fetched.right?._id === gameId);

      // No trace argument: the SDK generates one, and the server keeps it.
      const generated = await run(client.mutation("createGame", { xPlayer: "xg", oPlayer: "og" }));
      assert.ok(Either.isRight(generated));
      // An invalid trace argument: sent verbatim, replaced at ingress, the replacement echoed.
      const invalid = await run(client.query("listGames", { status: "playing" }, "not a valid trace"));
      assert.ok(Either.isRight(invalid));

      // A rolled-back mutation (stale version) and a document-query failure (invalid args).
      const conflict = await run(
        client.mutation("makeMove", { gameId, playerId: "xe", cell: 0, expectedVersion: 999 }, rollback),
      );
      assert.ok(Either.isLeft(conflict) && conflict.left.status === 409, String(conflict));
      const rejected = await run(
        client.query("getGame", { gameId: 7 } as unknown as { gameId: string }, readerFailure),
      );
      assert.ok(Either.isLeft(rejected) && rejected.left.status === 400, String(rejected));

      // Two callers overlapping: each creates its own game, then interleaves reads of it and more
      // writes, all at once, with independent document-read connections.
      const games = await Promise.all(callers.map(async (caller) => {
        const made = await run(
          client.mutation("createGame", { xPlayer: `x-${caller.name}`, oPlayer: "o" }, caller.traceId),
        );
        assert.ok(Either.isRight(made));
        return made.right.gameId;
      }));
      const burst = await Promise.all(
        callers.flatMap((caller, c) =>
          Array.from({ length: 12 }, (_, i) =>
            i % 3 === 2
              ? run(
                client.mutation(
                  "createGame",
                  { xPlayer: `x-${caller.name}-${i}`, oPlayer: "o" },
                  caller.traceId,
                ),
              )
              : run(client.query("getGame", { gameId: games[c] }, caller.traceId)).then((answer) => {
                assert.ok(
                  Either.isRight(answer) && answer.right?._id === games[c],
                  "each caller reads its own game",
                );
                return answer;
              }))
        ),
      );
      assert.ok(burst.every((answer) => answer._tag === "Right"));
      return { gameId, games, exchanges: [...exchanges] };
    });

    // --- the echo, as the SDK's requests saw it --------------------------------------------------
    const { exchanges } = outcome;
    const byTraceSent = (sent: string) => exchanges.filter((exchange) => exchange.sent === sent);
    for (const traceId of [explicit, rollback, readerFailure, ...callers.map((c) => c.traceId)]) {
      const seen = byTraceSent(traceId);
      assert.ok(seen.length > 0);
      for (const exchange of seen) {
        assert.equal(exchange.echoed, traceId, `${exchange.path} echoes ${traceId}`);
      }
    }
    assert.deepEqual(byTraceSent(rollback).map((e) => e.status), [409]);
    assert.deepEqual(byTraceSent(readerFailure).map((e) => e.status), [400]);
    const generatedExchange = exchanges[2];
    assert.ok(
      generatedExchange.sent !== null && TOKEN_SHAPE.test(generatedExchange.sent),
      "the SDK generated an id",
    );
    assert.equal(generatedExchange.echoed, generatedExchange.sent);
    const invalidExchange = exchanges[3];
    assert.equal(invalidExchange.sent, "not a valid trace");
    assert.ok(invalidExchange.echoed !== null && TOKEN_SHAPE.test(invalidExchange.echoed));
    assert.notEqual(invalidExchange.echoed, invalidExchange.sent);

    // --- stop the server, so both processes' logs are drained and closed ---------------------------
    server.kill("SIGTERM");
    const status = await server.status;
    serverStopped = true;
    assert.equal(status.code, 0, await serverStderr);
    const serverLog = await records(join(env.LOG_DIR, "server"));
    const readerLog = await records(join(env.LOG_DIR, "reader"));
    const inTrace = (log: LogRecord[], traceId: string, kind?: string) =>
      log.filter((record) => record.traceId === traceId && (kind === undefined || record.kind === kind));

    // --- the explicit trace, in both processes ----------------------------------------------------
    const http = inTrace(serverLog, explicit, "http");
    assert.deepEqual(http.map((r) => [r.name, r.status]), [["createGame", 200], ["getGame", 200]]);
    const handlers = inTrace(serverLog, explicit, "handler");
    assert.deepEqual(handlers.map((r) => [r.handlerKind, r.name, r.outcome]), [
      ["mutation", "createGame", "ok"],
      ["query", "getGame", "ok"],
    ]);
    const [mutationSpan, querySpan] = handlers.map((r) => r.spanId);
    assert.ok(mutationSpan && querySpan && mutationSpan !== querySpan, "each handler is its own span");
    const serverSql = inTrace(serverLog, explicit, "sql");
    const serverTx = inTrace(serverLog, explicit, "tx");
    assert.ok(serverSql.some((r) => r.statementKind === "insert" && r.engine === "sqlite"));
    assert.deepEqual(serverTx.map((r) => [r.engine, r.outcome]), [
      ["sqlite", "commit"],
      ["sqlite", "commit"],
    ]);
    const querySql = serverSql.filter((r) => r.spanId === querySpan);
    assert.ok(querySql.length > 0 && querySql.every((r) => r.engine === "sqlite" && r.appId === APP.id));
    assert.equal(serverTx[0].spanId, mutationSpan);
    assert.equal(serverTx[1].spanId, querySpan);
    assert.equal(inTrace(readerLog, explicit).length, 0, "document queries never reach the child");

    // --- generated and replaced ids -------------------------------------------------------------
    // Both mutations and document queries now emit server-side SQLite events.
    const kindsOf = (traceId: string) =>
      [
        ...new Set(
          [...inTrace(serverLog, traceId), ...inTrace(readerLog, traceId)].map((r) => `${r.role}:${r.kind}`),
        ),
      ]
        .sort();
    assert.deepEqual(kindsOf(generatedExchange.echoed!), [
      "server:handler",
      "server:http",
      "server:sql",
      "server:tx",
    ]);
    assert.deepEqual(kindsOf(invalidExchange.echoed!), [
      "server:handler",
      "server:http",
      "server:sql",
      "server:tx",
    ]);
    assert.ok(
      ![...serverLog, ...readerLog].some((r) => r.traceId === "not a valid trace"),
      "an invalid id is never recorded",
    );

    // --- rollback and a reader error keep the trace -----------------------------------------------
    const rolled = inTrace(serverLog, rollback);
    assert.deepEqual(inTrace(serverLog, rollback, "tx").map((r) => [r.outcome, r.errorTag]), [[
      "rollback",
      "ConflictError",
    ]]);
    assert.deepEqual(inTrace(serverLog, rollback, "handler").map((r) => [r.outcome, r.errorTag]), [[
      "error",
      "ConflictError",
    ]]);
    assert.deepEqual(inTrace(serverLog, rollback, "http").map((r) => [r.status, r.errorTag]), [[
      409,
      "ConflictError",
    ]]);
    assert.ok(rolled.filter((r) => r.kind === "sql").length > 0, "the rolled-back statements are traced");

    assert.deepEqual(inTrace(serverLog, readerFailure, "http").map((r) => [r.status, r.errorTag]), [[
      400,
      "SchemaError",
    ]]);
    assert.deepEqual(inTrace(serverLog, readerFailure, "handler").map((r) => [r.outcome, r.errorTag]), [[
      "error",
      "SchemaError",
    ]]);
    assert.deepEqual(inTrace(serverLog, readerFailure, "tx").map((r) => [r.outcome, r.errorTag]), [[
      "rollback",
      "SchemaError",
    ]]);

    // --- overlapping callers never swap traces ----------------------------------------------------
    callers.forEach((caller, c) => {
      const own = outcome.games[c];
      const other = outcome.games[1 - c];
      const reads = inTrace(serverLog, caller.traceId, "sql").filter((r) => r.statementKind === "select");
      assert.equal(
        inTrace(serverLog, caller.traceId, "tx").length,
        13,
        `${caller.name}'s 8 queries and 5 mutations ran locally`,
      );
      assert.equal(reads.length, 8);
      for (const read of reads) {
        const values = JSON.stringify(read.bindValues);
        assert.ok(
          values.includes(own) && !values.includes(other),
          `${caller.name}'s reads bind only its own game`,
        );
      }
      const writes = inTrace(serverLog, caller.traceId, "sql").filter((r) => r.statementKind === "insert");
      assert.equal(writes.length, 5);
      for (const write of writes) {
        assert.match(JSON.stringify(write.bindValues), new RegExp(`"x-${caller.name}(-\\d+)?"`));
      }
      const spans = new Set(inTrace(serverLog, caller.traceId, "handler").map((r) => r.spanId));
      assert.equal(spans.size, 13, "every request of a caller is its own handler span");
    });

    // --- a legacy frame, straight to a reader ----------------------------------------------------
    const reader = new Deno.Command(Deno.execPath(), {
      args: ["run", "-A", "platform/reader.ts"],
      cwd: repoRoot,
      env,
      stdin: "piped",
      stdout: "piped",
      stderr: "piped",
    }).spawn();
    const legacy = [
      { id: 1, kind: "query", appId: APP.id, name: "getGame", args: { gameId: outcome.gameId } },
      // A span with no trace cannot be placed: still the fallback.
      {
        id: 2,
        kind: "query",
        appId: APP.id,
        name: "getGame",
        args: { gameId: outcome.gameId },
        spanId: "orphan",
      },
    ];
    const input = reader.stdin.getWriter();
    await input.write(new TextEncoder().encode(legacy.map((frame) => JSON.stringify(frame) + "\n").join("")));
    await input.close();
    const [{ code }, stdout, stderr] = await Promise.all([
      reader.status,
      new Response(reader.stdout).text(),
      new Response(reader.stderr).text(),
    ]);
    assert.equal(code, 0, stderr);
    const replies = stdout.trim().split("\n").map((line) => JSON.parse(line));
    assert.deepEqual(replies.map((reply) => [reply.id, reply.result?._id]), [[1, outcome.gameId], [
      2,
      outcome.gameId,
    ]]);
    const fallbackTx = (await records(join(env.LOG_DIR, "reader")))
      .filter((r) => r.kind === "tx" && r.pid === reader.pid)
      .map((r) => r.traceId);
    assert.deepEqual(fallbackTx, [`reader-${reader.pid}-1`, `reader-${reader.pid}-2`]);
  } finally {
    if (!serverStopped) {
      try {
        server.kill("SIGKILL");
      } catch { /* already exited */ }
      await server.status;
    }
    await serverStderr;
    await Deno.remove(root, { recursive: true });
  }
});
