/**
 * Real-process integration checks (plan item 25), run separately from the unit suites:
 * `deno task integration`.
 *
 * One throwaway server (scratch `DATA_DIR`, `LOG_DIR`, object store and event-log file, free port)
 * is driven over HTTP with the Effect SDK client, then stopped, and everything that can only be
 * read with the server down is checked afterwards:
 *
 * ```text
 * server up    both layouts: read-after-write on the real DuckDB extension, aggregation, backup
 *              checksum; app-file isolation; 401; X-Trace-Id echoed and found in the server JSONL;
 *              POST /admin/logs/ingest
 * server down  restore checksum; the event-log DuckDB holds the driven requests; a named analysis
 *              query (item 23) names the handlers
 * ```
 *
 * Then the FFI-06 deadline scenario (`deadlineScenario`), twice, each on its own scratch server and a
 * private Postgres database (`npm:postgres`, `DATABASE_URL`) with `STATEMENT_DEADLINE_MS=50`: two
 * API-key tenants, one with 500,000 JSON games so `winsByMark`'s grouped count outruns the deadline
 * through the nonblocking binding path (`platform/sqlite_ffi`), the other serving primary-key reads.
 * Pass 1 (`TRACE_SQL=true`): 20 deadline windows each answer HTTP 504 `StatementDeadlineError` while the
 * other tenant's point reads, issued sequentially throughout, stay under a flat 50 ms p99 (the load
 * average is printed beside it, not folded into the budget). Pass 2 (`TRACE_SQL=false`, 3 windows) checks
 * only the logging claim in docs/observability.md: the failed statements still produce their `sql`
 * events. After shutdown each pass runs `logs:ingest` and `logs:query deadline-hits-per-handler-hour`
 * and asserts `path = 'nonblocking'` with the hits summed across hour buckets.
 *
 * The event-log file is read only after the server exits because the reader child holds it open,
 * and DuckDB's file lock refuses even a `READ_ONLY` open from another process while it does. It is
 * read from a `deno eval` subprocess (`duckdbRows`), so this process never loads `@duckdb/node-api`
 * beside anything that could load `node:sqlite` (ADR 0001).
 *
 * The scratch-server and DuckDB helpers are exported for `tests/e2e/observability_test.ts`; the
 * checks themselves run only when this module is the entry point.
 */
import assert from "node:assert/strict";
import { resolve } from "node:path";
import { Effect, Exit, Layer, type Scope } from "effect";
import { createLegacyClient } from "../sdk/client.ts";
import type { functions } from "../app/functions.ts";
import type { ResultOf } from "../sdk/functions.ts";
import { sha256 } from "../platform/object_store.ts";
import type { Manifest } from "../platform/backup.ts";
import postgres from "npm:postgres@3.4.9";
import { Database } from "../platform/sqlite_ffi/mod.ts";
import { ConfigTest } from "../platform/config.ts";
import { Analysis, EventSink } from "../platform/services.ts";

/** The plan's flat p99 budget for point reads while another tenant's statement hits its deadline. */
const POINT_P99_BUDGET_MS = 50;

/** The token every scratch server accepts, for apps and admin alike. */
export const DEV_TOKEN = "dev-only";

// ===========================================================================
// Scratch server
// ===========================================================================

export interface ScratchServer {
  readonly base: string;
  /** The environment the server ran with; `scripts/restore.ts` reuses it. */
  readonly env: Readonly<Record<string, string>>;
  readonly logDir: string;
  /** `EVENTLOG_DUCKDB`: the reader child's event-log file. Readable only once `stop` has run. */
  readonly eventLogPath: string;
  readonly objectDir: string;
  /** SIGTERM, then the exit status. Idempotent; the scope's finalizer calls it too. */
  readonly stop: Effect.Effect<Deno.CommandStatus>;
}

/** A free TCP port on the loopback interface (the probe listener is closed before returning). */
function freePort(): number {
  const probe = Deno.listen({ hostname: "127.0.0.1", port: 0 });
  const port = (probe.addr as Deno.NetAddr).port;
  probe.close();
  return port;
}

/**
 * Starts `platform/server.ts` over scratch directories under `root` and waits for `/health`.
 *
 * Every path is set explicitly, so nothing lands in the repo's `./data` or `./logs`. `extraEnv` is
 * applied last; the child inherits the rest of this process's environment.
 */
export function startScratchServer(
  root: string,
  extraEnv: Readonly<Record<string, string>> = {},
): Effect.Effect<ScratchServer, never, Scope.Scope> {
  return Effect.gen(function* () {
    // DuckDB's `INSTALL sqlite` in the reader child resolves `~/.duckdb`; with `HOME` unset every
    // query answers 500 ("Can't find the home directory at ''"), which reads like a platform bug.
    assert.ok(Deno.env.get("HOME"), "HOME is unset: the reader's DuckDB cannot install extensions");
    const port = freePort();
    const env = {
      DATA_DIR: resolve(root, "data"),
      LOG_DIR: resolve(root, "logs"),
      EVENTLOG_DUCKDB: resolve(root, "eventlog", "eventlog.duckdb"),
      LOCAL_OBJECT_DIR: resolve(root, "objects"),
      PORT: String(port),
      COLUMN_APP_TOKEN: DEV_TOKEN,
      JSON_APP_TOKEN: DEV_TOKEN,
      ADMIN_TOKEN: DEV_TOKEN,
      S3_BASE_URL: "",
      BACKUP_INTERVAL_MS: "0",
      ...extraEnv,
    };
    const base = `http://127.0.0.1:${port}`;
    const child = yield* Effect.acquireRelease(
      Effect.sync(() =>
        new Deno.Command(Deno.execPath(), {
          args: ["run", "-A", "platform/server.ts"],
          env,
          stdout: "null",
          stderr: "inherit",
        }).spawn()
      ),
      (child) => stopChild(child),
    );
    let exited = false;
    void child.status.then(() => {
      exited = true;
    });
    const healthy = Effect.promise(() =>
      fetch(`${base}/health`, { signal: AbortSignal.timeout(500) }).then(
        async (response) => {
          await response.body?.cancel();
          return response.ok;
        },
        () => false,
      )
    );
    let ready = false;
    for (let attempt = 0; attempt < 1200 && !ready; attempt++) {
      assert.ok(!exited, "Server exited before becoming ready");
      ready = yield* healthy;
      if (!ready) yield* Effect.sleep("100 millis");
    }
    assert.ok(ready, "Server never became ready");
    return {
      base,
      env,
      logDir: env.LOG_DIR,
      eventLogPath: env.EVENTLOG_DUCKDB,
      objectDir: env.LOCAL_OBJECT_DIR,
      stop: stopChild(child),
    };
  });
}

function stopChild(child: Deno.ChildProcess): Effect.Effect<Deno.CommandStatus> {
  return Effect.promise(async () => {
    try {
      child.kill("SIGTERM");
    } catch { /* already exited */ }
    return await child.status;
  });
}

/** A scratch directory for one run: removed on success, kept (and named on stderr) on failure. */
export function scratchRoot(prefix: string): Effect.Effect<string, never, Scope.Scope> {
  return Effect.acquireRelease(
    Effect.promise(() => Deno.makeTempDir({ prefix })),
    (root, exit) =>
      Exit.isSuccess(exit)
        ? Effect.promise(() => Deno.remove(root, { recursive: true }))
        : Effect.sync(() => console.error(`scratch kept for diagnosis: ${root}`)),
  );
}

/** `POST <base><path>` with the admin bearer token; the status and the parsed JSON body. */
export function adminPost(
  base: string,
  path: string,
): Effect.Effect<{ status: number; body: { result?: unknown; error?: unknown } }> {
  return Effect.promise(async () => {
    const response = await fetch(`${base}${path}`, {
      method: "POST",
      headers: { authorization: `Bearer ${DEV_TOKEN}` },
    });
    return { status: response.status, body: await response.json() };
  });
}

// ===========================================================================
// The server's JSONL log
// ===========================================================================

/** Every record in `<logDir>/server/<date>/events-*.jsonl`, parsed. Partial last lines are skipped. */
export async function serverLogRecords(logDir: string): Promise<Record<string, unknown>[]> {
  const records: Record<string, unknown>[] = [];
  const roleDir = resolve(logDir, "server");
  let dates: Deno.DirEntry[];
  try {
    dates = await Array.fromAsync(Deno.readDir(roleDir));
  } catch {
    return records;
  }
  for (const date of dates) {
    if (!date.isDirectory) continue;
    for await (const file of Deno.readDir(resolve(roleDir, date.name))) {
      if (!/^events-.*\.jsonl$/.test(file.name)) continue;
      const text = await Deno.readTextFile(resolve(roleDir, date.name, file.name));
      for (const line of text.split("\n")) {
        try {
          records.push(JSON.parse(line));
        } catch { /* empty, or still being written */ }
      }
    }
  }
  return records;
}

/**
 * Polls the server's JSONL until every id in `traceIds` appears as a record's `traceId`, or the
 * timeout passes. Returns the ids still missing (empty on success): the sink writes from a batching
 * fiber, so a record lands up to one flush period after the response.
 */
export function awaitTraceIdsInLog(
  logDir: string,
  traceIds: Iterable<string>,
  timeoutMs = 10_000,
): Effect.Effect<string[]> {
  return Effect.gen(function* () {
    const wanted = new Set(traceIds);
    const deadline = Date.now() + timeoutMs;
    while (true) {
      const seen = new Set(
        (yield* Effect.promise(() => serverLogRecords(logDir))).map((record) => record.traceId),
      );
      const missing = [...wanted].filter((id) => !seen.has(id));
      if (missing.length === 0 || Date.now() > deadline) return missing;
      yield* Effect.sleep("100 millis");
    }
  });
}

// ===========================================================================
// DuckDB, out of process
// ===========================================================================

/** Opens `DUCK_PATH` read-only, attaches `DUCK_ATTACH` (if any) read-only as `events`, prints JSON rows. */
const DUCKDB_QUERY_PROGRAM = `
import { DuckDBInstance } from "@duckdb/node-api";
const instance = await DuckDBInstance.create(Deno.env.get("DUCK_PATH"), { access_mode: "READ_ONLY" });
const connection = await instance.connect();
await connection.run("SET TimeZone = 'UTC'");
const attach = Deno.env.get("DUCK_ATTACH");
if (attach) await connection.run("ATTACH '" + attach.replaceAll("'", "''") + "' AS events (READ_ONLY)");
const rows = (await connection.runAndReadAll(Deno.env.get("DUCK_SQL"))).getRowObjectsJson();
console.log(JSON.stringify(rows));
connection.closeSync();
instance.closeSync();
`;

/**
 * Runs one read-only `sql` against the DuckDB file at `dbPath` in a separate `deno eval` process,
 * with `attachPath` (if given) attached read-only as `events`. Values come back JSON-shaped:
 * `BIGINT` counts as strings, timestamps as ISO-like strings.
 */
export function duckdbRows(
  dbPath: string,
  sql: string,
  attachPath?: string,
): Effect.Effect<Record<string, unknown>[]> {
  return Effect.promise(async () => {
    const output = await new Deno.Command(Deno.execPath(), {
      args: ["eval", "--ext=ts", DUCKDB_QUERY_PROGRAM],
      env: { DUCK_PATH: dbPath, DUCK_SQL: sql, ...(attachPath ? { DUCK_ATTACH: attachPath } : {}) },
      stdout: "piped",
      stderr: "piped",
    }).output();
    const decode = (bytes: Uint8Array) => new TextDecoder().decode(bytes);
    assert.ok(output.success, `DuckDB query failed on ${dbPath}: ${decode(output.stderr)}\n${sql}`);
    return JSON.parse(decode(output.stdout)) as Record<string, unknown>[];
  });
}

/** A string as a DuckDB SQL literal. */
export function sqlString(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

// ===========================================================================
// Item 23's analysis catalog, when it exists
// ===========================================================================

const ANALYSIS_CATALOG = new URL("../platform/analysis/catalog.ts", import.meta.url);

/**
 * Runs `AnalysisLive.run(name, params)` from item 23's `platform/analysis/catalog.ts` over the
 * event-log file, or returns `undefined` when that module does not exist yet. The import specifier
 * is a variable so this file type-checks without it; once the module exists, any failure to load or
 * run it fails the test rather than skipping.
 */
function runNamedAnalysis(
  server: ScratchServer,
  name: string,
  params: Record<string, unknown>,
): Effect.Effect<readonly Record<string, unknown>[] | undefined> {
  return Effect.gen(function* () {
    const present = yield* Effect.promise(() => Deno.stat(ANALYSIS_CATALOG).then(() => true, () => false));
    if (!present) return undefined;
    const catalog = yield* Effect.promise(() => import(ANALYSIS_CATALOG.href));
    const live = catalog.AnalysisLive;
    const config = ConfigTest({
      server: { dataDir: server.env.DATA_DIR },
      logging: { dir: server.logDir },
      eventLog: { duckdbPath: server.eventLogPath },
    });
    const sink = Layer.succeed(EventSink, { emit: () => Effect.void });
    // `AnalysisLive` per item 23's interface: a layer for the `Analysis` service (`run(name, params)`),
    // or the service value itself.
    if (Layer.isLayer(live)) {
      return yield* Effect.orDie(
        Effect.scoped(Effect.flatMap(Analysis, (analysis) => analysis.run(name, params))).pipe(
          Effect.provide(Layer.provide(live as Layer.Layer<Analysis>, Layer.merge(config, sink))),
        ),
      );
    }
    return yield* Effect.orDie(
      (live.run(name, params) as Effect.Effect<readonly Record<string, unknown>[], unknown, never>).pipe(
        Effect.provide(Layer.merge(config, sink)),
      ),
    );
  });
}

// ===========================================================================
// The checks
// ===========================================================================

const APP_IDS = ["tic-tac-toe", "tic-tac-toe-json"] as const;
const HANDLER_NAMES = ["createGame", "getGame", "makeMove", "winsByMark", "moveHistory", "listGames"];

const program = Effect.gen(function* () {
  const root = yield* scratchRoot("document-db-integration-");
  const runTag = crypto.randomUUID().slice(0, 8);
  const sentTraceIds: string[] = [];
  /** Every SDK call carries one of these, so each request can be found in the log by id. */
  const nextTraceId = () => {
    const id = `integration-${runTag}-${sentTraceIds.length}`;
    sentTraceIds.push(id);
    return id;
  };
  const snapshots: Manifest[] = [];

  // --- server up -----------------------------------------------------------
  const { server, ingested } = yield* Effect.scoped(Effect.gen(function* () {
    const server = yield* startScratchServer(root);
    const { base } = server;
    const games: Record<string, string> = {};
    for (const appId of APP_IDS) {
      const client = createLegacyClient<typeof functions>(base, appId, DEV_TOKEN, nextTraceId);
      const { gameId } = yield* client.mutation("createGame", { xPlayer: "alice", oPlayer: "bob" });
      games[appId] = gameId;
      let game = yield* client.query("getGame", { gameId });
      assert.equal(game!._version, 1);
      assert.equal(typeof game!._creationTime, "number");
      for (
        const [playerId, cell] of [["alice", 0], ["bob", 3], ["alice", 1], ["bob", 4], ["alice", 2]] as const
      ) {
        const written: ResultOf<typeof functions["makeMove"]> = yield* client.mutation("makeMove", {
          gameId,
          playerId,
          cell,
          expectedVersion: game!._version,
        });
        game = yield* client.query("getGame", { gameId });
        assert.deepEqual(game, written, "DuckDB must see the acknowledged SQLite commit");
      }
      assert.equal(game!.winner, "X");
      assert.deepEqual(yield* client.query("winsByMark", {}), [{ value: "X", count: 1 }]);
      assert.equal((yield* client.query("moveHistory", { gameId })).length, 5);
      assert.equal((yield* client.query("listGames", { status: "won" })).length, 1);

      const { status, body } = yield* adminPost(base, `/admin/backup/${appId}`);
      assert.equal(status, 200, JSON.stringify(body));
      const manifest = body.result as Manifest;
      const snapshot = yield* Effect.promise(() => Deno.readFile(resolve(server.objectDir, manifest.key)));
      assert.equal(yield* Effect.promise(() => sha256(snapshot)), manifest.sha256);
      snapshots.push(manifest);
    }
    const client = createLegacyClient<typeof functions>(base, "tic-tac-toe-json", DEV_TOKEN, nextTraceId);
    assert.equal(
      yield* client.query("getGame", { gameId: games["tic-tac-toe"] }),
      null,
      "Files must be app-scoped",
    );

    // 401, with a caller trace id: the id is echoed on a refusal too, and the refusal is logged.
    const refusedTraceId = nextTraceId();
    const refused = yield* Effect.promise(() =>
      fetch(`${base}/apps/tic-tac-toe/query/listGames`, {
        method: "POST",
        headers: { "x-trace-id": refusedTraceId },
        body: JSON.stringify({ status: "won" }),
      })
    );
    yield* Effect.promise(async () => await refused.body?.cancel());
    assert.equal(refused.status, 401);
    assert.equal(refused.headers.get("x-trace-id"), refusedTraceId, "X-Trace-Id must be echoed");

    // X-Trace-Id in JSONL: every id this run sent is in the server's log.
    const missing = yield* awaitTraceIdsInLog(server.logDir, sentTraceIds);
    assert.deepEqual(missing, [], `trace ids never reached ${server.logDir}/server/**/events-*.jsonl`);

    // Ingest runs in the reader child; the file is checked once the server is down.
    const ingest = yield* adminPost(base, "/admin/logs/ingest");
    assert.equal(ingest.status, 200, JSON.stringify(ingest.body));
    const ingested = (ingest.body.result as { linesIngested?: number }).linesIngested ?? 0;
    assert.ok(ingested > 0, `ingest reported no lines: ${JSON.stringify(ingest.body)}`);

    const stopped = yield* server.stop;
    assert.equal(stopped.code, 0, "SIGTERM must stop the server cleanly");
    return { server, ingested };
  }));

  // --- server down ---------------------------------------------------------
  for (const manifest of snapshots) {
    const target = resolve(root, "restored", `${manifest.appId}.sqlite`);
    const restore = yield* Effect.promise(() =>
      new Deno.Command(Deno.execPath(), {
        args: ["run", "-A", "scripts/restore.ts", manifest.appId, target],
        env: server.env,
      }).output()
    );
    assert.ok(restore.success, new TextDecoder().decode(restore.stderr));
    assert.equal(yield* Effect.promise(async () => sha256(await Deno.readFile(target))), manifest.sha256);
  }

  // Logs-ingest populates DuckDB: every request this run sent is an `http` row and a `platform_events`
  // row, and every app × handler pair ran is a `handler` row.
  const ids = sentTraceIds.map(sqlString).join(", ");
  const httpTraces = yield* duckdbRows(
    server.eventLogPath,
    `SELECT DISTINCT trace_id FROM http_events WHERE trace_id IN (${ids})`,
  );
  assert.deepEqual(
    httpTraces.map((row) => row.trace_id).sort(),
    [...sentTraceIds].sort(),
    "every request must be an http_events row after ingest",
  );
  const handlers = yield* duckdbRows(
    server.eventLogPath,
    `SELECT DISTINCT app_id, name FROM platform_events
     WHERE event_kind = 'handler' AND role = 'server' AND trace_id IN (${ids})`,
  );
  assert.deepEqual(
    handlers.map((row) => `${row.app_id}/${row.name}`).sort(),
    APP_IDS.flatMap((appId) => HANDLER_NAMES.map((name) => `${appId}/${name}`)).sort(),
    "every app handler driven must be a handler row in platform_events",
  );

  // A named analysis query names the handlers (item 23; skipped until its catalog exists).
  const analysis = yield* runNamedAnalysis(server, "error-rate-by-handler", {});
  let namedQuery: string;
  if (analysis === undefined) {
    console.log("SKIP (item 23 pending): named analysis query — platform/analysis/catalog.ts is absent");
    namedQuery = "named-query handlers SKIPPED (item 23 pending)";
  } else {
    const values = new Set(analysis.flatMap((row) => Object.values(row)).map(String));
    const absent = HANDLER_NAMES.filter((name) => !values.has(name));
    assert.deepEqual(absent, [], `error-rate-by-handler rows: ${JSON.stringify(analysis)}`);
    namedQuery = "error-rate-by-handler handlers";
  }

  console.log(
    `PASS: both layouts, DuckDB read-after-write, aggregation, app isolation, backup, restore, HTTP auth, ` +
      `X-Trace-Id in JSONL (${sentTraceIds.length} ids), logs-ingest populates DuckDB (${ingested} lines), ` +
      namedQuery,
  );
});

// FFI-06: real HTTP, two API-key tenants, and the production event-log projection.
// A private Postgres database avoids mutating the dev tenant registry used by the unit suite.
// `traceSql` sets TRACE_SQL for the server; `measure` runs the point-read competition and its p99 gate.
interface DeadlinePass {
  readonly label: string;
  readonly traceSql: boolean;
  readonly rounds: number;
  readonly measure: boolean;
}
const deadlineScenario = (pass: DeadlinePass) =>
  Effect.gen(function* () {
    const root = yield* scratchRoot("ffi06-integration-");
    const databaseName = "ffi06_" + crypto.randomUUID().replaceAll("-", "");
    const sourceUrl = Deno.env.get("DATABASE_URL") ?? "postgres://mansio:mansio@localhost:5432/mansio";
    const admin = yield* Effect.acquireRelease(
      Effect.sync(() => postgres(sourceUrl, { max: 1, onnotice() {} })),
      (sql) => Effect.promise(() => sql.end()),
    );
    yield* Effect.acquireRelease(
      Effect.promise(() => admin.unsafe(`CREATE DATABASE "${databaseName}"`)),
      () => Effect.promise(() => admin.unsafe(`DROP DATABASE "${databaseName}"`)),
    );
    const url = new URL(sourceUrl);
    url.pathname = "/" + databaseName;
    const migrated = yield* Effect.promise(() =>
      new Deno.Command(Deno.execPath(), {
        args: ["run", "-A", "scripts/db/migrate.ts"],
        env: { DATABASE_URL: url.href },
        stdout: "piped",
        stderr: "piped",
      }).output()
    );
    assert.ok(migrated.success, new TextDecoder().decode(migrated.stderr));
    const server = yield* startScratchServer(root, {
      TENANCY: "api-key",
      DATABASE_URL: url.href,
      TENANTS_DIR: resolve(root, "tenants"),
      STATEMENT_DEADLINE_MS: "50",
      READER_CALL_TIMEOUT_MS: "10000",
      TRACE_SQL: String(pass.traceSql),
    });
    const request = async (
      path: string,
      body: unknown,
      token = DEV_TOKEN,
      traceId: string = crypto.randomUUID(),
    ) => {
      const response = await fetch(server.base + path, {
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "content-type": "application/json",
          "x-trace-id": traceId,
        },
        body: JSON.stringify(body),
      });
      return { status: response.status, body: await response.json() };
    };
    const tenants = yield* Effect.promise(async () => {
      const result: { id: string; key: string }[] = [];
      for (const name of ["big", "small"]) {
        const created = await request("/admin/tenants", { name, slug: name });
        assert.equal(created.status, 200, JSON.stringify(created.body));
        result.push({ id: created.body.result.tenant.id, key: created.body.result.apiKey.plaintext });
      }
      return result;
    });
    // Initialize each tenant through the real router before seeding its own file through the binding.
    yield* Effect.promise(async () => {
      for (const tenant of tenants) {
        const created = await request("/apps/tic-tac-toe-json/mutation/createGame", {
          xPlayer: "alice",
          oPlayer: "bob",
        }, tenant.key);
        assert.equal(created.status, 200, JSON.stringify(created.body));
      }
      const db = Database.open(resolve(root, "tenants", tenants[0].id + ".db"));
      try {
        const doc = JSON.stringify({
          xPlayer: "alice",
          oPlayer: "bob",
          board: Array(9).fill(null),
          turn: "X",
          status: "won",
          winner: "X",
        });
        // countBy traverses every matching JSON doc; ample margin above the 50 ms deadline.
        db.prepare(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < 499999)
        INSERT INTO games (_id, _creationTime, _version, doc)
        SELECT 'seed-' || i, i, 1, ? FROM n`).run([doc]);
      } finally {
        db.close();
      }
    });
    const smallGame = yield* Effect.promise(() =>
      request(
        "/apps/tic-tac-toe-json/mutation/createGame",
        { xPlayer: "alice", oPlayer: "bob" },
        tenants[1].key,
      )
    );
    assert.equal(smallGame.status, 200);
    const pointBody = { gameId: smallGame.body.result.gameId };
    // Warm connection/library/JIT/auth-cache paths before measuring.
    for (let i = 0; i < 5; i++) {
      const warm = yield* Effect.promise(() =>
        request("/apps/tic-tac-toe-json/query/getGame", pointBody, tenants[1].key)
      );
      assert.equal(warm.status, 200);
    }
    const loadavg = Deno.loadavg();
    const traceIds: string[] = [];
    const evidence = yield* Effect.promise(async () => {
      const pointMs: number[] = [];
      let overlap = 0;
      // Twenty separate deadline windows produce enough point reads for a meaningful p99.
      for (let round = 0; round < pass.rounds; round++) {
        const traceId = "ffi06-deadline-" + crypto.randomUUID();
        traceIds.push(traceId);
        let finished = false;
        const slow = request("/apps/tic-tac-toe-json/query/winsByMark", {}, tenants[0].key, traceId)
          .finally(() => {
            finished = true;
          });
        await new Promise((r) => setTimeout(r, 10));
        while (pass.measure && !finished) {
          const t = performance.now();
          const fast = await request("/apps/tic-tac-toe-json/query/getGame", pointBody, tenants[1].key);
          const ms = performance.now() - t;
          assert.equal(fast.status, 200, JSON.stringify(fast.body));
          assert.equal(fast.body.result._id, pointBody.gameId);
          // Include every read started during the competing call, even if it finishes after it.
          pointMs.push(ms);
          if (!finished) overlap++;
        }
        const response = await slow;
        assert.equal(response.status, 504, JSON.stringify(response.body));
        assert.equal(response.body.error.tag, "StatementDeadlineError");
      }
      if (!pass.measure) return { rounds: pass.rounds, loadavg };
      assert.ok(overlap >= 50, `only ${overlap} overlapping point reads`);
      pointMs.sort((a, b) => a - b);
      const p99 = pointMs[Math.ceil(pointMs.length * 0.99) - 1];
      // The plan's flat budget; the load average is printed beside it, not folded into it.
      assert.ok(
        p99 < POINT_P99_BUDGET_MS,
        `point p99 ${p99} ms, budget ${POINT_P99_BUDGET_MS}, load ${loadavg}`,
      );
      return { point_reads: pointMs.length, point_p99_ms: p99, budget_ms: POINT_P99_BUDGET_MS, loadavg };
    });
    assert.deepEqual(yield* awaitTraceIdsInLog(server.logDir, traceIds), []);
    // Ingest after shutdown captures both server and reader logs and exercises the operator CLI.
    assert.equal((yield* server.stop).code, 0);
    const ingest = yield* Effect.promise(() =>
      new Deno.Command(Deno.execPath(), {
        args: ["task", "logs:ingest"],
        env: server.env,
        stdout: "piped",
        stderr: "piped",
      }).output()
    );
    assert.ok(ingest.success, new TextDecoder().decode(ingest.stderr));
    const rows = yield* duckdbRows(
      server.eventLogPath,
      `SELECT path, error_tag, count(*) AS hits FROM sql_events
     WHERE trace_id IN (${traceIds.map(sqlString).join(",")}) GROUP BY path, error_tag`,
    );
    // With TRACE_SQL unset these are the only sql events for the traces: failed statements are always logged.
    assert.deepEqual(rows, [{
      path: "nonblocking",
      error_tag: "StatementDeadlineError",
      hits: String(pass.rounds),
    }]);
    const query = yield* Effect.promise(() =>
      new Deno.Command(Deno.execPath(), {
        args: ["task", "logs:query", "deadline-hits-per-handler-hour", "--format", "json"],
        env: server.env,
        stdout: "piped",
        stderr: "piped",
      }).output()
    );
    assert.ok(query.success, new TextDecoder().decode(query.stderr));
    const projected = JSON.parse(new TextDecoder().decode(query.stdout)) as Record<string, unknown>[];
    // Sum across hour buckets: the rounds may straddle an hour boundary and split the rows.
    const projectedHits = projected
      .filter((r) => r.path === "nonblocking" && r.name === "winsByMark" && r.app_id === "tic-tac-toe-json")
      .reduce((n, r) => n + Number(r.deadline_hits), 0);
    assert.equal(projectedHits, pass.rounds, JSON.stringify(projected));
    console.log(`PASS FFI-06 deadline HTTP 504 / two tenants (${pass.label}): ${JSON.stringify(evidence)}`);
    console.log("logs:ingest " + new TextDecoder().decode(ingest.stdout).trim());
    console.log("logs:query " + JSON.stringify(projected));
  });

if (import.meta.main) {
  await Effect.runPromise(Effect.scoped(program));
  await Effect.runPromise(Effect.scoped(deadlineScenario({
    label: "TRACE_SQL=true, point-read p99 gate",
    traceSql: true,
    rounds: 20,
    measure: true,
  })));
  // The documented guarantee that failed statements are logged without TRACE_SQL (platform/document_reader.ts).
  await Effect.runPromise(Effect.scoped(deadlineScenario({
    label: "TRACE_SQL=false, failed-statement logging only",
    traceSql: false,
    rounds: 3,
    measure: false,
  })));
}
