/**
 * The logical read plan, and its **total** normalisation.
 *
 * This module is PURE, per the `core/` rule in `docs/adr/0001-effect-port.md`: it imports `effect`
 * and `./errors.ts`, and nothing else. Nothing in here raises; the one rejection is a
 * `CompileError` in the left channel of an `Either`.
 *
 * `normalise` is the single place two rules live that used to be spread across the tree:
 *
 * 1. **The limit rule.** `1..1000`, integer. It was duplicated in `sdk/db.ts`'s `Find.limit`
 *    (rejecting early, with a caller-facing message) and in the pre-port compiler's
 *    `compileSelect` (rejecting late, defending the SQL). `compileSelect` now gets the rule from
 *    here, so the compiler and the SDK can never drift apart on what a legal limit is.
 * 2. **Aggregates have no ordering.** `Find.count`/`Find.countBy` hand-build `order: undefined`
 *    today, and `compileSelect` separately ignores `plan.order` whenever `plan.aggregate` is set.
 *    Normalising it here makes the plan that reaches the compiler say what it means: a `count` has
 *    no row order to speak of, and a `countBy` is ordered by its grouping key, not by the caller's
 *    sort.
 *
 * Note on the type parameter order: Effect 3's `Either` is value-first — `Either<A, E>` — not
 * error-first (`Either<CompileError, SelectPlan>`).
 * The code below uses the real Effect order, as `core/schema.ts` does.
 */
import { Either } from "effect";
import { CompileError } from "./errors.ts";

/**
 * The intermediate representation, mirroring `sdk/db.ts`'s `Filter`/`SelectPlan` **without
 * importing them** — `core/` may not depend on `sdk/`, the same reason `core/errors.ts` mirrors
 * `sdk/functions.ts`'s `Kind` as `HandlerKind`. The two spellings are structurally identical and
 * `tests/unit/compile_test.ts` asserts their mutual assignability, so a change to one that is not
 * mirrored here fails the test run rather than drifting silently.
 */
export type Filter = { field: string; op: "eq" | "gt"; value: unknown };

export type SelectPlan = {
  kind: "select";
  table: string;
  filters: Filter[];
  order?: { field: string; direction: "asc" | "desc" };
  limit: number;
  aggregate?: { kind: "count" } | { kind: "countBy"; field: string };
};

/** The inclusive bounds every plan's `limit` must sit within. The MVP has no pagination cursor. */
// `sdk/db.ts`'s `Find.limit` imports these too, so the builder and `normalise` cannot drift.
export const MIN_LIMIT = 1;
export const MAX_LIMIT = 1000;

/**
 * Checks a plan's limit and drops the ordering of an aggregate plan, returning the plan the
 * compiler should work from. Everything else is passed through untouched: normalisation is not
 * validation of fields or operators, which needs the schema and happens in `core/compile/select.ts`.
 */
export function normalise(plan: SelectPlan): Either.Either<SelectPlan, CompileError> {
  if (!Number.isInteger(plan.limit) || plan.limit < MIN_LIMIT || plan.limit > MAX_LIMIT) {
    return Either.left(
      new CompileError({
        reason: "invalid-limit",
        table: plan.table,
        detail: `${MIN_LIMIT}..${MAX_LIMIT}, got ${plan.limit}`,
      }),
    );
  }
  if (plan.aggregate !== undefined && plan.order !== undefined) {
    return Either.right({ ...plan, order: undefined });
  }
  return Either.right(plan);
}
