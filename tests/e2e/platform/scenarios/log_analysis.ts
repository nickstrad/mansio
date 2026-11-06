import {
  admin,
  apps,
  assert,
  cli,
  delay,
  Effect,
  legacyClient,
  play,
  players,
  remoteError,
  request,
  run,
  scenarioDir,
  startServer,
  test,
} from "../harness.ts";
import { database } from "../postgres.ts";
import { moves, names } from "../seed.ts";
type Row = Record<string, unknown>;
const stable = (rows: Row[]) => rows.map((r) => JSON.stringify(r)).sort();

test(
  "e2e/6-log-analysis: HTTP ingest, catalog, cross-process trace and CLI agree",
  () =>
    Effect.gen(function* () {
      const dir = yield* scenarioDir("log_analysis");
      const url = yield* database("log_analysis");
      const server = yield* startServer(dir, url, { TRACE_SQL: "1" });
      yield* Effect.promise(async () => {
        const c = legacyClient(server);
        const game = await play(c, moves.X.slice(0, 2));
        const traceId = `e2e-${names[9]}`;
        const documentTraceId = `${traceId}-document`;
        await run(c.query("getGame", { gameId: game._id }, documentTraceId));
        await run(c.query("listGames", { status: "playing" }));
        await run(c.query("winsByMark", {}));
        await remoteError(
          c.mutation("makeMove", {
            gameId: game._id,
            playerId: players.xPlayer,
            cell: moves.X[2],
            expectedVersion: 1,
          }),
          409,
          "ConflictError",
        );
        const unknown = await request(
          server,
          `/apps/${apps[0]}/query/noSuchHandler`,
          {},
          server.env.COLUMN_APP_TOKEN,
        );
        assert.equal(unknown.status, 404);
        assert.equal(unknown.body.error.tag, "UnknownHandlerError");
        await admin(server, `/admin/backup/${apps[0]}`);
        const analysis = async (name: string, params: Row = {}) =>
          await admin(server, `/admin/logs/analysis/${name}`, params) as Row[];
        // Successful analysis/ingest RPCs emit no reader events under the caller's trace.
        // Probe the supported direct query RPC, whose SQL/tx events do retain that trace;
        // this is protocol coverage, not the production HTTP document-query path.
        const reader = new Deno.Command(Deno.execPath(), {
          args: ["run", "-A", "platform/reader.ts"],
          env: server.env,
          stdin: "piped",
          stdout: "piped",
          stderr: "piped",
        }).spawn();
        const output = reader.output();
        const bound = setTimeout(() => {
          try {
            reader.kill("SIGKILL");
          } catch { /* already exited */ }
        }, 10_000);
        try {
          const input = reader.stdin.getWriter();
          await input.write(new TextEncoder().encode(
            JSON.stringify({
              id: 1,
              kind: "query",
              appId: apps[0],
              name: "getGame",
              args: { gameId: game._id },
              traceId,
            }) + "\n",
          ));
          await input.close();
          const result = await output;
          assert.equal(result.code, 0, new TextDecoder().decode(result.stderr));
          assert.equal(JSON.parse(new TextDecoder().decode(result.stdout)).result._id, game._id);
        } finally {
          clearTimeout(bound);
          try {
            reader.kill("SIGTERM");
          } catch { /* already exited */ }
          await output;
        }
        const deadline = Date.now() + 10_000;
        const handlers = ["createGame", "getGame", "makeMove", "listGames", "winsByMark", "noSuchHandler"];
        const errorPairs = [["makeMove", "ConflictError"], ["noSuchHandler", "UnknownHandlerError"]];
        let trace: Row[] = [], documentTrace: Row[] = [], throughput: Row[] = [], errors: Row[] = [];
        let slow: Row[] = [], tx: Row[] = [], backups: Row[] = [];
        const complete = () => {
          const roles = new Set(trace.map((r) => r.role));
          return roles.has("server") && roles.has("reader") &&
            documentTrace.some((r) => r.source === "http" && r.name === "getGame") &&
            handlers.every((name) => throughput.some((r) => r.name === name)) &&
            errorPairs.every(([name, tag]) =>
              errors.some((r) =>
                r.name === name && Number(r.errors) >= 1 && String(r.error_tags).includes(tag)
              ) &&
              throughput.some((r) => r.name === name && Number(r.status_4xx) >= 1)
            ) &&
            slow.length > 0 && tx.length > 0 &&
            backups.some((r) => r.app_id === apps[0] && r.phase === "publish-latest" && r.outcome === "ok");
        };
        while (true) {
          await admin(server, "/admin/logs/ingest");
          // Endpoint errors are fatal; only asynchronously flushed evidence is retried.
          trace = await analysis("trace", { traceId });
          documentTrace = await analysis("trace", { traceId: documentTraceId });
          const crossed = await request(
            server,
            "/admin/logs/analysis/handler-throughput",
            { appId: apps[0] },
            server.env.ADMIN_TOKEN,
            { "x-trace-id": traceId },
          );
          assert.equal(crossed.status, 200, JSON.stringify(crossed.body));
          throughput = crossed.body.result as Row[];
          errors = await analysis("error-rate-by-handler");
          slow = await analysis("slowest-sql");
          tx = await analysis("tx-outcomes");
          backups = await analysis("backup-history");
          if (complete()) break;
          assert.ok(
            Date.now() < deadline,
            `Incomplete ingested workload within 10 s: ${
              JSON.stringify({ trace, documentTrace, throughput, errors, slow, tx, backups })
            }`,
          );
          await delay(100);
        }
        assert.deepEqual(
          [...new Set(documentTrace.map((r) => r.role))],
          ["server"],
          "HTTP document queries never reach the child",
        );
        assert.ok(
          documentTrace.some((r) => r.source === "sql" && r.role === "server"),
          "the HTTP document query emitted server-side SQLite SQL",
        );
        assert.ok(
          trace.some((r) => r.source === "sql" && r.role === "reader"),
          "the direct query RPC emitted reader-side SQL under the shared trace",
        );
        const times = trace.map((r) => String(r.ts));
        assert.deepEqual(times, [...times].sort(), "trace timeline must be ordered");
        const rejected = await request(server, "/admin/logs/analysis/noSuchQuery");
        assert.equal(rejected.status, 404);
        assert.equal(rejected.body.error.tag, "UnknownAnalysisQuery");
        // DuckDB permits one writer process. Freeze the ingested snapshot before the read-only CLI opens it.
        await server.stop();
        const result = await cli(server, "logs", [
          "query",
          "handler-throughput",
          "--param",
          `appId=${apps[0]}`,
          "--format",
          "json",
        ]);
        assert.deepEqual(stable(JSON.parse(result.stdout)), stable(throughput));
        const missing = await cli(server, "logs", ["query", "noSuchQuery", "--format", "json"], {}, false);
        assert.notEqual(missing.code, 0);
        assert.match(missing.stderr, /UnknownAnalysisQuery/);
      });
    }),
  { go: "IR-4 ports observability (event log, analysis) to Go" },
);
