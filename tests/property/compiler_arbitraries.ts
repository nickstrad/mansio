/**
 * Generators and SQL-auditing helpers for the compiler property suites (compiler, plan, decode, SQLite
 * oracle). Everything those four files share lives here.
 *
 * Two families of things:
 *
 * - **Generators.** Schemas built from the same `core/schema.ts` `Table` the app uses, valid plans over
 *   them (fields that exist, operators the field kind supports, values that validate), plans with
 *   exactly one injected defect (paired with the error it must produce), junk (schema, plan) pairs for
 *   totality, and adversarial strings full of SQL metacharacters.
 * - **An SQL lexer and auditor.** The compiler's output is small and regular, so rather than asking
 *   "does the text contain the value?" (which a properly-escaped interpolation would pass), the audit
 *   lexes the text and requires every token to be one the compiler is entitled to emit: a
 *   double-quoted identifier matching the identifier rule, a placeholder of the right dialect, a
 *   keyword from a closed vocabulary, or a single-quoted literal of a known safe shape. Any value,
 *   identifier or comment that slips into the text unbound shows up as a token outside that set.
 *
 * Seed and run count come from the suite-wide harness in `tests/property/arbitraries.ts`, which
 * importing this module installs: `PROP_SEED=<n>` overrides the seed, test names go through its
 * `titled()`, and a failure prints the replay command. Only that module calls `fc.configureGlobal`.
 */
import fc from "fast-check";
import { Either } from "effect";
import { type Fields, type Node, type Schema, Table } from "../../core/schema.ts";
import type { Filter, SelectPlan } from "../../core/plan.ts";
import type { Dialect } from "../../core/compile/dialect.ts";
import { NUM_RUNS } from "./arbitraries.ts";

// ---------------------------------------------------------------------------
// Run configuration
// ---------------------------------------------------------------------------

/** Per-assert run count. The seed and the failure reporter are the harness's global configuration. */
export function runs<T = unknown>(numRuns = NUM_RUNS): fc.Parameters<T> {
  return { numRuns };
}

/** `JSON.stringify` that survives bigints and shows `undefined`, for failure messages. */
export function show(value: unknown): string {
  return JSON.stringify(
    value,
    (_k, v) => (typeof v === "bigint" ? `${v}n` : v === undefined ? "<undefined>" : v),
  );
}

/** The right of an `Either`, or a failure naming the left. */
export function rightOf<A, E>(either: Either.Either<A, E>): A {
  if (Either.isRight(either)) return either.right;
  throw new Error(`expected Right, got Left ${show(either.left)}`);
}

/** The left of an `Either`, or a failure naming the right. */
export function leftOf<A, E>(either: Either.Either<A, E>): E {
  if (Either.isLeft(either)) return either.left;
  throw new Error(`expected Left, got Right ${show(either.right)}`);
}

// ---------------------------------------------------------------------------
// Names
// ---------------------------------------------------------------------------

export const IDENTIFIER = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** Names that are SQL keywords or collide with names the compiler itself emits. Legal, and only safe if quoted. */
const AWKWARD_NAMES = [
  "select",
  "order",
  "group",
  "value",
  "count",
  "doc",
  "limit",
  "from",
  "where",
  "table",
  "index",
  "null",
  "key",
  "app",
];

/**
 * A declarable field name: an identifier with no `_` prefix (reserved for metadata). Callers must
 * keep a table's names unique **case-insensitively**: SQLite column names are case-insensitive, so
 * `{ a, A }` passes `defineTable` but its DDL fails with "duplicate column name".
 */
export const fieldName: fc.Arbitrary<string> = fc.oneof(
  { weight: 4, arbitrary: fc.stringMatching(/^[a-z][A-Za-z0-9_]{0,7}$/) },
  { weight: 1, arbitrary: fc.constantFrom(...AWKWARD_NAMES) },
);

/** A declarable table name: no `_` prefix and no `sqlite_` prefix in any case. */
export const tableName: fc.Arbitrary<string> = fc
  .oneof(
    { weight: 4, arbitrary: fc.stringMatching(/^[a-z][A-Za-z0-9_]{0,7}$/) },
    { weight: 1, arbitrary: fc.constantFrom(...AWKWARD_NAMES) },
  )
  .filter((name) => !name.toLowerCase().startsWith("sqlite_"));

/** Strings that are not identifiers, biased towards ones that would break out of a quoted identifier. */
export const invalidIdentifier: fc.Arbitrary<string> = fc.oneof(
  fc.constantFrom(
    "",
    "1abc",
    "a b",
    'a"b',
    "a'b",
    'x"; DROP TABLE t; --',
    'x" OR 1=1 --',
    "a-b",
    "a.b",
    "a;",
    "é",
    "$p1",
    "?",
    "a\nb",
    "--",
    "/*x*/",
    '"',
  ),
  fc.string({ unit: "binary", minLength: 1, maxLength: 8 }).filter((s) => !IDENTIFIER.test(s)),
);

/** Names that are inherited properties of a plain object: a lookup that is not `hasOwn` would "find" them. */
export const PROTOTYPE_NAMES = ["__proto__", "constructor", "toString", "hasOwnProperty", "valueOf"];

// ---------------------------------------------------------------------------
// Strings and values
// ---------------------------------------------------------------------------

/** The injection string already used by `tests/core_test.ts`. Must reach every no-leakage property. */
export const INJECTION = "O'Reilly'); DROP TABLE games; --";

/** The metacharacters the no-leakage property is about. Every adversarial string contains one. */
export const METACHARACTERS = ["'", '"', ";", "--", "/*"];

const FRAGMENTS = [
  ...METACHARACTERS,
  "*/",
  "''",
  "\\",
  "' OR '1'='1",
  '"; DROP TABLE t; --',
  "'); DROP TABLE games; --",
  "?",
  "$p1",
  " ",
  "\n",
];

/**
 * Strings built to break out of a quoted context. Each one contains at least one of
 * `METACHARACTERS`, and the exact `INJECTION` string is weighted in directly.
 */
export const adversarialString: fc.Arbitrary<string> = fc.oneof(
  { weight: 1, arbitrary: fc.constant(INJECTION) },
  {
    weight: 3,
    arbitrary: fc
      .tuple(
        fc.array(
          fc.oneof(fc.constantFrom(...FRAGMENTS), fc.string({ unit: "grapheme-ascii", maxLength: 4 })),
          {
            maxLength: 3,
          },
        ),
        fc.constantFrom(...METACHARACTERS),
        fc.array(
          fc.oneof(fc.constantFrom(...FRAGMENTS), fc.string({ unit: "grapheme-ascii", maxLength: 4 })),
          {
            maxLength: 3,
          },
        ),
      )
      .map(([before, meta, after]) => before.join("") + meta + after.join("")),
  },
);

/** Whether a string carries one of the metacharacters the no-leakage property is about. */
export function isAdversarial(s: string): boolean {
  return METACHARACTERS.some((m) => s.includes(m));
}

/**
 * Text for `string` fields. A small pool so equality filters and `countBy` groups actually collide,
 * full-Unicode text (supplementary-plane characters are where UTF-8 byte order and UTF-16 code-unit
 * order disagree), and adversarial text. `fc.string({ unit: "binary" })` never yields lone
 * surrogates, which no SQLite TEXT value can hold.
 */
export const textValue: fc.Arbitrary<string> = fc.oneof(
  { weight: 3, arbitrary: fc.constantFrom("", "a", "b", "A", "ab", "\u{1F600}", "～") },
  { weight: 2, arbitrary: fc.string({ unit: "binary", maxLength: 6 }) },
  { weight: 2, arbitrary: adversarialString },
);

/** `-0` is normalised away: JSON (the `json` layout) cannot represent it, so it cannot round-trip. */
const noNegativeZero = (x: number): number => (Object.is(x, -0) ? 0 : x);

const numberValue: fc.Arbitrary<number> = fc
  .oneof(
    { weight: 3, arbitrary: fc.integer({ min: -3, max: 3 }) },
    { weight: 2, arbitrary: fc.constantFrom(0.5, -1.5, 0.1, 1e21, -1e-7, 2 ** 53, Number.MAX_VALUE) },
    { weight: 2, arbitrary: fc.double({ noNaN: true, noDefaultInfinity: true }) },
  )
  .map(noNegativeZero);

const integerValue: fc.Arbitrary<number> = fc.oneof(
  { weight: 3, arbitrary: fc.integer({ min: -3, max: 3 }) },
  { weight: 2, arbitrary: fc.integer({ min: Number.MIN_SAFE_INTEGER, max: Number.MAX_SAFE_INTEGER }) },
  { weight: 1, arbitrary: fc.constantFrom(Number.MAX_SAFE_INTEGER, Number.MIN_SAFE_INTEGER) },
);

/** A value that validates against `node`. Nullable nodes yield `null` a quarter of the time. */
export function valueFor(node: Node): fc.Arbitrary<unknown> {
  const base = baseValue(node);
  return node.nullable
    ? fc.oneof({ weight: 1, arbitrary: fc.constant(null) }, { weight: 3, arbitrary: base })
    : base;
}

function baseValue(node: Node): fc.Arbitrary<unknown> {
  switch (node.kind) {
    case "string":
      return textValue;
    case "number":
      return numberValue;
    case "integer":
      return integerValue;
    case "boolean":
      return fc.boolean();
    case "enum":
      return fc.constantFrom(...(node.values ?? []));
    case "array": {
      const item = valueFor(node.item!);
      return node.length === undefined
        ? fc.array(item, { maxLength: 3 })
        : fc.array(item, { minLength: node.length, maxLength: node.length });
    }
  }
}

/**
 * A value that does **not** validate against `node`: wrong type, out of range, not a declared enum
 * value, or `null` when the node is not nullable. Built per kind, independently of
 * `core/schema.ts`'s `validate`.
 */
export function wrongValueFor(node: Node): fc.Arbitrary<unknown> {
  const nullUnlessNullable: unknown[] = node.nullable ? [] : [null];
  switch (node.kind) {
    case "string":
      return fc.constantFrom(1, true, [], {}, ...nullUnlessNullable);
    case "number":
      return fc.constantFrom("1", NaN, Infinity, -Infinity, true, [], ...nullUnlessNullable);
    case "integer":
      return fc.constantFrom(
        1.5,
        "1",
        NaN,
        Infinity,
        Number.MAX_SAFE_INTEGER + 1,
        2 ** 60,
        true,
        ...nullUnlessNullable,
      );
    case "boolean":
      return fc.constantFrom(0, 1, "true", "false", ...nullUnlessNullable);
    case "enum":
      return fc.oneof(
        textValue.filter((s) => !(node.values ?? []).includes(s)),
        fc.constantFrom(0, ...nullUnlessNullable),
      );
    case "array":
      return fc.constantFrom("[]", 1, {}, ...nullUnlessNullable);
  }
}

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

const enumValues: fc.Arbitrary<string[]> = fc.uniqueArray(
  fc.oneof(fc.constantFrom("X", "O", "a", "it's", ""), adversarialString),
  { minLength: 1, maxLength: 4 },
);

/** A node as the `v` builders would produce it: every kind, nullable, arrays nested to `depth`. */
export function nodeArb(depth = 2): fc.Arbitrary<Node> {
  const scalar: fc.Arbitrary<Node> = fc.oneof(
    fc.constant<Node>({ kind: "string" }),
    fc.constant<Node>({ kind: "number" }),
    fc.constant<Node>({ kind: "integer" }),
    fc.constant<Node>({ kind: "boolean" }),
    enumValues.map((values): Node => ({ kind: "enum", values })),
  );
  const shape = depth <= 0 ? scalar : fc.oneof(
    { weight: 4, arbitrary: scalar },
    {
      weight: 1,
      arbitrary: fc
        .tuple(nodeArb(depth - 1), fc.option(fc.integer({ min: 0, max: 3 }), { nil: undefined }))
        .map((
          [item, length],
        ): Node => (length === undefined ? { kind: "array", item } : { kind: "array", item, length })),
    },
  );
  return fc.tuple(shape, fc.boolean()).map((
    [node, nullable],
  ) => (nullable ? { ...node, nullable: true } : node));
}

/** The metadata columns, mirrored from `core/compile/select.ts` (not exported there). */
export const SYSTEM: Record<string, Node> = {
  _id: { kind: "string" },
  _creationTime: { kind: "integer" },
  _version: { kind: "integer" },
};

export const isNumeric = (node: Node): boolean => node.kind === "integer" || node.kind === "number";

/** Field names unique case-insensitively, each with a node. */
export const fieldsArb: fc.Arbitrary<Fields> = fc
  .uniqueArray(fc.tuple(fieldName, nodeArb()), { maxLength: 5, selector: ([name]) => name.toLowerCase() })
  .map((entries) => Object.fromEntries(entries.map(([name, node]) => [name, { node }])));

// Built once: `fc.stringMatching` compiles its regex into an arbitrary, which is far too slow to
// repeat inside every `chain`.
const indexName = fc.stringMatching(/^by_[a-z0-9]{1,4}$/);

/** A table in either layout, with up to two indexes over its scalar fields. */
export function tableArb(
  storage?: "columns" | "json",
  fields: fc.Arbitrary<Fields> = fieldsArb,
): fc.Arbitrary<Table> {
  return fc
    .record({
      fields,
      storage: storage ? fc.constant(storage) : fc.constantFrom("columns" as const, "json" as const),
    })
    .chain(({ fields, storage }) => {
      const scalars = Object.keys(fields).filter((k) => fields[k].node.kind !== "array");
      const indexes = scalars.length
        ? fc.uniqueArray(
          fc.tuple(indexName, fc.shuffledSubarray(scalars, { minLength: 1 })),
          { maxLength: 2, selector: ([name]) => name },
        )
        : fc.constant([] as [string, string[]][]);
      return indexes.map((ix) => new Table(fields, storage, ix.map(([name, fields]) => ({ name, fields }))));
    });
}

/** One to three tables, names unique case-insensitively. */
export const schemaArb: fc.Arbitrary<Schema> = fc
  .uniqueArray(fc.tuple(tableName, tableArb()), {
    minLength: 1,
    maxLength: 3,
    selector: ([name]) => name.toLowerCase(),
  })
  .map((entries) => Object.fromEntries(entries));

/** A document body that validates against the table's fields. */
export function bodyArb(table: Table): fc.Arbitrary<Record<string, unknown>> {
  return fc.record(
    Object.fromEntries(Object.entries(table.fields).map(([name, field]) => [name, valueFor(field.node)])),
  );
}

/** Every queryable `(name, node)` of a table: the metadata columns, then the declared fields. */
export function nodesOf(table: Table): [string, Node][] {
  return [
    ...Object.entries(SYSTEM),
    ...Object.entries(table.fields).map(([name, field]): [string, Node] => [name, field.node]),
  ];
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

/** A stored document: body fields plus the metadata columns. */
export type Doc = Record<string, unknown> & { _id: string; _creationTime: number; _version: number };

/**
 * A plan that must compile against `schema[name]`: filters on scalar fields (metadata included) with
 * `eq` on any kind and `gt` only on numeric kinds, values that validate, an optional ordering and
 * aggregate, and a legal limit. When `docs` is given, filter values are drawn from it half the time so
 * filters actually select something.
 *
 * Aggregate plans get an ordering too, sometimes: `normalise` must drop it, and generating it is how
 * the properties check that it does.
 */
export function validPlanArb(
  schema: Schema,
  name: string,
  docs: readonly Doc[] = [],
): fc.Arbitrary<SelectPlan> {
  const table = schema[name];
  const scalars = nodesOf(table).filter(([, node]) => node.kind !== "array");
  const filter: fc.Arbitrary<Filter> = fc.constantFrom(...scalars).chain(([field, node]) => {
    const seen = docs.map((doc) => doc[field]);
    const present = seen.filter((v) => v !== null);
    const eqValue = seen.length ? fc.oneof(valueFor(node), fc.constantFrom(...seen)) : valueFor(node);
    const eq = eqValue.map((value): Filter => ({ field, op: "eq", value }));
    if (!isNumeric(node)) return eq;
    const bound = valueFor({ ...node, nullable: false });
    const gtValue = present.length ? fc.oneof(bound, fc.constantFrom(...present)) : bound;
    return fc.oneof(eq, gtValue.map((value): Filter => ({ field, op: "gt", value })));
  });
  const fields = scalars.map(([field]) => field);
  return fc
    .record({
      filters: fc.array(filter, { maxLength: 3 }),
      order: fc.option(
        fc.record({
          field: fc.constantFrom(...fields),
          direction: fc.constantFrom("asc" as const, "desc" as const),
        }),
        { nil: undefined },
      ),
      limit: fc.oneof(
        fc.integer({ min: 1, max: 4 }),
        fc.integer({ min: 1, max: 1000 }),
        fc.constantFrom(1, 1000),
      ),
      aggregate: fc.option(
        fc.oneof(
          fc.constant({ kind: "count" as const }),
          fc.constantFrom(...fields).map((field) => ({ kind: "countBy" as const, field })),
        ),
        { nil: undefined, freq: 2 },
      ),
    })
    .map(({ filters, order, limit, aggregate }) => {
      const plan: SelectPlan = { kind: "select", table: name, filters, limit };
      if (order) plan.order = order;
      if (aggregate) plan.aggregate = aggregate;
      return plan;
    });
}

/** A schema and a plan that must compile against it. */
export const schemaAndPlan: fc.Arbitrary<{ schema: Schema; plan: SelectPlan }> = schemaArb.chain((schema) =>
  fc.constantFrom(...Object.keys(schema)).chain((name) =>
    validPlanArb(schema, name).map((plan) => ({ schema, plan }))
  )
);

/** The error a defective plan must produce. */
export type Expected = { tag: "CompileError" | "SchemaError"; reason: string; defect: string };

const badLimit: fc.Arbitrary<number> = fc.oneof(
  fc.integer({ max: 0 }),
  fc.integer({ min: 1001 }),
  fc.double({ min: 1, max: 1000, noNaN: true }).filter((x) => !Number.isInteger(x)),
  fc.constantFrom(0, -0, -1, 1001, 0.5, 999.5, NaN, Infinity, -Infinity, 1e9),
);

const badOps = fc.constantFrom(
  "lt",
  "gte",
  "ne",
  "neq",
  "like",
  "in",
  "",
  "=",
  "EQ",
  "eq ",
  "eq; DROP TABLE t; --",
);
const badDirections = fc.constantFrom("ASC", "Desc", "up", "", "asc; DROP TABLE t; --", "random()", "asc ");

/**
 * A plan with exactly one defect, paired with the error it must produce. The defect is injected into
 * an otherwise valid plan (at a random filter position, when it is a filter), so the expected error is
 * unambiguous whatever order the compiler checks things in. Ordering defects are only injected into
 * non-aggregate plans, because an aggregate's ordering is dropped by `normalise` before anything
 * looks at it.
 */
export function invalidPlanArb(
  schema: Schema,
  name: string,
): fc.Arbitrary<{ plan: SelectPlan; expected: Expected }> {
  const table = schema[name];
  const entries = nodesOf(table);
  const scalars = entries.filter(([, node]) => node.kind !== "array");
  const arrays = entries.filter(([, node]) => node.kind === "array");
  const nonNumeric = scalars.filter(([, node]) => !isNumeric(node));
  const nullableNumeric = scalars.filter(([, node]) => isNumeric(node) && node.nullable);
  const unknownField = fc
    .oneof(fieldName, fc.constantFrom(...PROTOTYPE_NAMES, "_ID", "_other", "doc", "_id ", "ID"))
    .filter((f) => !Object.hasOwn(SYSTEM, f) && !Object.hasOwn(table.fields, f));
  const unknownTable = fc
    .oneof(
      tableName,
      fc.constantFrom(...PROTOTYPE_NAMES, "sqlite_master", "_platform_schema", name.toUpperCase()),
    )
    .filter((t) => !Object.hasOwn(schema, t));

  type Defect = (plan: SelectPlan, at: number) => { plan: SelectPlan; expected: Expected };
  const withFilter = (plan: SelectPlan, at: number, filter: Filter): SelectPlan => {
    const filters = [...plan.filters];
    filters.splice(at % (filters.length + 1), 0, filter);
    return { ...plan, filters };
  };
  const unordered = (plan: SelectPlan): SelectPlan => {
    const { aggregate: _aggregate, ...rest } = plan;
    return rest;
  };
  const compile = (reason: string, defect: string): Expected => ({ tag: "CompileError", reason, defect });

  const defects: fc.Arbitrary<Defect>[] = [
    unknownTable.map((t): Defect => (plan) => ({
      plan: { ...plan, table: t },
      expected: compile("unknown-collection", `table ${JSON.stringify(t)}`),
    })),
    badLimit.map((limit): Defect => (plan) => ({
      plan: { ...plan, limit },
      expected: compile("invalid-limit", `limit ${limit}`),
    })),
    fc.tuple(unknownField, fc.constantFrom("eq" as const, "gt" as const), fc.constantFrom(1, "x", null)).map(
      ([field, op, value]): Defect => (plan, at) => ({
        plan: withFilter(plan, at, { field, op, value }),
        expected: compile("unknown-field", `filter on ${JSON.stringify(field)}`),
      }),
    ),
    unknownField.map((field): Defect => (plan) => ({
      plan: { ...plan, aggregate: { kind: "countBy", field } },
      expected: compile("unknown-field", `countBy ${JSON.stringify(field)}`),
    })),
    unknownField.map((field): Defect => (plan) => ({
      plan: { ...unordered(plan), order: { field, direction: "asc" } },
      expected: compile("unknown-field", `order by ${JSON.stringify(field)}`),
    })),
    fc.tuple(fc.constantFrom(...scalars), badOps).chain(([[field, node], op]) =>
      valueFor({ ...node, nullable: false }).map((value): Defect => (plan, at) => ({
        plan: withFilter(plan, at, { field, op: op as Filter["op"], value }),
        expected: compile("unsupported-operator", `op ${JSON.stringify(op)} on ${field}`),
      }))
    ),
    fc.tuple(fc.constantFrom(...scalars), badDirections).map(([[field], direction]): Defect => (plan) => ({
      plan: { ...unordered(plan), order: { field, direction: direction as "asc" } },
      expected: compile("invalid-ordering", `direction ${JSON.stringify(direction)}`),
    })),
    fc.constantFrom(...scalars).chain(([field, node]) =>
      wrongValueFor(node).map((value): Defect => (plan, at) => ({
        plan: withFilter(plan, at, { field, op: "eq", value }),
        expected: { tag: "SchemaError", reason: "type-mismatch", defect: `${field} = ${String(value)}` },
      }))
    ),
    // `_id` is always a non-numeric scalar, so this pool is never empty.
    fc.constantFrom(...nonNumeric).chain(([field, node]) =>
      valueFor(node).map((value): Defect => (plan, at) => ({
        plan: withFilter(plan, at, { field, op: "gt", value }),
        expected: compile("gt-requires-numeric", `gt on ${node.kind} ${field}`),
      }))
    ),
  ];
  if (nullableNumeric.length) {
    defects.push(
      fc.constantFrom(...nullableNumeric).map(([field]): Defect => (plan, at) => ({
        plan: withFilter(plan, at, { field, op: "gt", value: null }),
        expected: compile("gt-requires-numeric", `gt null on ${field}`),
      })),
    );
  }
  if (arrays.length) {
    const arrayField = fc.constantFrom(...arrays);
    defects.push(
      arrayField.chain(([field, node]) =>
        fc.tuple(valueFor(node), fc.constantFrom("eq" as const, "gt" as const)).map(
          ([value, op]): Defect => (plan, at) => ({
            plan: withFilter(plan, at, { field, op, value }),
            expected: compile("array-predicate", `filter on array ${field}`),
          }),
        )
      ),
      arrayField.map(([field]): Defect => (plan) => ({
        plan: { ...unordered(plan), order: { field, direction: "desc" } },
        expected: compile("array-predicate", `order by array ${field}`),
      })),
      arrayField.map(([field]): Defect => (plan) => ({
        plan: { ...plan, aggregate: { kind: "countBy", field } },
        expected: compile("array-predicate", `countBy array ${field}`),
      })),
    );
  }
  return fc
    .tuple(validPlanArb(schema, name), fc.oneof(...defects), fc.nat())
    .map(([plan, defect, at]) => defect(plan, at));
}

/** A schema and a plan with one defect against it. */
export const schemaAndInvalidPlan: fc.Arbitrary<{ schema: Schema; plan: SelectPlan; expected: Expected }> =
  schemaArb
    .chain((schema) =>
      fc.constantFrom(...Object.keys(schema)).chain((name) =>
        invalidPlanArb(schema, name).map(({ plan, expected }) => ({ schema, plan, expected }))
      )
    );

// ---------------------------------------------------------------------------
// Junk, for totality
// ---------------------------------------------------------------------------

/** Any string a caller could put where a name goes. */
export const anyName: fc.Arbitrary<string> = fc.oneof(
  fieldName,
  invalidIdentifier,
  fc.constantFrom(...PROTOTYPE_NAMES, "_id", "_version", "doc", "sqlite_master"),
  fc.string({ maxLength: 6 }),
);

/** Nodes that stay inside the `Node` type but ignore every builder invariant (empty enums, itemless arrays). */
export function junkNode(depth = 2): fc.Arbitrary<Node> {
  return fc.record(
    {
      kind: fc.constantFrom("string", "number", "integer", "boolean", "enum", "array") as fc.Arbitrary<
        Node["kind"]
      >,
      nullable: fc.boolean(),
      values: fc.array(fc.oneof(fc.string({ maxLength: 4 }), adversarialString), { maxLength: 3 }),
      item: depth <= 0 ? fc.constant(undefined) : fc.option(junkNode(depth - 1), { nil: undefined }),
      length: fc.oneof(fc.integer({ min: -1, max: 3 }), fc.constantFrom(0.5, NaN)),
    },
    { requiredKeys: ["kind"] },
  );
}

const junkTable: fc.Arbitrary<Table> = fc
  .record({
    fields: fc.dictionary(anyName, junkNode().map((node) => ({ node })), { maxKeys: 4 }),
    storage: fc.constantFrom("columns" as const, "json" as const),
    indexes: fc.array(fc.record({ name: anyName, fields: fc.array(anyName, { maxLength: 2 }) }), {
      maxLength: 2,
    }),
  })
  .map(({ fields, storage, indexes }) => new Table(fields, storage, indexes));

/** A schema whose names, nodes and indexes may all be illegal. */
export const junkSchema: fc.Arbitrary<Schema> = fc.dictionary(anyName, junkTable, { minKeys: 1, maxKeys: 3 });

/**
 * A plan that respects `SelectPlan`'s shape (so `filters` is an array and `limit` a number) but
 * nothing else: names from anywhere, any operator string, any value, any direction, any number.
 */
export function junkPlan(schema: Schema): fc.Arbitrary<SelectPlan> {
  const tables = Object.keys(schema);
  const fields = tables.flatMap((t) => Object.keys(schema[t]?.fields ?? {}));
  const name = fields.length
    ? fc.oneof({ weight: 3, arbitrary: fc.constantFrom(...fields, "_id", "_creationTime") }, {
      weight: 1,
      arbitrary: anyName,
    })
    : anyName;
  return fc
    .record({
      // Weighted towards names that exist and legal limits, so junk reaches past the first two checks.
      table: tables.length
        ? fc.oneof({ weight: 4, arbitrary: fc.constantFrom(...tables) }, { weight: 1, arbitrary: anyName })
        : anyName,
      filters: fc.array(
        fc.record({
          field: name,
          op: fc.oneof(fc.constantFrom("eq", "gt"), fc.string({ maxLength: 4 })) as fc.Arbitrary<
            Filter["op"]
          >,
          value: fc.oneof(fc.anything(), adversarialString, integerValue),
        }),
        { maxLength: 3 },
      ),
      order: fc.option(
        fc.record({
          field: name,
          direction: fc.oneof(fc.constantFrom("asc", "desc"), fc.string({ maxLength: 4 })) as fc.Arbitrary<
            "asc"
          >,
        }),
        { nil: undefined },
      ),
      limit: fc.oneof(
        { weight: 4, arbitrary: fc.integer({ min: 1, max: 1000 }) },
        { weight: 1, arbitrary: fc.integer() },
        { weight: 1, arbitrary: fc.double() },
      ),
      aggregate: fc.option(
        fc.oneof(
          fc.constant({ kind: "count" as const }),
          name.map((field) => ({ kind: "countBy" as const, field })),
        ),
        { nil: undefined },
      ),
    })
    .map(({ table, filters, order, limit, aggregate }) => {
      const plan: SelectPlan = { kind: "select", table, filters, limit };
      if (order) plan.order = order;
      if (aggregate) plan.aggregate = aggregate;
      return plan;
    });
}

// ---------------------------------------------------------------------------
// SQL lexer and auditor
// ---------------------------------------------------------------------------

export type Token =
  | { kind: "ident"; value: string } // "…"
  | { kind: "string"; value: string } // '…', with '' unescaped
  | { kind: "param"; value: string } // ? or the N of $pN
  | { kind: "word"; value: string } // bare keyword or function name
  | { kind: "number"; value: string }
  | { kind: "punct"; value: string }
  | { kind: "bad"; value: string }; // anything the compiler never emits: ; - / newline, an unterminated quote

/**
 * Splits compiled SQL into tokens. The compiler emits single spaces between tokens and never escapes
 * a double quote inside an identifier (it refuses such names instead), so an identifier ends at the
 * next `"`; a smuggled `""` therefore lexes as an empty identifier, which the audit rejects.
 */
export function lex(text: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < text.length) {
    const c = text[i];
    if (c === " ") {
      i++;
    } else if (c === '"') {
      const end = text.indexOf('"', i + 1);
      if (end < 0) return [...tokens, { kind: "bad", value: text.slice(i) }];
      tokens.push({ kind: "ident", value: text.slice(i + 1, end) });
      i = end + 1;
    } else if (c === "'") {
      let j = i + 1;
      let value = "";
      for (;;) {
        if (j >= text.length) return [...tokens, { kind: "bad", value: text.slice(i) }];
        if (text[j] === "'") {
          if (text[j + 1] === "'") {
            value += "'";
            j += 2;
            continue;
          }
          break;
        }
        value += text[j++];
      }
      tokens.push({ kind: "string", value });
      i = j + 1;
    } else if (c === "?") {
      tokens.push({ kind: "param", value: "?" });
      i++;
    } else if (c === "$") {
      const match = /^\$p(\d+)/.exec(text.slice(i));
      if (!match) return [...tokens, { kind: "bad", value: text.slice(i) }];
      tokens.push({ kind: "param", value: match[1] });
      i += match[0].length;
    } else if (/[A-Za-z_]/.test(c)) {
      const match = /^[A-Za-z_][A-Za-z0-9_]*/.exec(text.slice(i))!;
      tokens.push({ kind: "word", value: match[0] });
      i += match[0].length;
    } else if (/[0-9]/.test(c)) {
      const match = /^[0-9]+/.exec(text.slice(i))!;
      tokens.push({ kind: "number", value: match[0] });
      i += match[0].length;
    } else if ("(),*=>.".includes(c)) {
      tokens.push({ kind: "punct", value: c });
      i++;
    } else {
      tokens.push({ kind: "bad", value: c });
      i++;
    }
  }
  return tokens;
}

const JSON_PATH = /^\$\.[A-Za-z_][A-Za-z0-9_]*$/;

const SELECT_WORDS = new Set([
  "SELECT",
  "FROM",
  "WHERE",
  "AND",
  "IS",
  "NULL",
  "ORDER",
  "BY",
  "ASC",
  "DESC",
  "NULLS",
  "FIRST",
  "LAST",
  "LIMIT",
  "GROUP",
  "COUNT",
  "AS",
  "CAST",
  "BIGINT",
  "DOUBLE",
  "REAL",
  "BOOLEAN",
  "INTEGER",
  "json_extract",
  "json_extract_string",
]);

const DDL_WORDS = new Set([
  "CREATE",
  "TABLE",
  "IF",
  "NOT",
  "EXISTS",
  "TEXT",
  "PRIMARY",
  "KEY",
  "NULL",
  "INTEGER",
  "REAL",
  "CHECK",
  "STRICT",
  "IN",
  "OR",
  "IS",
  "INDEX",
  "ON",
  "CAST",
  "AS",
  "json_valid",
  "json_extract",
]);

const WRITE_WORDS = new Set(["INSERT", "INTO", "VALUES", "UPDATE", "SET", "WHERE", "DELETE", "FROM"]);

/** Problems common to every statement kind: identifiers, placeholders, stray characters. */
function auditCommon(token: Token, dialect: Dialect | "none"): string | undefined {
  if (token.kind === "ident" && !IDENTIFIER.test(token.value)) {
    return `bad identifier ${JSON.stringify(token.value)}`;
  }
  if (token.kind === "bad") return `stray ${JSON.stringify(token.value)}`;
  if (token.kind === "param") {
    if (dialect === "none") return `placeholder ${token.value}`;
    if ((dialect === "sqlite") !== (token.value === "?")) return `${dialect} placeholder ${token.value}`;
  }
  return undefined;
}

/**
 * Every problem with a compiled select: any token outside the closed set a select may contain. The
 * only literal a select may carry is a `'$.<identifier>'` JSON path; the only number is the `1` of
 * `GROUP BY 1`. A filter value, a limit or an identifier interpolated into the text fails here.
 */
export function auditSelect(text: string, dialect: Dialect): string[] {
  const tokens = lex(text);
  const problems: string[] = [];
  tokens.forEach((token, i) => {
    const common = auditCommon(token, dialect);
    if (common) problems.push(common);
    if (token.kind === "string" && !JSON_PATH.test(token.value)) {
      problems.push(`literal ${JSON.stringify(token.value)}`);
    }
    if (token.kind === "word" && !SELECT_WORDS.has(token.value)) problems.push(`word ${token.value}`);
    if (
      token.kind === "number" &&
      !(token.value === "1" && tokens[i - 1]?.value === "BY" && tokens[i - 2]?.value === "GROUP")
    ) problems.push(`number ${token.value}`);
  });
  return problems;
}

/** Every problem with a DDL statement. Literals may only be JSON paths or the schema's enum values. */
export function auditDDL(text: string, enumValues: ReadonlySet<string>): string[] {
  const problems: string[] = [];
  for (const token of lex(text)) {
    const common = auditCommon(token, "none");
    if (common) problems.push(common);
    if (token.kind === "string" && !JSON_PATH.test(token.value) && !enumValues.has(token.value)) {
      problems.push(`literal ${JSON.stringify(token.value)}`);
    }
    if (token.kind === "word" && !DDL_WORDS.has(token.value)) problems.push(`word ${token.value}`);
    if (token.kind === "number" && token.value !== "0" && token.value !== "1") {
      problems.push(`number ${token.value}`);
    }
  }
  return problems;
}

/** Every problem with an insert, replace or delete: no literals and no numbers at all, only `?`. */
export function auditWrite(text: string): string[] {
  const problems: string[] = [];
  for (const token of lex(text)) {
    // `"_version" = "_version" + 1` is the one arithmetic a write may carry.
    if (token.kind === "bad" && token.value === "+") continue;
    const common = auditCommon(token, "sqlite");
    if (common) problems.push(common);
    if (token.kind === "string") problems.push(`literal ${JSON.stringify(token.value)}`);
    if (token.kind === "word" && !WRITE_WORDS.has(token.value)) problems.push(`word ${token.value}`);
    if (token.kind === "number" && token.value !== "1") problems.push(`number ${token.value}`);
  }
  return problems;
}

/** The placeholders of a statement, in order of appearance. */
export function placeholders(text: string): string[] {
  return lex(text).flatMap((t) => (t.kind === "param" ? [t.value] : []));
}

/** Every enum value declared anywhere in a schema, at any array depth. */
export function enumValuesOf(schema: Schema): Set<string> {
  const out = new Set<string>();
  const walk = (node: Node | undefined): void => {
    if (!node) return;
    for (const v of node.values ?? []) out.add(v);
    walk(node.item);
  };
  for (const table of Object.values(schema)) {
    for (const field of Object.values(table.fields)) walk(field.node);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Dialect skeleton
// ---------------------------------------------------------------------------

const DUCK_FIELD =
  /CAST\(CAST\(json_extract_string\("doc", '\$\.(\w+)'\) AS BOOLEAN\) AS INTEGER\)|CAST\(json_extract_string\("doc", '\$\.(\w+)'\) AS (BIGINT|DOUBLE)\)|json_extract_string\("doc", '\$\.(\w+)'\)/g;
const SQLITE_FIELD = /CAST\(json_extract\("doc", '\$\.(\w+)'\) AS REAL\)|json_extract\("doc", '\$\.(\w+)'\)/g;

/** The DuckDB cast a `json`-layout field read needs so it compares like SQLite's `json_extract` does. */
function expectedCast(node: Node | undefined): string {
  if (node?.kind === "integer") return "BIGINT";
  if (node?.kind === "number") return "DOUBLE";
  if (node?.kind === "boolean") return "BOOLEAN";
  return "none";
}

/** SQLite casts only a `json`-layout `number` read, to REAL; every other kind is bare. */
function expectedSqliteCast(node: Node | undefined): string {
  return node?.kind === "number" ? "REAL" : "none";
}

/**
 * A dialect-neutral token skeleton of a compiled select. Placeholders become `?`, DuckDB's `"app".`
 * schema prefix goes, and each dialect's JSON field read becomes `⟨field⟩`. A read whose cast does
 * not fit the field's kind (a DuckDB `moveNumber > 3` that would compare strings, or a SQLite
 * `number` read missing its REAL cast) becomes `⟨field:<cast>!⟩`, so the two skeletons cannot match.
 */
export function skeleton(text: string, dialect: Dialect, table: Table): string[] {
  let t = text;
  if (dialect === "duckdb") {
    t = t.replaceAll('"app".', "").replace(/\$p\d+/g, "?").replace(
      DUCK_FIELD,
      (_m, boolField?: string, castField?: string, cast?: string, plainField?: string) => {
        const field = (boolField ?? castField ?? plainField)!;
        const actual = boolField ? "BOOLEAN" : cast ?? "none";
        const ok = actual === expectedCast(table.fields[field]?.node);
        return ok ? `⟨${field}⟩` : `⟨${field}:${actual}!⟩`;
      },
    );
  } else {
    t = t.replace(SQLITE_FIELD, (_m, castField?: string, plainField?: string) => {
      const field = (castField ?? plainField)!;
      const actual = castField ? "REAL" : "none";
      return actual === expectedSqliteCast(table.fields[field]?.node)
        ? `⟨${field}⟩`
        : `⟨${field}:${actual}!⟩`;
    });
  }
  return t.match(/"[^"]*"|'(?:[^']|'')*'|⟨[^⟩]*⟩|\?|\w+|\S/g) ?? [];
}
