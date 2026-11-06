/**
 * `EventSinkTest`: every emitted event, kept in memory and queryable (plan item 20, §1.2).
 *
 * The layer provides two tags from one `Ref<EventRecord[]>`: `EventSink`, which the code under test
 * emits through, and `TestEvents`, which the test reads back through. Nothing is filtered, sampled
 * or dropped — unlike `EventSinkLive`, there is no queue to overflow — so an assertion on "exactly
 * these events" is meaningful.
 *
 * The query surface is deliberately general, because later items' tests (backups, ingest, HTTP)
 * assert on their own event kinds: `eventsOfKind("sql")` narrows to `SqlEventRecord[]`, and
 * `eventsWhere` takes any predicate.
 */
import { Context, Effect, Layer, Ref } from "effect";
import type { EventRecord } from "../../core/events.ts";
import { EventSink } from "../../platform/services.ts";

/** Every event kind, e.g. `"sql" | "tx" | "queue" | …`. */
export type EventKind = EventRecord["kind"];

/** The member of the event union with `kind: K`. */
export type EventOfKind<K extends EventKind> = Extract<EventRecord, { readonly kind: K }>;

export interface TestEventsService {
  /** Every event emitted so far, oldest first. */
  readonly all: Effect.Effect<readonly EventRecord[]>;
  /** The events of one kind, oldest first, narrowed to that kind's record type. */
  ofKind<K extends EventKind>(kind: K): Effect.Effect<readonly EventOfKind<K>[]>;
  /** The events matching `predicate`, oldest first. */
  where(predicate: (event: EventRecord) => boolean): Effect.Effect<readonly EventRecord[]>;
  /** Forget everything recorded so far, e.g. after a setup phase whose events are not under test. */
  readonly clear: Effect.Effect<void>;
}

/** The read side of `EventSinkTest`. Only present when `EventSinkTest` is. */
export class TestEvents extends Context.Tag("mansio/test/TestEvents")<TestEvents, TestEventsService>() {}

/** Builds the two services over one fresh `Ref`. */
export function makeEventSinkTest(): Effect.Effect<Context.Context<EventSink | TestEvents>> {
  return Effect.map(Ref.make<readonly EventRecord[]>([]), (ref) => {
    const where = (predicate: (event: EventRecord) => boolean) =>
      Effect.map(Ref.get(ref), (events) => events.filter(predicate));
    return Context.make(EventSink, {
      emit: (event) => Ref.update(ref, (events) => [...events, event]),
    }).pipe(
      Context.add(TestEvents, {
        all: Ref.get(ref),
        ofKind: <K extends EventKind>(kind: K) =>
          where((event) => event.kind === kind) as unknown as Effect.Effect<readonly EventOfKind<K>[]>,
        where,
        clear: Ref.set(ref, []),
      }),
    );
  });
}

/** `EventSink` into memory, plus `TestEvents` to read it back. */
export const EventSinkTest: Layer.Layer<EventSink | TestEvents> = Layer.effectContext(makeEventSinkTest());

/** Every event recorded so far. */
export const events: Effect.Effect<readonly EventRecord[], never, TestEvents> = Effect.flatMap(
  TestEvents,
  (t) => t.all,
);

/** The recorded events of one kind, e.g. `yield* eventsOfKind("sql")`. */
export function eventsOfKind<K extends EventKind>(
  kind: K,
): Effect.Effect<readonly EventOfKind<K>[], never, TestEvents> {
  return Effect.flatMap(TestEvents, (t) => t.ofKind(kind));
}

/** The recorded events matching `predicate`. */
export function eventsWhere(
  predicate: (event: EventRecord) => boolean,
): Effect.Effect<readonly EventRecord[], never, TestEvents> {
  return Effect.flatMap(TestEvents, (t) => t.where(predicate));
}

/** Forget the events recorded so far. */
export const clearEvents: Effect.Effect<void, never, TestEvents> = Effect.flatMap(TestEvents, (t) => t.clear);
