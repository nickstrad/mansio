/**
 * The schema on the wire (`ir/spec/v1.md` §2): the registered JSON form of a `defineSchema` value,
 * decoded back into `Table`s, and its fingerprint text.
 *
 * PURE. The JSON form is exactly `JSON.stringify(schema)` of a `core/schema.ts` schema:
 * `{ <table>: { fields: { <field>: { node } }, storage, indexes: [{ name, fields }] } }`. Decoding is
 * strict (unknown members rejected, every node shape checked) and then applies the declaration rules
 * of `defineTable`, `defineSchema` and `Table.addIndex`, so a schema the SDK could not have declared
 * is refused here too.
 *
 * The fingerprint text is `JSON.stringify` of the decoded schema: members in the order they arrived,
 * no whitespace. It is what `_platform_schema.definition` stores, and the request's `schema` field is
 * `sha256:` + the hex SHA-256 of its UTF-8 bytes (hashing needs WebCrypto, so it lives in
 * `platform/ir/`).
 */
import { Either } from "effect";
import { SchemaError } from "../errors.ts";
import {
  defineSchema,
  defineTable,
  type Fields,
  type Node,
  type Schema,
  type Storage,
  Table,
} from "../schema.ts";
import { element, member } from "./json.ts";

type Result<A> = Either.Either<A, SchemaError>;
const KINDS: readonly Node["kind"][] = ["string", "number", "integer", "boolean", "enum", "array"];

const mismatch = (path: string, expected: string, actual?: unknown): Result<never> =>
  Either.left(new SchemaError({ reason: "type-mismatch", path, expected, actual }));

function record(value: unknown, path: string, allowed?: readonly string[]): Result<Record<string, unknown>> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return Either.left(new SchemaError({ reason: "not-an-object", path, expected: "object", actual: value }));
  }
  const out = value as Record<string, unknown>;
  const unknown = allowed && Object.keys(out).find((key) => !allowed.includes(key));
  if (unknown !== undefined) {
    return Either.left(
      new SchemaError({ reason: "unknown-field", path: member(path, unknown), expected: "declared field" }),
    );
  }
  return Either.right(out);
}

function node(value: unknown, path: string): Result<Node> {
  return Either.gen(function* () {
    const n = yield* record(value, path, ["kind", "nullable", "values", "item", "length"]);
    if (typeof n.kind !== "string" || !(KINDS as readonly string[]).includes(n.kind)) {
      return yield* mismatch(member(path, "kind"), KINDS.join(" | "), n.kind);
    }
    const kind = n.kind as Node["kind"];
    if (Object.hasOwn(n, "nullable") && typeof n.nullable !== "boolean") {
      return yield* mismatch(member(path, "nullable"), "boolean", n.nullable);
    }
    if (Object.hasOwn(n, "values")) {
      const values = n.values;
      if (
        kind !== "enum" || !Array.isArray(values) || values.some((v) => typeof v !== "string") ||
        new Set(values).size !== values.length
      ) {
        return yield* mismatch(member(path, "values"), "distinct strings, on an enum", values);
      }
    } else if (kind === "enum") return yield* mismatch(member(path, "values"), "required member");
    if (kind === "array") {
      if (!Object.hasOwn(n, "item")) return yield* mismatch(member(path, "item"), "required member");
      yield* node(n.item, member(path, "item"));
      if (Object.hasOwn(n, "length") && (!Number.isSafeInteger(n.length) || (n.length as number) < 0)) {
        return yield* mismatch(member(path, "length"), "non-negative safe integer", n.length);
      }
    } else {
      for (const key of ["item", "length"]) {
        if (Object.hasOwn(n, key)) return yield* mismatch(member(path, key), "absent unless kind is array");
      }
    }
    return n as Node;
  });
}

/** Decodes the registered schema JSON into a `Schema`, applying every declaration rule. */
export function decodeSchema(value: unknown): Result<Schema> {
  return Either.gen(function* () {
    const tables = yield* record(value, "schema");
    const schema: Schema = {};
    for (const [name, raw] of Object.entries(tables)) {
      const path = member("schema", name);
      const t = yield* record(raw, path, ["fields", "storage", "indexes"]);
      const rawFields = yield* record(t.fields, member(path, "fields"));
      const fields: Fields = {};
      for (const [field, f] of Object.entries(rawFields)) {
        const at = member(member(path, "fields"), field);
        const wrapped = yield* record(f, at, ["node"]);
        fields[field] = { node: yield* node(wrapped.node, member(at, "node")) };
      }
      let table: Table = yield* defineTable(fields);
      if (t.storage !== "columns" && t.storage !== "json") {
        return yield* mismatch(member(path, "storage"), '"columns" | "json"', t.storage);
      }
      table = table.storeAs(t.storage as Storage);
      if (!Array.isArray(t.indexes)) return yield* mismatch(member(path, "indexes"), "array", t.indexes);
      for (let i = 0; i < t.indexes.length; i++) {
        const at = element(member(path, "indexes"), i);
        const index = yield* record(t.indexes[i], at, ["name", "fields"]);
        if (typeof index.name !== "string") return yield* mismatch(member(at, "name"), "string", index.name);
        if (!Array.isArray(index.fields) || index.fields.some((f) => typeof f !== "string")) {
          return yield* mismatch(member(at, "fields"), "array of strings", index.fields);
        }
        table = yield* table.addIndex(index.name, index.fields as string[]);
      }
      schema[name] = table;
    }
    return yield* defineSchema(schema);
  });
}

/** The fingerprint text: what `_platform_schema` stores and what the request's `schema` hashes. */
export function fingerprintText(schema: Schema): string {
  return JSON.stringify(schema);
}
