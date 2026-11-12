/**
 * The TS runner for the shared IR vectors (`intermediate-representation/vectors/*.json`, format in `intermediate-representation/vectors/README.md`).
 *
 * The decode, validation, schema and compile vectors run against the TS implementation, which is the
 * SDK's and the oracle's (ADR 0010): `core/intermediate-representation/decode.ts` for decode, `core/intermediate-representation/semantics.ts`'s `body` for
 * validation, `core/intermediate-representation/schema.ts` (+ `compileDDL`) for schema declarations, `core/compile` for compile.
 * The Go runner (`server/`) reads the same files and must reach the same outcomes.
 *
 * `execute.json` runs only in Go (`server/internal/docexec`). Its TS run went with the Deno platform's
 * reference executor and SQLite binding at IR-6; its expectations are hand-written in the generator, not
 * produced by an executor, so the staleness check below still covers the file.
 *
 * The last two tests regenerate the corpus from `scripts/intermediate-representation/gen-vectors.ts` and the compile corpus from
 * `scripts/intermediate-representation/gen-compile-corpus.ts` in memory and require the committed files to match, so the JSON cannot drift from its source (or from `core/compile`).
 */
import assert from "node:assert/strict";
import { Either } from "effect";
import type { Schema } from "../../core/schema.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileSelect } from "../../core/compile/select.ts";
import { compileDelete, compileInsert, compileReplace } from "../../core/compile/write.ts";
import type { SelectPlan } from "../../core/plan.ts";
import { decodeBody } from "../../core/intermediate-representation/decode.ts";
import { type IrCause, IrFailure, toWireError } from "../../core/intermediate-representation/errors.ts";
import { canonical } from "../../core/intermediate-representation/json.ts";
import { decodeSchema, fingerprintText } from "../../core/intermediate-representation/schema.ts";
import { body } from "../../core/intermediate-representation/semantics.ts";
import { fingerprintOfText } from "../../sdk/fingerprint.ts";
import { buildVectors } from "../../scripts/intermediate-representation/gen-vectors.ts";
import { corpus } from "../../scripts/intermediate-representation/gen-compile-corpus.ts";

type J = Record<string, unknown>;
type Vector = J & { name: string };
const DIR = new URL("../../intermediate-representation/vectors/", import.meta.url);
const FILES = ["decode.json", "validation.json", "schema.json", "compile.json", "execute.json"];

async function load(name: string): Promise<Vector[]> {
  return JSON.parse(await Deno.readTextFile(new URL(name, DIR))).vectors;
}

function right<A, E>(either: Either.Either<A, E>, name: string): A {
  if (Either.isLeft(either)) assert.fail(`${name}: unexpected failure ${JSON.stringify(either.left)}`);
  return either.right;
}

function schemaOf(json: unknown, name: string): Schema {
  return right(decodeSchema(json), name);
}

/** The wire comparison: tag, status and op exactly; `fields` as a subset. */
function assertError(failure: IrFailure, expected: J, name: string) {
  const { status, body: { error } } = toWireError(failure);
  const want = expected as { tag: string; status: number; op?: number; fields?: J };
  const got = JSON.stringify(error);
  assert.equal(error.tag, want.tag, `${name}: tag (${got})`);
  assert.equal(status, want.status, `${name}: status (${got})`);
  assert.equal(error.op, want.op, `${name}: op (${got})`);
  for (const [key, value] of Object.entries(want.fields ?? {})) {
    assert.deepEqual(error.fields?.[key], value, `${name}: fields.${key} (${got})`);
  }
}

function outcome<A>(either: Either.Either<A, IrCause>, expect: J, name: string, check: (a: A) => void) {
  if (expect.error) {
    if (Either.isRight(either)) assert.fail(`${name}: expected ${JSON.stringify(expect.error)}, got ok`);
    assertError(new IrFailure({ cause: either.left }), expect.error as J, name);
  } else check(right(either, name));
}

Deno.test("ir vectors: decode", async () => {
  for (const v of await load("decode.json")) {
    let bytes = v.base64 !== undefined
      ? Uint8Array.from(atob(v.base64 as string), (c) => c.charCodeAt(0))
      : new TextEncoder().encode(v.raw as string);
    if (v.pad !== undefined) {
      bytes = Uint8Array.from([...bytes, ...new Uint8Array((v.pad as number) - bytes.length).fill(0x20)]);
    }
    const decoded = decodeBody(bytes);
    const expect = v.expect as J;
    if (expect.ok) assert.equal(canonical(right(decoded, v.name)), expect.canonical, v.name);
    else {
      if (Either.isRight(decoded)) {
        assert.fail(`${v.name}: expected an error, decoded ${canonical(decoded.right)}`);
      }
      assertError(decoded.left, expect.error as J, v.name);
    }
  }
});

Deno.test("ir vectors: validation", async () => {
  for (const v of await load("validation.json")) {
    const table = schemaOf({ t: { fields: v.fields, storage: "columns", indexes: [] } }, v.name).t;
    const expect = v.expect as J;
    outcome(
      body(table, v.value, v.mode === "patch"),
      expect,
      v.name,
      (value) => assert.deepEqual(value, expect.value, v.name),
    );
  }
});

Deno.test("ir vectors: schema", async () => {
  for (const v of await load("schema.json")) {
    const expect = v.expect as J;
    const decoded: Either.Either<Schema, IrCause> = Either.flatMap(
      decodeSchema(v.schema),
      (schema) => Either.map(compileDDL(schema), () => schema),
    );
    if (expect.error) {
      outcome(decoded, expect, v.name, () => {});
      continue;
    }
    const text = fingerprintText(right(decoded, v.name));
    assert.equal(text, expect.text, v.name);
    assert.equal(await fingerprintOfText(text), expect.fingerprint, v.name);
  }
});

Deno.test("ir vectors: compile", async () => {
  for (const v of await load("compile.json")) {
    const schema = schemaOf(v.schema, v.name);
    const table = v.table as string;
    const sql = ((): Either.Either<unknown, { _tag: string }> => {
      switch (v.kind) {
        case "ddl":
          return Either.map(compileDDL(schema), (statements) => ({ statements }));
        case "select":
          return Either.map(compileSelect(schema, v.plan as SelectPlan, "sqlite"), (sql) => ({ sql }));
        case "insert":
          return Either.map(compileInsert(schema, table, v.value, v.meta as J), (sql) => ({ sql }));
        case "replace":
          return Either.map(compileReplace(schema, table, v.id as string, v.value), (sql) => ({ sql }));
        default:
          return Either.map(compileDelete(schema, table, v.id as string), (sql) => ({ sql }));
      }
    })();
    const got = Either.isRight(sql) ? sql.right : (() => {
      const { _tag, ...fields } = sql.left as unknown as J;
      return { error: { tag: _tag, fields: JSON.parse(JSON.stringify(fields)) } };
    })();
    assert.deepEqual(got, v.expect, v.name);
  }
});

Deno.test("ir vectors: committed files match scripts/intermediate-representation/gen-vectors.ts", async () => {
  const built = await buildVectors();
  assert.deepEqual(Object.keys(built).sort(), [...FILES].sort());
  for (const name of FILES) {
    const committed = JSON.parse(await Deno.readTextFile(new URL(name, DIR)));
    assert.deepEqual(
      committed,
      JSON.parse(JSON.stringify(built[name])),
      `${name} is stale: run scripts/intermediate-representation/gen-vectors.ts`,
    );
  }
});

Deno.test("ir vectors: generated compile corpus matches core/compile", async () => {
  const lines = (await Deno.readTextFile(new URL("generated/compile-corpus.jsonl", DIR))).trimEnd().split(
    "\n",
  );
  const { count, seed } = JSON.parse(lines[0]);
  assert.deepEqual(
    corpus(count, seed),
    lines,
    "compile-corpus.jsonl is stale: run scripts/intermediate-representation/gen-compile-corpus.ts",
  );
});
