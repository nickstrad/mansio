import { describe, expect, it } from "vitest";
import type { ArchitectureBundle } from "c4-sdk/core";
import { bundles } from "./fixtures.ts";

const pinned = /^https:\/\/github\.com\/nickstrad\/mansio\/blob\/([0-9a-f]{40})\/[^\s?#]+$/;

describe.each(bundles)("sources of $entry.id", ({ json }) => {
  const sources = (json as ArchitectureBundle).sources;

  it("has sources", () => {
    expect(sources.length).toBeGreaterThan(0);
  });

  it.each(sources.map((source) => [source.id, source] as const))(
    "%s has a pinned source revision",
    (_, source) => {
      const match = pinned.exec(source.url);
      expect(match, source.url).not.toBeNull();
      expect(source.revision, source.id).toBe(match![1]);
      expect(source.url.endsWith(`/${source.path}`), source.id).toBe(true);
    },
  );
});
