/**
 * `ObjectStoreTest`: an in-memory object store with injectable failures (plan item 20, §1.2).
 *
 * Objects live in a `Ref<HashMap<string, Uint8Array>>`. Bytes are copied on the way in and on the
 * way out, so a caller that mutates a buffer after `put` (or the one `get` returned) cannot reach
 * the stored object — the same isolation a real store gives.
 *
 * Keys are validated with `platform/object_store.ts`'s own `keyFailure`, so a key the live layers
 * reject with `InvalidKeyError` is rejected here too, with the same `reason`. A missing key fails
 * `ObjectStoreError({ operation: "get", status: 404 })`, the shape the S3 layer produces.
 *
 * Failure injection, for the backup tests (item 19) that must prove a failed upload never advances
 * `latest.json`:
 *
 * - `failures` (layer option) or `failNext` (at run time) fails matching operations a given number of
 *   times with an `ObjectStoreError`, before anything is stored.
 * - `intercept` runs an arbitrary effect before each operation — sleep, await a `Deferred`, or fail —
 *   which is how a test parks an upload mid-flight in order to interrupt it.
 *
 * An injected failure or an interceptor runs after key validation and before the store changes, so
 * a failed `put` never leaves a partial object behind.
 *
 * **No events, no ids.** The live layers (`platform/object_store.ts`'s `instrument`) emit one event
 * per operation, whatever its outcome and even for a rejected key — a `backup` event (phase
 * `put-object`) for `put`, a `lifecycle` event (phase `object-get`) for `get` — and draw that
 * event's `eventId` from `IdGen`. This double does neither: it requires neither `EventSink` nor
 * `IdGen`, so it emits nothing and spends no id. Two consequences for tests built on it (item 19's
 * among them): per-phase event assertions must not expect `put-object`/`object-get` events, and in
 * a test that mixes object-store calls with other `IdGen` consumers, the `id-N` sequence is the one
 * live would produce minus one id per object-store operation. Use `calls` to assert on the
 * operations themselves; assert on the events against a live layer.
 */
import { Context, Effect, HashMap, Layer, Option, Ref } from "effect";
import { InvalidKeyError, ObjectStoreError } from "../../core/errors.ts";
import { keyFailure } from "../../platform/object_store.ts";
import { ObjectStore, type ObjectStoreService } from "../../platform/services.ts";

export type ObjectOperation = "put" | "get";

/** Which operations an injected failure applies to. Absent fields match everything. */
export interface InjectedFailure {
  readonly operation?: ObjectOperation;
  /** An exact key, a pattern, or a predicate. */
  readonly key?: string | RegExp | ((key: string) => boolean);
  /** How many matching operations fail before the injection is spent. Default 1; `Infinity` never expires. */
  readonly times?: number;
  /** HTTP-style status on the `ObjectStoreError`. Default 503. */
  readonly status?: number;
  readonly detail?: string;
}

/** Runs before every validated operation. Failing it fails the operation; `bytes` is present on `put`. */
export type Interceptor = (
  operation: ObjectOperation,
  key: string,
  bytes?: Uint8Array,
) => Effect.Effect<void, ObjectStoreError>;

/** One operation that passed key validation, in call order (whether or not it then failed). */
export interface ObjectCall {
  readonly operation: ObjectOperation;
  readonly key: string;
}

export interface ObjectStoreTestService {
  /** The store itself. Prefer `keys`/`read` for assertions; seed it with `Ref.update` if needed. */
  readonly objects: Ref.Ref<HashMap.HashMap<string, Uint8Array>>;
  /** Every stored key, sorted. */
  readonly keys: Effect.Effect<readonly string[]>;
  /** A copy of the stored bytes, without going through (or counting as) a `get`. */
  read(key: string): Effect.Effect<Option.Option<Uint8Array>>;
  /** Arm a failure for the next matching operation(s). */
  failNext(failure?: InjectedFailure): Effect.Effect<void>;
  /** Install (or, with `undefined`, remove) the interceptor. */
  intercept(interceptor: Interceptor | undefined): Effect.Effect<void>;
  /** Every operation that passed key validation, oldest first. */
  readonly calls: Effect.Effect<readonly ObjectCall[]>;
}

/** The control side of `ObjectStoreTest`. Only present when `ObjectStoreTest` is. */
export class ObjectStoreTestControl extends Context.Tag("mansio/test/ObjectStoreTestControl")<
  ObjectStoreTestControl,
  ObjectStoreTestService
>() {}

export interface ObjectStoreTestOptions {
  /** Objects present before the first operation. */
  readonly initial?: Readonly<Record<string, Uint8Array>>;
  /** Failures armed before the first operation. */
  readonly failures?: readonly InjectedFailure[];
}

interface Armed {
  readonly failure: InjectedFailure;
  readonly remaining: number;
}

function keyMatches(pattern: InjectedFailure["key"], key: string): boolean {
  if (pattern === undefined) return true;
  if (typeof pattern === "string") return pattern === key;
  if (pattern instanceof RegExp) return pattern.test(key);
  return pattern(key);
}

const copy = (bytes: Uint8Array): Uint8Array => bytes.slice();

/** Builds both services over fresh state. */
export function makeObjectStoreTest(
  options: ObjectStoreTestOptions = {},
): Effect.Effect<Context.Context<ObjectStore | ObjectStoreTestControl>> {
  return Effect.gen(function* () {
    const objects = yield* Ref.make(
      HashMap.fromIterable(Object.entries(options.initial ?? {}).map(([k, v]) => [k, copy(v)] as const)),
    );
    const armed = yield* Ref.make<readonly Armed[]>(
      (options.failures ?? []).map((failure) => ({ failure, remaining: failure.times ?? 1 })),
    );
    const interceptor = yield* Ref.make<Interceptor | undefined>(undefined);
    const calls = yield* Ref.make<readonly ObjectCall[]>([]);

    const validated = (key: string): Effect.Effect<string, InvalidKeyError> => {
      const reason = keyFailure(key);
      return reason === undefined ? Effect.succeed(key) : Effect.fail(new InvalidKeyError({ key, reason }));
    };

    /** Records the call, then spends the first matching armed failure, then runs the interceptor. */
    const before = (operation: ObjectOperation, key: string, bytes?: Uint8Array) =>
      Effect.gen(function* () {
        yield* Ref.update(calls, (all) => [...all, { operation, key }]);
        const hit = yield* Ref.modify(armed, (all) => {
          const at = all.findIndex((a) =>
            (a.failure.operation ?? operation) === operation && keyMatches(a.failure.key, key)
          );
          if (at < 0) return [undefined, all] as const;
          const spent = all
            .map((a, i) => (i === at ? { ...a, remaining: a.remaining - 1 } : a))
            .filter((a) => a.remaining > 0);
          return [all[at].failure, spent] as const;
        });
        if (hit) {
          return yield* new ObjectStoreError({
            operation,
            key,
            status: hit.status ?? 503,
            detail: hit.detail ?? "injected failure",
          });
        }
        const intercept = yield* Ref.get(interceptor);
        if (intercept) yield* intercept(operation, key, bytes);
      });

    const store: ObjectStoreService = {
      put: (key, bytes) =>
        Effect.gen(function* () {
          yield* validated(key);
          // Copy before any interceptor can yield: the caller may reuse its buffer meanwhile.
          const owned = copy(bytes);
          yield* before("put", key, owned);
          yield* Ref.update(objects, HashMap.set(key, owned));
        }),
      get: (key) =>
        Effect.gen(function* () {
          yield* validated(key);
          yield* before("get", key);
          const found = HashMap.get(yield* Ref.get(objects), key);
          if (Option.isNone(found)) {
            return yield* new ObjectStoreError({ operation: "get", key, status: 404, detail: "NoSuchKey" });
          }
          return copy(found.value);
        }),
    };

    const control: ObjectStoreTestService = {
      objects,
      keys: Effect.map(Ref.get(objects), (map) => [...HashMap.keys(map)].sort()),
      read: (key) => Effect.map(Ref.get(objects), (map) => Option.map(HashMap.get(map, key), copy)),
      failNext: (failure = {}) =>
        Ref.update(armed, (all) => [...all, { failure, remaining: failure.times ?? 1 }]),
      intercept: (next) => Ref.set(interceptor, next),
      calls: Ref.get(calls),
    };

    return Context.make(ObjectStore, store).pipe(Context.add(ObjectStoreTestControl, control));
  });
}

/** `ObjectStore` in memory, plus `ObjectStoreTestControl` for seeding, inspection and failure injection. */
export function ObjectStoreTest(
  options: ObjectStoreTestOptions = {},
): Layer.Layer<ObjectStore | ObjectStoreTestControl> {
  return Layer.effectContext(makeObjectStoreTest(options));
}
