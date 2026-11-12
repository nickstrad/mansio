/**
 * Scratch-process helpers for `tests/e2e/observability_test.ts`: a throwaway agentd with an event log
 * (`startScratchAgent`), its JSONL, and DuckDB read out of process.
 *
 * The DuckDB files are read from a `deno eval` subprocess (`duckdbRows`), so a file `scripts/logs.ts` still
 * holds is never opened twice in one process.
 */
import assert from "node:assert/strict";
import { resolve } from "node:path";
import { Effect, Exit, type Scope } from "effect";
import { schema } from "../../examples/tic-tac-toe/schema.ts";
import { withStorage } from "../../sdk/schema.ts";
import { schemaFingerprint } from "../../sdk/fingerprint.ts";
import * as ticket from "../../sdk/ticket.ts";

function stopChild(child: Deno.ChildProcess): Effect.Effect<Deno.CommandStatus> {
  return Effect.promise(async () => {
    try {
      child.kill("SIGTERM");
    } catch { /* already exited */ }
    return await child.status;
  });
}

export interface ScratchAgent {
  readonly base: string;
  /** `event_log.dir`: agentd's JSONL event log (role `server`), the CLI's `LOG_DIR`. */
  readonly logDir: string;
  /** Where `ingest` puts the DuckDB event log (`EVENTLOG_DUCKDB`). */
  readonly eventLogPath: string;
  /** Tickets for `local:tic-tac-toe` (columns layout), valid ~50 minutes, and its schema fingerprint. */
  readonly readTicket: string;
  readonly writeTicket: string;
  readonly fingerprint: string;
  /** `deno run scripts/logs.ts ingest` over `logDir` (the offline ingest a Go deployment runs). */
  readonly ingest: Effect.Effect<{ code: number; stdout: string; stderr: string }>;
  /** SIGTERM, then the exit status. */
  readonly stop: Effect.Effect<Deno.CommandStatus>;
}

/**
 * Starts `cmd/agentd` in static mode with an event log over scratch directories under `root`: one empty
 * `local:tic-tac-toe.db`, its schema in the columns layout (the first request creates the tables), a fresh
 * ticket issuer. The binary is `AGENTD_BIN`, else built from `server/` into `root`. `eventLevel` is
 * `event_log.level` (`error` drops every request record: the negative control).
 */
export function startScratchAgent(
  root: string,
  options: { readonly eventLevel?: "debug" | "info" | "warn" | "error" } = {},
): Effect.Effect<ScratchAgent, never, Scope.Scope> {
  return Effect.gen(function* () {
    const app = "tic-tac-toe";
    const stored = withStorage(schema, "columns");
    const setup = yield* Effect.promise(async () => {
      for (const sub of ["dbs", "keys", "logs", `schemas/${app}`]) {
        await Deno.mkdir(resolve(root, sub), { recursive: true });
      }
      await Deno.writeFile(resolve(root, "dbs", `local:${app}.db`), new Uint8Array());
      await Deno.writeTextFile(resolve(root, "schemas", app, "v1.json"), JSON.stringify(stored));
      const issuer = await ticket.generateIssuer();
      await Deno.writeTextFile(resolve(root, "keys", "keys.json"), issuer.keysFile);
      await Deno.writeTextFile(
        resolve(root, "agentd.json"),
        JSON.stringify({
          listen: "127.0.0.1:0",
          vm: "vm-local",
          db_dir: resolve(root, "dbs"),
          keys_file: resolve(root, "keys", "keys.json"),
          schemas_dir: resolve(root, "schemas"),
          max_ticket_ttl: "1h",
          log_file: resolve(root, "logs", "agentd.log"),
          event_log: { dir: resolve(root, "logs"), level: options.eventLevel ?? "info" },
        }),
      );
      let bin = Deno.env.get("AGENTD_BIN");
      if (!bin) {
        bin = resolve(root, "agentd");
        const built = await new Deno.Command("go", {
          args: ["build", "-o", bin, "./cmd/agentd"],
          cwd: resolve("server"),
          stdout: "inherit",
          stderr: "inherit",
        }).output();
        assert.ok(built.success, "go build ./cmd/agentd failed");
      }
      const iat = Math.floor(Date.now() / 1000) - 1;
      const mint = (perms: number) =>
        ticket.mintTicket(issuer.key, {
          kid: issuer.kid,
          tid: "local",
          dbid: `local:${app}`,
          vm: "vm-local",
          epoch: 1,
          sub: "k6",
          perms,
          iat,
          exp: iat + 3000,
        });
      return {
        bin,
        readTicket: await mint(ticket.PERM_READ),
        writeTicket: await mint(ticket.PERM_WRITE),
        fingerprint: await schemaFingerprint(stored),
      };
    });
    const addrFile = resolve(root, "agentd.addr");
    const child = yield* Effect.acquireRelease(
      Effect.sync(() =>
        new Deno.Command(setup.bin, {
          args: ["-config", resolve(root, "agentd.json"), "-addr-file", addrFile],
          stdout: "null",
          stderr: "inherit",
        }).spawn()
      ),
      (child) => stopChild(child),
    );
    let exited = false;
    void child.status.then(() => void (exited = true));
    let base = "";
    for (let attempt = 0; attempt < 600 && !base; attempt++) {
      assert.ok(!exited, "agentd exited before serving");
      const line = yield* Effect.promise(() => Deno.readTextFile(addrFile).catch(() => ""));
      if (line.includes("\n")) base = `http://${line.split("\n")[0].trim()}`;
      else yield* Effect.sleep("50 millis");
    }
    assert.ok(base, "agentd never wrote its address");
    const logDir = resolve(root, "logs");
    const eventLogPath = resolve(root, "eventlog", "eventlog.duckdb");
    const ingest = Effect.promise(async () => {
      const out = await new Deno.Command(Deno.execPath(), {
        args: ["run", "-A", "scripts/logs.ts", "ingest"],
        env: { LOG_DIR: logDir, EVENTLOG_DUCKDB: eventLogPath, DATA_DIR: resolve(root, "data") },
        stdout: "piped",
        stderr: "piped",
      }).output();
      const decode = (bytes: Uint8Array) => new TextDecoder().decode(bytes);
      return { code: out.code, stdout: decode(out.stdout), stderr: decode(out.stderr) };
    });
    return { ...setup, base, logDir, eventLogPath, ingest, stop: stopChild(child) };
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

// ===========================================================================
// agentd's JSONL event log (role `server`)
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
