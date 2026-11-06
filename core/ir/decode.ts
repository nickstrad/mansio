/**
 * Strict decode of an IR request (`ir/spec/v1.md` §3–§5): untrusted bytes or JSON in, a `Request` or
 * an `IrFailure` out.
 *
 * PURE: nothing in here raises. Decode checks everything that does not need the schema: the
 * envelope, every operation's shape and literal vocabularies (`op`, filter `op`, `direction`,
 * aggregate `kind`), unknown members, the limits, request-id and client-id syntax, `expect`
 * well-formedness, and `$ref` targets. Tables, fields, values and the `limit` range are the
 * executor's (and so `core/compile`'s) to judge, with their existing tags.
 *
 * Every rejection is a `SchemaError`: `unknown-field` for a member this version does not define,
 * `not-an-object` where an object is required, `type-mismatch` for everything else, with `path`
 * naming the member (`ops[3].expect.version`) and `op` set on the failure when the path is inside
 * `ops[i]`. Checks run in the order of the spec's §4/§5 tables, top to bottom; a request with one
 * defect is reported the same way by every conforming implementation.
 */
import { Either } from "effect";
import { RequestTooLarge, SchemaError } from "../errors.ts";
import { NUL } from "../schema.ts";
import { IrFailure } from "./errors.ts";
import { element, member, parseStrict } from "./json.ts";
import {
  type Expect,
  type IdRef,
  type InsertOp,
  type Kind,
  MAX_BODY_BYTES,
  MAX_ID_BYTES,
  MAX_OPS,
  type Op,
  type OpName,
  READ_OPS,
  type Request,
  type SelectOp,
} from "./types.ts";

type Json = Record<string, unknown>;
type Result<A> = Either.Either<A, SchemaError>;

/** `[A-Za-z0-9._:-]`, 1..64 bytes, not starting with `.` — mansio-server's `front.checkID`. */
const WIRE_ID = /^[A-Za-z0-9_:-][A-Za-z0-9._:-]{0,63}$/;
/** A canonical lowercase UUIDv7: version nibble 7, RFC 9562 variant. */
export const UUID_V7 = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const FINGERPRINT = /^sha256:[0-9a-f]{64}$/;
/**
 * `db.tenant` and `db.app`: the control plane's registry domains (mansio-server `0002_registry.sql`,
 * `tenant_id` and `database_name`), narrower than wire ids because the database is stored under the
 * placement key `tenant ":" app` (spec §4, ADR 0011).
 */
const TENANT = /^[a-z0-9][a-z0-9-]{0,62}$/;
const APP = /^[a-z0-9][a-z0-9_-]{0,55}$/;
const OPS: readonly OpName[] = ["select", "get", "insert", "patch", "delete", "assert"];

const mismatch = (path: string, expected: string, actual?: unknown): Result<never> =>
  Either.left(new SchemaError({ reason: "type-mismatch", path, expected, actual }));

/**
 * `value` as an object with only `allowed` members and every `required` one. An unknown member is
 * reported before a missing one, each in member order.
 */
function object(
  value: unknown,
  path: string,
  allowed: readonly string[],
  required: readonly string[],
): Result<Json> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    const at = path === "" ? "body" : path;
    return Either.left(
      new SchemaError({ reason: "not-an-object", path: at, expected: "object", actual: value }),
    );
  }
  const record = value as Json;
  for (const key of Object.keys(record)) {
    if (!allowed.includes(key)) {
      return Either.left(
        new SchemaError({ reason: "unknown-field", path: member(path, key), expected: "declared field" }),
      );
    }
  }
  for (const key of required) {
    if (!Object.hasOwn(record, key)) return mismatch(member(path, key), "required member");
  }
  return Either.right(record);
}

function string(value: unknown, path: string): Result<string> {
  return typeof value === "string" ? Either.right(value) : mismatch(path, "string", value);
}

function literal<T extends string>(value: unknown, path: string, allowed: readonly T[]): Result<T> {
  return typeof value === "string" && (allowed as readonly string[]).includes(value)
    ? Either.right(value as T)
    : mismatch(path, allowed.map((a) => JSON.stringify(a)).join(" | "), value);
}

function wireId(value: unknown, path: string): Result<string> {
  return typeof value === "string" && WIRE_ID.test(value)
    ? Either.right(value)
    : mismatch(path, `1..${MAX_ID_BYTES} of [A-Za-z0-9._:-], not starting with "."`, value);
}

function registryName(value: unknown, path: string, pattern: RegExp): Result<string> {
  return typeof value === "string" && pattern.test(value)
    ? Either.right(value)
    : mismatch(path, pattern.source, value);
}

/**
 * Walks a value tree (an insert or patch field, a filter value, an `expect.fields` value) and checks
 * every `$ref` in it. An object with a `$ref` member is a reference; any other object is left for
 * schema validation to reject, since no document value is an object.
 */
function refs(value: unknown, path: string, here: number, ops: readonly Op[]): Result<unknown> {
  if (Array.isArray(value)) {
    for (let i = 0; i < value.length; i++) {
      const checked = refs(value[i], element(path, i), here, ops);
      if (Either.isLeft(checked)) return checked;
    }
    return Either.right(value);
  }
  if (value !== null && typeof value === "object" && Object.hasOwn(value, "$ref")) {
    return ref(value, path, here, ops);
  }
  return Either.right(value);
}

/** `{"$ref": i}` with `i` the index of an earlier insert. */
function ref(value: unknown, path: string, here: number, ops: readonly Op[]): Result<IdRef> {
  return Either.flatMap(object(value, path, ["$ref"], ["$ref"]), (r) => {
    const i = r.$ref;
    if (typeof i !== "number" || !Number.isInteger(i) || i < 0 || i >= here || ops[i].op !== "insert") {
      return mismatch(member(path, "$ref"), "index of an earlier insert", i);
    }
    return Either.right({ $ref: i });
  });
}

/** An id position: a non-empty string without U+0000, or a `$ref`. */
function idRef(value: unknown, path: string, here: number, ops: readonly Op[]): Result<IdRef> {
  if (value !== null && typeof value === "object" && Object.hasOwn(value, "$ref")) {
    return ref(value, path, here, ops);
  }
  if (typeof value !== "string" || value === "" || value.includes(NUL)) {
    return mismatch(path, "non-empty id without U+0000, or $ref", value);
  }
  return Either.right(value);
}

function fieldValues(value: unknown, path: string, here: number, ops: readonly Op[]): Result<Json> {
  return Either.gen(function* () {
    const record = yield* object(value, path, Object.keys(value ?? {}), []);
    for (const [key, item] of Object.entries(record)) yield* refs(item, member(path, key), here, ops);
    return record;
  });
}

function expectOf(
  value: unknown,
  path: string,
  op: OpName,
  here: number,
  ops: readonly Op[],
): Result<Expect> {
  return Either.gen(function* () {
    const e = yield* object(value, path, ["version", "exists", "fields"], []);
    const out: Expect = {};
    if (!Object.keys(e).length) return yield* mismatch(path, "at least one of version, exists, fields");
    if (Object.hasOwn(e, "version")) {
      const version = e.version;
      if (typeof version !== "number" || !Number.isSafeInteger(version) || version < 1) {
        return yield* mismatch(member(path, "version"), "safe integer >= 1", version);
      }
      out.version = version;
    }
    if (Object.hasOwn(e, "fields")) {
      const fields = yield* fieldValues(e.fields, member(path, "fields"), here, ops);
      if (!Object.keys(fields).length) return yield* mismatch(member(path, "fields"), "at least one field");
      out.fields = fields;
    }
    if (Object.hasOwn(e, "exists")) {
      const exists = e.exists;
      if (typeof exists !== "boolean") return yield* mismatch(member(path, "exists"), "boolean", exists);
      if (!exists && (out.version !== undefined || out.fields !== undefined)) {
        return yield* mismatch(member(path, "exists"), "true when version or fields is present", exists);
      }
      if (!exists && op === "patch") {
        return yield* mismatch(member(path, "exists"), "true on a patch", exists);
      }
      out.exists = exists;
    }
    return out;
  });
}

function selectOf(o: Json, path: string, here: number, ops: readonly Op[]): Result<Op> {
  return Either.gen(function* () {
    const table = yield* string(o.table, member(path, "table"));
    const list = o.filters;
    if (!Array.isArray(list)) return yield* mismatch(member(path, "filters"), "array", list);
    const filters = [];
    for (let i = 0; i < list.length; i++) {
      const at = element(member(path, "filters"), i);
      const f = yield* object(list[i], at, ["field", "op", "value"], ["field", "op", "value"]);
      filters.push({
        field: yield* string(f.field, member(at, "field")),
        op: yield* literal(f.op, member(at, "op"), ["eq", "gt"] as const),
        value: yield* refs(f.value, member(at, "value"), here, ops),
      });
    }
    if (typeof o.limit !== "number") return yield* mismatch(member(path, "limit"), "number", o.limit);
    const select: SelectOp = { op: "select", table, filters, limit: o.limit };
    if (Object.hasOwn(o, "order")) {
      const at = member(path, "order");
      const order = yield* object(o.order, at, ["field", "direction"], ["field", "direction"]);
      select.order = {
        field: yield* string(order.field, member(at, "field")),
        direction: yield* literal(order.direction, member(at, "direction"), ["asc", "desc"] as const),
      };
    }
    if (Object.hasOwn(o, "aggregate")) {
      const at = member(path, "aggregate");
      const agg = yield* object(o.aggregate, at, ["kind", "field"], ["kind"]);
      const kind = yield* literal(agg.kind, member(at, "kind"), ["count", "countBy"] as const);
      if (kind === "count") {
        if (Object.hasOwn(agg, "field")) {
          return yield* Either.left(
            new SchemaError({
              reason: "unknown-field",
              path: member(at, "field"),
              expected: "declared field",
            }),
          );
        }
        select.aggregate = { kind };
      } else {
        if (!Object.hasOwn(agg, "field")) return yield* mismatch(member(at, "field"), "required member");
        select.aggregate = { kind, field: yield* string(agg.field, member(at, "field")) };
      }
    }
    return select;
  });
}

/** The members each operation allows, and the ones it requires. */
const SHAPES: Record<OpName, { allowed: readonly string[]; required: readonly string[] }> = {
  select: {
    allowed: ["op", "table", "filters", "order", "limit", "aggregate"],
    required: ["table", "filters", "limit"],
  },
  get: { allowed: ["op", "table", "id"], required: ["table", "id"] },
  insert: { allowed: ["op", "table", "value", "id"], required: ["table", "value"] },
  patch: { allowed: ["op", "table", "id", "value", "expect"], required: ["table", "id", "value"] },
  delete: { allowed: ["op", "table", "id", "expect"], required: ["table", "id"] },
  assert: { allowed: ["op", "table", "id", "expect"], required: ["table", "id", "expect"] },
};

function opOf(value: unknown, here: number, kind: Kind, ops: readonly Op[]): Result<Op> {
  const path = element("ops", here);
  return Either.gen(function* () {
    const head = yield* object(value, path, Object.keys(value ?? {}), ["op"]);
    const name = yield* literal(head.op, member(path, "op"), OPS);
    if (kind === "query" && !READ_OPS.includes(name)) {
      return yield* mismatch(member(path, "op"), "select | get in a query", name);
    }
    const shape = SHAPES[name];
    const o = yield* object(head, path, shape.allowed, shape.required);
    if (name === "select") return yield* selectOf(o, path, here, ops);
    const table = yield* string(o.table, member(path, "table"));
    switch (name) {
      case "get":
        return { op: name, table, id: yield* idRef(o.id, member(path, "id"), here, ops) };
      case "insert": {
        const insert: InsertOp = {
          op: name,
          table,
          value: yield* fieldValues(o.value, member(path, "value"), here, ops),
        };
        if (Object.hasOwn(o, "id")) {
          if (typeof o.id !== "string" || !UUID_V7.test(o.id)) {
            return yield* mismatch(member(path, "id"), "lowercase canonical UUIDv7", o.id);
          }
          insert.id = o.id;
        }
        return insert;
      }
      case "patch":
      case "delete":
      case "assert": {
        const id = yield* idRef(o.id, member(path, "id"), here, ops);
        const value = name === "patch"
          ? yield* fieldValues(o.value, member(path, "value"), here, ops)
          : undefined;
        const expect = Object.hasOwn(o, "expect")
          ? yield* expectOf(o.expect, member(path, "expect"), name, here, ops)
          : undefined;
        if (name === "patch") return { op: name, table, id, value: value!, ...(expect ? { expect } : {}) };
        if (name === "delete") return { op: name, table, id, ...(expect ? { expect } : {}) };
        return { op: name, table, id, expect: expect! };
      }
    }
  });
}

/** The index `i` when `path` is inside `ops[i]`. */
function opIndex(path: string): number | undefined {
  const match = /^ops\[(\d+)\]/.exec(path);
  return match ? Number(match[1]) : undefined;
}

const failure = (cause: SchemaError) => new IrFailure({ cause, op: opIndex(cause.path) });

/** Decodes an already-parsed JSON value. */
export function decodeValue(value: unknown): Either.Either<Request, IrFailure> {
  const decoded = Either.gen(function* () {
    const top = yield* object(value, "", ["v", "db", "kind", "requestId", "schema", "ops"], [
      "v",
      "db",
      "kind",
      "schema",
      "ops",
    ]);
    if (top.v !== 1) return yield* mismatch("v", "1", top.v);
    const db = yield* object(top.db, "db", ["tenant", "app"], ["tenant", "app"]);
    const tenant = yield* registryName(db.tenant, "db.tenant", TENANT);
    const app = yield* registryName(db.app, "db.app", APP);
    const kind = yield* literal(top.kind, "kind", ["query", "mutation"] as const);
    let requestId: string | undefined;
    if (Object.hasOwn(top, "requestId")) requestId = yield* wireId(top.requestId, "requestId");
    else if (kind === "mutation") return yield* mismatch("requestId", "required member");
    if (typeof top.schema !== "string" || !FINGERPRINT.test(top.schema)) {
      return yield* mismatch("schema", "sha256:<64 lowercase hex>", top.schema);
    }
    const list = top.ops;
    if (!Array.isArray(list) || list.length < 1 || list.length > MAX_OPS) {
      return yield* mismatch(
        "ops",
        `array of 1..${MAX_OPS} operations`,
        Array.isArray(list) ? list.length : list,
      );
    }
    const ops: Op[] = [];
    for (let i = 0; i < list.length; i++) ops.push(yield* opOf(list[i], i, kind, ops));
    const request: Request = { v: 1, db: { tenant, app }, kind, schema: top.schema, ops };
    if (requestId !== undefined) request.requestId = requestId;
    return request;
  });
  return Either.mapLeft(decoded, failure);
}

/**
 * Decodes a request body: the byte cap, then UTF-8, then strict JSON, then `decodeValue`. A body over
 * `MAX_BODY_BYTES` is `RequestTooLarge` (413); invalid UTF-8 is a `SchemaError` at `body`.
 */
export function decodeBody(body: Uint8Array): Either.Either<Request, IrFailure> {
  if (body.byteLength > MAX_BODY_BYTES) {
    return Either.left(
      new IrFailure({ cause: new RequestTooLarge({ limit: MAX_BODY_BYTES, observed: body.byteLength }) }),
    );
  }
  const text = Either.try({
    // `ignoreBOM` keeps a byte-order mark in the text, where the parser refuses it.
    try: () => new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(body),
    catch: () => new SchemaError({ reason: "type-mismatch", path: "body", expected: "UTF-8" }),
  });
  return Either.flatMap(Either.mapLeft(Either.flatMap(text, parseStrict), failure), decodeValue);
}
