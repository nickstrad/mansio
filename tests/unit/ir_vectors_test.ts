/**
 * The TS runner for the shared IR vectors (`ir/vectors/*.json`, format in `ir/vectors/README.md`).
 *
 * Every vector runs against the reference implementation: `core/ir/decode.ts` for decode,
 * `core/ir/semantics.ts`'s `body` for validation, `core/ir/schema.ts` (+ `compileDDL`) for schema
 * declarations, `core/compile` for compile, and `platform/ir/executor.ts` on a real SQLite file for
 * execute. The Go runner (IR-1/IR-2) reads the same files and must reach the same outcomes.
 *
 * The last two tests regenerate the corpus from `scripts/ir/gen-vectors.ts` and the compile corpus from
 * `scripts/ir/gen-compile-corpus.ts` in memory and require the committed files to match, so the JSON cannot drift from its source (or from `core/compile`).
 */
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Cause, Clock, Effect, Either, Exit } from "effect";
import type { Schema } from "../../core/schema.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileSelect } from "../../core/compile/select.ts";
import { compileDelete, compileInsert, compileReplace } from "../../core/compile/write.ts";
import { decode } from "../../core/compile/decode.ts";
import type { SelectPlan } from "../../core/plan.ts";
import { decodeBody, decodeValue } from "../../core/ir/decode.ts";
import { type IrCause, IrFailure, toWireError } from "../../core/ir/errors.ts";
import { canonical } from "../../core/ir/json.ts";
import { decodeSchema, fingerprintText } from "../../core/ir/schema.ts";
import { body } from "../../core/ir/semantics.ts";
import { openIrDatabase, sha256Fingerprint } from "../../platform/ir/executor.ts";
import { IdGen } from "../../platform/services.ts";
import { Database } from "../../platform/sqlite_ffi/mod.ts";
import { buildVectors } from "../../scripts/ir/gen-vectors.ts";
import { corpus } from "../../scripts/ir/gen-compile-corpus.ts";

type J = Record<string, unknown>;
type Vector = J & { name: string };
const DIR = new URL("../../ir/vectors/", import.meta.url);
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
    assert.equal(await Effect.runPromise(sha256Fingerprint(text)), expect.fingerprint, v.name);
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

/** A clock that always reads `millis`. */
function fixedClock(millis: number): Clock.Clock {
  const base = Clock.make();
  return {
    [Clock.ClockTypeId]: Clock.ClockTypeId,
    unsafeCurrentTimeMillis: () => millis,
    unsafeCurrentTimeNanos: () => BigInt(millis) * 1_000_000n,
    currentTimeMillis: Effect.succeed(millis),
    currentTimeNanos: Effect.succeed(BigInt(millis) * 1_000_000n),
    sleep: (duration) => base.sleep(duration),
  };
}

/** Writes seed documents (with their `_creationTime` and `_version`) straight through `core/compile`. */
function seed(file: string, schema: Schema, tables: Record<string, J[]>) {
  const db = Database.open(file);
  try {
    for (const [table, docs] of Object.entries(tables)) {
      for (const { _id, _creationTime, _version, ...doc } of docs) {
        const insert = right(compileInsert(schema, table, doc, { _id, _creationTime }), "seed");
        db.prepare(insert.text).run(insert.values);
        db.prepare(`UPDATE "${table}" SET "_version" = ? WHERE "_id" = ?`).run([
          _version as number,
          _id as string,
        ]);
      }
    }
  } finally {
    db.close();
  }
}

/** Every row of every table, ordered by `_id`, decoded. */
function dump(file: string, schema: Schema): Record<string, unknown> {
  const db = Database.open(file);
  try {
    return Object.fromEntries(
      Object.keys(schema).map((table) => {
        const plan: SelectPlan = { kind: "select", table, filters: [], limit: 1000 };
        const sql = right(compileSelect(schema, plan, "sqlite"), "dump");
        return [table, right(decode(schema, plan, db.prepare(sql.text).all(sql.values)), "dump")];
      }),
    );
  } finally {
    db.close();
  }
}

/** `_tsdb_requests`, ordered by id, with `result` as lowercase hex. */
function requests(file: string): unknown[] {
  const db = Database.open(file);
  try {
    return db.prepare("select request_id, applied_ms, result from _tsdb_requests order by request_id").all()
      .map((row) => ({
        request_id: row.request_id,
        applied_ms: row.applied_ms,
        result_hex: Array.from(row.result as Uint8Array, (b) => b.toString(16).padStart(2, "0")).join(""),
      }));
  } finally {
    db.close();
  }
}

Deno.test("ir vectors: execute", async () => {
  for (const v of await load("execute.json")) {
    const dir = mkdtempSync(join(tmpdir(), "ir-vectors-"));
    const file = join(dir, "db.sqlite");
    const schema = schemaOf(v.schema, v.name);
    try {
      await Effect.runPromise(Effect.scoped(Effect.gen(function* () {
        const db = yield* openIrDatabase({ file, schema });
        assert.equal(db.fingerprint, v.fingerprint, `${v.name}: fingerprint`);
        seed(file, schema, v.seed as Record<string, J[]>);
        for (const [i, step] of (v.steps as J[]).entries()) {
          const name = `${v.name} step ${i}`;
          const ids = [...step.serverIds as string[]];
          const idGen = {
            next: Effect.sync(() => ids.shift() ?? assert.fail(`${name}: server ids exhausted`)),
          };
          const run = Either.isLeft(decodeValue(step.request))
            ? Effect.fail(right(Either.flip(decodeValue(step.request)), name))
            : db.execute(right(decodeValue(step.request), name));
          const exit = yield* Effect.either(
            run.pipe(Effect.provideService(IdGen, idGen), Effect.withClock(fixedClock(step.clock as number))),
          );
          const expect = step.expect as J;
          if (expect.error) {
            if (Either.isRight(exit)) {
              assert.fail(`${name}: expected ${JSON.stringify(expect.error)}, got ok`);
            }
            assertError(exit.left, expect.error as J, name);
          } else assert.deepEqual(JSON.parse(JSON.stringify(right(exit, name))), expect, name);
          assert.deepEqual(ids, [], `${name}: serverIds not fully consumed`);
        }
      })));
      const want = v.final as Record<string, unknown[]>;
      const got = dump(file, schema);
      if (want.requests) assert.deepEqual(requests(file), want.requests, `${v.name}: final requests`);
      for (const table of Object.keys(schema)) {
        assert.deepEqual(got[table], want[table] ?? [], `${v.name}: final ${table}`);
      }
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }
});

Deno.test("ir vectors: committed files match scripts/ir/gen-vectors.ts", async () => {
  const built = await buildVectors();
  assert.deepEqual(Object.keys(built).sort(), [...FILES].sort());
  for (const name of FILES) {
    const committed = JSON.parse(await Deno.readTextFile(new URL(name, DIR)));
    assert.deepEqual(
      committed,
      JSON.parse(JSON.stringify(built[name])),
      `${name} is stale: run scripts/ir/gen-vectors.ts`,
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
    "compile-corpus.jsonl is stale: run scripts/ir/gen-compile-corpus.ts",
  );
});

Deno.test("executor: a query holding a write is a defect, even when it bypassed decode", async () => {
  const dir = mkdtempSync(join(tmpdir(), "ir-exec-"));
  try {
    const schema = schemaOf({
      t: { fields: { a: { node: { kind: "string" } } }, storage: "columns", indexes: [] },
    }, "t");
    const exit = await Effect.runPromiseExit(Effect.scoped(Effect.gen(function* () {
      const db = yield* openIrDatabase({ file: join(dir, "db.sqlite"), schema });
      return yield* db.execute({
        v: 1,
        db: { tenant: "t", app: "a" },
        kind: "query",
        schema: db.fingerprint,
        ops: [{ op: "insert", table: "t", value: { a: "x" } }],
      }).pipe(Effect.provideService(IdGen, { next: Effect.succeed("s1") }));
    })));
    assert.ok(Exit.isFailure(exit) && Cause.isDie(exit.cause), String(exit));
    assert.deepEqual(dump(join(dir, "db.sqlite"), schema), { t: [] });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
