/**
 * The composition roots (plan items 22 and 32): every live layer, wired once.
 *
 * ## `PlatformLive` — the server (writer) process
 *
 * The plan's sketch is `Layer.mergeAll(ConfigLive, LoggerLive, EventSinkLive, AppRegistryLive,
 * IdGenLive, StorageLive, ReaderClientLive, ObjectStoreLive, BackupsLive)`. Two things change in the
 * real wiring, both on purpose:
 *
 * 1. **`Storage` and `Backups` are per app.** The writer owns one SQLite connection per file, so
 *    there is one `StorageLive` (and one `BackupsLive` bound to it) per registered app.
 *    `AppServicesLive` builds those stacks into its own scope and exposes them behind the
 *    `AppServices` tag (`platform/registry.ts`); the router never sees a bare `Storage`.
 * 2. **The layers are a chain, not a flat merge.** `Layer.mergeAll` may build (and therefore
 *    release) its members concurrently, and shutdown order is load-bearing here. Each
 *    `Layer.provideMerge` below builds its argument first and releases it last, so acquisition and
 *    release are both total orders:
 *
 * ```text
 * acquire:  Config → Logging (sink + Logger) → shutdown marker → IdGen → ObjectStore, AppRegistry
 *           → AppServices (Storage + Backups, per app, in registry order) → ReaderClient (child)
 * release:  ReaderClient → AppServices (apps in reverse) → … → shutdown marker → Logging → Config
 * ```
 *
 * `platform/server.ts` acquires the ingest/sweep and backup fibers and then the HTTP listener inside
 * this context, so the full shutdown sequence is HTTP drain → backup and ingest/sweep fibers → reader
 * child → DB handles → final `lifecycle` event → log flush. The "shutdown marker" is a finalizer-only
 * layer that emits the `lifecycle` `shutdown` event after everything that could still log has closed
 * and before the sink drains, so it is the last line of the server's log file.
 *
 * ## Partial startup (item 32)
 *
 * Every resource is acquired by a scoped layer, from the first SQLite handle onward, so a failure at
 * any later step — a second app's file that will not open, a reader that will not answer, a port that
 * is taken — closes everything acquired before it, in reverse, and still flushes the log. Nothing is
 * opened outside a scope, so there is no window in which a handle exists that no finalizer owns.
 *
 * The resource steps leave evidence in the log: `sqlite-library` once, before the first open (the
 * `libsqlite3` every connection in this process uses: requested and resolved path, version),
 * `storage-opened`/`storage-closed` per app (emitted
 * here; `storage-closed` only after that app's handle and writer queue have closed, and only if it
 * opened), `reader-spawned`/`reader-exited` (`platform/ipc.ts`). `makePlatformLive`'s `afterStep`
 * hook runs after each step acquires; tests use it to inject a failure or an interruption at exactly
 * that point (`tests/unit/server_scope_test.ts`). It is a no-op in production.
 *
 * Memoization: within one build, Effect memoizes layers by reference, so `ConfigLive` and the
 * logging layer — required by nearly every other layer — are each built exactly once and shared.
 * The per-app stacks are built with their own `Layer.build`, but every service they need besides
 * `Storage` (`Config`, `EventSink`, `IdGen`, `ObjectStore`) comes from this chain's context, not
 * from a layer, so nothing below them is built twice: one sink, one object store, one child.
 * `Layer.memoize` (which shares a layer across *separate* builds) has no second build to serve in
 * this process, so it is not used.
 *
 * ## The reader process root
 *
 * The two processes must not share a module graph: this one loads the system `libsqlite3` through
 * the FFI binding (`platform/sqlite_ffi/`, for `StorageLive`, the document reader and backups), the
 * reader loads `@duckdb/node-api` with its own SQLite inside, and one process must never open a file
 * through two SQLite copies (ADR 0001, `platform/duckdb.ts`). So the reader's root cannot live here
 * as a value — importing `EventLogIngestLive` would load DuckDB into the writer, and
 * `platform/reader.ts` importing this module would load the binding into the reader. What is fixed
 * here is its *shape*:
 * `ReaderProcessServices` = `Config` + the reader's `EventSink` (`LoggingReaderLive`) +
 * `EventLogIngest`, with per-app `ReaderLive` instances opened lazily by `makeReaderPool`
 * (`platform/duckdb.ts`) inside the reader's program scope. `platform/reader.ts` composes it
 * (item 33). The server reaches the event log only through the reader's `ingest` RPC, which item 36's
 * ingest-then-sweep coordinator answers there (ADR 0007); this process never opens the event-log
 * DuckDB file.
 *
 * The event-log modules are imported below **as types only**: `deno task check` type-checks
 * `platform/eventlog/*` through this file, and nothing of theirs is loaded at runtime.
 *
 * ## Seams for the multitenancy plan
 *
 * MT-05/MT-07 replace `AppRegistryLive`/`AppServicesLive` (resolve stacks per tenant behind the same
 * `AppServices.get`) and the auth functions in `platform/http/auth.ts`; nothing else in this chain
 * names an app.
 */
import { makeCachedTenantRegistry } from "./http/auth.ts";
import { TenantRegistryLazy } from "./tenants/store.ts";
import { LocalFolderRouter, TenantStorageRouter } from "./tenants/routing.ts";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { Cause, Context, Deferred, Duration, Effect, Exit, Layer, Ref, Scope } from "effect";
import { StorageError } from "../core/errors.ts";
import { lifecycleEvent } from "../core/events.ts";
import { makeBackupsLive } from "./backup.ts";
import { DocumentReaderClientLive } from "./document_reader.ts";
import { ConfigLive } from "./config.ts";
// Type-only, on purpose: see "The reader process root" above.
import type { EventLogIngestLive } from "./eventlog/ingest.ts";
import type { StagedLine } from "./eventlog/tables.ts";
import { makeReaderConnection, type ReaderConnection } from "./ipc.ts";
import { LoggingLive } from "./logging/logger.ts";
import { ObjectStoreLive } from "./object_store.ts";
import { AppRegistryLive, AppServices, type AppStack, makeAppServices } from "./registry.ts";
import { IdGenLive, sqliteFailureMessage, StorageLive } from "./sqlite.ts";
import { isSqliteFfiError, loadLibrary } from "./sqlite_ffi/mod.ts";
import {
  AppRegistry,
  Backups,
  Config,
  EventSink,
  type EventSinkService,
  ReaderClient,
  type ReaderClientService,
  Storage,
  TenantRegistry,
} from "./services.ts";

/** The reader entry module the server spawns as its child process. */
export const READER_MODULE = fileURLToPath(new URL("./reader.ts", import.meta.url));

/** A `lifecycle` event from the server process, outside any request. */
export function emitServerLifecycle(
  sink: EventSinkService,
  phase: string,
  detail?: unknown,
  level: "info" | "warn" | "error" = "info",
): Effect.Effect<void> {
  return Effect.flatMap(
    Effect.clockWith((clock) => clock.currentTimeMillis),
    (ms) =>
      sink.emit(lifecycleEvent({
        eventId: crypto.randomUUID(),
        ts: new Date(ms).toISOString(),
        level,
        role: "server",
        pid: Deno.pid,
        traceId: "server-lifecycle",
        phase,
        ...(detail === undefined ? {} : { detail }),
      })),
  );
}

// ===========================================================================
// Startup steps
// ===========================================================================

/**
 * The points in server startup at which a resource has just been acquired: the layer steps here,
 * then `platform/server.ts`'s program steps. `storage:<appId>` is one per app, in registry order.
 */
export type StartupStep =
  | "logging"
  | "object-store"
  | `storage:${string}`
  | "reader"
  | "reader-ready"
  | "ingest-sweep"
  | "backup-timer"
  | "http";

/**
 * Runs right after `step` acquires. A defect or an interruption here fails startup at exactly that
 * point, so a test can prove everything acquired before it is released. Production passes none.
 */
export type AfterStep = (step: StartupStep) => Effect.Effect<void>;

const noStep: AfterStep = () => Effect.void;

/** A layer that only runs the hook for `step`, placed in the chain right after that step's layer. */
const stepDone = (afterStep: AfterStep, step: StartupStep): Layer.Layer<never> =>
  Layer.effectDiscard(afterStep(step));

// ===========================================================================
// Per-app stacks
// ===========================================================================

/**
 * One `StorageLive` + `BackupsLive` per registered app, built into this layer's scope in registry
 * order (and so closed in reverse). Each app's data directory is created first, as before the port.
 *
 * Each app gets a `storage-opened` event once its handle is open and a `storage-closed` event once
 * it has closed again (the writer queue drained, then the connection closed). The closing event's
 * finalizer is registered *before* the stack is built, so it runs *after* the stack's own release.
 *
 * Before the first app, the `libsqlite3` named by `Config.sqlite.library` (or the binding's default
 * list) is loaded once and reported as one `sqlite-library` event. Every open in this process —
 * `Storage`, the tenant router, the document reader, the backup verifier — passes the same
 * `Config.sqlite.library`, and the binding caches the loaded library per path, so they all use the
 * copy this event names. A library that cannot load (or is too old, or single-threaded) fails
 * startup here as `StorageError{operation:"open"}`, before any file is opened.
 */
export function makeAppServicesLive(afterStep: AfterStep = noStep) {
  return Layer.scoped(
    AppServices,
    Effect.gen(function* () {
      const sink = yield* EventSink;
      const apps = yield* (yield* AppRegistry).all;
      const stacks: AppStack[] = [];
      const config = yield* Config;
      const library = yield* Effect.try({
        try: () => loadLibrary(config.sqlite.library),
        catch: (cause) =>
          new StorageError({
            operation: "open",
            detail: isSqliteFfiError(cause) ? sqliteFailureMessage(cause) : String(cause),
            cause,
          }),
      });
      yield* emitServerLifecycle(sink, "sqlite-library", {
        requested: config.sqlite.library ?? null,
        path: library.path,
        version: library.version,
        versionNumber: library.versionNumber,
      });
      for (const app of apps) {
        yield* Effect.tryPromise({
          try: () => Deno.mkdir(dirname(app.file), { recursive: true }),
          catch: (cause) =>
            new StorageError({ operation: "open", detail: `cannot create ${dirname(app.file)}`, cause }),
        });
        let opened = false;
        yield* Effect.addFinalizer(() =>
          opened ? emitServerLifecycle(sink, "storage-closed", { appId: app.id }) : Effect.void
        );
        const rawRouter = config.tenancy.mode === "off"
          ? undefined
          : Context.get(yield* Layer.build(LocalFolderRouter(app.schema)), TenantStorageRouter);
        // Reader preparation and Storage's first open share one initialization barrier.
        // Repeating prepare on every read would take BEGIN IMMEDIATE on a second SQLite
        // connection while an asynchronous mutation may already hold the writer lock.
        const prepared = new WeakSet<object>();
        const prepareLock = yield* Effect.makeSemaphore(1);
        const tenantRouter = rawRouter && {
          resolve: rawRouter.resolve,
          prepare: (location: Parameters<typeof rawRouter.prepare>[0]) =>
            prepareLock.withPermits(1)(
              Effect.suspend(() =>
                prepared.has(location) ? Effect.void : rawRouter.prepare(location).pipe(
                  Effect.tap(() =>
                    Effect.sync(() => {
                      prepared.add(location);
                    })
                  ),
                )
              ),
            ),
        };
        const storage = StorageLive({ file: app.file, schema: app.schema, appId: app.id });
        const stack = makeBackupsLive({ sqliteLibrary: config.sqlite.library }).pipe(
          Layer.provideMerge(
            tenantRouter
              ? storage.pipe(
                Layer.provide(Layer.succeed(TenantStorageRouter, tenantRouter)),
              )
              : storage,
          ),
        );
        const context = yield* Layer.build(stack);
        opened = true;
        stacks.push({
          app,
          tenantRouter,
          storage: Context.get(context, Storage),
          backups: Context.get(context, Backups),
        });
        yield* emitServerLifecycle(sink, "storage-opened", { appId: app.id, file: app.file });
        yield* afterStep(`storage:${app.id}`);
      }
      return makeAppServices(stacks);
    }),
  );
}

/** The live per-app stacks, with no startup hook. */
export const AppServicesLive = makeAppServicesLive();

// ===========================================================================
// The reader child
// ===========================================================================

/** The reader child as a process: what the server watches besides the calls it makes. */
export interface ReaderProcessService {
  /** The current reader child's pid; changes across restarts. */
  readonly pid: number;
  /**
   * Completes only when the supervisor gives up restarting the child (circuit breaker tripped).
   * An unexpected child death while serving is *not* fatal: the supervisor respawns the child
   * (`reader-restarting` / `reader-restarted` lifecycle events) and the server keeps serving.
   * The server treats this completing as fatal (`platform/server.ts`), as before.
   */
  readonly exited: Effect.Effect<Deno.CommandStatus>;
}

export class ReaderProcess
  extends Context.Tag("mansio/ReaderProcess")<ReaderProcess, ReaderProcessService>() {}

/**
 * `ReaderClient` over a supervised child running the reader module, with the per-call deadline
 * from `READER_CALL_TIMEOUT_MS`, plus `ReaderProcess` for the current child. The child inherits
 * this process's environment, which `ConfigLive` has already decoded, so both processes read one
 * configuration.
 *
 * Supervision (segfault mitigation): if the child exits unexpectedly while the server is serving,
 * a supervisor fiber respawns it with backoff instead of failing the server. `ReaderClient.call`
 * always goes through the current connection, so calls made during the restart window fail with
 * `ReaderExitedError` (HTTP 502) and resume on the new child once it answers its ready-ping.
 * `ReaderProcess.exited` completes only when the supervisor gives up (more than 5 restarts within
 * 10 minutes), at which point the server shuts down as it did before supervision.
 */
export const ReaderClientConfigured: Layer.Layer<ReaderClient | ReaderProcess, never, Config | EventSink> =
  Layer.scopedContext(
    Effect.gen(function* () {
      const config = yield* Config;
      const sink = yield* EventSink;
      const options = {
        module: READER_MODULE,
        callTimeout: Duration.millis(config.reader.callTimeoutMs),
      };

      // The current connection, swapped for a fresh child on every restart. `call` always goes
      // through the ref, so callers see the new child as soon as it answers its ping. Each
      // connection lives in its own child scope so a restart can release the old one.
      const initialScope = yield* Scope.make();
      const initial = yield* Scope.extend(makeReaderConnection(options), initialScope);
      const current = yield* Ref.make(initial);
      const scopeRef = yield* Ref.make(initialScope);
      const shuttingDown = yield* Ref.make(false);
      const gaveUp = yield* Deferred.make<Deno.CommandStatus>();
      let currentPid = initial.child.pid;

      /**
       * Spawn a child in a fresh scope and ping it. On success the caller owns the scope; on
       * failure (or interruption) the scope is closed and nothing leaks.
       */
      const spawnReady = Effect.acquireUseRelease(
        Scope.make(),
        (scope) =>
          Scope.extend(makeReaderConnection(options), scope).pipe(
            Effect.flatMap((conn) => Effect.as(conn.call({ kind: "ping" }), { conn, scope })),
          ),
        (scope, exit) => (Exit.isSuccess(exit) ? Effect.void : Scope.close(scope, Exit.void)),
      );

      const restartDelaysMs = [1000, 2000, 4000, 8000];
      const maxRestarts = 5;
      const restartWindowMs = 10 * 60 * 1000;

      const supervise: Effect.Effect<void, never, EventSink> = Effect.gen(function* () {
        const restarts: number[] = [];
        while (true) {
          const conn = yield* Ref.get(current);
          const status = yield* Effect.promise(() => conn.child.status);
          if (yield* Ref.get(shuttingDown)) return;

          // The child died unexpectedly. Respawn with backoff, or give up.
          let replacement: { conn: ReaderConnection; scope: Scope.CloseableScope } | undefined;
          while (replacement === undefined) {
            const now = Date.now();
            while (restarts.length > 0 && now - restarts[0] > restartWindowMs) restarts.shift();
            restarts.push(now);
            const attempt = restarts.length;
            if (attempt > maxRestarts) {
              yield* emitServerLifecycle(sink, "reader-restart-abandoned", { attempts: attempt }, "error");
              yield* Deferred.succeed(gaveUp, status);
              return;
            }
            const backoffMs = restartDelaysMs[Math.min(attempt - 1, restartDelaysMs.length - 1)];
            yield* emitServerLifecycle(
              sink,
              "reader-restarting",
              { attempt, backoffMs, code: status.code, signal: status.signal ?? null },
              "warn",
            );
            yield* Effect.sleep(Duration.millis(backoffMs));
            if (yield* Ref.get(shuttingDown)) return;
            const spawned = yield* Effect.either(spawnReady);
            if (spawned._tag === "Right") replacement = spawned.right;
          }

          // Install the new child atomically: uninterruptible, so a shutdown racing the swap
          // either sees the old child (and closes it via the finalizer) or the new one.
          // If we are shutting down after all, close the replacement instead of installing it.
          const install = yield* Effect.uninterruptible(
            Effect.gen(function* () {
              if (yield* Ref.get(shuttingDown)) {
                yield* Scope.close(replacement.scope, Exit.void);
                return false;
              }
              const oldScope = yield* Ref.getAndSet(scopeRef, replacement.scope);
              yield* Ref.set(current, replacement.conn);
              currentPid = replacement.conn.child.pid;
              yield* emitServerLifecycle(sink, "reader-restarted", { pid: currentPid });
              yield* Scope.close(oldScope, Exit.void);
              return true;
            }),
          );
          if (!install) return;
        }
      });

      yield* Effect.forkScoped(supervise);
      // Registered after the fork, so it runs before the fiber is interrupted on scope close
      // (LIFO): stops the supervisor from starting a restart mid-shutdown, then releases the
      // current child scope.
      yield* Effect.addFinalizer(() =>
        Effect.zipRight(
          Ref.set(shuttingDown, true),
          Effect.flatMap(Ref.get(scopeRef), (scope) => Scope.close(scope, Exit.void)),
        )
      );

      const client: ReaderClientService = {
        call: (request) => Effect.flatMap(Ref.get(current), (conn) => conn.call(request)),
      };
      const process: ReaderProcessService = {
        get pid() {
          return currentPid;
        },
        exited: Deferred.await(gaveUp),
      };
      return Context.make(ReaderClient, client).pipe(Context.add(ReaderProcess, process));
    }),
  );

// ===========================================================================
// The chain
// ===========================================================================

/**
 * Finalizer only: emits the `lifecycle` `shutdown` event when the platform's scope closes. Placed
 * directly above the logging layer, so it runs after every other finalizer and before the drain.
 */
export const ShutdownEventLive: Layer.Layer<never, never, EventSink> = Layer.scopedDiscard(
  Effect.flatMap(EventSink, (sink) =>
    Effect.addFinalizer((exit) =>
      emitServerLifecycle(sink, "shutdown", {
        exit: Exit.isSuccess(exit)
          ? "success"
          : Cause.isInterruptedOnly(exit.cause)
          ? "interrupted"
          : "failure",
      })
    )),
);

export interface PlatformOptions {
  /** See `AfterStep`. Default: none. */
  readonly afterStep?: AfterStep;
}

/** Everything the server process serves with, in the order of the module comment. */
export function makePlatformLive(options: PlatformOptions = {}) {
  const afterStep = options.afterStep ?? noStep;
  return stepDone(afterStep, "reader").pipe(
    Layer.provideMerge(DocumentReaderClientLive.pipe(Layer.provideMerge(ReaderClientConfigured))),
    Layer.provideMerge(makeAppServicesLive(afterStep)),
    Layer.provideMerge(
      Layer.effect(
        TenantRegistry,
        Effect.gen(function* () {
          const registry = yield* TenantRegistry;
          const config = yield* Config;
          return yield* makeCachedTenantRegistry(registry, config.tenancy.tenantKeyCacheTtlMs ?? 30_000);
        }),
      ).pipe(Layer.provide(TenantRegistryLazy)),
    ),
    Layer.provideMerge(stepDone(afterStep, "object-store")),
    Layer.provideMerge(Layer.mergeAll(ObjectStoreLive, AppRegistryLive)),
    Layer.provideMerge(IdGenLive),
    Layer.provideMerge(stepDone(afterStep, "logging")),
    Layer.provideMerge(ShutdownEventLive),
    Layer.provideMerge(LoggingLive),
    Layer.provideMerge(ConfigLive),
  );
}

/** The production composition: `makePlatformLive` with no startup hook. */
export const PlatformLive = makePlatformLive();

/** What `PlatformLive` provides. */
export type PlatformServices = Layer.Layer.Success<typeof PlatformLive>;

/** What building `PlatformLive` can fail with (configuration, schema drift, a driver open). */
export type PlatformBuildError = Layer.Layer.Error<typeof PlatformLive>;

/**
 * What the reader process's root provides (`ConfigLive` + `LoggingReaderLive` +
 * `EventLogIngestLive`), besides its lazily opened per-app readers. A type, not a layer: see the
 * module comment.
 */
export type ReaderProcessServices = Config | EventSink | Layer.Layer.Success<typeof EventLogIngestLive>;

/** One staged event-log line, as the reader's ingest sees it. Re-exported for the same coverage. */
export type { StagedLine };
