/**
 * Unit tests for `core/compile/*` and `core/plan.ts` (plan item 09).
 *
 * The compiler is now *total*: every rejection is a `CompileError`, `DecodeError` or `SchemaError`
 * value, and nothing in `core/compile/` raises. So the assertions below check tags and reasons, not
 * messages — except in the last section, which pins `sdk/schema.ts`'s `formatSchemaError` rendering:
 * those strings are the pre-port `Error` messages the SDK's compatibility boundary still throws. (The
 * Deno platform's `formatCompileError`/`formatDecodeError` went with it at IR-6.)
 *
 * The matrix that matters most is `expression`: each node kind x each physical layout x each
 * dialect. That function decides whether `moveNumber > 3` compares numbers or strings, and it is
 * the one place a dialect difference can silently change query semantics. `tests/core_test.ts` and
 * `GENERATED_SQL.txt` cover the app's real schema; this file covers the cells of the matrix the app
 * schema has no field for (`number`, `boolean`, nullable scalars).
 */
import assert from "node:assert/strict";
import { Either } from "effect";
import type { CompileError, DecodeError, SchemaError } from "../../core/errors.ts";
import { type Fields, type Node, type Schema, Table, v, withStorage } from "../../core/schema.ts";
import { MAX_LIMIT, MIN_LIMIT, normalise, type SelectPlan } from "../../core/plan.ts";
import { literal, placeholder, quote } from "../../core/compile/dialect.ts";
import {
  columnType,
  compileSelect,
  encode,
  expression,
  fieldNode,
  scalar,
  tableFor,
} from "../../core/compile/select.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileDelete, compileInsert, compileReplace } from "../../core/compile/write.ts";
import { decode, decodeValue, safeNumber } from "../../core/compile/decode.ts";
import type { SelectPlan as SdkSelectPlan } from "../../sdk/db.ts";
import { formatSchemaError } from "../../sdk/schema.ts";

const test = Deno.test;

// ---------------------------------------------------------------------------
// Fixtures and helpers
// ---------------------------------------------------------------------------

type AnyError = CompileError | DecodeError | SchemaError;

function expectRight<A>(either: Either.Either<A, AnyError>): A {
  if (Either.isRight(either)) return either.right;
  return assert.fail(`expected a right, got left ${JSON.stringify(either.left)}`);
}
function expectLeft<A>(either: Either.Either<A, AnyError>): AnyError {
  if (Either.isLeft(either)) return either.left;
  return assert.fail(`expected a left, got right ${JSON.stringify(either.right)}`);
}
/** Asserts a `CompileError` with this `reason`, and returns it for any further field checks. */
function expectCompile<A>(either: Either.Either<A, AnyError>, reason: CompileError["reason"]) {
  const error = expectLeft(either) as CompileError;
  assert.equal(error._tag, "CompileError");
  assert.equal(error.reason, reason);
  return error;
}
function expectDecode<A>(either: Either.Either<A, AnyError>, reason: DecodeError["reason"]) {
  const error = expectLeft(either) as DecodeError;
  assert.equal(error._tag, "DecodeError");
  assert.equal(error.reason, reason);
  return error;
}
function expectSchema<A>(either: Either.Either<A, AnyError>, reason: SchemaError["reason"]) {
  const error = expectLeft(either) as SchemaError;
  assert.equal(error._tag, "SchemaError");
  assert.equal(error.reason, reason);
  return error;
}

/** One field of every node kind, so the `expression`/`columnType`/`encode` matrices are complete. */
const fields = {
  s: v.string(),
  i: v.integer(),
  n: v.number(),
  b: v.boolean(),
  e: v.enum("a", "b"),
  arr: v.array(v.string(), 2),
  nil: v.nullable(v.integer()),
} satisfies Fields;

const probe = new Table(fields, "columns", [{ name: "by_s", fields: ["s", "i"] }]);
const columns: Schema = { probe };
const json: Schema = withStorage(columns, "json");
const table = probe;
const jsonTable = json.probe;

/** A body that satisfies every field above. */
const body = { s: "x", i: 7, n: 1.5, b: true, e: "a", arr: ["p", "q"], nil: null };

const base: SelectPlan = { kind: "select", table: "probe", filters: [], limit: 20 };
const dialects = ["sqlite", "duckdb"] as const;

// ---------------------------------------------------------------------------
// dialect.ts
// ---------------------------------------------------------------------------

test("quote accepts identifiers and rejects everything else", () => {
  assert.equal(expectRight(quote("games")), '"games"');
  assert.equal(expectRight(quote("_creationTime")), '"_creationTime"');
  for (const bad of ['a"b', "a-b", "1a", "", "drop table", 'x"); DROP TABLE "games"; --']) {
    const error = expectSchema(quote(bad), "invalid-identifier");
    assert.equal(error.path, bad);
  }
});

test("literal doubles embedded single quotes", () => {
  assert.equal(literal("playing"), "'playing'");
  assert.equal(literal("O'Hara"), "'O''Hara'");
  assert.equal(literal("''"), "''''''");
});

test("placeholder numbers binds per dialect", () => {
  assert.equal(placeholder("sqlite", 1), "?");
  assert.equal(placeholder("sqlite", 9), "?");
  assert.equal(placeholder("duckdb", 1), "$p1");
  assert.equal(placeholder("duckdb", 12), "$p12");
});

// ---------------------------------------------------------------------------
// select.ts: schema lookup
// ---------------------------------------------------------------------------

test("tableFor finds declared collections and names the missing one", () => {
  assert.equal(expectRight(tableFor(columns, "probe")), probe);
  const error = expectCompile(tableFor(columns, "nope"), "unknown-collection");
  assert.equal(error.table, "nope");
});

test("fieldNode covers metadata columns, declared fields and neither", () => {
  assert.deepEqual(expectRight(fieldNode(table, "_id")), { kind: "string" });
  assert.deepEqual(expectRight(fieldNode(table, "_creationTime")), { kind: "integer" });
  assert.deepEqual(expectRight(fieldNode(table, "_version")), { kind: "integer" });
  assert.equal(expectRight(fieldNode(table, "s")), fields.s.node);
  // Arrays are a field like any other to `fieldNode`; only `scalar` refuses them.
  assert.equal(expectRight(fieldNode(table, "arr")), fields.arr.node);
  const error = expectCompile(fieldNode(table, "nope"), "unknown-field");
  assert.equal(error.field, "nope");
});

test("scalar refuses array fields", () => {
  assert.equal(expectRight(scalar(table, "i")), fields.i.node);
  const error = expectCompile(scalar(table, "arr"), "array-predicate");
  assert.equal(error.field, "arr");
  expectCompile(scalar(table, "nope"), "unknown-field");
});

// ---------------------------------------------------------------------------
// select.ts: the expression matrix
// ---------------------------------------------------------------------------

test("expression reads a column directly in the columns layout, in both dialects", () => {
  for (const dialect of dialects) {
    for (const field of ["s", "i", "n", "b", "e", "nil"]) {
      assert.equal(expectRight(expression(table, field, dialect)), `"${field}"`);
    }
  }
});

test("expression reads metadata columns directly even in the json layout", () => {
  for (const dialect of dialects) {
    for (const field of ["_id", "_creationTime", "_version"]) {
      assert.equal(expectRight(expression(jsonTable, field, dialect)), `"${field}"`);
    }
  }
});

test("expression uses json_extract on sqlite, casting only number fields to REAL", () => {
  // SQLite's json_extract already returns the stored JSON type for every kind but `number`, so no
  // cast is needed or wanted there: an integer keeps its exact INTEGER and its safe-integer check.
  for (const field of ["s", "i", "b", "e", "nil"]) {
    assert.equal(
      expectRight(expression(jsonTable, field, "sqlite")),
      `json_extract("doc", '$.${field}')`,
    );
  }
  // A `number` is a double, but `JSON.stringify(2 ** 53)` is integer text that json_extract would
  // read as an exact INTEGER; the cast reads it back as the REAL the columns layout stores.
  assert.equal(
    expectRight(expression(jsonTable, "n", "sqlite")),
    `CAST(json_extract("doc", '$.n') AS REAL)`,
  );
  const nullableNumber = new Table({ m: v.nullable(v.number()) }, "json");
  assert.equal(
    expectRight(expression(nullableNumber, "m", "sqlite")),
    `CAST(json_extract("doc", '$.m') AS REAL)`,
  );
});

test("a json number field uses one REAL expression for filters, ordering, countBy and indexes", () => {
  const real = `CAST(json_extract("doc", '$.n') AS REAL)`;
  const filtered = expectRight(compileSelect(json, {
    ...base,
    filters: [{ field: "n", op: "eq", value: 2 ** 53 }, { field: "n", op: "gt", value: -0.5 }],
    order: { field: "n", direction: "desc" },
  }, "sqlite"));
  assert.equal(
    filtered.text,
    `SELECT * FROM "probe" WHERE ${real} = ? AND ${real} > ? ORDER BY ${real} DESC NULLS LAST, "_id" ASC LIMIT ?`,
  );
  // Filter values are bound unchanged: the fix is in the read expression, not in the values.
  assert.deepEqual(filtered.values, [2 ** 53, -0.5, 20]);
  const grouped = expectRight(
    compileSelect(json, { ...base, aggregate: { kind: "countBy", field: "n" } }, "sqlite"),
  );
  assert.equal(
    grouped.text,
    `SELECT ${real} AS "value", COUNT(*) AS "count" FROM "probe" GROUP BY 1 ORDER BY "value" ASC NULLS FIRST LIMIT ?`,
  );
  // An index on the field is built from the same expression, so the planner can use it.
  const indexed: Schema = { probe: new Table(fields, "json", [{ name: "by_n", fields: ["n"] }]) };
  assert.equal(
    expectRight(compileDDL(indexed))[1],
    `CREATE INDEX IF NOT EXISTS "idx_probe_by_n" ON "probe" (${real})`,
  );
  // A null filter on a nullable number still compares with IS NULL and binds nothing.
  const nullable: Schema = { probe: new Table({ m: v.nullable(v.number()) }, "json") };
  const nulls = expectRight(
    compileSelect(nullable, { ...base, filters: [{ field: "m", op: "eq", value: null }] }, "sqlite"),
  );
  assert.match(nulls.text, /WHERE CAST\(json_extract\("doc", '\$\.m'\) AS REAL\) IS NULL/);
  assert.deepEqual(nulls.values, [20]);
});

test("expression casts json_extract_string per node kind on duckdb", () => {
  const extract = (field: string) => `json_extract_string("doc", '$.${field}')`;
  const cases: [string, string][] = [
    ["s", extract("s")],
    ["e", extract("e")],
    ["i", `CAST(${extract("i")} AS BIGINT)`],
    ["nil", `CAST(${extract("nil")} AS BIGINT)`],
    ["n", `CAST(${extract("n")} AS DOUBLE)`],
    ["b", `CAST(CAST(${extract("b")} AS BOOLEAN) AS INTEGER)`],
  ];
  for (const [field, expected] of cases) {
    assert.equal(expectRight(expression(jsonTable, field, "duckdb")), expected);
  }
});

test("expression refuses arrays and unknown fields in every layout and dialect", () => {
  for (const dialect of dialects) {
    for (const t of [table, jsonTable]) {
      expectCompile(expression(t, "arr", dialect), "array-predicate");
      expectCompile(expression(t, "nope", dialect), "unknown-field");
    }
  }
});

test("expression rejects a field name that is not an identifier", () => {
  // Only reachable by bypassing `defineTable`, which validates names; this is the second defence.
  const hostile = new Table({ 'x"); DROP TABLE "probe': v.string() });
  for (const dialect of dialects) {
    expectSchema(expression(hostile, 'x"); DROP TABLE "probe', dialect), "invalid-identifier");
    expectSchema(
      expression(hostile.storeAs("json"), 'x"); DROP TABLE "probe', dialect),
      "invalid-identifier",
    );
  }
});

// ---------------------------------------------------------------------------
// select.ts: encode and columnType
// ---------------------------------------------------------------------------

test("encode maps document values onto bind values", () => {
  assert.equal(encode(fields.nil.node, null), null);
  assert.equal(encode(fields.arr.node, null), null);
  assert.equal(encode(fields.b.node, true), 1);
  assert.equal(encode(fields.b.node, false), 0);
  assert.equal(encode(fields.arr.node, ["p", "q"]), '["p","q"]');
  assert.equal(encode(fields.s.node, "x"), "x");
  assert.equal(encode(fields.i.node, 7), 7);
  assert.equal(encode(fields.n.node, 1.5), 1.5);
});

test("columnType maps node kinds onto STRICT column types", () => {
  const kinds: [Node["kind"], string][] = [
    ["integer", "INTEGER"],
    ["boolean", "INTEGER"],
    ["number", "REAL"],
    ["string", "TEXT"],
    ["enum", "TEXT"],
    ["array", "TEXT"],
  ];
  for (const [kind, expected] of kinds) assert.equal(columnType({ kind }), expected);
});

// ---------------------------------------------------------------------------
// select.ts: compileSelect
// ---------------------------------------------------------------------------

test("compileSelect emits the id tie-breaker and a bound limit by default", () => {
  const sqlite = expectRight(compileSelect(columns, base, "sqlite"));
  assert.equal(sqlite.text, 'SELECT * FROM "probe" ORDER BY "_id" ASC LIMIT ?');
  assert.deepEqual(sqlite.values, [20]);
  const duckdb = expectRight(compileSelect(columns, base, "duckdb"));
  // The reader sees the writer's database as the attached `app` schema.
  assert.equal(duckdb.text, 'SELECT * FROM "app"."probe" ORDER BY "_id" ASC LIMIT $p1');
  assert.deepEqual(duckdb.values, [20]);
});

test("compileSelect binds filter values positionally, before the limit", () => {
  const plan: SelectPlan = {
    ...base,
    filters: [{ field: "s", op: "eq", value: "x" }, { field: "i", op: "gt", value: 3 }],
  };
  const sqlite = expectRight(compileSelect(columns, plan, "sqlite"));
  assert.equal(
    sqlite.text,
    'SELECT * FROM "probe" WHERE "s" = ? AND "i" > ? ORDER BY "_id" ASC LIMIT ?',
  );
  assert.deepEqual(sqlite.values, ["x", 3, 20]);
  const duckdb = expectRight(compileSelect(columns, plan, "duckdb"));
  assert.match(duckdb.text, /WHERE "s" = \$p1 AND "i" > \$p2 .* LIMIT \$p3$/);
  assert.deepEqual(duckdb.values, ["x", 3, 20]);
});

test("compileSelect encodes booleans and compares null with IS NULL", () => {
  const boolean = expectRight(
    compileSelect(columns, { ...base, filters: [{ field: "b", op: "eq", value: false }] }, "sqlite"),
  );
  assert.match(boolean.text, /WHERE "b" = \?/);
  assert.deepEqual(boolean.values, [0, 20]);
  // `IS NULL` takes no bind, so the limit is still the first placeholder.
  const nulls = expectRight(
    compileSelect(columns, { ...base, filters: [{ field: "nil", op: "eq", value: null }] }, "duckdb"),
  );
  assert.match(nulls.text, /WHERE "nil" IS NULL/);
  assert.deepEqual(nulls.values, [20]);
});

test("compileSelect orders with NULLS FIRST ascending and NULLS LAST descending", () => {
  const asc = expectRight(
    compileSelect(columns, { ...base, order: { field: "nil", direction: "asc" } }, "sqlite"),
  );
  assert.equal(asc.text, 'SELECT * FROM "probe" ORDER BY "nil" ASC NULLS FIRST, "_id" ASC LIMIT ?');
  const desc = expectRight(
    compileSelect(columns, { ...base, order: { field: "nil", direction: "desc" } }, "sqlite"),
  );
  assert.equal(desc.text, 'SELECT * FROM "probe" ORDER BY "nil" DESC NULLS LAST, "_id" ASC LIMIT ?');
  // Ordering a json-stored field orders by the extracted, cast value, not by the raw text.
  const extracted = expectRight(
    compileSelect(json, { ...base, order: { field: "i", direction: "desc" } }, "duckdb"),
  );
  assert.match(
    extracted.text,
    /ORDER BY CAST\(json_extract_string\("doc", '\$\.i'\) AS BIGINT\) DESC NULLS LAST, "_id" ASC/,
  );
});

test("compileSelect counts every match, with no ORDER BY and no LIMIT", () => {
  const plan: SelectPlan = {
    ...base,
    filters: [{ field: "s", op: "eq", value: "x" }],
    aggregate: { kind: "count" },
  };
  const sql = expectRight(compileSelect(columns, plan, "sqlite"));
  assert.equal(sql.text, 'SELECT COUNT(*) AS "count" FROM "probe" WHERE "s" = ?');
  // The limit would otherwise cap the count at 20 rather than counting all matches.
  assert.deepEqual(sql.values, ["x"]);
});

test("compileSelect groups countBy on the extracted key and limits the groups", () => {
  const plan: SelectPlan = { ...base, aggregate: { kind: "countBy", field: "e" } };
  const sqlite = expectRight(compileSelect(columns, plan, "sqlite"));
  assert.equal(
    sqlite.text,
    'SELECT "e" AS "value", COUNT(*) AS "count" FROM "probe" GROUP BY 1 ORDER BY "value" ASC NULLS FIRST LIMIT ?',
  );
  assert.deepEqual(sqlite.values, [20]);
  const duckdb = expectRight(compileSelect(json, plan, "duckdb"));
  assert.match(
    duckdb.text,
    /SELECT json_extract_string\("doc", '\$\.e'\) AS "value", COUNT\(\*\) AS "count"/,
  );
  assert.match(duckdb.text, /GROUP BY 1 ORDER BY "value" ASC NULLS FIRST LIMIT \$p1$/);
});

test("compileSelect drops a caller's sort on an aggregate plan", () => {
  const order = { field: "s", direction: "asc" } as const;
  const counted = expectRight(
    compileSelect(columns, { ...base, order, aggregate: { kind: "count" } }, "sqlite"),
  );
  assert.equal(counted.text, 'SELECT COUNT(*) AS "count" FROM "probe"');
  const grouped = expectRight(
    compileSelect(columns, { ...base, order, aggregate: { kind: "countBy", field: "e" } }, "sqlite"),
  );
  // Only the grouping key's ordering survives.
  assert.equal(grouped.text.match(/ORDER BY/g)?.length, 1);
  assert.match(grouped.text, /ORDER BY "value" ASC NULLS FIRST/);
});

test("compileSelect reports every CompileError reason", () => {
  const plan = (over: Partial<SelectPlan>): SelectPlan => ({ ...base, ...over });
  expectCompile(compileSelect(columns, plan({ table: "nope" }), "sqlite"), "unknown-collection");
  expectCompile(compileSelect(columns, plan({ limit: 0 }), "sqlite"), "invalid-limit");
  expectCompile(
    compileSelect(columns, plan({ filters: [{ field: "nope", op: "eq", value: "x" }] }), "sqlite"),
    "unknown-field",
  );
  expectCompile(
    compileSelect(columns, plan({ filters: [{ field: "arr", op: "eq", value: ["p", "q"] }] }), "sqlite"),
    "array-predicate",
  );
  const unsupported = expectCompile(
    compileSelect(
      columns,
      plan({ filters: [{ field: "s", op: "lt" as "eq", value: "x" }] }),
      "sqlite",
    ),
    "unsupported-operator",
  );
  assert.equal(unsupported.field, "s");
  assert.equal(unsupported.detail, "lt");
  // `gt` on a non-numeric field, and `gt` against null, are the two halves of the same rule.
  expectCompile(
    compileSelect(columns, plan({ filters: [{ field: "s", op: "gt", value: "x" }] }), "sqlite"),
    "gt-requires-numeric",
  );
  expectCompile(
    compileSelect(columns, plan({ filters: [{ field: "nil", op: "gt", value: null }] }), "sqlite"),
    "gt-requires-numeric",
  );
  const ordering = expectCompile(
    compileSelect(
      columns,
      plan({ order: { field: "s", direction: "sideways" as "asc" } }),
      "sqlite",
    ),
    "invalid-ordering",
  );
  assert.equal(ordering.detail, "sideways");
});

test("compileSelect propagates a filter value that does not match its node", () => {
  const mismatch = expectSchema(
    compileSelect(columns, { ...base, filters: [{ field: "i", op: "eq", value: "7" }] }, "sqlite"),
    "type-mismatch",
  );
  assert.equal(mismatch.path, "i");
  assert.equal(mismatch.expected, "integer");
  // `null` against a non-nullable field fails validation before the IS NULL shortcut is reached.
  expectSchema(
    compileSelect(columns, { ...base, filters: [{ field: "i", op: "eq", value: null }] }, "sqlite"),
    "type-mismatch",
  );
});

// ---------------------------------------------------------------------------
// plan.ts
// ---------------------------------------------------------------------------

test("normalise accepts the limit range and rejects everything outside it", () => {
  for (const limit of [MIN_LIMIT, 2, 999, MAX_LIMIT]) {
    assert.equal(expectRight(normalise({ ...base, limit })).limit, limit);
  }
  for (const limit of [0, -1, 1.5, 1001, NaN, Infinity]) {
    const error = expectCompile(normalise({ ...base, limit }), "invalid-limit");
    assert.equal(error.table, "probe");
  }
});

test("normalise drops the order of an aggregate plan and leaves other plans alone", () => {
  const order = { field: "s", direction: "asc" } as const;
  assert.equal(
    expectRight(normalise({ ...base, order, aggregate: { kind: "count" } })).order,
    undefined,
  );
  const grouped = expectRight(normalise({ ...base, order, aggregate: { kind: "countBy", field: "e" } }));
  assert.equal(grouped.order, undefined);
  assert.deepEqual(expectRight(normalise({ ...base, order })).order, order);
  const plain = { ...base, aggregate: { kind: "count" } as const };
  assert.deepEqual(expectRight(normalise(plain)), plain);
});

// ---------------------------------------------------------------------------
// ddl.ts
// ---------------------------------------------------------------------------

test("compileDDL emits STRICT tables with the metadata columns and declared CHECKs", () => {
  const [create, index] = expectRight(compileDDL(columns));
  assert.equal(
    create,
    'CREATE TABLE IF NOT EXISTS "probe" ("_id" TEXT PRIMARY KEY NOT NULL, "_creationTime" INTEGER NOT NULL, ' +
      '"_version" INTEGER NOT NULL CHECK ("_version" >= 1), "s" TEXT NOT NULL, "i" INTEGER NOT NULL, ' +
      '"n" REAL NOT NULL, "b" INTEGER NOT NULL CHECK ("b" IN (0, 1)), ' +
      `"e" TEXT NOT NULL CHECK ("e" IN ('a', 'b')), ` +
      '"arr" TEXT NOT NULL CHECK ("arr" IS NULL OR json_valid("arr")), "nil" INTEGER) STRICT',
  );
  // Columns-layout indexes are plain column lists.
  assert.equal(index, 'CREATE INDEX IF NOT EXISTS "idx_probe_by_s" ON "probe" ("s", "i")');
});

test("compileDDL stores the json layout as one checked doc column, and indexes the extraction", () => {
  const [create, index] = expectRight(compileDDL(json));
  assert.equal(
    create,
    'CREATE TABLE IF NOT EXISTS "probe" ("_id" TEXT PRIMARY KEY NOT NULL, "_creationTime" INTEGER NOT NULL, ' +
      '"_version" INTEGER NOT NULL CHECK ("_version" >= 1), "doc" TEXT NOT NULL CHECK (json_valid("doc"))) STRICT',
  );
  // The indexed expression has to be the same one the WHERE clause uses, or the planner skips it.
  assert.equal(
    index,
    'CREATE INDEX IF NOT EXISTS "idx_probe_by_s" ON "probe" ' +
      `(json_extract("doc", '$.s'), json_extract("doc", '$.i'))`,
  );
});

test("compileDDL emits one statement per table, in declaration order", () => {
  const two: Schema = { a: new Table({ s: v.string() }), b: new Table({ i: v.integer() }) };
  const statements = expectRight(compileDDL(two));
  assert.equal(statements.length, 2);
  assert.match(statements[0], /^CREATE TABLE IF NOT EXISTS "a"/);
  assert.match(statements[1], /^CREATE TABLE IF NOT EXISTS "b"/);
  assert.deepEqual(expectRight(compileDDL({})), []);
});

test("compileDDL rejects an index over a missing or array field", () => {
  // `Table.addIndex` refuses both, so these tables can only be built by hand.
  const missing: Schema = { t: new Table({ s: v.string() }, "columns", [{ name: "x", fields: ["nope"] }]) };
  expectCompile(compileDDL(missing), "unknown-field");
  const arrays: Schema = {
    t: new Table({ arr: v.array(v.string()) }, "columns", [{ name: "x", fields: ["arr"] }]),
  };
  expectCompile(compileDDL(arrays), "array-predicate");
});

test("compileDDL rejects names that are not identifiers", () => {
  expectSchema(compileDDL({ 'a"b': new Table({ s: v.string() }) }), "invalid-identifier");
  expectSchema(compileDDL({ t: new Table({ 'a"b': v.string() }) }), "invalid-identifier");
  expectSchema(
    compileDDL({ t: new Table({ s: v.string() }, "columns", [{ name: 'a"b', fields: ["s"] }]) }),
    "invalid-identifier",
  );
});

test("compileDDL refuses an enum with zero values instead of emitting an unconstrained column", () => {
  // `v.enum` requires at least one value, so a zero-value node can only be built by hand —
  // defense in depth for a descriptor no write could ever populate.
  const empty: Schema = { t: new Table({ e: { node: { kind: "enum", values: [] } } }) };
  const error = expectCompile(compileDDL(empty), "empty-enum");
  assert.equal(error.field, "e");
  assert.equal(error.table, "t");
});

test("compileDDL omits the CHECK when an enum node has no values at all", () => {
  const absent: Schema = { t: new Table({ e: { node: { kind: "enum" } } }) };
  const [create] = expectRight(compileDDL(absent));
  assert.match(create, /"e" TEXT NOT NULL\) STRICT/);
});

// ---------------------------------------------------------------------------
// write.ts
// ---------------------------------------------------------------------------

const meta = { _id: "doc-1", _creationTime: 1700000000000 };

test("compileInsert writes one column per field in the columns layout", () => {
  const sql = expectRight(compileInsert(columns, "probe", body, meta));
  assert.equal(
    sql.text,
    'INSERT INTO "probe" ("_id", "_creationTime", "_version", "s", "i", "n", "b", "e", "arr", "nil") ' +
      "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
  );
  // `_version` starts at 1; booleans encode to 0/1 and arrays to JSON text.
  assert.deepEqual(sql.values, ["doc-1", 1700000000000, 1, "x", 7, 1.5, 1, "a", '["p","q"]', null]);
});

test("compileInsert writes the whole body as doc in the json layout", () => {
  const sql = expectRight(compileInsert(json, "probe", body, meta));
  assert.equal(
    sql.text,
    'INSERT INTO "probe" ("_id", "_creationTime", "_version", "doc") VALUES (?, ?, ?, ?)',
  );
  assert.deepEqual(sql.values, ["doc-1", 1700000000000, 1, JSON.stringify(body)]);
});

test("compileReplace sets every column, bumps the version in SQL, and binds the id last", () => {
  const sql = expectRight(compileReplace(columns, "probe", "doc-1", body));
  assert.equal(
    sql.text,
    'UPDATE "probe" SET "s" = ?, "i" = ?, "n" = ?, "b" = ?, "e" = ?, "arr" = ?, "nil" = ?, ' +
      '"_version" = "_version" + 1 WHERE "_id" = ?',
  );
  assert.deepEqual(sql.values, ["x", 7, 1.5, 1, "a", '["p","q"]', null, "doc-1"]);
  const asJson = expectRight(compileReplace(json, "probe", "doc-1", body));
  assert.equal(
    asJson.text,
    'UPDATE "probe" SET "doc" = ?, "_version" = "_version" + 1 WHERE "_id" = ?',
  );
  assert.deepEqual(asJson.values, [JSON.stringify(body), "doc-1"]);
});

test("compileDelete binds the id and still checks the collection exists", () => {
  const sql = expectRight(compileDelete(columns, "probe", "doc-1"));
  assert.equal(sql.text, 'DELETE FROM "probe" WHERE "_id" = ?');
  assert.deepEqual(sql.values, ["doc-1"]);
  expectCompile(compileDelete(columns, "nope", "doc-1"), "unknown-collection");
});

test("the write path rejects unknown collections and bodies that do not validate", () => {
  expectCompile(compileInsert(columns, "nope", body, meta), "unknown-collection");
  expectCompile(compileReplace(columns, "nope", "doc-1", body), "unknown-collection");
  expectSchema(compileInsert(columns, "probe", { ...body, extra: 1 }, meta), "unknown-field");
  expectSchema(compileInsert(columns, "probe", { ...body, i: "7" }, meta), "type-mismatch");
  expectSchema(compileInsert(columns, "probe", "not an object", meta), "not-an-object");
  // A partial body is not a legal replace: a missing field fails its node.
  expectSchema(compileReplace(columns, "probe", "doc-1", { s: "x" }), "type-mismatch");
});

// ---------------------------------------------------------------------------
// decode.ts
// ---------------------------------------------------------------------------

test("safeNumber narrows driver integers and refuses to lose precision", () => {
  assert.equal(expectRight(safeNumber(1700000000000n)), 1700000000000);
  assert.equal(expectRight(safeNumber(7)), 7);
  assert.equal(expectRight(safeNumber("7")), 7);
  for (const unsafe of [2n ** 70n, "nope", undefined, 1.5, NaN]) {
    const error = expectDecode(safeNumber(unsafe, "count", "probe"), "unsafe-integer");
    assert.equal(error.field, "count");
    assert.equal(error.table, "probe");
    assert.equal(error.actual, unsafe);
  }
});

test("decodeValue reverses encode for every node kind", () => {
  assert.equal(expectRight(decodeValue(fields.nil.node, null)), null);
  assert.equal(expectRight(decodeValue(fields.i.node, 7n)), 7);
  assert.equal(expectRight(decodeValue(fields.n.node, "1.5")), 1.5);
  // SQLite stores 0/1; DuckDB's BOOLEAN cast can return a real boolean, and BIGINT a bigint.
  assert.equal(expectRight(decodeValue(fields.b.node, 1)), true);
  assert.equal(expectRight(decodeValue(fields.b.node, 1n)), true);
  assert.equal(expectRight(decodeValue(fields.b.node, true)), true);
  assert.equal(expectRight(decodeValue(fields.b.node, 0)), false);
  assert.deepEqual(expectRight(decodeValue(fields.arr.node, '["p","q"]')), ["p", "q"]);
  assert.equal(expectRight(decodeValue(fields.s.node, "x")), "x");
  const error = expectDecode(decodeValue(fields.arr.node, "{oops", "arr", "probe"), "malformed-json");
  assert.equal(error.field, "arr");
});

/** The row a columns-layout `probe` document comes back as, with driver-native integers. */
const row = {
  _id: "doc-1",
  _creationTime: 1700000000000n,
  _version: 1n,
  s: "x",
  i: 7n,
  n: 1.5,
  b: 1,
  e: "a",
  arr: '["p","q"]',
  nil: null,
};

test("decode returns documents with safe numbers in either layout", () => {
  const fromColumns = expectRight(decode(columns, base, [row])) as Record<string, unknown>[];
  assert.deepEqual(fromColumns, [{ ...body, _id: "doc-1", _creationTime: 1700000000000, _version: 1 }]);
  const fromJson = expectRight(decode(json, base, [{
    _id: "doc-1",
    _creationTime: 1700000000000n,
    _version: 1n,
    doc: JSON.stringify(body),
  }])) as Record<string, unknown>[];
  assert.deepEqual(fromJson, fromColumns);
  assert.deepEqual(expectRight(decode(columns, base, [])), []);
});

test("decode reads a count out of the single aggregate row", () => {
  const plan: SelectPlan = { ...base, aggregate: { kind: "count" } };
  assert.equal(expectRight(decode(columns, plan, [{ count: 42n }])), 42);
  expectDecode(decode(columns, plan, [{ count: 2n ** 70n }]), "unsafe-integer");
  // An empty result set for a count is impossible from a real engine, and still does not raise.
  expectDecode(decode(columns, plan, []), "unsafe-integer");
});

test("decode decodes countBy groups through the grouped field's node", () => {
  const plan: SelectPlan = { ...base, aggregate: { kind: "countBy", field: "i" } };
  assert.deepEqual(
    expectRight(decode(columns, plan, [{ value: 7n, count: 2n }, { value: null, count: 1n }])),
    [{ value: 7, count: 2 }, { value: null, count: 1 }],
  );
  // The grouped field is resolved before any row is looked at, so a typo fails on an empty result.
  const unknown: SelectPlan = { ...base, aggregate: { kind: "countBy", field: "nope" } };
  expectCompile(decode(columns, unknown, []), "unknown-field");
  expectDecode(decode(columns, plan, [{ value: 7n, count: 1.5 }]), "unsafe-integer");
});

test("decode fails with a value, not a raised SyntaxError, on a corrupt row", () => {
  const corrupt = expectDecode(
    decode(json, base, [{ _id: "d", _creationTime: 1n, _version: 1n, doc: "{oops" }]),
    "malformed-json",
  );
  assert.equal(corrupt.field, "doc");
  assert.equal(corrupt.table, "probe");
  expectDecode(decode(columns, base, [{ ...row, arr: "{oops" }]), "malformed-json");
  expectDecode(decode(columns, base, [{ ...row, _creationTime: 2n ** 70n }]), "unsafe-integer");
  expectCompile(decode(columns, { ...base, table: "nope" }, []), "unknown-collection");
  // Storage that has drifted from the schema fails the same validation an insert would.
  expectSchema(decode(columns, base, [{ ...row, e: "not-in-enum" }]), "type-mismatch");
});

test("decode reports a missing column explicitly instead of coercing it downstream", () => {
  const partial = { ...row };
  delete (partial as Partial<typeof row>).e;
  const error = expectDecode(decode(columns, base, [partial]), "missing-column");
  assert.equal(error.field, "e");
  assert.equal(error.table, "probe");
});

// ---------------------------------------------------------------------------
// The IR mirror, and the legacy message rendering
// ---------------------------------------------------------------------------

type Mirrors<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;

test("core/plan.ts's SelectPlan mirrors sdk/db.ts's exactly", () => {
  // `core/` may not import `sdk/`, so the IR is mirrored rather than imported. This assertion is
  // what keeps the two copies honest: a field added on one side makes the next line a type error.
  const mirrored: Mirrors<SelectPlan, SdkSelectPlan> = true;
  assert.equal(mirrored, true);
});

test("formatSchemaError reproduces the pre-port messages verbatim", () => {
  // Schema failures keep `sdk/schema.ts`'s rendering, unchanged.
  assert.equal(
    formatSchemaError(expectLeft(compileInsert(columns, "probe", { ...body, extra: 1 }, meta)) as SchemaError)
      .message,
    "Unknown field: extra",
  );
  assert.equal(
    formatSchemaError(expectLeft(quote('a"b')) as SchemaError).message,
    'Invalid identifier: a"b',
  );
});

test("the Either compiler API produces SQL, and a rejection carries the tagged error", () => {
  assert.equal(expectRight(quote("probe")), '"probe"');
  assert.equal(literal("O'Hara"), "'O''Hara'");
  assert.match(expectRight(compileSelect(columns, base, "sqlite")).text, /^SELECT /);
  assert.equal(expectRight(compileDDL(columns)).length, 2);
  assert.equal(expectRight(compileDelete(columns, "probe", "d")).text, 'DELETE FROM "probe" WHERE "_id" = ?');
  assert.equal(expectRight(compileReplace(columns, "probe", "d", body)).values.at(-1), "d");
  assert.equal((expectRight(decode(columns, base, [row])) as unknown[]).length, 1);
  // A rejection is the structured error, so callers can branch on the reason.
  const left = expectLeft(tableFor(columns, "nope")) as CompileError;
  assert.equal(left._tag, "CompileError");
  assert.equal(left.reason, "unknown-collection");
});
