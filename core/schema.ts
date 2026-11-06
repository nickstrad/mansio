/**
 * Schema descriptors and **total** validation (plan item 04).
 *
 * This module is PURE, per the `core/` rule in `docs/adr/0001-effect-port.md`: it imports `effect`
 * and `./errors.ts`, and nothing else — no `eventlog/`, no `sdk/`, no `node:*`, no `npm:*`, no
 * `Deno.*`. Nothing in here raises. Every rejection is a `SchemaError` in the left channel of an
 * `Either`, carrying the structured `path`/`expected`/`actual` fields rather than a formatted
 * message; rendering a human string is the caller's job (`sdk/schema.ts`'s `formatSchemaError`).
 *
 * Note on the type parameter order: the plan writes these signatures error-first
 * (`Either<SchemaError, unknown>`), but Effect 3's `Either` is value-first — `Either<A, E>`. The
 * code below uses the real Effect order.
 *
 * The validation semantics are carried over from the pre-port `sdk/schema.ts` unchanged:
 * `null` is accepted only when the node is `nullable`, `number` requires `Number.isFinite`,
 * `integer` requires `Number.isSafeInteger`, an `array` with a declared `length` must match it
 * exactly and is validated element-wise, unknown object keys are rejected, and `partial` mode
 * (document patches) skips absent keys instead of validating `undefined`.
 *
 * `storable` is the one storage rule layered on top (plan item 31, `docs/adr/0005-text-storage-contract.md`):
 * text bound for SQLite must not contain U+0000. `validate` itself still accepts it.
 */
import { Either } from "effect";
import { SchemaError } from "./errors.ts";

/** A runtime schema descriptor. Types are inferred from the builders; descriptors survive erasure. */
export type Node = {
  kind: "string" | "number" | "integer" | "boolean" | "enum" | "array";
  nullable?: boolean;
  values?: readonly string[];
  item?: Node;
  length?: number;
};
export type Field<T = unknown> = { node: Node; readonly _type?: T };
export type Fields = Record<string, Field>;
export type Infer<F extends Fields> = { [K in keyof F]: F[K] extends Field<infer T> ? T : never };
export type Storage = "columns" | "json";
/** A declared secondary index: a name plus the scalar fields it covers, in order. */
export type Index = { name: string; fields: readonly string[] };

export const v = {
  string: (): Field<string> => ({ node: { kind: "string" } }),
  number: (): Field<number> => ({ node: { kind: "number" } }),
  integer: (): Field<number> => ({ node: { kind: "integer" } }),
  boolean: (): Field<boolean> => ({ node: { kind: "boolean" } }),
  enum: <const T extends readonly [string, ...string[]]>(...values: T): Field<T[number]> => ({
    node: { kind: "enum", values },
  }),
  nullable: <T>(field: Field<T>): Field<T | null> => ({ node: { ...field.node, nullable: true } }),
  array: <T>(item: Field<T>, length?: number): Field<T[]> => ({
    node: { kind: "array", item: item.node, length },
  }),
};

/**
 * A table descriptor: its fields, its physical layout, and its declared indexes.
 *
 * `addIndex` is the total replacement for the pre-port `Table.index`. The app-facing variant that
 * raises lives in `sdk/schema.ts` (`AppTable`), because `app/schema.ts` declares its indexes at
 * module load and has no place to put an `Either`.
 */
export class Table<F extends Fields = Fields> {
  constructor(
    readonly fields: F,
    readonly storage: Storage = "columns",
    readonly indexes: readonly Index[] = [],
  ) {}

  addIndex(name: string, fields: readonly (keyof F & string)[]): Either.Either<Table<F>, SchemaError> {
    const named = identifier(name);
    if (Either.isLeft(named)) return Either.left(named.left);
    if (!fields.length) {
      return Either.left(
        new SchemaError({ reason: "empty-index", path: name, expected: "one or more fields" }),
      );
    }
    for (const field of fields) {
      if (!Object.hasOwn(this.fields, field) || this.fields[field].node.kind === "array") {
        return Either.left(
          new SchemaError({ reason: "unindexable-field", path: field, expected: "scalar field" }),
        );
      }
    }
    if (this.indexes.some((i) => i.name === name)) {
      return Either.left(
        new SchemaError({ reason: "duplicate-index", path: name, expected: "unused index name" }),
      );
    }
    return Either.right(new Table(this.fields, this.storage, [...this.indexes, { name, fields }]));
  }

  storeAs(storage: Storage): Table<F> {
    return new Table(this.fields, storage, this.indexes);
  }
}

export type Schema = Record<string, Table>;
export type Document<S extends Schema, K extends keyof S> = Infer<S[K]["fields"]> & {
  _id: string;
  _creationTime: number;
  _version: number;
};
export type Body<S extends Schema, K extends keyof S> = Infer<S[K]["fields"]>;

const IDENTIFIER = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** Accepts the SQL identifiers this repo is willing to interpolate into generated SQL. */
export function identifier(name: string): Either.Either<string, SchemaError> {
  if (!IDENTIFIER.test(name)) {
    return Either.left(
      new SchemaError({ reason: "invalid-identifier", path: name, expected: "identifier", actual: name }),
    );
  }
  return Either.right(name);
}

/**
 * Declares a table. Field names must be identifiers, and `_`-prefixed names are reserved for metadata.
 * An enum member may not contain U+0000: no value of it could be stored (see `storable`).
 */
export function defineTable<F extends Fields>(fields: F): Either.Either<Table<F>, SchemaError> {
  for (const [name, field] of Object.entries(fields)) {
    const named = identifier(name);
    if (Either.isLeft(named)) return Either.left(named.left);
    if (name.startsWith("_")) {
      return Either.left(
        new SchemaError({ reason: "reserved-field", path: name, expected: "unreserved name" }),
      );
    }
    let node: Node | undefined = field.node;
    while (node?.kind === "array") node = node.item;
    const member = node?.values?.find((value) => value.includes(NUL));
    if (member !== undefined) {
      return Either.left(
        new SchemaError({
          reason: "type-mismatch",
          path: name,
          expected: "enum values without U+0000",
          actual: member,
        }),
      );
    }
  }
  return Either.right(new Table(fields));
}

/**
 * Validates the table names of a schema and returns it unchanged. `_`-prefixed names are reserved
 * for metadata and `sqlite_`-prefixed ones (in any case) are reserved by SQLite itself.
 */
export function defineSchema<S extends Schema>(schema: S): Either.Either<S, SchemaError> {
  for (const name of Object.keys(schema)) {
    const named = identifier(name);
    if (Either.isLeft(named)) return Either.left(named.left);
    if (name.startsWith("_") || name.toLowerCase().startsWith("sqlite_")) {
      return Either.left(
        new SchemaError({ reason: "reserved-table", path: name, expected: "unreserved name" }),
      );
    }
  }
  return Either.right(schema);
}

/** Reprojects every table in a schema onto one physical layout. Pure data; nothing to validate. */
export function withStorage<S extends Schema>(schema: S, storage: Storage): S {
  return Object.fromEntries(Object.entries(schema).map(([k, t]) => [k, t.storeAs(storage)])) as S;
}

/** What the node demanded, in the wording the legacy `Error` message used. */
function expectation(node: Node): string {
  return `${node.kind}${node.nullable ? " or null" : ""}`;
}

/**
 * Checks one value against one node, returning the value on success and a `SchemaError` carrying
 * the failing path on rejection. Arrays recurse element-wise, so `path` accumulates indices
 * (`"board[3]"`) and the first failing element decides the result.
 */
export function validate(node: Node, value: unknown, path: string): Either.Either<unknown, SchemaError> {
  if (value === null && node.nullable) return Either.right(null);
  let valid = false;
  switch (node.kind) {
    case "string":
      valid = typeof value === "string";
      break;
    case "number":
      valid = typeof value === "number" && Number.isFinite(value);
      break;
    case "integer":
      valid = typeof value === "number" && Number.isSafeInteger(value);
      break;
    case "boolean":
      valid = typeof value === "boolean";
      break;
    case "enum":
      valid = typeof value === "string" && !!node.values?.includes(value);
      break;
    case "array":
      if (node.item === undefined) {
        // Only reachable by bypassing the `v` builders with a hand-built node. Returning a
        // Left keeps this module total: nothing in here raises, not even on malformed input.
        return Either.left(
          new SchemaError({
            reason: "type-mismatch",
            path,
            expected: "array with an item descriptor",
            actual: value,
          }),
        );
      }
      if (Array.isArray(value) && (node.length === undefined || value.length === node.length)) {
        // Sparse arrays are intentionally rejected: the old code used `value.map`, which
        // skips holes, while this index loop reads `value[i]` as `undefined` and fails the
        // item node. Strictly stronger, and unreachable from `JSON.parse` (never emits holes).
        const items: unknown[] = [];
        for (let i = 0; i < value.length; i++) {
          const item = validate(node.item, value[i], `${path}[${i}]`);
          if (Either.isLeft(item)) return Either.left(item.left);
          items.push(item.right);
        }
        return Either.right(items);
      }
      break;
  }
  if (!valid) {
    return Either.left(
      new SchemaError({ reason: "type-mismatch", path, expected: expectation(node), actual: value }),
    );
  }
  return Either.right(value);
}

/** U+0000, the one character the text storage contract refuses (`docs/adr/0005-text-storage-contract.md`). */
export const NUL = "\u0000";

/** The first string inside `value` that holds U+0000, with its path; arrays and objects are walked. */
function firstNul(value: unknown, path: string): { path: string; text: string } | undefined {
  if (typeof value === "string") return value.includes(NUL) ? { path, text: value } : undefined;
  if (Array.isArray(value)) {
    for (let i = 0; i < value.length; i++) {
      const found = firstNul(value[i], `${path}[${i}]`);
      if (found) return found;
    }
  } else if (value !== null && typeof value === "object") {
    for (const [key, item] of Object.entries(value)) {
      const found = firstNul(item, path ? `${path}.${key}` : key);
      if (found) return found;
    }
  }
  return undefined;
}

/**
 * The text storage contract (`docs/adr/0005-text-storage-contract.md`): text that would be written to
 * SQLite or compared against it must not hold U+0000, because `node:sqlite` truncates TEXT at the
 * first NUL on read. Walks strings, arrays and plain objects — a document body (`path` `""`, so its
 * keys are the paths), an array value, an id or a filter value — and rejects the first such string
 * with a `type-mismatch` at `validate`'s path (`tags[2]`). Anything else passes unchanged.
 *
 * Deliberately separate from `validate`: U+0000 is a valid `string`, so handler arguments and reader
 * filters accept it. Only the Storage boundary applies this check.
 */
export function storable<A>(value: A, path: string): Either.Either<A, SchemaError> {
  const found = firstNul(value, path);
  if (found === undefined) return Either.right(value);
  return Either.left(
    new SchemaError({
      reason: "type-mismatch",
      path: found.path,
      expected: "text without U+0000",
      actual: found.text,
    }),
  );
}

/**
 * Checks a whole object against a field map. Unknown keys are rejected before any field is
 * validated, so a typo never silently lands. With `partial` (a document patch) absent keys are
 * skipped; without it, every declared field must validate — `undefined` is not nullable, so a
 * missing field fails its node.
 */
export function parseObject<F extends Fields>(
  fields: F,
  input: unknown,
  partial = false,
): Either.Either<Infer<F>, SchemaError> {
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    return Either.left(
      new SchemaError({ reason: "not-an-object", path: "", expected: "object", actual: input }),
    );
  }
  const obj = input as Record<string, unknown>;
  for (const key of Object.keys(obj)) {
    if (!Object.hasOwn(fields, key)) {
      return Either.left(
        new SchemaError({ reason: "unknown-field", path: key, expected: "declared field", actual: obj[key] }),
      );
    }
  }
  const result: Record<string, unknown> = {};
  for (const [key, field] of Object.entries(fields)) {
    if (partial && !Object.hasOwn(obj, key)) continue;
    const value = validate(field.node, obj[key], key);
    if (Either.isLeft(value)) return Either.left(value.left);
    result[key] = value.right;
  }
  return Either.right(result as Infer<F>);
}
