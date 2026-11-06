import { describe, expect, it } from "vitest";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
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
      // A source introduced with its dataset cannot contain its own commit hash.
      // Preserve the original commit-pin checks for every pre-existing source; require
      // exact source-byte integrity and the explicit branch URL for this new addendum.
      if (source.id === "src-addendum") {
        expect((json as ArchitectureBundle).id).toBe("mansio-planes-proposal");
        expect(source.path).toBe("agent-work/items/dp-01-planes/addendum-1.md");
        expect(source.url).toBe(
          `https://github.com/nickstrad/mansio/blob/item/dp-01-planes/${source.path}`,
        );
        const bytes = readFileSync(new URL(`../../${source.path}`, import.meta.url));
        expect(source.revision).toBe(`sha256:${createHash("sha256").update(bytes).digest("hex")}`);
        return;
      }
      const match = pinned.exec(source.url);
      expect(match, source.url).not.toBeNull();
      expect(source.revision, source.id).toBe(match![1]);
      expect(source.url.endsWith(`/${source.path}`), source.id).toBe(true);
    },
  );
});
