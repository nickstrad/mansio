/**
 * The test layers (plan item 20): in-memory doubles for the services whose live layer touches a file or
 * the wall clock.
 *
 * | Service     | Double                                     | Read-back / control tag        |
 * | ----------- | ------------------------------------------ | ------------------------------ |
 * | `EventSink` | `EventSinkTest`                            | `TestEvents`                   |
 * | `Clock`     | Effect's `TestClock`, via `TestContext`    | `TestClock.adjust` / `setTime` |
 * | `Config`    | `ConfigTest()` from `eventlog/config.ts`   | —                              |
 *
 * `TestLayer` is the default stack `tests/helpers/run.ts` provides to every test. None of these layers
 * opens a file or spawns a process. (The storage, reader, object-store and id doubles went with the Deno
 * platform at IR-6.)
 */
import { Layer, TestContext, type TestServices } from "effect";
import { ConfigTest } from "../../eventlog/config.ts";
import type { Config, EventSink } from "../../eventlog/services.ts";
import { EventSinkTest, type TestEvents } from "./event_sink_test_layer.ts";

export * from "./event_sink_test_layer.ts";

/** What `TestLayer` provides. */
export type DefaultTestServices = EventSink | TestEvents | Config | TestServices.TestServices;

/**
 * The default test stack. `TestContext` swaps Effect's `Clock` for `TestClock` (time starts at 0 and
 * moves only when a test moves it), so every event timestamp is deterministic. Each build is fresh: the
 * event log starts empty.
 */
export const TestLayer: Layer.Layer<DefaultTestServices> = Layer.mergeAll(
  EventSinkTest,
  ConfigTest(),
  TestContext.TestContext,
);
