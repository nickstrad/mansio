/**
 * Property tests for `core/compile/*`: security and semantics invariants of the
 * SQL the compiler emits, over generated schemas and plans.
 *
 * The properties are chosen to catch the two failure classes that matter for a SQL generator, not to
 * restate its implementation:
 *
 * - **Injection.** No filter value is ever interpolated (bind parity, no value leakage, the lexer audit
 *   in `compiler_arbitraries.ts`), and no name that is not an identifier ever reaches the text
 *   (identifier safety, totality over junk input).
 * - **Drift.** The two dialects and the two storage layouts must agree on what a plan means (dialect
 *   and storage agreement), and the aggregate/limit rules must hold for every plan, not just the
 *   handful in `core/compile/GENERATED_SQL.txt`.
 *
 * Execution semantics against a real engine are `sqlite_oracle_test.ts`'s job.
 */
import assert from "node:assert/strict";
import fc from "fast-check";
import { Either } from "effect";
import { type Schema, Table, withStorage } from "../../core/schema.ts";
import type { Filter, SelectPlan } from "../../core/plan.ts";
import type { Dialect, SQL } from "../../core/compile/dialect.ts";
import { compileSelect } from "../../core/compile/select.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileDelete, compileInsert, compileReplace } from "../../core/compile/write.ts";
import {
  adversarialString,
  anyName,
  auditDDL,
  auditSelect,
  auditWrite,
  bodyArb,
  enumValuesOf,
  fieldName,
  fieldsArb,
  IDENTIFIER,
  INJECTION,
  invalidIdentifier,
  isAdversarial,
  junkPlan,
  junkSchema,
  leftOf,
  lex,
  placeholders,
  rightOf,
  runs,
  schemaAndPlan,
  schemaArb,
  show,
  skeleton,
  tableArb,
  tableName,
  textValue,
  validPlanArb,
} from "./compiler_arbitraries.ts";
import { titled } from "./arbitraries.ts";

const DIALECTS: readonly Dialect[] = ["sqlite", "duckdb"];
const STORAGES = ["columns", "json"] as const;

type Compiled = {
  storage: (typeof STORAGES)[number];
  dialect: Dialect;
  schema: Schema;
  result: Either.Either<SQL, unknown>;
};

/** The plan compiled in all four (storage layout x dialect) configurations. */
function compileAll(schema: Schema, plan: SelectPlan): Compiled[] {
  return STORAGES.flatMap((storage) => {
    const stored = withStorage(schema, storage);
    return DIALECTS.map((dialect) => ({
      storage,
      dialect,
      schema: stored,
      result: compileSelect(stored, plan, dialect),
    }));
  });
}

/** Tag and reason of an error, for comparing rejections across configurations. */
function why(error: unknown): string {
  const e = error as { _tag?: string; reason?: string };
  return `${e._tag}/${e.reason}`;
}

/** Filters whose value travels as a bind parameter: everything except `eq null`, which compiles to IS NULL. */
const bound = (filters: readonly Filter[]): Filter[] =>
  filters.filter((f) => !(f.op === "eq" && f.value === null));

/** Names the compiler itself may quote, beyond the schema's tables, fields and indexes. */
const COMPILER_NAMES = ["_id", "_creationTime", "_version", "doc", "app", "value", "count"];

/** Every identifier a compilation over `schema` is entitled to emit. */
function namesOf(schema: Schema): Set<string> {
  const names = new Set(COMPILER_NAMES);
  for (const [name, table] of Object.entries(schema)) {
    names.add(name);
    for (const field of Object.keys(table.fields)) names.add(field);
    for (const index of table.indexes) names.add(`idx_${name}_${index.name}`);
  }
  return names;
}

/** Identifiers in `text` that the schema did not declare and the compiler does not own. */
function foreignIdentifiers(text: string, schema: Schema): string[] {
  const allowed = namesOf(schema);
  return lex(text).flatMap((t) => (t.kind === "ident" && !allowed.has(t.value) ? [t.value] : []));
}

// ---------------------------------------------------------------------------
// Bind parity
// ---------------------------------------------------------------------------

Deno.test(titled("compiler: bind parity — select placeholders equal values; $pN is exactly 1..n"), () => {
  fc.assert(
    fc.property(schemaAndPlan, ({ schema, plan }) => {
      for (const { dialect, result } of compileAll(schema, plan)) {
        const sql = rightOf(result);
        const marks = placeholders(sql.text);
        assert.equal(marks.length, sql.values.length, `${dialect}: ${sql.text} ${show(sql.values)}`);
        const expected = dialect === "sqlite"
          ? sql.values.map(() => "?")
          : sql.values.map((_, i) => String(i + 1));
        assert.deepEqual(marks, expected, `${dialect}: ${sql.text}`);
        // One value per bound filter, plus the limit unless this is a plain count.
        assert.equal(
          sql.values.length,
          bound(plan.filters).length + (plan.aggregate?.kind === "count" ? 0 : 1),
        );
      }
    }),
    runs(),
  );
});

Deno.test(titled("compiler: bind parity — insert/replace/delete placeholders equal values"), () => {
  const arb = fc
    .tuple(tableName, tableArb())
    .chain(([name, table]) =>
      fc.record({
        name: fc.constant(name),
        table: fc.constant(table),
        body: bodyArb(table),
        id: textValue,
        time: fc.nat(),
      })
    );
  fc.assert(
    fc.property(arb, ({ name, table, body, id, time }) => {
      const schema = { [name]: table };
      const width = table.storage === "json" ? 1 : Object.keys(table.fields).length;
      const insert = rightOf(compileInsert(schema, name, body, { _id: id, _creationTime: time }));
      const replace = rightOf(compileReplace(schema, name, id, body));
      const remove = rightOf(compileDelete(schema, name, id));
      for (const sql of [insert, replace, remove]) {
        assert.deepEqual(placeholders(sql.text), sql.values.map(() => "?"), sql.text);
        assert.deepEqual(auditWrite(sql.text), [], sql.text);
        assert.deepEqual(foreignIdentifiers(sql.text, schema), [], sql.text);
      }
      assert.equal(insert.values.length, 3 + width);
      assert.equal(replace.values.length, width + 1);
      // Every caller-supplied value is bound, in particular the ids, which may be adversarial.
      assert.equal(insert.values[0], id);
      assert.equal(replace.values.at(-1), id);
      assert.deepEqual(remove.values, [id]);
    }),
    runs(),
  );
});

// ---------------------------------------------------------------------------
// No value leakage
// ---------------------------------------------------------------------------

/**
 * A table guaranteed to have a string field and an enum field whose declared values are themselves
 * adversarial, and a valid plan over it with at least one adversarial equality filter on the string
 * field, the enum field or `_id`.
 */
const leakCase = fc
  .tuple(
    tableName,
    fc.uniqueArray(fieldName, { minLength: 2, maxLength: 2, selector: (n) => n.toLowerCase() }),
    fc.uniqueArray(adversarialString, { minLength: 1, maxLength: 3 }),
    fieldsArb,
    fc.boolean(),
    fc.constantFrom("columns" as const, "json" as const),
  )
  .chain(([name, [s, e], values, others, nullable, storage]) => {
    const fields = Object.fromEntries(
      Object.entries(others).filter(([k]) =>
        k.toLowerCase() !== s.toLowerCase() && k.toLowerCase() !== e.toLowerCase()
      ),
    );
    fields[s] = { node: nullable ? { kind: "string", nullable: true } : { kind: "string" } };
    fields[e] = { node: { kind: "enum", values } };
    const schema: Schema = { [name]: new Table(fields, storage) };
    const adversarial = fc.array(
      fc.oneof(
        adversarialString.map((value): Filter => ({ field: s, op: "eq", value })),
        fc.constantFrom(...values).map((value): Filter => ({ field: e, op: "eq", value })),
        adversarialString.map((value): Filter => ({ field: "_id", op: "eq", value })),
      ),
      { minLength: 1, maxLength: 3 },
    );
    return fc.tuple(validPlanArb(schema, name), adversarial, fc.boolean()).map(([plan, extra, first]) => ({
      schema,
      plan: { ...plan, filters: first ? [...extra, ...plan.filters] : [...plan.filters, ...extra] },
    }));
  });

/** The exact string from `tests/core_test.ts`, pinned as the first example the property runs. */
const injectionExample = {
  schema: {
    games: new Table({
      status: { node: { kind: "string" } },
      winner: { node: { kind: "enum", values: ["X", "O"] } },
    }),
  },
  plan: {
    kind: "select",
    table: "games",
    filters: [{ field: "status", op: "eq", value: INJECTION }],
    limit: 20,
  },
} satisfies { schema: Schema; plan: SelectPlan };

Deno.test(
  titled("compiler: no value leakage — adversarial filter values are bound, never in the text"),
  () => {
    let injectionRuns = 0;
    fc.assert(
      fc.property(leakCase, ({ schema, plan }) => {
        if (plan.filters.some((f) => f.value === INJECTION)) injectionRuns++;
        for (const { dialect, storage, result } of compileAll(schema, plan)) {
          const sql = rightOf(result);
          const where = `${storage}/${dialect}: ${sql.text}`;
          // The compiler's own quoting (`"name"`, `'$.name'`) is the only place a metacharacter may
          // legitimately appear; with those removed, an adversarial value has nowhere to hide.
          const bare = sql.text.replace(/"[A-Za-z0-9_]*"|'\$\.[A-Za-z0-9_]*'/g, "_");
          for (const { value } of plan.filters) {
            if (typeof value !== "string" || !isAdversarial(value)) continue;
            assert.ok(!bare.includes(value), `value ${show(value)} leaked into ${where}`);
            assert.ok(sql.values.includes(value), `value ${show(value)} is not bound in ${where}`);
          }
          // Stronger than `includes`: an escaped interpolation ('O''Reilly…') passes the check above
          // but shows up here as a literal that is not a JSON path.
          assert.deepEqual(auditSelect(sql.text, dialect), [], where);
        }
      }),
      { ...runs(1000), examples: [[injectionExample]] },
    );
    // The generator weights INJECTION in directly; make sure that is still true.
    assert.ok(injectionRuns > 1, `INJECTION reached only ${injectionRuns} runs`);
  },
);

// ---------------------------------------------------------------------------
// Identifier safety
// ---------------------------------------------------------------------------

Deno.test(
  titled("compiler: identifier safety — every emitted token is a quoted identifier or a known keyword"),
  () => {
    const arb = schemaAndPlan.chain(({ schema, plan }) =>
      bodyArb(schema[plan.table]).map((body) => ({ schema, plan, body }))
    );
    fc.assert(
      fc.property(arb, ({ schema, plan, body }) => {
        for (const { dialect, storage, schema: stored, result } of compileAll(schema, plan)) {
          const sql = rightOf(result);
          assert.deepEqual(auditSelect(sql.text, dialect), [], `${storage}/${dialect}: ${sql.text}`);
          assert.deepEqual(foreignIdentifiers(sql.text, stored), [], sql.text);
        }
        for (const storage of STORAGES) {
          const stored = withStorage(schema, storage);
          const enums = enumValuesOf(stored);
          for (const statement of rightOf(compileDDL(stored))) {
            assert.deepEqual(auditDDL(statement, enums), [], statement);
            assert.deepEqual(foreignIdentifiers(statement, stored), [], statement);
          }
          const insert = rightOf(compileInsert(stored, plan.table, body, { _id: "x", _creationTime: 1 }));
          assert.deepEqual(auditWrite(insert.text), [], insert.text);
        }
      }),
      runs(),
    );
  },
);

type Poison = "table" | "field" | "indexed-field" | "index";
type PoisonCase = { schema: Schema; name: string; bad: string; where: Poison; plan: SelectPlan };

/**
 * A valid schema with exactly one name replaced or added that is not an identifier: a table name, a
 * (string) field, a field that is also indexed, or an index name. Returns the poisoned schema, the
 * table it is in, the bad name, and a plan over that table that references the bad name when it is a
 * field.
 */
const poisoned = fc
  .tuple(schemaArb, invalidIdentifier, fc.constantFrom<Poison>("table", "field", "indexed-field", "index"))
  .chain(([schema, bad, where]) =>
    fc.constantFrom(...Object.keys(schema)).chain((name): fc.Arbitrary<PoisonCase> => {
      const table = schema[name];
      const rest = Object.fromEntries(Object.entries(schema).filter(([k]) => k !== name));
      if (where === "table") {
        const out: Schema = { ...rest, [bad]: table };
        return validPlanArb(schema, name).map((plan) => ({
          schema: out,
          name: bad,
          bad,
          where,
          plan: { ...plan, table: bad },
        }));
      }
      if (where === "index") {
        const out: Schema = {
          ...rest,
          [name]: new Table(table.fields, table.storage, [...table.indexes, { name: bad, fields: ["_id"] }]),
        };
        return validPlanArb(schema, name).map((plan) => ({ schema: out, name, bad, where, plan }));
      }
      const fields = { ...table.fields, [bad]: { node: { kind: "string" as const } } };
      const indexes = where === "indexed-field"
        ? [...table.indexes, { name: "by_bad", fields: [bad] }]
        : table.indexes;
      const out: Schema = { ...rest, [name]: new Table(fields, table.storage, indexes) };
      const reference = fc.oneof(
        textValue.map((value) => (plan: SelectPlan): SelectPlan => ({
          ...plan,
          filters: [...plan.filters, { field: bad, op: "eq", value }],
        })),
        fc.constant((plan: SelectPlan): SelectPlan => {
          const { aggregate: _aggregate, ...rest } = plan;
          return { ...rest, order: { field: bad, direction: "asc" } };
        }),
        fc.constant((plan: SelectPlan): SelectPlan => ({
          ...plan,
          aggregate: { kind: "countBy", field: bad },
        })),
      );
      return fc.tuple(validPlanArb(schema, name), reference).map(([plan, refer]) => ({
        schema: out,
        name,
        bad,
        where,
        plan: refer(plan),
      }));
    })
  );

function expectInvalidIdentifier(result: Either.Either<unknown, unknown>, context: string): void {
  const error = leftOf(result) as { _tag: string; reason: string };
  assert.equal(why(error), "SchemaError/invalid-identifier", context);
}

Deno.test(
  titled("compiler: identifier safety — a non-identifier name fails to compile rather than being emitted"),
  () => {
    fc.assert(
      fc.property(poisoned, ({ schema, name, bad, where, plan }) => {
        for (const storage of STORAGES) {
          const stored = withStorage(schema, storage);
          const context = `${where} ${show(bad)} in ${storage}`;
          const ddl = compileDDL(stored);
          // An index name is only ever emitted as part of `idx_<table>_<name>`, which may itself be a
          // legal identifier ("1abc" -> "idx_t_1abc"). Otherwise a poisoned name the DDL has to write
          // must fail it; a `json` table's unindexed fields never reach the DDL at all.
          const ddlMustFail = where === "table" || where === "indexed-field" ||
            (where === "field" && storage === "columns") ||
            (where === "index" && !IDENTIFIER.test(`idx_${name}_${bad}`));
          if (ddlMustFail) expectInvalidIdentifier(ddl, `DDL, ${context}`);
          else {for (const statement of rightOf(ddl)) {
              assert.deepEqual(auditDDL(statement, enumValuesOf(stored)), [], statement);
            }}

          for (const dialect of DIALECTS) {
            const result = compileSelect(stored, plan, dialect);
            if (where === "index") assert.deepEqual(auditSelect(rightOf(result).text, dialect), []);
            else expectInvalidIdentifier(result, `select ${dialect}, ${context}`);
          }

          const body = Object.fromEntries(
            Object.entries(stored[name].fields).map(([k, f]) => [k, f.node.nullable ? null : undefined]),
          );
          const insert = compileInsert(stored, name, body, { _id: "x", _creationTime: 1 });
          // A body of nulls/undefineds rarely validates, so only the identifier outcome is asserted:
          // either a SchemaError (the bad name, or the body), or a clean statement.
          if (Either.isRight(insert)) assert.deepEqual(auditWrite(insert.right.text), [], insert.right.text);
          else assert.equal(insert.left._tag, "SchemaError");
          const remove = compileDelete(stored, name, "x");
          if (where === "table") expectInvalidIdentifier(remove, `delete, ${context}`);
          else assert.deepEqual(auditWrite(rightOf(remove).text), []);
        }
      }),
      runs(),
    );
  },
);

// ---------------------------------------------------------------------------
// Totality
// ---------------------------------------------------------------------------

/** The outcome of calling a compiler entry point: it must return an Either, never raise. */
function total<A>(run: () => Either.Either<A, unknown>, context: string): Either.Either<A, unknown> {
  let result: Either.Either<A, unknown>;
  try {
    result = run();
  } catch (error) {
    throw new Error(`${context} raised ${String(error)}`);
  }
  assert.ok(Either.isEither(result), `${context} returned ${show(result)}`);
  if (Either.isLeft(result)) {
    const tag = (result.left as { _tag?: string })._tag;
    assert.ok(tag === "CompileError" || tag === "SchemaError", `${context} failed with ${show(result.left)}`);
  }
  return result;
}

Deno.test(titled("compiler: totality — junk schemas and plans yield Either, never a raise"), () => {
  const arb = junkSchema.chain((schema) =>
    fc.record({
      schema: fc.constant(schema),
      plan: junkPlan(schema),
      name: fc.oneof(fc.constantFrom(...Object.keys(schema)), anyName),
      body: fc.oneof(fc.anything(), fc.dictionary(anyName, fc.oneof(fc.anything(), adversarialString))),
      id: fc.oneof(fc.string(), adversarialString),
    })
  );
  fc.assert(
    fc.property(arb, ({ schema, plan, name, body, id }) => {
      for (const dialect of DIALECTS) {
        const select = total(() => compileSelect(schema, plan, dialect), `compileSelect ${dialect}`);
        if (Either.isRight(select)) {
          const { text, values } = select.right;
          assert.deepEqual(auditSelect(text, dialect), [], text);
          assert.equal(placeholders(text).length, values.length, text);
        }
      }
      const ddl = total(() => compileDDL(schema), "compileDDL");
      if (Either.isRight(ddl)) {
        for (const statement of ddl.right) {
          assert.deepEqual(auditDDL(statement, enumValuesOf(schema)), [], statement);
        }
      }
      const meta = { _id: id, _creationTime: 1 };
      for (
        const [label, run] of [
          ["compileInsert", () => compileInsert(schema, name, body, meta)],
          ["compileReplace", () => compileReplace(schema, name, id, body)],
          ["compileDelete", () => compileDelete(schema, name, id)],
        ] as const
      ) {
        const write = total<SQL>(run, label);
        if (Either.isRight(write)) {
          assert.deepEqual(auditWrite(write.right.text), [], write.right.text);
          assert.equal(placeholders(write.right.text).length, write.right.values.length);
        }
      }
    }),
    runs(1000),
  );
});

// ---------------------------------------------------------------------------
// Dialect and storage agreement
// ---------------------------------------------------------------------------

Deno.test(titled("compiler: dialect agreement — same values and same filter/order/limit skeleton"), () => {
  fc.assert(
    fc.property(schemaAndPlan, ({ schema, plan }) => {
      for (const storage of STORAGES) {
        const stored = withStorage(schema, storage);
        const table = stored[plan.table];
        const lite = rightOf(compileSelect(stored, plan, "sqlite"));
        const duck = rightOf(compileSelect(stored, plan, "duckdb"));
        assert.deepEqual(duck.values, lite.values, storage);
        assert.deepEqual(
          skeleton(duck.text, "duckdb", table),
          skeleton(lite.text, "sqlite", table),
          `${storage}\n  sqlite: ${lite.text}\n  duckdb: ${duck.text}`,
        );
      }
    }),
    runs(),
  );
});

Deno.test(titled("compiler: storage agreement — columns and json layouts bind the same values"), () => {
  fc.assert(
    fc.property(schemaAndPlan, ({ schema, plan }) => {
      for (const dialect of DIALECTS) {
        const columns = rightOf(compileSelect(withStorage(schema, "columns"), plan, dialect));
        const json = rightOf(compileSelect(withStorage(schema, "json"), plan, dialect));
        assert.deepEqual(json.values, columns.values, dialect);
        assert.deepEqual(placeholders(json.text), placeholders(columns.text), dialect);
      }
    }),
    runs(),
  );
});

// ---------------------------------------------------------------------------
// Limit and aggregate semantics
// ---------------------------------------------------------------------------

/** An ordering that need not be valid in any way: unknown fields, array fields, bad directions. */
const anyOrder = fc.record({
  field: fc.oneof(anyName, fc.constantFrom("_id", "_creationTime")),
  direction: fc.oneof(fc.constantFrom("asc", "desc"), fc.string({ maxLength: 4 })) as fc.Arbitrary<"asc">,
});

Deno.test(
  titled("compiler: limit semantics — count never LIMITs, countBy and rows always do; aggregates drop order"),
  () => {
    fc.assert(
      fc.property(schemaAndPlan, anyOrder, ({ schema, plan }, order) => {
        for (const { storage, dialect, schema: stored, result } of compileAll(schema, plan)) {
          const sql = rightOf(result);
          const tokens = lex(sql.text);
          const words = tokens.map((t) => t.value);
          const where = `${storage}/${dialect}: ${sql.text}`;
          if (plan.aggregate?.kind === "count") {
            assert.ok(!words.includes("LIMIT"), `count with LIMIT: ${where}`);
            assert.ok(!words.includes("ORDER"), `count with ORDER: ${where}`);
          } else {
            // Exactly one LIMIT, last, bound to the plan's limit.
            assert.equal(words.filter((w) => w === "LIMIT").length, 1, where);
            assert.equal(tokens.at(-2)?.value, "LIMIT", where);
            assert.equal(tokens.at(-1)?.kind, "param", where);
            assert.equal(sql.values.at(-1), plan.limit, where);
          }
          if (plan.aggregate?.kind === "countBy") {
            // A countBy is ordered by its grouping key, whatever the caller asked for.
            assert.match(sql.text, / GROUP BY 1 ORDER BY "value" ASC NULLS FIRST LIMIT (\?|\$p\d+)$/, where);
          }
          if (!plan.aggregate) {
            // Every row page is ordered with the `_id` tie-breaker last, so page boundaries are deterministic.
            const limitAt = words.lastIndexOf("LIMIT");
            assert.deepEqual(words.slice(limitAt - 2, limitAt), ["_id", "ASC"], where);
          }
          if (plan.aggregate) {
            // The ordering of an aggregate plan is dropped before anything inspects it: any ordering,
            // however invalid, compiles to exactly the same statement as no ordering at all.
            const { order: _order, ...unordered } = plan;
            const withJunk = compileSelect(stored, { ...unordered, order }, dialect);
            const without = compileSelect(stored, unordered, dialect);
            assert.deepEqual(rightOf(withJunk), sql, `junk order changed ${where}`);
            assert.deepEqual(rightOf(without), sql, `order changed ${where}`);
          }
        }
      }),
      runs(),
    );
  },
);
