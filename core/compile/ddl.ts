/**
 * Schema descriptors -> SQLite DDL.
 *
 * PURE: nothing in here raises; a bad identifier comes back as a `SchemaError` and an unindexable
 * field as a `CompileError`.
 *
 * Every table is `STRICT`, so SQLite enforces the column types rather than coercing them, and the
 * declared constraints that STRICT cannot express are emitted as CHECKs: enums as an `IN` list,
 * booleans as `IN (0, 1)` (the encoding `core/compile/select.ts`'s `encode` produces), arrays and
 * the `json` layout's `doc` column as `json_valid`. The DDL is dialect-free: DuckDB only ever reads
 * this database through `ATTACH`, so the writer's DDL is the only DDL.
 */
import { Either } from "effect";
import { CompileError, type SchemaError } from "../errors.ts";
import type { Schema } from "../schema.ts";
import { literal, quote } from "./dialect.ts";
import { columnType, expression } from "./select.ts";

export function compileDDL(schema: Schema): Either.Either<string[], CompileError | SchemaError> {
  return Either.gen(function* () {
    const statements: string[] = [];
    for (const [name, table] of Object.entries(schema)) {
      const columns = [
        '"_id" TEXT PRIMARY KEY NOT NULL',
        '"_creationTime" INTEGER NOT NULL',
        '"_version" INTEGER NOT NULL CHECK ("_version" >= 1)',
      ];
      if (table.storage === "json") {
        columns.push('"doc" TEXT NOT NULL CHECK (json_valid("doc"))');
      } else {
        for (const [field, { node }] of Object.entries(table.fields)) {
          const column = yield* quote(field);
          let col = `${column} ${columnType(node)}${node.nullable ? "" : " NOT NULL"}`;
          if (node.kind === "enum") {
            // `v.enum` requires at least one value, so a zero-value node can only be built by
            // hand — and `IN ()` is not legal SQL. Refuse loudly rather than emit an
            // unconstrained column: `validate` rejects every value for such a node, so no write
            // could ever populate it. (An *absent* `values` is different: the pre-port code
            // crashed on it with a `TypeError`, and there is no constraint to declare, so the
            // CHECK is simply omitted for that case.)
            if (node.values !== undefined && node.values.length === 0) {
              return yield* Either.left(
                new CompileError({
                  reason: "empty-enum",
                  field,
                  table: name,
                  detail: "enum declares no values",
                }),
              );
            }
            const allowed = node.values ?? [];
            if (allowed.length) {
              col += ` CHECK (${column} IN (${allowed.map(literal).join(", ")}))`;
            }
          }
          if (node.kind === "boolean") col += ` CHECK (${column} IN (0, 1))`;
          if (node.kind === "array") col += ` CHECK (${column} IS NULL OR json_valid(${column}))`;
          columns.push(col);
        }
      }
      const target = yield* quote(name);
      statements.push(`CREATE TABLE IF NOT EXISTS ${target} (${columns.join(", ")}) STRICT`);
      // Indexes work in either layout: on a `json` table the indexed expression is the same
      // `json_extract` the WHERE clause will use, which is what makes it usable by the planner.
      for (const index of table.indexes) {
        const indexName = yield* quote(`idx_${name}_${index.name}`);
        const keys: string[] = [];
        for (const field of index.fields) keys.push(yield* expression(table, field, "sqlite"));
        statements.push(`CREATE INDEX IF NOT EXISTS ${indexName} ON ${target} (${keys.join(", ")})`);
      }
    }
    return statements;
  });
}
