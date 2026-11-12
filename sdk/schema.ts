/**
 * App-facing schema surface.
 *
 * The descriptors and the validation logic live in `core/schema.ts`, where they are total:
 * every rejection comes back as a `SchemaError` in an `Either`. This module re-exports the core
 * types and builders, plus `formatSchemaError`, which renders a `SchemaError` back into the exact
 * `Error` message the pre-port code produced, for the callers that still need an `Error`.
 *
 * `AppTable.index` and `defineTable`/`defineSchema` still throw: they run at module load in
 * `examples/tic-tac-toe/schema.ts`, which has nowhere to put an `Either`. Item 26 deleted the throwing wrappers
 * (plan items 04 and 09); every other caller is on the `Either` forms in `core/schema.ts` now.
 */
import { Either, Function as EffectFunction } from "effect";
import type { SchemaError } from "../core/errors.ts";
import * as Core from "../core/schema.ts";
import { type Fields, type Schema, type Storage, Table } from "../core/schema.ts";

export { Table, v, withStorage } from "../core/schema.ts";
export type { Body, Document, Field, Fields, Index, Infer, Node, Schema, Storage } from "../core/schema.ts";

/**
 * Renders a structured `SchemaError` into the pre-port message, verbatim —
 * `tests/unit/schema_test.ts` pins these strings. The switch is total over `SchemaFailure`:
 * adding a reason without a case here is a compile error.
 */
export function formatSchemaError(error: SchemaError): Error {
  const message = ((): string => {
    switch (error.reason) {
      case "type-mismatch":
        return `${error.path}: expected ${error.expected ?? "a valid value"}`;
      case "not-an-object":
        return "Expected an object";
      case "unknown-field":
        return `Unknown field: ${error.path}`;
      case "invalid-identifier":
        return `Invalid identifier: ${error.path}`;
      case "reserved-field":
        return `Reserved field: ${error.path}`;
      case "reserved-table":
        return `Reserved table: ${error.path}`;
      case "empty-index":
        return "An index needs at least one field";
      case "unindexable-field":
        return `Cannot index ${error.path}`;
      case "duplicate-index":
        return `Duplicate index ${error.path}`;
      default:
        return EffectFunction.absurd<string>(error.reason);
    }
  })();
  return new Error(message, { cause: error });
}

/**
 * Unwrap for the module-load declarations below: they run where no `Either` can go, so a
 * rejection throws the rendered legacy `Error` instead of returning a left.
 */
const unwrap = <A>(either: Either.Either<A, SchemaError>): A => {
  if (Either.isLeft(either)) throw formatSchemaError(either.left);
  return either.right;
};

/**
 * A `Table` whose `index` throws, for schemas declared at module load. Identical data to
 * `core/schema.ts`'s `Table`, so platform code that only reads `fields`/`storage`/`indexes` is
 * unaffected by which of the two it is handed.
 */
export class AppTable<F extends Fields = Fields> extends Table<F> {
  index(name: string, fields: readonly (keyof F & string)[]): AppTable<F> {
    const next = unwrap(this.addIndex(name, fields));
    return new AppTable(next.fields, next.storage, next.indexes);
  }

  override storeAs(storage: Storage): AppTable<F> {
    return new AppTable(this.fields, storage, this.indexes);
  }
}

/** Declares a table, throwing on a bad or reserved field name. */
export function defineTable<F extends Fields>(fields: F): AppTable<F> {
  return unwrap(
    Either.map(
      Core.defineTable(fields),
      (table) => new AppTable(table.fields, table.storage, table.indexes),
    ),
  );
}

/** Validates a schema's table names, throwing on a bad or reserved one, and returns it unchanged. */
export function defineSchema<S extends Schema>(schema: S): S {
  return unwrap(Core.defineSchema(schema));
}
