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
  it("draws all seven C4 diagram kinds across the current and the proposed dataset", () => {
    const kinds = new Set(
      all.flatMap(({ bundle }) => bundle.diagrams.map((diagram) => diagram.kind)),
    );
    expect([...kinds].sort()).toEqual(
      [
        "code",
        "component",
        "container",
        "context",
        "deployment",
        "dynamic",
        "landscape",
      ],
    );
  });
});

describe.each(["mansio-lanes-proposal", "mansio-planes-proposal", "mansio-planes-fable"])(
  "dynamic views of %s",
  (dataset) => {
    const proposed = all.find(({ id }) => id === dataset)!.bundle;
    const relationships = new Map(
      proposed.model.relationships.map((r) => [r.id, r]),
    );
    const dynamics = proposed.diagrams.filter((d): d is DiagramOf<"dynamic"> => d.kind === "dynamic");
    const cases = dynamics.map((d) => [d.id, d] as const);

    it("draws the four key flows", () => {
      for (
        const id of (dataset === "mansio-planes-proposal" || dataset === "mansio-planes-fable"
          ? [
            "flow-create-database",
            "flow-query-right-node",
            "flow-move-database",
            "flow-drop-database",
          ]
          : [
            "flow-point-read",
            "flow-long-query-interrupt",
            "flow-forwarded-mutation",
            "flow-lane-kill-restart",
          ])
      ) {
        expect(dynamics.map((d) => d.id)).toContain(id);
      }
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

describe("the proposed deployment", () => {
  const proposed = all.find(({ id }) => id === "mansio-lanes-proposal")!.bundle;
  const elements = new Map(proposed.model.elements.map((e) => [e.id, e]));
  const deployments = proposed.diagrams.filter((
    d,
  ): d is DiagramOf<"deployment"> => d.kind === "deployment");

  it("is one illustrative view whose members and edges are all planned", () => {
    expect(deployments.map((d) => [d.environment, d.status])).toEqual([[
      "illustrative",
      "planned",
    ]]);
    const [deployment] = deployments;
    const statuses = [
      ...deployment.nodes.map((node) => elements.get(node.elementId)!.status),
      ...deployment.edges.map((edge) =>
        proposed.model.relationships.find((r) => r.id === edge.relationshipId)!
          .status
      ),
    ];
    expect(new Set(statuses)).toEqual(new Set(["planned"]));
  });

  it("has two data nodes, each holding lane instances", () => {
    const lanesByNode = new Map<string, number>();
    for (const element of proposed.model.elements) {
      if (element.kind === "instance" && element.instanceOf === "lanes") {
        lanesByNode.set(
          element.parentId,
          (lanesByNode.get(element.parentId) ?? 0) + 1,
        );
      }
    }
    expect(lanesByNode.size).toBe(2);
    expect([...lanesByNode.values()].every((count) => count >= 1)).toBe(true);
  });

  it("says in its document that it is illustrative and not the actual droplet", () => {
    const docIds = deployments[0].docIds ?? [];
    const text = proposed.documents.filter((d) => docIds.includes(d.id)).map((
      d,
    ) => d.plain).join("\n");
    expect(text).toMatch(/illustrative/i);
    expect(text).toMatch(/not a claim about the actual droplet/);
  });
});

describe("the current code view", () => {
  const current = all.find(({ id }) => id === "mansio-current")!.bundle;
  const elements = new Map(current.model.elements.map((e) => [e.id, e]));
  const code = current.diagrams.filter((d): d is DiagramOf<"code"> => d.kind === "code");

  it("is one view of document_reader.ts under the DocumentReaderClient component", () => {
    expect(code.map((d) => d.scopeId)).toEqual(["document-reader"]);
    const sources = new Map(current.sources.map((s) => [s.id, s]));
    expect(code[0].sourceIds?.map((id) => sources.get(id)!.path)).toContain(
      "platform/document_reader.ts",
    );
  });

  it("draws only existing, source-linked symbols; imported ones are tagged external", () => {
    for (const node of code[0].nodes) {
      const element = elements.get(node.elementId)!;
      expect(element.kind, element.id).toBe("code");
      expect(element.status, element.id).toBe("existing");
      expect(element.sourceIds.length, element.id).toBeGreaterThan(0);
      if (element.kind !== "code") continue;
      const local = element.parentId === "document-reader";
      expect(element.tags.includes("external"), element.id).toBe(!local);
      expect(element.members.length, element.id).toBeGreaterThan(0);
    }
  });
});

describe("database-scoped planes proposal", () => {
  const proposed = all.find(({ id }) => id === "mansio-planes-proposal")!.bundle;
  const elements = new Map(proposed.model.elements.map((e) => [e.id, e]));
  const deployment = proposed.diagrams.find((d) => d.kind === "deployment")!;

  it("keeps deployment illustrative with uneven database counts and tenants spanning nodes", () => {
    expect(deployment.kind === "deployment" && deployment.environment).toBe(
      "illustrative",
    );
    expect(deployment.status).toBe("planned");
    const files = proposed.model.elements.filter((e) => e.kind === "instance" && e.instanceOf === "files");
    const byHost = new Map<string, number>();
    for (const file of files) {
      if (file.kind === "instance") {
        byHost.set(file.parentId, (byHost.get(file.parentId) ?? 0) + 1);
      }
    }
    expect([...byHost.values()].sort()).toEqual([1, 2, 3]);
    expect(elements.get("dep-alpha-orders")).toMatchObject({
      parentId: "dep-a",
    });
    expect(elements.get("dep-alpha-analytics")).toMatchObject({
      parentId: "dep-b",
    });
    expect(elements.get("dep-beta-metrics")).toMatchObject({
      parentId: "dep-a",
    });
    for (const node of deployment.nodes) {
      expect(elements.get(node.elementId)!.status).toBe("planned");
    }
    for (const edge of deployment.edges) {
      const relationship = proposed.model.relationships.find((r) => r.id === edge.relationshipId)!;
      expect(relationship.status).toBe("planned");
      expect(relationship.modelRelationshipId).toBeTruthy();
    }
  });

  it("documents every element with both language variants and preserves landed-code status", () => {
    for (const element of proposed.model.elements) {
      expect(element.docIds.length, element.id).toBeGreaterThan(0);
      for (const id of element.docIds) {
        const document = proposed.documents.find((d) => d.id === id)!;
        expect(document.plain, id).toMatch(/zero or more databases/);
        expect(document.ste.text, id).toMatch(/zero or more databases/);
      }
    }
    expect(elements.get("classifier")!.status).toBe("existing");
    expect(elements.get("execution")!.status).toBe("existing");
    expect(elements.get("deadline")!.status).toBe("mixed");
    expect(elements.get("placement-router")!.status).toBe("mixed");
    expect(elements.get("lanes")!.status).toBe("planned");
  });

  it("keeps routing authority in the control plane and file access behind VM agents", () => {
    expect(elements.get("placement-router")).toMatchObject({
      parentId: "control",
    });
    expect(elements.get("node")!.name).toBe("VM agent (process)");
    const relationships = proposed.model.relationships;
    expect(relationships.find((r) => r.id === "c-resolve")).toMatchObject({
      sourceId: "data",
      targetId: "control",
    });
    expect(relationships.find((r) => r.id === "dp-route")).toMatchObject({
      sourceId: "authorization",
      targetId: "placement-router",
    });
    // Physical file access has one VM-local boundary, including agent-managed execution workers.
    for (
      const r of relationships.filter((r) => r.modelRelationshipId === "c-files")
    ) {
      expect(elements.get(r.sourceId)).toMatchObject({ instanceOf: "node" });
      expect(elements.get(r.targetId)).toMatchObject({ instanceOf: "files" });
    }
    expect(relationships.find((r) => r.id === "dep-query-to-dep-metadata"))
      .toBeUndefined();
    const provisioner = proposed.documents.find((d) => d.id === "doc-provisioner")!;
    expect(provisioner.plain).toMatch(/placement policy is an OPEN QUESTION/);
    expect(provisioner.sourceIds).toContain("src-addendum");
  });

  it("has separate failure scenarios for lifecycle and routing failures", () => {
    const ids = proposed.diagrams.filter((d) => d.kind === "dynamic").map((d) => d.id);
    expect(ids).toEqual(expect.arrayContaining([
      "flow-create-failed",
      "flow-query-stale",
      "flow-query-deadline",
      "flow-move-failed",
      "flow-drop-failed",
    ]));
  });
});

describe("the Fable revision of the planes proposal (DP-02, M1–M8)", () => {
  const fable = all.find(({ id }) => id === "mansio-planes-fable")!.bundle;
  const elements = new Map(fable.model.elements.map((e) => [e.id, e]));
  const documents = new Map(fable.documents.map((d) => [d.id, d]));
  const diagramIds = fable.diagrams.map((d) => d.id);
  const text = (id: string) => documents.get(id)!.plain + "\n" + documents.get(id)!.ste.text;

  it("M1: draws the route grant in the gateway and says a stale retry obtains a fresh decision", () => {
    expect(elements.get("grant-cache")).toMatchObject({
      parentId: "data",
      status: "planned",
    });
    expect(fable.model.relationships.find((r) => r.id === "dp-grant"))
      .toMatchObject({
        sourceId: "authorization",
        targetId: "grant-cache",
      });
    expect(diagramIds).toEqual(
      expect.arrayContaining(["flow-query-grant-reuse", "flow-query-stale"]),
    );
    expect(text("doc-flow-query-stale")).toMatch(/fresh decision/);
    expect(text("doc-node-admission")).toMatch(
      /existing grants serve until their TTL; no new grants/i,
    );
    expect(text("doc-node-admission")).not.toMatch(
      /one-request admission decision/,
    );
  });

  it("M3: has the lane view with snapshots off the writer lock; M4: names the default database", () => {
    const lane = fable.diagrams.find((d) => d.id === "planes-lane-components")!;
    expect(lane.kind).toBe("component");
    expect(lane.nodes.map((n) => n.elementId)).toEqual(
      expect.arrayContaining([
        "lane-snapshot",
        "lane-scheduler",
        "classifier",
        "execution",
        "deadline",
      ]),
    );
    expect(text("doc-lane-snapshot")).toMatch(/read-only/);
    expect(text("doc-ingress")).toMatch(/`default`/);
    expect(elements.get("classifier")!.status).toBe("existing");
    expect(elements.get("execution")!.status).toBe("existing");
    expect(elements.get("deadline")!.status).toBe("mixed");
    expect(elements.get("placement-router")!.status).toBe("mixed");
  });

  it("M5, M6, M7: recovery, manifest and instance-move flows exist with their failure alternatives", () => {
    expect(diagramIds).toEqual(expect.arrayContaining([
      "flow-recover-from-loss",
      "flow-recover-refused",
      "flow-drop-lost-agent",
      "flow-backup-manifest",
      "flow-backup-unverified",
      "flow-move-instances",
      "flow-move-failed-instances",
    ]));
    expect(text("doc-flow-recover-refused")).toMatch(
      /never permission to activate a copy/,
    );
    expect(text("doc-backup-registry")).toMatch(/restore candidate only if/);
    const move = fable.diagrams.find((d) => d.id === "flow-move-instances")!;
    expect(move.kind === "deployment" && move.environment).toBe("illustrative");
    expect(move.nodes.map((n) => n.elementId)).toEqual(
      expect.arrayContaining([
        "dep-a-agent",
        "dep-b-agent",
        "dep-control",
        "dep-metadata",
        "dep-alpha-archive",
      ]),
    );
    expect(move.edges.map((e) => e.relationshipId)).toContain(
      "dep-a-agent-to-dep-b-agent",
    );
    expect(text("doc-flow-move-instances")).toMatch(
      /Checkpoint through a controlled maintenance connection/,
    );
  });

  it("M8: the candidate placement rule is labelled pending Nick's decision", () => {
    expect(elements.get("placement-policy")).toMatchObject({
      parentId: "control",
      status: "planned",
    });
    expect(text("doc-placement-policy")).toMatch(/PENDING NICK'S DECISION/);
    expect(text("doc-provisioner")).toMatch(/OPEN/);
  });

  it("stays a proposal: every new element is planned and every element is documented in both variants", () => {
    for (const element of fable.model.elements) {
      expect(element.docIds.length, element.id).toBeGreaterThan(0);
      for (const id of element.docIds) {
        expect(documents.get(id)!.plain, id).toMatch(
          /Fable revision \(DP-02\)/,
        );
        expect(documents.get(id)!.ste.text, id).toMatch(
          /Fable revision of the planes proposal/,
        );
      }
    }
    for (
      const id of [
        "grant-cache",
        "lane-ipc",
        "lane-request-ids",
        "lane-snapshot",
        "backup-registry",
        "placement-policy",
        "platform-operator",
        "provider",
      ]
    ) {
      expect(elements.get(id)!.status, id).toBe("planned");
    }
  });
});
