import { describe, expect, it } from "vitest";
import type { ArchitectureBundle, Step } from "c4-sdk/core";
import { bundles } from "./fixtures.ts";

type Interaction = Extract<Step, { kind: "interaction" }>;
type DiagramOf<K extends ArchitectureBundle["diagrams"][number]["kind"]> = Extract<
  ArchitectureBundle["diagrams"][number],
  { kind: K }
>;

const all = bundles.map(({ entry, json }) => ({
  id: entry.id,
  bundle: json as ArchitectureBundle,
}));

/** Every step of a scenario tree, depth first. */
function steps(step: Step): Step[] {
  if (step.kind === "interaction") return [step];
  const children = step.kind === "sequence" ? step.children : step.branches;
  return [step, ...children.flatMap(steps)];
}

const interactions = (step: Step) => steps(step).filter((s): s is Interaction => s.kind === "interaction");

describe("C4 kinds", () => {
  it("draws context, container, component and dynamic views of main", () => {
    const kinds = new Set(
      all.flatMap(({ bundle }) => bundle.diagrams.map((diagram) => diagram.kind)),
    );
    expect([...kinds].sort()).toEqual(["component", "container", "context", "dynamic"]);
  });
});

describe.each(all.map(({ id }) => id))(
  "dynamic views of %s",
  (dataset) => {
    const bundle = all.find(({ id }) => id === dataset)!.bundle;
    const relationships = new Map(
      bundle.model.relationships.map((r) => [r.id, r]),
    );
    const dynamics = bundle.diagrams.filter((d): d is DiagramOf<"dynamic"> => d.kind === "dynamic");
    const cases = dynamics.map((d) => [d.id, d] as const);

    it("draws the query and database-move flows", () => {
      expect(dynamics.map((d) => d.id)).toEqual(expect.arrayContaining(["query-flow", "database-move"]));
    });

    it.each(cases)(
      "%s: every interaction uses a relationship whose endpoints are members of the diagram",
      (_, diagram) => {
        const members = new Set(diagram.nodes.map((node) => node.elementId));
        const declared = new Set(
          diagram.edges.map((edge) => edge.relationshipId),
        );
        const outside = interactions(diagram.scenario).flatMap((step) => {
          const relationship = relationships.get(step.relationshipId);
          if (!relationship) {
            return [`${step.id}: unknown relationship ${step.relationshipId}`];
          }
          const problems: string[] = [];
          if (!declared.has(relationship.id)) {
            problems.push(`${step.id}: ${relationship.id} is not an edge`);
          }
          for (const end of [relationship.sourceId, relationship.targetId]) {
            if (!members.has(end)) {
              problems.push(`${step.id}: ${end} is not a member`);
            }
          }
          return problems;
        });
        expect(outside).toEqual([]);
      },
    );

    it.each(cases)(
      "%s: step ids are unique, every edge is used by a step and every member takes part",
      (_, diagram) => {
        const ids = steps(diagram.scenario).map((step) => step.id);
        expect(new Set(ids).size).toBe(ids.length);
        const used = new Set(
          interactions(diagram.scenario).map((step) => step.relationshipId),
        );
        expect(
          diagram.edges.map((edge) => edge.relationshipId).filter((id) => !used.has(id)),
        ).toEqual([]);
        const ends = new Set(
          [...used].flatMap((
            id,
          ) => [
            relationships.get(id)!.sourceId,
            relationships.get(id)!.targetId,
          ]),
        );
        expect(
          diagram.nodes.map((node) => node.elementId).filter((id) => !ends.has(id)),
        ).toEqual([]);
      },
    );

    it.each(cases)(
      "%s: every parallel group has at least two branches",
      (_, diagram) => {
        const thin = steps(diagram.scenario).filter((s) => s.kind === "parallel" && s.branches.length < 2);
        expect(thin.map((s) => s.id)).toEqual([]);
      },
    );
  },
);
