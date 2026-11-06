/**
 * `deno task logs:*` — the operator CLI for the event log (plan item 23): agentd writes the JSONL
 * (`event_log` in its config, `server/internal/eventlog`); this ingests and analyses it offline.
 *
 *   logs:ingest                                  ingest closed and growing JSONL into the DuckDB event log
 *   logs:query <name> [--param k=v]... [--format table|json|csv]
 *   logs:list                                    the catalog's names and the question each answers
 *   logs:sweep [--dry-run]                       ingest, seal torn tails, then the retention sweep
 *
 * Each command is its own Effect program. `ingest` and `sweep` run on the CLI root
 * (`eventlog/root.ts`: `Config` and the CLI's own logging sink) and build the event-log handle in the
 * program's scope; `query` and `list` need only `Config` and the read-only `AnalysisLive`.
 *
 * The DuckDB file has a single writer: while another `logs:ingest` holds it, these commands fail with a
 * lock error rather than waiting.
 *
 * Exit codes: 0 success; 1 an operational failure (`IngestError`, `AnalysisError`, bad arguments);
 * 2 `UnknownAnalysisQuery`, which also lists the valid names.
 */
import { Cause, Effect, Exit, Option } from "effect";
import { AnalysisError, UnknownAnalysisQuery } from "../core/errors.ts";
import { AnalysisLive, CATALOG } from "../eventlog/catalog.ts";
import { ConfigLive } from "../eventlog/config.ts";
import { makeEventLogIngest } from "../eventlog/ingest.ts";
import { CliRootLive, ownOpenLogPaths } from "../eventlog/root.ts";
import { Analysis, Config, type Row } from "../eventlog/services.ts";

// ---------------------------------------------------------------------------
// Arguments
// ---------------------------------------------------------------------------

export type Format = "table" | "json" | "csv";

export class UsageError extends Error {
  override readonly name = "UsageError";
}

export interface QueryArgs {
  readonly name: string;
  readonly params: Readonly<Record<string, string>>;
  readonly format: Format;
}

/** `<name> [--param k=v | --param=k=v]... [--format table|json|csv | --format=...]`. */
export function parseQueryArgs(args: readonly string[]): QueryArgs {
  const params: Record<string, string> = {};
  let format: Format = "table";
  let name: string | undefined;
  for (let i = 0; i < args.length; i++) {
    const arg = args[i]!;
    const flag = arg.startsWith("--") ? arg.slice(2).split("=", 1)[0] : undefined;
    if (flag === "param" || flag === "format") {
      const inline = arg.includes("=") ? arg.slice(arg.indexOf("=") + 1) : undefined;
      const value = inline ?? args[++i];
      if (value === undefined) throw new UsageError(`--${flag} needs a value`);
      if (flag === "format") {
        if (value !== "table" && value !== "json" && value !== "csv") {
          throw new UsageError(`--format must be table, json or csv, got ${JSON.stringify(value)}`);
        }
        format = value;
      } else {
        const eq = value.indexOf("=");
        if (eq <= 0) throw new UsageError(`--param needs key=value, got ${JSON.stringify(value)}`);
        params[value.slice(0, eq)] = value.slice(eq + 1);
      }
    } else if (arg.startsWith("--")) {
      throw new UsageError(`unknown option ${arg}`);
    } else if (name === undefined) {
      name = arg;
    } else {
      throw new UsageError(`unexpected argument ${JSON.stringify(arg)}`);
    }
  }
  if (name === undefined) {
    throw new UsageError("usage: logs:query <name> [--param k=v]... [--format table|json|csv]");
  }
  return { name, params, format };
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

function cell(value: unknown): string {
  if (value === null || value === undefined) return "";
  return typeof value === "object" ? JSON.stringify(value) : String(value);
}

function columnsOf(rows: readonly Row[]): string[] {
  const columns: string[] = [];
  for (const row of rows) for (const key of Object.keys(row)) if (!columns.includes(key)) columns.push(key);
  return columns;
}

/** Rows as `table` (aligned, with a row count), `json` (one array) or `csv` (RFC 4180, header first). */
export function formatRows(rows: readonly Row[], format: Format): string {
  if (format === "json") return JSON.stringify(rows, null, 2);
  const columns = columnsOf(rows);
  if (format === "csv") {
    const quote = (text: string) => /[",\n\r]/.test(text) ? `"${text.replaceAll('"', '""')}"` : text;
    return [
      columns.map(quote).join(","),
      ...rows.map((row) => columns.map((c) => quote(cell(row[c]))).join(",")),
    ]
      .join("\n");
  }
  if (rows.length === 0) return "(0 rows)";
  const widths = columns.map((c) => Math.max(c.length, ...rows.map((row) => cell(row[c]).length)));
  const line = (cells: readonly string[]) =>
    cells.map((text, i) => text.padEnd(widths[i]!)).join("  ").trimEnd();
  return [
    line(columns),
    line(widths.map((w) => "-".repeat(w))),
    ...rows.map((row) => line(columns.map((c) => cell(row[c])))),
    `(${rows.length} row${rows.length === 1 ? "" : "s"})`,
  ].join("\n");
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

const list = Effect.sync(() => {
  const width = Math.max(...Object.keys(CATALOG).map((name) => name.length));
  return Object.entries(CATALOG)
    .map(([name, entry]) => `${name.padEnd(width)}  ${entry.description}`)
    .join("\n");
});

const query = (args: QueryArgs) =>
  Effect.gen(function* () {
    const analysis = yield* Analysis;
    const rows = yield* analysis.run(args.name, args.params);
    return formatRows(rows, args.format);
  }).pipe(Effect.provide(AnalysisLive), Effect.provide(ConfigLive));

/**
 * Plain ingest: the event-log handle opens in this program's scope and `ingest` runs once. Nothing is
 * sealed, gzipped or deleted — that is `sweep`'s job.
 */
const ingest = Effect.scoped(Effect.gen(function* () {
  const config = yield* Config;
  const control = yield* makeEventLogIngest({
    retention: { openPaths: ownOpenLogPaths(config.logging.dir, Deno.pid) },
  });
  return JSON.stringify(yield* control.ingest, null, 2);
})).pipe(Effect.provide(CliRootLive));

/**
 * `ingestThenSweep({ dryRun })` — the coordinator in `eventlog/ingest.ts`. With `--dry-run` it
 * plans the sweep from the already-committed `_ingest_state` and changes nothing: no ingest, no
 * sealing, no file touched.
 */
const sweep = (dryRun: boolean) =>
  Effect.scoped(Effect.gen(function* () {
    const config = yield* Config;
    const control = yield* makeEventLogIngest({
      retention: { openPaths: ownOpenLogPaths(config.logging.dir, Deno.pid) },
    });
    const report = dryRun
      ? yield* control.ingestThenSweep({ dryRun: true })
      : yield* control.ingestThenSweep();
    return JSON.stringify(report, null, 2);
  })).pipe(Effect.provide(CliRootLive));

// ---------------------------------------------------------------------------
// Entry
// ---------------------------------------------------------------------------

export interface Outcome {
  readonly code: number;
  readonly stdout?: string;
  readonly stderr?: string;
}

function describe(error: unknown): string {
  if (error instanceof UnknownAnalysisQuery) {
    return `UnknownAnalysisQuery: no analysis query named ${JSON.stringify(error.name)}\nvalid names:\n${
      error.known.map((n) => `  ${n}`).join("\n")
    }`;
  }
  if (error instanceof AnalysisError) {
    return `AnalysisError: ${error.name}: ${error.detail ?? "failed"}`;
  }
  if (typeof error === "object" && error !== null && "_tag" in error) {
    const e = error as { _tag: string; phase?: string; detail?: string; message?: string };
    return `${e._tag}${e.phase ? ` (${e.phase})` : ""}: ${e.detail ?? e.message ?? ""}`.trimEnd();
  }
  return error instanceof Error ? error.message : String(error);
}

/** Runs one command line (`argv` without the program name) and returns what to print and exit with. */
export async function run(argv: readonly string[]): Promise<Outcome> {
  const [command, ...rest] = argv;
  let program: Effect.Effect<string, unknown>;
  try {
    switch (command) {
      case "list":
        program = list;
        break;
      case "query":
        program = query(parseQueryArgs(rest));
        break;
      case "ingest":
        if (rest.length > 0) throw new UsageError(`logs:ingest takes no arguments, got ${rest.join(" ")}`);
        program = ingest;
        break;
      case "sweep": {
        const unknown = rest.filter((arg) => arg !== "--dry-run");
        if (unknown.length > 0) {
          throw new UsageError(`unknown option ${unknown[0]}; logs:sweep accepts --dry-run`);
        }
        program = sweep(rest.includes("--dry-run"));
        break;
      }
      default:
        throw new UsageError("usage: logs.ts <ingest | query <name> | list | sweep [--dry-run]>");
    }
  } catch (error) {
    if (error instanceof UsageError) return { code: 1, stderr: error.message };
    throw error;
  }
  const exit = await Effect.runPromiseExit(program as Effect.Effect<string, unknown, never>);
  if (Exit.isSuccess(exit)) return { code: 0, stdout: exit.value };
  const failure = Cause.failureOption(exit.cause);
  if (Option.isSome(failure)) {
    return { code: failure.value instanceof UnknownAnalysisQuery ? 2 : 1, stderr: describe(failure.value) };
  }
  return { code: 1, stderr: Cause.pretty(exit.cause) };
}

if (import.meta.main) {
  const outcome = await run(Deno.args);
  if (outcome.stdout !== undefined) console.log(outcome.stdout);
  if (outcome.stderr !== undefined) console.error(outcome.stderr);
  Deno.exit(outcome.code);
}
