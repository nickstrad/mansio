/**
 * The app-facing document API.
 *
 * The intermediate representation below — `Filter`, `SelectPlan`, `Operation` — is the stable
 * contract shared by the SDK, the compiler (`core/compile`, and its Go port in `server/`) and
 * `scripts/explain.ts`. The Effect port leaves it exactly as it was; `core/compile/GENERATED_SQL.txt` is the
 * canary that proves it. There is still no SQL in the public SDK.
 *
 * What changed is the seam underneath it: `Execute` returns an `Effect` carrying
 * `DbError` in its error channel instead of a `Promise` that rejects, so every `Find` and
 * `Documents` method now *describes* a read or a write rather than starting one. Nothing in this
 * module runs an effect; the runtime that supplies `Execute` (the recording executor in
 * `sdk/recording.ts`) decides when and where the work happens.
 */
import { Effect } from "effect";
import type { DbError } from "../core/errors.ts";
import { MAX_LIMIT, MIN_LIMIT } from "../core/plan.ts";
import type { Body, Document, Schema } from "./schema.ts";

/** This is the intermediate representation. There is no SQL in the public SDK. */
export type Filter = { field: string; op: "eq" | "gt"; value: unknown };
export type SelectPlan = {
  kind: "select";
  table: string;
  filters: Filter[];
  order?: { field: string; direction: "asc" | "desc" };
  limit: number;
  aggregate?: { kind: "count" } | { kind: "countBy"; field: string };
};
export type Operation =
  | SelectPlan
  | { kind: "insert"; table: string; value: unknown }
  | { kind: "patch"; table: string; id: string; value: unknown }
  | { kind: "delete"; table: string; id: string };
/**
 * `guarded` is for a select only: false when the caller opted out of guarding it (`Find.unguarded`). An
 * executor without guards ignores it.
 */
export type Execute = (operation: Operation, guarded?: boolean) => Effect.Effect<unknown, DbError>;
type ScalarKey<T> =
  & { [K in keyof T]: T[K] extends string | number | boolean | null ? K : never }[keyof T]
  & string;
type NumericKey<T> = { [K in keyof T]: T[K] extends number ? K : never }[keyof T] & string;
export type Equality<T> = Partial<Pick<T, ScalarKey<T>>>;

/**
 * A chainable select. The builder methods stay synchronous — they only rewrite the plan — and the
 * four terminal methods return effects.
 */
export class Find<T> {
  constructor(private execute: Execute, readonly plan: SelectPlan, private guarded = true) {}
  private with(plan: SelectPlan): Find<T> {
    return new Find(this.execute, plan, this.guarded);
  }
  where<K extends ScalarKey<T>>(field: K, value: T[K]): Find<T> {
    return this.with({ ...this.plan, filters: [...this.plan.filters, { field, op: "eq", value }] });
  }
  gt<K extends NumericKey<T>>(field: K, value: number): Find<T> {
    return this.with({ ...this.plan, filters: [...this.plan.filters, { field, op: "gt", value }] });
  }
  sort(field: ScalarKey<T>, direction: "asc" | "desc" = "asc"): Find<T> {
    return this.with({ ...this.plan, order: { field, direction } });
  }
  /**
   * Opts this select out of its guard. By default what a `find` answers is part of the run's read set: a
   * mutation that decided on it ("no row matches, so insert one") commits only if the table has not
   * changed since, and re-runs otherwise. Use `unguarded()` for a read that is only shown, not decided
   * on, when the table is busy and the re-runs cost more than the protection is worth.
   */
  unguarded(): Find<T> {
    return new Find(this.execute, this.plan, false);
  }
  /**
   * The limit rule is still enforced here, and still by raising: a bad limit is a programming
   * error in app code, not a request failure. The bounds are `core/plan.ts`'s, which `normalise`
   * checks again (as a `CompileError`) for plans that do not come through this builder.
   */
  limit(limit: number): Find<T> {
    if (!Number.isInteger(limit) || limit < MIN_LIMIT || limit > MAX_LIMIT) {
      throw new Error(`Limit must be ${MIN_LIMIT}..${MAX_LIMIT}`);
    }
    return this.with({ ...this.plan, limit });
  }
  collect(): Effect.Effect<T[], DbError> {
    return Effect.map(this.execute(this.plan, this.guarded), (rows) => rows as T[]);
  }
  first(): Effect.Effect<T | null, DbError> {
    return Effect.map(this.limit(1).collect(), (rows) => rows[0] ?? null);
  }
  count(): Effect.Effect<number, DbError> {
    const plan: SelectPlan = { ...this.plan, order: undefined, aggregate: { kind: "count" } };
    return Effect.map(this.execute(plan, this.guarded), (value) => value as number);
  }
  countBy<K extends ScalarKey<T>>(field: K): Effect.Effect<{ value: T[K]; count: number }[], DbError> {
    const plan: SelectPlan = { ...this.plan, order: undefined, aggregate: { kind: "countBy", field } };
    return Effect.map(this.execute(plan, this.guarded), (rows) => rows as { value: T[K]; count: number }[]);
  }
}

export interface ReadDB<S extends Schema> {
  get<K extends keyof S & string>(table: K, id: string): Effect.Effect<Document<S, K> | null, DbError>;
  /** The documents with `ids`, in that order (`null` where there is none), read together. */
  getMany<K extends keyof S & string>(
    table: K,
    ids: readonly string[],
  ): Effect.Effect<(Document<S, K> | null)[], DbError>;
  find<K extends keyof S & string>(table: K, where?: Equality<Document<S, K>>): Find<Document<S, K>>;
}
export interface WriteDB<S extends Schema> extends ReadDB<S> {
  insert<K extends keyof S & string>(table: K, value: Body<S, K>): Effect.Effect<string, DbError>;
  patch<K extends keyof S & string>(
    table: K,
    id: string,
    value: Partial<Body<S, K>>,
  ): Effect.Effect<void, DbError>;
  delete<K extends keyof S & string>(table: K, id: string): Effect.Effect<void, DbError>;
}

/** The runtime injects an executor. App code only receives one of the interfaces. */
export class Documents<S extends Schema> implements WriteDB<S> {
  constructor(private execute: Execute) {}
  find<K extends keyof S & string>(table: K, where: Equality<Document<S, K>> = {}): Find<Document<S, K>> {
    return new Find(this.execute, {
      kind: "select",
      table,
      limit: 100,
      filters: Object.entries(where).map(([field, value]) => ({ field, op: "eq", value })),
    });
  }
  get<K extends keyof S & string>(table: K, id: string): Effect.Effect<Document<S, K> | null, DbError> {
    const plan: SelectPlan = {
      kind: "select",
      table,
      filters: [{ field: "_id", op: "eq", value: id }],
      limit: 1,
    };
    // Type erasure is limited to these four boundaries: the engines answer `unknown`.
    return Effect.map(this.execute(plan), (rows) => (rows as Document<S, K>[])[0] ?? null);
  }
  getMany<K extends keyof S & string>(
    table: K,
    ids: readonly string[],
  ): Effect.Effect<(Document<S, K> | null)[], DbError> {
    return Effect.all(ids.map((id) => this.get(table, id)));
  }
  insert<K extends keyof S & string>(table: K, value: Body<S, K>): Effect.Effect<string, DbError> {
    return Effect.map(this.execute({ kind: "insert", table, value }), (id) => id as string);
  }
  patch<K extends keyof S & string>(
    table: K,
    id: string,
    value: Partial<Body<S, K>>,
  ): Effect.Effect<void, DbError> {
    return Effect.asVoid(this.execute({ kind: "patch", table, id, value }));
  }
  delete<K extends keyof S & string>(table: K, id: string): Effect.Effect<void, DbError> {
    return Effect.asVoid(this.execute({ kind: "delete", table, id }));
  }
}
