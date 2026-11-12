/**
 * Property tests for `core/plan.ts` and for the plan generators themselves.
 *
 * `normalise` is where the limit rule and the aggregate-drops-order rule live, and it is the only
 * copy of either rule: `compileSelect` must reject a limit exactly when `normalise` does. The second
 * half of this file pins the plan generators the other suites build on — a valid plan compiles in
 * every dialect and layout, and a plan with one injected defect fails with that defect's reason in
 * every dialect and layout. Without that, a generator that quietly produced only easy plans would
 * make every other property vacuous.
 */
import assert from "node:assert/strict";
import fc from "fast-check";
import { Either } from "effect";
import { withStorage } from "../../core/schema.ts";
import { MAX_LIMIT, MIN_LIMIT, normalise, type SelectPlan } from "../../core/plan.ts";
import type { Dialect } from "../../core/compile/dialect.ts";
import { compileSelect } from "../../core/compile/select.ts";
import {
  anyName,
  leftOf,
  rightOf,
  runs,
  schemaAndInvalidPlan,
  schemaAndPlan,
  show,
} from "./compiler_arbitraries.ts";
import { titled } from "./arbitraries.ts";

const DIALECTS: readonly Dialect[] = ["sqlite", "duckdb"];
const STORAGES = ["columns", "json"] as const;

/** Any number a caller could put in `limit`, dense around both bounds. */
const anyLimit = fc.oneof(
  fc.integer({ min: MIN_LIMIT - 3, max: MIN_LIMIT + 3 }),
  fc.integer({ min: MAX_LIMIT - 3, max: MAX_LIMIT + 3 }),
  fc.integer(),
  fc.double(),
  fc.constantFrom(
    -0,
    0.5,
    999.5,
    1000.0000001,
    NaN,
    Infinity,
    -Infinity,
    Number.MAX_SAFE_INTEGER,
    Number.EPSILON,
  ),
);

/** The limit rule, stated independently of `normalise`: a whole number of rows from 1 to 1000. */
function legalLimit(limit: number): boolean {
  return Number.isFinite(limit) && Math.trunc(limit) === limit && limit >= 1 && limit <= 1000;
}

Deno.test(titled("plan: normalise accepts exactly the whole-number limits 1..1000"), () => {
  fc.assert(
    fc.property(schemaAndPlan, anyLimit, ({ plan }, limit) => {
      const result = normalise({ ...plan, limit });
      assert.equal(Either.isRight(result), legalLimit(limit), `limit ${limit}`);
      if (Either.isLeft(result)) {
        assert.equal(result.left._tag, "CompileError");
        assert.equal(result.left.reason, "invalid-limit");
        assert.equal(result.left.table, plan.table);
      } else {
        assert.equal(result.right.limit, limit);
      }
    }),
    runs(1000),
  );
});

Deno.test(titled("plan: normalise drops the ordering of aggregates and touches nothing else"), () => {
  const order = fc.record({ field: anyName, direction: fc.constantFrom("asc" as const, "desc" as const) });
  fc.assert(
    fc.property(schemaAndPlan, fc.option(order, { nil: undefined }), ({ plan }, extraOrder) => {
      const input: SelectPlan = extraOrder ? { ...plan, order: extraOrder } : plan;
      // A serialised snapshot, not `structuredClone`: generated records may have a null prototype.
      const before = show(input);
      const out = rightOf(normalise(input));
      assert.equal(show(input), before, "normalise mutated its input");
      const { order: outOrder, ...outRest } = out;
      const { order: inOrder, ...inRest } = input;
      assert.deepEqual(outRest, inRest);
      assert.deepEqual(outOrder, input.aggregate ? undefined : inOrder);
      // Idempotent: a normalised plan is its own normal form.
      assert.deepEqual(rightOf(normalise(out)), out);
    }),
    runs(),
  );
});

Deno.test(
  titled("plan: compileSelect rejects a limit exactly when normalise does (one copy of the rule)"),
  () => {
    fc.assert(
      fc.property(schemaAndPlan, anyLimit, ({ schema, plan }, limit) => {
        const candidate = { ...plan, limit };
        const normalised = normalise(candidate);
        for (const dialect of DIALECTS) {
          const compiled = compileSelect(schema, candidate, dialect);
          if (Either.isRight(normalised)) {
            assert.ok(Either.isRight(compiled), `${dialect} rejected limit ${limit}: ${show(compiled)}`);
          } else {
            assert.deepEqual(leftOf(compiled), normalised.left, dialect);
          }
        }
      }),
      runs(),
    );
  },
);

Deno.test(titled("plan: generated valid plans compile in every dialect and layout"), () => {
  fc.assert(
    fc.property(schemaAndPlan, ({ schema, plan }) => {
      for (const storage of STORAGES) {
        for (const dialect of DIALECTS) rightOf(compileSelect(withStorage(schema, storage), plan, dialect));
      }
    }),
    runs(),
  );
});

Deno.test(
  titled("plan: a plan with one defect fails with that defect's reason in every dialect and layout"),
  () => {
    const reasons = new Set<string>();
    fc.assert(
      fc.property(schemaAndInvalidPlan, ({ schema, plan, expected }) => {
        const want = `${expected.tag}/${expected.reason}`;
        reasons.add(want);
        for (const storage of STORAGES) {
          for (const dialect of DIALECTS) {
            const error = leftOf(compileSelect(withStorage(schema, storage), plan, dialect)) as {
              _tag: string;
              reason: string;
            };
            assert.equal(`${error._tag}/${error.reason}`, want, `${storage}/${dialect}: ${expected.defect}`);
          }
        }
      }),
      runs(500),
    );
    // Every defect class the generator knows about must actually have been exercised.
    assert.deepEqual([...reasons].sort(), [
      "CompileError/array-predicate",
      "CompileError/gt-requires-numeric",
      "CompileError/invalid-limit",
      "CompileError/invalid-ordering",
      "CompileError/unknown-collection",
      "CompileError/unknown-field",
      "CompileError/unsupported-operator",
      "SchemaError/type-mismatch",
    ]);
  },
);
