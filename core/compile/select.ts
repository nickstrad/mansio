/**
 * Schema lookup, dialect-specific value access, and the read-plan compiler (plan item 09).
 *
 * PURE: `effect`, `../errors.ts`, `../schema.ts`, `../plan.ts` and `./dialect.ts` only. Nothing in
 * here raises. Every rejection is a `CompileError` (the plan is not compilable against this schema)
 * or a `SchemaError` (a filter value does not match its node, or a name is not an identifier) in
 * the left channel of an `Either`.
 *
 * The SQL this module emits is byte-for-byte what the pre-port compiler (the Deno platform's
 * `compiler.ts`) emitted, with one intentional exception (plan item 30): SQLite reads a
 * `json`-layout `number` field through `CAST(... AS REAL)` (see `expression`). `deno task explain`
 * regenerating `GENERATED_SQL.txt` is the safety net for the rest. The order in which it checks
 * things is also preserved, so a plan that was wrong in two ways still reports the same failure first.
 */
import { Either } from "effect";
import { CompileError, type SchemaError } from "../errors.ts";
import { identifier, type Node, type Schema, type Table, validate } from "../schema.ts";
import { normalise, type SelectPlan } from "../plan.ts";
import { type Bind, type Dialect, literal, placeholder, quote, type SQL } from "./dialect.ts";

/**
 * The metadata columns every table has. They are real columns in both physical layouts, which is
 * why they are never read out of the `doc` JSON, and they are not declarable as fields
 * (`core/schema.ts`'s `reserved-field` rule).
 */
const system: Record<string, Node> = {
  _id: { kind: "string" },
  _creationTime: { kind: "integer" },
  _version: { kind: "integer" },
};

/** Looks a collection up in the schema. */
export function tableFor(schema: Schema, name: string): Either.Either<Table, CompileError> {
  if (!Object.hasOwn(schema, name)) {
    return Either.left(new CompileError({ reason: "unknown-collection", table: name }));
  }
  return Either.right(schema[name]);
}

/** The node behind a field name: a metadata column, or a declared field. */
export function fieldNode(table: Table, name: string): Either.Either<Node, CompileError> {
  if (Object.hasOwn(system, name)) return Either.right(system[name]);
  if (!Object.hasOwn(table.fields, name)) {
    return Either.left(new CompileError({ reason: "unknown-field", field: name }));
  }
  return Either.right(table.fields[name].node);
}

/**
 * The node behind a field name, rejecting arrays. Arrays are stored as JSON text, so comparing or
 * ordering by one would compare text, not elements — the MVP refuses rather than quietly doing
 * something indefensible.
 */
export function scalar(table: Table, name: string): Either.Either<Node, CompileError> {
  return Either.flatMap(
    fieldNode(table, name),
    (node) =>
      node.kind === "array"
        ? Either.left(new CompileError({ reason: "array-predicate", field: name }))
        : Either.right(node),
  );
}

/**
 * The SQL expression that reads one field, which is where the two physical layouts and the two
 * dialects actually diverge:
 *
 * - `columns` storage (and any metadata column, in either layout) is a plain quoted column.
 * - `json` storage on SQLite is `json_extract`, whose return type follows the stored JSON text, so
 *   strings, integers and booleans need no cast. A `number` does: the writer stores
 *   `JSON.stringify(x)`, which for an integral double such as 2^53 or 1693240866440448768 is integer
 *   text (`1693240866440448800`), and `json_extract` reads integer text below 2^63 as an exact
 *   64-bit INTEGER rather than as the double it denotes. That INTEGER misses `eq x`, satisfies
 *   `gt x` against itself, and reads back beyond 2^53 where the driver refuses it. `CAST(... AS
 *   REAL)` rounds it to the nearest double, which is `x` again, so a `number` reads as REAL exactly
 *   like the `columns` layout's REAL column. Only `number` nodes are cast: integers keep their
 *   exact INTEGER (and `decode`'s safe-integer check), and nothing else is coerced.
 * - `json` storage on DuckDB is `json_extract_string`, which always yields `VARCHAR`, so numerics
 *   are cast to `BIGINT`/`DOUBLE` and booleans through `BOOLEAN` to `INTEGER` to match SQLite's
 *   `0`/`1` encoding. Without the casts, `moveNumber > 3` would compare strings.
 *
 * The same expression serves filters, ordering, `countBy` group keys and (via `core/compile/ddl.ts`)
 * index keys, so an index on a `json`-layout field matches the expression a query compares. A
 * database created before item 30 keeps its old bare `json_extract` index on a `json`-layout
 * `number` field (`CREATE INDEX IF NOT EXISTS` reuses the name), which queries no longer use:
 * results stay correct, but drop and recreate that `idx_*` to get indexed reads back.
 */
export function expression(
  table: Table,
  name: string,
  dialect: Dialect,
): Either.Either<string, CompileError | SchemaError> {
  return Either.gen(function* () {
    const node = yield* scalar(table, name);
    if (table.storage === "columns" || Object.hasOwn(system, name)) return yield* quote(name);
    const checked = yield* identifier(name);
    const path = literal(`$.${checked}`);
    if (dialect === "sqlite") {
      const extract = `json_extract("doc", ${path})`;
      return node.kind === "number" ? `CAST(${extract} AS REAL)` : extract;
    }
    const extract = `json_extract_string("doc", ${path})`;
    if (node.kind === "integer") return `CAST(${extract} AS BIGINT)`;
    if (node.kind === "number") return `CAST(${extract} AS DOUBLE)`;
    if (node.kind === "boolean") return `CAST(CAST(${extract} AS BOOLEAN) AS INTEGER)`;
    return extract;
  });
}

/**
 * A validated document value as a bind parameter. Total: the value has already been through
 * `core/schema.ts`'s `validate`, so the `string | number` cast at the end is sound.
 */
export function encode(node: Node, value: unknown): Bind {
  if (value === null) return null;
  if (node.kind === "array") return JSON.stringify(value);
  if (node.kind === "boolean") return value ? 1 : 0;
  return value as string | number;
}

/** The STRICT-mode column type for a node. `enum` and `array` both land in `TEXT`. */
export function columnType(node: Node): string {
  if (node.kind === "integer" || node.kind === "boolean") return "INTEGER";
  if (node.kind === "number") return "REAL";
  return "TEXT";
}

/**
 * Logical read plan -> parameterized SQL in the selected engine's dialect.
 *
 * The limit rule and the aggregate/order interaction come from `core/plan.ts`'s `normalise`, so
 * this function no longer carries its own copy of `1..1000`. Everything else is schema-dependent
 * and stays here. Two semantics worth naming, because they are easy to "fix" into a regression:
 *
 * - `count` counts every matching document, so it takes **no** `LIMIT`; `countBy` limits the number
 *   of groups returned, not the rows read, so it does.
 * - ordering always appends `"_id" ASC` as a tie-breaker, and places NULLs at the start of an
 *   ascending sort and the end of a descending one, so a page boundary is deterministic.
 */
export function compileSelect(
  schema: Schema,
  plan: SelectPlan,
  dialect: Dialect,
): Either.Either<SQL, CompileError | SchemaError> {
  return Either.gen(function* () {
    const table = yield* tableFor(schema, plan.table);
    const normalised = yield* normalise(plan);
    const values: Bind[] = [];
    const bind = (value: Bind): string => {
      values.push(value);
      return placeholder(dialect, values.length);
    };
    const where: string[] = [];
    for (const filter of normalised.filters) {
      const node = yield* scalar(table, filter.field);
      yield* validate(node, filter.value, filter.field);
      const expr = yield* expression(table, filter.field, dialect);
      if (filter.op === "eq" && filter.value === null) {
        where.push(`${expr} IS NULL`);
        continue;
      }
      if (filter.op !== "eq" && filter.op !== "gt") {
        return yield* Either.left(
          new CompileError({
            reason: "unsupported-operator",
            table: plan.table,
            field: filter.field,
            detail: String(filter.op),
          }),
        );
      }
      if (filter.op === "gt" && (filter.value === null || !["integer", "number"].includes(node.kind))) {
        return yield* Either.left(
          new CompileError({
            reason: "gt-requires-numeric",
            table: plan.table,
            field: filter.field,
            detail: node.kind,
          }),
        );
      }
      where.push(`${expr} ${filter.op === "eq" ? "=" : ">"} ${bind(encode(node, filter.value))}`);
    }
    const quoted = yield* quote(normalised.table);
    // The reader sees the writer's database as an attached `app` schema; the writer has no prefix.
    const target = dialect === "duckdb" ? `"app".${quoted}` : quoted;
    let projection = "*";
    if (normalised.aggregate?.kind === "count") projection = 'COUNT(*) AS "count"';
    if (normalised.aggregate?.kind === "countBy") {
      const grouped = yield* expression(table, normalised.aggregate.field, dialect);
      projection = `${grouped} AS "value", COUNT(*) AS "count"`;
    }
    let text = `SELECT ${projection} FROM ${target}`;
    if (where.length) text += ` WHERE ${where.join(" AND ")}`;
    if (normalised.aggregate?.kind === "countBy") {
      text += ' GROUP BY 1 ORDER BY "value" ASC NULLS FIRST';
    } else if (!normalised.aggregate) {
      if (normalised.order) {
        const { field, direction } = normalised.order;
        if (!["asc", "desc"].includes(direction)) {
          return yield* Either.left(
            new CompileError({
              reason: "invalid-ordering",
              table: plan.table,
              field,
              detail: String(direction),
            }),
          );
        }
        const expr = yield* expression(table, field, dialect);
        const nulls = direction === "asc" ? "FIRST" : "LAST";
        text += ` ORDER BY ${expr} ${direction.toUpperCase()} NULLS ${nulls}, "_id" ASC`;
      } else text += ' ORDER BY "_id" ASC';
    }
    // count counts all matching documents; countBy limits returned groups, not input rows.
    if (normalised.aggregate?.kind !== "count") text += ` LIMIT ${bind(normalised.limit)}`;
    return { text, values };
  });
}
