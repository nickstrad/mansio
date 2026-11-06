/**
 * `StorageTest`: the document store, in memory (plan item 20, §1.2).
 *
 * This is a **second implementation** of the semantics `platform/sqlite.ts` gets from SQLite, and
 * every test built on it is only as honest as the two agree. The conformance suite in
 * `tests/core_test.ts` runs one scripted operation sequence against both and asserts deep equality
 * of every result and of the final contents; change anything here and run it.
 *
 * ## What it reuses, and why
 *
 * Validation is not re-implemented: every operation first runs the engine's own pure compiler
 * (`compileSelect` / `compileInsert` / `compileReplace` / `compileDelete`, then `parseObject`) and
 * discards the SQL, so the same inputs fail with the same tagged error, and a plan wrong in two ways
 * reports the same failure first. Results are not re-implemented either: matching documents are
 * encoded into the rows the engine would have returned (`encode` per column, or the `doc` JSON text
 * for the `json` layout) and handed to `core/compile/decode.ts`'s `decode`, so booleans, arrays,
 * metadata and the key order of every document come out exactly as the live reader builds them.
 *
 * What *is* re-implemented is the part SQLite executes: filtering, ordering and aggregation. That
 * evaluator is the reference implementation from item 13's oracle property
 * (`tests/property/sqlite_oracle_test.ts`, not importable — it is a test module), with its rules:
 *
 * - `eq null` matches nulls only; `eq v` and `gt v` never match a null.
 * - Rows order by the plan's field — NULLs first ascending, last descending — then `_id` ascending;
 *   with no ordering, `_id` alone. Then `limit`.
 * - Text compares by code point (SQLite's BINARY collation over UTF-8), **not** by JS `<`, which
 *   compares UTF-16 code units and sorts U+FFxx after supplementary-plane characters.
 * - `count` counts every match and ignores `limit`; `countBy` orders groups by value with NULL
 *   first and limits the groups, not the rows.
 *
 * ## Mutations
 *
 * The mutation pipeline mirrors `makeStorage` step for step: the same writer-queue cap
 * (`QueueFullError` at 1000), one mutation at a time under `Semaphore(1)`, the same handler-error
 * narrowing (anything outside `MutationError` is a defect), the same JSON round-trip of the result
 * (`NonSerializableResultError`), the same expiring context (`ContextExpiredError` for a `db` used
 * after its transaction), and the same order of side effects — an insert draws its id from `IdGen`
 * and reads `Clock` *before* validating, so a rejected insert still consumes an id, as it does live.
 *
 * Transactions are copy-on-write: a mutation works on its own copy of the committed contents (the
 * store is an immutable `HashMap`, so the copy is free) and publishes it with one `Ref.set` at
 * commit. Rollback — on a typed failure, a defect or an interruption — is discarding the copy. This
 * is the plan's "snapshot the `Ref` and restore it" without the restore step, and it adds the one
 * thing restore cannot give: `ReaderTest` reading the same store never sees an uncommitted write,
 * exactly as DuckDB never sees an uncommitted SQLite transaction.
 *
 * ## The text storage contract
 *
 * U+0000 follows `docs/adr/0005-text-storage-contract.md` exactly as live does, in the same order:
 * bodies, ids and filter values holding one fail `SchemaError` (`core/schema.ts`'s `storable`) after
 * the compiler's own checks; an `IdGen` id holding one fails the insert with `StorageError`; and a
 * Storage read whose rows carry stored NUL text (only reachable by planting documents in
 * `MemoryStore.state`, as a pre-contract database would hold them) fails `StorageError` (operation
 * `select`) before decoding — the live layer's stored-text probe. `MemoryStore.select`, which
 * `ReaderTest` uses, has no such probe: the DuckDB reader reads NUL text losslessly.
 *
 * ## Known divergences (live-side bugs, not semantics to copy)
 *
 * Item 13's oracle (`tests/property/sqlite_oracle_test.ts`) found two live-side bugs that are now
 * resolved: `json`-layout numbers at or above 2^53 (item 30's `CAST(... AS REAL)`), and U+0000
 * truncation (item 31's contract, above). One divergence remains. It is the same class of finding,
 * filed here because the oracle's generators never produce a lone surrogate. It keeps its old number,
 * 3, because `tests/core_test.ts` pins it as "known divergence 3".
 *
 * 3. Text containing a lone surrogate (e.g. `"a\uD800b"`): the live driver binds strings as UTF-8,
 *    which replaces it with U+FFFD. In the `columns` layout a scalar text column reads back as
 *    `"a�b"` (and `eq "a\uD800b"` still matches, since the bound filter is replaced the same way);
 *    array fields are stored there as JSON text, where `JSON.stringify` escapes it as `\ud800`, so
 *    they round-trip. In the `json` layout the whole document is JSON text, so every value
 *    round-trips, but `json_extract` decodes the escape and the bound filter does not, so
 *    `eq "a\uD800b"` misses the row. Here the value round-trips and `eq` matches in both layouts.
 *    Pinned by "known divergence 3" in `tests/core_test.ts`.
 *
 * No SQLite, no filesystem: `snapshot(path)` records the committed contents under `path` in
 * `MemoryStore.snapshots` (and runs `onSnapshot`, if given) instead of writing a file. Live
 * `VACUUM INTO` refuses a destination that already exists; the double can only know about paths it
 * has itself snapshotted, so a second `snapshot` to the same path fails with the live error
 * (`StorageError`, operation `snapshot`), but a path that exists only on disk — written by
 * `onSnapshot`, another test or anything else — is not detected and the snapshot succeeds.
 *
 * No events either: the live layer's `sql`/`tx`/`queue` events describe SQL statements this double
 * never runs, so it does not require `EventSink`. Assert on events against `StorageLive`.
 */
import { Context, Deferred, Effect, Either, HashMap, Layer, Option, Ref, type Scope } from "effect";
import {
  type CompileError,
  ContextExpiredError,
  type DecodeError,
  type MutationError,
  NonSerializableResultError,
  NotFoundError,
  QueueFullError,
  type SchemaError,
  StorageError,
} from "../../core/errors.ts";
import { type Node, NUL, parseObject, type Schema, storable, type Table } from "../../core/schema.ts";
import { normalise } from "../../core/plan.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileSelect, encode, fieldNode, tableFor } from "../../core/compile/select.ts";
import { compileDelete, compileInsert, compileReplace } from "../../core/compile/write.ts";
import { decode } from "../../core/compile/decode.ts";
import type { Bind, Dialect, Row } from "../../core/compile/dialect.ts";
import { Documents, type Operation, type SelectPlan } from "../../sdk/db.ts";
import type { AnyDefinition } from "../../sdk/functions.ts";
import { IdGen, Storage, type StorageService } from "../../platform/services.ts";

/** Jobs admitted to the writer queue at once. `platform/sqlite.ts`'s `WRITER_QUEUE_CAP`, not imported: that module loads the FFI SQLite binding. */
export const WRITER_QUEUE_CAP = 1000;

// ===========================================================================
// The store
// ===========================================================================

/** One committed document: its metadata, and its body exactly as `parseObject` validated it. */
export interface StoredDocument {
  readonly _id: string;
  readonly _creationTime: number;
  readonly _version: number;
  readonly body: Readonly<Record<string, unknown>>;
}

/** Table name to its documents, in insertion order. Reads impose their own order. */
export type StoreContents = HashMap.HashMap<string, readonly StoredDocument[]>;

/** What a read can fail with: the compiler's and the decoder's errors, as live. */
export type SelectError = CompileError | SchemaError | DecodeError;

export interface MemoryStoreService {
  readonly schema: Schema;
  /** The committed contents. Mutations publish here at commit; readers read only from here. */
  readonly state: Ref.Ref<StoreContents>;
  /** `Storage.snapshot(path)` calls, by path: the committed contents at the moment of the snapshot. */
  readonly snapshots: Ref.Ref<HashMap.HashMap<string, StoreContents>>;
  /** Evaluate a plan against the committed contents, as a read would. */
  select(plan: SelectPlan, dialect?: Dialect): Effect.Effect<unknown, SelectError>;
  /**
   * Every committed document of every table, decoded as a read returns it and ordered by `_id` —
   * what `find(table).collect()` would return without a limit. For final-state assertions.
   */
  readonly dump: Effect.Effect<Readonly<Record<string, readonly Row[]>>>;
}

/** The shared in-memory store behind `StorageTest` and `ReaderTest`. */
export class MemoryStore extends Context.Tag("mansio/test/MemoryStore")<MemoryStore, MemoryStoreService>() {}

// ===========================================================================
// The evaluator (pure)
// ===========================================================================

/** Code-point order, which is what SQLite's BINARY collation over UTF-8 amounts to. */
export function compareText(a: string, b: string): number {
  const x = [...a];
  const y = [...b];
  for (let i = 0; i < Math.min(x.length, y.length); i++) {
    const d = x[i].codePointAt(0)! - y[i].codePointAt(0)!;
    if (d !== 0) return d;
  }
  return x.length - y.length;
}

/** Two non-null encoded values of one node kind, in SQLite order. Booleans are already `0`/`1`. */
function compareBound(node: Node, a: Bind, b: Bind): number {
  if (node.kind === "string" || node.kind === "enum") return compareText(a as string, b as string);
  const x = Number(a);
  const y = Number(b);
  return x < y ? -1 : x > y ? 1 : 0;
}

/** Ascending puts NULLs first and descending puts them last, as the compiler's `ORDER BY` says. */
function compareNullable(node: Node, a: Bind, b: Bind, direction: "asc" | "desc"): number {
  if (a === null || b === null) {
    if (a === b) return 0;
    const nullFirst = a === null ? -1 : 1;
    return direction === "asc" ? nullFirst : -nullFirst;
  }
  const c = compareBound(node, a, b);
  return direction === "asc" ? c : -c;
}

/** The value one field holds, encoded the way the engine stores and compares it. */
function column(node: Node, doc: StoredDocument, field: string): Bind {
  if (field === "_id" || field === "_creationTime" || field === "_version") return doc[field];
  return encode(node, doc.body[field] ?? null);
}

/** The row the engine would hand `decode` for one stored document. */
function rowOf(table: Table, doc: StoredDocument): Row {
  const meta: Row = { _id: doc._id, _creationTime: doc._creationTime, _version: doc._version };
  if (table.storage === "json") return { ...meta, doc: JSON.stringify(doc.body) };
  for (const [name, field] of Object.entries(table.fields)) meta[name] = encode(field.node, doc.body[name]);
  return meta;
}

function documentsOf(contents: StoreContents, table: string): readonly StoredDocument[] {
  return Option.getOrElse(HashMap.get(contents, table), () => []);
}

/**
 * A select plan against `contents`: validated by the engine's compiler (in `dialect`, so a reader
 * validates as DuckDB would), evaluated here, decoded by the engine's decoder.
 */
export function evaluateSelect(
  schema: Schema,
  contents: StoreContents,
  plan: SelectPlan,
  dialect: Dialect = "sqlite",
): Either.Either<unknown, SelectError> {
  return Either.flatMap(selectRows(schema, contents, plan, dialect), (rows) => decode(schema, plan, rows));
}

/** `evaluateSelect` up to the engine rows, before `decode`: what the driver would have returned. */
function selectRows(
  schema: Schema,
  contents: StoreContents,
  plan: SelectPlan,
  dialect: Dialect,
): Either.Either<Row[], SelectError> {
  return Either.gen(function* () {
    yield* compileSelect(schema, plan, dialect);
    const table = yield* tableFor(schema, plan.table);
    const { filters, order, aggregate, limit } = yield* normalise(plan);
    const predicates: ((doc: StoredDocument) => boolean)[] = [];
    for (const filter of filters) {
      const node = yield* fieldNode(table, filter.field);
      const wanted = filter.value === null ? null : encode(node, filter.value);
      predicates.push((doc) => {
        const stored = column(node, doc, filter.field);
        if (filter.op === "eq") {
          return wanted === null ? stored === null : stored !== null && stored === wanted;
        }
        return stored !== null && Number(stored) > Number(wanted);
      });
    }
    const hits = documentsOf(contents, plan.table).filter((doc) => predicates.every((p) => p(doc)));
    let rows: Row[];
    if (aggregate?.kind === "count") {
      rows = [{ count: hits.length }];
    } else if (aggregate?.kind === "countBy") {
      const node = yield* fieldNode(table, aggregate.field);
      const counts = new Map<Bind, number>();
      for (const doc of hits) {
        const value = column(node, doc, aggregate.field);
        counts.set(value, (counts.get(value) ?? 0) + 1);
      }
      rows = [...counts]
        .map(([value, count]) => ({ value, count }))
        .sort((a, b) => compareNullable(node, a.value, b.value, "asc"))
        .slice(0, limit);
    } else {
      const byOrder = order ? yield* fieldNode(table, order.field) : undefined;
      rows = [...hits]
        .sort((a, b) =>
          (order && byOrder
            ? compareNullable(
              byOrder,
              column(byOrder, a, order.field),
              column(byOrder, b, order.field),
              order.direction,
            )
            : 0) || compareText(a._id, b._id)
        )
        .slice(0, limit)
        .map((doc) => rowOf(table, doc));
    }
    return rows;
  });
}

/**
 * The live stored-text probe over the rows a read returned: the first column holding U+0000, either
 * as raw text or, in the JSON text of `doc` and array columns, as an escape. An aggregate's rows are
 * result columns (`value`, `count`), never stored ones, so none is JSON even when a field shares its
 * name. A `countBy` group value is named by its field, as live names it. `undefined` when there is none.
 */
function storedNul(table: Table, plan: SelectPlan, rows: readonly Row[]): string | undefined {
  const json = (column: string) =>
    !plan.aggregate &&
    (table.storage === "json" ? column === "doc" : table.fields[column]?.node.kind === "array");
  for (const row of rows) {
    for (const [column, value] of Object.entries(row)) {
      if (typeof value !== "string") continue;
      if (Either.isRight(storable(json(column) ? JSON.parse(value) : value, column))) continue;
      return plan.aggregate?.kind === "countBy" ? plan.aggregate.field : column;
    }
  }
  return undefined;
}

/** Every committed document of every table, as a read decodes it, ordered by `_id`. */
function dumpOf(schema: Schema, contents: StoreContents): Either.Either<Record<string, Row[]>, SelectError> {
  return Either.gen(function* () {
    const out: Record<string, Row[]> = {};
    for (const [name, table] of Object.entries(schema)) {
      const docs = [...documentsOf(contents, name)].sort((a, b) => compareText(a._id, b._id));
      const plan: SelectPlan = { kind: "select", table: name, filters: [], limit: Math.max(docs.length, 1) };
      out[name] = (yield* decode(schema, plan, docs.map((doc) => rowOf(table, doc)))) as Row[];
    }
    return out;
  });
}

/**
 * The shared store for one schema. Fails like `StorageLive`'s startup does when the schema itself
 * cannot compile to DDL (a bad identifier, an empty enum), so a schema the live layer refuses is
 * refused here too.
 */
export function makeMemoryStore(
  schema: Schema,
): Effect.Effect<MemoryStoreService, CompileError | SchemaError> {
  return Effect.gen(function* () {
    yield* compileDDL(schema);
    const state = yield* Ref.make<StoreContents>(HashMap.empty());
    const snapshots = yield* Ref.make(HashMap.empty<string, StoreContents>());
    return {
      schema,
      state,
      snapshots,
      select: (plan, dialect) =>
        Effect.flatMap(Ref.get(state), (c) => evaluateSelect(schema, c, plan, dialect)),
      dump: Effect.flatMap(Ref.get(state), (c) => Effect.orDie(dumpOf(schema, c))),
    };
  });
}

/** `MemoryStore` for `schema`, empty. */
export function MemoryStoreTest(schema: Schema): Layer.Layer<MemoryStore, CompileError | SchemaError> {
  return Layer.effect(MemoryStore, makeMemoryStore(schema));
}

// ===========================================================================
// Storage over the store
// ===========================================================================

/** What one document operation can fail with inside a mutation. As `platform/sqlite.ts`. */
type ExecuteError = SelectError | StorageError | ContextExpiredError | MutationError;

/** Every `MutationError` tag, so a handler's erased failure channel narrows exactly as live. */
const MUTATION_ERROR_TAGS: Record<MutationError["_tag"], true> = {
  SchemaError: true,
  CompileError: true,
  DecodeError: true,
  RuleError: true,
  NotFoundError: true,
  ConflictError: true,
  ContextExpiredError: true,
  NonSerializableResultError: true,
  StorageError: true,
  QueueFullError: true,
};

function isMutationError(e: unknown): e is MutationError {
  return e instanceof Error && Object.hasOwn(MUTATION_ERROR_TAGS, (e as { _tag?: unknown })._tag as string);
}

function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** One open transaction: the expiry flag, and the private copy its writes go to. */
interface Tx {
  active: boolean;
  working: StoreContents;
}

export interface StorageTestOptions {
  /** Stamped on `NonSerializableResultError`, as `StorageOptions.appId` is live. */
  readonly appId?: string;
  /**
   * Runs inside the writer queue for each `snapshot(path)`, after the contents are recorded — e.g.
   * to write a real file for a test that must read one back. Default: nothing.
   */
  readonly onSnapshot?: (path: string, contents: StoreContents) => Effect.Effect<void, StorageError>;
  /**
   * Replaces `evaluateSelect` for this service's reads (not `MemoryStore.select`, so not
   * `ReaderTest`). Only for planting a deliberate divergence: the conformance suite uses it to
   * prove that a double which diverged from SQLite fails against `StorageLive`. Default: the real
   * evaluator.
   */
  readonly evaluate?: typeof evaluateSelect;
}

/** Builds the service in the caller's scope over the `MemoryStore` in context. */
export function makeStorageTest(
  options: StorageTestOptions = {},
): Effect.Effect<StorageService, never, Scope.Scope | MemoryStore | IdGen> {
  const { appId, onSnapshot, evaluate = evaluateSelect } = options;
  return Effect.gen(function* () {
    const store = yield* MemoryStore;
    const { schema, state } = store;
    const idGen = yield* IdGen;
    const clock = yield* Effect.clock;
    const semaphore = yield* Effect.makeSemaphore(1);
    const drained = yield* Deferred.make<void>();
    let depth = 0;
    let closing = false;

    // As live: refuse new work, then wait for the queue to drain.
    yield* Effect.addFinalizer(() =>
      Effect.suspend(() => {
        closing = true;
        return depth > 0 ? Deferred.await(drained) : Effect.void;
      })
    );

    /** Admit `job` to the writer queue (or reject it at the cap), then run it alone. */
    const exclusive = <A, E>(job: Effect.Effect<A, E>): Effect.Effect<A, E | QueueFullError | StorageError> =>
      Effect.uninterruptibleMask((restore) =>
        Effect.suspend((): Effect.Effect<A, E | QueueFullError | StorageError> => {
          if (closing) {
            return Effect.fail(new StorageError({ operation: "close", detail: "Storage is closed" }));
          }
          if (depth >= WRITER_QUEUE_CAP) {
            return Effect.fail(new QueueFullError({ queue: "writer", depth, cap: WRITER_QUEUE_CAP }));
          }
          depth++;
          return Effect.ensuring(
            restore(semaphore.withPermits(1)(job)),
            Effect.suspend(() => {
              depth--;
              return closing && depth === 0 ? Deferred.succeed(drained, undefined) : Effect.void;
            }),
          );
        })
      );

    /**
     * Copy-on-write transaction. Commit expires the context and publishes the copy in one
     * uninterruptible step; every other exit just expires the context, which discards the copy.
     */
    const transaction = <A, E>(use: (tx: Tx) => Effect.Effect<A, E>): Effect.Effect<A, E> =>
      Effect.acquireUseRelease(
        Effect.map(Ref.get(state), (working): Tx => ({ active: true, working })),
        (tx) =>
          Effect.flatMap(use(tx), (result) =>
            Effect.uninterruptible(Effect.suspend(() => {
              tx.active = false;
              return Effect.as(Ref.set(state, tx.working), result);
            }))),
        (tx) => Effect.sync(() => void (tx.active = false)),
      );

    /**
     * One synchronous step against the transaction's copy, refused once the transaction has ended.
     * Checking and writing in the same step is what keeps a forked fiber's late write out, as the
     * live `statement` guard does.
     */
    const step = <A>(tx: Tx, run: () => Either.Either<A, ExecuteError>): Effect.Effect<A, ExecuteError> =>
      Effect.suspend(
        () => (tx.active ? run() : Either.left(new ContextExpiredError({ context: "mutation" }))),
      );

    const replaceTable = (tx: Tx, table: string, docs: readonly StoredDocument[]) => {
      tx.working = HashMap.set(tx.working, table, docs);
    };

    /**
     * A read inside a mutation, in live's order: the compiler's checks, then the filter values
     * (`storable`), then the stored-text probe over the rows the driver would return, then `decode`.
     */
    const read = (working: StoreContents, plan: SelectPlan): Either.Either<unknown, ExecuteError> =>
      Either.gen(function* () {
        yield* compileSelect(schema, plan, "sqlite");
        for (const filter of plan.filters) yield* storable(filter.value, filter.field);
        const table = yield* tableFor(schema, plan.table);
        const column = storedNul(table, plan, yield* selectRows(schema, working, plan, "sqlite"));
        if (column !== undefined) {
          return yield* Either.left(
            new StorageError({
              operation: "select",
              table: plan.table,
              detail: `Stored text in "${column}" contains U+0000, which the text storage contract rejects`,
            }),
          );
        }
        return yield* evaluate(schema, working, plan, "sqlite");
      });

    const execute = (op: Operation, tx: Tx): Effect.Effect<unknown, ExecuteError> => {
      switch (op.kind) {
        case "select":
          return step(tx, () => read(tx.working, op));
        case "insert":
          return Effect.gen(function* () {
            // Same order as live: id and clock first, so a rejected insert still spends an id.
            const id = yield* idGen.next;
            const now = yield* clock.currentTimeMillis;
            return yield* step(tx, () =>
              Either.gen(function* () {
                yield* compileInsert(schema, op.table, op.value, { _id: id, _creationTime: now });
                const table = yield* tableFor(schema, op.table);
                const body = yield* parseObject(table.fields, op.value);
                yield* storable(op.value, ""); // the input, as live checks it, so paths agree
                if (id.includes(NUL)) {
                  return yield* Either.left(
                    new StorageError({
                      operation: "insert",
                      table: op.table,
                      detail: "IdGen produced an _id containing U+0000",
                    }),
                  );
                }
                const docs = documentsOf(tx.working, op.table);
                if (docs.some((doc) => doc._id === id)) {
                  return yield* Either.left(
                    new StorageError({
                      operation: "insert",
                      table: op.table,
                      detail: `UNIQUE constraint failed: ${op.table}._id`,
                    }),
                  );
                }
                replaceTable(tx, op.table, [...docs, { _id: id, _creationTime: now, _version: 1, body }]);
                return id;
              }));
          });
        case "patch":
          return step(tx, () =>
            Either.gen(function* () {
              const table = yield* tableFor(schema, op.table);
              const patch = yield* storable(yield* parseObject(table.fields, op.value, true), "");
              // The live path reads the old row through a compiled select, which also validates the id.
              const found = (yield* read(tx.working, {
                kind: "select",
                table: op.table,
                filters: [{ field: "_id", op: "eq", value: op.id }],
                limit: 1,
              })) as Row[];
              const old = found[0];
              if (!old) return yield* Either.left(new NotFoundError({ table: op.table, id: op.id }));
              const merged = {
                ...Object.fromEntries(Object.keys(table.fields).map((key) => [key, old[key]])),
                ...patch,
              };
              yield* compileReplace(schema, op.table, op.id, merged);
              const body = yield* parseObject(table.fields, merged);
              replaceTable(
                tx,
                op.table,
                documentsOf(tx.working, op.table).map((doc) =>
                  doc._id === op.id ? { ...doc, _version: doc._version + 1, body } : doc
                ),
              );
              return null;
            }));
        case "delete":
          return step(tx, () =>
            Either.gen(function* () {
              yield* compileDelete(schema, op.table, op.id);
              yield* storable(op.id, "_id");
              const docs = documentsOf(tx.working, op.table);
              if (docs.some((doc) => doc._id === op.id)) {
                replaceTable(tx, op.table, docs.filter((doc) => doc._id !== op.id));
              }
              return null;
            }));
      }
    };

    const service: StorageService = {
      // The in-memory double has no IR executor; `/v1/ops` is tested against real SQLite.
      ir: () => Effect.dieMessage("StorageTest does not run IR requests"),
      mutation: (definition: AnyDefinition, args: unknown) =>
        definition.kind !== "mutation" ? Effect.die(new Error("Expected a mutation")) : exclusive(
          transaction((tx) => {
            const documents = new Documents<Schema>((op) =>
              Effect.suspend(() =>
                tx.active ? execute(op, tx) : Effect.fail(new ContextExpiredError({ context: "mutation" }))
              )
            );
            return definition.invoke(documents, args).pipe(
              Effect.catchAll((e) => isMutationError(e) ? Effect.fail(e) : Effect.die(e)),
              Effect.flatMap((result) =>
                Effect.try({
                  try: (): unknown => JSON.parse(JSON.stringify(result ?? null)),
                  catch: (cause) => new NonSerializableResultError({ appId, detail: messageOf(cause) }),
                })
              ),
            );
          }),
        ),
      snapshot: (path: string) =>
        exclusive(
          Effect.gen(function* () {
            // As `VACUUM INTO`: an existing destination is refused, not overwritten.
            if (HashMap.has(yield* Ref.get(store.snapshots), path)) {
              return yield* new StorageError({ operation: "snapshot", detail: "output file already exists" });
            }
            const contents = yield* Ref.get(state);
            yield* Ref.update(store.snapshots, HashMap.set(path, contents));
            if (onSnapshot) yield* onSnapshot(path, contents);
          }),
        ).pipe(
          Effect.catchTag(
            "QueueFullError",
            (cause) =>
              Effect.fail(new StorageError({ operation: "snapshot", detail: "Writer queue full", cause })),
          ),
        ),
    };
    return service;
  });
}

/** `Storage` over a `MemoryStore` provided elsewhere — use this to share one store with `ReaderTest`. */
export function StorageTestOn(
  options: StorageTestOptions = {},
): Layer.Layer<Storage, never, MemoryStore | IdGen> {
  return Layer.scoped(Storage, makeStorageTest(options));
}

/** `Storage` over its own fresh `MemoryStore` for `schema`, which it also exposes. */
export function StorageTest(
  options: StorageTestOptions & { readonly schema: Schema },
): Layer.Layer<Storage | MemoryStore, CompileError | SchemaError, IdGen> {
  return Layer.provideMerge(StorageTestOn(options), MemoryStoreTest(options.schema));
}
