import { useEffect, useState, useSyncExternalStore } from "react";
import type { ChangeEvent, MouseEvent, ReactNode } from "react";
import {
  ArchitectureProvider,
  browserRoutePort,
  DiagramDocumentation,
  DiagramNavigation,
  DiagramView,
  layoutPort,
  loadArchitectures,
} from "./sdk.ts";
import type { Diagnostic, LoadedArchitecture, ValidatedArchitecture } from "./sdk.ts";
import { liftDefaultId } from "./catalog.ts";
import { chooseDefault, parseSiteRoute, shownArchitectureId, singleHash } from "./routes.ts";
import type { SiteRoute } from "./routes.ts";

const REPO = "https://github.com/nickstrad/mansio";
const CATALOG_URL = `${import.meta.env.BASE_URL}catalog.json`;

// The SDK hash port reports its own pushState writes and browser navigation (popstate, hashchange).
function subscribeHash(listener: () => void): () => void {
  return browserRoutePort().subscribe(listener);
}
function useHash(): string {
  return useSyncExternalStore(subscribeHash, () => location.hash, () => "");
}

/** One catalog entry, in catalog order: its title (the dropdown label) and its validated bundle. */
interface Architecture {
  readonly id: string;
  readonly title: string;
  readonly validated: ValidatedArchitecture;
}

type Loaded =
  | { readonly state: "loading" }
  | { readonly state: "failed"; readonly message: string }
  | {
    readonly state: "ready";
    readonly architectures: readonly Architecture[];
    readonly defaultId: string;
  };

function useArchitectures(): Loaded {
  const [loaded, setLoaded] = useState<Loaded>({ state: "loading" });
  useEffect(() => {
    const abort = new AbortController();
    const catalog = liftDefaultId(CATALOG_URL);
    loadArchitectures(CATALOG_URL, abort.signal, catalog.fetch).then(
      (entries) => {
        if (!abort.signal.aborted) setLoaded(settle(entries, catalog.defaultId()));
      },
      (error: unknown) => {
        if (!abort.signal.aborted) {
          setLoaded({
            state: "failed",
            message: error instanceof Error ? error.message : String(error),
          });
        }
      },
    );
    return () => abort.abort();
  }, []);
  return loaded;
}

function settle(entries: readonly LoadedArchitecture[], defaultId: unknown): Loaded {
  const problems: string[] = [];
  const architectures: Architecture[] = [];
  for (const { entry, result } of entries) {
    if (result.ok) {
      architectures.push({ id: entry.id, title: entry.title, validated: result.value });
    } else problems.push(`${entry.id}: ${describe(result.diagnostics)}`);
  }
  const choice = chooseDefault(entries.map(({ entry }) => entry.id), defaultId);
  if (!choice.ok) problems.push(choice.message);
  return problems.length || !choice.ok
    ? { state: "failed", message: problems.join(" ") }
    : { state: "ready", architectures, defaultId: choice.id };
}

function describe(diagnostics: readonly Diagnostic[]): string {
  return diagnostics.filter((d) => d.severity === "error").map((d) => `${d.code} at ${d.path}: ${d.message}`)
    .join("; ");
}

export function App() {
  const hash = useHash();
  const loaded = useArchitectures();
  const route = parseSiteRoute(hash);
  return (
    <>
      <a className="skip-link" href="#site-main" onClick={skipToMain}>
        Skip to the diagrams
      </a>
      <header className="site-header">
        <div className="site-heading">
          <p className="site-eyebrow">C4 model</p>
          <p className="site-title">mansio architecture</p>
          <p className="site-purpose">
            Use these diagrams when you design a feature. They show what runs on{" "}
            <code>main</code>: the Go control and data planes and the TypeScript SDK. Drill from the system
            context down to components and the key flows. Every element traces to a commit-pinned source.
          </p>
          <p className="site-links">
            <a href={`${REPO}/tree/main/docs/architecture`}>
              Markdown diagrams in docs/architecture/
            </a>
            <span aria-hidden="true">·</span>
            <a href={`${REPO}/tree/main/docs/pages/content`}>
              Conversion inventories
            </a>
          </p>
        </div>
      </header>
      {loaded.state === "ready" ? <Site route={route} {...loaded} /> : (
        <SiteMain>
          {loaded.state === "loading" && <p role="status">Loading the architecture catalog…</p>}
          {loaded.state === "failed" && (
            <section role="alert" className="site-panel">
              <h2>The diagrams could not be loaded</h2>
              <p>{loaded.message}</p>
            </section>
          )}
        </SiteMain>
      )}
    </>
  );
}

function skipToMain(event: MouseEvent<HTMLAnchorElement>) {
  // Hash links drive routing here, so move focus without changing the URL.
  event.preventDefault();
  document.getElementById("site-main")?.focus();
}

function SiteMain({ children }: { readonly children: ReactNode }) {
  return <main id="site-main" tabIndex={-1} className="site-main">{children}</main>;
}

interface SiteProps {
  readonly route: SiteRoute;
  readonly architectures: readonly Architecture[];
  readonly defaultId: string;
}

/**
 * One architecture at a time. The sticky bar holds the architecture dropdown and, inside the provider, the
 * SDK's diagram navigation (Back, Up, breadcrumbs), so both work from anywhere in a tall diagram.
 */
function Site({ route, architectures, defaultId }: SiteProps) {
  const shownId = shownArchitectureId(route, defaultId);
  const shown = architectures.find((architecture) => architecture.id === shownId);
  if (!shown) {
    const message = route.kind === "not-found" ? route.message : `Unknown architecture "${shownId}".`;
    return (
      <>
        <SiteBar architectures={architectures} />
        <SiteMain>
          <NotFound message={message} architectures={architectures} />
        </SiteMain>
      </>
    );
  }
  return (
    <ArchitectureProvider
      key={shown.id}
      validatedBundle={shown.validated}
      routePort={browserRoutePort()}
      layoutPort={layoutPort}
    >
      <SiteBar architectures={architectures} shown={shown}>
        <DiagramNavigation />
      </SiteBar>
      <SiteMain>
        <section aria-label={shown.title}>
          <StatusNote architecture={shown.validated} />
          <div className="c4-viewer-grid">
            <div>
              <DiagramView />
            </div>
            <DiagramDocumentation />
          </div>
        </section>
      </SiteMain>
    </ArchitectureProvider>
  );
}

function SiteBar(
  { architectures, shown, children }: {
    readonly architectures: readonly Architecture[];
    readonly shown?: Architecture;
    readonly children?: ReactNode;
  },
) {
  const choose = (event: ChangeEvent<HTMLSelectElement>) => {
    const next = architectures.find((architecture) => architecture.id === event.target.value);
    if (next) {
      browserRoutePort().push({
        architectureId: next.id,
        diagramId: next.validated.bundle.defaultDiagramId,
      });
    }
  };
  return (
    <div className="site-bar">
      <div className="site-bar-inner">
        <div className="architecture-picker">
          <label htmlFor="architecture-select">Architecture</label>
          <select id="architecture-select" value={shown?.id ?? ""} onChange={choose}>
            {!shown && <option value="" disabled>Choose an architecture</option>}
            {architectures.map((architecture) => (
              <option key={architecture.id} value={architecture.id}>
                {architecture.title}
              </option>
            ))}
          </select>
        </div>
        {children}
      </div>
    </div>
  );
}

/** What the dashed boxes and lines mean in this architecture, counted from its element statuses. */
function StatusNote({ architecture }: { readonly architecture: ValidatedArchitecture }) {
  const { model, title } = architecture.bundle;
  const items = [...model.elements, ...model.relationships];
  const changing = items.filter((item) => item.status !== "existing").length;
  return (
    <p className={changing ? "dataset-note dataset-note-planned" : "dataset-note"}>
      <strong>{title}.</strong> {changing
        ? `${changing} of ${items.length} elements and relationships are planned or existing with planned changes; dashed boxes and lines mark them.`
        : `All ${items.length} elements and relationships exist on main.`}
    </p>
  );
}

function NotFound(
  { message, architectures }: {
    readonly message: string;
    readonly architectures: readonly Architecture[];
  },
) {
  return (
    <section role="alert" className="site-panel">
      <h2>Not Found</h2>
      <p>{message}</p>
      <ul>
        {architectures.map((architecture) => (
          <li key={architecture.id}>
            <a href={singleHash(architecture.id, architecture.validated.bundle.defaultDiagramId)}>
              Open {architecture.title}
            </a>
          </li>
        ))}
      </ul>
    </section>
  );
}
