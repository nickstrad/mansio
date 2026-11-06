import { describe, expect, it } from "vitest";
import { chooseDefault, parseSiteRoute, shownArchitectureId } from "../src/routes.ts";

describe("parseSiteRoute", () => {
  it.each([
    ["", { kind: "home" }],
    ["#", { kind: "home" }],
    ["#/architectures/mansio-current/diagrams/context", {
      kind: "single",
      architectureId: "mansio-current",
      diagramId: "context",
    }],
    [
      "#/architectures/mansio-current/diagrams/components-server/elements/storage",
      {
        kind: "single",
        architectureId: "mansio-current",
        diagramId: "components-server",
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
    "#/architectures/mansio-current",
    "#/architectures/mansio-current/diagrams/context/extra",
  ])("%j is Not Found", (hash) => {
    expect(parseSiteRoute(hash).kind).toBe("not-found");
  });
});

describe("chooseDefault", () => {
  // Three peer entries; none is special to the code.
  const ids = ["mansio-current", "mansio-lanes-proposal", "fixture-third"];

  it("uses defaultId when it names an entry", () => {
    expect(chooseDefault(ids, "fixture-third")).toEqual({ ok: true, id: "fixture-third" });
    expect(chooseDefault(ids, "mansio-current")).toEqual({ ok: true, id: "mansio-current" });
  });

  it("falls back to the first entry when the catalog has no defaultId", () => {
    expect(chooseDefault(ids, undefined)).toEqual({ ok: true, id: "mansio-current" });
    expect(chooseDefault(["fixture-third", "mansio-current"], undefined)).toEqual({
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
        { kind: "single", architectureId: "mansio-lanes-proposal", diagramId: "lanes-context" },
        "mansio-current",
      ),
    ).toBe("mansio-lanes-proposal");
    expect(shownArchitectureId({ kind: "not-found", message: "x" }, "mansio-current"))
      .toBeUndefined();
  });
});
