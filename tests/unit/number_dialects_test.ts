/**
 * The numeric boundary matrix (plan item 30), run on both engines and both storage layouts.
 *
 * A `number` field is a JS double. The writer stores `JSON.stringify(x)` in the `json` layout, which
 * for an integral double at or beyond 2^53 is integer text (`1693240866440448800` for the double
 * 1693240866440448768). SQLite's `json_extract` used to read that text as an exact 64-bit INTEGER,
 * which is not the double, so `eq` missed, `gt` matched the value against itself and `countBy`
 * returned an integer the driver refuses. This module checks equality, `gt`, ordering in both
 * directions, `count` and `countBy` over the values around those boundaries against a JS reference,
 * in the `columns` and the `json` layout, on SQLite (the writer) and DuckDB (the reader).
 *
 * **Three processes.** DuckDB's `sqlite` extension is a second copy of the SQLite library, and it
 * must never share an address space with the writer's SQLite (plan §0, ADR 0001; `reader_test.ts`
 * does the same). `deno task test` already loads the writer's SQLite (the FFI binding) in this
 * process, so the parent half loads neither engine. It runs this same module twice more with
 * `deno run`, selected by `ROLE_ENV`:
 *
 * - `sqlite` writes the fixture database for each layout with the pure compiler and the FFI binding
 *   (`tests/helpers/sqlite_inspect.ts`), then answers every plan with `compileSelect(…, "sqlite")`
 *   and `decode` over the binding's blocking `all`, as `platform/sqlite.ts` does.
 * - `duckdb` attaches those files read-only through the real `ReaderLive` and answers every plan
 *   from a query handler, so the plans travel the production bind path. It never loads the
 *   writer's SQLite.
 *
 * Each child prints one JSON line with its engine name and one answer per plan. The parent checks
 * the child exited 0, named the engine it was asked for, and answered every plan, so neither leg can
 * pass by being skipped. (`Deno.test` is a no-op under `deno run`, so the children run only the
 * code under their role.)
 *
 * One DuckDB limitation is pinned rather than hidden: `@duckdb/node-api` infers a `BIGINT` bind for
 * any integral JS number, and refuses one outside the int64 range (`bigint out of int64 range`), so
 * on DuckDB a filter on an integral double >= 2^63 in magnitude fails in either layout. That is the
 * reader's bind path (`platform/duckdb.ts`), not the compiler. The matrix still stores such values
 * and orders and groups them on DuckDB; only those filter plans are expected to fail, and the test
 * asserts exactly that, so fixing the bind path turns this test red until the expectation is
 * removed.
 */
import assert from "node:assert/strict";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { Effect, Either, Exit, Layer } from "effect";
import { type Schema, Table, v, withStorage } from "../../core/schema.ts";
import type { SelectPlan } from "../../core/plan.ts";
import type { DbError } from "../../core/errors.ts";

/** Unwrap a right, failing the test with the rendered left on a left. */
function expectRight<A, E>(either: Either.Either<A, E>): A {
  if (Either.isRight(either)) return either.right;
  assert.fail(`expected a right, got left ${JSON.stringify(either.left)}`);
}

const ROLE_ENV = "MANSIO_NUMBER_DIALECTS_ROLE";
const DIR_ENV = "MANSIO_NUMBER_DIALECTS_DIR";
const role = Deno.env.get(ROLE_ENV);
const repoRoot = fileURLToPath(new URL("../../", import.meta.url));

// ---------------------------------------------------------------------------
// The matrix (shared by all three processes; pure)
// ---------------------------------------------------------------------------

/** The adjacent representable doubles, via the IEEE-754 bit pattern. */
function nextUp(x: number): number {
  if (x === 0) return Number.MIN_VALUE;
  const bits = new BigInt64Array(new Float64Array([x]).buffer);
  bits[0] += x > 0 ? 1n : -1n;
  return new Float64Array(bits.buffer)[0];
}
function nextDown(x: number): number {
  return -nextUp(-x);
}

const P53 = 2 ** 53;
/** The double the reflection reported; its shortest decimal, `…448800`, is not the double itself. */
const REPORTED = 1693240866440448800;

/** Values a `number` field holds and is filtered by. Every one is distinct as a double, bar `-0`. */
const NUMBERS: readonly number[] = [
  0,
  -0,
  1,
  -1,
  0.5,
  -0.5,
  0.1,
  -2.75,
  Number.MIN_VALUE,
  -Number.MIN_VALUE,
  Number.MAX_SAFE_INTEGER,
  -Number.MAX_SAFE_INTEGER,
  P53,
  nextUp(P53),
  nextUp(nextUp(P53)),
  -P53,
  nextDown(-P53),
  REPORTED,
  nextDown(REPORTED),
  nextUp(REPORTED),
  -REPORTED,
  2 ** 62,
  2 ** 63,
  -(2 ** 63),
  1e21,
  -1e21,
  Number.MAX_VALUE,
];

/** Safe integers for the `integer` field, whose exact-INTEGER behaviour must not change. */
const INTEGERS: readonly number[] = [0, 1, -1, 42, -42, Number.MAX_SAFE_INTEGER, -Number.MAX_SAFE_INTEGER];

const schemaFor = (storage: "columns" | "json"): Schema => {
  const nums = new Table(
    { n: v.nullable(v.number()), i: v.nullable(v.integer()) },
    "columns",
    [{ name: "by_n", fields: ["n"] }, { name: "by_i", fields: ["i"] }],
  );
  return withStorage({ nums }, storage);
};
const LAYOUTS = ["columns", "json"] as const;

type Doc = { _id: string; _creationTime: number; _version: number; n: number | null; i: number | null };

/**
 * One document per matrix value, plus repeats (so `countBy` counts past 1 and ordering has ties
 * that `_id` must break) and NULLs in both fields.
 */
function documents(): Doc[] {
  const values: (number | null)[] = [...NUMBERS, null, null, P53, REPORTED, REPORTED, 0.5];
  return values.map((n, k) => ({
    _id: `d${String(k).padStart(2, "0")}`,
    _creationTime: k,
    _version: 1,
    n,
    i: k % 5 === 4 ? null : INTEGERS[k % INTEGERS.length],
  }));
}

const select = (rest: Partial<SelectPlan>): SelectPlan => ({
  kind: "select",
  table: "nums",
  filters: [],
  limit: 1000,
  ...rest,
});

/** Every plan, for one layout: eq/gt on every value, eq null, both orders, count and countBy. */
function plans(): SelectPlan[] {
  const out: SelectPlan[] = [];
  for (const [field, values] of [["n", NUMBERS], ["i", INTEGERS]] as const) {
    for (const value of values) {
      out.push(select({ filters: [{ field, op: "eq", value }] }));
      out.push(select({ filters: [{ field, op: "gt", value }] }));
      out.push(select({ filters: [{ field, op: "gt", value }], aggregate: { kind: "count" } }));
    }
    out.push(select({ filters: [{ field, op: "eq", value: null }] }));
    out.push(select({ order: { field, direction: "asc" } }));
    out.push(select({ order: { field, direction: "desc" } }));
    out.push(select({ order: { field, direction: "asc" }, limit: 3 }));
    out.push(select({ aggregate: { kind: "countBy", field } }));
    out.push(select({ aggregate: { kind: "countBy", field }, limit: 2 }));
  }
  return out;
}

type Answer = { value: unknown } | { error: string };
type Report = { engine: string; layouts: Record<string, Answer[]> };

// ---------------------------------------------------------------------------
// JS reference (parent only)
// ---------------------------------------------------------------------------

/** Neither layout keeps the sign of zero (SQLite stores REAL `-0.0` as `0.0`; JSON writes `0`). */
const stored = (doc: Doc): Doc => ({ ...doc, n: doc.n === 0 ? 0 : doc.n });

function compareNullable(a: number | null, b: number | null, direction: "asc" | "desc"): number {
  if (a === null || b === null) {
    if (a === b) return 0;
    const nullFirst = a === null ? -1 : 1;
    return direction === "asc" ? nullFirst : -nullFirst;
  }
  const c = a < b ? -1 : a > b ? 1 : 0;
  return direction === "asc" ? c : -c;
}

function reference(docs: readonly Doc[], plan: SelectPlan): unknown {
  const field = (doc: Doc, name: string) => doc[name as "n" | "i"];
  const hits = docs.map(stored).filter((doc) =>
    plan.filters.every(({ field: name, op, value }) => {
      const x = field(doc, name);
      if (op === "eq") return value === null ? x === null : x !== null && x === value;
      return x !== null && x > (value as number);
    })
  );
  if (plan.aggregate?.kind === "count") return hits.length;
  if (plan.aggregate?.kind === "countBy") {
    const name = plan.aggregate.field;
    const counts = new Map<number | null, number>();
    for (const doc of hits) counts.set(field(doc, name), (counts.get(field(doc, name)) ?? 0) + 1);
    return [...counts]
      .map(([value, count]) => ({ value, count }))
      .sort((a, b) => compareNullable(a.value, b.value, "asc"))
      .slice(0, plan.limit);
  }
  const order = plan.order;
  return hits
    .sort((a, b) =>
      (order ? compareNullable(field(a, order.field), field(b, order.field), order.direction) : 0) ||
      (a._id < b._id ? -1 : a._id > b._id ? 1 : 0)
    )
    .slice(0, plan.limit);
}

/** Whether `@duckdb/node-api` refuses to bind this filter value (see the module comment). */
const duckUnbindable = (value: unknown): boolean =>
  typeof value === "number" && Number.isInteger(value) && (value >= 2 ** 63 || value < -(2 ** 63));

// ---------------------------------------------------------------------------
// Parent half: no engine in this process
// ---------------------------------------------------------------------------

async function runLeg(engine: "sqlite" | "duckdb", dir: string): Promise<Report> {
  const output = await new Deno.Command(Deno.execPath(), {
    args: ["run", "-A", "--no-check", fileURLToPath(import.meta.url)],
    cwd: repoRoot,
    env: { [ROLE_ENV]: engine, [DIR_ENV]: dir },
    stdout: "piped",
    stderr: "piped",
  }).output();
  const stdout = new TextDecoder().decode(output.stdout);
  const stderr = new TextDecoder().decode(output.stderr);
  assert.equal(output.code, 0, `${engine} leg exited ${output.code}\n${stderr}`);
  const line = stdout.trim().split("\n").at(-1) ?? "";
  const report = JSON.parse(line) as Report;
  // Guard against a leg that ran nothing, or ran as the wrong engine, and so passed vacuously.
  assert.equal(report.engine, engine);
  for (const layout of LAYOUTS) {
    assert.equal(report.layouts[layout]?.length, plans().length, `${engine}/${layout}`);
  }
  return report;
}

const show = (value: unknown) => JSON.stringify(value);

if (role === undefined) {
  Deno.test("number boundary matrix: SQLite and DuckDB, in separate processes, agree with the JS reference in both layouts", async () => {
    const dir = await Deno.makeTempDir({ prefix: "mansio-number-dialects-" });
    try {
      // SQLite writes the fixture files, so it runs to completion before DuckDB attaches them.
      const sqlite = await runLeg("sqlite", dir);
      const duckdb = await runLeg("duckdb", dir);
      const docs = documents();
      const all = plans();
      let pinned = 0;
      for (const layout of LAYOUTS) {
        all.forEach((plan, k) => {
          const expected = reference(docs, plan);
          const where = `${layout} ${show(plan)}`;
          assert.deepStrictEqual(sqlite.layouts[layout][k], { value: expected }, `sqlite ${where}`);
          const duck = duckdb.layouts[layout][k];
          if (plan.filters.some((f) => duckUnbindable(f.value))) {
            pinned++;
            assert.ok(
              "error" in duck && /out of int64 range/.test(duck.error),
              `duckdb ${where}: ${show(duck)}`,
            );
          } else assert.deepStrictEqual(duck, { value: expected }, `duckdb ${where}`);
        });
      }
      // 2^63, 1e21, -1e21 and MAX_VALUE (-(2^63) fits a BIGINT), each as eq, gt and gt+count, per layout.
      assert.equal(pinned, 4 * 3 * LAYOUTS.length);
    } finally {
      await Deno.remove(dir, { recursive: true });
    }
  });

  Deno.test("number boundary matrix: the reference itself distinguishes the boundary doubles", () => {
    // If these collapsed, the matrix would test nothing at 2^53 or at the reported value.
    assert.equal(nextUp(P53), P53 + 2);
    // The double above -2^53 is -MAX_SAFE_INTEGER, already in the matrix.
    assert.equal(nextUp(-P53), -Number.MAX_SAFE_INTEGER);
    assert.equal(nextDown(-P53), -P53 - 2);
    assert.equal(REPORTED, 1693240866440448768);
    assert.equal(nextUp(REPORTED) - REPORTED, 256);
    assert.notEqual(nextDown(REPORTED), REPORTED);
    assert.equal(new Set(NUMBERS.map((x) => (x === 0 ? 0 : x))).size, NUMBERS.length - 1);
    // The JSON text of the reported value is integer text, which is what tripped json_extract.
    assert.equal(JSON.stringify(REPORTED), "1693240866440448800");
  });
}

// ---------------------------------------------------------------------------
// SQLite leg: the FFI binding, never DuckDB
// ---------------------------------------------------------------------------

/** Error or value, as one JSON-safe answer. */
function answer(result: Either.Either<unknown, unknown>): Answer {
  return Either.isRight(result) ? { value: result.right } : { error: String(result.left) };
}

if (role === "sqlite") {
  const { inspect } = await import("../helpers/sqlite_inspect.ts");
  const { compileDDL } = await import("../../core/compile/ddl.ts");
  const { compileInsert } = await import("../../core/compile/write.ts");
  const { compileSelect } = await import("../../core/compile/select.ts");
  const { decode } = await import("../../core/compile/decode.ts");
  const dir = Deno.env.get(DIR_ENV)!;
  const report: Report = { engine: "sqlite", layouts: {} };
  for (const layout of LAYOUTS) {
    const schema = schemaFor(layout);
    const db = inspect(join(dir, `${layout}.sqlite`), { write: true });
    try {
      for (const statement of expectRight(compileDDL(schema))) db.exec(statement);
      for (const { _id, _creationTime, n, i } of documents()) {
        const sql = expectRight(compileInsert(schema, "nums", { n, i }, { _id, _creationTime }));
        db.run(sql.text, sql.values);
      }
      report.layouts[layout] = plans().map((plan) => {
        try {
          const sql = expectRight(compileSelect(schema, plan, "sqlite"));
          return answer(decode(schema, plan, db.all(sql.text, sql.values)));
        } catch (error) {
          return { error: String(error) };
        }
      });
    } finally {
      db.close();
    }
  }
  console.log(JSON.stringify(report));
}

// ---------------------------------------------------------------------------
// DuckDB leg: the real ReaderLive, never the writer's SQLite
// ---------------------------------------------------------------------------

if (role === "duckdb") {
  const { ReaderLive } = await import("../../platform/duckdb.ts");
  const { ConfigTest } = await import("../../platform/config.ts");
  const { EventSink, Reader } = await import("../../platform/services.ts");
  const { defineFunctions } = await import("../../sdk/functions.ts");
  const dir = Deno.env.get(DIR_ENV)!;
  const report: Report = { engine: "duckdb", layouts: {} };
  const deps = Layer.merge(
    ConfigTest(),
    Layer.succeed(EventSink, { emit: () => Effect.void }),
  );
  for (const layout of LAYOUTS) {
    const schema = schemaFor(layout);
    const { query } = defineFunctions(schema);
    // Replays a plan through the SDK builder, so it reaches DuckDB the way app queries do.
    const replay = query({
      args: { plan: v.string() },
      handler: ({ db }, { plan: text }) => {
        const plan = JSON.parse(text) as SelectPlan;
        let find = db.find("nums");
        for (const { field, op, value } of plan.filters) {
          find = op === "eq"
            ? find.where(field as never, value as never)
            : find.gt(field as never, value as number);
        }
        if (plan.order) find = find.sort(plan.order.field as never, plan.order.direction);
        find = find.limit(plan.limit);
        if (plan.aggregate?.kind === "count") return find.count() as Effect.Effect<unknown, DbError>;
        if (plan.aggregate?.kind === "countBy") return find.countBy(plan.aggregate.field as never);
        return find.collect();
      },
    });
    const program = Effect.gen(function* () {
      const reader = yield* Reader;
      const answers: Answer[] = [];
      for (const plan of plans()) {
        const exit = yield* Effect.exit(reader.query(replay, { plan: JSON.stringify(plan) }));
        answers.push(
          Exit.isSuccess(exit) ? { value: exit.value } : { error: JSON.stringify(exit.cause) },
        );
      }
      return answers;
    });
    const app = { id: `numbers-${layout}`, file: join(dir, `${layout}.sqlite`), schema };
    report.layouts[layout] = await Effect.runPromise(
      Effect.provide(program, ReaderLive(app).pipe(Layer.provide(deps))),
    );
  }
  console.log(JSON.stringify(report));
}
