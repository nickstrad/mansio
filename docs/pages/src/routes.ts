import { formatRoute, parseRoute } from "./sdk.ts";

/**
 * The site's URL grammar is the SDK's own deep link, unchanged:
 * `#/architectures/<id>/diagrams/<id>[/elements/<id>][?step=<id>]`. An empty hash opens the catalog's
 * default architecture. Anything else is Not Found, never a fallback.
 */
export type SiteRoute =
  | { readonly kind: "home" }
  | {
    readonly kind: "single";
    readonly architectureId: string;
    readonly diagramId: string;
  }
  | { readonly kind: "not-found"; readonly message: string };

export function parseSiteRoute(hash: string): SiteRoute {
  if (hash === "" || hash === "#") return { kind: "home" };
  const parsed = parseRoute(hash);
  if (!parsed.ok) {
    return {
      kind: "not-found",
      message: parsed.diagnostics.map((d) => d.message).join(" "),
    };
  }
  if (!parsed.value) return { kind: "home" };
  return {
    kind: "single",
    architectureId: parsed.value.architectureId,
    diagramId: parsed.value.diagramId,
  };
}

export function singleHash(architectureId: string, diagramId: string): string {
  return formatRoute({ architectureId, diagramId });
}

/** The architecture the site opens when the URL names none, or why the catalog does not name one. */
export type DefaultChoice =
  | { readonly ok: true; readonly id: string }
  | { readonly ok: false; readonly message: string };

/**
 * Catalog entries are peers; `defaultId` (optional, from catalog.json) only picks the one shown first.
 * Without it the first entry is the default. A `defaultId` that names no entry is an authoring error and
 * is reported, never silently replaced.
 */
export function chooseDefault(
  entryIds: readonly string[],
  defaultId: unknown,
): DefaultChoice {
  if (defaultId === undefined) {
    return entryIds.length > 0
      ? { ok: true, id: entryIds[0] }
      : { ok: false, message: "The catalog lists no architectures." };
  }
  if (typeof defaultId === "string" && entryIds.includes(defaultId)) {
    return { ok: true, id: defaultId };
  }
  return {
    ok: false,
    message: `The catalog's defaultId ${JSON.stringify(defaultId)} names no catalog entry.`,
  };
}

/** The architecture a route shows: the routed one, the default for an empty hash, none for Not Found. */
export function shownArchitectureId(
  route: SiteRoute,
  defaultId: string,
): string | undefined {
  if (route.kind === "home") return defaultId;
  return route.kind === "single" ? route.architectureId : undefined;
}
