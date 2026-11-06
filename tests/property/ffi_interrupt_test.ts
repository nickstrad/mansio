/**
 * FFI-07, work 2: interrupt totality. A nonblocking statement (`allAsync`, and the Effect layer's
 * `allAsync`) given a random statement and a random interrupt delay of 0-50 ms ALWAYS terminates, with
 * exactly one of
 *
 * - its rows (the statement finished first, and the rows are the ones the same statement returns
 *   undisturbed), or
 * - the cancellation its mechanism promises: `SqliteCancelled` (`reason: "deadline"` for `timeoutMs`,
 *   `"aborted"` for an `AbortSignal`, either when both are armed) or, for fiber interruption through
 *   `Effect.timeout`, Effect's `TimeoutException`.
 *
 * It never hangs, and it never leaves the connection unusable: the very next statement, blocking and
 * nonblocking, answers, and `db.inFlight` is false.
 *
 * Hang safety, in three layers:
 *
 * 1. The WHOLE iteration (the statement, its outcome checks and the recovery probes) is raced against
 *    a hard wall-clock guard (`GUARD_MS`) that FAILS the property when hit. The guard is cleared only
 *    after the last probe. A hit marks the connection `stuck`, and every further run (fast-check's
 *    shrinking) returns at once: nothing is replayed against a connection whose execution timed out.
 * 2. Cleanup closes the database with `closeAsync` (which cancels an in-flight execution and waits for
 *    its pool thread). A close error is a test failure. If `closeAsync` has not finished within
 *    `CLOSE_GRACE_MS`, a native step is running that nothing in this process can stop, so the test
 *    reports it and kills the process (SIGKILL, exit 137): a stuck native execution must not survive
 *    into later tests, and abandoning it only moves the crash to runtime teardown (SIGSEGV, exit 139).
 * 3. A watchdog on its own thread (a `Worker`) kills the process with SIGKILL if the test (fixture
 *    setup, iterations, cleanup) makes no progress for `BACKSTOP_MS`. It is armed before the fixture
 *    opens the database and re-armed at each phase boundary, so it covers a synchronous stall anywhere,
 *    which blocks the event loop and so every timer on it. Its report names the phase or iteration.
 *
 * A failure report carries the iteration's inputs (statement, mechanism and delays), phase, elapsed
 * time and the host load average. The seed fixes the generated inputs, not scheduling: a timing
 * failure can recur only under similar load, so replay with `PROP_SEED` and compare the recorded load.
 *
 * Statements (one prepared set, reused across all iterations, so a cancelled statement is also reused):
 * `forever` (an unbounded recursive CTE: must be cancelled), `cte` and `join` (a few hundred ms of CPU
 * on this VM: either outcome is valid), `scan` (a few ms), and `point` and `trivial` (microseconds:
 * almost always rows, but a delay of 0 may legitimately cancel them first). The generators hit both
 * outcomes; the test asserts, with seed-independent floors, that it saw at least 20 of each overall and
 * at least one rows result and one cancellation under every mechanism (not every shape under every
 * mechanism: 200 runs do not sample all 24 combinations evenly).
 *
 * Deliberately NOT generated: statements that wait on a lock. `sqlite3_interrupt` does not cut a
 * `busy_timeout` wait (`docs/knowledge/gotchas.md`, FFI-05), so a blocked statement ends with
 * `SQLITE_BUSY` after the whole timeout rather than `SqliteCancelled`. There is one connection on one
 * temp file here, so `SQLITE_BUSY` cannot occur, and any `SqliteError` fails the property.
 *
 * `numRuns: 200` (the plan's 200 iterations), seed from `PROP_SEED` (default 20261010). Sweep:
 * `bash work-scripts/bash/property-seed-sweep.sh tests/property/ffi_interrupt_test.ts - 1 200`.
 */
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Cause, Effect, Exit, Option } from "effect";
import fc from "fast-check";
import {
  allAsync as allAsyncEffect,
  Database,
  type Row,
  SqliteCancelled,
  type Statement,
} from "../../platform/sqlite_ffi/mod.ts";
import { SEED, titled } from "./arbitraries.ts";
import { runs } from "./compiler_arbitraries.ts";

const ITERATIONS = 200;
/** Hard bound on one iteration. Far above the slowest statement; hitting it means a hang. */
const GUARD_MS = 15_000;
const CLOSE_GRACE_MS = 1_000;
/** No progress for this long and the watchdog thread kills the process (covers synchronous stalls). */
const BACKSTOP_MS = GUARD_MS + 10_000;
/** Backstop deadline for the fiber mechanism: if interruption failed, this would end the statement. */
const FIBER_BACKSTOP_MS = 8_000;

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const STATEMENTS = {
  forever: "WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c) SELECT count(*) AS n FROM c",
  cte: "WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c WHERE n < 4000000) " +
    "SELECT count(*) AS n, sum(n) AS s FROM c",
  join: "SELECT count(*) AS n FROM big a JOIN big b ON a.grp = b.grp WHERE a.id <= 4000",
  scan: "SELECT count(*) AS n, sum(length(pad)) AS s FROM big WHERE grp % 7 = 3",
  point: "SELECT id, grp, pad FROM big WHERE id = 4242",
  trivial: "SELECT 1 AS n",
} as const;
type Kind = keyof typeof STATEMENTS;
const KINDS = Object.keys(STATEMENTS) as Kind[];

const SEED_ROWS = 100_000;

type Fixture = {
  db: Database;
  statements: Record<Kind, Statement>;
  /** The undisturbed result of every statement that ends. */
  expected: Record<Exclude<Kind, "forever">, Row[]>;
  probe: Statement;
};

const plain = (rows: readonly Row[]) => rows.map((r) => ({ ...r }));

/** Set once an iteration has timed out: its execution may still be running on the connection. */
type Health = { stuck: string | null };

const loadavg = () => Deno.loadavg().map((l) => l.toFixed(2)).join("/");
/** Watchdog context for a phase outside the generated iterations (the same fields as an iteration's). */
const setupContext = (phase: string) => `${phase}, load ${loadavg()}, seed ${SEED}`;

/**
 * Report a native execution that cannot be stopped, then kill the process (SIGKILL, exit 137). Never
 * returns. `Deno.exit` is not enough: with the pool thread still inside `sqlite3_step`, runtime teardown
 * segfaults (exit 139, a core dump), which is what abandoning the execution does anyway.
 */
function abandonProcess(reason: string): never {
  console.error(
    `\nFATAL ffi_interrupt_test (seed ${SEED}, load ${loadavg()}): ${reason}\n` +
      "A native SQLite execution is still running on the blocking pool and sqlite3_interrupt did not stop it; " +
      "killing the process so it cannot outlive this test.\n",
  );
  Deno.kill(Deno.pid, "SIGKILL");
  throw new Error("unreachable: SIGKILL did not end the process");
}

/**
 * `closeAsync` cancels an in-flight execution and waits for its pool thread, so it ends unless a step
 * cannot be interrupted. Resolves "closed", or "expired" when the grace ran out first. A rejection
 * propagates (after a best-effort synchronous close so the handle is not leaked). The grace timer is
 * always cleared.
 */
async function closeWithGrace(db: Database): Promise<"closed" | "expired"> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const grace = new Promise<"expired">((resolve) => {
    timer = setTimeout(() => resolve("expired"), CLOSE_GRACE_MS);
  });
  try {
    const result = await Promise.race([db.closeAsync().then(() => "closed" as const), grace]);
    if (result === "closed") assert.equal(db.isOpen, false, "closeAsync resolved but the database is open");
    return result;
  } catch (error) {
    try {
      db.close();
    } catch { /* already reported: the close failure below is the finding */ }
    throw error;
  } finally {
    clearTimeout(timer);
  }
}

async function withFixture<T>(
  health: Health,
  beat: (context: string) => void,
  fn: (f: Fixture) => Promise<T>,
): Promise<T> {
  const dir = mkdtempSync(join(tmpdir(), "mansio-ffi-interrupt-"));
  const db = Database.open(join(dir, "interrupt.db"));
  let body: { ok: true; value: T } | { ok: false; error: unknown };
  try {
    db.exec("CREATE TABLE big(id INTEGER PRIMARY KEY, grp INTEGER NOT NULL, pad TEXT NOT NULL)");
    beat(setupContext("fixture seed insert"));
    db.prepare(
      "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?) " +
        "INSERT INTO big SELECT i, i % 50, printf('row-%08d-padding', i) FROM n",
    ).run([SEED_ROWS]);
    const statements = Object.fromEntries(
      KINDS.map((k) => [k, db.prepare(STATEMENTS[k])]),
    ) as Fixture["statements"];
    const expected = {} as Fixture["expected"];
    beat(setupContext("fixture baseline reads"));
    for (const k of KINDS) if (k !== "forever") expected[k] = plain(statements[k].all());
    assert.equal(expected.point.length, 1, "the point read finds its row");
    assert.ok(Number(expected.join[0].n) > 1_000_000, "the join is big enough to run for a while");
    beat(setupContext("fixture ready, starting iterations"));
    body = { ok: true, value: await fn({ db, statements, expected, probe: db.prepare("SELECT 42 AS x") }) };
  } catch (error) {
    body = { ok: false, error };
  }
  // Cleanup is part of the test: a close failure fails it, and so does running out of grace.
  let closeError: unknown;
  let closed: "closed" | "expired" = "closed";
  beat(setupContext("cleanup"));
  try {
    closed = await closeWithGrace(db);
  } catch (error) {
    closeError = error;
  }
  rmSync(dir, { recursive: true, force: true });
  if (closed === "expired") {
    abandonProcess(
      `closeAsync did not finish within ${CLOSE_GRACE_MS} ms` +
        (health.stuck === null ? "" : ` after: ${health.stuck}`) +
        `; db.inFlight=${db.inFlight}, db.isOpen=${db.isOpen}`,
    );
  }
  if (closeError !== undefined) {
    const failure = new Error(`closing the database failed: ${String(closeError)}`, { cause: closeError });
    if (body.ok) throw failure;
    throw new AggregateError([body.error, failure], "the property failed and the cleanup failed too");
  }
  if (!body.ok) throw body.error;
  return body.value;
}

/**
 * A watchdog on its own thread. It is armed by `beat(context)` at every step of progress and kills the
 * process with SIGKILL when `BACKSTOP_MS` pass without one: the event loop of a synchronously stalled
 * test cannot run its own timers, so only another thread can report and end that.
 */
function startWatchdog(): { beat(context: string): void; stop(): void } {
  const source = `
    let context = "no progress reported", pid = 0, ms = 0, timer;
    self.onmessage = (e) => {
      if (e.data.stop) { clearTimeout(timer); self.close(); return; }
      ({ pid, ms, context } = { ...e.data });
      clearTimeout(timer);
      timer = setTimeout(() => {
        console.error("\\nFATAL ffi_interrupt_test watchdog: no progress for " + ms + " ms; " + context);
        Deno.kill(pid, "SIGKILL");
      }, ms);
    };`;
  const url = URL.createObjectURL(new Blob([source], { type: "application/javascript" }));
  const worker = new Worker(url, { type: "module" });
  return {
    beat: (context) => worker.postMessage({ pid: Deno.pid, ms: BACKSTOP_MS, context }),
    stop: () => {
      worker.postMessage({ stop: true });
      worker.terminate();
      URL.revokeObjectURL(url);
    },
  };
}

// ---------------------------------------------------------------------------
// Generators
// ---------------------------------------------------------------------------

const MECHANISMS = ["deadline", "abort", "both", "fiber"] as const;

type Mechanism =
  | { how: "deadline"; delayMs: number }
  | { how: "abort"; delayMs: number }
  | { how: "both"; deadlineMs: number; abortMs: number }
  | { how: "fiber"; delayMs: number };

const delayArb = fc.oneof(
  { weight: 1, arbitrary: fc.constant(0) },
  { weight: 6, arbitrary: fc.integer({ min: 0, max: 50 }) },
);

const mechanismArb: fc.Arbitrary<Mechanism> = fc.oneof(
  delayArb.map((delayMs): Mechanism => ({ how: "deadline", delayMs })),
  delayArb.map((delayMs): Mechanism => ({ how: "abort", delayMs })),
  fc.tuple(delayArb, delayArb).map(([deadlineMs, abortMs]): Mechanism => ({
    how: "both",
    deadlineMs,
    abortMs,
  })),
  delayArb.map((delayMs): Mechanism => ({ how: "fiber", delayMs })),
);

/** `forever` is common enough to see many cancellations; the fast ones check the rows branch. */
const kindArb: fc.Arbitrary<Kind> = fc.oneof(
  { weight: 3, arbitrary: fc.constant<Kind>("forever") },
  { weight: 2, arbitrary: fc.constantFrom<Kind>("cte", "join") },
  { weight: 2, arbitrary: fc.constant<Kind>("scan") },
  { weight: 2, arbitrary: fc.constantFrom<Kind>("point", "trivial") },
);

// ---------------------------------------------------------------------------
// One iteration
// ---------------------------------------------------------------------------

type Outcome = { kind: "rows"; rows: Row[] } | { kind: "cancelled"; error: unknown };

/** Starts the statement under `mechanism` and settles with its rows or its failure. */
function execute(statement: Statement, mechanism: Mechanism): { done: Promise<Outcome>; cleanup(): void } {
  const timers: ReturnType<typeof setTimeout>[] = [];
  const settle = (p: Promise<Row[]>): Promise<Outcome> =>
    p.then((rows): Outcome => ({ kind: "rows", rows }), (error): Outcome => ({ kind: "cancelled", error }));
  const cleanup = () => timers.forEach(clearTimeout);
  switch (mechanism.how) {
    case "deadline":
      return { done: settle(statement.allAsync(undefined, { timeoutMs: mechanism.delayMs })), cleanup };
    case "abort": {
      const ac = new AbortController();
      timers.push(setTimeout(() => ac.abort(), mechanism.delayMs));
      return { done: settle(statement.allAsync(undefined, { signal: ac.signal })), cleanup };
    }
    case "both": {
      const ac = new AbortController();
      timers.push(setTimeout(() => ac.abort(), mechanism.abortMs));
      return {
        done: settle(statement.allAsync(undefined, { timeoutMs: mechanism.deadlineMs, signal: ac.signal })),
        cleanup,
      };
    }
    case "fiber": {
      // Interruption through the Effect layer: `Effect.timeout` interrupts the fiber, whose canceler
      // aborts the binding call and waits for the statement to return.
      const run = Effect.runPromiseExit(
        Effect.timeout(
          allAsyncEffect(statement, undefined, { timeoutMs: FIBER_BACKSTOP_MS }),
          mechanism.delayMs,
        ),
      ).then((exit) => {
        if (Exit.isSuccess(exit)) return exit.value as Row[];
        throw Option.getOrElse(Cause.failureOption(exit.cause), () => exit.cause);
      });
      return { done: settle(run), cleanup };
    }
  }
}

/** The ways `mechanism` is allowed to end a statement early. */
function isPromisedCancellation(error: unknown, mechanism: Mechanism): boolean {
  if (mechanism.how === "fiber") return Cause.isTimeoutException(error);
  if (!(error instanceof SqliteCancelled)) return false;
  if (mechanism.how === "deadline") {
    return error.reason === "deadline" && error.timeoutMs === mechanism.delayMs;
  }
  if (mechanism.how === "abort") return error.reason === "aborted";
  return error.reason === "deadline" ? error.timeoutMs === mechanism.deadlineMs : error.reason === "aborted";
}

Deno.test(
  titled(
    `ffi interrupt: a nonblocking step always ends in rows or its promised cancellation (${ITERATIONS} runs)`,
  ),
  async () => {
    const tally: Record<string, number> = { rows: 0, cancelled: 0 };
    for (const k of KINDS) tally[k] = 0;
    for (const m of MECHANISMS) tally[`${m}:cancelled`] = tally[`${m}:rows`] = 0;
    let slowest = 0;
    const health: Health = { stuck: null };
    const watchdog = startWatchdog();
    try {
      watchdog.beat(setupContext("fixture open"));
      await withFixture(health, watchdog.beat, async ({ db, statements, expected, probe }) => {
        await fc.assert(
          fc.asyncProperty(kindArb, mechanismArb, async (kind, mechanism) => {
            // After a timeout the connection may still be executing: do not replay (shrink) against it.
            if (health.stuck !== null) return;
            const started = performance.now();
            let phase = "statement";
            const report = () =>
              `${kind} under ${JSON.stringify(mechanism)}, phase ${phase}, ` +
              `${(performance.now() - started).toFixed(0)} ms in, load ${loadavg()}, ` +
              `db.inFlight=${db.inFlight}, seed ${SEED}`;
            watchdog.beat(`iteration started: ${report()}`);
            const { done, cleanup } = execute(statements[kind], mechanism);
            // The whole iteration, recovery probes included, is under the guard.
            const iteration = (async () => {
              const outcome = await done;
              tally[kind]++;
              tally[outcome.kind]++;
              tally[`${mechanism.how}:${outcome.kind}`]++;
              if (outcome.kind === "rows") {
                assert.notEqual(kind, "forever", "a statement that never ends returned rows");
                assert.deepStrictEqual(plain(outcome.rows), expected[kind as Exclude<Kind, "forever">]);
              } else {
                assert.ok(
                  isPromisedCancellation(outcome.error, mechanism),
                  `${kind} under ${JSON.stringify(mechanism)} failed with ${String(outcome.error)} ` +
                    `(${JSON.stringify(outcome.error)}), not its promised cancellation`,
                );
              }
              // The connection is reusable at once: released, no interrupt left over for the next statement.
              phase = "recovery";
              assert.equal(db.inFlight, false, "the connection is still held after the step settled");
              assert.equal(probe.get()?.x, 42);
              phase = "recovery (async probe)";
              assert.equal((await probe.getAsync())?.x, 42);
              assert.equal(statements.point.get()?.pad, "row-00004242-padding");
              phase = "done";
            })();
            iteration.catch(() => {}); // a late rejection after the guard fired is not unhandled
            let guard: ReturnType<typeof setTimeout> | undefined;
            const timeout = new Promise<never>((_, reject) => {
              guard = setTimeout(() => {
                health.stuck = report();
                reject(new Error(`HANG: the iteration did not finish in ${GUARD_MS} ms`));
              }, GUARD_MS);
            });
            try {
              await Promise.race([iteration, timeout]);
              slowest = Math.max(slowest, performance.now() - started);
              watchdog.beat(`iteration finished: ${report()}`);
            } catch (error) {
              throw new Error(
                `${error instanceof Error ? error.message : String(error)}\n  iteration: ${report()}`,
                {
                  cause: error,
                },
              );
            } finally {
              clearTimeout(guard);
              cleanup();
            }
          }),
          runs(ITERATIONS),
        );
      });
    } finally {
      watchdog.stop();
    }
    console.log(
      `interrupt coverage (seed ${SEED}): ${JSON.stringify(tally)}, slowest iteration ${
        slowest.toFixed(0)
      } ms, load ${loadavg()}`,
    );
    // Seed-independent lower bounds: both outcomes overall, and both under every mechanism.
    assert.ok(tally.rows >= 20, `too few completions: ${JSON.stringify(tally)}`);
    assert.ok(tally.cancelled >= 20, `too few cancellations: ${JSON.stringify(tally)}`);
    for (const m of MECHANISMS) {
      assert.ok(tally[`${m}:cancelled`] > 0, `${m} never cancelled: ${JSON.stringify(tally)}`);
      assert.ok(tally[`${m}:rows`] > 0, `${m} never completed: ${JSON.stringify(tally)}`);
    }
    for (const k of KINDS) assert.ok(tally[k] > 0, `${k} never ran`);
  },
);
