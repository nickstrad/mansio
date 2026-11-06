/**
 * Property tests for the transaction commit path of the `Storage` service (plan item 15).
 *
 * Every property runs against `StorageTest` (the item-20 in-memory double: fast, and kept honest by
 * the conformance suite in `tests/core_test.ts`) and against `StorageLive` (real SQLite through the
 * FFI binding over a temp file, `BEGIN IMMEDIATE` … `COMMIT` / `ROLLBACK`):
 *
 * 1. **Atomicity.** A random sequence of batches, each a random list of document operations (insert,
 *    increment the counter, edit or delete an item) with an optional injected failure at a random
 *    point: a typed handler failure, a storage operation that fails validation, a patch of a missing
 *    document, a defect, an interruption of the mutation fiber, or a result that cannot be serialized
 *    (which fails after every operation has run). After each batch the stored state is either exactly
 *    the state before it (failed: nothing visible) or exactly a serial model of every operation the
 *    handler reported (committed: everything visible), down to each document's `_version`.
 * 2. **No lost updates.** N writers race read-modify-write increments of one counter, yielding between
 *    the read and the write, and some fail after writing. The final counter is the start plus the
 *    deltas of exactly the committed writers, its `_version` advanced once per committed writer, and
 *    the committed writers' (read, wrote) pairs chain into one serial history.
 * 3. **Single-writer serialization.** N concurrent mutations log entering and leaving their handler:
 *    the log never interleaves two handlers, and the `_version` each committed writer assigned is
 *    `2, 3, …, S + 1` in execution order — no gaps, no duplicates, and rolled-back writers spend none.
 * 4. **Commit ordering** (`StorageLive` only: the `tx` events come from the live layer, and the double
 *    emits no events by design, see `tests/layers/storage_test_layer.ts`). Each writer's transaction
 *    emits exactly one `tx` event, and the committed ones appear in the order their versions were
 *    assigned.
 *
 * Each property opens one `Storage` for all of its runs (opening SQLite per run would dominate the
 * run time). Runs cannot see each other's documents: every run stamps its own `owner` on what it
 * creates and filters on it in every read, so a shrink replay starts from a clean namespace too.
 *
 * Seed and failure reporting come from `./arbitraries.ts`; `PROP_SEED=<n>` replays a run.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { join } from "node:path";
import fc from "fast-check";
import { Cause, Deferred, Effect, Exit, Fiber, Layer, ManagedRuntime, Option } from "effect";
import { ConflictError } from "../../core/errors.ts";
import type { EventRecord, TxEventRecord } from "../../core/events.ts";
import { ConfigTest } from "../../platform/config.ts";
import { EventSink, IdGen, Storage } from "../../platform/services.ts";
import { StorageLive } from "../../platform/sqlite.ts";
import { defineFunctions } from "../../sdk/functions.ts";
import { defineSchema, defineTable, v } from "../../sdk/schema.ts";
import { IdGenTest, StorageTest } from "../layers/index.ts";
import { NUM_RUNS, replayCommand, SEED, titled } from "./arbitraries.ts";

// ---------------------------------------------------------------------------
// Schema, functions and backends
// ---------------------------------------------------------------------------

const schema = defineSchema({
  counters: defineTable({ owner: v.string(), n: v.integer() }),
  items: defineTable({ owner: v.string(), value: v.integer() }),
});

const app = defineFunctions(schema);

/** Runs per property against `StorageLive`: every transaction there is a real fsync (`synchronous=FULL`). */
const LIVE_RUNS = Math.min(NUM_RUNS, 100);

type BackendName = "StorageTest" | "StorageLive";
const BACKENDS: readonly BackendName[] = ["StorageTest", "StorageLive"];

interface Backend {
  readonly name: BackendName;
  readonly numRuns: number;
  /** Runs `effect` against this property's one `Storage`. The error side includes a failed layer build. */
  run<A, E>(effect: Effect.Effect<A, E, Storage>): Promise<Exit.Exit<A, unknown>>;
  /** Every event `StorageLive` emitted so far; always empty for `StorageTest`. */
  readonly events: EventRecord[];
}

/** Builds one `Storage` for the duration of `body`, then closes it (and removes the live file). */
async function withBackend(name: BackendName, body: (backend: Backend) => Promise<void>): Promise<void> {
  const events: EventRecord[] = [];
  let dir: string | undefined;
  let layer: Layer.Layer<Storage, unknown>;
  if (name === "StorageTest") {
    layer = StorageTest({ schema }).pipe(Layer.provide(IdGenTest));
  } else {
    dir = await Deno.makeTempDir({ prefix: "mansio-commit-prop-" });
    let id = 0;
    layer = StorageLive({ file: join(dir, "app.sqlite"), schema, appId: "commit-prop" }).pipe(
      Layer.provide(Layer.mergeAll(
        Layer.succeed(EventSink, { emit: (event) => Effect.sync(() => void events.push(event)) }),
        Layer.succeed(IdGen, { next: Effect.sync(() => `id-${++id}`) }),
        ConfigTest(),
      )),
    );
  }
  const runtime = ManagedRuntime.make(layer);
  try {
    await body({
      name,
      numRuns: name === "StorageTest" ? NUM_RUNS : LIVE_RUNS,
      run: (effect) => runtime.runPromiseExit(effect),
      events,
    });
  } finally {
    await runtime.dispose();
    if (dir !== undefined) await Deno.remove(dir, { recursive: true });
  }
}

/** The value of `effect`, or its failure rethrown (an `AssertionError` as itself). */
async function value<A, E>(backend: Backend, effect: Effect.Effect<A, E, Storage>): Promise<A> {
  const exit = await backend.run(effect);
  if (Exit.isSuccess(exit)) return exit.value;
  const failure = Cause.squash(exit.cause);
  if (failure instanceof Error && failure.name === "AssertionError") throw failure;
  throw new Error(Cause.pretty(exit.cause), { cause: failure });
}

const mutate = (definition: Parameters<Storage["Type"]["mutation"]>[0], args: unknown) =>
  Effect.flatMap(Storage, (storage) => storage.mutation(definition, args));

/** How an exit ended: `ok`, the failure's `_tag`, `Defect` or `Interrupted`. */
function outcome(exit: Exit.Exit<unknown, unknown>): string {
  if (Exit.isSuccess(exit)) return "ok";
  if (Cause.isInterruptedOnly(exit.cause)) return "Interrupted";
  const failure = Cause.failureOption(exit.cause);
  if (Option.isSome(failure)) return (failure.value as { _tag?: string })._tag ?? "Untagged";
  return "Defect";
}

let runCounter = 0;
/** A fresh namespace for one property run. */
const nextOwner = () => `run-${++runCounter}`;

// ---------------------------------------------------------------------------
// Setup and observation
// ---------------------------------------------------------------------------

/** One counter at `n` (`_version` 1) and one item per value, all stamped with `owner`. */
const setup = app.mutation({
  args: { owner: v.string(), n: v.integer(), values: v.array(v.integer()) },
  handler: ({ db }, { owner, n, values }) =>
    Effect.gen(function* () {
      const counterId = yield* db.insert("counters", { owner, n });
      for (const value of values) yield* db.insert("items", { owner, value });
      return counterId;
    }),
});

interface ItemState {
  readonly id: string;
  readonly value: number;
  readonly version: number;
}

/** Everything a run owns, in a canonical order. Compared with `deepEqual`, so `_version` counts too. */
interface State {
  readonly counter: { readonly n: number; readonly version: number };
  readonly items: readonly ItemState[];
}

const byId = (a: ItemState, b: ItemState) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);

const inspect = app.mutation({
  args: { owner: v.string() },
  handler: ({ db }, { owner }) =>
    Effect.gen(function* () {
      const counters = yield* db.find("counters", { owner }).limit(1000).collect();
      const items = yield* db.find("items", { owner }).limit(1000).collect();
      assert.equal(counters.length, 1, "each run owns exactly one counter");
      const state: State = {
        counter: { n: counters[0].n, version: counters[0]._version },
        items: items.map((item) => ({ id: item._id, value: item.value, version: item._version })).sort(byId),
      };
      return state;
    }),
});

const observe = (backend: Backend, owner: string) =>
  value(backend, mutate(inspect, { owner })) as Promise<State>;

// ---------------------------------------------------------------------------
// 1. Atomicity
// ---------------------------------------------------------------------------

/** A document operation. `pick` selects an existing item by index (mod the count) at the time it runs. */
type Op =
  | { readonly kind: "insert"; readonly value: number }
  | { readonly kind: "bump"; readonly delta: number }
  | { readonly kind: "edit"; readonly pick: number; readonly value: number }
  | { readonly kind: "delete"; readonly pick: number };

type FailureKind = "typed" | "invalid-op" | "missing" | "defect" | "interrupt" | "non-serializable";

/** Where a batch fails: before operation `at` runs (`at === ops.length`: after the last). */
type Failure = { readonly kind: "none" } | { readonly kind: FailureKind; readonly at: number };

interface Batch {
  readonly ops: readonly Op[];
  readonly failure: Failure;
}

/** What the handler actually did, in order. The model replays this; the store must agree with it. */
type Step =
  | { readonly kind: "insert"; readonly id: string; readonly value: number }
  | { readonly kind: "bump"; readonly delta: number }
  | { readonly kind: "edit"; readonly id: string; readonly value: number }
  | { readonly kind: "delete"; readonly id: string }
  | { readonly kind: "skip" };

const EXPECTED_TAG: Record<FailureKind, string> = {
  typed: "ConflictError",
  "invalid-op": "SchemaError",
  missing: "NotFoundError",
  defect: "Defect",
  interrupt: "Interrupted",
  "non-serializable": "NonSerializableResultError",
};

const valueArb = fc.integer({ min: -1000, max: 1000 });

const opArb: fc.Arbitrary<Op> = fc.oneof(
  fc.record({ kind: fc.constant("insert" as const), value: valueArb }),
  fc.record({ kind: fc.constant("bump" as const), delta: fc.integer({ min: -5, max: 5 }) }),
  fc.record({ kind: fc.constant("edit" as const), pick: fc.nat(20), value: valueArb }),
  fc.record({ kind: fc.constant("delete" as const), pick: fc.nat(20) }),
);

const batchArb: fc.Arbitrary<Batch> = fc.array(opArb, { minLength: 1, maxLength: 8 }).chain((ops) =>
  fc.record({
    ops: fc.constant(ops),
    failure: fc.oneof(
      { weight: 3, arbitrary: fc.constant<Failure>({ kind: "none" }) },
      {
        weight: 5,
        arbitrary: fc.record({
          kind: fc.constantFrom<FailureKind>("typed", "invalid-op", "missing", "defect", "interrupt"),
          at: fc.integer({ min: 0, max: ops.length }),
        }),
      },
      { weight: 1, arbitrary: fc.constant<Failure>({ kind: "non-serializable", at: ops.length }) },
    ),
  })
);

/** A one-off mutation running `batch` against `owner`'s documents; `reached` fires at an interruption point. */
function batchMutation(owner: string, counterId: string, batch: Batch, reached: Deferred.Deferred<void>) {
  return app.mutation({
    args: {},
    handler: ({ db }) =>
      Effect.gen(function* () {
        const steps: Step[] = [];
        const inject = (kind: FailureKind) => {
          switch (kind) {
            case "typed":
              return Effect.fail(new ConflictError({ expected: 0, actual: 1 }));
            case "invalid-op":
              return db.insert("items", { owner, value: "not an integer" as unknown as number });
            case "missing":
              return db.patch("items", `${owner}-absent`, { value: 0 });
            case "defect":
              return Effect.die(new Error("injected defect"));
            case "interrupt":
              return Effect.zipRight(Deferred.succeed(reached, undefined), Effect.never);
            case "non-serializable":
              return Effect.void; // the poisoned result below fails it
          }
        };
        const owned = db.find("items", { owner }).limit(1000).collect();
        for (const [index, op] of batch.ops.entries()) {
          if (batch.failure.kind !== "none" && batch.failure.at === index) yield* inject(batch.failure.kind);
          yield* Effect.yieldNow();
          switch (op.kind) {
            case "insert": {
              const id = yield* db.insert("items", { owner, value: op.value });
              steps.push({ kind: "insert", id, value: op.value });
              break;
            }
            case "bump": {
              const counter = yield* db.get("counters", counterId);
              yield* db.patch("counters", counterId, { n: counter!.n + op.delta });
              steps.push({ kind: "bump", delta: op.delta });
              break;
            }
            case "edit":
            case "delete": {
              const items = yield* owned;
              if (items.length === 0) {
                steps.push({ kind: "skip" });
                break;
              }
              const id = items[op.pick % items.length]._id;
              if (op.kind === "edit") {
                yield* db.patch("items", id, { value: op.value });
                steps.push({ kind: "edit", id, value: op.value });
              } else {
                yield* db.delete("items", id);
                steps.push({ kind: "delete", id });
              }
              break;
            }
          }
        }
        if (batch.failure.kind !== "none" && batch.failure.at === batch.ops.length) {
          yield* inject(batch.failure.kind);
        }
        return batch.failure.kind === "non-serializable" ? { steps, poison: 1n } : { steps };
      }),
  });
}

/** Forks the batch, interrupts it at its interruption point if it has one, and returns its exit. */
const runBatch = (owner: string, counterId: string, batch: Batch) =>
  Effect.gen(function* () {
    const storage = yield* Storage;
    const reached = yield* Deferred.make<void>();
    const fiber = yield* Effect.fork(storage.mutation(batchMutation(owner, counterId, batch, reached), {}));
    if (batch.failure.kind === "interrupt") {
      yield* Effect.race(Deferred.await(reached), Effect.asVoid(Fiber.await(fiber)));
      yield* Fiber.interrupt(fiber);
    }
    return yield* Fiber.await(fiber);
  });

/** The serial model: `state` with every step applied in order. Referencing a missing item is a bug. */
function apply(state: State, steps: readonly Step[]): State {
  let { n, version } = state.counter;
  const items = new Map(state.items.map((item) => [item.id, item]));
  for (const step of steps) {
    switch (step.kind) {
      case "insert":
        assert.ok(!items.has(step.id), `insert reused id ${step.id}`);
        items.set(step.id, { id: step.id, value: step.value, version: 1 });
        break;
      case "bump":
        n += step.delta;
        version += 1;
        break;
      case "edit": {
        const item = items.get(step.id);
        assert.ok(item, `edit of an item the model does not have: ${step.id}`);
        items.set(step.id, { id: step.id, value: step.value, version: item.version + 1 });
        break;
      }
      case "delete":
        assert.ok(items.delete(step.id), `delete of an item the model does not have: ${step.id}`);
        break;
      case "skip":
        break;
    }
  }
  return { counter: { n, version }, items: [...items.values()].sort(byId) };
}

for (const name of BACKENDS) {
  test(
    titled(
      `atomicity [${name}]: a batch with an injected failure or interruption leaves no trace; else all of it lands`,
    ),
    async () => {
      await withBackend(name, async (backend) => {
        const seen: Record<string, number> = {};
        await fc.assert(
          fc.asyncProperty(
            fc.array(valueArb, { maxLength: 4 }),
            fc.array(batchArb, { minLength: 1, maxLength: 6 }),
            async (values, batches) => {
              const owner = nextOwner();
              const counterId = await value(backend, mutate(setup, { owner, n: 0, values })) as string;
              let model = await observe(backend, owner);
              const numeric = (a: number, b: number) => a - b;
              assert.deepEqual(model.counter, { n: 0, version: 1 });
              assert.deepEqual(
                model.items.map((item) => item.value).sort(numeric),
                [...values].sort(numeric),
              );
              assert.ok(model.items.every((item) => item.version === 1));
              for (const batch of batches) {
                const exit = await value(backend, runBatch(owner, counterId, batch));
                const after = await observe(backend, owner);
                seen[batch.failure.kind] = (seen[batch.failure.kind] ?? 0) + 1;
                if (batch.failure.kind === "none") {
                  if (Exit.isFailure(exit)) {
                    assert.fail(`an uninjected batch failed: ${Cause.pretty(exit.cause)}`);
                  }
                  model = apply(model, (exit.value as { steps: Step[] }).steps);
                  assert.deepEqual(after, model, "every operation of the committed batch is visible");
                } else {
                  assert.equal(
                    outcome(exit),
                    EXPECTED_TAG[batch.failure.kind],
                    `failure ${batch.failure.kind}`,
                  );
                  assert.deepEqual(after, model, `none of the ${batch.failure.kind} batch is visible`);
                }
              }
            },
          ),
          { numRuns: backend.numRuns },
        );
        // The generators must actually reach every failure mode, or the property proves less than it says.
        for (const kind of ["none", ...Object.keys(EXPECTED_TAG)]) {
          assert.ok(
            (seen[kind] ?? 0) >= 5,
            `failure mode ${kind} ran ${seen[kind] ?? 0} times\nReplay: ${replayCommand(SEED)}`,
          );
        }
      });
    },
  );
}

// ---------------------------------------------------------------------------
// Concurrent writers: shared by properties 2, 3 and 4
// ---------------------------------------------------------------------------

/** One racing writer: increment the counter by `delta`, yielding `yields` times between read and write. */
interface Writer {
  readonly delta: number;
  readonly yields: number;
  /** `commit`, or fail after the write: a typed failure or a defect. Either way it must roll back. */
  readonly fate: "commit" | "typed" | "defect";
}

/** What a committed writer saw and did. `version` is the counter's `_version` its write assigned. */
interface Wrote {
  readonly writer: number;
  readonly read: number;
  readonly wrote: number;
  readonly version: number;
}

type LogEntry = readonly ["enter" | "exit", number];

const writerArb: fc.Arbitrary<Writer> = fc.record({
  delta: fc.integer({ min: 1, max: 5 }),
  yields: fc.nat(3),
  fate: fc.oneof(
    { weight: 3, arbitrary: fc.constant("commit" as const) },
    { weight: 1, arbitrary: fc.constantFrom("typed" as const, "defect" as const) },
  ),
});

const writersArb = fc.array(writerArb, { minLength: 2, maxLength: 12 });

const FATE_TAG: Record<Writer["fate"], string> = { commit: "ok", typed: "ConflictError", defect: "Defect" };

/** A read-modify-write increment that logs entering and leaving its handler. */
function writerMutation(counterId: string, writer: Writer, index: number, log: LogEntry[]) {
  return app.mutation({
    args: {},
    handler: ({ db }) =>
      Effect.gen(function* () {
        log.push(["enter", index]);
        const before = (yield* db.get("counters", counterId))!;
        for (let i = 0; i < writer.yields; i++) yield* Effect.yieldNow();
        yield* db.patch("counters", counterId, { n: before.n + writer.delta });
        const after = (yield* db.get("counters", counterId))!;
        if (writer.fate === "typed") {
          return yield* new ConflictError({ expected: before._version, actual: after._version });
        }
        if (writer.fate === "defect") return yield* Effect.die(new Error("injected defect"));
        const wrote: Wrote = { writer: index, read: before.n, wrote: after.n, version: after._version };
        return wrote;
      }).pipe(Effect.ensuring(Effect.sync(() => void log.push(["exit", index])))),
  });
}

/** Starts every writer at once; each mutation runs under the log annotation `traceId = <owner>/w<i>`. */
const race = (owner: string, counterId: string, writers: readonly Writer[], log: LogEntry[]) =>
  Effect.forEach(
    writers,
    (writer, index) =>
      Effect.exit(mutate(writerMutation(counterId, writer, index, log), {})).pipe(
        Effect.annotateLogs({ traceId: `${owner}/w${index}` }),
      ),
    { concurrency: "unbounded" },
  );

/** Asserts every writer ended as its fate says; returns the committed writers' records by index. */
function settle(
  writers: readonly Writer[],
  exits: readonly Exit.Exit<unknown, unknown>[],
): Map<number, Wrote> {
  const committed = new Map<number, Wrote>();
  exits.forEach((exit, index) => {
    assert.equal(outcome(exit), FATE_TAG[writers[index].fate], `writer ${index} (${writers[index].fate})`);
    if (Exit.isSuccess(exit)) committed.set(index, exit.value as Wrote);
  });
  return committed;
}

/** `2, 3, …, count + 1`: the versions `count` serial writes assign to a document created at version 1. */
const versionsAfterCreate = (count: number) => Array.from({ length: count }, (_, k) => k + 2);

// ---------------------------------------------------------------------------
// 2. No lost updates
// ---------------------------------------------------------------------------

for (const name of BACKENDS) {
  test(
    titled(
      `no lost updates [${name}]: N racing read-modify-write increments all count, and only committed ones`,
    ),
    async () => {
      await withBackend(name, async (backend) => {
        await fc.assert(
          fc.asyncProperty(fc.integer({ min: -100, max: 100 }), writersArb, async (start, writers) => {
            const owner = nextOwner();
            const counterId = await value(backend, mutate(setup, { owner, n: start, values: [] })) as string;
            const exits = await value(backend, race(owner, counterId, writers, []));
            const committed = settle(writers, exits);
            const final = (await observe(backend, owner)).counter;

            let sum = 0;
            for (const index of committed.keys()) sum += writers[index].delta;
            assert.equal(final.n, start + sum, "the counter is the start plus every committed delta");
            assert.equal(final.version, 1 + committed.size, "one version per committed write");

            // One serial history: in version order, each writer read exactly what the previous one wrote.
            let expected = start;
            for (const wrote of [...committed.values()].sort((a, b) => a.version - b.version)) {
              assert.equal(wrote.read, expected, `writer ${wrote.writer} read a stale or uncommitted value`);
              assert.equal(wrote.wrote, wrote.read + writers[wrote.writer].delta);
              expected = wrote.wrote;
            }
            assert.equal(expected, final.n);
          }),
          { numRuns: backend.numRuns },
        );
      });
    },
  );
}

// ---------------------------------------------------------------------------
// 3. Single-writer serialization
// ---------------------------------------------------------------------------

for (const name of BACKENDS) {
  test(
    titled(
      `single-writer serialization [${name}]: handlers never overlap; versions are 2..S+1, gapless and unique`,
    ),
    async () => {
      await withBackend(name, async (backend) => {
        await fc.assert(
          fc.asyncProperty(writersArb, async (writers) => {
            const owner = nextOwner();
            const counterId = await value(backend, mutate(setup, { owner, n: 0, values: [] })) as string;
            const log: LogEntry[] = [];
            const committed = settle(writers, await value(backend, race(owner, counterId, writers, log)));

            // Serial execution: every handler leaves before the next one enters.
            assert.equal(log.length, 2 * writers.length, "every handler entered and left once");
            for (let k = 0; k < log.length; k += 2) {
              assert.deepEqual(
                [log[k], log[k + 1]],
                [["enter", log[k][1]], ["exit", log[k][1]]],
                `handlers interleaved at log entry ${k}: ${JSON.stringify(log)}`,
              );
            }

            // In execution order, the committed writers assigned 2, 3, …: no gap, no duplicate, and a
            // rolled-back writer's version was never seen by anyone.
            const executed = log.filter(([event]) => event === "enter").map(([, index]) => index);
            const versions = executed.flatMap((index) =>
              committed.has(index) ? [committed.get(index)!.version] : []
            );
            assert.deepEqual(versions, versionsAfterCreate(committed.size));
            assert.equal((await observe(backend, owner)).counter.version, 1 + committed.size);
          }),
          { numRuns: backend.numRuns },
        );
      });
    },
  );
}

// ---------------------------------------------------------------------------
// 4. Commit ordering (StorageLive: the double emits no tx events)
// ---------------------------------------------------------------------------

test(
  titled(
    "commit ordering [StorageLive]: one tx event per writer, commits in the order versions were assigned",
  ),
  async () => {
    await withBackend("StorageLive", async (backend) => {
      await fc.assert(
        fc.asyncProperty(writersArb, async (writers) => {
          const owner = nextOwner();
          const counterId = await value(backend, mutate(setup, { owner, n: 0, values: [] })) as string;
          const from = backend.events.length;
          const log: LogEntry[] = [];
          const committed = settle(writers, await value(backend, race(owner, counterId, writers, log)));

          const prefix = `${owner}/w`;
          const txs = backend.events.slice(from).filter((event): event is TxEventRecord =>
            event.kind === "tx" && event.traceId.startsWith(prefix)
          );
          const writerOf = (event: TxEventRecord) => Number(event.traceId.slice(prefix.length));
          assert.deepEqual(
            txs.map(writerOf).sort((a, b) => a - b),
            writers.map((_, index) => index),
            "exactly one tx event per writer",
          );
          const commits = txs.filter((event) => event.outcome === "commit").map(writerOf);
          assert.deepEqual(
            commits.map((index) => committed.get(index)?.version),
            versionsAfterCreate(committed.size),
            "commit events in version order",
          );
          assert.deepEqual(
            txs.filter((event) => event.outcome === "rollback").map(writerOf).sort((a, b) => a - b),
            writers.flatMap((writer, index) => writer.fate === "commit" ? [] : [index]),
            "every failed writer, and only those, rolled back",
          );
          // And the commit order is the order the handlers ran in.
          const executed = log.filter(([event]) => event === "enter").map(([, index]) => index);
          assert.deepEqual(commits, executed.filter((index) => committed.has(index)));
        }),
        { numRuns: backend.numRuns },
      );
    });
  },
);
