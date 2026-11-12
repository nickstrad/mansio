/**
 * The schema-dependent rules of execution (`intermediate-representation/spec/v1.md` §6), as pure functions the executor
 * composes with storage: `$ref` resolution, document-body validation, and `expect` evaluation.
 *
 * PURE: no I/O. The executor loads the current document and hands it in; these functions decide.
 */
import { Either } from "effect";
import { CompileError, type SchemaError } from "../errors.ts";
import { parseObject, storable, type Table, validate } from "../schema.ts";
import { scalar } from "../compile/select.ts";
import { PreconditionConflict } from "./errors.ts";
import type { Expect, IdRef } from "./types.ts";

/** A stored document as the decoder returns it: body fields plus the three metadata columns. */
export type Doc = Record<string, unknown> & { _id: string; _creationTime: number; _version: number };

/**
 * Replaces every `{"$ref": i}` in `value` with `ids[i]`, the `_id` insert `i` produced. Decode has
 * already checked that each `i` names an earlier insert, so the lookup always hits.
 */
export function resolve(value: unknown, ids: readonly (string | undefined)[]): unknown {
  if (Array.isArray(value)) return value.map((item) => resolve(item, ids));
  if (value !== null && typeof value === "object" && Object.hasOwn(value, "$ref")) {
    return ids[(value as { $ref: number }).$ref];
  }
  if (value !== null && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, resolve(v, ids)]));
  }
  return value;
}

export function resolveId(id: IdRef, ids: readonly (string | undefined)[]): string {
  return resolve(id, ids) as string;
}

/**
 * A document body for an insert (`partial = false`) or a patch (`partial = true`): `parseObject`, then
 * the text storage contract (ADR 0005). Metadata names (`_id`, `_version`, …) are unknown fields.
 */
export function body(
  table: Table,
  value: unknown,
  partial: boolean,
): Either.Either<Record<string, unknown>, SchemaError> {
  return Either.flatMap(parseObject(table.fields, value, partial), (parsed) => storable(parsed, ""));
}

/**
 * Evaluates `expect` against the current document (`null` when it does not exist), in the order
 * existence, version, fields. `expect.fields` names declared scalar fields only; each value is
 * validated against its node before it is compared, whether or not the document exists, so a malformed
 * precondition fails the same way every time.
 */
export function checkExpect(
  table: Table,
  tableName: string,
  id: string,
  expect: Expect,
  doc: Doc | null,
): Either.Either<void, PreconditionConflict | CompileError | SchemaError> {
  return Either.gen(function* () {
    const conflict = (check: PreconditionConflict["check"], expected: unknown, actual: unknown) =>
      Either.left(new PreconditionConflict({ table: tableName, id, check, expected, actual }));
    const fields = expect.fields ?? {};
    for (const [field, value] of Object.entries(fields)) {
      // Metadata columns are not comparable here: `version` is the precondition on `_version`.
      if (!Object.hasOwn(table.fields, field)) {
        return yield* Either.left(new CompileError({ reason: "unknown-field", field, table: tableName }));
      }
      const node = yield* scalar(table, field);
      yield* validate(node, value, field);
      yield* storable(value, field);
    }
    const mustExist = expect.exists ??
      (expect.version !== undefined || expect.fields !== undefined ? true : undefined);
    if (mustExist === true && doc === null) return yield* conflict("exists", true, false);
    if (mustExist === false && doc !== null) return yield* conflict("exists", false, true);
    if (doc === null) return;
    if (expect.version !== undefined && doc._version !== expect.version) {
      return yield* conflict("version", expect.version, doc._version);
    }
    const keys = Object.keys(fields);
    if (keys.some((key) => doc[key] !== fields[key])) {
      return yield* conflict("fields", fields, Object.fromEntries(keys.map((key) => [key, doc[key]])));
    }
  });
}
