import { describe, expect, it } from "vitest";
import { chooseDefault, parseSiteRoute, shownArchitectureId } from "../src/routes.ts";

describe("parseSiteRoute", () => {
  it.each([
    ["", { kind: "home" }],
    ["#", { kind: "home" }],
    ["#/architectures/mansio-go-platform/diagrams/context", {
      kind: "single",
      architectureId: "mansio-go-platform",
      diagramId: "context",
    }],
    [
      "#/architectures/mansio-go-platform/diagrams/components/elements/storage",
      {
        kind: "single",
        architectureId: "mansio-go-platform",
        diagramId: "components",
      },
    ],
    ["#/architectures/fixture-third/diagrams/fixture-context", {
      kind: "single",
      architectureId: "fixture-third",
      diagramId: "fixture-context",
    }],
  ])("%j", (hash, expected) => {
    expect(parseSiteRoute(hash)).toEqual(expected);
  });

  // Never a silent fallback: each of these is Not Found.
  it.each([
    "#/somewhere",
    "#/architectures/mansio-go-platform",
    "#/architectures/mansio-go-platform/diagrams/context/extra",
  ])("%j is Not Found", (hash) => {
    expect(parseSiteRoute(hash).kind).toBe("not-found");
  });
});

describe("chooseDefault", () => {
  // Three peer entries; none is special to the code.
  const ids = ["mansio-go-platform", "other-entry", "fixture-third"];

  it("uses defaultId when it names an entry", () => {
    expect(chooseDefault(ids, "fixture-third")).toEqual({ ok: true, id: "fixture-third" });
    expect(chooseDefault(ids, "mansio-go-platform")).toEqual({ ok: true, id: "mansio-go-platform" });
  });

  it("falls back to the first entry when the catalog has no defaultId", () => {
    expect(chooseDefault(ids, undefined)).toEqual({ ok: true, id: "mansio-go-platform" });
    expect(chooseDefault(["fixture-third", "mansio-go-platform"], undefined)).toEqual({
      ok: true,
      id: "fixture-third",
    });
  });

  it("reports a defaultId that names no entry instead of replacing it", () => {
    expect(chooseDefault(ids, "nope")).toEqual({
      ok: false,
      message: 'The catalog\'s defaultId "nope" names no catalog entry.',
    });
    expect(chooseDefault(ids, 3)).toEqual({
      ok: false,
      message: "The catalog's defaultId 3 names no catalog entry.",
    });
    expect(chooseDefault([], undefined)).toEqual({
      ok: false,
      message: "The catalog lists no architectures.",
    });
  });
});

describe("shownArchitectureId", () => {
  it("shows the default on an empty hash, the routed entry otherwise, nothing for Not Found", () => {
    expect(shownArchitectureId({ kind: "home" }, "fixture-third")).toBe("fixture-third");
    expect(
      shownArchitectureId(
        { kind: "single", architectureId: "fixture-third", diagramId: "fixture-context" },
        "mansio-go-platform",
      ),
    ).toBe("fixture-third");
    expect(shownArchitectureId({ kind: "not-found", message: "x" }, "mansio-go-platform"))
      .toBeUndefined();
  });
});
