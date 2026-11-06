/**
 * The text storage contract (plan item 31, `docs/adr/0005-text-storage-contract.md`), against the
 * real `StorageLive` over a SQLite file and against `StorageTest`, in both physical layouts.
 *
 * The contract was made when the writer's driver bound U+0000 intact but truncated TEXT at it on
 * read; the FFI binding reads it intact, and the contract is unchanged (ADR 0005, FFI plan §6). It is
 * typed rejection before commit:
 *
 * - a body, id or filter value holding U+0000 — leading, interior or trailing, in a scalar, a
 *   nullable scalar, an array item or a nested array item — fails `SchemaError` before its statement
 *   runs, and an uncaught rejection rolls back everything the mutation wrote before it;
 * - an `IdGen` id holding U+0000 fails the insert with `StorageError`;
 * - NUL text already in the file (planted here with the compiler's own `compileInsert`, which a
 *   pre-contract build used unchecked) fails the read with `StorageError`, never a truncated success;
 * - every accepted string, including the near misses (`\u0001`, a literal `\u0000` escape, the
 *   empty string), round-trips unchanged through insert, `get`, `where`, `countBy` and `patch`.
 *
 * Each scenario runs on all four backends and asserts the same outcomes, so the double is held to the
 * live contract as well. The property at the end draws texts and positions from the repo seed
 * (`PROP_SEED=<n>` replays a run).
 */
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import fc from "fast-check";
import { Cause, Effect, Exit, HashMap, Option, Ref, TestClock } from "effect";
import { compileInsert } from "../../core/compile/write.ts";
import type { IdGenService, StorageService } from "../../platform/services.ts";
import { IdGen } from "../../platform/services.ts";
import { inspect, type Inspection } from "../helpers/sqlite_inspect.ts";
import { makeStorage } from "../../platform/sqlite.ts";
import type { WriteDB } from "../../sdk/db.ts";
import { defineFunctions } from "../../sdk/functions.ts";
import {
  defineSchema,
  defineTable,
  type Schema,
  type Storage as Layout,
  v,
  withStorage,
} from "../../sdk/schema.ts";
import { runTest } from "../helpers/run.ts";
import {
  makeIdGenTest,
  makeMemoryStore,
  makeStorageTest,
  MemoryStore,
  type StoredDocument,
} from "../layers/index.ts";
import { rightOf, runs, show } from "../property/compiler_arbitraries.ts";
import { titled } from "../property/arbitraries.ts";

// ===========================================================================
// Harness
// ===========================================================================

const T0 = 1_700_000_000_000;

const schema = defineSchema({
  docs: defineTable({
    name: v.string(),
    kind: v.enum("plain", "fancy"),
    tags: v.array(v.string()),
    grid: v.nullable(v.array(v.array(v.string()))),
    note: v.nullable(v.string()),
    n: v.integer(),
  }),
});
type S = typeof schema;
type Body = {
  name: string;
  kind: "plain" | "fancy";
  tags: string[];
  grid: string[][] | null;
  note: string | null;
  n: number;
};

const app = defineFunctions(schema);
const mutate = <A, E>(handler: (db: WriteDB<S>) => Effect.Effect<A, E>) =>
  app.mutation({ args: {}, handler: ({ db }) => handler(db) });

const body = (overrides: Partial<Body> = {}): Body => ({
  name: "ok",
  kind: "plain",
  tags: ["t0", "t1"],
  grid: [["g00", "g01"]],
  note: "note",
  n: 1,
  ...overrides,
});

/** The three places a NUL can sit in a string. */
const POSITIONS = {
  leading: (s: string) => `\u0000${s}`,
  interior: (s: string) => `${s.slice(0, 1)}\u0000${s.slice(1)}`,
  trailing: (s: string) => `${s}\u0000`,
} as const;

/** Every text location in a body, with the path `storable` reports and how to put `text` there. */
const LOCATIONS: readonly { path: string; put: (text: string) => Partial<Body> }[] = [
  { path: "name", put: (text) => ({ name: text }) },
  { path: "note", put: (text) => ({ note: text }) },
  { path: "tags[1]", put: (text) => ({ tags: ["t0", text] }) },
  { path: "grid[0][1]", put: (text) => ({ grid: [["g00", text]] }) },
];

type Target = "live" | "double";
const BACKENDS = (["live", "double"] as const).flatMap((target) =>
  (["columns", "json"] as const).map((layout) => ({ target, layout }))
);

interface Opened {
  readonly storage: StorageService;
  /** Writes documents straight into storage, bypassing the contract, as a pre-contract build could. */
  readonly plant: (docs: readonly StoredDocument[]) => Effect.Effect<void>;
  /** `_version` of a stored document, read around Storage (so a NUL-bearing row can still be checked). */
  readonly version: (id: string) => Effect.Effect<number | undefined>;
}

const tempDir = Effect.acquireRelease(
  Effect.sync(() => mkdtempSync(join(tmpdir(), "mansio-nul-"))),
  (dir) => Effect.sync(() => rmSync(dir, { recursive: true, force: true })),
);

function open(target: Target, layout: Layout, ids?: IdGenService, tables: Schema = schema) {
  return Effect.gen(function* () {
    yield* TestClock.setTime(T0);
    const laidOut = withStorage(tables, layout);
    const idGen = ids ? Effect.succeed(ids) : makeIdGenTest();
    if (target === "live") {
      const file = join(yield* tempDir, "app.sqlite");
      const storage = yield* makeStorage({ file, schema: laidOut }).pipe(
        Effect.provideServiceEffect(IdGen, idGen),
      );
      const raw = <A>(use: (db: Inspection) => A) =>
        Effect.sync(() => {
          const db = inspect(file, { write: true });
          try {
            return use(db);
          } finally {
            db.close();
          }
        });
      const opened: Opened = {
        storage,
        plant: (docs) =>
          raw((db) => {
            for (const doc of docs) {
              const sql = rightOf(
                compileInsert(laidOut, "docs", doc.body, { _id: doc._id, _creationTime: doc._creationTime }),
              );
              db.run(sql.text, sql.values);
            }
          }),
        version: (id) =>
          raw((db) => db.get('SELECT "_version" AS v FROM "docs" WHERE "_id" = ?', [id])?.v as number),
      };
      return opened;
    }
    const store = yield* makeMemoryStore(laidOut);
    const storage = yield* makeStorageTest().pipe(
      Effect.provideService(MemoryStore, store),
      Effect.provideServiceEffect(IdGen, idGen),
    );
    const docsOf = (contents: HashMap.HashMap<string, readonly StoredDocument[]>) =>
      Option.getOrElse(HashMap.get(contents, "docs"), (): readonly StoredDocument[] => []);
    const opened: Opened = {
      storage,
      plant: (docs) => Ref.update(store.state, (c) => HashMap.set(c, "docs", [...docsOf(c), ...docs])),
      version: (id) => Effect.map(Ref.get(store.state), (c) => docsOf(c).find((d) => d._id === id)?._version),
    };
    return opened;
  });
}

/** An exit as plain data: `{ ok }`, or `{ fail }` with the error's tag and fields (`cause` dropped). */
type Outcome = { ok: unknown } | { fail: Record<string, unknown> };

function outcome(effect: Effect.Effect<unknown, unknown>): Effect.Effect<Outcome> {
  return Effect.map(Effect.exit(effect), (exit): Outcome => {
    if (Exit.isSuccess(exit)) return { ok: exit.value };
    const failure = Cause.squash(exit.cause);
    if (!(failure instanceof Error) || !("_tag" in failure)) throw failure;
    const fields = Object.fromEntries(Object.entries(failure).filter(([key]) => key !== "cause"));
    return { fail: { _tag: failure._tag, ...fields } };
  });
}

const nulRejected = (path: string, actual: string) => ({
  fail: { _tag: "SchemaError", reason: "type-mismatch", path, expected: "text without U+0000", actual },
});

const storedNul = (column: string) => ({
  fail: {
    _tag: "StorageError",
    operation: "select",
    table: "docs",
    detail: `Stored text in "${column}" contains U+0000, which the text storage contract rejects`,
  },
});

const all = mutate((db) => db.find("docs").limit(1000).collect());
const run = (storage: StorageService, definition: Parameters<StorageService["mutation"]>[0]) =>
  storage.mutation(definition, {});

function forEachBackend(
  name: string,
  test: (opened: Opened, layout: Layout) => Effect.Effect<void, unknown>,
) {
  for (const { target, layout } of BACKENDS) {
    runTest(`${name} (${target}, ${layout})`, Effect.flatMap(open(target, layout), (o) => test(o, layout)));
  }
}

// ===========================================================================
// Rejection before commit
// ===========================================================================

forEachBackend(
  "insert: U+0000 anywhere in the body fails SchemaError and rolls back earlier writes",
  (o) =>
    Effect.gen(function* () {
      for (const [position, place] of Object.entries(POSITIONS)) {
        for (const { path, put } of LOCATIONS) {
          const text = place("abc");
          const result = yield* outcome(run(
            o.storage,
            mutate((db) =>
              Effect.gen(function* () {
                yield* db.insert("docs", body());
                return yield* db.insert("docs", body(put(text)));
              })
            ),
          ));
          assert.deepStrictEqual(result, nulRejected(path, text), `${position} NUL at ${path}`);
        }
      }
      // An enum member can never hold U+0000 (`defineTable` refuses one), so `validate` rejects it first.
      const enumResult = yield* outcome(
        run(o.storage, mutate((db) => db.insert("docs", body({ kind: "plain\u0000" as "plain" })))),
      );
      assert.deepStrictEqual(
        enumResult,
        {
          fail: {
            _tag: "SchemaError",
            reason: "type-mismatch",
            path: "kind",
            expected: "enum",
            actual: "plain\u0000",
          },
        },
      );
      assert.deepStrictEqual(
        yield* run(o.storage, all),
        [],
        "every mutation rolled back, earlier inserts included",
      );
    }),
);

forEachBackend(
  "insert: a caught rejection wrote nothing, and the rest of the mutation commits",
  (o) =>
    Effect.gen(function* () {
      const result = yield* run(
        o.storage,
        mutate((db) =>
          Effect.gen(function* () {
            const first = yield* db.insert("docs", body({ name: "first" }));
            const rejected = yield* Effect.either(db.insert("docs", body({ name: "x\u0000" })));
            const second = yield* db.insert("docs", body({ name: "second" }));
            return { first, rejected: rejected._tag, second };
          })
        ),
      );
      // The rejected insert drew `id-2` from IdGen (ids are drawn before validation, as ever).
      assert.deepStrictEqual(result, { first: "id-1", rejected: "Left", second: "id-3" });
      const docs = (yield* run(o.storage, all)) as { _id: string; name: string }[];
      assert.deepStrictEqual(docs.map((d) => [d._id, d.name]), [["id-1", "first"], ["id-3", "second"]]);
    }),
);

forEachBackend(
  "patch: U+0000 in the patch fails SchemaError; the document and its _version are unchanged",
  (o) =>
    Effect.gen(function* () {
      const id = (yield* run(o.storage, mutate((db) => db.insert("docs", body())))) as string;
      for (const [position, place] of Object.entries(POSITIONS)) {
        for (const { path, put } of LOCATIONS) {
          const text = place("abc");
          const result = yield* outcome(run(
            o.storage,
            mutate((db) =>
              Effect.gen(function* () {
                yield* db.patch("docs", id, { n: 2 });
                yield* db.patch("docs", id, put(text));
              })
            ),
          ));
          assert.deepStrictEqual(result, nulRejected(path, text), `${position} NUL at ${path}`);
        }
      }
      assert.deepStrictEqual(yield* run(o.storage, all), [{
        ...body(),
        _id: id,
        _creationTime: T0,
        _version: 1,
      }]);
    }),
);

forEachBackend(
  "ids and filter values: get, patch, delete and where reject U+0000 with SchemaError",
  (o) =>
    Effect.gen(function* () {
      const id = (yield* run(o.storage, mutate((db) => db.insert("docs", body())))) as string;
      for (const place of Object.values(POSITIONS)) {
        const bad = place(id);
        assert.deepStrictEqual(
          yield* outcome(run(o.storage, mutate((db) => db.get("docs", bad)))),
          nulRejected("_id", bad),
        );
        assert.deepStrictEqual(
          yield* outcome(run(o.storage, mutate((db) => db.patch("docs", bad, { n: 5 })))),
          nulRejected("_id", bad),
        );
        assert.deepStrictEqual(
          yield* outcome(run(o.storage, mutate((db) => db.delete("docs", bad)))),
          nulRejected("_id", bad),
        );
        const name = place("ok");
        assert.deepStrictEqual(
          yield* outcome(run(o.storage, mutate((db) => db.find("docs", { name }).collect()))),
          nulRejected("name", name),
        );
        assert.deepStrictEqual(
          yield* outcome(run(o.storage, mutate((db) => db.find("docs").where("note", name).count()))),
          nulRejected("note", name),
        );
      }
      assert.deepStrictEqual(yield* run(o.storage, all), [{
        ...body(),
        _id: id,
        _creationTime: T0,
        _version: 1,
      }]);
    }),
);

for (const { target, layout } of BACKENDS) {
  runTest(
    `generated ids: an IdGen id holding U+0000 fails StorageError(insert) and rolls back (${target}, ${layout})`,
    Effect.gen(function* () {
      const ids = ["base", "\u0000gen", "ge\u0000n", "gen\u0000", "clean"];
      const o = yield* open(target, layout, { next: Effect.sync(() => ids.shift()!) });
      assert.equal(yield* run(o.storage, mutate((db) => db.insert("docs", body()))), "base");
      for (let i = 0; i < 3; i++) {
        const result = yield* outcome(run(
          o.storage,
          mutate((db) =>
            Effect.gen(function* () {
              yield* db.patch("docs", "base", { n: 99 });
              return yield* db.insert("docs", body());
            })
          ),
        ));
        assert.deepStrictEqual(result, {
          fail: {
            _tag: "StorageError",
            operation: "insert",
            table: "docs",
            detail: "IdGen produced an _id containing U+0000",
          },
        });
      }
      assert.equal(yield* run(o.storage, mutate((db) => db.insert("docs", body()))), "clean");
      assert.deepStrictEqual(yield* run(o.storage, all), [
        { ...body(), _id: "base", _creationTime: T0, _version: 1 },
        { ...body(), _id: "clean", _creationTime: T0, _version: 1 },
      ], "the patch before each rejected insert rolled back");
    }),
  );
}

// ===========================================================================
// Accepted text round-trips unchanged
// ===========================================================================

/** Strings next to the contract's edge, every one of which must survive exactly. */
const NEAR_MISSES = [
  "",
  "\u0001",
  "a\u0001b",
  "\\u0000",
  "\\0",
  "null",
  " ",
  "é日😀",
  String.fromCodePoint(0xffff),
  "\u{10FFFF}",
];

forEachBackend(
  "accepted text round-trips unchanged through insert, get, where, countBy and patch",
  (o) =>
    Effect.gen(function* () {
      for (const text of NEAR_MISSES) {
        const value = body({ name: text, note: text, tags: [text, "t"], grid: [[text], []] });
        const result = (yield* run(
          o.storage,
          mutate((db) =>
            Effect.gen(function* () {
              const id = yield* db.insert("docs", value);
              const got = yield* db.get("docs", id);
              const matched = yield* db.find("docs", { name: text }).collect();
              const groups = yield* db.find("docs", { note: text }).countBy("name");
              yield* db.patch("docs", id, { note: `${text}!`, tags: [`!${text}`] });
              const patched = yield* db.get("docs", id);
              yield* db.delete("docs", id);
              return { id, got, matched, groups, patched };
            })
          ),
        )) as { id: string; got: unknown; matched: unknown[]; groups: unknown; patched: unknown };
        const stored = { ...value, _id: result.id, _creationTime: T0, _version: 1 };
        assert.deepStrictEqual(result.got, stored, show(text));
        assert.deepStrictEqual(result.matched, [stored], show(text));
        assert.deepStrictEqual(result.groups, [{ value: text, count: 1 }], show(text));
        assert.deepStrictEqual(
          result.patched,
          { ...stored, note: `${text}!`, tags: [`!${text}`], _version: 2 },
          show(text),
        );
      }
    }),
);

// ===========================================================================
// NUL text stored before the contract
// ===========================================================================

/** One clean document and one per place a pre-contract build could have stored a NUL. */
const PLANTED: readonly StoredDocument[] = [
  { _id: "a", body: body({ name: "clean", n: 1 }) },
  { _id: "b", body: body({ name: "b\u0000x", n: 2 }) },
  { _id: "c", body: body({ name: "clean", tags: ["t", "\u0000"], n: 3 }) },
  { _id: "d\u0000", body: body({ name: "clean", n: 4 }) },
  { _id: "e", body: body({ name: "clean", note: "trailing\u0000", n: 5 }) },
].map((doc) => ({ ...doc, _creationTime: T0, _version: 1 }));

forEachBackend(
  "pre-existing NUL text fails the read with StorageError; clean rows still read",
  (o, layout) =>
    Effect.gen(function* () {
      yield* o.plant(PLANTED);
      // The `json` layout keeps the body in `doc`, so its probe names that column.
      const column = (field: string) => (layout === "json" ? "doc" : field);
      const expect = (definition: Parameters<StorageService["mutation"]>[0]) =>
        outcome(run(o.storage, definition));

      assert.deepStrictEqual(
        yield* expect(mutate((db) => db.get("docs", "a"))),
        { ok: { ...PLANTED[0].body, _id: "a", _creationTime: T0, _version: 1 } },
      );
      assert.deepStrictEqual(yield* expect(mutate((db) => db.get("docs", "b"))), storedNul(column("name")));
      assert.deepStrictEqual(yield* expect(mutate((db) => db.get("docs", "c"))), storedNul(column("tags")));
      assert.deepStrictEqual(yield* expect(mutate((db) => db.get("docs", "e"))), storedNul(column("note")));
      // Each planted document read on its own by a filter: the clean one reads, every NUL one fails.
      const alone: [number, Outcome][] = [
        [1, { ok: [{ ...PLANTED[0].body, _id: "a", _creationTime: T0, _version: 1 }] }],
        [2, storedNul(column("name"))],
        [3, storedNul(column("tags"))],
        [4, storedNul("_id")],
        [5, storedNul(column("note"))],
      ];
      for (const [n, expected] of alone) {
        assert.deepStrictEqual(
          yield* expect(mutate((db) => db.find("docs").where("n", n).collect())),
          expected,
          `n = ${n}`,
        );
      }
      // Reading them all fails on the first NUL row in read order, `b`.
      assert.deepStrictEqual(yield* expect(all), storedNul(column("name")), "the whole table");
      // A `countBy` group value is named by its field in either layout.
      assert.deepStrictEqual(
        yield* expect(mutate((db) => db.find("docs").countBy("name"))),
        storedNul("name"),
      );
      // Text-free reads are unaffected.
      assert.deepStrictEqual(yield* expect(mutate((db) => db.find("docs").count())), { ok: 5 });
      assert.deepStrictEqual(
        yield* expect(mutate((db) => db.find("docs").countBy("n"))),
        { ok: [1, 2, 3, 4, 5].map((n) => ({ value: n, count: 1 })) },
      );

      // `patch` reads before it writes, so it cannot rewrite a truncated document as truth.
      assert.deepStrictEqual(
        yield* expect(mutate((db) => db.patch("docs", "b", { n: 20 }))),
        storedNul(column("name")),
      );
      assert.equal(yield* o.version("b"), 1, "the failed patch wrote nothing");
      // Patching away the only NUL is a read too, so it fails alike; the clean row patches as usual.
      assert.deepStrictEqual(
        yield* expect(mutate((db) => db.patch("docs", "e", { note: null }))),
        storedNul(column("note")),
      );
      assert.deepStrictEqual(yield* expect(mutate((db) => db.patch("docs", "a", { n: 10 }))), { ok: null });
      assert.equal(yield* o.version("a"), 2);

      // The recovery path: a row with a clean id can be deleted; one whose id holds a NUL cannot be named.
      assert.deepStrictEqual(yield* expect(mutate((db) => db.delete("docs", "b"))), { ok: null });
      assert.equal(yield* o.version("b"), undefined);
      assert.deepStrictEqual(
        yield* expect(mutate((db) => db.delete("docs", "d\u0000"))),
        nulRejected("_id", "d\u0000"),
      );
      assert.equal(yield* o.version("d\u0000"), 1);
    }),
);

/** A field named `doc`: in the `columns` layout its column holds JSON text, but not the body. */
const docField = defineSchema({ docs: defineTable({ doc: v.array(v.string()) }) });

for (const { target, layout } of BACKENDS) {
  runTest(
    `pre-existing NUL text in an array field named \`doc\` fails the read (${target}, ${layout})`,
    Effect.gen(function* () {
      const o = yield* open(target, layout, undefined, docField);
      yield* o.plant([
        { _id: "a", body: { doc: ["clean"] } },
        { _id: "b", body: { doc: ["t", "x\u0000"] } },
      ].map((doc) => ({ ...doc, _creationTime: T0, _version: 1 })));
      const get = (id: string) =>
        outcome(
          run(
            o.storage,
            defineFunctions(docField).mutation({ args: {}, handler: ({ db }) => db.get("docs", id) }),
          ),
        );
      assert.deepStrictEqual(yield* get("a"), {
        ok: { doc: ["clean"], _id: "a", _creationTime: T0, _version: 1 },
      });
      // Either layout stores the NUL in a column named `doc`, as a JSON escape.
      assert.deepStrictEqual(yield* get("b"), storedNul("doc"));
    }),
  );
}

/** An array field named `value`: a `countBy` result column shares its name but is never JSON. */
const valueField = defineSchema({ docs: defineTable({ name: v.string(), value: v.array(v.string()) }) });

for (const { target, layout } of BACKENDS) {
  runTest(
    `countBy beside an array field named \`value\` reads its group values as text (${target}, ${layout})`,
    Effect.gen(function* () {
      const o = yield* open(target, layout, undefined, valueField);
      const countBy = () =>
        outcome(
          run(
            o.storage,
            defineFunctions(valueField).mutation({
              args: {},
              handler: ({ db }) => db.find("docs").countBy("name"),
            }),
          ),
        );
      yield* o.plant([{ _id: "a", body: { name: "abc", value: ["v"] }, _creationTime: T0, _version: 1 }]);
      assert.deepStrictEqual(yield* countBy(), { ok: [{ value: "abc", count: 1 }] });
      yield* o.plant([{
        _id: "b",
        body: { name: "b\u0000x", value: ["v"] },
        _creationTime: T0,
        _version: 1,
      }]);
      assert.deepStrictEqual(yield* countBy(), storedNul("name"));
    }),
  );
}

runTest(
  "pre-existing NUL text: MemoryStore.select (ReaderTest's path) reads it losslessly, as DuckDB does",
  Effect.gen(function* () {
    const store = yield* makeMemoryStore(schema);
    yield* Ref.set(store.state, HashMap.make(["docs", PLANTED]));
    const rows = yield* store.select({ kind: "select", table: "docs", filters: [], limit: 10 });
    assert.deepStrictEqual(
      (rows as { _id: string; name: string; note: string | null }[]).map((d) => [d._id, d.name, d.note]),
      [["a", "clean", "note"], ["b", "b\u0000x", "note"], ["c", "clean", "note"], [
        "d\u0000",
        "clean",
        "note",
      ], [
        "e",
        "clean",
        "trailing\u0000",
      ]],
    );
  }),
);

// ===========================================================================
// Property: accepted iff NUL-free, and accepted text round-trips
// ===========================================================================

/** A text and where it goes; `nul` splices U+0000 in at `at` (mod length + 1), otherwise any NUL is removed. */
const caseArb = fc.record({
  location: fc.constantFrom(...LOCATIONS),
  text: fc.string({ unit: "binary", maxLength: 8 }),
  nul: fc.boolean(),
  at: fc.nat(),
});

for (const { target, layout } of BACKENDS) {
  runTest(
    titled(
      `property: a text is stored iff it holds no U+0000, and reads back unchanged (${target}, ${layout})`,
    ),
    Effect.gen(function* () {
      const o = yield* open(target, layout);
      let committed = 0;
      fc.assert(
        fc.property(caseArb, ({ location, text, nul, at }) => {
          const clean = text.replaceAll("\u0000", "");
          const i = at % (clean.length + 1);
          const value = nul ? `${clean.slice(0, i)}\u0000${clean.slice(i)}` : clean;
          const exit = Effect.runSyncExit(run(
            o.storage,
            mutate((db) =>
              Effect.gen(function* () {
                const id = yield* db.insert("docs", body(location.put(value)));
                return yield* db.get("docs", id);
              })
            ),
          ));
          if (nul) {
            assert.ok(Exit.isFailure(exit));
            assert.deepStrictEqual(
              { ...(Cause.squash(exit.cause) as object) },
              nulRejected(location.path, value).fail,
            );
          } else {
            assert.ok(Exit.isSuccess(exit), show(value));
            const { _id, _creationTime, _version, ...stored } = exit.value as Record<string, unknown>;
            assert.deepStrictEqual(stored, body(location.put(value)));
            assert.deepStrictEqual([typeof _id, _creationTime, _version], ["string", T0, 1]);
            committed++;
          }
        }),
        runs(target === "live" ? 100 : 200),
      );
      const count = yield* run(o.storage, mutate((db) => db.find("docs").count()));
      assert.equal(count, committed, "exactly the NUL-free cases committed");
    }),
  );
}
