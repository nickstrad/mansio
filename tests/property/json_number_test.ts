/**
 * `number` fields read as doubles in both storage layouts on SQLite.
 *
 * The writer stores a `json`-layout document as `JSON.stringify(body)`, and the shortest decimal of
 * an integral double at or beyond 2^53 is integer text: `1693240866440448800` for the double
 * 1693240866440448768. SQLite's `json_extract` reads integer text below 2^63 as an exact INTEGER,
 * not as the double, so before the compiler cast `number` reads to REAL, `eq x` missed the row,
 * `gt x` matched it against itself, ordering ranked it by the wrong value and `countBy` returned an
 * INTEGER beyond 2^53 that `node:sqlite` refuses to read.
 *
 * Each case writes one generated document set into a `columns` and a `json` table, runs a batch of
 * generated plans on both, and requires each answer to equal a JS reference that compares doubles.
 * The values lean hard on the boundaries: 2^53 and its representable neighbours, the reported value
 * and its neighbours, their negations, values up to `Number.MAX_VALUE`, small fractions, safe
 * integers, `-0` and NULL, mixed with arbitrary finite doubles. A nullable `integer` field rides
 * along so the integer path, which keeps its exact INTEGER read and safe-integer decode, is checked
 * against the same reference.
 *
 * Only `node:sqlite` is loaded here. DuckDB's leg of the same matrix runs in its own process from
 * `tests/unit/number_dialects_test.ts`. The seed comes from `arbitraries.ts`'s harness and is in
 * every test name; `PROP_SEED=<n>` replays or explores.
 */
import assert from "node:assert/strict";
import { DatabaseSync } from "node:sqlite";
import fc from "fast-check";
import { type Schema, Table, v } from "../../core/schema.ts";
import type { SelectPlan } from "../../core/plan.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileSelect } from "../../core/compile/select.ts";
import { compileInsert } from "../../core/compile/write.ts";
import { decode } from "../../core/compile/decode.ts";
import { rightOf, runs, show } from "./compiler_arbitraries.ts";
import { SEED, titled } from "./arbitraries.ts";

// ---------------------------------------------------------------------------
// Values
// ---------------------------------------------------------------------------

function nextUp(x: number): number {
  if (x === 0) return Number.MIN_VALUE;
  const bits = new BigInt64Array(new Float64Array([x]).buffer);
  bits[0] += x > 0 ? 1n : -1n;
  return new Float64Array(bits.buffer)[0];
}
const nextDown = (x: number): number => -nextUp(-x);

const P53 = 2 ** 53;
const REPORTED = 1693240866440448800;

/** Points whose neighbourhoods matter: where JSON integer text and the double part ways. */
const ANCHORS = [P53, REPORTED, 2 ** 62, 2 ** 63, 1e21, Number.MAX_VALUE, 1, 0.5];

/** An anchor, possibly negated, moved up to three representable doubles either way. */
const boundaryArb: fc.Arbitrary<number> = fc
  .tuple(fc.constantFrom(...ANCHORS), fc.boolean(), fc.integer({ min: -3, max: 3 }))
  .map(([anchor, negate, steps]) => {
    let x = negate ? -anchor : anchor;
    for (let k = 0; k < Math.abs(steps); k++) x = steps > 0 ? nextUp(x) : nextDown(x);
    return x;
  })
  .filter(Number.isFinite);

const numberArb: fc.Arbitrary<number> = fc.oneof(
  { weight: 4, arbitrary: boundaryArb },
  { weight: 2, arbitrary: fc.maxSafeInteger() },
  { weight: 2, arbitrary: fc.double({ noNaN: true, noDefaultInfinity: true }) },
  { weight: 1, arbitrary: fc.constantFrom(0, -0, 0.1, -2.75, Number.MIN_VALUE, -Number.MIN_VALUE) },
  // Integral doubles just past 2^53, where every other integer is not representable.
  { weight: 2, arbitrary: fc.integer({ min: 0, max: 1 << 20 }).map((k) => P53 + 2 * k) },
);

const integerArb: fc.Arbitrary<number> = fc.oneof(
  fc.maxSafeInteger(),
  fc.constantFrom(0, -1, Number.MAX_SAFE_INTEGER, -Number.MAX_SAFE_INTEGER),
);

const nullable = <T>(arb: fc.Arbitrary<T>) => fc.option(arb, { nil: null, freq: 5 });

// ---------------------------------------------------------------------------
// Scenario
// ---------------------------------------------------------------------------

type Doc = { _id: string; _creationTime: number; _version: number; n: number | null; i: number | null };
type Field = "n" | "i";

const fields = { n: v.nullable(v.number()), i: v.nullable(v.integer()) };
const indexes = [{ name: "by_n", fields: ["n"] }];
const SCHEMAS: Record<"columns" | "json", Schema> = {
  columns: { nums: new Table(fields, "columns", indexes) },
  json: { nums: new Table(fields, "json", indexes) },
};

/** A filter value for `field`: usually one a document holds (so `eq` hits), sometimes a fresh one. */
function valueArb(field: Field, docs: readonly Doc[]): fc.Arbitrary<number> {
  const held = docs.map((d) => d[field]).filter((x): x is number => x !== null);
  const fresh = field === "n" ? numberArb : integerArb;
  return held.length ? fc.oneof(fc.constantFrom(...held), fresh) : fresh;
}

function planArb(docs: readonly Doc[]): fc.Arbitrary<SelectPlan> {
  return fc.constantFrom<Field>("n", "i").chain((field) => {
    const filter = fc.oneof(
      valueArb(field, docs).map((value) => ({ field, op: "eq" as const, value })),
      valueArb(field, docs).map((value) => ({ field, op: "gt" as const, value })),
      fc.constant({ field, op: "eq" as const, value: null }),
    );
    return fc.record({
      filters: fc.array(filter, { maxLength: 2 }),
      shape: fc.constantFrom("rows", "count", "countBy"),
      direction: fc.option(fc.constantFrom("asc" as const, "desc" as const), { nil: undefined }),
      limit: fc.oneof(fc.constant(1000), fc.integer({ min: 1, max: 5 })),
    }).map(({ filters, shape, direction, limit }): SelectPlan => ({
      kind: "select",
      table: "nums",
      filters,
      limit,
      ...(shape === "count" ? { aggregate: { kind: "count" } } : {}),
      ...(shape === "countBy" ? { aggregate: { kind: "countBy", field } } : {}),
      ...(shape === "rows" && direction ? { order: { field, direction } } : {}),
    }));
  });
}

const scenarioArb = fc
  .uniqueArray(
    fc.record({ _id: fc.stringMatching(/^[a-z0-9]{1,4}$/), n: nullable(numberArb), i: nullable(integerArb) }),
    { selector: (d) => d._id, maxLength: 16 },
  )
  .map((rows): Doc[] => rows.map((row, k) => ({ ...row, _creationTime: k, _version: 1 })))
  .chain((docs) =>
    fc.record({ docs: fc.constant(docs), plans: fc.array(planArb(docs), { minLength: 1, maxLength: 8 }) })
  );

// ---------------------------------------------------------------------------
// Reference
// ---------------------------------------------------------------------------

/** Neither layout keeps the sign of zero: SQLite stores REAL `-0.0` as `0.0`, and JSON writes `0`. */
const stored = (doc: Doc): Doc => ({ ...doc, n: doc.n === 0 ? 0 : doc.n });

function compareNullable(a: number | null, b: number | null, direction: "asc" | "desc"): number {
  if (a === null || b === null) {
    if (a === b) return 0;
    const nullFirst = a === null ? -1 : 1;
    return direction === "asc" ? nullFirst : -nullFirst;
  }
  const c = a < b ? -1 : a > b ? 1 : 0;
  return direction === "asc" ? c : -c;
}

/** Ids are ASCII here, so JS string order is SQLite's BINARY order. */
const byId = (a: Doc, b: Doc) => (a._id < b._id ? -1 : a._id > b._id ? 1 : 0);

function reference(docs: readonly Doc[], plan: SelectPlan): unknown {
  const get = (doc: Doc, field: string) => doc[field as Field];
  const hits = docs.map(stored).filter((doc) =>
    plan.filters.every(({ field, op, value }) => {
      const x = get(doc, field);
      if (op === "eq") return value === null ? x === null : x !== null && x === value;
      return x !== null && x > (value as number);
    })
  );
  if (plan.aggregate?.kind === "count") return hits.length;
  if (plan.aggregate?.kind === "countBy") {
    const field = plan.aggregate.field;
    const counts = new Map<number | null, number>();
    for (const doc of hits) counts.set(get(doc, field), (counts.get(get(doc, field)) ?? 0) + 1);
    return [...counts]
      .map(([value, count]) => ({ value, count }))
      .sort((a, b) => compareNullable(a.value, b.value, "asc"))
      .slice(0, plan.limit);
  }
  const order = plan.order;
  return hits
    .sort((a, b) =>
      (order ? compareNullable(get(a, order.field), get(b, order.field), order.direction) : 0) || byId(a, b)
    )
    .slice(0, plan.limit);
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

/** Writes `docs` into a fresh database in `schema`'s layout and answers each plan through `decode`. */
function answers(schema: Schema, docs: readonly Doc[], plans: readonly SelectPlan[]): unknown[] {
  const db = new DatabaseSync(":memory:");
  try {
    for (const statement of rightOf(compileDDL(schema))) db.exec(statement);
    for (const { _id, _creationTime, n, i } of docs) {
      const sql = rightOf(compileInsert(schema, "nums", { n, i }, { _id, _creationTime }));
      db.prepare(sql.text).run(...sql.values);
    }
    return plans.map((plan) => {
      const sql = rightOf(compileSelect(schema, plan, "sqlite"));
      return rightOf(decode(schema, plan, db.prepare(sql.text).all(...sql.values)));
    });
  } finally {
    db.close();
  }
}

Deno.test(titled("json numbers: eq, gt, ordering, count and countBy match doubles in both layouts"), () => {
  fc.assert(
    fc.property(scenarioArb, ({ docs, plans }) => {
      for (const layout of ["columns", "json"] as const) {
        const actual = answers(SCHEMAS[layout], docs, plans);
        plans.forEach((plan, k) => {
          assert.deepStrictEqual(actual[k], reference(docs, plan), `${layout} ${show(plan)}`);
        });
      }
    }),
    runs(),
  );
});

Deno.test(
  titled("json numbers: the boundary generator reaches 2^53, its neighbours and the reported value"),
  () => {
    // Without these, the property above could pass by never generating the values it exists for.
    const sample = new Set(fc.sample(boundaryArb, { numRuns: 2000, seed: SEED }));
    for (const x of [P53, nextUp(P53), nextDown(P53), -P53, REPORTED, nextUp(REPORTED), nextDown(REPORTED)]) {
      assert.ok(sample.has(x), `boundary generator never produced ${x}`);
    }
  },
);

Deno.test(titled("json numbers: the reported regressions, pinned in both layouts"), () => {
  const docs: Doc[] = [REPORTED, P53, nextUp(P53), Number.MAX_SAFE_INTEGER, -REPORTED, 0.5, null].map((
    n,
    k,
  ) => ({
    _id: `d${k}`,
    _creationTime: k,
    _version: 1,
    n,
    i: null,
  }));
  const plans: SelectPlan[] = [
    ...[REPORTED, P53, nextUp(P53), -REPORTED].flatMap((value): SelectPlan[] => [
      { kind: "select", table: "nums", filters: [{ field: "n", op: "eq", value }], limit: 10 },
      { kind: "select", table: "nums", filters: [{ field: "n", op: "gt", value }], limit: 10 },
    ]),
    { kind: "select", table: "nums", filters: [], limit: 10, order: { field: "n", direction: "asc" } },
    { kind: "select", table: "nums", filters: [], limit: 10, order: { field: "n", direction: "desc" } },
    { kind: "select", table: "nums", filters: [], limit: 10, aggregate: { kind: "countBy", field: "n" } },
  ];
  for (const layout of ["columns", "json"] as const) {
    const actual = answers(SCHEMAS[layout], docs, plans);
    plans.forEach((plan, k) =>
      assert.deepStrictEqual(actual[k], reference(docs, plan), `${layout} ${show(plan)}`)
    );
  }
  // The headline case, spelled out: `eq` finds exactly the one document, and `gt` does not match it.
  const [eq, gt] = answers(SCHEMAS.json, docs, plans.slice(0, 2)) as Doc[][];
  assert.deepEqual(eq.map((d) => d._id), ["d0"]);
  assert.ok(!gt.some((d) => d._id === "d0"));
});
