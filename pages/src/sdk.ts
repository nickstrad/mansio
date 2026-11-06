/**
 * The ONLY module of the site that touches c4-sdk. Everything else imports SDK components, ports and
 * types from here, so a change to the c4-sdk dependency (the published npm package) touches this file alone.
 */
import "c4-sdk/styles.css";
import layoutWorkerUrl from "c4-sdk/layout-worker.js?url";
import {
  ArchitectureProvider,
  ArchitectureViewer,
  createElkLayoutPort,
  createFetchLoader,
  createHashRoutePort,
  DiagramDocumentation,
  DiagramNavigation,
  DiagramView,
} from "c4-sdk";
import { formatRoute, parseRoute } from "c4-sdk/core";
import type {
  CatalogEntry,
  Diagnostic,
  Diagram,
  Result,
  Route,
  RoutePort,
  ValidatedArchitecture,
} from "c4-sdk/core";

export {
  ArchitectureProvider,
  ArchitectureViewer,
  DiagramDocumentation,
  DiagramNavigation,
  DiagramView,
  formatRoute,
  parseRoute,
};
export type { CatalogEntry, Diagnostic, Diagram, Route, RoutePort, ValidatedArchitecture };

/**
 * One layout port for every viewer on the page. The package ships its ELK worker as a separate file; Vite
 * fingerprints it through `?url` and resolves it under the `/mansio/` base.
 */
export const layoutPort = createElkLayoutPort(() => new Worker(layoutWorkerUrl, { type: "module" }));

let browserPort: RoutePort | undefined;
/** The URL-hash route port, created on first use so importing this module touches no browser state. */
export function browserRoutePort(): RoutePort {
  browserPort ??= createHashRoutePort();
  return browserPort;
}

/** A catalog entry with its decoded bundle, or the reason it could not be shown. */
export interface LoadedArchitecture {
  readonly entry: CatalogEntry;
  readonly result: Result<ValidatedArchitecture>;
}

/**
 * Loads the catalog and every bundle it lists through the SDK's bounded fetch loader. Like the SDK's
 * CatalogViewer, a bundle whose `id` differs from its catalog entry `id` is rejected, so a deep link's
 * architecture id always names the bundle it shows. `fetchResource` is the loader's fetch (the site passes one
 * that lifts its own `defaultId` key off the catalog, see catalog.ts).
 */
export async function loadArchitectures(
  catalogUrl: string,
  signal: AbortSignal,
  fetchResource: typeof fetch = fetch,
): Promise<readonly LoadedArchitecture[]> {
  const loader = createFetchLoader(fetchResource);
  const catalog = await loader.loadCatalog(catalogUrl, signal);
  return await Promise.all(
    catalog.entries.map(async (entry): Promise<LoadedArchitecture> => {
      const result = await loader.loadBundle(entry.url, signal);
      if (result.ok && result.value.bundle.id !== entry.id) {
        return {
          entry,
          result: {
            ok: false,
            diagnostics: [{
              code: "catalog-identity",
              severity: "error",
              path: "$.id",
              message: `Catalog entry "${entry.id}" loaded a bundle whose id is "${result.value.bundle.id}".`,
            }],
          },
        };
      }
      return { entry, result };
    }),
  );
}
