import {
  apps,
  assert,
  cleanEnv,
  cli,
  client,
  delay,
  Effect,
  play,
  remoteError,
  run,
  scenarioDir,
  type Server,
  startGoAgent,
  TENANT,
  test,
} from "../harness.ts";
import { createClient } from "../../../../sdk/client.ts";
import { withStorage } from "../../../../sdk/schema.ts";
import { functions } from "../../../../app/functions.ts";
import { schema } from "../../../../app/schema.ts";
import { moves, names } from "../seed.ts";
type Row = Record<string, unknown>;
const stable = (rows: Row[]) => rows.map((r) => JSON.stringify(r)).sort();

/** One catalog query: the offline CLI over agentd's JSONL. */
type Analysis = (name: string, params?: Row) => Promise<Row[]>;

/** What a scenario's workload must leave in the event log. */
interface Workload {
  /** The traced document read (`getGame`). */
  readonly traceId: string;
  /** The handlers that ran (http_events names). */
  readonly handlers: readonly string[];
  /** (handler, error tag) pairs that failed at the server. */
  readonly errors: readonly (readonly [string, string])[];
  /** Whether a backup ran (backup-history). */
  readonly backups: boolean;
}

/**
 * The analysis checks. Polls `ingest` and the catalog until the asynchronously flushed
 * events are all in, checks the traced read's timeline, and runs every catalog query: the catalog answers
 * the expected shapes. Returns `handler-throughput` for `apps[0]`.
 */
async function analyse(
  ingest: () => Promise<unknown>,
  analysis: Analysis,
  catalog: readonly string[],
  workload: Workload,
) {
  const deadline = Date.now() + 15_000;
  let trace: Row[] = [], throughput: Row[] = [], errors: Row[] = [], slow: Row[] = [], tx: Row[] = [];
  let backups: Row[] = [];
  const complete = () =>
    trace.some((r) => r.source === "http" && r.name === "getGame") &&
    trace.some((r) => r.source === "sql" && r.role === "server") &&
    workload.handlers.every((name) => throughput.some((r) => r.name === name)) &&
    workload.errors.every(([name, tag]) =>
      errors.some((r) => r.name === name && Number(r.errors) >= 1 && String(r.error_tags).includes(tag)) &&
      throughput.some((r) => r.name === name && Number(r.status_4xx) >= 1)
    ) &&
    slow.length > 0 && tx.length > 0 &&
    (!workload.backups ||
      backups.some((r) => r.app_id === apps[0] && r.phase === "publish-latest" && r.outcome === "ok"));
  while (true) {
    await ingest();
    // Query failures are fatal; only asynchronously flushed evidence is retried.
    trace = await analysis("trace", { traceId: workload.traceId });
    throughput = await analysis("handler-throughput", { appId: apps[0] });
    errors = await analysis("error-rate-by-handler");
    slow = await analysis("slowest-sql");
    tx = await analysis("tx-outcomes");
    if (workload.backups) backups = await analysis("backup-history");
    if (complete()) break;
    assert.ok(
      Date.now() < deadline,
      `Incomplete ingested workload within 15 s: ${
        JSON.stringify({ trace, throughput, errors, slow, tx, backups })
      }`,
    );
    await delay(100);
  }
  assert.deepEqual([...new Set(trace.map((r) => r.role))], ["server"], "document requests are server events");
  assert.ok(
    trace.some((r) => r.source === "tx" && r.role === "server"),
    "the traced read ran in a transaction",
  );
  const times = trace.map((r) => String(r.ts));
  assert.deepEqual(times, [...times].sort(), "trace timeline must be ordered");
  for (const name of catalog) {
    const rows = await analysis(name, name === "trace" ? { traceId: workload.traceId } : {});
    assert.ok(Array.isArray(rows), `${name} answers rows`);
  }
  for (const name of ["plan-shapes", "mutation-vs-query-latency"]) {
    assert.ok((await analysis(name)).length > 0, `${name} has rows for this workload`);
  }
  return throughput;
}

/** The catalog's names, from `logs list`. */
async function catalogNames(server: Server, env: Record<string, string>) {
  const out = await cli(server, "logs", ["list"], env);
  const names = out.stdout.trim().split("\n").map((line) => line.split(/\s+/)[0]);
  assert.ok(names.includes("trace") && names.length >= 10, `logs list: ${out.stdout}`);
  return names;
}

/** The read-only CLI answers what the analysis answered, and refuses an unknown query (exit 2). */
async function cliAgrees(server: Server, env: Record<string, string>, throughput: Row[]) {
  const args = ["query", "handler-throughput", "--param", `appId=${apps[0]}`, "--format", "json"];
  const result = await cli(server, "logs", args, env);
  assert.deepEqual(stable(JSON.parse(result.stdout)), stable(throughput));
  const missing = await cli(server, "logs", ["query", "noSuchQuery", "--format", "json"], env, false);
  assert.equal(missing.code, 2);
  assert.match(missing.stderr, /UnknownAnalysisQuery/);
}

/**
 * The SDK runs the handlers; agentd writes the event log (`event_log`: JSONL under `<dir>/logs`, role
 * server); the offline CLI (`scripts/logs.ts`: `ingest`, `query`) is the ingest and the analysis. An app
 * conflict never reaches the server (the SDK fails `makeMove` locally), so the server-side failure is a
 * schema drift: a client compiled for the other layout.
 */
const scenario = (dir: string) =>
  Effect.gen(function* () {
    const server = yield* startGoAgent(dir, { event_log: { dir: `${dir}/logs`, trace_sql: true } });
    const env = {
      ...cleanEnv(),
      LOG_DIR: `${dir}/logs`,
      EVENTLOG_DUCKDB: `${dir}/eventlog.duckdb`,
      DATA_DIR: `${dir}/data`,
    };
    yield* Effect.promise(async () => {
      const c = client(server);
      const game = await play(c, moves.X.slice(0, 2));
      const traceId = `e2e-${names[9]}`;
      await run(c.query("getGame", { gameId: game._id }, traceId));
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
      const query: Analysis = async (name, params = {}) => {
        const args = Object.entries(params).flatMap(([k, v]) => ["--param", `${k}=${v}`]);
        return JSON.parse(
          (await cli(server, "logs", ["query", name, ...args, "--format", "json"], env)).stdout,
        );
      };
      const throughput = await analyse(
        () => cli(server, "logs", ["ingest"], env),
        query,
        await catalogNames(server, env),
        {
          traceId,
          handlers: ["createGame", "getGame", "makeMove", "listGames", "winsByMark"],
          errors: [["listGames", "SchemaDriftError"]],
          backups: false, // static mode: backups are durable mode's (POST /admin/backup)
        },
      );
      await cliAgrees(server, env, throughput);
    });
  });

test(
  "e2e/6-log-analysis: ingest, catalog, traces and the CLI agree (agentd JSONL + offline CLI)",
  () =>
    Effect.gen(function* () {
      const dir = yield* scenarioDir("log_analysis");
      yield* scenario(dir);
    }),
);
