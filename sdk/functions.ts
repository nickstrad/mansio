/**
 * Handler declaration (plan item 10).
 *
 * A handler is an `Effect`, not an `async` function: it returns a description of the work, and the
 * platform decides when to run it. Two consequences app authors see directly:
 *
 * 1. Argument parsing moved into the error channel. `invoke` is
 *    `Effect<R, E | SchemaError>` — `core/schema.parseObject` returns an `Either`, which *is* an
 *    `Effect` in Effect 3, so the parse simply flat-maps into the handler. Nothing throws.
 * 2. A handler's own failures are typed. `app/functions.ts` fails with `RuleError`,
 *    `NotFoundError` and `ConflictError` instead of `throw new Error("Not your turn")`, and `E`
 *    carries them all the way out to the caller.
 *
 * Note on the type parameter order: `Definition` gained a fourth parameter for the handler's error
 * channel, after `R`. `ArgsOf`/`ResultOf`/`Names` are unchanged, and `tests/types.ts` still
 * infers (and still rejects) exactly what it did before the port.
 */
import { Effect } from "effect";
import { SchemaError } from "../core/errors.ts";
import { type Fields, type Infer, parseObject, type Schema } from "../core/schema.ts";
import type { ReadDB, WriteDB } from "./db.ts";

export type Kind = "query" | "mutation";
export type Definition<K extends Kind, A, R, E = never> = {
  kind: K;
  invoke(db: unknown, input: unknown): Effect.Effect<R, E | SchemaError>;
  readonly _args?: A;
  readonly _result?: R;
};
export type AnyDefinition = Definition<Kind, unknown, unknown, unknown>;
export type Functions = Record<string, AnyDefinition>;
export type ArgsOf<F> = F extends Definition<Kind, infer A, unknown, unknown> ? A : never;
export type ResultOf<F> = F extends Definition<Kind, unknown, infer R, unknown> ? R : never;
export type Names<F extends Functions, K extends Kind> =
  & {
    [N in keyof F]: F[N]["kind"] extends K ? N : never;
  }[keyof F]
  & string;

export function defineFunctions<S extends Schema>(_schema: S) {
  function build<K extends Kind, A extends Fields, R, E>(kind: K, config: {
    args: A;
    handler: (
      ctx: { db: K extends "query" ? ReadDB<S> : WriteDB<S> },
      args: Infer<A>,
    ) => Effect.Effect<R, E>;
  }): Definition<K, Infer<A>, R, E> {
    return {
      kind,
      invoke(db, input) {
        // `parseObject` is `Either`-returning, and an `Either` is an `Effect` whose error channel
        // is its left: the schema failure and the handler's own failures share one channel.
        // Type erasure is limited to this runtime boundary. The router chooses the adapter.
        return Effect.flatMap(
          parseObject(config.args, input),
          (args) => config.handler({ db } as Parameters<typeof config.handler>[0], args),
        );
      },
    };
  }
  return {
    query: <A extends Fields, R, E>(config: {
      args: A;
      handler: (ctx: { db: ReadDB<S> }, args: Infer<A>) => Effect.Effect<R, E>;
    }) => build("query", config),
    mutation: <A extends Fields, R, E>(config: {
      args: A;
      handler: (ctx: { db: WriteDB<S> }, args: Infer<A>) => Effect.Effect<R, E>;
    }) => build("mutation", config),
  };
}
