/**
 * The deployment registry (plan item 22, work 1): which apps this process serves, where their files
 * live, and the per-app services built over them.
 *
 * - `AppRegistryLive` is the `AppRegistry` service over `Config`. The registry is a deployment
 *   decision, never a path supplied by an HTTP caller; `Config.apps` lists the apps and their
 *   tokens, and each app's SQLite file sits under `Config.server.dataDir`.
 * - `AppServices` is the per-app half: one `Storage` (and the `Backups` bound to it) per app, because
 *   the writer owns one SQLite connection per file (`platform/services.ts`, `Storage`). The live
 *   layer is built in `platform/layers.ts`; the HTTP router only sees this tag, so a test supplies
 *   in-memory stacks and a later tenancy layer (multitenancy plan MT-03/MT-07) can resolve stacks by
 *   tenant behind the same `get`.
 *
 * The pre-port `trace()` helper is gone: SQL tracing is the `sql` event kind at the `TRACE_SQL`
 * level (`Config.logging.traceSql`), written by the event log rather than to stderr.
 */
import type { TenantStorageRouterService } from "./tenants/routing.ts";
import { resolve } from "node:path";
import { Context, Effect, Layer } from "effect";
import { UnknownAppError } from "../core/errors.ts";
import { withStorage } from "../core/schema.ts";
import { functions } from "../app/functions.ts";
import { schema } from "../app/schema.ts";
import { ConfigLive } from "./config.ts";
import {
  type App,
  AppRegistry,
  type AppRegistryService,
  type BackupsService,
  Config,
  type ConfigService,
  type StorageService,
} from "./services.ts";

/**
 * The registered apps, in `Config.apps` order. Both apps run the same tic-tac-toe code; they differ
 * only in physical layout (`columns` or `json`), file, and token.
 */
export function appsFromConfig(config: ConfigService): readonly App[] {
  return config.apps.map((app) => ({
    id: app.id,
    file: resolve(config.server.dataDir, `${app.id}.sqlite`),
    schema: withStorage(schema, app.storage),
    functions,
    token: app.token,
  }));
}

/** An `AppRegistryService` over a fixed list. Ids are matched exactly; the first entry for an id wins. */
export function makeAppRegistry(apps: readonly App[]): AppRegistryService {
  const byId = new Map<string, App>();
  for (const app of apps) if (!byId.has(app.id)) byId.set(app.id, app);
  const all = Effect.succeed([...byId.values()] as readonly App[]);
  return {
    get: (id) => {
      const app = byId.get(id);
      return app ? Effect.succeed(app) : Effect.fail(new UnknownAppError({ appId: id }));
    },
    all,
  };
}

/** Live: the apps `Config` declares. Reads no environment of its own. */
export const AppRegistryLive: Layer.Layer<AppRegistry, never, Config> = Layer.effect(
  AppRegistry,
  Effect.map(Config, (config) => makeAppRegistry(appsFromConfig(config))),
);

/** A fixed registry, for tests and for compositions that do not read `Config`. */
export function AppRegistryTest(apps: readonly App[]): Layer.Layer<AppRegistry> {
  return Layer.succeed(AppRegistry, makeAppRegistry(apps));
}

// ===========================================================================
// AppServices — the per-app service stacks
// ===========================================================================

/** One app's write-side services. `backups.take` snapshots exactly this `storage`. */
export interface AppStack {
  readonly app: App;
  readonly tenantRouter?: TenantStorageRouterService;
  readonly storage: StorageService;
  readonly backups: BackupsService;
}

export interface AppServicesService {
  /** The stack for `appId`, or `UnknownAppError` if this process serves no such app. */
  get(appId: string): Effect.Effect<AppStack, UnknownAppError>;
  /** Every stack, in registry order. The backup timer sweeps these one at a time. */
  readonly all: readonly AppStack[];
}

/**
 * Live: built per app by `makeAppServicesLive` in `platform/layers.ts` (one scoped `StorageLive` +
 * `BackupsLive` per app). Test: `makeAppServices` over `StorageTest`-backed stacks.
 */
export class AppServices extends Context.Tag("mansio/AppServices")<AppServices, AppServicesService>() {}

/** An `AppServicesService` over already-built stacks. */
export function makeAppServices(stacks: readonly AppStack[]): AppServicesService {
  const byId = new Map(stacks.map((stack) => [stack.app.id, stack] as const));
  return {
    get: (appId) => {
      const stack = byId.get(appId);
      return stack ? Effect.succeed(stack) : Effect.fail(new UnknownAppError({ appId }));
    },
    all: stacks,
  };
}

// ===========================================================================
// COMPATIBILITY WRAPPER — for `platform/reader.ts` until item 33 composes it over `AppRegistry`
// ===========================================================================

/**
 * The pre-port `Map` of apps, from `ConfigLive` rather than from the environment directly. Throws the
 * `ConfigError` if the environment does not decode; the reader process has already built
 * `ConfigLive` successfully by the time it calls this.
 *
 * The server never calls this: its registry is `AppRegistryLive` over the one `Config` its layer
 * chain builds. Only the reader process does, which builds its own `ConfigLive` from the same
 * inherited environment.
 *
 * @deprecated Use the `AppRegistry` tag (`AppRegistryLive`). Kept only because `platform/reader.ts`
 * is outside items 22 and 32's file sets; item 33 owns the reader's composition.
 */
export function loadApps(): Map<string, App> {
  const config = Effect.runSync(Effect.provide(Config, ConfigLive));
  return new Map(appsFromConfig(config).map((app) => [app.id, app] as const));
}
