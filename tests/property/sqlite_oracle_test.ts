/**
 * The SQLite oracle property (plan item 13, work 4): the compiled SQL, executed by a real SQLite,
 * returns what the plan means.
 *
 * For a generated schema and document set, the test builds a temporary in-memory database with
 * `compileDDL`, writes the documents with `compileInsert` (then a few `compileReplace`/`compileDelete`
 * mutations, so `_version` varies and deleted rows must stay gone), and runs a batch of generated
 * plans through `compileSelect` and `decode`. Each result must equal what a pure JS reference
 * implementation of the plan returns over the same documents.
 *
 * The reference is written from the plan's documented semantics, not from the SQL:
 *
 * - `eq null` matches nulls; `eq v` and `gt v` never match a null.
 * - Rows are ordered by the plan's field (NULLs first ascending, last descending), then by `_id`
 *   ascending, then cut to `limit`. With no ordering, by `_id` alone.
 * - `count` counts every match, ignoring `limit`; `countBy` groups by the field's value, orders
 *   groups by value ascending with NULL first, and cuts the *groups* to `limit`.
 * - Text compares by Unicode code point — SQLite's BINARY collation compares UTF-8 bytes, which is
 *   code-point order — **not** by JS `<`, which compares UTF-16 code units and puts U+FFxx after
 *   supplementary-plane characters. The generators deliberately produce both.
 *
 * Runs against real `node:sqlite`; `numRuns: 50`, each with up to six plans, to keep it fast.
 *
 * **What the oracle found** (sweeping 200 seeds, ~10 000 scenarios). Both findings are resolved, and
 * their regressions at the bottom of this file run in the default suite:
 *
 * 1. `json` layout, `number` field holding an integral double with 2^53 <= |x| < 2^63. The writer
 *    stores `JSON.stringify(x)` (`1693240866440448800` for the double 1693240866440448768), which
 *    SQLite's `json_extract` read as an exact 64-bit INTEGER: `eq x` missed the row and `countBy`
 *    returned an INTEGER that `node:sqlite` refuses beyond 2^53. Fixed by item 30's
 *    `CAST(... AS REAL)` for `number` nodes; regressions 1a and 1b. The generated documents include
 *    such numbers again.
 * 2. Text containing U+0000. `node:sqlite` binds it intact but returns TEXT truncated at the first
 *    NUL, so `_id: "\0"` read back as `""`. A driver issue, resolved by item 31's text storage
 *    contract (`docs/adr/0005-text-storage-contract.md`): `Storage` rejects such text before commit
 *    and fails a read of older stored NUL text instead of truncating it. Regression 2 asserts that
 *    through the real `StorageLive`. The generated documents still exclude U+0000: this oracle runs
 *    compiled SQL directly, beneath the Storage boundary where the contract is enforced.
 *
 * `node:sqlite` opens only `:memory:` databases here (the property and `runOne`). Regression 2 works
 * on a file `Storage` holds through the FFI binding, so it touches that file only through the same
 * binding (`tests/helpers/sqlite_inspect.ts`); see FFI plan §1.2 and `tests/unit/boundaries_test.ts`.
 */
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { Effect } from "effect";
import fc from "fast-check";
import { type Node, NUL, type Schema, Table } from "../../core/schema.ts";
import type { SelectPlan } from "../../core/plan.ts";
import type { SQL } from "../../core/compile/dialect.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileSelect } from "../../core/compile/select.ts";
import { compileDelete, compileInsert, compileReplace } from "../../core/compile/write.ts";
import { decode } from "../../core/compile/decode.ts";
import {
  bodyArb,
  type Doc,
  rightOf,
  runs,
  show,
  SYSTEM,
  tableArb,
  tableName,
  textValue,
  validPlanArb,
} from "./compiler_arbitraries.ts";
import { titled } from "./arbitraries.ts";
import { makeStorage } from "../../platform/sqlite.ts";
import { defineFunctions } from "../../sdk/functions.ts";
import { runTest } from "../helpers/run.ts";
import { inspect } from "../helpers/sqlite_inspect.ts";

// ---------------------------------------------------------------------------
// Reference implementation
// ---------------------------------------------------------------------------

/** Code-point order, which is what SQLite's BINARY collation over UTF-8 amounts to. */
function compareText(a: string, b: string): number {
  const x = [...a];
  const y = [...b];
  for (let i = 0; i < Math.min(x.length, y.length); i++) {
    const d = x[i].codePointAt(0)! - y[i].codePointAt(0)!;
    if (d !== 0) return d;
  }
  return x.length - y.length;
}

/** Non-null values of one node kind, in SQLite order. */
function compareValues(node: Node, a: unknown, b: unknown): number {
  if (node.kind === "string" || node.kind === "enum") return compareText(a as string, b as string);
  const x = Number(a);
  const y = Number(b);
  return x < y ? -1 : x > y ? 1 : 0;
}

/** Ascending puts NULLs first and descending puts them last, as the compiler's ORDER BY promises. */
function compareNullable(node: Node, a: unknown, b: unknown, direction: "asc" | "desc"): number {
  if (a === null || b === null) {
    if (a === b) return 0;
    const nullFirst = a === null ? -1 : 1;
    return direction === "asc" ? nullFirst : -nullFirst;
  }
  const c = compareValues(node, a, b);
  return direction === "asc" ? c : -c;
}

function matches(doc: Doc, plan: SelectPlan): boolean {
  return plan.filters.every(({ field, op, value }) => {
    const stored = doc[field];
    if (op === "eq") return value === null ? stored === null : stored !== null && stored === value;
    return stored !== null && (stored as number) > (value as number);
  });
}

/** What the plan means, applied to the in-memory documents. */
function reference(table: Table, docs: readonly Doc[], plan: SelectPlan): unknown {
  const nodeOf = (field: string): Node => SYSTEM[field] ?? table.fields[field].node;
  const hits = docs.filter((doc) => matches(doc, plan));
  if (plan.aggregate?.kind === "count") return hits.length;
  if (plan.aggregate?.kind === "countBy") {
    const field = plan.aggregate.field;
    const counts = new Map<unknown, number>();
    for (const doc of hits) counts.set(doc[field], (counts.get(doc[field]) ?? 0) + 1);
    return [...counts]
      .map(([value, count]) => ({ value, count }))
      .sort((a, b) => compareNullable(nodeOf(field), a.value, b.value, "asc"))
      .slice(0, plan.limit);
  }
  const order = plan.order;
  return [...hits]
    .sort((a, b) =>
      (order ? compareNullable(nodeOf(order.field), a[order.field], b[order.field], order.direction) : 0) ||
      compareText(a._id, b._id)
    )
    .slice(0, plan.limit);
}

// ---------------------------------------------------------------------------
// Scenario
// ---------------------------------------------------------------------------

type Mutation = { at: number; body?: Record<string, unknown> };

type Scenario = {
  name: string;
  table: Table;
  inserted: Doc[];
  mutations: Mutation[];
  final: Doc[];
  plans: SelectPlan[];
};

/** The documents after the mutations: a replace swaps the body and bumps `_version`; a delete removes. */
function applyMutations(inserted: readonly Doc[], mutations: readonly Mutation[]): Doc[] {
  const live = new Map(inserted.map((doc) => [doc._id, doc]));
  for (const { at, body } of mutations) {
    const id = inserted[at]._id;
    const current = live.get(id);
    if (!current) continue; // replacing a deleted document updates no row
    if (body) {
      live.set(id, {
        ...body,
        _id: id,
        _creationTime: current._creationTime,
        _version: current._version + 1,
      });
    } else live.delete(id);
  }
  return [...live.values()];
}

/**
 * Whether a document holds text the storage contract refuses: a top-level string containing U+0000
 * (finding 2). Array items need no check: arrays travel as JSON text, which escapes the NUL.
 */
function outsideContract(doc: Record<string, unknown>): boolean {
  return Object.values(doc).some((value) => typeof value === "string" && value.includes(NUL));
}

/** Small pools for ids and times, so ties on `_creationTime` happen and `_id` must break them. */
const idArb = fc.oneof(textValue, fc.uuid());
const timeArb = fc.oneof(fc.integer({ min: 0, max: 3 }), fc.integer({ min: 0, max: 2 ** 45 }));

const scenario: fc.Arbitrary<Scenario> = fc.tuple(tableName, tableArb()).chain(([name, table]) => {
  const schema: Schema = { [name]: table };
  const body = bodyArb(table).filter((b) => !outsideContract(b));
  const doc = fc.record({ _id: idArb.filter((id) => !id.includes(NUL)), _creationTime: timeArb, body });
  return fc.uniqueArray(doc, { selector: (d) => d._id, maxLength: 12 }).chain((rows) => {
    const inserted: Doc[] = rows.map(({ _id, _creationTime, body }) => ({
      ...body,
      _id,
      _creationTime,
      _version: 1,
    }));
    const mutations: fc.Arbitrary<Mutation[]> = inserted.length
      ? fc.array(
        fc.record({
          at: fc.nat({ max: inserted.length - 1 }),
          body: fc.option(body, { nil: undefined, freq: 3 }),
        }),
        { maxLength: 4 },
      )
      : fc.constant([]);
    return mutations.chain((mutations) => {
      const final = applyMutations(inserted, mutations);
      return fc
        .array(validPlanArb(schema, name, final), { minLength: 1, maxLength: 6 })
        .map((plans) => ({ name, table, inserted, mutations, final, plans }));
    });
  });
});

/** The body fields of a stored document, without its metadata. */
function bodyOf(doc: Doc): Record<string, unknown> {
  const { _id: _i, _creationTime: _c, _version: _v, ...body } = doc;
  return body;
}

Deno.test(titled("oracle: compiled selects on real SQLite return what the plan means"), () => {
  fc.assert(
    fc.property(scenario, ({ name, table, inserted, mutations, final, plans }) => {
      const schema: Schema = { [name]: table };
      const db = new DatabaseSync(":memory:");
      const run = (sql: SQL) => db.prepare(sql.text).run(...sql.values);
      try {
        for (const statement of rightOf(compileDDL(schema))) db.exec(statement);
        for (const doc of inserted) {
          run(
            rightOf(
              compileInsert(schema, name, bodyOf(doc), { _id: doc._id, _creationTime: doc._creationTime }),
            ),
          );
        }
        for (const { at, body } of mutations) {
          const id = inserted[at]._id;
          run(rightOf(body ? compileReplace(schema, name, id, body) : compileDelete(schema, name, id)));
        }
        for (const plan of plans) {
          const sql = rightOf(compileSelect(schema, plan, "sqlite"));
          const rows = db.prepare(sql.text).all(...sql.values);
          const actual = rightOf(decode(schema, plan, rows));
          assert.deepStrictEqual(actual, reference(table, final, plan), `${sql.text} ${show(sql.values)}`);
        }
      } finally {
        db.close();
      }
    }),
    runs(50),
  );
});

// ---------------------------------------------------------------------------
// Regressions for what the oracle found (default suite; no opt-in)
// ---------------------------------------------------------------------------

/** Runs one plan against a fresh database holding `bodies`, returning the decoded result. */
function runOne(table: Table, bodies: Record<string, unknown>[], plan: SelectPlan): unknown {
  const schema: Schema = { t: table };
  const db = new DatabaseSync(":memory:");
  try {
    for (const statement of rightOf(compileDDL(schema))) db.exec(statement);
    bodies.forEach((body, i) => {
      const sql = rightOf(compileInsert(schema, "t", body, { _id: `id${i}`, _creationTime: i }));
      db.prepare(sql.text).run(...sql.values);
    });
    const sql = rightOf(compileSelect(schema, plan, "sqlite"));
    return rightOf(decode(schema, plan, db.prepare(sql.text).all(...sql.values)));
  } finally {
    db.close();
  }
}

const bigNumber = new Table({ n: { node: { kind: "number" } } }, "json");
const big = 1693240866440448800; // the double 1693240866440448768; its shortest decimal is not

Deno.test("oracle regression 1a: json layout, eq on a number field holding an integral double >= 2^53", () => {
  const plan: SelectPlan = {
    kind: "select",
    table: "t",
    filters: [{ field: "n", op: "eq", value: big }],
    limit: 10,
  };
  assert.deepStrictEqual(runOne(bigNumber, [{ n: big }], plan), [{
    n: big,
    _id: "id0",
    _creationTime: 0,
    _version: 1,
  }]);
});

Deno.test("oracle regression 1b: json layout, countBy over a number field holding 2^53", () => {
  const plan: SelectPlan = {
    kind: "select",
    table: "t",
    filters: [],
    limit: 10,
    aggregate: { kind: "countBy", field: "n" },
  };
  assert.deepStrictEqual(runOne(bigNumber, [{ n: 2 ** 53 }], plan), [{ value: 2 ** 53, count: 1 }]);
});

/**
 * Regression 2, through the real `StorageLive` rather than raw compiled SQL: text with U+0000 is
 * rejected before commit, and the same text written the way this file used to write it (compiled SQL,
 * beneath Storage) fails the read rather than coming back truncated. The full matrix — positions, ids,
 * patch, both `Storage` implementations — is `tests/unit/nul_contract_test.ts`.
 */
for (const layout of ["columns", "json"] as const) {
  runTest(
    `oracle regression 2: U+0000 text is rejected before commit, never read back truncated (${layout})`,
    Effect.gen(function* () {
      const schema: Schema = { t: new Table({ s: { node: { kind: "string" } } }, layout) };
      const dir = yield* Effect.acquireRelease(
        Effect.sync(() => mkdtempSync(join(tmpdir(), "mansio-oracle-"))),
        (dir) => Effect.sync(() => rmSync(dir, { recursive: true, force: true })),
      );
      const file = join(dir, "app.sqlite");
      const storage = yield* makeStorage({ file, schema });
      const app = defineFunctions(schema);
      const all = app.mutation({
        args: {},
        handler: ({ db }) => db.find("t").collect(),
      });
      const insertBoth = app.mutation({
        args: {},
        handler: ({ db }) =>
          Effect.gen(function* () {
            yield* db.insert("t", { s: "ok" });
            return yield* db.insert("t", { s: "a\u0000b" });
          }),
      });

      const rejected = yield* Effect.flip(storage.mutation(insertBoth, {}));
      assert.deepStrictEqual({ ...rejected }, {
        _tag: "SchemaError",
        reason: "type-mismatch",
        path: "s",
        expected: "text without U+0000",
        actual: "a\u0000b",
      });
      assert.deepStrictEqual(yield* storage.mutation(all, {}), [], "the earlier insert rolled back too");

      // The old bypass: `_id: "\0"` and `s: "a\0b"` committed beneath Storage, as before item 31.
      // Storage holds this file open through the FFI binding, so the bypass goes through the same
      // binding (`inspect`), never `node:sqlite`: two SQLite copies on one file in one process drop
      // each other's POSIX locks (FFI plan §1.2). The binding reads TEXT by its byte length, so the
      // raw row comes back whole; only Storage's read contract rejects it.
      const db = inspect(file, { write: true });
      try {
        const sql = rightOf(compileInsert(schema, "t", { s: "a\u0000b" }, { _id: NUL, _creationTime: 0 }));
        db.run(sql.text, sql.values);
        assert.equal(db.get('SELECT "_id" FROM "t"')?._id, NUL, "the binding reads the stored text whole");
      } finally {
        db.close();
      }
      const stored = yield* Effect.flip(storage.mutation(all, {}));
      assert.deepStrictEqual(
        [stored._tag, (stored as { operation?: string }).operation, (stored as { table?: string }).table],
        ["StorageError", "select", "t"],
      );
    }),
  );
}
