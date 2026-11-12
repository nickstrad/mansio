/**
 * The Effect test runner: tests are effects, `node:test` still runs them.
 *
 * `runTest(name, effect)` registers a `node:test` case that provides `TestLayer` (every in-memory
 * double plus `TestClock`) and a `Scope`, runs the effect with `Effect.runPromiseExit`, and turns a
 * failed exit into a thrown error. A thrown `assert` failure inside the effect is a defect, and is
 * rethrown as-is so `node:test` prints its diff; any other failure is rethrown with the whole
 * `Cause` rendered, so a typed error that has no `message` is still readable.
 *
 * Per-test services go in `options.layer`, built on top of `TestLayer` (so they may require any of
 * its services), e.g. a layer over `ConfigTest`. The layer's outputs are inferred from
 * `options.layer` only, so an effect that needs a service nobody provided is a type error, not a
 * runtime "service not found".
 */
import { test } from "node:test";
import { Cause, Effect, Exit, Layer, type Scope } from "effect";
import { type DefaultTestServices, TestLayer } from "../layers/index.ts";

export interface RunTestOptions<ROut, LE> {
  /** Extra services for this test, built over `TestLayer`. */
  readonly layer?: Layer.Layer<ROut, LE, DefaultTestServices>;
  readonly skip?: boolean | string;
  readonly timeout?: number;
}

/** What an effect run by `runTest` may require. */
export type TestRequirements<ROut> = ROut | DefaultTestServices | Scope.Scope;

/** `effect` with `TestLayer`, the optional extra layer, and a scope provided. */
export function provideTest<A, E, ROut = never, LE = never>(
  effect: Effect.Effect<A, E, NoInfer<TestRequirements<ROut>>>,
  layer?: Layer.Layer<ROut, LE, DefaultTestServices>,
): Effect.Effect<A, E | LE> {
  const stack = layer
    ? Layer.provideMerge(layer, TestLayer)
    : (TestLayer as unknown as Layer.Layer<ROut | DefaultTestServices, LE>);
  const scoped = Effect.scoped(effect as Effect.Effect<A, E, TestRequirements<ROut>>);
  return Effect.provide(scoped, stack) as Effect.Effect<A, E | LE>;
}

/** Runs `effect` under the test stack; resolves with its value or throws its failure. */
export async function runEffect<A, E, ROut = never, LE = never>(
  effect: Effect.Effect<A, E, NoInfer<TestRequirements<ROut>>>,
  layer?: Layer.Layer<ROut, LE, DefaultTestServices>,
): Promise<A> {
  const exit = await Effect.runPromiseExit(provideTest(effect, layer));
  if (Exit.isSuccess(exit)) return exit.value;
  const failure = Cause.squash(exit.cause);
  if (failure instanceof Error && failure.name === "AssertionError") throw failure;
  throw new Error(Cause.pretty(exit.cause), { cause: failure });
}

/** Registers `effect` as a `node:test` case. */
export function runTest<ROut = never, LE = never>(
  name: string,
  effect: Effect.Effect<unknown, unknown, NoInfer<TestRequirements<ROut>>>,
  options: RunTestOptions<ROut, LE> = {},
): void {
  test(name, { skip: options.skip, timeout: options.timeout }, async () => {
    await runEffect(effect, options.layer);
  });
}
