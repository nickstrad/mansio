/**
 * The write path: insert, replace and delete.
 *
 * PURE: nothing in here raises. A bad collection is a `CompileError`, a body that does not match
 * the table's fields is a `SchemaError` from `core/schema.ts`'s `parseObject`.
 *
 * All three statements go to SQLite only — DuckDB is read-only in this repo — so they bind with `?`
 * unconditionally rather than going through `placeholder`. Every write is validated before it is
 * compiled: a document that cannot be parsed never reaches a statement.
 */
import { Either } from "effect";
import type { CompileError, SchemaError } from "../errors.ts";
import { parseObject, type Schema } from "../schema.ts";
import { type Bind, quote, type Row, type SQL } from "./dialect.ts";
import { encode, tableFor } from "./select.ts";

/** A fresh document: `_version` starts at 1, and `meta` carries the generated id and clock reading. */
export function compileInsert(
  schema: Schema,
  name: string,
  input: unknown,
  meta: Row,
): Either.Either<SQL, CompileError | SchemaError> {
  return Either.gen(function* () {
    const table = yield* tableFor(schema, name);
    const body = yield* parseObject(table.fields, input);
    const columns = ["_id", "_creationTime", "_version"];
    const values: Bind[] = [meta._id as string, meta._creationTime as number, 1];
    if (table.storage === "json") {
      columns.push("doc");
      values.push(JSON.stringify(body));
    } else {
      for (const [key, field] of Object.entries(table.fields)) {
        columns.push(key);
        values.push(encode(field.node, body[key]));
      }
    }
    const target = yield* quote(name);
    const quoted: string[] = [];
    for (const column of columns) quoted.push(yield* quote(column));
    const binds = values.map(() => "?").join(", ");
    return { text: `INSERT INTO ${target} (${quoted.join(", ")}) VALUES (${binds})`, values };
  });
}

/**
 * A document patch is implemented as a validated read/merge/replace within one transaction, so the
 * input here is always a whole document body. `_version` is bumped in SQL rather than read back,
 * which is what makes the optimistic-concurrency check a single statement.
 */
export function compileReplace(
  schema: Schema,
  name: string,
  id: string,
  input: unknown,
): Either.Either<SQL, CompileError | SchemaError> {
  return Either.gen(function* () {
    const table = yield* tableFor(schema, name);
    const body = yield* parseObject(table.fields, input);
    const sets: string[] = [];
    const values: Bind[] = [];
    if (table.storage === "json") {
      sets.push('"doc" = ?');
      values.push(JSON.stringify(body));
    } else {
      for (const [key, field] of Object.entries(table.fields)) {
        const column = yield* quote(key);
        sets.push(`${column} = ?`);
        values.push(encode(field.node, body[key]));
      }
    }
    sets.push('"_version" = "_version" + 1');
    values.push(id);
    const target = yield* quote(name);
    return { text: `UPDATE ${target} SET ${sets.join(", ")} WHERE "_id" = ?`, values };
  });
}

/** Deletes by id. The collection is still looked up, so deleting from a typo fails rather than no-ops. */
export function compileDelete(
  schema: Schema,
  name: string,
  id: string,
): Either.Either<SQL, CompileError | SchemaError> {
  return Either.gen(function* () {
    yield* tableFor(schema, name);
    const target = yield* quote(name);
    return { text: `DELETE FROM ${target} WHERE "_id" = ?`, values: [id] };
  });
}
