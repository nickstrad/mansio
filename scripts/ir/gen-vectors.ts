/**
 * The source of the shared IR test vectors (`ir/vectors/*.json`, format in `ir/vectors/README.md`).
 *
 * Vectors are authored here, in TypeScript, because a few hundred hand-written JSON documents drift
 * and typo; the JSON files are the artifact both runners read. Every expectation below is written by
 * hand, except two kinds of golden output that the TS implementation is the oracle for by decision
 * (ADR 0010): compiled SQL (`compile.json`) and schema fingerprints (`schema.json`, and the request
 * `schema` field of `execute.json`). Those are computed from `core/compile` and `core/ir/schema.ts`
 * when the files are generated, and `tests/unit/ir_vectors_test.ts` fails if a committed file no
 * longer matches this source.
 *
 * ```sh
 * deno run -A scripts/ir/gen-vectors.ts      # rewrite ir/vectors/*.json, then `deno fmt ir/`
 * ```
 */
import { Effect, Either } from "effect";
import type { Schema, Storage } from "../../core/schema.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileSelect } from "../../core/compile/select.ts";
import { compileDelete, compileInsert, compileReplace } from "../../core/compile/write.ts";
import type { SelectPlan } from "../../core/plan.ts";
import { decodeValue } from "../../core/ir/decode.ts";
import { canonical } from "../../core/ir/json.ts";
import { decodeSchema, fingerprintText } from "../../core/ir/schema.ts";
import { sha256Fingerprint } from "../../platform/ir/executor.ts";

type J = Record<string, unknown>;
type ErrorExpect = { tag: string; status: number; op?: number; fields?: J };
export type VectorFile = { category: string; version: 1; description: string; vectors: J[] };

const err = (tag: string, status: number, fields?: J, op?: number): { error: ErrorExpect } => ({
  error: { tag, status, ...(op === undefined ? {} : { op }), ...(fields ? { fields } : {}) },
});
const schemaErr = (reason: string, path: string, op?: number) =>
  err("SchemaError", 400, { reason, path }, op);
const compileErr = (fields: J, op?: number) => err("CompileError", 400, fields, op);

// ---------------------------------------------------------------------------
// The vector schema: one table of every node kind, plus two for `$ref` chains
// ---------------------------------------------------------------------------

const NOTES_FIELDS = {
  title: { node: { kind: "string" } },
  score: { node: { kind: "number" } },
  rank: { node: { kind: "integer" } },
  done: { node: { kind: "boolean" } },
  tag: { node: { kind: "enum", values: ["a", "b"], nullable: true } },
  tags: { node: { kind: "array", item: { kind: "string" } } },
  grid: { node: { kind: "array", item: { kind: "integer" }, length: 3 } },
  owner: { node: { kind: "string", nullable: true } },
};

function schemaJson(storage: Storage): J {
  return {
    notes: { fields: NOTES_FIELDS, storage, indexes: [{ name: "by_rank", fields: ["rank"] }] },
    lists: { fields: { name: { node: { kind: "string" } } }, storage, indexes: [] },
    entries: {
      fields: { listId: { node: { kind: "string" } }, n: { node: { kind: "integer" } } },
      storage,
      indexes: [{ name: "by_list", fields: ["listId", "n"] }],
    },
  };
}

function schemaOf(json: unknown): Schema {
  const decoded = decodeSchema(json);
  if (Either.isLeft(decoded)) throw decoded.left;
  return decoded.right;
}

/** Any well-formed fingerprint, for vectors that never reach a database. */
const ANY_FP = `sha256:${"0".repeat(64)}`;
const FP = "$FINGERPRINT";
const U1 = "01890a5d-ac96-774b-bcce-b302099a8057";
const U2 = "01890a5d-ac96-774b-bcce-b302099a8058";

const note = (id: string, creation: number, version: number, body: J): J => ({
  _id: id,
  _creationTime: creation,
  _version: version,
  title: "t",
  score: 0,
  rank: 0,
  done: false,
  tag: null,
  tags: [],
  grid: [0, 0, 0],
  owner: null,
  ...body,
});
const BODY = {
  title: "new",
  score: 0.5,
  rank: 7,
  done: true,
  tag: "b",
  tags: ["p", "q"],
  grid: [4, 5, 6],
  owner: null,
};
const N1 = note("n1", 100, 1, {
  title: "alpha",
  score: 1.5,
  rank: 1,
  tag: "a",
  tags: ["x"],
  grid: [1, 2, 3],
});
const N2 = note("n2", 200, 3, { title: "beta", score: 2.5, rank: 2, done: true, owner: "bob" });
const N3 = note("n3", 300, 1, {
  title: "gamma",
  score: -1,
  rank: 2,
  tag: "b",
  tags: ["y", "z"],
  grid: [9, 9, 9],
  owner: "amy",
});
const SEED = { notes: [N1, N2, N3] };

// ---------------------------------------------------------------------------
// decode.json
// ---------------------------------------------------------------------------

const envelope = (over: J = {}): J => ({
  v: 1,
  db: { tenant: "acme", app: "notes" },
  kind: "mutation",
  requestId: "r-1",
  schema: ANY_FP,
  ops: [{ op: "get", table: "notes", id: "n1" }],
  ...over,
});
const query = (ops: unknown[], over: J = {}) =>
  envelope({ kind: "query", requestId: undefined, ops, ...over });
const mutation = (ops: unknown[], over: J = {}) => envelope({ ops, ...over });
const getN0 = (): J => ({ op: "get", table: "lists", id: { $ref: 0 } });
const sel = (over: J = {}): J => ({ op: "select", table: "notes", filters: [], limit: 10, ...over });
const without = (o: J, key: string): J => Object.fromEntries(Object.entries(o).filter(([k]) => k !== key));

type DecodeCase = {
  name: string;
  why: string;
  request?: unknown;
  raw?: string;
  base64?: string;
  pad?: number;
  expect: J;
};
const ok = { ok: true };

const nested = (n: number, inner: string) => "[".repeat(n) + inner + "]".repeat(n);
/** A query whose filter value is `n` nested arrays: with the 5 containers around it, `n + 5` deep. */
const deep = (n: number, inner: string) =>
  JSON.stringify(query([sel({ filters: [{ field: "tags", op: "eq", value: "X" }] })])).replace(
    '"X"',
    nested(n, inner),
  );
const SAFE_PLUS_TWO = JSON.stringify(query([sel({ filters: [{ field: "rank", op: "eq", value: 7 }] })]))
  .replace('"value":7', '"value":9007199254740993');

const DECODE: DecodeCase[] = [
  // Accepted
  { name: "minimal query", why: "one select, no requestId", request: query([sel()]), expect: ok },
  {
    name: "query may carry a requestId",
    why: "reads ignore it, as mansio-server does",
    request: query([sel()], { requestId: "q-1" }),
    expect: ok,
  },
  {
    name: "every mutation op",
    why: "the full v1 vocabulary in one request",
    request: mutation([
      { op: "insert", table: "lists", value: { name: "l" } },
      { op: "insert", table: "entries", value: { listId: { $ref: 0 }, n: 1 }, id: U1 },
      { op: "get", table: "lists", id: { $ref: 0 } },
      { op: "patch", table: "entries", id: { $ref: 1 }, value: { n: 2 }, expect: { version: 1 } },
      { op: "assert", table: "notes", id: "n1", expect: { exists: true, version: 1, fields: { rank: 1 } } },
      { op: "delete", table: "notes", id: "n2" },
      { op: "delete", table: "notes", id: "n3", expect: { exists: false } },
      sel({
        filters: [{ field: "rank", op: "gt", value: 1 }, { field: "tag", op: "eq", value: null }],
        order: { field: "score", direction: "desc" },
      }),
      sel({ aggregate: { kind: "count" } }),
      sel({ aggregate: { kind: "countBy", field: "tag" } }),
    ]),
    expect: ok,
  },
  {
    name: "$ref inside an array value",
    why: "a reference may sit anywhere in a value tree",
    request: mutation([
      { op: "insert", table: "lists", value: { name: "l" } },
      { op: "insert", table: "notes", value: { ...BODY, tags: ["a", { $ref: 0 }] } },
    ]),
    expect: ok,
  },
  {
    name: "whitespace and member order are not significant",
    why: "the canonical form sorts members and drops whitespace",
    raw:
      ` { "ops" : [ { "limit" : 10 , "filters" : [ ] , "table" : "notes" , "op" : "select" } ] ,\n "schema" : "${ANY_FP}" , "kind" : "query" , "db" : { "app" : "notes" , "tenant" : "acme" } , "v" : 1 } `,
    expect: ok,
  },
  {
    name: "limit 0 decodes",
    why: "the limit range is the compiler's rule (CompileError)",
    request: query([sel({ limit: 0 })]),
    expect: ok,
  },
  {
    name: "limit 10.0",
    why: "a number literal is its value: 10.0 is 10",
    raw: JSON.stringify(query([sel()])).replace('"limit":10', '"limit":10.0'),
    expect: ok,
  },
  {
    name: "64 ops",
    why: "on point of MAX_OPS",
    request: query(Array.from({ length: 64 }, () => sel())),
    expect: ok,
  },
  {
    name: "64-byte request id",
    why: "on point of the wire-id length",
    request: mutation([sel()], { requestId: "r".repeat(64) }),
    expect: ok,
  },
  {
    name: "63-byte tenant, 56-byte app",
    why: "on points of the registry domains' lengths (spec §4)",
    request: query([sel()], { db: { tenant: "t".repeat(63), app: "a".repeat(56) } }),
    expect: ok,
  },
  {
    name: "registry charset",
    why: "every character the tenant and app domains allow",
    request: query([sel()], { db: { tenant: "0a-z9", app: "9a_b-z0" } }),
    expect: ok,
  },
  {
    name: "id charset",
    why: "every allowed id character",
    request: mutation([sel()], { requestId: "Az09._:-x" }),
    expect: ok,
  },
  {
    name: "unicode text",
    why: "non-ASCII and astral characters are ordinary text",
    request: query([sel({ filters: [{ field: "title", op: "eq", value: "héllo 🌍" }] })]),
    expect: ok,
  },
  {
    name: "body at the cap",
    why: "on point: exactly 65536 bytes is accepted",
    request: query([sel()]),
    pad: 65_536,
    expect: ok,
  },
  {
    name: "U+0000 in a filter value decodes",
    why: "NUL is refused at execution (SchemaError on the field), not by decode",
    request: query([sel({ filters: [{ field: "title", op: "eq", value: "a\u0000" }] })]),
    expect: ok,
  },
  // Body and JSON
  {
    name: "body over the cap",
    why: "off point: 65537 bytes",
    request: query([sel()]),
    pad: 65_537,
    expect: err("RequestTooLarge", 413, { limit: 65_536 }),
  },
  { name: "not JSON", why: "malformed text", raw: "{", expect: schemaErr("type-mismatch", "body") },
  {
    name: "trailing text",
    why: "one value only",
    raw: JSON.stringify(query([sel()])) + " x",
    expect: schemaErr("type-mismatch", "body"),
  },
  {
    name: "byte-order mark",
    why: "a BOM is not JSON whitespace",
    raw: "﻿" + JSON.stringify(query([sel()])),
    expect: schemaErr("type-mismatch", "body"),
  },
  {
    name: "invalid UTF-8",
    why: "bytes must be UTF-8",
    base64: "eyJ2Ijr/fQ==",
    expect: schemaErr("type-mismatch", "body"),
  },
  {
    name: "duplicate member",
    why: "JSON.parse keeps the last; the protocol refuses rather than guess",
    raw: JSON.stringify(query([sel()])).replace('"kind":"query"', '"kind":"query","kind":"mutation"'),
    expect: schemaErr("type-mismatch", "kind"),
  },
  {
    name: "duplicate member inside an op",
    why: "the path names the duplicate; op names its index",
    raw: JSON.stringify(query([sel()])).replace('"table":"notes"', '"table":"notes","table":"lists"'),
    expect: schemaErr("type-mismatch", "ops[0].table", 0),
  },
  {
    name: "lone surrogate",
    why: "Go would read it as U+FFFD; refusing keeps both sides equal",
    raw: JSON.stringify(query([sel({ filters: [{ field: "title", op: "eq", value: "X" }] })])).replace(
      '"X"',
      '"\\ud800"',
    ),
    expect: schemaErr("type-mismatch", "ops[0].filters[0].value", 0),
  },
  {
    name: "number out of double range",
    why: "JSON.parse would produce Infinity",
    raw: JSON.stringify(query([sel()])).replace('"limit":10', '"limit":1e400'),
    expect: schemaErr("type-mismatch", "ops[0].limit", 0),
  },
  {
    name: "body is an array",
    why: "the envelope is an object",
    request: [],
    expect: schemaErr("not-an-object", "body"),
  },
  // Envelope
  {
    name: "unknown envelope member",
    why: "strict decode",
    request: envelope({ extra: 1 }),
    expect: schemaErr("unknown-field", "extra"),
  },
  {
    name: "v 2",
    why: "only version 1 exists",
    request: envelope({ v: 2 }),
    expect: schemaErr("type-mismatch", "v"),
  },
  {
    name: 'v "1"',
    why: "a string is not the number 1",
    request: envelope({ v: "1" }),
    expect: schemaErr("type-mismatch", "v"),
  },
  {
    name: "missing db",
    why: "db is required",
    request: without(envelope(), "db"),
    expect: schemaErr("type-mismatch", "db"),
  },
  {
    name: "db unknown member",
    why: "strict decode",
    request: envelope({ db: { tenant: "a", app: "b", region: "x" } }),
    expect: schemaErr("unknown-field", "db.region"),
  },
  {
    name: "tenant with a slash",
    why: "tenants are [a-z0-9-]",
    request: envelope({ db: { tenant: "a/b", app: "x" } }),
    expect: schemaErr("type-mismatch", "db.tenant"),
  },
  {
    name: "app starting with a dot",
    why: "apps are [a-z0-9_-]; names become file names",
    request: envelope({ db: { tenant: "a", app: ".x" } }),
    expect: schemaErr("type-mismatch", "db.app"),
  },
  {
    name: "64-byte tenant",
    why: "off point of the tenant domain's length",
    request: envelope({ db: { tenant: "t".repeat(64), app: "x" } }),
    expect: schemaErr("type-mismatch", "db.tenant"),
  },
  {
    name: "57-byte app",
    why: "off point of the app domain's length",
    request: envelope({ db: { tenant: "a", app: "a".repeat(57) } }),
    expect: schemaErr("type-mismatch", "db.app"),
  },
  {
    name: "uppercase tenant",
    why: "registry names are lowercase (a wire id would allow it)",
    request: envelope({ db: { tenant: "Acme", app: "x" } }),
    expect: schemaErr("type-mismatch", "db.tenant"),
  },
  {
    name: "uppercase app",
    why: "registry names are lowercase (a wire id would allow it)",
    request: envelope({ db: { tenant: "a", app: "Notes" } }),
    expect: schemaErr("type-mismatch", "db.app"),
  },
  {
    name: "tenant with a dot",
    why: "the tenant domain has no '.' (a wire id would allow it)",
    request: envelope({ db: { tenant: "a.b", app: "x" } }),
    expect: schemaErr("type-mismatch", "db.tenant"),
  },
  {
    name: "app with a colon",
    why: "':' separates tenant and app in the placement key",
    request: envelope({ db: { tenant: "a", app: "b:c" } }),
    expect: schemaErr("type-mismatch", "db.app"),
  },
  {
    name: "tenant with an underscore",
    why: "only the app domain allows '_'",
    request: envelope({ db: { tenant: "a_b", app: "x" } }),
    expect: schemaErr("type-mismatch", "db.tenant"),
  },
  {
    name: "app starting with a dash",
    why: "both domains start with [a-z0-9]",
    request: envelope({ db: { tenant: "a", app: "-x" } }),
    expect: schemaErr("type-mismatch", "db.app"),
  },
  {
    name: "empty app",
    why: "apps are 1..56 bytes",
    request: envelope({ db: { tenant: "a", app: "" } }),
    expect: schemaErr("type-mismatch", "db.app"),
  },
  {
    name: "unknown kind",
    why: "query | mutation",
    request: envelope({ kind: "write" }),
    expect: schemaErr("type-mismatch", "kind"),
  },
  {
    name: "mutation without requestId",
    why: "a mutation's idempotency key is required",
    request: without(envelope(), "requestId"),
    expect: schemaErr("type-mismatch", "requestId"),
  },
  {
    name: "requestId with a space",
    why: "same rule as ids",
    request: envelope({ requestId: "r 1" }),
    expect: schemaErr("type-mismatch", "requestId"),
  },
  {
    name: "requestId 65 bytes",
    why: "off point",
    request: envelope({ requestId: "r".repeat(65) }),
    expect: schemaErr("type-mismatch", "requestId"),
  },
  {
    name: "missing schema",
    why: "the fingerprint is required",
    request: without(envelope(), "schema"),
    expect: schemaErr("type-mismatch", "schema"),
  },
  {
    name: "uppercase fingerprint",
    why: "sha256:<64 lowercase hex>",
    request: envelope({ schema: `sha256:${"AB".repeat(32)}` }),
    expect: schemaErr("type-mismatch", "schema"),
  },
  {
    name: "no ops",
    why: "at least one op",
    request: envelope({ ops: [] }),
    expect: schemaErr("type-mismatch", "ops"),
  },
  {
    name: "65 ops",
    why: "off point of MAX_OPS",
    request: query(Array.from({ length: 65 }, () => sel())),
    expect: schemaErr("type-mismatch", "ops"),
  },
  // Operations
  {
    name: "unknown op",
    why: "the v1 vocabulary is closed",
    request: mutation([{ op: "upsert", table: "notes" }]),
    expect: schemaErr("type-mismatch", "ops[0].op", 0),
  },
  {
    name: "op not an object",
    why: "ops are objects",
    request: mutation(["get"]),
    expect: schemaErr("not-an-object", "ops[0]", 0),
  },
  {
    name: "query containing an insert",
    why: "a query may contain only select and get",
    request: query([sel(), { op: "insert", table: "notes", value: BODY }]),
    expect: schemaErr("type-mismatch", "ops[1].op", 1),
  },
  {
    name: "query containing assert",
    why: "assert is a mutation op",
    request: query([{ op: "assert", table: "notes", id: "n1", expect: { exists: true } }]),
    expect: schemaErr("type-mismatch", "ops[0].op", 0),
  },
  {
    name: "unknown op member",
    why: "get has no value",
    request: query([{ op: "get", table: "notes", id: "n1", value: {} }]),
    expect: schemaErr("unknown-field", "ops[0].value", 0),
  },
  {
    name: "select without limit",
    why: "limit is required",
    request: query([without(sel(), "limit")]),
    expect: schemaErr("type-mismatch", "ops[0].limit", 0),
  },
  {
    name: "select without filters",
    why: "filters is required, even empty",
    request: query([without(sel(), "filters")]),
    expect: schemaErr("type-mismatch", "ops[0].filters", 0),
  },
  {
    name: "limit as a string",
    why: "limit is a number",
    request: query([sel({ limit: "10" })]),
    expect: schemaErr("type-mismatch", "ops[0].limit", 0),
  },
  {
    name: "filter op ne",
    why: "eq | gt only",
    request: query([sel({ filters: [{ field: "rank", op: "ne", value: 1 }] })]),
    expect: schemaErr("type-mismatch", "ops[0].filters[0].op", 0),
  },
  {
    name: "filter without value",
    why: "value is required (null is a value)",
    request: query([sel({ filters: [{ field: "rank", op: "eq" }] })]),
    expect: schemaErr("type-mismatch", "ops[0].filters[0].value", 0),
  },
  {
    name: "direction ASC",
    why: "lowercase only",
    request: query([sel({ order: { field: "rank", direction: "ASC" } })]),
    expect: schemaErr("type-mismatch", "ops[0].order.direction", 0),
  },
  {
    name: "aggregate sum",
    why: "count | countBy",
    request: query([sel({ aggregate: { kind: "sum" } })]),
    expect: schemaErr("type-mismatch", "ops[0].aggregate.kind", 0),
  },
  {
    name: "count with a field",
    why: "count takes no field",
    request: query([sel({ aggregate: { kind: "count", field: "rank" } })]),
    expect: schemaErr("unknown-field", "ops[0].aggregate.field", 0),
  },
  {
    name: "countBy without a field",
    why: "countBy needs its grouping field",
    request: query([sel({ aggregate: { kind: "countBy" } })]),
    expect: schemaErr("type-mismatch", "ops[0].aggregate.field", 0),
  },
  {
    name: "table not a string",
    why: "tables are named",
    request: query([{ op: "get", table: 1, id: "n1" }]),
    expect: schemaErr("type-mismatch", "ops[0].table", 0),
  },
  {
    name: "empty id",
    why: "ids are non-empty",
    request: query([{ op: "get", table: "notes", id: "" }]),
    expect: schemaErr("type-mismatch", "ops[0].id", 0),
  },
  {
    name: "id with U+0000",
    why: "text storage contract (ADR 0005)",
    request: query([{ op: "get", table: "notes", id: "n\u00001" }]),
    expect: schemaErr("type-mismatch", "ops[0].id", 0),
  },
  {
    name: "insert value is an array",
    why: "a body is an object",
    request: mutation([{ op: "insert", table: "notes", value: [] }]),
    expect: schemaErr("not-an-object", "ops[0].value", 0),
  },
  {
    name: "insert with expect",
    why: "an insert's precondition is implicit (exists:false)",
    request: mutation([{ op: "insert", table: "notes", value: BODY, expect: { exists: false } }]),
    expect: schemaErr("unknown-field", "ops[0].expect", 0),
  },
  {
    name: "insert id UUIDv4",
    why: "client ids are UUIDv7",
    request: mutation([{
      op: "insert",
      table: "notes",
      value: BODY,
      id: "1b4e28ba-2fa1-41d2-883f-0016d3cca427",
    }]),
    expect: schemaErr("type-mismatch", "ops[0].id", 0),
  },
  {
    name: "insert id uppercase",
    why: "canonical lowercase only, so one id has one spelling",
    request: mutation([{ op: "insert", table: "notes", value: BODY, id: U1.toUpperCase() }]),
    expect: schemaErr("type-mismatch", "ops[0].id", 0),
  },
  {
    name: "insert id wrong variant",
    why: "RFC 9562 variant bits 10",
    request: mutation([{
      op: "insert",
      table: "notes",
      value: BODY,
      id: "01890a5d-ac96-774b-7cce-b302099a8057",
    }]),
    expect: schemaErr("type-mismatch", "ops[0].id", 0),
  },
  {
    name: "insert id as $ref",
    why: "an insert's id is literal",
    request: mutation([{ op: "insert", table: "lists", value: { name: "l" } }, {
      op: "insert",
      table: "lists",
      value: { name: "m" },
      id: { $ref: 0 },
    }]),
    expect: schemaErr("type-mismatch", "ops[1].id", 1),
  },
  {
    name: "patch without value",
    why: "value is required",
    request: mutation([{ op: "patch", table: "notes", id: "n1" }]),
    expect: schemaErr("type-mismatch", "ops[0].value", 0),
  },
  {
    name: "assert without expect",
    why: "an assert is its precondition",
    request: mutation([{ op: "assert", table: "notes", id: "n1" }]),
    expect: schemaErr("type-mismatch", "ops[0].expect", 0),
  },
  // $ref
  {
    name: "$ref forward",
    why: "references only point backwards",
    request: mutation([{ op: "get", table: "notes", id: { $ref: 1 } }, {
      op: "insert",
      table: "notes",
      value: BODY,
    }]),
    expect: schemaErr("type-mismatch", "ops[0].id.$ref", 0),
  },
  {
    name: "$ref to itself",
    why: "an op cannot use its own result",
    request: mutation([{ op: "insert", table: "lists", value: { name: { $ref: 0 } } }]),
    expect: schemaErr("type-mismatch", "ops[0].value.name.$ref", 0),
  },
  {
    name: "$ref to a non-insert",
    why: "only insert results are ids",
    request: mutation([{ op: "get", table: "notes", id: "n1" }, {
      op: "delete",
      table: "notes",
      id: { $ref: 0 },
    }]),
    expect: schemaErr("type-mismatch", "ops[1].id.$ref", 1),
  },
  {
    name: "$ref not an integer",
    why: "an index is an integer",
    request: mutation([{ op: "insert", table: "lists", value: { name: "l" } }, {
      op: "get",
      table: "lists",
      id: { $ref: 0.5 },
    }]),
    expect: schemaErr("type-mismatch", "ops[1].id.$ref", 1),
  },
  {
    name: "$ref with an extra member",
    why: "a reference is exactly {$ref}",
    request: mutation([{ op: "insert", table: "lists", value: { name: "l" } }, {
      op: "get",
      table: "lists",
      id: { $ref: 0, x: 1 },
    }]),
    expect: schemaErr("unknown-field", "ops[1].id.x", 1),
  },
  {
    name: "$ref in a query",
    why: "a query has no inserts to reference",
    request: query([sel({ filters: [{ field: "title", op: "eq", value: { $ref: 0 } }] })]),
    expect: schemaErr("type-mismatch", "ops[0].filters[0].value.$ref", 0),
  },
  {
    name: "$ref in expect.fields",
    why: "allowed: fields values may reference",
    request: mutation([{ op: "insert", table: "lists", value: { name: "l" } }, {
      op: "assert",
      table: "entries",
      id: "e1",
      expect: { fields: { listId: { $ref: 0 } } },
    }]),
    expect: ok,
  },
  // expect
  {
    name: "empty expect",
    why: "an expect states at least one check",
    request: mutation([{ op: "delete", table: "notes", id: "n1", expect: {} }]),
    expect: schemaErr("type-mismatch", "ops[0].expect", 0),
  },
  {
    name: "expect version 0",
    why: "versions start at 1",
    request: mutation([{ op: "delete", table: "notes", id: "n1", expect: { version: 0 } }]),
    expect: schemaErr("type-mismatch", "ops[0].expect.version", 0),
  },
  {
    name: "expect version 1.5",
    why: "versions are integers",
    request: mutation([{ op: "delete", table: "notes", id: "n1", expect: { version: 1.5 } }]),
    expect: schemaErr("type-mismatch", "ops[0].expect.version", 0),
  },
  {
    name: "expect version 2^53",
    why: "off point of the safe-integer range",
    request: mutation([{ op: "delete", table: "notes", id: "n1", expect: { version: 2 ** 53 } }]),
    expect: schemaErr("type-mismatch", "ops[0].expect.version", 0),
  },
  {
    name: 'expect exists "yes"',
    why: "exists is a boolean",
    request: mutation([{ op: "delete", table: "notes", id: "n1", expect: { exists: "yes" } }]),
    expect: schemaErr("type-mismatch", "ops[0].expect.exists", 0),
  },
  {
    name: "exists false with version",
    why: "contradictory: version implies existence",
    request: mutation([{ op: "delete", table: "notes", id: "n1", expect: { exists: false, version: 1 } }]),
    expect: schemaErr("type-mismatch", "ops[0].expect.exists", 0),
  },
  {
    name: "patch expecting absence",
    why: "a patch of a missing document can never succeed",
    request: mutation([{ op: "patch", table: "notes", id: "n1", value: {}, expect: { exists: false } }]),
    expect: schemaErr("type-mismatch", "ops[0].expect.exists", 0),
  },
  {
    name: "empty expect.fields",
    why: "fields names at least one field",
    request: mutation([{ op: "assert", table: "notes", id: "n1", expect: { fields: {} } }]),
    expect: schemaErr("type-mismatch", "ops[0].expect.fields", 0),
  },
  {
    name: "unknown expect member",
    why: "strict decode; range is reserved for v1.1",
    request: mutation([{ op: "assert", table: "notes", id: "n1", expect: { range: 1 } }]),
    expect: schemaErr("unknown-field", "ops[0].expect.range", 0),
  },
  // Number literals, nesting, keys and ids (review fixups)
  {
    name: "integer literal above 2^53",
    why: "a literal is its double value: 9007199254740993 reads as 9007199254740992",
    raw: SAFE_PLUS_TWO,
    expect: ok,
  },
  {
    name: "v 1.0",
    why: "1.0 is the number 1",
    raw: JSON.stringify(query([sel()])).replace('"v":1', '"v":1.0'),
    expect: ok,
  },
  {
    name: "$ref 0.0",
    why: "0.0 is the integer 0",
    raw: JSON.stringify(mutation([{ op: "insert", table: "lists", value: { name: "l" } }, getN0()])).replace(
      '"$ref":0',
      '"$ref":0.0',
    ),
    expect: ok,
  },
  {
    name: "expect version 1.0",
    why: "1.0 is the integer 1",
    raw: JSON.stringify(mutation([{ op: "delete", table: "notes", id: "n1", expect: { version: 1 } }]))
      .replace('"version":1', '"version":1.0'),
    expect: ok,
  },
  {
    name: "expect version 9007199254740993",
    why: "reads as 2^53, which is not a safe integer",
    raw: JSON.stringify(mutation([{ op: "delete", table: "notes", id: "n1", expect: { version: 1 } }]))
      .replace('"version":1', '"version":9007199254740993'),
    expect: schemaErr("type-mismatch", "ops[0].expect.version", 0),
  },
  {
    name: "number underflow",
    why: "1e-400 is finite after rounding: it reads as 0",
    raw: JSON.stringify(query([sel({ filters: [{ field: "score", op: "eq", value: 7 }] })])).replace(
      '"value":7',
      '"value":1e-400',
    ),
    expect: ok,
  },
  {
    name: "64 containers, empty innermost",
    why: "on point of the nesting limit",
    raw: deep(59, ""),
    expect: ok,
  },
  {
    name: "64 containers, non-empty innermost",
    why: "scalars do not count as nesting",
    raw: deep(59, "1"),
    expect: ok,
  },
  {
    name: "65 containers, empty innermost",
    why: "off point: the 65th container is refused at its own path",
    raw: deep(60, ""),
    expect: schemaErr("type-mismatch", "ops[0].filters[0].value" + "[0]".repeat(59), 0),
  },
  {
    name: "65 containers, non-empty innermost",
    why: "off point",
    raw: deep(60, "1"),
    expect: schemaErr("type-mismatch", "ops[0].filters[0].value" + "[0]".repeat(59), 0),
  },
  {
    name: "lone surrogate in a member name",
    why:
      "names are text too; the path names the member (not compared here: a lone surrogate cannot survive a Go string)",
    raw: JSON.stringify(mutation([{ op: "insert", table: "notes", value: { Q: 1 } }])).replace(
      '"Q"',
      '"\\ud800"',
    ),
    expect: err("SchemaError", 400, { reason: "type-mismatch" }, 0),
  },
  {
    name: "duplicate member spelled with an escape",
    why: "names compare after unescaping: \\u006bind is kind",
    raw: JSON.stringify(query([sel()])).replace('"kind":"query"', '"kind":"query","\\u006bind":"query"'),
    expect: schemaErr("type-mismatch", "kind"),
  },
  {
    name: "object in an id position",
    why: "an object without $ref is not an id",
    request: query([{ op: "get", table: "notes", id: { id: "n1" } }]),
    expect: schemaErr("type-mismatch", "ops[0].id", 0),
  },
  {
    name: "array in an id position",
    why: "an array is not an id",
    request: query([{ op: "get", table: "notes", id: ["n1"] }]),
    expect: schemaErr("type-mismatch", "ops[0].id", 0),
  },
];

function decodeFile(): VectorFile {
  const vectors = DECODE.map((c) => {
    const v: J = { name: c.name, why: c.why };
    const raw = c.raw ?? JSON.stringify(c.request);
    if (c.base64 !== undefined) v.base64 = c.base64;
    else v.raw = raw;
    if (c.pad !== undefined) v.pad = c.pad;
    if (c.expect.ok) {
      const decoded = decodeValue(JSON.parse(raw));
      if (Either.isLeft(decoded)) {
        throw new Error(`${c.name}: expected ok, got ${JSON.stringify(decoded.left)}`);
      }
      v.expect = { ok: true, canonical: canonical(decoded.right) };
    } else v.expect = c.expect;
    return v;
  });
  return {
    category: "decode",
    version: 1,
    description:
      "Request bytes -> accepted (with the canonical encoding of the decoded request) or an error.",
    vectors,
  };
}

// ---------------------------------------------------------------------------
// validation.json
// ---------------------------------------------------------------------------

type Mode = "insert" | "patch";
const val = (name: string, why: string, mode: Mode, value: unknown, expect: J): J => ({
  name,
  why,
  mode,
  value,
  expect,
});
const valid = (value: unknown) => ({ ok: true, value });
const MAX = Number.MAX_SAFE_INTEGER;
const full = (over: J) => ({ ...BODY, ...over });

const VALIDATION: J[] = [
  val("full body", "every kind at a valid value", "insert", BODY, valid(BODY)),
  val("integer max safe", "on point: 2^53-1", "insert", full({ rank: MAX }), valid(full({ rank: MAX }))),
  val(
    "integer 2^53",
    "off point: not a safe integer",
    "insert",
    full({ rank: MAX + 1 }),
    schemaErr("type-mismatch", "rank"),
  ),
  val("integer min safe", "on point: -(2^53-1)", "insert", full({ rank: -MAX }), valid(full({ rank: -MAX }))),
  val("integer -2^53", "off point", "insert", full({ rank: -MAX - 1 }), schemaErr("type-mismatch", "rank")),
  val("integer 1.5", "not integral", "insert", full({ rank: 1.5 }), schemaErr("type-mismatch", "rank")),
  val('integer "1"', "no string coercion", "insert", full({ rank: "1" }), schemaErr("type-mismatch", "rank")),
  val(
    "number 2^53",
    "a number may exceed the safe range",
    "insert",
    full({ score: 2 ** 53 }),
    valid(full({ score: 2 ** 53 })),
  ),
  val(
    "number max double",
    "finite",
    "insert",
    full({ score: Number.MAX_VALUE }),
    valid(full({ score: Number.MAX_VALUE })),
  ),
  val(
    "number as boolean",
    "no coercion",
    "insert",
    full({ score: true }),
    schemaErr("type-mismatch", "score"),
  ),
  val("empty string", "empty text is text", "insert", full({ title: "" }), valid(full({ title: "" }))),
  val("string with U+0000", "text storage contract (ADR 0005)", "insert", full({ title: "a\u0000b" }), {
    error: {
      tag: "SchemaError",
      status: 400,
      fields: { reason: "type-mismatch", path: "title", expected: "text without U+0000" },
    },
  }),
  val(
    "U+0000 alone",
    "on point: one NUL",
    "insert",
    full({ title: "\u0000" }),
    schemaErr("type-mismatch", "title"),
  ),
  val(
    "U+0001",
    "only U+0000 is refused",
    "insert",
    full({ title: "\u0001" }),
    valid(full({ title: "\u0001" })),
  ),
  val(
    "U+0000 in an array element",
    "the path names the element",
    "insert",
    full({ tags: ["ok", "b\u0000"] }),
    schemaErr("type-mismatch", "tags[1]"),
  ),
  val(
    "U+0000 in a nullable string",
    "storable applies to every string",
    "insert",
    full({ owner: "\u0000" }),
    schemaErr("type-mismatch", "owner"),
  ),
  val("enum member", "declared value", "insert", full({ tag: "a" }), valid(full({ tag: "a" }))),
  val("enum non-member", "undeclared value", "insert", full({ tag: "c" }), schemaErr("type-mismatch", "tag")),
  val(
    "enum case",
    "members are case-sensitive",
    "insert",
    full({ tag: "A" }),
    schemaErr("type-mismatch", "tag"),
  ),
  val(
    "nullable enum null",
    "nullable accepts null",
    "insert",
    full({ tag: null }),
    valid(full({ tag: null })),
  ),
  val(
    "non-nullable null",
    "null needs nullable",
    "insert",
    full({ title: null }),
    schemaErr("type-mismatch", "title"),
  ),
  val(
    "missing field",
    "a full body needs every field",
    "insert",
    without(BODY, "owner"),
    schemaErr("type-mismatch", "owner"),
  ),
  val("boolean false", "valid", "insert", full({ done: false }), valid(full({ done: false }))),
  val("boolean as 0", "no coercion", "insert", full({ done: 0 }), schemaErr("type-mismatch", "done")),
  val(
    "fixed array exact",
    "on point: length 3",
    "insert",
    full({ grid: [0, 0, 0] }),
    valid(full({ grid: [0, 0, 0] })),
  ),
  val(
    "fixed array short",
    "off point: length 2",
    "insert",
    full({ grid: [0, 0] }),
    schemaErr("type-mismatch", "grid"),
  ),
  val(
    "fixed array long",
    "off point: length 4",
    "insert",
    full({ grid: [0, 0, 0, 0] }),
    schemaErr("type-mismatch", "grid"),
  ),
  val(
    "array element type",
    "elements are validated",
    "insert",
    full({ grid: [0, "1", 0] }),
    schemaErr("type-mismatch", "grid[1]"),
  ),
  val(
    "unbounded array empty",
    "no length: any length",
    "insert",
    full({ tags: [] }),
    valid(full({ tags: [] })),
  ),
  val(
    "array as object",
    "an array is required",
    "insert",
    full({ tags: { 0: "a" } }),
    schemaErr("type-mismatch", "tags"),
  ),
  val(
    "unknown field",
    "unknown keys are rejected before any field",
    "insert",
    full({ colour: "red" }),
    schemaErr("unknown-field", "colour"),
  ),
  val(
    "metadata in a body",
    "_id is server-owned",
    "insert",
    full({ _id: "x" }),
    schemaErr("unknown-field", "_id"),
  ),
  val(
    "_version in a patch",
    "versions move only by +1 in SQL",
    "patch",
    { _version: 9 },
    schemaErr("unknown-field", "_version"),
  ),
  val("body not an object", "bodies are objects", "insert", ["a"], schemaErr("not-an-object", "")),
  val("body null", "bodies are objects", "patch", null, schemaErr("not-an-object", "")),
  val("empty patch", "a patch may change nothing (and still bumps _version)", "patch", {}, valid({})),
  val("partial patch", "absent keys are skipped", "patch", { rank: 3 }, valid({ rank: 3 })),
  val(
    "patch null into non-nullable",
    "partial still validates present keys",
    "patch",
    { rank: null },
    schemaErr("type-mismatch", "rank"),
  ),
  val(
    "patch U+0000",
    "the storage contract applies to patches",
    "patch",
    { title: "\u0000" },
    schemaErr("type-mismatch", "title"),
  ),
];

function validationFile(): VectorFile {
  return {
    category: "validation",
    version: 1,
    description:
      "A body for table `notes` of the vector schema -> the validated body or an error. insert = full body, patch = partial.",
    vectors: VALIDATION.map((v) => ({ ...v, fields: NOTES_FIELDS })),
  };
}

// ---------------------------------------------------------------------------
// schema.json
// ---------------------------------------------------------------------------

const tbl = (fields: J, extra: J = {}): J => ({ fields, storage: "columns", indexes: [], ...extra });
const one = (node: J) => tbl({ f: { node } });
const SCHEMA: { name: string; why: string; schema: unknown; expect: J }[] = [
  {
    name: "vector schema, columns",
    why: "the schema every other vector uses",
    schema: schemaJson("columns"),
    expect: ok,
  },
  {
    name: "vector schema, json",
    why: "the layout is part of the fingerprint",
    schema: schemaJson("json"),
    expect: ok,
  },
  { name: "empty schema", why: "no tables is a schema", schema: {}, expect: ok },
  {
    name: "reserved field",
    why: "_-prefixed names are metadata",
    schema: { t: tbl({ _x: { node: { kind: "string" } } }) },
    expect: schemaErr("reserved-field", "_x"),
  },
  {
    name: "reserved table _",
    why: "_-prefixed tables are the platform's",
    schema: { _t: tbl({}) },
    expect: schemaErr("reserved-table", "_t"),
  },
  {
    name: "reserved table sqlite_",
    why: "SQLite's prefix, any case",
    schema: { SQLite_t: tbl({}) },
    expect: schemaErr("reserved-table", "SQLite_t"),
  },
  {
    name: "field not an identifier",
    why: "names are interpolated, so only identifiers",
    schema: { t: tbl({ "1a": { node: { kind: "string" } } }) },
    expect: schemaErr("invalid-identifier", "1a"),
  },
  {
    name: "table not an identifier",
    why: "same rule for tables",
    schema: { "t-1": tbl({}) },
    expect: schemaErr("invalid-identifier", "t-1"),
  },
  {
    name: "enum member with U+0000",
    why: "no value of it could be stored",
    schema: { t: one({ kind: "enum", values: ["a\u0000"] }) },
    expect: schemaErr("type-mismatch", "f"),
  },
  {
    name: "enum without values",
    why: "values is required",
    schema: { t: one({ kind: "enum" }) },
    expect: schemaErr("type-mismatch", "schema.t.fields.f.node.values"),
  },
  {
    name: "enum with no values",
    why: "IN () is not SQL: the DDL refuses",
    schema: { t: one({ kind: "enum", values: [] }) },
    expect: compileErr({ reason: "empty-enum", field: "f" }),
  },
  {
    name: "enum duplicate values",
    why: "members are distinct",
    schema: { t: one({ kind: "enum", values: ["a", "a"] }) },
    expect: schemaErr("type-mismatch", "schema.t.fields.f.node.values"),
  },
  {
    name: "array without item",
    why: "arrays declare their element",
    schema: { t: one({ kind: "array" }) },
    expect: schemaErr("type-mismatch", "schema.t.fields.f.node.item"),
  },
  {
    name: "length on a scalar",
    why: "only arrays have a length",
    schema: { t: one({ kind: "string", length: 2 }) },
    expect: schemaErr("type-mismatch", "schema.t.fields.f.node.length"),
  },
  {
    name: "unknown node kind",
    why: "the kinds are closed",
    schema: { t: one({ kind: "object" }) },
    expect: schemaErr("type-mismatch", "schema.t.fields.f.node.kind"),
  },
  {
    name: "unknown node member",
    why: "strict decode",
    schema: { t: one({ kind: "string", default: "x" }) },
    expect: schemaErr("unknown-field", "schema.t.fields.f.node.default"),
  },
  {
    name: "unknown storage",
    why: "columns | json",
    schema: { t: tbl({}, { storage: "rows" }) },
    expect: schemaErr("type-mismatch", "schema.t.storage"),
  },
  {
    name: "index on an array",
    why: "arrays are JSON text; not indexable",
    schema: {
      t: tbl({ a: { node: { kind: "array", item: { kind: "string" } } } }, {
        indexes: [{ name: "by_a", fields: ["a"] }],
      }),
    },
    expect: schemaErr("unindexable-field", "a"),
  },
  {
    name: "index on an unknown field",
    why: "indexes cover declared fields",
    schema: { t: tbl({}, { indexes: [{ name: "by_a", fields: ["a"] }] }) },
    expect: schemaErr("unindexable-field", "a"),
  },
  {
    name: "empty index",
    why: "an index covers one or more fields",
    schema: { t: tbl({}, { indexes: [{ name: "by", fields: [] }] }) },
    expect: schemaErr("empty-index", "by"),
  },
  {
    name: "duplicate index",
    why: "index names are unique per table",
    schema: {
      t: tbl({ a: { node: { kind: "string" } } }, {
        indexes: [{ name: "by", fields: ["a"] }, { name: "by", fields: ["a"] }],
      }),
    },
    expect: schemaErr("duplicate-index", "by"),
  },
];

async function schemaFile(): Promise<VectorFile> {
  const vectors: J[] = [];
  for (const c of SCHEMA) {
    const v: J = { name: c.name, why: c.why, schema: c.schema };
    if (c.expect.ok) {
      const text = fingerprintText(schemaOf(c.schema));
      v.expect = { ok: true, text, fingerprint: await Effect.runPromise(sha256Fingerprint(text)) };
    } else v.expect = c.expect;
    vectors.push(v);
  }
  return {
    category: "schema",
    version: 1,
    description:
      "Registered schema JSON -> its fingerprint text and sha256 fingerprint, or the declaration error.",
    vectors,
  };
}

// ---------------------------------------------------------------------------
// compile.json (golden: core/compile is the oracle, ADR 0010)
// ---------------------------------------------------------------------------

type CompileCase =
  | { name: string; why: string; kind: "ddl" }
  | { name: string; why: string; kind: "select"; plan: Omit<SelectPlan, "kind"> & { table?: string } }
  | {
    name: string;
    why: string;
    kind: "insert";
    table: string;
    value: unknown;
    meta: { _id: string; _creationTime: number };
  }
  | { name: string; why: string; kind: "replace"; table: string; id: string; value: unknown }
  | { name: string; why: string; kind: "delete"; table: string; id: string };
const p = (over: Partial<SelectPlan>): Omit<SelectPlan, "kind"> => ({
  table: "notes",
  filters: [],
  limit: 10,
  ...over,
});
const f = (field: string, op: string, value: unknown) =>
  ({ field, op, value }) as SelectPlan["filters"][number];

const COMPILE: CompileCase[] = [
  { name: "DDL", why: "every column type and CHECK, plus both indexes", kind: "ddl" },
  { name: "select all", why: "default order is _id", kind: "select", plan: p({}) },
  {
    name: "eq string",
    why: "a bound text value",
    kind: "select",
    plan: p({ filters: [f("title", "eq", "O'Reilly'); --")] }),
  },
  {
    name: "eq null",
    why: "IS NULL, nothing bound",
    kind: "select",
    plan: p({ filters: [f("owner", "eq", null)] }),
  },
  {
    name: "eq boolean",
    why: "booleans bind as 0/1",
    kind: "select",
    plan: p({ filters: [f("done", "eq", true)] }),
  },
  { name: "eq enum", why: "enums bind as text", kind: "select", plan: p({ filters: [f("tag", "eq", "a")] }) },
  {
    name: "gt integer",
    why: "json layout reads integers uncast",
    kind: "select",
    plan: p({ filters: [f("rank", "gt", 1)] }),
  },
  {
    name: "gt number",
    why: "json layout reads numbers through CAST AS REAL (item 30)",
    kind: "select",
    plan: p({ filters: [f("score", "gt", 0.5)] }),
  },
  {
    name: "eq _id",
    why: "metadata is a real column in both layouts",
    kind: "select",
    plan: p({ filters: [f("_id", "eq", "n1")], limit: 1 }),
  },
  {
    name: "two filters",
    why: "AND, binds in filter order",
    kind: "select",
    plan: p({ filters: [f("rank", "gt", 0), f("tag", "eq", "b")] }),
  },
  {
    name: "order asc",
    why: "NULLS FIRST and the _id tie-break",
    kind: "select",
    plan: p({ order: { field: "owner", direction: "asc" } }),
  },
  {
    name: "order desc",
    why: "NULLS LAST and the _id tie-break",
    kind: "select",
    plan: p({ order: { field: "score", direction: "desc" } }),
  },
  {
    name: "order by _creationTime",
    why: "metadata ordering",
    kind: "select",
    plan: p({ order: { field: "_creationTime", direction: "desc" } }),
  },
  {
    name: "count",
    why: "no LIMIT, no ORDER",
    kind: "select",
    plan: p({ aggregate: { kind: "count" }, filters: [f("done", "eq", false)] }),
  },
  {
    name: "count drops order",
    why: "normalise removes an aggregate's order",
    kind: "select",
    plan: p({ aggregate: { kind: "count" }, order: { field: "rank", direction: "asc" } }),
  },
  {
    name: "countBy",
    why: "GROUP BY 1, value ASC NULLS FIRST, LIMIT on groups",
    kind: "select",
    plan: p({ aggregate: { kind: "countBy", field: "tag" }, limit: 5 }),
  },
  { name: "limit 1000", why: "on point of MAX_LIMIT", kind: "select", plan: p({ limit: 1000 }) },
  { name: "limit 1", why: "on point of MIN_LIMIT", kind: "select", plan: p({ limit: 1 }) },
  { name: "limit 1001", why: "off point", kind: "select", plan: p({ limit: 1001 }) },
  { name: "limit 0", why: "off point", kind: "select", plan: p({ limit: 0 }) },
  { name: "limit 1.5", why: "integral only", kind: "select", plan: p({ limit: 1.5 }) },
  { name: "unknown table", why: "unknown-collection", kind: "select", plan: p({ table: "nope" }) },
  {
    name: "unknown field",
    why: "unknown-field",
    kind: "select",
    plan: p({ filters: [f("colour", "eq", "red")] }),
  },
  {
    name: "array predicate",
    why: "arrays are JSON text: refused",
    kind: "select",
    plan: p({ filters: [f("tags", "eq", ["x"])] }),
  },
  {
    name: "order by array",
    why: "refused like a predicate",
    kind: "select",
    plan: p({ order: { field: "grid", direction: "asc" } }),
  },
  {
    name: "gt on string",
    why: "gt is numeric only",
    kind: "select",
    plan: p({ filters: [f("title", "gt", "a")] }),
  },
  {
    name: "gt null",
    why: "gt needs a value",
    kind: "select",
    plan: p({ filters: [f("owner", "gt", null)] }),
  },
  {
    name: "filter value type",
    why: "values are validated against the node: SchemaError",
    kind: "select",
    plan: p({ filters: [f("rank", "eq", "1")] }),
  },
  {
    name: "unsupported operator",
    why: "the compiler's own check, behind decode's",
    kind: "select",
    plan: p({ filters: [f("rank", "lt", 1)] }),
  },
  {
    name: "invalid ordering",
    why: "the compiler's own check, behind decode's",
    kind: "select",
    plan: p({ order: { field: "rank", direction: "ASC" as "asc" } }),
  },
  {
    name: "insert",
    why: "_version starts at 1; arrays JSON; booleans 0/1",
    kind: "insert",
    table: "notes",
    value: BODY,
    meta: { _id: "s1", _creationTime: 1000 },
  },
  {
    name: "insert invalid",
    why: "validated before compiled",
    kind: "insert",
    table: "notes",
    value: { title: 1 },
    meta: { _id: "s1", _creationTime: 1000 },
  },
  {
    name: "replace",
    why: "whole body, _version + 1 in SQL",
    kind: "replace",
    table: "notes",
    id: "n1",
    value: BODY,
  },
  { name: "delete", why: "by id", kind: "delete", table: "notes", id: "n1" },
  {
    name: "delete unknown table",
    why: "a typo fails rather than no-ops",
    kind: "delete",
    table: "nope",
    id: "n1",
  },
];

function wireOf(left: { _tag: string }): J {
  const { _tag, ...fields } = left as unknown as J & { _tag: string };
  return {
    error: {
      tag: _tag,
      fields: Object.fromEntries(Object.entries(fields).filter(([, v]) => v !== undefined)),
    },
  };
}

function compileFile(): VectorFile {
  const vectors: J[] = [];
  for (const storage of ["columns", "json"] as const) {
    const json = schemaJson(storage);
    const schema = schemaOf(json);
    for (const c of COMPILE) {
      const v: J = { name: `${c.name} [${storage}]`, why: c.why, schema: json, kind: c.kind };
      let out: Either.Either<unknown, { _tag: string }>;
      switch (c.kind) {
        case "ddl":
          out = Either.map(compileDDL(schema), (statements) => ({ statements }));
          break;
        case "select": {
          const plan = { kind: "select", ...c.plan } as SelectPlan;
          v.plan = plan;
          out = Either.map(compileSelect(schema, plan, "sqlite"), (sql) => ({ sql }));
          break;
        }
        case "insert":
          Object.assign(v, { table: c.table, value: c.value, meta: c.meta });
          out = Either.map(compileInsert(schema, c.table, c.value, c.meta), (sql) => ({ sql }));
          break;
        case "replace":
          Object.assign(v, { table: c.table, id: c.id, value: c.value });
          out = Either.map(compileReplace(schema, c.table, c.id, c.value), (sql) => ({ sql }));
          break;
        case "delete":
          Object.assign(v, { table: c.table, id: c.id });
          out = Either.map(compileDelete(schema, c.table, c.id), (sql) => ({ sql }));
          break;
      }
      v.expect = Either.isRight(out) ? out.right : wireOf(out.left);
      vectors.push(v);
    }
  }
  return {
    category: "compile",
    version: 1,
    description:
      "Schema + plan or write -> SQLite SQL text and bind values, or the compile error. Golden from core/compile.",
    vectors,
  };
}

// ---------------------------------------------------------------------------
// execute.json
// ---------------------------------------------------------------------------

type Step = { clock?: number; serverIds?: string[]; request: J; expect: J };
type Scenario = { name: string; why: string; seed?: J; steps: Step[]; final?: J };

const m = (ops: unknown[], requestId = "r1"): J => ({
  v: 1,
  db: { tenant: "acme", app: "notes" },
  kind: "mutation",
  requestId,
  schema: FP,
  ops,
});
const q = (ops: unknown[], over: J = {}): J => ({
  v: 1,
  db: { tenant: "acme", app: "notes" },
  kind: "query",
  schema: FP,
  ops,
  ...over,
});
const res = (...result: unknown[]) => ({ result });
const getN = (id: unknown, table = "notes") => ({ op: "get", table, id });
const ins = (value: J, id?: string, table = "notes") => ({
  op: "insert",
  table,
  value,
  ...(id ? { id } : {}),
});
const srv = (id: string, body: J, creation = 1000) => ({
  _id: id,
  _creationTime: creation,
  _version: 1,
  ...body,
});
const NEW = srv("s1", BODY);
const s = (over: J = {}) => ({ op: "select", table: "notes", filters: [], limit: 10, ...over });
const single = (step: Omit<Step, "expect">, expect: J, final?: J): Pick<Scenario, "steps" | "final"> => ({
  steps: [{ ...step, expect }],
  final,
});
const UNCHANGED = SEED;
const conflict = (fields: J, op: number) => err("ConflictError", 409, fields, op);

const SCENARIOS: Scenario[] = [
  // insert
  {
    name: "insert with a server id",
    why: "no client id: the server draws one",
    seed: SEED,
    ...single({ serverIds: ["s1"], request: m([ins(BODY)]) }, res("s1"), { notes: [N1, N2, N3, NEW] }),
  },
  {
    name: "insert with a client id",
    why: "the client's UUIDv7 is the _id",
    seed: SEED,
    ...single({ request: m([ins(BODY, U1)]) }, res(U1), { notes: [srv(U1, BODY), N1, N2, N3] }),
  },
  {
    name: "insert with a taken client id",
    why: "an insert implies expect exists:false: ConflictError, not a storage error",
    seed: { notes: [...SEED.notes, note(U1, 50, 1, {})] },
    ...single(
      { request: m([ins(BODY, U1)]) },
      conflict({ table: "notes", id: U1, check: "exists", expected: false, actual: true }, 0),
      {
        notes: [note(U1, 50, 1, {}), N1, N2, N3],
      },
    ),
  },
  {
    name: "one clock reading per request",
    why: "every insert in a request shares _creationTime; server ids are drawn in op order",
    seed: SEED,
    ...single(
      {
        clock: 5000,
        serverIds: ["s1", "s2"],
        request: m([ins(BODY), ins(BODY, U1), ins({ ...BODY, rank: 8 })]),
      },
      res("s1", U1, "s2"),
      {
        notes: [
          srv(U1, BODY, 5000),
          N1,
          N2,
          N3,
          srv("s1", BODY, 5000),
          srv("s2", { ...BODY, rank: 8 }, 5000),
        ],
      },
    ),
  },
  {
    name: "same client id twice in one request",
    why: "the second insert sees the first",
    seed: SEED,
    ...single(
      { request: m([ins(BODY, U1), ins(BODY, U1)]) },
      conflict({ id: U1, check: "exists" }, 1),
      UNCHANGED,
    ),
  },
  {
    name: "insert into an unknown table",
    why: "CompileError, op index set",
    seed: SEED,
    ...single(
      { request: m([ins(BODY, undefined, "nope")]) },
      compileErr({ reason: "unknown-collection", table: "nope" }, 0),
      UNCHANGED,
    ),
  },
  {
    name: "insert with _id in the body",
    why: "server-owned fields are not body fields",
    seed: SEED,
    ...single({ request: m([ins({ ...BODY, _id: "x" })]) }, schemaErr("unknown-field", "_id", 0), UNCHANGED),
  },
  {
    name: "insert with U+0000",
    why: "text storage contract",
    seed: SEED,
    ...single(
      { request: m([ins({ ...BODY, title: "a\u0000" })]) },
      schemaErr("type-mismatch", "title", 0),
      UNCHANGED,
    ),
  },
  {
    name: "insert at the integer bounds",
    why: "2^53-1 stores and reads back exactly; gt sees it",
    ...single(
      {
        serverIds: ["s1"],
        request: m([
          ins({ ...BODY, rank: MAX }),
          s({ filters: [{ field: "rank", op: "gt", value: MAX - 1 }] }),
        ]),
      },
      res("s1", [srv("s1", { ...BODY, rank: MAX })]),
      { notes: [srv("s1", { ...BODY, rank: MAX })] },
    ),
  },
  {
    name: "number beyond 2^53",
    why: "an integral double above 2^53 round-trips and matches eq in both layouts (item 30)",
    ...single(
      {
        serverIds: ["s1"],
        request: m([
          ins({ ...BODY, score: 2 ** 60 }),
          s({ filters: [{ field: "score", op: "eq", value: 2 ** 60 }] }),
        ]),
      },
      res("s1", [srv("s1", { ...BODY, score: 2 ** 60 })]),
      { notes: [srv("s1", { ...BODY, score: 2 ** 60 })] },
    ),
  },
  // get and select
  {
    name: "get existing and missing",
    why: "a document or null",
    seed: SEED,
    ...single({ request: q([getN("n2"), getN("zz")]) }, res(N2, null), UNCHANGED),
  },
  {
    name: "select all",
    why: "ordered by _id",
    seed: SEED,
    ...single({ request: q([s()]) }, res([N1, N2, N3]), UNCHANGED),
  },
  {
    name: "order ties break on _id",
    why: "n2 and n3 share rank 2: _id ASC decides",
    seed: SEED,
    ...single(
      { request: q([s({ order: { field: "rank", direction: "desc" } })]) },
      res([N2, N3, N1]),
      UNCHANGED,
    ),
  },
  {
    name: "nulls first ascending",
    why: "NULLS FIRST on asc",
    seed: SEED,
    ...single(
      { request: q([s({ order: { field: "owner", direction: "asc" } })]) },
      res([N1, N3, N2]),
      UNCHANGED,
    ),
  },
  {
    name: "nulls last descending",
    why: "NULLS LAST on desc",
    seed: SEED,
    ...single(
      { request: q([s({ order: { field: "owner", direction: "desc" } })]) },
      res([N2, N3, N1]),
      UNCHANGED,
    ),
  },
  {
    name: "order by number",
    why: "numbers order numerically in both layouts",
    seed: SEED,
    ...single(
      { request: q([s({ order: { field: "score", direction: "asc" } })]) },
      res([N3, N1, N2]),
      UNCHANGED,
    ),
  },
  {
    name: "limit",
    why: "limit cuts after ordering",
    seed: SEED,
    ...single(
      { request: q([s({ order: { field: "score", direction: "desc" }, limit: 2 })]) },
      res([N2, N1]),
      UNCHANGED,
    ),
  },
  {
    name: "eq boolean",
    why: "booleans compare as 0/1 in both layouts",
    seed: SEED,
    ...single(
      { request: q([s({ filters: [{ field: "done", op: "eq", value: true }] })]) },
      res([N2]),
      UNCHANGED,
    ),
  },
  {
    name: "eq null",
    why: "IS NULL",
    seed: SEED,
    ...single(
      { request: q([s({ filters: [{ field: "tag", op: "eq", value: null }] })]) },
      res([N2]),
      UNCHANGED,
    ),
  },
  {
    name: "gt number",
    why: "score > 0",
    seed: SEED,
    ...single(
      { request: q([s({ filters: [{ field: "score", op: "gt", value: 0 }] })]) },
      res([N1, N2]),
      UNCHANGED,
    ),
  },
  {
    name: "count",
    why: "all matching rows, no limit",
    seed: SEED,
    ...single({ request: q([s({ aggregate: { kind: "count" }, limit: 1 })]) }, res(3), UNCHANGED),
  },
  {
    name: "countBy",
    why: "groups ordered by value, null first",
    seed: SEED,
    ...single(
      { request: q([s({ aggregate: { kind: "countBy", field: "tag" } })]) },
      res([{ value: null, count: 1 }, { value: "a", count: 1 }, { value: "b", count: 1 }]),
      UNCHANGED,
    ),
  },
  {
    name: "countBy limit",
    why: "limit caps groups",
    seed: SEED,
    ...single(
      { request: q([s({ aggregate: { kind: "countBy", field: "rank" }, limit: 1 })]) },
      res([{ value: 1, count: 1 }]),
      UNCHANGED,
    ),
  },
  {
    name: "select limit 0",
    why: "CompileError invalid-limit at execution",
    seed: SEED,
    ...single({ request: q([s({ limit: 0 })]) }, compileErr({ reason: "invalid-limit" }, 0), UNCHANGED),
  },
  {
    name: "gt on a string",
    why: "CompileError gt-requires-numeric",
    seed: SEED,
    ...single(
      { request: q([s({ filters: [{ field: "title", op: "gt", value: "a" }] })]) },
      compileErr({ reason: "gt-requires-numeric", field: "title" }, 0),
      UNCHANGED,
    ),
  },
  {
    name: "filter value U+0000",
    why: "a NUL never reaches SQLite, even as a filter",
    seed: SEED,
    ...single(
      { request: q([s({ filters: [{ field: "title", op: "eq", value: "a\u0000" }] })]) },
      schemaErr("type-mismatch", "title", 0),
      UNCHANGED,
    ),
  },
  {
    name: "query ignores requestId",
    why: "reads are never deduplicated",
    seed: SEED,
    steps: [{ serverIds: ["s1"], request: m([ins(BODY)]), expect: res("s1") }, {
      request: q([s({ aggregate: { kind: "count" } })], { requestId: "r1" }),
      expect: res(4),
    }],
    final: { notes: [N1, N2, N3, NEW], requests: [{ request_id: "r1", applied_ms: 1000, result: ["s1"] }] },
  },
  // patch
  {
    name: "patch",
    why: "partial merge; result is the new _version; _creationTime unchanged",
    seed: SEED,
    ...single(
      { request: m([{ op: "patch", table: "notes", id: "n2", value: { title: "beta2", owner: null } }]) },
      res({ _version: 4 }),
      { notes: [N1, { ...N2, title: "beta2", owner: null, _version: 4 }, N3] },
    ),
  },
  {
    name: "empty patch",
    why: "still bumps _version",
    seed: SEED,
    ...single({ request: m([{ op: "patch", table: "notes", id: "n1", value: {} }]) }, res({ _version: 2 }), {
      notes: [{ ...N1, _version: 2 }, N2, N3],
    }),
  },
  {
    name: "patch missing",
    why: "NotFoundError 404",
    seed: SEED,
    ...single(
      { request: m([{ op: "patch", table: "notes", id: "zz", value: {} }]) },
      err("NotFoundError", 404, { table: "notes", id: "zz" }, 0),
      UNCHANGED,
    ),
  },
  {
    name: "patch invalid value",
    why: "validated with partial = true",
    seed: SEED,
    ...single(
      { request: m([{ op: "patch", table: "notes", id: "n1", value: { grid: [1] } }]) },
      schemaErr("type-mismatch", "grid", 0),
      UNCHANGED,
    ),
  },
  {
    name: "patch expect version pass",
    why: "version equal",
    seed: SEED,
    ...single(
      { request: m([{ op: "patch", table: "notes", id: "n2", value: { rank: 5 }, expect: { version: 3 } }]) },
      res({ _version: 4 }),
      { notes: [N1, { ...N2, rank: 5, _version: 4 }, N3] },
    ),
  },
  {
    name: "patch expect version fail",
    why: "ConflictError with both versions",
    seed: SEED,
    ...single(
      { request: m([{ op: "patch", table: "notes", id: "n2", value: { rank: 5 }, expect: { version: 2 } }]) },
      conflict({ table: "notes", id: "n2", check: "version", expected: 2, actual: 3 }, 0),
      UNCHANGED,
    ),
  },
  {
    name: "patch expect version on missing",
    why: "version implies existence: exists check fails first",
    seed: SEED,
    ...single(
      { request: m([{ op: "patch", table: "notes", id: "zz", value: {}, expect: { version: 1 } }]) },
      conflict({ check: "exists", expected: true, actual: false }, 0),
      UNCHANGED,
    ),
  },
  {
    name: "patch expect fields pass",
    why: "scalar equality",
    seed: SEED,
    ...single(
      {
        request: m([{
          op: "patch",
          table: "notes",
          id: "n1",
          value: { done: true },
          expect: { fields: { done: false, tag: "a", owner: null } },
        }]),
      },
      res({ _version: 2 }),
      { notes: [{ ...N1, done: true, _version: 2 }, N2, N3] },
    ),
  },
  {
    name: "patch expect fields fail",
    why: "reports the expected and the actual values of the named fields",
    seed: SEED,
    ...single(
      {
        request: m([{
          op: "patch",
          table: "notes",
          id: "n1",
          value: {},
          expect: { fields: { rank: 1, title: "x" } },
        }]),
      },
      conflict(
        { check: "fields", expected: { rank: 1, title: "x" }, actual: { rank: 1, title: "alpha" } },
        0,
      ),
      UNCHANGED,
    ),
  },
  {
    name: "expect fields unknown field",
    why: "CompileError unknown-field",
    seed: SEED,
    ...single(
      { request: m([{ op: "assert", table: "notes", id: "n1", expect: { fields: { colour: "red" } } }]) },
      compileErr({ reason: "unknown-field", field: "colour" }, 0),
      UNCHANGED,
    ),
  },
  {
    name: "expect fields metadata",
    why: "_version is checked with expect.version, not fields",
    seed: SEED,
    ...single(
      { request: m([{ op: "assert", table: "notes", id: "n1", expect: { fields: { _version: 1 } } }]) },
      compileErr({ reason: "unknown-field", field: "_version" }, 0),
      UNCHANGED,
    ),
  },
  {
    name: "expect fields array",
    why: "arrays are not comparable",
    seed: SEED,
    ...single(
      { request: m([{ op: "assert", table: "notes", id: "n1", expect: { fields: { tags: ["x"] } } }]) },
      compileErr({ reason: "array-predicate", field: "tags" }, 0),
      UNCHANGED,
    ),
  },
  {
    name: "expect fields wrong type",
    why: "values are validated against the node",
    seed: SEED,
    ...single(
      { request: m([{ op: "assert", table: "notes", id: "n1", expect: { fields: { rank: "1" } } }]) },
      schemaErr("type-mismatch", "rank", 0),
      UNCHANGED,
    ),
  },
  // delete and assert
  {
    name: "delete existing",
    why: "{deleted: true}",
    seed: SEED,
    ...single({ request: m([{ op: "delete", table: "notes", id: "n1" }]) }, res({ deleted: true }), {
      notes: [N2, N3],
    }),
  },
  {
    name: "delete missing",
    why: "not an error: {deleted: false}",
    seed: SEED,
    ...single(
      { request: m([{ op: "delete", table: "notes", id: "zz" }]) },
      res({ deleted: false }),
      UNCHANGED,
    ),
  },
  {
    name: "delete missing that must exist",
    why: "expect exists:true turns it into a conflict",
    seed: SEED,
    ...single(
      { request: m([{ op: "delete", table: "notes", id: "zz", expect: { exists: true } }]) },
      conflict({ check: "exists", expected: true, actual: false }, 0),
      UNCHANGED,
    ),
  },
  {
    name: "delete with version",
    why: "guarded delete",
    seed: SEED,
    ...single(
      { request: m([{ op: "delete", table: "notes", id: "n2", expect: { version: 3 } }]) },
      res({ deleted: true }),
      { notes: [N1, N3] },
    ),
  },
  {
    name: "assert pass",
    why: "result null, no write",
    seed: SEED,
    ...single(
      {
        request: m([{
          op: "assert",
          table: "notes",
          id: "n2",
          expect: { exists: true, version: 3, fields: { owner: "bob" } },
        }]),
      },
      res(null),
      UNCHANGED,
    ),
  },
  {
    name: "assert absence pass",
    why: "exists:false on a missing document",
    seed: SEED,
    ...single(
      { request: m([{ op: "assert", table: "notes", id: "zz", expect: { exists: false } }]) },
      res(null),
      UNCHANGED,
    ),
  },
  {
    name: "assert absence fail",
    why: "exists:false on a present document",
    seed: SEED,
    ...single(
      { request: m([{ op: "assert", table: "notes", id: "n1", expect: { exists: false } }]) },
      conflict({ table: "notes", id: "n1", check: "exists", expected: false, actual: true }, 0),
      UNCHANGED,
    ),
  },
  {
    name: "assert unknown table",
    why: "the table is looked up first",
    seed: SEED,
    ...single(
      { request: m([{ op: "assert", table: "nope", id: "n1", expect: { exists: true } }]) },
      compileErr({ reason: "unknown-collection" }, 0),
      UNCHANGED,
    ),
  },
  // $ref
  {
    name: "$ref chain",
    why: "insert a list, insert its entry by reference, read both back",
    steps: [{
      serverIds: ["L", "E"],
      request: m([
        ins({ name: "groceries" }, undefined, "lists"),
        ins({ listId: { $ref: 0 }, n: 1 }, undefined, "entries"),
        getN({ $ref: 0 }, "lists"),
        {
          op: "select",
          table: "entries",
          filters: [{ field: "listId", op: "eq", value: { $ref: 0 } }],
          limit: 10,
        },
      ]),
      expect: res("L", "E", srv("L", { name: "groceries" }), [srv("E", { listId: "L", n: 1 })]),
    }],
    final: { lists: [srv("L", { name: "groceries" })], entries: [srv("E", { listId: "L", n: 1 })] },
  },
  {
    name: "$ref as a patch and assert id",
    why: "ids resolve in every id position",
    ...single(
      {
        request: m([ins({ name: "a" }, U2, "lists"), {
          op: "patch",
          table: "lists",
          id: { $ref: 0 },
          value: { name: "b" },
        }, { op: "assert", table: "lists", id: { $ref: 0 }, expect: { version: 2, fields: { name: "b" } } }]),
      },
      res(U2, { _version: 2 }, null),
      { lists: [{ ...srv(U2, { name: "b" }), _version: 2 }] },
    ),
  },
  // atomicity and visibility
  {
    name: "selects see earlier writes",
    why: "ops run in order in one transaction",
    seed: SEED,
    ...single(
      {
        serverIds: ["s1"],
        request: m([
          ins(BODY),
          { op: "patch", table: "notes", id: "n1", value: { rank: 9 } },
          s({ filters: [{ field: "rank", op: "gt", value: 6 }] }),
          { op: "delete", table: "notes", id: "n3" },
          s({ aggregate: { kind: "count" } }),
        ]),
      },
      res("s1", { _version: 2 }, [{ ...N1, rank: 9, _version: 2 }, NEW], { deleted: true }, 3),
      { notes: [{ ...N1, rank: 9, _version: 2 }, N2, NEW] },
    ),
  },
  {
    name: "delete then reinsert the same id",
    why: "a delete frees the id for a later insert in the same request",
    seed: { notes: [...SEED.notes, note(U1, 50, 4, {})] },
    ...single(
      { clock: 7000, request: m([{ op: "delete", table: "notes", id: U1 }, ins(BODY, U1), getN(U1)]) },
      res({ deleted: true }, U1, srv(U1, BODY, 7000)),
      {
        notes: [srv(U1, BODY, 7000), N1, N2, N3],
      },
    ),
  },
  {
    name: "failed mutation leaves no trace",
    why: "a late failure rolls back earlier ops and the request id; a retry with the same id applies",
    seed: SEED,
    steps: [
      {
        serverIds: ["s1"],
        request: m([ins(BODY), { op: "patch", table: "notes", id: "n1", value: { rank: 4 } }, {
          op: "patch",
          table: "notes",
          id: "zz",
          value: {},
        }]),
        expect: err("NotFoundError", 404, { id: "zz" }, 2),
      },
      { request: q([s({ aggregate: { kind: "count" } }), getN("n1")]), expect: res(3, N1) },
      { serverIds: ["s2"], request: m([ins(BODY)]), expect: res("s2") },
    ],
    final: {
      notes: [N1, N2, N3, srv("s2", BODY)],
      requests: [{ request_id: "r1", applied_ms: 1000, result: ["s2"] }],
    },
  },
  {
    name: "failed precondition after a write",
    why: "preconditions anywhere in the list guard the whole request",
    seed: SEED,
    ...single(
      {
        request: m([{ op: "delete", table: "notes", id: "n1" }, {
          op: "assert",
          table: "notes",
          id: "n2",
          expect: { version: 1 },
        }]),
      },
      conflict({ check: "version", expected: 1, actual: 3 }, 1),
      { ...UNCHANGED, requests: [] },
    ),
  },
  // request ids
  {
    name: "duplicate request id",
    why: "answers the first result with duplicate:true and runs nothing, even with a different payload",
    seed: SEED,
    steps: [
      {
        serverIds: ["s1"],
        request: m([ins(BODY), { op: "delete", table: "notes", id: "n3" }]),
        expect: res("s1", { deleted: true }),
      },
      {
        request: m([ins(BODY)]),
        expect: { result: ["s1", { deleted: true }], duplicate: true },
      },
      { serverIds: ["s3"], request: m([ins(BODY)], "r2"), expect: res("s3") },
    ],
    final: {
      notes: [N1, N2, NEW, srv("s3", BODY)],
      requests: [
        { request_id: "r1", applied_ms: 1000, result: ["s1", { deleted: true }] },
        { request_id: "r2", applied_ms: 1000, result: ["s3"] },
      ],
    },
  },
  {
    name: "duplicate of a read-only mutation",
    why: "a mutation of only reads is still recorded and answered from the record",
    seed: SEED,
    steps: [
      { request: m([getN("n1")]), expect: res(N1) },
      {
        request: m([{ op: "patch", table: "notes", id: "n1", value: { rank: 5 } }]),
        expect: { result: [N1], duplicate: true },
      },
    ],
    final: { ...UNCHANGED, requests: [{ request_id: "r1", applied_ms: 1000, result: [N1] }] },
  },
  {
    name: "duplicate whose new payload is invalid",
    why:
      "a duplicate's operations are decoded but never validated: an unknown table still gets the first answer",
    seed: SEED,
    steps: [
      {
        clock: 2000,
        request: m([{ op: "delete", table: "notes", id: "n1" }]),
        expect: res({ deleted: true }),
      },
      {
        clock: 3000,
        request: m([ins(BODY, undefined, "nope")]),
        expect: { result: [{ deleted: true }], duplicate: true },
      },
    ],
    final: {
      notes: [N2, N3],
      requests: [{ request_id: "r1", applied_ms: 2000, result: [{ deleted: true }] }],
    },
  },
  {
    name: "expect fields wrong type on a missing document",
    why: "expect.fields values are validated before existence is checked",
    seed: SEED,
    ...single(
      { request: m([{ op: "assert", table: "notes", id: "zz", expect: { fields: { rank: "1" } } }]) },
      schemaErr("type-mismatch", "rank", 0),
      UNCHANGED,
    ),
  },
  {
    name: "invalid patch body on a missing document",
    why: "the body is validated before the document is loaded: SchemaError, not NotFoundError",
    seed: SEED,
    ...single(
      { request: m([{ op: "patch", table: "notes", id: "zz", value: { grid: [1] } }]) },
      schemaErr("type-mismatch", "grid", 0),
      UNCHANGED,
    ),
  },
  {
    name: "version and fields both failing",
    why: "checks run exists, version, fields: version is reported",
    seed: SEED,
    ...single(
      {
        request: m([{
          op: "patch",
          table: "notes",
          id: "n2",
          value: {},
          expect: { version: 2, fields: { title: "x" } },
        }]),
      },
      conflict({ check: "version", expected: 2, actual: 3 }, 0),
      UNCHANGED,
    ),
  },
  // envelope at execution
  {
    name: "schema fingerprint mismatch",
    why: "SchemaDriftError 409, no op index, nothing runs",
    seed: SEED,
    ...single(
      { request: { ...m([ins(BODY)]), schema: ANY_FP } },
      err("SchemaDriftError", 409, { actual: ANY_FP }),
    ),
    final: UNCHANGED,
  },
];

/** `final.requests[].result` → `result_hex`: version byte 0x02, then the canonical JSON of the result. */
function finalOf(final: J): J {
  if (!final.requests) return final;
  const requests = (final.requests as J[]).map(({ result, ...row }) => ({
    ...row,
    result_hex: "02" +
      Array.from(new TextEncoder().encode(canonical(result)), (b) => b.toString(16).padStart(2, "0")).join(
        "",
      ),
  }));
  return { ...final, requests };
}

async function executeFile(): Promise<VectorFile> {
  const vectors: J[] = [];
  for (const storage of ["columns", "json"] as const) {
    const json = schemaJson(storage);
    const fingerprint = await Effect.runPromise(sha256Fingerprint(fingerprintText(schemaOf(json))));
    for (const c of SCENARIOS) {
      const steps = c.steps.map((step) => ({
        clock: step.clock ?? 1000,
        serverIds: step.serverIds ?? [],
        request: step.request.schema === FP ? { ...step.request, schema: fingerprint } : step.request,
        expect: step.expect,
      }));
      vectors.push({
        name: `${c.name} [${storage}]`,
        why: c.why,
        schema: json,
        fingerprint,
        seed: c.seed ?? {},
        steps,
        final: finalOf(c.final ?? {}),
      });
    }
  }
  return {
    category: "execute",
    version: 1,
    description:
      "Schema + seed rows, then requests under a fixed clock and server-id sequence -> per-step result or error, then every table's final rows.",
    vectors,
  };
}

/** Every vector file, by file name. */
export async function buildVectors(): Promise<Record<string, VectorFile>> {
  return {
    "decode.json": decodeFile(),
    "validation.json": validationFile(),
    "schema.json": await schemaFile(),
    "compile.json": compileFile(),
    "execute.json": await executeFile(),
  };
}

if (import.meta.main) {
  const dir = new URL("../../ir/vectors/", import.meta.url);
  await Deno.mkdir(dir, { recursive: true });
  for (const [name, file] of Object.entries(await buildVectors())) {
    await Deno.writeTextFile(new URL(name, dir), JSON.stringify(file, null, 2) + "\n");
    console.log(`${name}: ${file.vectors.length} vectors`);
  }
}
