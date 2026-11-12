/**
 * Unit tests for `core/schema.ts`.
 *
 * The point of these is that validation is now *total*: every rejection is a `SchemaError` value
 * with a structured `reason`/`path`, and nothing in `core/` throws. So the assertions below check
 * the tag and the path, not a message. The legacy message rendering lives in `sdk/schema.ts` and is
 * covered in the last section.
 */
import assert from "node:assert/strict";
import { Either } from "effect";
import type { SchemaError } from "../../core/errors.ts";
import {
  defineSchema,
  defineTable,
  identifier,
  type Node,
  NUL,
  parseObject,
  storable,
  Table,
  v,
  validate,
  withStorage,
} from "../../core/schema.ts";
import * as Sdk from "../../sdk/schema.ts";

function expectLeft<A>(either: Either.Either<A, SchemaError>): SchemaError {
  if (Either.isLeft(either)) return either.left;
  return assert.fail(`expected a left, got right ${JSON.stringify(either.right)}`);
}
function expectRight<A>(either: Either.Either<A, SchemaError>): A {
  if (Either.isRight(either)) return either.right;
  return assert.fail(`expected a right, got left ${JSON.stringify(either.left)}`);
}
/** Asserts a `type-mismatch` at `path` demanding `expected`, which is the whole failure contract. */
function assertMismatch(error: SchemaError, path: string, expected: string) {
  assert.equal(error._tag, "SchemaError");
  assert.equal(error.reason, "type-mismatch");
  assert.equal(error.path, path);
  assert.equal(error.expected, expected);
}

const nullableOf = (field: { node: Node }): Node => v.nullable(field as never).node;

// ---------------------------------------------------------------------------
// Either channel contract (plan prose is error-first; Effect 3.22 is value-first)
// ---------------------------------------------------------------------------

Deno.test("Either channels: values ride right, SchemaErrors ride left (value-first order)", () => {
  // Pins the type-parameter order contract explicitly: a future edit that silently inverts
  // `Either<A, E>` must trip this assertion, not just the type-checker.
  const ok = validate(v.string().node, "x", "f");
  assert.ok(Either.isRight(ok), "success must be a Right");
  const bad = validate(v.string().node, 1, "f");
  assert.ok(Either.isLeft(bad), "rejection must be a Left");
});

// ---------------------------------------------------------------------------
// validate: one test per node kind, both nullable combinations, every failure mode
// ---------------------------------------------------------------------------

Deno.test("validate: string accepts strings, including empty, and rejects everything else", () => {
  const node = v.string().node;
  assert.equal(expectRight(validate(node, "x", "f")), "x");
  assert.equal(expectRight(validate(node, "", "f")), "");
  assertMismatch(expectLeft(validate(node, 1, "f")), "f", "string");
  assertMismatch(expectLeft(validate(node, undefined, "f")), "f", "string");
  assertMismatch(expectLeft(validate(node, null, "f")), "f", "string");
});

Deno.test("validate: nullable string accepts null and still rejects non-strings", () => {
  const node = nullableOf(v.string());
  assert.equal(expectRight(validate(node, null, "f")), null);
  assert.equal(expectRight(validate(node, "x", "f")), "x");
  assertMismatch(expectLeft(validate(node, 1, "f")), "f", "string or null");
  assertMismatch(expectLeft(validate(node, undefined, "f")), "f", "string or null");
});

Deno.test("validate: number requires a finite number", () => {
  const node = v.number().node;
  assert.equal(expectRight(validate(node, 1.5, "f")), 1.5);
  assert.equal(expectRight(validate(node, -0, "f")), -0);
  assertMismatch(expectLeft(validate(node, Number.NaN, "f")), "f", "number");
  assertMismatch(expectLeft(validate(node, Infinity, "f")), "f", "number");
  assertMismatch(expectLeft(validate(node, "1.5", "f")), "f", "number");
  assertMismatch(expectLeft(validate(node, null, "f")), "f", "number");
});

Deno.test("validate: nullable number accepts null, rejects NaN", () => {
  const node = nullableOf(v.number());
  assert.equal(expectRight(validate(node, null, "f")), null);
  assert.equal(expectRight(validate(node, 0, "f")), 0);
  assertMismatch(expectLeft(validate(node, Number.NaN, "f")), "f", "number or null");
});

Deno.test("validate: integer requires Number.isSafeInteger, not merely an integral value", () => {
  const node = v.integer().node;
  assert.equal(expectRight(validate(node, 0, "f")), 0);
  assert.equal(expectRight(validate(node, Number.MAX_SAFE_INTEGER, "f")), Number.MAX_SAFE_INTEGER);
  assertMismatch(expectLeft(validate(node, 0.5, "f")), "f", "integer");
  assertMismatch(expectLeft(validate(node, Number.MAX_SAFE_INTEGER + 1, "f")), "f", "integer");
  assertMismatch(expectLeft(validate(node, 1e21, "f")), "f", "integer");
  assertMismatch(expectLeft(validate(node, Number.NaN, "f")), "f", "integer");
  assertMismatch(expectLeft(validate(node, Infinity, "f")), "f", "integer");
  // A bigint is integral but is not a JS number, and never round-trips through JSON.
  assertMismatch(expectLeft(validate(node, 1n, "f")), "f", "integer");
});

Deno.test("validate: nullable integer accepts null, rejects unsafe integers", () => {
  const node = nullableOf(v.integer());
  assert.equal(expectRight(validate(node, null, "f")), null);
  assert.equal(expectRight(validate(node, 7, "f")), 7);
  assertMismatch(expectLeft(validate(node, 2 ** 53, "f")), "f", "integer or null");
});

Deno.test("validate: boolean accepts only true and false", () => {
  const node = v.boolean().node;
  assert.equal(expectRight(validate(node, true, "f")), true);
  assert.equal(expectRight(validate(node, false, "f")), false);
  assertMismatch(expectLeft(validate(node, 1, "f")), "f", "boolean");
  assertMismatch(expectLeft(validate(node, "true", "f")), "f", "boolean");
  assertMismatch(expectLeft(validate(node, null, "f")), "f", "boolean");
});

Deno.test("validate: nullable boolean accepts null", () => {
  const node = nullableOf(v.boolean());
  assert.equal(expectRight(validate(node, null, "f")), null);
  assert.equal(expectRight(validate(node, false, "f")), false);
  assertMismatch(expectLeft(validate(node, 0, "f")), "f", "boolean or null");
});

Deno.test("validate: enum accepts only declared members", () => {
  const node = v.enum("X", "O").node;
  assert.equal(expectRight(validate(node, "X", "turn")), "X");
  assert.equal(expectRight(validate(node, "O", "turn")), "O");
  assertMismatch(expectLeft(validate(node, "x", "turn")), "turn", "enum");
  assertMismatch(expectLeft(validate(node, "", "turn")), "turn", "enum");
  assertMismatch(expectLeft(validate(node, 0, "turn")), "turn", "enum");
  assertMismatch(expectLeft(validate(node, null, "turn")), "turn", "enum");
});

Deno.test("validate: nullable enum accepts null and rejects undeclared members", () => {
  const node = nullableOf(v.enum("X", "O"));
  assert.equal(expectRight(validate(node, null, "winner")), null);
  assert.equal(expectRight(validate(node, "O", "winner")), "O");
  assertMismatch(expectLeft(validate(node, "D", "winner")), "winner", "enum or null");
});

Deno.test("validate: array validates element-wise and accepts any length when unconstrained", () => {
  const node = v.array(v.string()).node;
  assert.deepEqual(expectRight(validate(node, [], "tags")), []);
  assert.deepEqual(expectRight(validate(node, ["a", "b"], "tags")), ["a", "b"]);
  assertMismatch(expectLeft(validate(node, "a", "tags")), "tags", "array");
  assertMismatch(expectLeft(validate(node, { 0: "a", length: 1 }, "tags")), "tags", "array");
  assertMismatch(expectLeft(validate(node, null, "tags")), "tags", "array");
});

Deno.test("validate: a fixed-length array rejects any other length", () => {
  const node = v.array(v.integer(), 2).node;
  assert.deepEqual(expectRight(validate(node, [1, 2], "pair")), [1, 2]);
  assertMismatch(expectLeft(validate(node, [1], "pair")), "pair", "array");
  assertMismatch(expectLeft(validate(node, [1, 2, 3], "pair")), "pair", "array");
  assertMismatch(expectLeft(validate(node, [], "pair")), "pair", "array");
});

Deno.test("validate: a failing array element reports its own indexed path", () => {
  const board = v.array(v.nullable(v.enum("X", "O")), 9).node;
  const nulls = Array(9).fill(null);
  assert.deepEqual(expectRight(validate(board, nulls, "board")), nulls);
  const mixed = ["X", null, "O", null, null, null, null, null, null];
  assert.deepEqual(expectRight(validate(board, mixed, "board")), mixed);
  const bad = [null, null, null, "D", null, null, null, null, null];
  assertMismatch(expectLeft(validate(board, bad, "board")), "board[3]", "enum or null");
  // The first failure decides; index 1 is reported, not index 4.
  const twoBad = [null, 1, null, null, 2, null, null, null, null];
  assertMismatch(expectLeft(validate(board, twoBad, "board")), "board[1]", "enum or null");
});

Deno.test("validate: nested arrays accumulate both indices in the path", () => {
  const node = v.array(v.array(v.integer(), 2)).node;
  assert.deepEqual(expectRight(validate(node, [[1, 2], [3, 4]], "grid")), [[1, 2], [3, 4]]);
  assertMismatch(expectLeft(validate(node, [[1, 2], [3, "4"]], "grid")), "grid[1][1]", "integer");
  assertMismatch(expectLeft(validate(node, [[1, 2], [3]], "grid")), "grid[1]", "array");
});

Deno.test("validate: a nullable array accepts null but a non-nullable element does not", () => {
  const node = nullableOf(v.array(v.string(), 2));
  assert.equal(expectRight(validate(node, null, "tags")), null);
  assert.deepEqual(expectRight(validate(node, ["a", "b"], "tags")), ["a", "b"]);
  assertMismatch(expectLeft(validate(node, ["a", null], "tags")), "tags[1]", "string");
});

Deno.test("validate: a sparse array is rejected at the first hole (strictly stronger than .map)", () => {
  // The old implementation used `value.map`, which skips holes, so they were never
  // validated and came back as holes (serializing to null). The index loop reads
  // `value[i]` as `undefined` instead, failing the item node. Pinned so a future
  // reader does not restore `.map` and silently re-accept holes.
  const node = v.array(v.string(), 3).node;
  const sparse: unknown[] = new Array(3);
  assertMismatch(expectLeft(validate(node, sparse, "board")), "board[0]", "string");
  const partlySparse: unknown[] = new Array(3);
  partlySparse[0] = "a";
  partlySparse[2] = "c";
  assertMismatch(expectLeft(validate(node, partlySparse, "board")), "board[1]", "string");
});

Deno.test("validate: an array node without an item descriptor returns a left, never raises", () => {
  // Only reachable by bypassing the `v` builders with a hand-built node; the module keeps
  // its "nothing in here raises" guarantee even for malformed descriptors.
  const node = { kind: "array" } as Node;
  const error = expectLeft(validate(node, ["a", "b"], "f"));
  assert.equal(error._tag, "SchemaError");
  assert.equal(error.reason, "type-mismatch");
  assert.equal(error.path, "f");
});

// ---------------------------------------------------------------------------
// parseObject
// ---------------------------------------------------------------------------

const fields = { name: v.string(), age: v.integer(), nickname: v.nullable(v.string()) };

Deno.test("parseObject: accepts a complete object and keys the result in field order", () => {
  const parsed = expectRight(parseObject(fields, { age: 3, nickname: null, name: "a" }));
  assert.deepEqual(parsed, { name: "a", age: 3, nickname: null });
  // JSON storage stringifies this object, so the key order is part of the stored bytes.
  assert.deepEqual(Object.keys(parsed), ["name", "age", "nickname"]);
});

Deno.test("parseObject: rejects anything that is not a plain object", () => {
  for (const input of [null, undefined, 0, "", "x", true, [], [{ name: "a" }]]) {
    const error = expectLeft(parseObject(fields, input));
    assert.equal(error.reason, "not-an-object");
    assert.equal(error.expected, "object");
  }
});

Deno.test("parseObject: rejects an unknown field before validating any declared one", () => {
  // `name` is also wrong here; the unknown key is what comes back.
  const error = expectLeft(parseObject(fields, { name: 1, age: 3, nickname: null, extra: true }));
  assert.equal(error.reason, "unknown-field");
  assert.equal(error.path, "extra");
  assert.equal(error.actual, true);
});

Deno.test("parseObject: a missing field fails its own node, since undefined is not null", () => {
  assertMismatch(expectLeft(parseObject(fields, { name: "a", age: 3 })), "nickname", "string or null");
  assertMismatch(expectLeft(parseObject(fields, { name: "a" })), "age", "integer");
});

Deno.test("parseObject: a bad field value reports that field's path", () => {
  assertMismatch(expectLeft(parseObject(fields, { name: "a", age: 0.5, nickname: null })), "age", "integer");
});

Deno.test("parseObject: partial mode skips absent fields and keeps every other rule", () => {
  assert.deepEqual(expectRight(parseObject(fields, {}, true)), {});
  assert.deepEqual(expectRight(parseObject(fields, { age: 3 }, true)), { age: 3 });
  assert.deepEqual(expectRight(parseObject(fields, { nickname: null }, true)), { nickname: null });
  assertMismatch(expectLeft(parseObject(fields, { age: 0.5 }, true)), "age", "integer");
  assert.equal(expectLeft(parseObject(fields, { extra: 1 }, true)).reason, "unknown-field");
  assert.equal(expectLeft(parseObject(fields, null, true)).reason, "not-an-object");
});

Deno.test("parseObject: an explicit undefined is a present key and still fails validation", () => {
  assertMismatch(expectLeft(parseObject(fields, { age: undefined }, true)), "age", "integer");
});

Deno.test("parseObject: an empty field map accepts only an empty object", () => {
  assert.deepEqual(expectRight(parseObject({}, {})), {});
  assert.equal(expectLeft(parseObject({}, { a: 1 })).reason, "unknown-field");
});

// ---------------------------------------------------------------------------
// identifier, defineTable, defineSchema, Table.addIndex, withStorage
// ---------------------------------------------------------------------------

Deno.test("identifier: accepts SQL-safe names and rejects everything else", () => {
  for (const name of ["a", "_a", "A1", "by_status", "_id"]) {
    assert.equal(expectRight(identifier(name)), name);
  }
  for (const name of ["", "1a", "a-b", "a b", "a.b", 'a"b', "a;b", "drop table", "é"]) {
    const error = expectLeft(identifier(name));
    assert.equal(error.reason, "invalid-identifier");
    assert.equal(error.path, name);
  }
});

Deno.test("defineTable: accepts identifier field names and rejects reserved _-prefixed ones", () => {
  const table = expectRight(defineTable({ name: v.string() }));
  assert.deepEqual(Object.keys(table.fields), ["name"]);
  assert.equal(table.storage, "columns");
  assert.deepEqual(table.indexes, []);

  for (const name of ["_id", "_version", "_"]) {
    const error = expectLeft(defineTable({ [name]: v.string() }));
    assert.equal(error.reason, "reserved-field");
    assert.equal(error.path, name);
  }
  const invalid = expectLeft(defineTable({ "not an identifier": v.string() }));
  assert.equal(invalid.reason, "invalid-identifier");
  assert.equal(invalid.path, "not an identifier");
});

Deno.test("defineSchema: rejects _-prefixed and sqlite_-prefixed table names in any case", () => {
  const games = expectRight(defineTable({ status: v.string() }));
  assert.deepEqual(Object.keys(expectRight(defineSchema({ games }))), ["games"]);

  for (const name of ["_meta", "sqlite_master", "SQLite_Master", "SQLITE_SEQUENCE"]) {
    const error = expectLeft(defineSchema({ [name]: games }));
    assert.equal(error.reason, "reserved-table");
    assert.equal(error.path, name);
  }
  // `sqlite_` is reserved only as a prefix.
  assert.equal(Object.keys(expectRight(defineSchema({ not_sqlite_master: games }))).length, 1);
  assert.equal(expectLeft(defineSchema({ "bad name": games })).reason, "invalid-identifier");
});

// ---------------------------------------------------------------------------
// The text storage contract (docs/adr/0005-text-storage-contract.md)
// ---------------------------------------------------------------------------

Deno.test("names: table, field and index names holding U+0000 fail the identifier rule", () => {
  for (const name of [`${NUL}a`, `a${NUL}b`, `a${NUL}`]) {
    assert.equal(expectLeft(identifier(name)).reason, "invalid-identifier");
    assert.equal(expectLeft(defineTable({ [name]: v.string() })).path, name);
    assert.equal(expectLeft(defineSchema({ [name]: new Table({}) })).reason, "invalid-identifier");
    assert.equal(expectLeft(new Table({ s: v.string() }).addIndex(name, ["s"])).reason, "invalid-identifier");
  }
});

Deno.test("defineTable: rejects an enum member holding U+0000, directly or inside arrays", () => {
  for (
    const field of [
      v.enum("ok", "b\u0000"),
      v.nullable(v.enum("\u0000")),
      v.array(v.array(v.enum("ok", "x\u0000"))),
    ]
  ) {
    const error = expectLeft(defineTable({ status: field }));
    assertMismatch(error, "status", "enum values without U+0000");
    assert.ok(String(error.actual).includes(NUL));
  }
  assert.ok(
    Either.isRight(defineTable({ status: v.enum("ok", "\\u0000", "\u0001") })),
    "near misses are members",
  );
});

Deno.test("validate: U+0000 is still a valid string; storage refusal is storable's job", () => {
  assert.equal(expectRight(validate({ kind: "string" }, `a${NUL}b`, "s")), `a${NUL}b`);
  assert.deepEqual(expectRight(parseObject({ s: v.string() }, { s: NUL })), { s: NUL });
});

Deno.test("storable: rejects the first string holding U+0000 at validate's path, leading, interior or trailing", () => {
  for (const text of [`${NUL}abc`, `a${NUL}bc`, `abc${NUL}`, NUL]) {
    assertMismatch(expectLeft(storable(text, "_id")), "_id", "text without U+0000");
    assert.equal(expectLeft(storable(text, "_id")).actual, text);
    assertMismatch(expectLeft(storable({ name: "ok", note: text }, "")), "note", "text without U+0000");
    assertMismatch(expectLeft(storable({ tags: ["ok", text] }, "")), "tags[1]", "text without U+0000");
    assertMismatch(
      expectLeft(storable({ grid: [[], ["ok", text]] }, "")),
      "grid[1][1]",
      "text without U+0000",
    );
    assertMismatch(expectLeft(storable(["x", text], "board")), "board[1]", "text without U+0000");
  }
  // The first NUL in walk order wins.
  assert.equal(expectLeft(storable({ a: "ok", b: [NUL], c: NUL }, "")).path, "b[0]");
});

Deno.test("storable: passes every value without U+0000 through unchanged, by reference", () => {
  const body = { name: "\\u0000", note: null, n: 0, flag: false, tags: ["", "\u0001", "é日😀"], grid: [[]] };
  assert.equal(expectRight(storable(body, "")), body);
  for (const value of [null, undefined, 0, NaN, true, "", "\u0001", [], {}]) {
    assert.equal(expectRight(storable(value, "x")), value);
  }
});

Deno.test("Table.addIndex: accepts scalar fields and leaves the receiver unchanged", () => {
  const table = new Table({ status: v.string(), turn: v.enum("X", "O") });
  const indexed = expectRight(table.addIndex("by_status", ["status"]));
  assert.deepEqual(indexed.indexes, [{ name: "by_status", fields: ["status"] }]);
  assert.deepEqual(table.indexes, []);
  const both = expectRight(indexed.addIndex("by_turn_status", ["turn", "status"]));
  assert.deepEqual(both.indexes.map((i) => i.name), ["by_status", "by_turn_status"]);
});

Deno.test("Table.addIndex: rejects a bad name, no fields, array or unknown fields, and duplicates", () => {
  const table = new Table({ status: v.string(), board: v.array(v.string(), 9) });

  assert.equal(expectLeft(table.addIndex("by status", ["status"])).reason, "invalid-identifier");

  const empty = expectLeft(table.addIndex("by_nothing", []));
  assert.equal(empty.reason, "empty-index");
  assert.equal(empty.path, "by_nothing");

  const array = expectLeft(table.addIndex("by_board", ["board"]));
  assert.equal(array.reason, "unindexable-field");
  assert.equal(array.path, "board");

  const unknown = expectLeft(table.addIndex("by_missing", ["nope" as "status"]));
  assert.equal(unknown.reason, "unindexable-field");
  assert.equal(unknown.path, "nope");

  const indexed = expectRight(table.addIndex("by_status", ["status"]));
  const duplicate = expectLeft(indexed.addIndex("by_status", ["status"]));
  assert.equal(duplicate.reason, "duplicate-index");
  assert.equal(duplicate.path, "by_status");
});

Deno.test("withStorage: reprojects every table onto one layout, preserving fields and indexes", () => {
  const games = expectRight(
    expectRight(defineTable({ status: v.string() })).addIndex("by_status", ["status"]),
  );
  const schema = expectRight(defineSchema({ games }));
  const json = withStorage(schema, "json");
  assert.equal(json.games.storage, "json");
  assert.deepEqual(json.games.indexes, games.indexes);
  assert.deepEqual(Object.keys(json.games.fields), ["status"]);
  // The input is untouched: descriptors are immutable values.
  assert.equal(schema.games.storage, "columns");
  assert.equal(withStorage(json, "columns").games.storage, "columns");
});

// ---------------------------------------------------------------------------
// sdk/schema.ts: the legacy message rendering
//
// These strings are the pre-port `Error` messages. `formatSchemaError` renders them from the
// tagged `SchemaError`; `defineTable`/`defineSchema`/`AppTable.index` still throw them because
// they run at module load, where no `Either` can go.
// ---------------------------------------------------------------------------

Deno.test("sdk: the Either forms pass values through unchanged", () => {
  assert.equal(expectRight(identifier("by_status")), "by_status");
  assert.equal(expectRight(validate(v.integer().node, 3, "cell")), 3);
  assert.deepEqual(expectRight(parseObject(fields, { name: "a", age: 1, nickname: null })), {
    name: "a",
    age: 1,
    nickname: null,
  });
  assert.deepEqual(expectRight(parseObject(fields, { age: 1 }, true)), { age: 1 });
});

Deno.test("sdk: every failure renders the pre-port message", () => {
  const rendered = (either: Either.Either<unknown, SchemaError>): string =>
    Sdk.formatSchemaError(expectLeft(either)).message;
  assert.equal(rendered(validate(v.integer().node, 0.5, "cell")), "cell: expected integer");
  assert.equal(
    rendered(validate(nullableOf(v.enum("X", "O")), "D", "board[3]")),
    "board[3]: expected enum or null",
  );
  assert.equal(rendered(parseObject(fields, { extra: 1 })), "Unknown field: extra");
  assert.equal(rendered(parseObject(fields, [])), "Expected an object");
  assert.equal(rendered(identifier("bad name")), "Invalid identifier: bad name");
  // The module-load declarations still throw the same messages.
  assert.throws(() => Sdk.defineTable({ _id: v.string() }), /^Error: Reserved field: _id$/);
  assert.throws(
    () => Sdk.defineSchema({ sqlite_master: Sdk.defineTable({ a: v.string() }) }),
    /^Error: Reserved table: sqlite_master$/,
  );
  const table = Sdk.defineTable({ status: v.string(), board: v.array(v.string(), 9) });
  assert.throws(() => table.index("by_nothing", []), /^Error: An index needs at least one field$/);
  assert.throws(() => table.index("by_board", ["board"]), /^Error: Cannot index board$/);
  assert.throws(
    () => table.index("by_status", ["status"]).index("by_status", ["status"]),
    /^Error: Duplicate index by_status$/,
  );
});

Deno.test("sdk: a module-load throw carries the structured SchemaError as its cause", () => {
  try {
    Sdk.defineTable({ board: v.array(v.string(), 9) }).index("by_board", ["board"]);
    assert.fail("expected a throw");
  } catch (error) {
    assert.ok(error instanceof Error);
    const cause = error.cause as SchemaError;
    assert.equal(cause._tag, "SchemaError");
    assert.equal(cause.reason, "unindexable-field");
    assert.equal(cause.path, "board");
  }
});

Deno.test("sdk: AppTable keeps index chaining and storage reprojection app-shaped", () => {
  const table = Sdk.defineTable({ status: v.string(), turn: v.enum("X", "O") })
    .index("by_status", ["status"])
    .index("by_turn", ["turn"]);
  assert.deepEqual(table.indexes.map((i) => i.name), ["by_status", "by_turn"]);
  assert.ok(table instanceof Sdk.AppTable);
  assert.ok(table instanceof Table);
  const json = table.storeAs("json");
  assert.ok(json instanceof Sdk.AppTable);
  assert.equal(json.storage, "json");
  assert.deepEqual(json.indexes, table.indexes);
  // withStorage goes through storeAs, so an app schema stays app-shaped.
  assert.ok(withStorage({ games: table }, "json").games instanceof Sdk.AppTable);
});
