/**
 * The server (writer) process entrypoint (plan items 22 and 32): `PlatformLive`, one HTTP listener
 * over `platform/http/router.ts`, the ingest-then-sweep and backup timers, and shutdown.
 *
 * Everything runs in one root fiber over one `Scope`. Resources are acquired in the order below, so
 * closing the scope releases them in exactly the reverse order:
 *
 * ```text
 * main          stop signal (SIGINT/SIGTERM listeners)
 * PlatformLive  Config → logging → shutdown marker → IdGen → ObjectStore, AppRegistry
 *               → per-app Storage + Backups → ReaderClient (child process)      (platform/layers.ts)
 * program       reader ping → ingest-then-sweep fiber → backup fiber → HTTP listener
 *
 * shutdown:     HTTP drain → backup fiber → ingest-then-sweep fiber → reader child → DB handles
 *               → `lifecycle` shutdown event → log drain + close → signal listeners
 * ```
 *
 * - **HTTP drain first.** `server.shutdown()` stops accepting and waits for in-flight requests, so a
 *   mutation that was admitted completes (or rolls back on its own failure) against a still-open
 *   database before anything below it closes. The storage finalizer additionally waits for its
 *   writer queue to drain. The reader child runs in its own process group (`platform/ipc.ts`), so a
 *   group signal (Ctrl-C, a systemd stop) reaches only this process and cannot stop the reader
 *   ahead of the drain.
 * - **Signals interrupt the root fiber.** SIGINT/SIGTERM complete a `Deferred`. Once serving, the
 *   program emits a `lifecycle` `stopping` event naming the signal and then interrupts itself, which
 *   closes the scope. The listeners are installed before `PlatformLive` builds, and a signal while it
 *   builds or while the startup ping waits interrupts startup at once (releasing whatever was
 *   acquired, log included). They are removed last, so a second signal during shutdown cannot kill
 *   the process half-way.
 * - **Partial startup releases everything.** Every step from the first SQLite handle onward is
 *   scoped, so a failure at any later step — a second app's file, the reader's ping, the HTTP bind
 *   (`ListenError`, e.g. a port in use) — closes what was acquired before it, in reverse, and the
 *   process exits 1 with no child and no handle left behind.
 * - **The reader child is supervised, not load-bearing.** If it exits unexpectedly while the
 *   server is serving, a supervisor respawns it with backoff (`reader-restarting` /
 *   `reader-restarted` lifecycle events); queries answer 502 only during the restart window and
 *   resume on the new child. Only if the supervisor gives up (more than 5 restarts within 10
 *   minutes) does the server log `stopping` (reason `reader-exited`), shut down in the order
 *   above, and exit 1, so a supervisor restarts both processes together.
 * - **The log flushes last.** The `lifecycle` `shutdown` event is emitted by a finalizer that sits
 *   directly above the logging layer, so it is the final line of the server's log file.
 *
 * ## Event-log ingest and retention
 *
 * This process never opens the event-log DuckDB file (ADR 0004). Every `INGEST_SWEEP_INTERVAL` it
 * sends the reader the existing `ingest` RPC. Until item 33 lands the reader answers it with an
 * `IngestError` ("not available"); item 33 connects the RPC to `EventLogIngestLive`'s
 * ingest-then-sweep coordinator (ADR 0007): ingest, then — only after a committed ingest — the
 * retention sweep. `POST /admin/logs/ingest` sends the same RPC on demand. The first run is one
 * interval after startup, so a restart never queues a long ingest ahead of the first queries.
 *
 * The ingest RPC shares the reader connection's per-call deadline (`READER_CALL_TIMEOUT_MS`, sized for
 * queries). A run that takes longer is logged here as `ingest-sweep-failed` (`IpcError` timeout)
 * while the reader carries on and commits; its late reply is then a `reader-reply-anomaly`. A
 * per-call deadline needs a `ReaderClient` change (`platform/ipc.ts`, not this module's).
 *
 * Only an abnormal end reaches stderr — a failure to *start* (bad configuration, schema drift, a
 * reader that will not answer, a port in use), the reader exiting while serving, or the listener
 * closing on its own — because the event log may not exist yet or may be the thing that broke; the
 * process then exits 1. A signal is a normal stop: exit 0.
 */
import {
  Cause,
  Data,
  Deferred,
  Duration,
  Effect,
  Exit,
  Layer,
  Option,
  Runtime,
  Schedule,
  type Scope,
} from "effect";
import { type AnyError, ReaderExitedError } from "../core/errors.ts";
import { fromPlatformError } from "../core/events.ts";
import { devTokensInUse } from "./http/auth.ts";
import { handle, type RouterRequirements } from "./http/router.ts";
import { type AfterStep, emitServerLifecycle, makePlatformLive, ReaderProcess } from "./layers.ts";
import { AppServices } from "./registry.ts";
import { Config, EventSink, ReaderClient } from "./services.ts";

/**
 * The HTTP listener could not bind (`Deno.serve` threw: the port is taken, the address is not
 * local, …), or closed on its own while serving. Like `SinkIoError`, deliberately not in
 * `core/errors.ts`: it only ever ends the process, so it never needs an HTTP status.
 */
export class ListenError extends Data.TaggedError("ListenError")<{
  readonly hostname: string;
  readonly port: number;
  readonly detail: string;
  readonly cause?: unknown;
}> {}

/** How often the server asks the reader to ingest the event log and then sweep it. */
export const INGEST_SWEEP_INTERVAL: Duration.Duration = Duration.hours(1);

const SIGNALS = ["SIGINT", "SIGTERM"] as const;

/**
 * Installs the signal listeners for the life of the scope; the `Deferred` completes with the first
 * signal's name. This is the production stop source; `ServerOptions.stop` replaces it in tests.
 */
export const signalHandlers: Effect.Effect<Deferred.Deferred<string>, never, Scope.Scope> = Effect
  .acquireRelease(
    Effect.gen(function* () {
      const received = yield* Deferred.make<string>();
      const listeners = SIGNALS.map((signal) =>
        [signal, () => void Deferred.unsafeDone(received, Effect.succeed(signal))] as const
      );
      for (const [signal, listener] of listeners) Deno.addSignalListener(signal, listener);
      return { received, listeners };
    }),
    ({ listeners }) =>
      Effect.sync(() => {
        for (const [signal, listener] of listeners) Deno.removeSignalListener(signal, listener);
      }),
  ).pipe(Effect.map(({ received }) => received));

/**
 * Forks `loop` into the scope, and emits `<name>-stopped` once the scope has interrupted it, so the
 * shutdown order of the timers is visible in the log. The event is a scope finalizer registered
 * before the fork, so it runs after the fork's own finalizer (interrupt and await the fiber) — even
 * when the fiber is interrupted before it ever ran.
 */
const forkTimer = <R>(name: string, loop: Effect.Effect<unknown, never, R>) =>
  Effect.gen(function* () {
    const sink = yield* EventSink;
    yield* Effect.addFinalizer(() => emitServerLifecycle(sink, `${name}-stopped`));
    yield* Effect.forkScoped(loop);
  });

/** The numeric counters of an ingest reply, and its retention status: small enough for one event. */
function ingestSummary(reply: unknown): Record<string, unknown> {
  const summary: Record<string, unknown> = {};
  if (reply === null || typeof reply !== "object") return summary;
  for (const [key, value] of Object.entries(reply)) {
    if (typeof value === "number") summary[key] = value;
  }
  const retention = (reply as { retention?: { status?: unknown } }).retention;
  if (typeof retention?.status === "string") summary.retention = retention.status;
  return summary;
}

/**
 * The ingest-then-sweep timer: every `every`, the reader's `ingest` RPC (see the module comment). A
 * run is a plain sequential effect, so two can never overlap from this fiber; once item 33 connects
 * the RPC to `EventLogIngestLive`, its ingest lock serializes it with `POST /admin/logs/ingest`.
 * Until then every run fails with the reader's `IngestError`. A failed run is a `warn` event and
 * the next run still happens.
 */
const ingestSweepTimer = (every: Duration.DurationInput) =>
  Effect.gen(function* () {
    const sink = yield* EventSink;
    const reader = yield* ReaderClient;
    const once = reader.call({ kind: "ingest" }).pipe(
      Effect.matchEffect({
        onSuccess: (reply) => emitServerLifecycle(sink, "ingest-sweep", ingestSummary(reply)),
        onFailure: (error) =>
          emitServerLifecycle(sink, "ingest-sweep-failed", fromPlatformError(error), "warn"),
      }),
      Effect.catchAllCause((cause) => Effect.logError("ingest-sweep defect", cause)),
    );
    yield* forkTimer("ingest-sweep", Effect.schedule(once, Schedule.spaced(every)));
  });

/**
 * The backup timer: when `BACKUP_INTERVAL_MS` is non-zero, one scoped fiber sweeps every app's
 * backup in turn, then waits the interval before the next sweep (`Schedule.spaced`, first sweep one
 * interval after start, as the pre-port `setInterval`). A sweep is a plain sequential effect, so two
 * can never overlap — no guard flag needed. Each `take` records its own `backup` events (with the
 * failing phase on a failure), so a failed app is logged and the sweep moves on.
 */
const backupTimer = Effect.gen(function* () {
  const { intervalMs } = (yield* Config).backup;
  if (intervalMs === 0) return;
  const stacks = (yield* AppServices).all;
  const sweep = Effect.forEach(
    stacks,
    (stack) =>
      stack.backups.take(stack.app.id).pipe(
        Effect.asVoid,
        Effect.catchAll(() => Effect.void),
        Effect.catchAllCause((cause) => Effect.logError("backup sweep defect", cause)),
      ),
    { discard: true },
  );
  yield* forkTimer("backup-timer", Effect.schedule(sweep, Schedule.spaced(Duration.millis(intervalMs))));
});

/**
 * `Deno.serve` under `Effect.acquireRelease`. Each request runs `handle` on this fiber's runtime, so
 * it sees every service in `PlatformLive`. A bind failure is a typed `ListenError` (the listener was
 * never acquired, so only what came before it is released). Release is `server.shutdown()`: stop
 * accepting, then wait for every in-flight request to finish.
 */
const listen = Effect.gen(function* () {
  const { server: settings } = yield* Config;
  const sink = yield* EventSink;
  const run = Runtime.runPromise(yield* Effect.runtime<RouterRequirements>());
  const internalError = () =>
    Response.json({ error: { tag: "Defect", message: "internal error" } }, { status: 500 });
  return yield* Effect.acquireRelease(
    Effect.try({
      try: () =>
        Deno.serve({
          hostname: settings.hostname,
          port: settings.port,
          // Both defaults write to the console; startup is reported as a `lifecycle` event instead.
          onListen: () => {},
          onError: internalError,
        }, (request) => run(handle(request)).catch(internalError)),
      catch: (cause) =>
        new ListenError({
          hostname: settings.hostname,
          port: settings.port,
          detail: cause instanceof Error ? `${cause.name}: ${cause.message}` : String(cause),
          cause,
        }),
    }),
    (server) =>
      Effect.zipRight(
        Effect.promise(() => server.shutdown()),
        emitServerLifecycle(sink, "http-drained"),
      ),
  );
});

export interface ServerOptions {
  /** What stops the server. Default: `signalHandlers` (SIGINT/SIGTERM). */
  readonly stop?: Effect.Effect<Deferred.Deferred<string>, never, Scope.Scope>;
  /** Runs after each startup step acquires; see `AfterStep` in `platform/layers.ts`. */
  readonly afterStep?: AfterStep;
  /** Default `INGEST_SWEEP_INTERVAL`. */
  readonly ingestSweepEvery?: Duration.DurationInput;
}

const program = (stop: Deferred.Deferred<string>, options: ServerOptions) =>
  Effect.gen(function* () {
    const sink = yield* EventSink;
    const config = yield* Config;
    const reader = yield* ReaderProcess;
    const afterStep = options.afterStep ?? (() => Effect.void);

    const devTokens = devTokensInUse(config);
    if (devTokens.length > 0) {
      yield* emitServerLifecycle(sink, "config", {
        message: "default dev-only token in use",
        tokens: devTokens,
        note: "Local/trusted-app MVP: set these before exposing this server.",
      }, "warn");
    }

    // The reader must answer before the server takes traffic, as before the port. A stop while it
    // has not yet answered ends startup instead of waiting out the call timeout; a child that dies
    // first fails the ping with `ReaderExitedError`.
    yield* Effect.raceFirst(
      (yield* ReaderClient).call({ kind: "ping" }),
      Effect.flatMap(Deferred.await(stop), (reason) =>
        Effect.zipRight(emitServerLifecycle(sink, "stopping", { reason }), Effect.interrupt)),
    );
    yield* afterStep("reader-ready");
    yield* ingestSweepTimer(options.ingestSweepEvery ?? INGEST_SWEEP_INTERVAL);
    yield* afterStep("ingest-sweep");
    yield* backupTimer;
    yield* afterStep("backup-timer");
    const server = yield* listen;
    yield* afterStep("http");
    yield* emitServerLifecycle(sink, "ready", {
      hostname: server.addr.hostname,
      port: server.addr.port,
      apps: (yield* AppServices).all.map((stack) =>
        stack.app.id
      ),
      readerPid: reader.pid,
      backupIntervalMs: config.backup.intervalMs,
      ingestSweepEveryMs: Duration.toMillis(options.ingestSweepEvery ?? INGEST_SWEEP_INTERVAL),
    });

    type Ended =
      | { readonly reason: "reader-exited"; readonly status: Deno.CommandStatus }
      | { readonly reason: string; readonly status?: undefined };
    const stopped: Effect.Effect<Ended> = Effect.map(Deferred.await(stop), (reason) => ({ reason }));
    const closed: Effect.Effect<Ended> = Effect.as(Effect.promise(() => server.finished), {
      reason: "listener-closed",
    });
    const exited: Effect.Effect<Ended> = Effect.map(
      reader.exited,
      (status) => ({ reason: "reader-exited", status }),
    );
    const ended = yield* Effect.raceAll([stopped, closed, exited]);
    if (ended.status !== undefined) {
      const error = new ReaderExitedError(
        ended.status.signal
          ? { code: ended.status.code, signal: ended.status.signal }
          : { code: ended.status.code },
      );
      yield* emitServerLifecycle(
        sink,
        "stopping",
        { reason: ended.reason, code: error.code, signal: error.signal },
        "error",
      );
      return yield* error;
    }
    if (ended.reason === "listener-closed") {
      // Only `server.shutdown()` should close it, and that runs after this program ends.
      yield* emitServerLifecycle(sink, "stopping", { reason: ended.reason }, "error");
      return yield* new ListenError({
        hostname: server.addr.hostname,
        port: server.addr.port,
        detail: "the listener closed while the server was serving",
      });
    }
    yield* emitServerLifecycle(sink, "stopping", { reason: ended.reason });
    return yield* Effect.interrupt;
  });

/**
 * The whole server: the stop source first, then `PlatformLive` and the program, all in one scope.
 * A stop during the layer build interrupts it; the build's finalizers are already in the scope, so
 * what was acquired is released in order and the log drains. Succeeds never: it ends interrupted
 * (a stop) or failed (startup, or the reader exiting).
 */
export function makeServer(options: ServerOptions = {}) {
  return Effect.scoped(Effect.gen(function* () {
    const stop = yield* (options.stop ?? signalHandlers);
    const context = yield* Effect.raceFirst(
      Layer.build(makePlatformLive({ afterStep: options.afterStep })),
      Effect.zipRight(Deferred.await(stop), Effect.interrupt),
    );
    return yield* Effect.provide(program(stop, options), context);
  }));
}

/** A startup failure as one line: the tag and its payload, or the rendered cause. */
export function describeFailure(cause: Cause.Cause<unknown>): string {
  const failure = Option.getOrUndefined(Cause.failureOption(cause));
  if (failure !== undefined && failure !== null && typeof failure === "object" && "_tag" in failure) {
    const { errorTag, errorDetail } = fromPlatformError(failure as AnyError);
    try {
      return `${errorTag} ${JSON.stringify(errorDetail)}`;
    } catch {
      return errorTag;
    }
  }
  return Cause.pretty(cause);
}

if (import.meta.main) {
  const exit = await Effect.runPromiseExit(makeServer());
  if (Exit.isFailure(exit) && !Cause.isInterruptedOnly(exit.cause)) {
    Deno.stderr.writeSync(new TextEncoder().encode(`mansio server failed: ${describeFailure(exit.cause)}\n`));
    Deno.exit(1);
  }
  Deno.exit(0);
}
