import { readFileSync } from "node:fs";

const root = new URL("../", import.meta.url);

export const readText = (path: string): string => readFileSync(new URL(path, root), "utf8");
export const readJson = (path: string): unknown => JSON.parse(readText(path));

export interface CatalogFile {
  /** The entry the site opens when the URL names none; without it, the first entry. */
  readonly defaultId?: string;
  readonly entries: readonly {
    readonly id: string;
    readonly title: string;
    readonly url: string;
  }[];
}

export const catalog = readJson("public/catalog.json") as CatalogFile;

/** Each catalog entry with its raw bundle JSON, resolved the way the site resolves it (relative to the catalog). */
export const bundles = catalog.entries.map((entry) => ({
  entry,
  path: `public/${entry.url}`,
  json: readJson(`public/${entry.url}`),
}));
