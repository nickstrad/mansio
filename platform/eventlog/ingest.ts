/**
 * `EventLogIngestLive`: idempotent, exactly-once ingest of the rotating JSONL event log into the
 * append-only DuckDB event tables (plan item 21).
 *
 * **Reader process only.** This module imports `@duckdb/node-api`; the writer (server) process must
 * never load it — see ADR 0004. The event-log DuckDB file (`Config.eventLog.duckdbPath`) is a
 * persistent file of its own, separate from the per-app `:memory:` instances `platform/duckdb.ts`
 * opens, and nothing here loads DuckDB's `sqlite` extension.
 *
 * ## One run
 *
 * 1. **Inventory**: list `LOG_DIR/<role>/<YYYY-MM-DD>/events-*.jsonl` and read every
 *    `_ingest_state` row.
 * 2. **Plan**, per file: read its bytes and hand them to `core/eventlog/offsets.ts`'s `planFile`,
 *    which re-hashes the recorded prefix, decides `skip` / `new` / `append` / `rewritten`, and cuts
 *    the complete lines past the offset. Lines that are not UTF-8 or exceed `MAX_LINE_BYTES` are
 *    rejected here; the rest are written to an offset-trimmed temp file (see `tables.ts`).
 * 3. **Commit**, per batch of roughly `batchBytes` of new data: in **one DuckDB transaction**, stage the temp
 *    files with `read_json_auto` over the explicit file list, insert into the event tables, write
 *    rejected lines to quarantine, and upsert each file's `_ingest_state` row. Either all of it
 *    commits or none of it does, so a crash or a failed statement never advances an offset past rows
 *    that did not land, and never lands rows without advancing the offset.
 *
 * Event tables only ever receive `INSERT`s (from `tables.ts`); `_ingest_state` is the one mutable
 * table, and the upsert below is the only statement in this module that changes an existing row.
 *
 * ## Rejected lines
 *
 * A line that cannot be ingested is appended to `LOG_DIR/<role>/<date>/rejected.jsonl` beside its
 * source file, and the rest of the batch proceeds. (A line rejected for exceeding `MAX_LINE_BYTES`
 * keeps only its first 64 KiB of bytes there, decoded lossily, so a UTF-8 sequence cut at the end
 * becomes U+FFFD; the source JSONL still holds it in full.) Quarantine
 * is written *before* `COMMIT`: if the commit then fails, the next run quarantines the same lines
 * again. A duplicate in a diagnostic file
 * is the safe failure; a rejected line that vanished would not be. Each file with rejects yields one
 * `ingest` event.
 *
 * Only anomalies (rejected lines, a rewritten source, an unreadable file, a failed run) are emitted
 * as `ingest` events. A success event per run would itself be ingested by the next run, so a quiet
 * tree would never become an unchanged one.
 *
 * Quarantine records are fsynced (file, then directory) before the `COMMIT` that moves the offsets
 * past them, so a committed offset only ever covers a rejected line whose evidence is durable. That
 * is what lets retention count a rejected line as consumed (ADR 0007).
 *
 * ## Ingest, then sweep (item 36, ADR 0007)
 *
 * Retention may gzip or delete a source only once committed ingest covers all of it, and only this
 * process can see `_ingest_state`. So the coordinator lives here: `ingestThenSweep` runs, under the
 * same lock as `ingest`,
 *
 * 1. an ordinary ingest run — if it fails, the call fails and nothing is swept;
 * 2. `sealTornTails`: a closed source whose offset stops before a newline-free tail (a write that
 *    was torn by a crash or a failed write, which no sink will ever finish) has that tail quarantined
 *    as a `torn-tail` reject and its offset moved to the end, in one transaction;
 * 3. `platform/logging/retention.sweep`, with the committed rows and the accepted `source_offset`s
 *    as its ledger, so the gate judges exactly what is durable;
 * 4. a measurement of both budgets (`RetentionBudgets`).
 *
 * `eventLogIngestThenSweepLayer` provides the existing `EventLogIngest` tag with `ingest` bound to
 * that call, which is the whole hook items 32 and 33 need: the reader's existing `ingest` RPC gains
 * retention without a new request kind or a service change. A sweep failure is reported in the reply
 * (and as its own lifecycle event); it does not fail the ingest that already committed.
 *
 * The schema compatibility check (item 35) needs no extra wiring: every connection runs
 * `EVENTLOG_DDL`, whose guards refuse a v1 or foreign file before any statement touches it, so the
 * layer fails to build and no sweep can run against it. This module never calls `upgradeEventLog`;
 * it would have to close its own connection first (see that function), so it stays an operator step.
 *
 * `deno task check` reaches this module through `platform/layers.ts`'s type-only import; item 33
 * wires it into the reader entry point at runtime.
 */
import { createHash } from "node:crypto";
import { dirname, join, resolve } from "node:path";
import { DuckDBInstance } from "@duckdb/node-api";
import { Clock, Effect, Exit, Layer, type Scope } from "effect";
import { drainDuckDB } from "../duckdb.ts";
import { IngestError, type IngestPhase } from "../../core/errors.ts";
import {
  fromPlatformError,
  ingestEvent,
  type IngestPayload,
  type Level,
  type Role,
} from "../../core/events.ts";
import { dateBucketOf } from "../../core/logging/rotation.ts";
import {
  compareSourceFiles,
  type FilePlan,
  type IngestStateRow,
  parseSourceFile,
  planFile,
  quarantinePath,
  SOURCE_ROLES,
  type SourceFile,
} from "../../core/eventlog/offsets.ts";
import {
  countsSql,
  dropStagingSql,
  EVENTLOG_DDL,
  insertStatements,
  MAX_LINE_BYTES,
  rejectedSql,
  type RejectReason,
  repairLoneSurrogates,
  rowsSql,
  sqlString,
  type StagedLine,
  stageSql,
} from "./tables.ts";
import {
  DEFAULT_SETTLE_MS,
  type IngestLedger,
  inventory as retentionInventory,
  policyFromConfig,
  RetentionLedgerError,
  sweep,
  type SweepReport,
  syncDir,
} from "../logging/retention.ts";
import {
  Config,
  EventLogIngest,
  type EventLogIngestService,
  EventSink,
  type IngestReport,
} from "../services.ts";

// ---------------------------------------------------------------------------
// The native seam
// ---------------------------------------------------------------------------

type SqlValue = string | number | boolean | null;

/** The slice of `DuckDBConnection` ingest uses. */
export interface EventLogConnection {
  run(sql: string, values?: SqlValue[]): Promise<unknown>;
  runAndReadAll(sql: string): Promise<{ getRowObjects(): Record<string, unknown>[] }>;
  closeSync(): void;
}

/** The slice of `DuckDBInstance` ingest uses. */
export interface EventLogInstance {
  connect(): Promise<EventLogConnection>;
  closeSync(): void;
}

/** How the layer opens the event-log file. Tests wrap it to inject failures. */
export interface EventLogDriver {
  readonly open: (path: string) => Promise<EventLogInstance>;
}

/** The persistent event-log file, sized like the per-app readers. */
export const eventLogDriver: EventLogDriver = {
  open: (path) => DuckDBInstance.create(path, { threads: "2", memory_limit: "256MB" }),
};

export interface EventLogIngestOptions {
  readonly driver?: EventLogDriver;
  /** Target new bytes per transaction. A single file is never split, so a batch may exceed it. */
  readonly batchBytes?: number;
  /** The `role` on emitted `ingest` and retention events. Default `"reader"`, the process that owns DuckDB. */
  readonly role?: Role;
  /** How `ingestThenSweep` judges a file closed. */
  readonly retention?: RetentionCoordinatorOptions;
}

/** How the ingest-then-sweep coordinator decides a source is closed (ADR 0007). */
export interface RetentionCoordinatorOptions {
  /** Paths this process's sinks hold open; never sealed, gzipped or deleted. */
  readonly openPaths?: Effect.Effect<readonly string[]>;
  /** A file modified more recently than this is not yet closed. Default `DEFAULT_SETTLE_MS`. */
  readonly settleMs?: number;
}

const DEFAULT_BATCH_BYTES = 32 * 1024 * 1024;

/** Per call of `ingestThenSweep`. */
export interface IngestThenSweepOptions {
  /**
   * Plan the sweep from the state already committed and report it, changing nothing: no ingest, no
   * sealing, no file touched, no event emitted unless the sweep is refused.
   */
  readonly dryRun?: boolean;
}

/** What the retention half of one `ingestThenSweep` did. A sweep failure is data, not the RPC's error. */
export type RetentionOutcome =
  | {
    readonly status: "swept";
    /** Sources whose torn tails were sealed into quarantine this run. */
    readonly sealed: readonly string[];
    readonly sealError?: string;
    readonly sweep: SweepReport;
  }
  | {
    readonly status: "failed";
    readonly sealed: readonly string[];
    readonly sealError?: string;
    /** The sweep's typed failure; it already emitted a `retention-sweep-failed` lifecycle event. */
    readonly errorTag: string;
    readonly error: string;
  };

/**
 * The two storage budgets, which are distinct (ADR 0007). `LOG_MAX_TOTAL_BYTES` bounds only the
 * retained JSONL, and only as far as closed-file safety allows (`blockedBytes` is what it could not
 * remove). The derived DuckDB file is never pruned: it grows with every ingested event, measured
 * here, not capped. Logger drops are counted apart from everything that reached disk.
 */
export interface RetentionBudgets {
  readonly jsonl: {
    readonly retainedBytes: number;
    readonly files: number;
    /** Today's (and any future-dated) files: never swept, always counted. */
    readonly todayBytes: number;
    /** Files retention wanted gone or compressed but the gate kept: the unsafe-to-delete backlog. */
    readonly blockedBytes: number;
    readonly capBytes: number;
    readonly withinCap: boolean;
  };
  readonly derived: {
    readonly duckdbBytes: number;
    readonly walBytes: number;
    readonly platformEventRows: number;
    readonly ingestStateRows: number;
    readonly boundedByLogMaxTotalBytes: false;
  };
  readonly logger: {
    /** Records the sinks reported dropped (`queue_events`, `event = 'dropped'`), as ingested so far. */
    readonly dropsRecorded: number;
  };
}

/** `IngestReport` plus the sweep it gated and the budgets after it. A superset, so the wire type holds. */
export interface IngestThenSweepReport extends IngestReport {
  readonly retention: RetentionOutcome;
  /** Absent only when measuring failed (`budgetsError`); ingest and sweep had already completed. */
  readonly budgets?: RetentionBudgets;
  readonly budgetsError?: string;
}

/** What `makeEventLogIngest` builds: the frozen service plus the coordinator. */
export interface EventLogIngestControl extends EventLogIngestService {
  /**
   * Under the ingest lock: ingest, seal closed torn tails, then sweep with the committed state as
   * the gate's ledger. An ingest failure fails the whole call before anything is swept.
   */
  readonly ingestThenSweep: (
    options?: IngestThenSweepOptions,
  ) => Effect.Effect<IngestThenSweepReport, IngestError>;
}

/**
 * Moves one file's offset. `_ingest_state` is the one mutable table, and this is the only statement in
 * this module that replaces an existing row. `$5` is an ISO-8601 instant; it is stored as UTC.
 */
const UPSERT_INGEST_STATE =
  "INSERT OR REPLACE INTO _ingest_state (source_file, bytes_ingested, prefix_sha256, dedupe, last_ingested_at) " +
  "VALUES ($1, $2, $3, $4, CAST($5 AS TIMESTAMPTZ) AT TIME ZONE 'UTC')";

/** How many rejected offsets an `ingest` event lists before it only counts. */
const MAX_LISTED_REJECTS = 20;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function messageOf(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause);
}

/** Runs one native or filesystem call; a rejection becomes `IngestError({ phase })`. */
function attempt<A>(
  phase: IngestPhase,
  run: () => Promise<A>,
  sourceFile?: string,
): Effect.Effect<A, IngestError> {
  return Effect.tryPromise({
    try: run,
    catch: (cause) => new IngestError({ phase, sourceFile, detail: messageOf(cause), cause }),
  });
}

function sha256Hex(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}

/** Closes a native handle; a failing close is logged, never raised — finalizers cannot fail. */
function close(what: string, handle: { closeSync(): void }): Effect.Effect<void> {
  return Effect.try(() => handle.closeSync()).pipe(
    Effect.catchAll((error) => Effect.logWarning(`event-log ${what} failed to close`, error)),
  );
}

/** The entries of a directory, or none if it does not exist. */
async function entries(path: string): Promise<Deno.DirEntry[]> {
  const found: Deno.DirEntry[] = [];
  try {
    for await (const entry of Deno.readDir(path)) found.push(entry);
  } catch (cause) {
    if (!(cause instanceof Deno.errors.NotFound)) throw cause;
  }
  return found;
}

/** Every event-log source file under `logDir`, in ingest order. */
async function inventory(logDir: string): Promise<SourceFile[]> {
  const sources: SourceFile[] = [];
  for (const role of SOURCE_ROLES) {
    for (const day of await entries(join(logDir, role))) {
      if (!day.isDirectory) continue;
      for (const file of await entries(join(logDir, role, day.name))) {
        if (!file.isFile) continue;
        const source = parseSourceFile(`${role}/${day.name}/${file.name}`);
        if (source !== undefined) sources.push(source);
      }
    }
  }
  return sources.sort(compareSourceFiles);
}

function toNumber(value: unknown): number {
  return typeof value === "bigint" ? Number(value) : Number(value ?? 0);
}

// ---------------------------------------------------------------------------
// Per-file staging
// ---------------------------------------------------------------------------

/**
 * Why a record was quarantined: `tables.ts`'s reasons, or `torn-tail` — a closed source's partial
 * last line, sealed by the ingest-then-sweep coordinator.
 */
export type QuarantineReason = RejectReason | "torn-tail";

/** One line that will not reach an event table, and why. */
interface Reject {
  readonly sourceOffset: number;
  readonly reason: QuarantineReason;
  readonly line: string;
}

/** One file's plan, its staged temp file (when it has lines to stage), and its pre-staging rejects. */
interface Staged {
  readonly source: SourceFile;
  readonly plan: FilePlan;
  readonly tempFile?: string;
  readonly rejects: readonly Reject[];
}

const strictUtf8 = new TextDecoder("utf-8", { fatal: true });
const lossyUtf8 = new TextDecoder("utf-8");

/**
 * Quarantine keeps at most this many bytes (64 KiB, not characters) of a line rejected for its size.
 * The cut can split a UTF-8 sequence, which the lossy decode turns into U+FFFD.
 */
const QUARANTINE_LINE_BYTES = 64 * 1024;

/**
 * Writes `plan`'s lines as `StagedLine` wrappers to `tempFile`. The original line travels as a JSON
 * *string*, byte-for-byte, so DuckDB alone parses it and no JavaScript number ever touches it.
 */
async function stage(
  source: SourceFile,
  plan: FilePlan,
  bytes: Uint8Array,
  tempFile: string,
): Promise<Staged> {
  const dedupe = plan.decision.kind === "read" && plan.decision.dedupe;
  const rejects: Reject[] = [];
  const out: string[] = [];
  for (const span of plan.lines) {
    const raw = bytes.subarray(span.start, span.end);
    if (raw.length > MAX_LINE_BYTES) {
      const prefix = lossyUtf8.decode(raw.subarray(0, QUARANTINE_LINE_BYTES));
      rejects.push({ sourceOffset: span.start, reason: "line-too-large", line: prefix });
      continue;
    }
    let line: string;
    try {
      line = repairLoneSurrogates(strictUtf8.decode(raw));
    } catch {
      rejects.push({ sourceOffset: span.start, reason: "invalid-utf8", line: lossyUtf8.decode(raw) });
      continue;
    }
    const wrapper: StagedLine = { source_file: source.path, source_offset: span.start, dedupe, line };
    out.push(JSON.stringify(wrapper));
  }
  if (out.length === 0) return { source, plan, rejects };
  await Deno.writeTextFile(tempFile, out.join("\n") + "\n");
  return { source, plan, tempFile, rejects };
}

// ---------------------------------------------------------------------------
// The service
// ---------------------------------------------------------------------------

/** What one committed batch contributes to the run's report and events. */
interface BatchResult {
  readonly inserted: number;
  readonly rejected: number;
  /** Per source file: rejects, and lines skipped by dedupe because they were already ingested. */
  readonly perFile: ReadonlyMap<string, { rejects: Reject[]; deduplicated: number }>;
}

/**
 * Builds the service in the current scope: opens (creating if needed) the event-log file, ensures
 * every table exists, and registers connection-then-instance release.
 */
export function makeEventLogIngest(
  options: EventLogIngestOptions = {},
): Effect.Effect<EventLogIngestControl, IngestError, Scope.Scope | Config | EventSink> {
  return Effect.gen(function* () {
    const config = yield* Config;
    const sink = yield* EventSink;
    const driver = options.driver ?? eventLogDriver;
    const batchBytes = options.batchBytes ?? DEFAULT_BATCH_BYTES;
    const role = options.role ?? "reader";
    const logDir = config.logging.dir;
    const dbPath = config.eventLog.duckdbPath;

    yield* attempt("inventory", () => Deno.mkdir(dirname(dbPath), { recursive: true }));
    const instance = yield* Effect.acquireRelease(
      attempt("inventory", () => driver.open(dbPath)),
      (handle) => close("instance", handle),
    );

    /** A fresh connection with the session settings and schema every statement below assumes. */
    const connect = Effect.gen(function* () {
      const fresh = yield* attempt("inventory", () => instance.connect());
      const prepared = yield* Effect.either(Effect.gen(function* () {
        // An offset-less `ts` is read as UTC rather than in the host's zone.
        yield* attempt("inventory", () => fresh.run("SET TimeZone = 'UTC'"));
        for (const ddl of EVENTLOG_DDL) yield* attempt("inventory", () => fresh.run(ddl));
      }));
      if (prepared._tag === "Left") {
        yield* close("connection", fresh);
        return yield* Effect.fail(prepared.left);
      }
      return fresh;
    });

    /**
     * The current connection. A failed `ROLLBACK` can leave it inside a transaction, where every later
     * `BEGIN` would fail, so it is then marked `broken` and the next run replaces it.
     */
    const db = { connection: yield* connect, broken: false };
    // Added after the instance's finalizer, so it runs first (LIFO): connection, then instance.
    yield* Effect.addFinalizer(() => close("connection", db.connection));
    // Drain pending chunk finalizers while the handles are still open (LIFO: runs before the
    // connection close above).
    yield* Effect.addFinalizer(() => drainDuckDB());

    const reconnectIfBroken: Effect.Effect<void, IngestError> = Effect.suspend(() => {
      if (!db.broken) return Effect.void;
      return Effect.gen(function* () {
        yield* close("connection", db.connection);
        db.connection = yield* connect;
        db.broken = false;
      });
    });

    const lock = yield* Effect.makeSemaphore(1);

    const emit = (
      level: Level,
      traceId: string,
      payload: IngestPayload,
    ): Effect.Effect<void> =>
      Effect.gen(function* () {
        const ts = new Date(yield* Clock.currentTimeMillis).toISOString();
        yield* sink.emit(ingestEvent({
          eventId: crypto.randomUUID(),
          ts,
          level,
          role,
          pid: Deno.pid,
          traceId,
          ...payload,
        }));
      });

    // `_ingest_state` rows are never removed, by decision (ADR 0007): a row outlives its source as a
    // tombstone, so a gunzipped-in-place or restored source is recognised (`skip`), and a recreated
    // path is read as a rewrite with dedupe, never as a new file whose events land twice. One row per
    // source file is part of the derived store's reported, unbounded growth.
    const readState: Effect.Effect<Map<string, IngestStateRow>, IngestError> = Effect.map(
      attempt(
        "inventory",
        () =>
          db.connection.runAndReadAll(
            `SELECT source_file, bytes_ingested, prefix_sha256, dedupe, CAST(last_ingested_at AS VARCHAR) AS at
             FROM _ingest_state`,
          ),
      ),
      (reader) =>
        new Map(
          reader.getRowObjects().map((row) => {
            const state: IngestStateRow = {
              sourceFile: String(row.source_file),
              bytesIngested: toNumber(row.bytes_ingested),
              prefixSha256: String(row.prefix_sha256),
              dedupe: row.dedupe === true,
              lastIngestedAt: String(row.at),
            };
            return [state.sourceFile, state];
          }),
        ),
    );

    /**
     * Appends quarantine records, grouped per `rejected.jsonl`, and makes them durable — file data,
     * then its directory entry — before returning. Every caller commits the offsets that consume
     * these lines only afterwards, so a committed watermark never covers a reject whose evidence a
     * crash could still lose (ADR 0007). A failure here fails the batch, which rolls back.
     */
    const quarantine = (rejectsBySource: ReadonlyMap<SourceFile, readonly Reject[]>, at: string) =>
      Effect.forEach(
        groupByQuarantine(rejectsBySource),
        ([relative, lines]) =>
          attempt("insert", async () => {
            const path = join(logDir, relative);
            // A write torn earlier (a full disk) leaves a fragment with no terminator; start on a fresh
            // line, or the first retried record would be glued to it and lost as evidence.
            const torn = await endsMidLine(path);
            const file = await Deno.open(path, { create: true, append: true });
            try {
              const body = lines.map((line) => line(at)).join("\n") + "\n";
              const data = new TextEncoder().encode(torn ? `\n${body}` : body);
              let written = 0;
              while (written < data.length) written += await file.write(data.subarray(written));
              await file.syncData();
            } finally {
              file.close();
            }
            return await syncDir(dirname(path));
          }, relative).pipe(
            Effect.flatMap((note) =>
              note === undefined ? Effect.void : Effect.fail(
                new IngestError({
                  phase: "insert",
                  sourceFile: relative,
                  detail: `quarantine directory not durable: ${note}`,
                }),
              )
            ),
          ),
        { discard: true },
      );

    /**
     * Runs `work` in one transaction: `BEGIN`, `work`, `COMMIT`; on failure `ROLLBACK`, and a
     * connection whose rollback failed is marked broken so the next run replaces it. `cleanup` runs
     * last either way and cannot fail.
     */
    const inTransaction = <A>(
      work: Effect.Effect<A, IngestError>,
      cleanup: Effect.Effect<void> = Effect.void,
    ): Effect.Effect<A, IngestError> =>
      Effect.acquireUseRelease(
        attempt("commit", () => db.connection.run("BEGIN TRANSACTION")),
        () => Effect.zipLeft(work, attempt("commit", () => db.connection.run("COMMIT"))),
        (_, exit) =>
          Effect.zipRight(
            Exit.isFailure(exit)
              ? attempt("commit", () => db.connection.run("ROLLBACK")).pipe(
                Effect.catchAll((error) =>
                  Effect.zipRight(
                    Effect.sync(() => {
                      db.broken = true;
                    }),
                    Effect.logWarning("event-log rollback failed; reconnecting before the next run", error),
                  )
                ),
              )
              : Effect.void,
            cleanup,
          ),
      );

    /** Moves one file's offset, inside the caller's transaction. */
    const upsertState = (next: IngestStateRow) =>
      attempt(
        "state-update",
        () =>
          db.connection.run(UPSERT_INGEST_STATE, [
            next.sourceFile,
            next.bytesIngested,
            next.prefixSha256,
            next.dedupe,
            next.lastIngestedAt,
          ]),
        next.sourceFile,
      );

    /** One transaction: stage, insert, quarantine, move offsets, commit. */
    const commitBatch = (batch: readonly Staged[], at: string): Effect.Effect<BatchResult, IngestError> => {
      const work = Effect.gen(function* () {
        const files = batch.flatMap((item) => item.tempFile === undefined ? [] : [item.tempFile]);
        const perFile = new Map<string, { rejects: Reject[]; deduplicated: number }>(
          batch.map((item) => [item.source.path, { rejects: [...item.rejects], deduplicated: 0 }]),
        );
        let inserted = 0;
        if (files.length > 0) {
          yield* attempt("insert", () => db.connection.run(stageSql(files)));
          // Only a batch with a flagged (rewritten) file pays for the anti-join against all of history.
          const dedupe = batch.some((item) =>
            item.tempFile !== undefined && item.plan.decision.kind === "read" && item.plan.decision.dedupe
          );
          yield* attempt("insert", () => db.connection.run(rowsSql(dedupe)));
          for (const statement of insertStatements) {
            yield* attempt("insert", () => db.connection.run(statement.sql), statement.table);
          }
          const rejected = yield* attempt("insert", () => db.connection.runAndReadAll(rejectedSql));
          for (const row of rejected.getRowObjects()) {
            perFile.get(String(row.source_file))?.rejects.push({
              sourceOffset: toNumber(row.source_offset),
              reason: String(row.reject_reason) as RejectReason,
              line: String(row.line),
            });
          }
          const counts = yield* attempt("insert", () => db.connection.runAndReadAll(countsSql));
          for (const row of counts.getRowObjects()) {
            const entry = perFile.get(String(row.source_file));
            const valid = toNumber(row.valid);
            const landed = toNumber(row.inserted);
            inserted += landed;
            if (entry) entry.deduplicated = valid - landed;
          }
        }
        const rejectsBySource = new Map<SourceFile, readonly Reject[]>();
        for (const item of batch) {
          const rejects = perFile.get(item.source.path)?.rejects ?? [];
          if (rejects.length > 0) {
            rejectsBySource.set(item.source, [...rejects].sort((a, b) => a.sourceOffset - b.sourceOffset));
          }
        }
        yield* quarantine(rejectsBySource, at);
        for (const item of batch) {
          const next = item.plan.nextState;
          if (next !== undefined) yield* upsertState(next);
        }
        let rejected = 0;
        for (const rejects of rejectsBySource.values()) rejected += rejects.length;
        return { inserted, rejected, perFile };
      });

      return inTransaction(
        work,
        Effect.forEach(
          dropStagingSql,
          (sql) => attempt("insert", () => db.connection.run(sql)).pipe(Effect.ignore),
          { discard: true },
        ),
      );
    };

    const run: Effect.Effect<IngestReport, IngestError> = Effect.gen(function* () {
      const startNanos = yield* Clock.currentTimeNanos;
      const at = new Date(yield* Clock.currentTimeMillis).toISOString();
      const runId = crypto.randomUUID();
      const traceId = `ingest-${runId}`;

      const body = Effect.gen(function* () {
        yield* reconnectIfBroken;
        const sources = yield* attempt("inventory", () => inventory(logDir));
        const state = yield* readState;
        const tempDir = yield* Effect.acquireRelease(
          attempt("read", () => Deno.makeTempDir({ prefix: "mansio-ingest-" })),
          (dir) => Effect.promise(() => Deno.remove(dir, { recursive: true }).catch(() => undefined)),
        );

        let filesIngested = 0;
        let bytesIngested = 0;
        let linesIngested = 0;
        let linesRejected = 0;

        /** Commits the pending batch, reports its anomalies, and frees its temp files. */
        let pending: Staged[] = [];
        let pendingBytes = 0;
        const flush = Effect.gen(function* () {
          const batch = pending;
          pending = [];
          pendingBytes = 0;
          if (batch.length === 0) return;
          const result = yield* commitBatch(batch, at);
          linesIngested += result.inserted;
          linesRejected += result.rejected;
          for (const item of batch) {
            const { decision, end } = item.plan;
            const from = decision.kind === "read" ? decision.from : end;
            filesIngested += 1;
            bytesIngested += end - from;
            const outcome = result.perFile.get(item.source.path);
            if (outcome !== undefined && outcome.rejects.length > 0) {
              yield* emit("warn", traceId, {
                sourceFile: item.source.path,
                bytesIngested: end - from,
                runId,
                status: "error",
                errorTag: "RejectedLines",
                errorDetail: {
                  quarantine: quarantinePath(item.source),
                  rejected: outcome.rejects.length,
                  lines: outcome.rejects.slice(0, MAX_LISTED_REJECTS).map(({ sourceOffset, reason }) => ({
                    sourceOffset,
                    reason,
                  })),
                },
              });
            }
            if (decision.kind === "read" && decision.mode === "rewritten") {
              yield* emit("warn", traceId, {
                sourceFile: item.source.path,
                bytesIngested: end - from,
                runId,
                status: "error",
                errorTag: "SourceRewritten",
                errorDetail: {
                  reason: decision.rewrite,
                  previousBytesIngested: decision.previous?.bytesIngested,
                  size: decision.size,
                  deduplicated: outcome?.deduplicated ?? 0,
                },
              });
            }
            const temp = item.tempFile;
            if (temp !== undefined) yield* Effect.promise(() => Deno.remove(temp).catch(() => undefined));
          }
        });

        // Files are staged as they are planned and committed once roughly `batchBytes` of new data is
        // pending, so temp disk and memory stay bounded by the batch, not by the backlog.
        for (const [index, source] of sources.entries()) {
          const bytes = yield* Effect.either(
            attempt("read", () => Deno.readFile(join(logDir, source.path)), source.path),
          );
          if (bytes._tag === "Left") {
            // Retention may remove a file between listing and reading; that is not an anomaly.
            if (bytes.left.cause instanceof Deno.errors.NotFound) continue;
            // One unreadable file must not wedge every file after it: report it and move on.
            yield* emit("error", traceId, {
              sourceFile: source.path,
              bytesIngested: 0,
              runId,
              status: "error",
              ...fromPlatformError(bytes.left),
            });
            continue;
          }
          const plan = planFile(source.path, state.get(source.path), bytes.right, sha256Hex, at);
          if (plan.nextState === undefined) continue;
          pending.push(
            yield* attempt(
              "read",
              () => stage(source, plan, bytes.right, join(tempDir, `${index}.jsonl`)),
              source.path,
            ),
          );
          pendingBytes += plan.end - (plan.decision.kind === "read" ? plan.decision.from : plan.end);
          if (pendingBytes >= batchBytes) yield* flush;
        }
        yield* flush;
        return { filesScanned: sources.length, filesIngested, bytesIngested, linesIngested, linesRejected };
      });

      const counts = yield* Effect.scoped(body).pipe(
        Effect.tapError((error) =>
          emit("error", traceId, {
            sourceFile: error.sourceFile ?? "",
            bytesIngested: 0,
            runId,
            status: "error",
            ...fromPlatformError(error),
          })
        ),
      );
      const durationMs = Number((yield* Clock.currentTimeNanos) - startNanos) / 1e6;
      return { ...counts, durationMs };
    });

    // -------------------------------------------------------------------------
    // Ingest-then-sweep (item 36, ADR 0007)
    // -------------------------------------------------------------------------

    const retention = options.retention ?? {};
    const settleMs = retention.settleMs ?? DEFAULT_SETTLE_MS;
    const openPaths = retention.openPaths ?? Effect.succeed([] as readonly string[]);

    /**
     * Seals the partial tail of each closed source into quarantine: a source dated before today, not
     * open here, settled, whose committed offset stops at its last `\n` with only a newline-free tail
     * after it. No sink will ever finish that line — a sink abandons a file after a failed write and a
     * crashed process never returns to it — so without this the source could never be swept. In one
     * transaction: the tail is quarantined as a `torn-tail` reject (durably, before `COMMIT`) and the
     * offset moves to the end of the file. The offset then sits at the end of a partial line rather
     * than just past a `\n`; a byte appended after that is out of the writers' model and would be
     * read, and quarantined, as a line of its own. Run right after a successful ingest.
     *
     * A closed source that is empty and has no row gets an empty-prefix row in the same transaction:
     * ingest never writes one (it skips a 0-byte file), and without it the source would stay
     * `no-watermark`, pinning its directory and its `rejected.jsonl` forever.
     */
    const sealTornTails = (now: Date, at: string, traceId: string, runId: string) =>
      Effect.gen(function* () {
        const today = dateBucketOf(now);
        const open = new Set((yield* openPaths).map((path) => resolve(path)));
        const sources = yield* attempt("inventory", () => inventory(logDir));
        const state = yield* readState;
        const seals: { source: SourceFile; reject: Reject; next: IngestStateRow }[] = [];
        const empties: IngestStateRow[] = [];
        for (const source of sources) {
          if (source.date >= today) continue;
          const path = join(logDir, source.path);
          if (open.has(resolve(path))) continue;
          const found = yield* attempt("read", async () => {
            const info = await Deno.lstat(path).catch(() => undefined);
            if (info === undefined || !info.isFile) return undefined;
            const age = now.getTime() - (info.mtime?.getTime() ?? now.getTime());
            if (age < 0 || age < settleMs) return undefined;
            const row = state.get(source.path);
            const empty = info.size === 0 && row === undefined;
            if (!empty && info.size <= (row?.bytesIngested ?? 0)) return undefined;
            const bytes = await Deno.readFile(path);
            const after = await Deno.lstat(path);
            if (after.size !== info.size || after.mtime?.getTime() !== info.mtime?.getTime()) {
              return undefined;
            }
            return { bytes, row };
          }, source.path);
          if (found === undefined) continue;
          const { bytes, row } = found;
          if (bytes.length === 0) {
            if (row === undefined) {
              empties.push({
                sourceFile: source.path,
                bytesIngested: 0,
                prefixSha256: sha256Hex(bytes),
                dedupe: false,
                lastIngestedAt: at,
              });
            }
            continue;
          }
          const from = row?.bytesIngested ?? 0;
          // The committed offset must still describe these bytes, and the tail must be one partial
          // line: complete lines past the offset are ingest's to read, not a torn write.
          if (row !== undefined && sha256Hex(bytes.subarray(0, from)) !== row.prefixSha256) continue;
          const tail = bytes.subarray(from);
          if (tail.includes(0x0a)) continue;
          seals.push({
            source,
            reject: {
              sourceOffset: from,
              reason: "torn-tail",
              line: lossyUtf8.decode(tail.subarray(0, QUARANTINE_LINE_BYTES)),
            },
            next: {
              sourceFile: source.path,
              bytesIngested: bytes.length,
              prefixSha256: sha256Hex(bytes),
              dedupe: row?.dedupe ?? false,
              lastIngestedAt: at,
            },
          });
        }
        if (seals.length === 0 && empties.length === 0) return [] as readonly string[];
        yield* inTransaction(Effect.gen(function* () {
          if (seals.length > 0) {
            yield* quarantine(new Map(seals.map(({ source, reject }) => [source, [reject]])), at);
          }
          for (const { next } of seals) yield* upsertState(next);
          for (const next of empties) yield* upsertState(next);
        }));
        for (const { source, reject, next } of seals) {
          yield* emit("warn", traceId, {
            sourceFile: source.path,
            bytesIngested: next.bytesIngested - reject.sourceOffset,
            runId,
            status: "error",
            errorTag: "TornTailSealed",
            errorDetail: {
              quarantine: quarantinePath(source),
              sourceOffset: reject.sourceOffset,
              tailBytes: next.bytesIngested - reject.sourceOffset,
            },
          });
        }
        return seals.map(({ source }) => source.path);
      });

    /** The committed state, as the sweep's gate reads it. Read after the ingest it follows committed. */
    const ledger: Effect.Effect<IngestLedger, RetentionLedgerError> = Effect.map(
      Effect.mapError(
        readState,
        (error) =>
          new RetentionLedgerError({
            operation: "watermarks",
            detail: error.detail ?? error.phase,
            cause: error,
          }),
      ),
      (state): IngestLedger => ({
        watermarks: new Map(
          [...state.values()].map((row) => [row.sourceFile, {
            bytesIngested: row.bytesIngested,
            prefixSha256: row.prefixSha256,
            dedupe: row.dedupe,
          }]),
        ),
        acceptedOffsets: (sources) =>
          Effect.mapError(
            acceptedOffsets(sources),
            (error) =>
              new RetentionLedgerError({
                operation: "accepted-offsets",
                detail: error.detail ?? error.phase,
                cause: error,
              }),
          ),
      }),
    );

    /** `platform_events.source_offset`s per source file, for the sweep's reconciliation. */
    const acceptedOffsets = (sources: readonly string[]) =>
      Effect.gen(function* () {
        const out = new Map<string, Set<number>>();
        for (let at = 0; at < sources.length; at += 256) {
          const chunk = sources.slice(at, at + 256);
          const reader = yield* attempt("inventory", () =>
            db.connection.runAndReadAll(
              `SELECT source_file, source_offset FROM platform_events WHERE source_file IN (${
                chunk.map(sqlString).join(", ")
              })`,
            ));
          for (const row of reader.getRowObjects()) {
            const file = String(row.source_file);
            const offsets = out.get(file) ?? new Set<number>();
            offsets.add(toNumber(row.source_offset));
            out.set(file, offsets);
          }
        }
        return out as ReadonlyMap<string, ReadonlySet<number>>;
      });

    /** The two budgets side by side, plus the logger's own drop accounting (ADR 0007). */
    const measure = (now: Date, blockedBytes: number) =>
      Effect.gen(function* () {
        const today = dateBucketOf(now);
        const tree = yield* Effect.either(retentionInventory(resolve(logDir)));
        const files = tree._tag === "Right" ? tree.right.entries : [];
        const size = (path: string) => Deno.stat(path).then((info) => info.size).catch(() => 0);
        const [counts] = (yield* attempt("inventory", () =>
          db.connection.runAndReadAll(
            `SELECT (SELECT count(*) FROM platform_events) AS events,
                    (SELECT count(*) FROM _ingest_state) AS sources,
                    (SELECT coalesce(sum(dropped), 0) FROM queue_events WHERE event = 'dropped') AS dropped`,
          ))).getRowObjects();
        const retainedBytes = files.reduce((sum, e) => sum + e.bytes, 0);
        const capBytes = config.logging.maxTotalBytes;
        return {
          jsonl: {
            retainedBytes,
            files: files.length,
            todayBytes: files.filter((e) => e.dir >= today).reduce((sum, e) => sum + e.bytes, 0),
            blockedBytes,
            capBytes,
            withinCap: retainedBytes <= capBytes,
          },
          derived: {
            duckdbBytes: yield* Effect.promise(() => size(dbPath)),
            walBytes: yield* Effect.promise(() => size(`${dbPath}.wal`)),
            platformEventRows: toNumber(counts.events),
            ingestStateRows: toNumber(counts.sources),
            boundedByLogMaxTotalBytes: false as const,
          },
          logger: { dropsRecorded: toNumber(counts.dropped) },
        } satisfies RetentionBudgets;
      });

    const ingestThenSweep = (
      call: IngestThenSweepOptions = {},
    ): Effect.Effect<IngestThenSweepReport, IngestError> =>
      lock.withPermits(1)(Effect.gen(function* () {
        const counts: IngestReport = call.dryRun === true
          ? {
            filesScanned: 0,
            filesIngested: 0,
            bytesIngested: 0,
            linesIngested: 0,
            linesRejected: 0,
            durationMs: 0,
          }
          : yield* run;
        const now = new Date(yield* Clock.currentTimeMillis);
        const runId = crypto.randomUUID();
        // A failed seal leaves every row as it was: those sources simply stay blocked this time.
        const sealed = call.dryRun === true ? { sealed: [] as readonly string[] } : yield* Effect.either(
          sealTornTails(now, now.toISOString(), `ingest-${runId}`, runId),
        ).pipe(Effect.map((result) =>
          result._tag === "Right"
            ? { sealed: result.right }
            : { sealed: [] as readonly string[], sealError: result.left.detail }
        ));
        const swept = yield* Effect.either(
          sweep({
            logDir,
            policy: policyFromConfig(config.logging),
            role,
            ledger,
            openPaths,
            settleMs,
            dryRun: call.dryRun,
          }).pipe(Effect.provideService(EventSink, sink)),
        );
        const outcome: RetentionOutcome = swept._tag === "Right"
          ? { status: "swept", ...sealed, sweep: swept.right }
          : {
            status: "failed",
            ...sealed,
            errorTag: swept.left._tag,
            error: describeRetentionError(swept.left),
          };
        // Ingest committed and the sweep ran: a failed measurement is reported, never the call's failure.
        const measured = yield* Effect.either(
          measure(now, swept._tag === "Right" ? swept.right.blockedBytes : 0),
        );
        return measured._tag === "Right"
          ? { ...counts, retention: outcome, budgets: measured.right }
          : { ...counts, retention: outcome, budgetsError: measured.left.detail ?? measured.left.phase };
      }));

    return { ingest: lock.withPermits(1)(run), ingestThenSweep };
  });
}

/** Whether `path` exists, is non-empty and does not end in `\n`. */
async function endsMidLine(path: string): Promise<boolean> {
  let file: Deno.FsFile;
  try {
    file = await Deno.open(path, { read: true });
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return false;
    throw cause;
  }
  try {
    const size = (await file.stat()).size;
    if (size === 0) return false;
    await file.seek(size - 1, Deno.SeekMode.Start);
    const last = new Uint8Array(1);
    return (await file.read(last)) === 1 && last[0] !== 0x0a;
  } finally {
    file.close();
  }
}

/** One line for a sweep failure that already emitted its own `error` lifecycle event. */
function describeRetentionError(error: { readonly _tag: string }): string {
  const { _tag, ...fields } = error as unknown as Record<string, unknown>;
  try {
    return `${_tag} ${JSON.stringify(fields)}`;
  } catch {
    return String(_tag);
  }
}

/** Builds quarantine record writers grouped by their `rejected.jsonl`, relative to `LOG_DIR`. */
function groupByQuarantine(
  rejectsBySource: ReadonlyMap<SourceFile, readonly Reject[]>,
): Map<string, ((at: string) => string)[]> {
  const groups = new Map<string, ((at: string) => string)[]>();
  for (const [source, rejects] of rejectsBySource) {
    const target = quarantinePath(source);
    const lines = groups.get(target) ?? [];
    for (const reject of rejects) {
      lines.push((at) =>
        JSON.stringify({
          rejectedAt: at,
          sourceFile: source.path,
          sourceOffset: reject.sourceOffset,
          reason: reject.reason,
          line: reject.line,
        })
      );
    }
    groups.set(target, lines);
  }
  return groups;
}

/** A layer over explicit options; `EventLogIngestLive` is this with the defaults. */
export function eventLogIngestLayer(
  options: EventLogIngestOptions = {},
): Layer.Layer<EventLogIngest, IngestError, Config | EventSink> {
  return Layer.scoped(EventLogIngest, makeEventLogIngest(options));
}

/** Live: the persistent DuckDB event log, ingesting `LOG_DIR`. Reader process only. */
export const EventLogIngestLive: Layer.Layer<EventLogIngest, IngestError, Config | EventSink> =
  eventLogIngestLayer();

/**
 * The hook for items 32 and 33 (ADR 0007): the same `EventLogIngest` tag, whose `ingest` is
 * `ingestThenSweep()`. The reader answers its existing `ingest` RPC with it — no new RPC kind, no
 * service change — and the reply is an `IngestThenSweepReport`, a superset of `IngestReport`.
 * Retention therefore runs only after a successful ingest, in the process that owns DuckDB.
 */
export function eventLogIngestThenSweepLayer(
  options: EventLogIngestOptions = {},
): Layer.Layer<EventLogIngest, IngestError, Config | EventSink> {
  return Layer.scoped(
    EventLogIngest,
    Effect.map(makeEventLogIngest(options), (control) => ({ ingest: control.ingestThenSweep() })),
  );
}

/** Live ingest-then-sweep over `Config`. Reader process only. */
export const EventLogIngestThenSweepLive: Layer.Layer<EventLogIngest, IngestError, Config | EventSink> =
  eventLogIngestThenSweepLayer();
