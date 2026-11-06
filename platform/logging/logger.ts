/**
 * The logging service (plan item 11): `EventSinkLive`, the Effect `Logger` that funnels
 * `Effect.log*` into the same records, and the layer that wires both over `Config`.
 *
 * ## The shape of it
 *
 * ```text
 *   emit(record) ─┐                        (never blocks, never fails)
 *                 ├─> Queue.dropping(capacity) ─> consumer fiber ─> FileSink ─> events-HH-pid-NNN.jsonl
 *   Effect.log* ──┘        drops counted              forkScoped        rolls on date/hour/size
 *                          and reported as a
 *                          `queue` event
 * ```
 *
 * Four decisions carry the item, each of them a way of not losing records:
 *
 * 1. **`Queue.dropping`, never `Queue.bounded`.** A mutation must never wait on, or fail because
 *    of, logging (plan item 11 step 3). Producers `unsafeOffer` and move on; an offer that does not
 *    fit increments a counter instead of applying backpressure.
 * 2. **Drops are themselves events.** Each flush that follows one or more rejected offers writes a
 *    `queue` record (`event: "dropped"`, with `depth`/`cap`/`dropped` — the §4.1 `queue_events`
 *    columns) *in its own append, before the batch*, so the report cannot be dropped by the very
 *    saturation it describes, and cannot be lost along with a failing data batch. Drop reports
 *    ignore the `logLevel` gate: silence here would defeat the purpose.
 * 3. **The consumer drains, it does not accumulate.** Every iteration takes *everything* queued
 *    (`Queue.takeAll`, which is atomic and cannot lose a value to interruption the way a timed-out
 *    `take` can), writes it in `BATCH_MAX` slices, and sleeps only when it drained less than a full
 *    slice — and only until a producer finds a full slice queued, which ends the sleep early. So
 *    there is no long-lived accumulator to grow unboundedly (sq-01 pitfall 2) and a backlog is
 *    never parked behind the flush timer, while a trickle still costs one write per `eventFlushMs`.
 * 4. **Finalizer order is load-bearing.** Finalizers run LIFO, so they are added in reverse:
 *    `close` first, `drain` second, `forkScoped`'s interrupt last. Shutdown therefore runs
 *    interrupt → drain → close: the consumer stops, what it left behind (plus anything still in the
 *    queue) is written, and only then is the handle closed. Reverse that order and the drain writes
 *    into a closed handle (sq-01 pitfall 1).
 *
 * Take-and-write is `Effect.uninterruptible`, so an interrupt arriving mid-flush cannot leave a
 * half-written batch for the drain to duplicate, nor strand a batch already taken off the queue. Signal handling is *not* wired here: graceful shutdown is
 * whatever closes this layer's scope, and the server/reader entrypoints (items 16 and 22) own the
 * SIGTERM/SIGINT wiring that gets there (sq-01 pitfall 4).
 *
 * stderr is written exactly once in this directory, in `noteFailure`: the documented degradation
 * path for when the event log itself cannot be written, rate-limited to one line per minute of wall
 * time (plan item 11 step 3). Nothing here uses the console (item 22's `platform/` grep).
 */
import {
  Cause,
  Chunk,
  Clock,
  Deferred,
  Duration,
  Effect,
  Exit,
  FiberId,
  HashMap,
  Layer,
  List,
  Logger,
  LogLevel,
  Option,
  Queue,
  type Scope,
} from "effect";
import {
  type EventRecord,
  type Level,
  lifecycleEvent,
  queueEvent,
  type Role,
  toJsonLine,
} from "../../core/events.ts";
import type { LogRole, RotationPolicy } from "../../core/logging/rotation.ts";
import { Config, EventSink, type EventSinkService } from "../services.ts";
import { type FileSink, makeFileSink, SinkIoError } from "./file_sink.ts";

/** Hard batch threshold: one append carries at most this many records (plan item 11 step 2). */
const BATCH_MAX = 256;
/** Fallback flush period for a non-finite or non-positive `EVENT_FLUSH_MS` (sq-01 pitfall 2). */
const DEFAULT_FLUSH_MS = 250;
/** Ceiling on the flush period: no configuration may park records for longer than this. */
const MAX_FLUSH_MS = 60_000;
/** Floor on the queue capacity, so a zero or negative `EVENT_QUEUE_CAPACITY` still has a queue. */
const MIN_CAPACITY = 1;
/** At most one stderr degradation line per minute of wall time. */
const STDERR_THROTTLE_MS = 60_000;
const stderrText = new TextEncoder();
/** `traceId` on sink-internal records: they belong to no request. */
const SINK_TRACE_ID = "event-sink";
/** `phase` on a log record that carries no `Effect.withLogSpan` label. */
const DEFAULT_LOG_PHASE = "log";

const LEVEL_ORDER: Record<Level, number> = { debug: 0, info: 1, warn: 2, error: 3 };

/** Ranks an unknown `level` as `info` rather than discarding the record. */
function rankOf(level: string): number {
  return LEVEL_ORDER[level as Level] ?? LEVEL_ORDER.info;
}

/** Maps the configured minimum level onto Effect's own, so `Effect.log*` is gated before it runs. */
function effectLogLevel(level: Level): LogLevel.LogLevel {
  switch (level) {
    case "debug":
      return LogLevel.Debug;
    case "info":
      return LogLevel.Info;
    case "warn":
      return LogLevel.Warning;
    case "error":
      return LogLevel.Error;
  }
}

/** Maps an Effect log level onto the four levels `core/events.ts` records. */
function levelOf(level: LogLevel.LogLevel): Level {
  switch (level._tag) {
    case "Fatal":
    case "Error":
      return "error";
    case "Warning":
      return "warn";
    case "Info":
      return "info";
    case "Debug":
    case "Trace":
    case "All":
    case "None":
      return "debug";
  }
}

/**
 * Sink-internal counters.
 *
 * Plain mutable fields rather than a `Ref` for one hard reason: Effect's `Logger` callback is
 * synchronous (`Logger.make` returns `Output`, not an `Effect`), so the log path cannot run a `Ref`
 * update. `emit` shares the same counters, which also keeps it at one synchronous step per record.
 * JS is single-threaded, so `+= 1` here is atomic.
 */
interface Counters {
  /** Records rejected by the dropping queue since the last `queue` report. */
  dropped: number;
  /** Queue depth observed at the most recent rejected offer, for the `queue` report's `depth`. */
  droppedDepth: number;
  /** Records lost to an I/O failure. Cumulative; reported through the stderr path. */
  failed: number;
  /** `Date.now()` of the last stderr line, for the one-per-minute throttle. */
  lastStderrMs: number;
}

/** Normalizes `EVENT_FLUSH_MS` into a usable period: never `0`, never `Infinity`, never `NaN`. */
function flushPeriodMs(configured: number): number {
  if (!Number.isFinite(configured) || configured <= 0) return DEFAULT_FLUSH_MS;
  return Math.min(Math.floor(configured), MAX_FLUSH_MS);
}

function queueCapacity(configured: number): number {
  if (!Number.isFinite(configured)) return MIN_CAPACITY;
  return Math.max(MIN_CAPACITY, Math.floor(configured));
}

/** ISO-8601 for an epoch-millis value that may be anything at all. */
function isoOf(ms: number): string {
  try {
    return new Date(ms).toISOString();
  } catch {
    return new Date(0).toISOString();
  }
}

/**
 * `EventSink` with `emit` typed for `core/events.ts` records. `EventSinkService` now declares the
 * same canonical type, so this is an alias kept for the existing `eventSink` accessor's callers.
 */
export type EventSinkRecordService = EventSinkService;

/** Identity: a sink service is already typed for canonical records. See `EventSinkRecordService`. */
export function asRecordSink(sink: EventSinkService): EventSinkRecordService {
  return sink;
}

/** `EventSink`, with `emit` typed for `core/events.ts` records. See `EventSinkRecordService`. */
export const eventSink: Effect.Effect<EventSinkRecordService, never, EventSink> = Effect.map(
  EventSink,
  asRecordSink,
);

/** Reads `annotations` as a plain object. Values stay `unknown`; `toJsonLine` redacts them. */
function annotationsOf(entry: Logger.Logger.Options<unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [key, value] of HashMap.toEntries(entry.annotations)) out[key] = value;
  return out;
}

function stringAnnotation(annotations: Record<string, unknown>, ...keys: string[]): string | undefined {
  for (const key of keys) {
    const value = annotations[key];
    if (typeof value === "string" && value !== "") return value;
    if (typeof value === "number") return String(value);
  }
  return undefined;
}

/**
 * Converts one Effect log entry into a `lifecycle` record, so `Effect.log*`,
 * `Effect.annotateLogs`, `Effect.withLogSpan` and fiber metadata all land in the same JSONL stream
 * as `emit`ted events (plan item 11 step 1).
 *
 * `lifecycle` is the right kind: §4.1's `lifecycle_events` is `(ts, role, pid, phase, detail JSON)`,
 * which is exactly a free-form log line. The innermost `withLogSpan` label becomes `phase`; the
 * message, every span label, the fiber id, the remaining annotations and any `Cause` go into
 * `detail`. A `traceId`/`spanId`/`appId` annotation is promoted into the envelope, which is what
 * makes an annotated log line joinable against the request that produced it.
 *
 * Total: conversion failures still produce a record, marked in `detail`.
 */
export function recordFromLogEntry(
  entry: Logger.Logger.Options<unknown>,
  role: Role,
  pid: number,
): EventRecord {
  try {
    const annotations = annotationsOf(entry);
    const spans = List.toArray(entry.spans);
    // `Effect.withLogSpan` prepends, so the head of the list is the innermost span.
    const innermost = spans.length > 0 ? spans[0].label : undefined;
    const fiber = FiberId.threadName(entry.fiberId);
    const reserved = new Set(["traceId", "trace_id", "spanId", "span_id", "appId", "app_id"]);
    const rest: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(annotations)) {
      if (!reserved.has(key)) rest[key] = value;
    }
    return lifecycleEvent({
      eventId: crypto.randomUUID(),
      ts: entry.date.toISOString(),
      level: levelOf(entry.logLevel),
      role,
      pid,
      traceId: stringAnnotation(annotations, "traceId", "trace_id") ?? fiber,
      spanId: stringAnnotation(annotations, "spanId", "span_id"),
      appId: stringAnnotation(annotations, "appId", "app_id"),
      phase: innermost ?? DEFAULT_LOG_PHASE,
      detail: {
        message: renderMessage(entry.message),
        fiber,
        spans: spans.map((span) => span.label),
        annotations: rest,
        cause: Cause.isEmpty(entry.cause) ? undefined : renderCause(entry.cause),
      },
    });
  } catch (cause) {
    return lifecycleEvent({
      eventId: crypto.randomUUID(),
      ts: isoOf(Date.now()),
      level: "error",
      role,
      pid,
      traceId: SINK_TRACE_ID,
      phase: DEFAULT_LOG_PHASE,
      detail: { conversion: "log-entry-conversion-failed", error: String(cause) },
    });
  }
}

/**
 * The logged message as it should read in `detail.message`.
 *
 * `Effect.log*` is variadic, so Effect always hands the logger an array. A single argument — the
 * overwhelmingly common case — reads better unwrapped; several stay an array, and the values keep
 * their structure (`toJsonLine` serializes and redacts them).
 */
function renderMessage(message: unknown): unknown {
  if (Array.isArray(message)) return message.length === 1 ? message[0] : message;
  return message;
}

/** `Cause.pretty` behind a guard: rendering a hostile cause must not take the logger down. */
function renderCause(cause: Cause.Cause<unknown>): string {
  try {
    return Cause.pretty(cause);
  } catch {
    return "<unrenderable-cause>";
  }
}

/** A sink failure as one stderr-sized line: the operation, the path, and what went wrong. */
function describeFailure(cause: Cause.Cause<unknown>): string {
  try {
    const failure = Option.getOrUndefined(Cause.failureOption(cause));
    if (failure instanceof SinkIoError) {
      return `${failure.operation} ${failure.path}: ${String(failure.cause)}`;
    }
  } catch {
    // Fall through to the generic rendering.
  }
  return renderCause(cause);
}

/** What one sink instance exposes: the frozen service, plus the `Logger` that shares its queue. */
interface SinkParts {
  readonly service: EventSinkService;
  readonly logger: Logger.Logger<unknown, void>;
  readonly minimumLogLevel: LogLevel.LogLevel;
}

/**
 * Builds one sink: the dropping queue, the rotating file sink, the batching fiber, the shutdown
 * finalizers, and the two entry points (`emit` and the `Logger`) that share them.
 *
 * Scoped: the returned parts are valid until the scope closes, which is what flushes and closes the
 * open file.
 */
function makeSink(role: LogRole): Effect.Effect<SinkParts, never, Config | Scope.Scope> {
  return Effect.gen(function* () {
    const logging = (yield* Config).logging;
    const policy: RotationPolicy = {
      keepDays: logging.retentionDays,
      gzipAfterDays: logging.gzipAfterDays,
      maxTotalBytes: logging.maxTotalBytes,
      maxFileBytes: logging.maxFileBytes,
    };
    const minimum = logging.level;
    const capacity = queueCapacity(logging.eventQueueCapacity);
    const periodMs = flushPeriodMs(logging.eventFlushMs);
    const pid = Deno.pid;

    const queue = yield* Queue.dropping<EventRecord>(capacity);
    const sink: FileSink = yield* makeFileSink({ dir: logging.dir, role, pid, policy });
    const counters: Counters = {
      dropped: 0,
      droppedDepth: 0,
      failed: 0,
      lastStderrMs: Number.NEGATIVE_INFINITY,
    };

    /**
     * The one documented degradation path (plan item 11 step 3). The event log is where a failure
     * would normally be reported, and the event log is what just failed, so there is nowhere left
     * but stderr — rate-limited to one line per minute of wall time (`Date.now()`, not the `Clock`:
     * this is an operator-facing safety valve, not simulated time).
     */
    const noteFailure = (cause: Cause.Cause<unknown>, lost: number): void => {
      counters.failed += lost;
      const nowMs = Date.now();
      if (nowMs - counters.lastStderrMs < STDERR_THROTTLE_MS) return;
      counters.lastStderrMs = nowMs;
      try {
        Deno.stderr.writeSync(stderrText.encode(
          `mansio event sink (${role}): ${counters.failed} record(s) lost to I/O failure; ` +
            `latest: ${describeFailure(cause)}\n`,
        ));
      } catch {
        // stderr is gone too; the loss is still counted.
      }
    };

    /** Runs one append, converting any failure or defect into a counted, reported loss. */
    const attempt = (
      write: Effect.Effect<void, unknown>,
      lost: number,
    ): Effect.Effect<boolean> =>
      Effect.map(Effect.exit(write), (exit) => {
        if (Exit.isSuccess(exit)) return true;
        noteFailure(exit.cause, lost);
        return false;
      });

    const dropReport = (dropped: number, depth: number, nowMs: number): EventRecord =>
      queueEvent({
        eventId: crypto.randomUUID(),
        ts: isoOf(nowMs),
        level: "warn",
        role,
        pid,
        traceId: SINK_TRACE_ID,
        queue: "events",
        depth,
        cap: capacity,
        event: "dropped",
        dropped,
      });

    /**
     * Writes `records`, preceded by a `queue` report for anything dropped since the last flush.
     *
     * Never fails and never interrupts mid-write. The drop report is its own append so that a
     * failing data batch cannot take the loss accounting with it; if the report itself fails to
     * land, its count is returned to the counter and retried on the next flush rather than lost.
     */
    const flush = (records: readonly EventRecord[]): Effect.Effect<void> =>
      Effect.uninterruptible(
        Effect.gen(function* () {
          const dropped = counters.dropped;
          counters.dropped = 0;
          if (dropped === 0 && records.length === 0) return;
          const nowMs = yield* Clock.currentTimeMillis;
          const now = new Date(nowMs);
          if (dropped > 0) {
            const report = dropReport(dropped, counters.droppedDepth, nowMs);
            // `lost: 0` — the count is handed back to the counter below, so a failed report is a
            // retry, not a loss.
            const ok = yield* attempt(sink.append([toJsonLine(report)], now), 0);
            if (!ok) counters.dropped += dropped;
          }
          for (let i = 0; i < records.length; i += BATCH_MAX) {
            const slice = records.slice(i, i + BATCH_MAX);
            yield* attempt(sink.append(slice.map(toJsonLine), now), slice.length);
          }
        }),
      );

    /**
     * Takes everything queued and writes it, returning how many records it took.
     *
     * Uninterruptible from the take onward: `Queue.takeAll` is a single atomic poll, and an interrupt
     * landing between it and `flush` would strand the records it just took.
     */
    const drainOnce: Effect.Effect<number> = Effect.uninterruptible(
      Effect.flatMap(Queue.takeAll(queue), (chunk) => {
        const records = Chunk.toReadonlyArray(chunk);
        return Effect.as(flush(records), records.length);
      }),
    );

    /**
     * Completed by `offer` once a full batch is waiting, so the consumer does not sleep out the flush
     * period while the queue overflows. Replaced after every pause, before the next take, so an offer
     * that lands mid-flush wakes the next pause rather than a deferred nobody awaits.
     */
    let wake = Deferred.unsafeMake<void>(FiberId.none);

    const pause: Effect.Effect<void> = Effect.zipRight(
      Effect.suspend(() => Effect.raceFirst(Effect.sleep(Duration.millis(periodMs)), Deferred.await(wake))),
      Effect.sync(() => {
        wake = Deferred.unsafeMake<void>(FiberId.none);
      }),
    );

    /**
     * The batching fiber: drain, write, and pause only when there was less than a full slice.
     *
     * A full slice means there is a backlog, and the loop goes straight round again instead of
     * waiting out the timer; a pause ends early when `offer` signals a full batch.
     */
    const consumer: Effect.Effect<never> = Effect.gen(function* () {
      while (true) {
        const drained = yield* drainOnce;
        if (drained < BATCH_MAX) yield* pause;
      }
    });

    /** Last writes of the process: whatever is still queued, then the open handle is closed. */
    const drain: Effect.Effect<void> = Effect.asVoid(drainOnce);

    // Finalizers run LIFO, so they are registered in reverse of the order they must run in:
    // `close` (added first, runs last), `drain` (added second, runs before close), and
    // `forkScoped`'s interrupt (added last, runs first). Shutdown is therefore
    // interrupt consumer -> drain queue -> close file (sq-01 pitfall 1).
    //
    // After the scope closes there is no consumer and no open file, so a record emitted past that
    // point is not written. That is the shutdown boundary, not a drop to account for: by then the
    // process is on its way out and the queue it would land in is gone with its reader.
    yield* Effect.addFinalizer(() => Effect.ignore(sink.close));
    yield* Effect.addFinalizer(() => drain);
    yield* Effect.forkScoped(consumer);

    /**
     * Offers one record, synchronously. The single write path shared by `emit` and the `Logger`.
     *
     * Returns nothing and throws nothing: a record below the configured level is skipped, a record
     * the queue will not take is counted. `unsafeOffer` is what keeps this one step with no fiber
     * yield, which is what "logging never blocks a mutation" means in practice. Waking the consumer
     * is synchronous too: `unsafeDone` on an already-completed deferred is a no-op.
     */
    const offer = (record: EventRecord): void => {
      try {
        if (rankOf(record.level) < LEVEL_ORDER[minimum]) return;
        const accepted = Queue.unsafeOffer(queue, record);
        const depth = Option.getOrElse(queue.unsafeSize(), () => capacity);
        if (!accepted) {
          counters.dropped += 1;
          // Depth as it was when the record was rejected, not as the next flush finds it: by then
          // the consumer has drained the queue and the number would understate the saturation.
          counters.droppedDepth = depth;
        }
        if (depth >= BATCH_MAX) Deferred.unsafeDone(wake, Effect.void);
      } catch {
        // Unreachable in practice (the gate reads one field and the offer cannot fail), but `emit`
        // promises `Effect<void, never>`, so even a hostile record only costs a counted loss.
        counters.dropped += 1;
      }
    };

    const service: EventSinkService = {
      emit: (event: EventRecord): Effect.Effect<void> =>
        Effect.sync(() => {
          // Identity, deliberately: `sqlEvent`'s bind-value marker is a symbol property that a
          // spread would strip, silently turning an authorized `bindValues` into `null`.
          offer(event);
        }),
    };

    const logger = Logger.make<unknown, void>((entry) => {
      offer(recordFromLogEntry(entry, role, pid));
    });

    return { service, logger, minimumLogLevel: effectLogLevel(minimum) };
  });
}

/**
 * The event sink alone: `Queue.dropping` → batching fiber → rotating JSONL, for the given role.
 *
 * Use `loggingLayer` in a process entrypoint; this exists for callers (and tests) that want the
 * `EventSink` service without taking over Effect's default logger.
 */
export function eventSinkLayer(role: LogRole): Layer.Layer<EventSink, never, Config> {
  return Layer.scoped(EventSink, Effect.map(makeSink(role), (parts) => parts.service));
}

/**
 * The whole logging stack for one role: the `EventSink` service, `Effect`'s default logger replaced
 * by the JSONL logger that shares its queue, and the minimum log level from `LOG_LEVEL`.
 *
 * One sink instance backs all three, so `Effect.log*` and `EventSink.emit` interleave into the same
 * file in emission order.
 */
export function loggingLayer(role: LogRole): Layer.Layer<EventSink, never, Config> {
  return Layer.unwrapScoped(
    Effect.map(makeSink(role), (parts) =>
      Layer.mergeAll(
        Layer.succeed(EventSink, parts.service),
        Logger.replace(Logger.defaultLogger, parts.logger),
        Logger.minimumLogLevel(parts.minimumLogLevel),
      )),
  );
}

/** The writer process's sink: `<LOG_DIR>/server/<date>/events-<HH>-<pid>-<seq>.jsonl`. */
export const EventSinkLive: Layer.Layer<EventSink, never, Config> = eventSinkLayer("server");

/** The reader process's sink. A separate file from the server's, by role and by pid (step 4). */
export const EventSinkReaderLive: Layer.Layer<EventSink, never, Config> = eventSinkLayer("reader");

/** Sink + `Logger.replace` + minimum level, for the server entrypoint (item 22). */
export const LoggingLive: Layer.Layer<EventSink, never, Config> = loggingLayer("server");

/** Sink + `Logger.replace` + minimum level, for the reader entrypoint (item 16). */
export const LoggingReaderLive: Layer.Layer<EventSink, never, Config> = loggingLayer("reader");
