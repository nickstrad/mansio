/**
 * Engine rows -> documents.
 *
 * PURE: nothing in here raises, including on malformed stored JSON — `JSON.parse` is wrapped in
 * `Either.try`, so a corrupt `doc` column is a `DecodeError` value rather than a `SyntaxError`
 * escaping the read path.
 *
 * Two driver facts this module exists to absorb:
 *
 * - DuckDB returns `BIGINT` as a JavaScript `bigint`, and SQLite can too. `safeNumber` narrows to
 *   `number` and rejects anything outside the safe-integer range rather than silently losing
 *   precision — `_creationTime`, `_version` and every `count` go through it.
 * - `boolean` is stored as `0`/`1` (see `core/compile/select.ts`'s `encode`), and arrives as a
 *   number, a bigint or, from DuckDB's `BOOLEAN` cast, a real boolean.
 *
 * Decoded bodies are re-validated with `core/schema.ts`'s `parseObject`: what comes out of storage
 * has to satisfy the same schema as what went in, so a drifted row fails loudly here.
 */
import { Either } from "effect";
import { type CompileError, DecodeError, type SchemaError } from "../errors.ts";
import { type Node, parseObject, type Schema } from "../schema.ts";
import type { SelectPlan } from "../plan.ts";
import type { Row } from "./dialect.ts";
import { fieldNode, tableFor } from "./select.ts";

/** Narrows a driver integer (`number`, `bigint`, or text) to a safe JS `number`. */
export function safeNumber(
  value: unknown,
  field?: string,
  table?: string,
): Either.Either<number, DecodeError> {
  const n = Number(value);
  if (!Number.isSafeInteger(n)) {
    return Either.left(new DecodeError({ reason: "unsafe-integer", field, table, actual: value }));
  }
  return Either.right(n);
}

/** One stored column back to its document value, per the field's node. */
export function decodeValue(
  node: Node,
  value: unknown,
  field?: string,
  table?: string,
): Either.Either<unknown, DecodeError> {
  if (value === null) return Either.right(null);
  if (node.kind === "integer") return safeNumber(value, field, table);
  if (node.kind === "number") return Either.right(Number(value));
  if (node.kind === "boolean") return Either.right(value === true || value === 1 || value === 1n);
  if (node.kind === "array") return parseJson(value, field, table);
  return Either.right(value);
}

/** `JSON.parse` with the failure in the error channel. Used for array columns and the `doc` column. */
function parseJson(value: unknown, field?: string, table?: string): Either.Either<unknown, DecodeError> {
  return Either.try({
    try: () => JSON.parse(value as string),
    catch: () => new DecodeError({ reason: "malformed-json", field, table, actual: value }),
  });
}

/**
 * The rows a compiled plan produced, as the value the plan asked for: a number for `count`, a
 * `{ value, count }[]` for `countBy`, a document array otherwise.
 */
export function decode(
  schema: Schema,
  plan: SelectPlan,
  rows: Row[],
): Either.Either<unknown, CompileError | DecodeError | SchemaError> {
  return Either.gen(function* () {
    const table = yield* tableFor(schema, plan.table);
    if (plan.aggregate?.kind === "count") {
      // A `count` plan always produces exactly one row; `?.` keeps an empty result set total.
      return yield* safeNumber(rows[0]?.count, "count", plan.table);
    }
    if (plan.aggregate?.kind === "countBy") {
      const field = plan.aggregate.field;
      const node = yield* fieldNode(table, field);
      const groups: { value: unknown; count: number }[] = [];
      for (const row of rows) {
        groups.push({
          value: yield* decodeValue(node, row.value, field, plan.table),
          count: yield* safeNumber(row.count, "count", plan.table),
        });
      }
      return groups;
    }
    const documents: Row[] = [];
    for (const row of rows) {
      let body: Row;
      if (table.storage === "json") {
        body = (yield* parseJson(row.doc, "doc", plan.table)) as Row;
      } else {
        body = {};
        for (const [name, field] of Object.entries(table.fields)) {
          // A column missing from the row is a decode failure of its own, not a value to
          // coerce: surfacing it here is what keeps the `missing-column` reason (and the
          // `absurd` exhaustiveness guard over `DecodeFailure`) honest.
          if (!Object.hasOwn(row, name)) {
            return yield* Either.left(
              new DecodeError({ reason: "missing-column", field: name, table: plan.table }),
            );
          }
          body[name] = yield* decodeValue(field.node, row[name], name, plan.table);
        }
      }
      documents.push({
        ...(yield* parseObject(table.fields, body)),
        _id: row._id,
        _creationTime: yield* safeNumber(row._creationTime, "_creationTime", plan.table),
        _version: yield* safeNumber(row._version, "_version", plan.table),
      });
    }
    return documents;
  });
}
