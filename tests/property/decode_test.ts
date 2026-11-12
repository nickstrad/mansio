/**
 * Property tests for `core/compile/decode.ts`: what the write path encodes, the
 * read path decodes back to the same document, whatever form the driver hands each value back in.
 *
 * The rows are built from `compileInsert`'s bound values — the exact bytes the writer stores — and
 * then re-dressed the way the two engines return them: `node:sqlite` rows have a null prototype;
 * DuckDB returns `BIGINT` (every SQLite `INTEGER` it reads through `ATTACH`) as a `bigint`, and a
 * boolean either as an integer or, through its `BOOLEAN` cast, as a real boolean.
 *
 * The failure half is about precision: an integer outside the safe range must fail as a
 * `DecodeError` with reason `unsafe-integer` (this codebase's "SafeIntegerError") rather than
 * decode to a nearby double. For a `json`-layout body field the integer arrives as JSON text, where
 * `JSON.parse` has already rounded it, so there the guarantee is that the row fails schema
 * re-validation — it must still never decode.
 */
import assert from "node:assert/strict";
import fc from "fast-check";
import { Either } from "effect";
import { type Node, type Schema, Table } from "../../core/schema.ts";
import type { SelectPlan } from "../../core/plan.ts";
import type { Row, SQL } from "../../core/compile/dialect.ts";
import { encode } from "../../core/compile/select.ts";
import { compileInsert } from "../../core/compile/write.ts";
import { decode, safeNumber } from "../../core/compile/decode.ts";
import {
  bodyArb,
  fieldName,
  fieldsArb,
  leftOf,
  nodeArb,
  rightOf,
  runs,
  show,
  SYSTEM,
  tableArb,
  tableName,
  textValue,
  valueFor,
} from "./compiler_arbitraries.ts";
import { titled } from "./arbitraries.ts";

const MAX = BigInt(Number.MAX_SAFE_INTEGER);

/** How a driver hands one value back. Applied per column, so one row mixes forms. */
type Form = 0 | 1 | 2;
const forms = fc.array(fc.constantFrom<Form>(0, 1, 2), { minLength: 1, maxLength: 12 });

/**
 * One stored value as a driver might return it: integers as `number` or `bigint`, booleans as
 * `0`/`1`, `0n`/`1n` or `false`/`true`. Everything else (text, REAL, JSON text) is returned as stored.
 */
function driverForm(node: Node | undefined, value: unknown, form: Form): unknown {
  if (value === null || node === undefined) return value;
  if (node.kind === "integer") return form === 0 ? value : BigInt(value as number);
  if (node.kind === "boolean") return [value, BigInt(value as number), value === 1][form];
  return value;
}

/** The row an engine returns for a freshly inserted document, built from the insert's bound values. */
function rowFor(table: Table, insert: SQL, columnForms: Form[], nullPrototype: boolean): Row {
  const names = [
    "_id",
    "_creationTime",
    "_version",
    ...(table.storage === "json" ? ["doc"] : Object.keys(table.fields)),
  ];
  const row: Row = nullPrototype ? Object.create(null) : {};
  names.forEach((name, i) => {
    const node = SYSTEM[name] ??
      (name === "doc" && table.storage === "json" ? undefined : table.fields[name]?.node);
    row[name] = driverForm(node, insert.values[i], columnForms[i % columnForms.length]);
  });
  return row;
}

const rowsPlan = (table: string): SelectPlan => ({ kind: "select", table, filters: [], limit: 1 });

/** A table, a body for it, and the metadata the writer would have generated. */
const stored = fc.tuple(tableName, tableArb()).chain(([name, table]) =>
  fc.record({
    name: fc.constant(name),
    table: fc.constant(table),
    body: bodyArb(table),
    id: textValue,
    time: fc.oneof(fc.nat(), fc.constant(Number.MAX_SAFE_INTEGER)),
    forms,
    nullPrototype: fc.boolean(),
  })
);

Deno.test(
  titled("decode: decode(insert(doc)) round-trips in both layouts, whatever the driver forms"),
  () => {
    fc.assert(
      fc.property(stored, ({ name, table, body, id, time, forms, nullPrototype }) => {
        const schema: Schema = { [name]: table };
        const insert = rightOf(compileInsert(schema, name, body, { _id: id, _creationTime: time }));
        const row = rowFor(table, insert, forms, nullPrototype);
        const decoded = rightOf(decode(schema, rowsPlan(name), [row]));
        assert.deepStrictEqual(decoded, [{ ...body, _id: id, _creationTime: time, _version: 1 }], show(row));
      }),
      runs(500),
    );
  },
);

/** A scalar node and a value for it, for the aggregate decode paths. */
const scalarCase = nodeArb(0).chain((node) =>
  fc.record({ node: fc.constant(node), value: valueFor(node), form: fc.constantFrom<Form>(0, 1, 2) })
);

Deno.test(titled("decode: count and countBy decode driver integers and booleans to plain values"), () => {
  const arb = fc.record({
    name: tableName,
    field: fieldName,
    groups: fc.array(fc.tuple(scalarCase, fc.nat(), fc.constantFrom<Form>(0, 1)), { maxLength: 4 }),
    total: fc.tuple(fc.nat(), fc.constantFrom<Form>(0, 1)),
    storage: fc.constantFrom("columns" as const, "json" as const),
  });
  fc.assert(
    fc.property(arb, ({ name, field, groups, total: [total, totalForm], storage }) => {
      const countPlan: SelectPlan = { ...rowsPlan(name), aggregate: { kind: "count" } };
      const countRow = { count: driverForm(SYSTEM._version, total, totalForm) };
      const emptySchema: Schema = { [name]: new Table({}, storage) };
      assert.equal(rightOf(decode(emptySchema, countPlan, [countRow])), total);

      for (const [{ node, value, form }, count, countForm] of groups) {
        const schema: Schema = { [name]: new Table({ [field]: { node } }, storage) };
        const plan: SelectPlan = { ...rowsPlan(name), aggregate: { kind: "countBy", field } };
        const row = {
          value: driverForm(node, encode(node, value), form),
          count: driverForm(SYSTEM._version, count, countForm),
        };
        assert.deepStrictEqual(rightOf(decode(schema, plan, [row])), [{ value, count }], show(row));
      }
    }),
    runs(),
  );
});

// ---------------------------------------------------------------------------
// Unsafe integers
// ---------------------------------------------------------------------------

/** Integers outside ±(2^53 - 1), as the bigints DuckDB returns and as the doubles a lossy path yields. */
const unsafeInteger: fc.Arbitrary<bigint | number> = fc.oneof(
  fc.bigInt({ min: MAX + 1n, max: 2n ** 80n }),
  fc.bigInt({ min: -(2n ** 80n), max: -(MAX + 1n) }),
  fc.constantFrom(MAX + 1n, MAX + 2n, -(MAX + 1n), 2n ** 63n - 1n, -(2n ** 63n)),
  fc.constantFrom(2 ** 53, -(2 ** 53), 2 ** 53 + 2, 1e20, -1e300),
);

Deno.test(titled("decode: safeNumber is exact or fails — it never rounds"), () => {
  fc.assert(
    fc.property(
      fc.oneof(
        fc.bigInt({ min: -(2n ** 70n), max: 2n ** 70n }),
        fc.bigInt({ min: -MAX - 3n, max: -MAX + 3n }),
        fc.bigInt({ min: MAX - 3n, max: MAX + 3n }),
      ),
      (b) => {
        const result = safeNumber(b);
        const safe = b >= -MAX && b <= MAX;
        assert.equal(Either.isRight(result), safe, `${b}n`);
        if (Either.isRight(result)) assert.equal(BigInt(result.right), b);
        else assert.equal(result.left.reason, "unsafe-integer");
      },
    ),
    runs(1000),
  );
  fc.assert(
    fc.property(
      fc.oneof(fc.double(), fc.integer(), unsafeInteger.filter((x) => typeof x === "number")),
      (x) => {
        const result = safeNumber(x);
        assert.equal(Either.isRight(result), Number.isSafeInteger(x), String(x));
        if (Either.isRight(result)) assert.ok(Object.is(result.right, x));
      },
    ),
    runs(1000),
  );
});

/** A table guaranteed to have one integer field `i`, so there is a body column to corrupt. */
const withInteger = fc
  .tuple(tableName, fieldName, fieldsArb, fc.boolean(), fc.constantFrom("columns" as const, "json" as const))
  .chain(([name, i, others, nullable, storage]) => {
    const fields = Object.fromEntries(
      Object.entries(others).filter(([k]) => k.toLowerCase() !== i.toLowerCase()),
    );
    fields[i] = { node: nullable ? { kind: "integer", nullable: true } : { kind: "integer" } };
    const table = new Table(fields, storage);
    return fc.record({
      name: fc.constant(name),
      table: fc.constant(table),
      integer: fc.constant(i),
      body: bodyArb(table),
      forms,
      target: fc.constantFrom(
        "_creationTime",
        "_version",
        "field",
        "count",
        "countBy-count",
        "countBy-value",
      ),
      unsafe: unsafeInteger,
    });
  });

Deno.test(titled("decode: an integer outside the safe range fails to decode, never truncates"), () => {
  fc.assert(
    fc.property(withInteger, ({ name, table, integer, body, forms, target, unsafe }) => {
      const schema: Schema = { [name]: table };
      const insert = rightOf(compileInsert(schema, name, body, { _id: "id", _creationTime: 1 }));
      const row = rowFor(table, insert, forms, false);
      let plan = rowsPlan(name);
      let rows: Row[] = [row];
      let field: string = target;
      if (target === "_creationTime" || target === "_version") {
        row[target] = unsafe;
      } else if (target === "field") {
        field = integer;
        if (table.storage === "columns") {
          row[integer] = unsafe;
        } else {
          // The writer stores JSON text, where an out-of-range integer is a bare numeric literal.
          const marker = "\u0000unsafe\u0000";
          const doc = JSON.parse(insert.values[3] as string);
          doc[integer] = marker;
          row.doc = JSON.stringify(doc).replace(JSON.stringify(marker), String(unsafe));
        }
      } else if (target === "count") {
        plan = { ...plan, aggregate: { kind: "count" } };
        rows = [{ count: unsafe }];
      } else {
        plan = { ...plan, aggregate: { kind: "countBy", field: integer } };
        rows = target === "countBy-count" ? [{ value: 1, count: unsafe }] : [{ value: unsafe, count: 1 }];
        field = target === "countBy-count" ? "count" : integer;
      }
      const error = leftOf(decode(schema, plan, rows)) as { _tag: string; reason: string; field?: string };
      if (target === "field" && table.storage === "json") {
        // JSON.parse has already rounded the literal to a double; re-validation must refuse it.
        assert.equal(`${error._tag}/${error.reason}`, "SchemaError/type-mismatch", show(rows));
      } else {
        assert.equal(`${error._tag}/${error.reason}`, "DecodeError/unsafe-integer", show(rows));
        assert.equal(error.field, field);
      }
    }),
    runs(500),
  );
});
