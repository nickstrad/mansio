/**
 * Property test for plan item 36 (ADR 0007): retention follows committed ingest across seeded
 * interleavings of append, rotate, torn write, day change, late append (also racing inside a run:
 * right after ingest commits, or after the gate read the files), ingest-then-sweep, injected commit
 * failure, interruption, reader restart and gzip-crash leftovers — against a real `LOG_DIR`, the
 * real `EventLogIngestLive` coordinator (`makeEventLogIngest(...).ingestThenSweep`) and a real DuckDB file.
 *
 * The model is the writers' protocol `eventlog/file_sink.ts` documents: each writer appends
 * whole lines to its current file, rolls to a fresh `seq` (or, on a day change, a fresh dated
 * directory), and abandons a file after a torn write. A late append to a closed file breaks that
 * protocol on purpose; the gate must still lose nothing. Every case runs one reader "process" at a
 * time, restarted by the `restart` op; time is a virtual clock that only the `advance` op moves.
 *
 * Checked after every ingest-then-sweep (successful, failed or interrupted):
 *
 * 1. **Exactly once.** No `event_id` is in `platform_events` twice.
 * 2. **Nothing un-ingested becomes inaccessible.** Every valid complete line whose `.jsonl` is gone
 *    (deleted, or gzipped) is in `platform_events`.
 * 3. **Rejected lines are consumed only with durable evidence.** Every malformed line and every torn
 *    tail whose `.jsonl` is gone has its quarantine record in its directory's `rejected.jsonl` — unless
 *    that whole directory is gone, quarantine file and all.
 *
 * And once the history ends, after a restart and a jump past `keepDays`: every valid line ever
 * written is ingested exactly once, and retention has removed every file, with no backlog left.
 *
 * Real I/O and a real DuckDB file per case make each case expensive, so this property runs
 * `CASES` cases rather than the suite's `NUM_RUNS`; the seed is the suite's (`PROP_SEED`).
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { dirname, join } from "node:path";
import { gzipSync } from "node:zlib";
import fc from "fast-check";
import { Clock, Effect, Exit, Layer, Scope } from "effect";
import { type EventRecord, lifecycleEvent, toJsonLine } from "../../core/events.ts";
import { ConfigTest } from "../../eventlog/config.ts";
import { EventSink } from "../../eventlog/services.ts";
import {
  type EventLogConnection,
  type EventLogDriver,
  eventLogDriver,
  type EventLogIngestControl,
  type IngestThenSweepReport,
  makeEventLogIngest,
} from "../../eventlog/ingest.ts";
import { titled } from "./arbitraries.ts";

const CASES = 20;

/** How often each outcome occurred across the whole property; printed, and floored at the end. */
const seen = {
  gzipped: 0,
  deleted: 0,
  sealed: 0,
  skipped: 0,
  commitFailures: 0,
  interrupted: 0,
  blocked: {} as Record<string, number>,
};
const DAY_MS = 24 * 60 * 60 * 1000;
const KEEP_DAYS = 4;

// ---------------------------------------------------------------------------
// Ops
// ---------------------------------------------------------------------------

type Op =
  | { readonly kind: "append"; readonly writer: number; readonly n: number }
  | { readonly kind: "malformed"; readonly writer: number }
  | { readonly kind: "torn"; readonly writer: number }
  | { readonly kind: "roll"; readonly writer: number }
  | { readonly kind: "advance"; readonly days: number }
  | { readonly kind: "late"; readonly pick: number }
  | { readonly kind: "leftover"; readonly pick: number }
  | {
    readonly kind: "ingest";
    readonly fault: "none" | "commit" | { readonly interruptAt: number };
    /** A late append to a closed file inside the run: after ingest committed, or after the gate read. */
    readonly race: "none" | "after-commit" | "after-gate";
    readonly pick: number;
  }
  | { readonly kind: "restart" };

const writerArb = fc.integer({ min: 0, max: 1 });
const opArb: fc.Arbitrary<Op> = fc.oneof(
  {
    weight: 6,
    arbitrary: fc.record({
      kind: fc.constant("append" as const),
      writer: writerArb,
      n: fc.integer({ min: 1, max: 4 }),
    }),
  },
  { weight: 1, arbitrary: fc.record({ kind: fc.constant("malformed" as const), writer: writerArb }) },
  { weight: 1, arbitrary: fc.record({ kind: fc.constant("torn" as const), writer: writerArb }) },
  { weight: 1, arbitrary: fc.record({ kind: fc.constant("roll" as const), writer: writerArb }) },
  {
    weight: 3,
    arbitrary: fc.record({ kind: fc.constant("advance" as const), days: fc.integer({ min: 1, max: 3 }) }),
  },
  { weight: 1, arbitrary: fc.record({ kind: fc.constant("late" as const), pick: fc.nat() }) },
  { weight: 1, arbitrary: fc.record({ kind: fc.constant("leftover" as const), pick: fc.nat() }) },
  {
    weight: 6,
    arbitrary: fc.record({
      kind: fc.constant("ingest" as const),
      fault: fc.oneof(
        { weight: 4, arbitrary: fc.constant("none" as const) },
        { weight: 2, arbitrary: fc.constant("commit" as const) },
        { weight: 2, arbitrary: fc.record({ interruptAt: fc.integer({ min: 1, max: 60 }) }) },
      ),
      race: fc.constantFrom("none" as const, "after-commit" as const, "after-gate" as const),
      pick: fc.nat(),
    }),
  },
  { weight: 1, arbitrary: fc.record({ kind: fc.constant("restart" as const) }) },
);

const caseArb = fc.record({
  ops: fc.array(opArb, { minLength: 4, maxLength: 18 }),
  // From a cap that prunes almost everything to one that never binds.
  capBytes: fc.oneof(fc.integer({ min: 500, max: 6_000 }), fc.constant(Number.MAX_SAFE_INTEGER)),
});

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

interface LineModel {
  readonly kind: "valid" | "malformed" | "torn";
  readonly id: string;
  readonly offset: number;
}

interface FileModel {
  readonly rel: string;
  size: number;
  torn: boolean;
  readonly lines: LineModel[];
}

const encoder = new TextEncoder();
const pad = (n: number, width: number) => String(n).padStart(width, "0");

function bucketOf(ms: number): string {
  const d = new Date(ms);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1, 2)}-${pad(d.getDate(), 2)}`;
}

function line(id: string): string {
  return toJsonLine(lifecycleEvent({
    eventId: id,
    ts: "2026-10-11T12:00:00.000Z",
    level: "info",
    role: "server",
    pid: 1,
    traceId: "prop",
    phase: "prop",
    detail: {},
  }));
}

async function exists(path: string): Promise<boolean> {
  try {
    await Deno.lstat(path);
    return true;
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return false;
    throw cause;
  }
}

/** One case's world: the tree, the clock, the reader "process", and the model of what was written. */
class World {
  readonly files = new Map<string, FileModel>();
  readonly writers = [
    { role: "server", pid: 100, seq: 0, current: undefined as string | undefined },
    { role: "reader", pid: 200, seq: 0, current: undefined as string | undefined },
  ];
  private ids = 0;
  readonly clock: Clock.Clock;
  readonly time: { ms: number };
  private session?: { scope: Scope.CloseableScope; control: EventLogIngestControl };
  private instance?: { connect(): Promise<EventLogConnection> };
  /** Fault injection for the next ingest call. */
  private fault: {
    failCommit: boolean;
    interruptAt?: number;
    seen: number;
    signal?: () => void;
    race?: { at: "after-commit" | "after-gate"; pick: number };
  } = { failCommit: false, seen: 0 };

  constructor(readonly root: string, readonly capBytes: number) {
    // Ahead of the real clock, so every real mtime is settled by virtual time.
    const start = new Date(Date.now() + 2 * DAY_MS);
    start.setHours(12, 0, 0, 0);
    this.time = { ms: start.getTime() };
    const real = Clock.make();
    const time = this.time;
    this.clock = {
      [Clock.ClockTypeId]: Clock.ClockTypeId,
      unsafeCurrentTimeMillis: () => time.ms,
      currentTimeMillis: Effect.sync(() => time.ms),
      unsafeCurrentTimeNanos: () => BigInt(time.ms) * 1_000_000n,
      currentTimeNanos: Effect.sync(() => BigInt(time.ms) * 1_000_000n),
      sleep: (duration) => real.sleep(duration),
    };
  }

  get logDir(): string {
    return join(this.root, "logs");
  }

  private nextId(): string {
    return `id-${++this.ids}`;
  }

  // --- the reader process ---------------------------------------------------

  private get driver(): EventLogDriver {
    return {
      open: async (path) => {
        const instance = await eventLogDriver.open(path);
        this.instance = instance;
        return {
          closeSync: () => instance.closeSync(),
          connect: async () => {
            const connection = await instance.connect();
            const guard = async (sql: string) => {
              this.fault.seen += 1;
              if (this.fault.interruptAt !== undefined && this.fault.seen === this.fault.interruptAt) {
                this.fault.signal?.();
                await new Promise((resolve) => setTimeout(resolve, 5));
                throw new Error("statement abandoned by an interruption");
              }
              if (this.fault.failCommit && sql === "COMMIT") {
                this.fault.failCommit = false;
                throw new Error("injected commit failure");
              }
            };
            /** Fires the pending race, once, right after the statement it waits for. */
            const after = async (sql: string) => {
              const race = this.fault.race;
              if (race === undefined) return;
              const hit = race.at === "after-commit"
                ? sql === "COMMIT"
                : sql.includes("SELECT source_file, source_offset FROM platform_events");
              if (!hit) return;
              this.fault.race = undefined;
              await this.late(race.pick);
            };
            return {
              closeSync: () => connection.closeSync(),
              run: async (sql, values) => {
                await guard(sql);
                const result = await connection.run(sql, values);
                await after(sql);
                return result;
              },
              runAndReadAll: async (sql) => {
                await guard(sql);
                const result = await connection.runAndReadAll(sql);
                await after(sql);
                return result;
              },
            };
          },
        };
      },
    };
  }

  private layers() {
    const sink = Layer.succeed(EventSink, { emit: (_: EventRecord) => Effect.void });
    const config = ConfigTest({
      logging: {
        dir: this.logDir,
        retentionDays: KEEP_DAYS,
        gzipAfterDays: 1,
        maxTotalBytes: this.capBytes,
      },
      eventLog: { duckdbPath: join(this.root, "duckdb", "events.duckdb") },
    });
    return Layer.merge(config, sink);
  }

  async start(): Promise<void> {
    const scope = Effect.runSync(Scope.make());
    const control = await Effect.runPromise(
      Scope.extend(makeEventLogIngest({ driver: this.driver }), scope).pipe(
        Effect.provide(this.layers()),
        Effect.withClock(this.clock),
      ),
    );
    this.session = { scope, control };
  }

  async stop(): Promise<void> {
    if (this.session === undefined) return;
    await Effect.runPromise(Scope.close(this.session.scope, Exit.void));
    this.session = undefined;
    this.instance = undefined;
  }

  /** One ingest-then-sweep, with the requested fault. Returns whether it completed successfully. */
  async ingestThenSweep(
    fault: Extract<Op, { kind: "ingest" }>["fault"] = "none",
    race?: { at: "after-commit" | "after-gate"; pick: number },
  ): Promise<boolean> {
    const control = this.session!.control;
    this.fault = { failCommit: fault === "commit", seen: 0, race };
    let call: Effect.Effect<IngestThenSweepReport | void, unknown> = control.ingestThenSweep();
    if (typeof fault === "object") {
      this.fault.interruptAt = fault.interruptAt;
      const signalled = new Promise<void>((resolve) => (this.fault.signal = resolve));
      call = Effect.raceFirst(call, Effect.flatMap(Effect.promise(() => signalled), () => Effect.interrupt));
    }
    const exit = await Effect.runPromiseExit(call.pipe(Effect.withClock(this.clock)));
    this.fault = { failCommit: false, seen: 0 };
    if (Exit.isSuccess(exit) && exit.value !== undefined) {
      const { retention } = exit.value;
      seen.sealed += retention.sealed.length;
      if (retention.status === "swept") {
        seen.gzipped += retention.sweep.gzipped.length;
        seen.deleted += retention.sweep.deleted.length;
        seen.skipped += retention.sweep.skipped.length;
        for (const b of retention.sweep.blocked) seen.blocked[b.reason] = (seen.blocked[b.reason] ?? 0) + 1;
      }
    } else if (Exit.isFailure(exit)) {
      if (fault === "commit") seen.commitFailures += 1;
      else seen.interrupted += 1;
    }
    return Exit.isSuccess(exit);
  }

  async query(sql: string): Promise<Record<string, unknown>[]> {
    const connection = await this.instance!.connect();
    try {
      return (await connection.runAndReadAll(sql)).getRowObjects();
    } finally {
      connection.closeSync();
    }
  }

  // --- the writers ----------------------------------------------------------

  private async write(rel: string, text: string): Promise<number> {
    const path = join(this.logDir, rel);
    await Deno.mkdir(dirname(path), { recursive: true });
    const bytes = encoder.encode(text);
    await Deno.writeFile(path, bytes, { append: true });
    return bytes.length;
  }

  /** The writer's current file, opening a fresh `seq` in today's directory when it has none. */
  private currentFile(index: number): FileModel {
    const writer = this.writers[index];
    if (writer.current === undefined) {
      writer.seq += 1;
      const rel = `${writer.role}/${bucketOf(this.time.ms)}/events-12-${writer.pid}-${
        pad(writer.seq, 3)
      }.jsonl`;
      this.files.set(rel, { rel, size: 0, torn: false, lines: [] });
      writer.current = rel;
    }
    return this.files.get(writer.current)!;
  }

  async append(index: number, kind: "valid" | "malformed", n = 1): Promise<void> {
    const file = this.currentFile(index);
    for (let i = 0; i < n; i++) {
      const id = this.nextId();
      const text = kind === "valid" ? `${line(id)}\n` : `malformed ${id} {\n`;
      file.lines.push({ kind, id, offset: file.size });
      file.size += await this.write(file.rel, text);
    }
  }

  /** A torn write: half a line, then the writer abandons the file, as the sink does. */
  async torn(index: number): Promise<void> {
    const file = this.currentFile(index);
    const id = this.nextId();
    file.lines.push({ kind: "torn", id, offset: file.size });
    file.size += await this.write(file.rel, `{"eventId":"${id}","ts":"2026-`);
    file.torn = true;
    this.writers[index].current = undefined;
  }

  roll(index: number): void {
    this.writers[index].current = undefined;
  }

  advance(days: number): void {
    this.time.ms += days * DAY_MS;
    for (const writer of this.writers) writer.current = undefined;
  }

  /** Closed, untorn files still on disk as `.jsonl`, with no `.gz` beside them. */
  private async closedFiles(): Promise<FileModel[]> {
    const current = new Set(this.writers.map((w) => w.current));
    const out: FileModel[] = [];
    for (const file of this.files.values()) {
      if (current.has(file.rel) || file.torn) continue;
      const path = join(this.logDir, file.rel);
      if (await exists(path) && !(await exists(`${path}.gz`))) out.push(file);
    }
    return out;
  }

  /** Breaks the protocol: a complete line appended to a file its writer has left. */
  async late(pick: number): Promise<void> {
    const candidates = await this.closedFiles();
    if (candidates.length === 0) return;
    const file = candidates[pick % candidates.length];
    const id = this.nextId();
    file.lines.push({ kind: "valid", id, offset: file.size });
    file.size += await this.write(file.rel, `${line(id)}\n`);
  }

  /** What a gzip killed after its rename leaves: a complete `.gz` beside its source, plus an old `.tmp`. */
  async leftover(pick: number): Promise<void> {
    const today = bucketOf(this.time.ms);
    const candidates = (await this.closedFiles()).filter((f) => f.rel.split("/")[1] < today);
    if (candidates.length === 0) return;
    const file = candidates[pick % candidates.length];
    const path = join(this.logDir, file.rel);
    const info = await Deno.stat(path);
    await Deno.writeFile(`${path}.gz`, gzipSync(await Deno.readFile(path)));
    await Deno.utime(`${path}.gz`, info.mtime!, info.mtime!);
    const old = new Date(info.mtime!.getTime() - 2 * 60 * 60 * 1000);
    await Deno.writeTextFile(`${path}.gz.999.tmp`, "partial");
    await Deno.utime(`${path}.gz.999.tmp`, old, old);
  }

  // --- invariants -----------------------------------------------------------

  async check(label: string): Promise<void> {
    const rows = await this.query("SELECT event_id, count(*) AS n FROM platform_events GROUP BY 1");
    const ingested = new Map(rows.map((r) => [String(r.event_id), Number(r.n)]));
    for (const [id, n] of ingested) assert.equal(n, 1, `${label}: ${id} ingested ${n} times`);
    const quarantines = new Map<string, Set<string>>();
    for (const file of this.files.values()) {
      const path = join(this.logDir, file.rel);
      if (await exists(path)) continue;
      for (const l of file.lines) {
        if (l.kind === "valid") {
          assert.ok(ingested.has(l.id), `${label}: ${l.id} of ${file.rel} became inaccessible un-ingested`);
          continue;
        }
        const dir = dirname(path);
        if (!(await exists(dir))) continue;
        let records = quarantines.get(dir);
        if (records === undefined) {
          records = new Set();
          const q = join(dir, "rejected.jsonl");
          if (await exists(q)) {
            for (const text of (await Deno.readTextFile(q)).split("\n")) {
              if (text === "") continue;
              const record = JSON.parse(text) as { sourceFile: string; sourceOffset: number };
              records.add(`${record.sourceFile}@${record.sourceOffset}`);
            }
          }
          quarantines.set(dir, records);
        }
        assert.ok(
          records.has(`${file.rel}@${l.offset}`),
          `${label}: ${l.kind} line at ${file.rel}@${l.offset} is gone without quarantine evidence`,
        );
      }
    }
  }

  /** Every file under LOG_DIR's role trees. */
  async remaining(): Promise<string[]> {
    const found: string[] = [];
    const walk = async (dir: string): Promise<void> => {
      for await (const entry of Deno.readDir(dir)) {
        const full = join(dir, entry.name);
        if (entry.isDirectory) await walk(full);
        else found.push(full.slice(this.logDir.length + 1));
      }
    };
    if (await exists(this.logDir)) await walk(this.logDir);
    return found.sort();
  }

  validIds(): string[] {
    return [...this.files.values()].flatMap((f) =>
      f.lines.filter((l) => l.kind === "valid").map((l) => l.id)
    );
  }
}

async function runCase(ops: readonly Op[], capBytes: number): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "retention-ingest-prop-" });
  const world = new World(root, capBytes);
  try {
    await world.start();
    for (const [index, op] of ops.entries()) {
      switch (op.kind) {
        case "append":
          await world.append(op.writer, "valid", op.n);
          break;
        case "malformed":
          await world.append(op.writer, "malformed");
          break;
        case "torn":
          await world.torn(op.writer);
          break;
        case "roll":
          world.roll(op.writer);
          break;
        case "advance":
          world.advance(op.days);
          break;
        case "late":
          await world.late(op.pick);
          break;
        case "leftover":
          await world.leftover(op.pick);
          break;
        case "restart":
          await world.stop();
          await world.start();
          break;
        case "ingest":
          await world.ingestThenSweep(
            op.fault,
            op.race === "none" ? undefined : { at: op.race, pick: op.pick },
          );
          await world.check(`after op ${index} (${JSON.stringify(op)})`);
          break;
      }
    }

    // The history ends: restart, then jump past keepDays so that everything is retention's to remove.
    await world.stop();
    await world.start();
    world.advance(KEEP_DAYS + 2);
    for (let i = 0; i < 2; i++) {
      assert.equal(await world.ingestThenSweep(), true, "a clean run succeeds");
      await world.check(`final run ${i}`);
    }
    const ingested = new Set(
      (await world.query("SELECT event_id FROM platform_events")).map((r) => String(r.event_id)),
    );
    const valid = world.validIds();
    assert.equal(ingested.size, valid.length, "every valid line ingested, and nothing else");
    for (const id of valid) assert.ok(ingested.has(id), `${id} was never ingested`);
    assert.deepEqual(await world.remaining(), [], "retention removed every file, with no backlog left");
  } finally {
    await world.stop();
    await Deno.remove(root, { recursive: true });
  }
}

test(
  titled(
    "retention follows committed ingest: exactly once, nothing un-ingested lost, across crash interleavings",
  ),
  { timeout: 600_000 },
  async () => {
    // Anchor the rare stale-watermark and post-gate races independently of the random seed.
    await runCase([
      { kind: "append", writer: 0, n: 1 },
      { kind: "advance", days: 1 },
      { kind: "ingest", fault: "none", race: "after-commit", pick: 0 },
      { kind: "ingest", fault: "none", race: "after-gate", pick: 0 },
    ], Number.MAX_SAFE_INTEGER);
    await fc.assert(
      fc.asyncProperty(caseArb, ({ ops, capBytes }) => runCase(ops, capBytes)),
      { numRuns: CASES },
    );
    console.log(`retention/ingest outcomes over ${CASES} cases + race anchor: ${JSON.stringify(seen)}`);
    // The property only means something if the interleavings actually reach these outcomes.
    for (const key of ["gzipped", "deleted", "sealed", "commitFailures", "interrupted"] as const) {
      assert.ok(seen[key] > 0, `no case produced ${key}; retune the op weights`);
    }
    assert.ok(
      (seen.blocked["unread-tail"] ?? 0) > 0,
      "no late append was ever blocked by its stale watermark",
    );
    assert.ok(seen.skipped > 0, "no append after the gate was ever caught by the recheck");
  },
);
