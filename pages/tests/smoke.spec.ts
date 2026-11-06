import { readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";
import type { Page } from "@playwright/test";

// A diagram is rendered once the SDK's canvas reports a finished layout.
const canvas = "[data-layout-ready]";

const picker = (page: Page) => page.getByRole("combobox", { name: "Architecture" });
const back = (page: Page) =>
  page.getByRole("navigation", { name: "Diagram navigation" }).getByRole("button", { name: "Back" });

/**
 * Serves the three-entry fixture catalog (the two real bundles plus tests/fixtures/fixture-third.json, which is
 * never published) in place of catalog.json. The site code is the production build, unchanged.
 */
async function serveThreeEntryCatalog(page: Page) {
  const fixture = (name: string) => readFileSync(new URL(`fixtures/${name}`, import.meta.url), "utf8");
  await page.route(
    "**/catalog.json",
    (route) => route.fulfill({ contentType: "application/json", body: fixture("catalog-three.json") }),
  );
  await page.route(
    "**/architectures/fixture-third.json",
    (route) => route.fulfill({ contentType: "application/json", body: fixture("fixture-third.json") }),
  );
}

test("the site opens on the default architecture and lists every catalog entry", async ({ page }) => {
  await page.goto("./");
  await expect(page).toHaveURL(
    /#\/architectures\/mansio-current\/diagrams\/context$/,
  );
  await expect(
    page.getByRole("heading", {
      level: 1,
      name: "System context: Document DB platform",
    }),
  ).toBeVisible();
  await expect(page.locator(canvas)).toHaveCount(1);
  await expect(picker(page)).toHaveValue("mansio-current");
  await expect(picker(page).getByRole("option")).toHaveText([
    "mansio today (main 2d30330)",
    "Proposed: statement lanes and tenant placement (scale-out)",
    "Proposed: control plane / data plane — tenants own many databases",
    "Fable revision: control/data planes with route grants (M1–M8 applied)",
  ]);
});

test("choosing an architecture opens its default diagram, and Back returns", async ({ page }) => {
  await page.goto("./");
  await expect(page.locator(canvas)).toHaveCount(1);
  await picker(page).selectOption("mansio-lanes-proposal");
  await expect(page).toHaveURL(
    /#\/architectures\/mansio-lanes-proposal\/diagrams\/lanes-context$/,
  );
  await expect(
    page.getByRole("heading", {
      level: 1,
      name: "System context: proposed statement lanes and placement",
    }),
  ).toBeVisible();
  await expect(page.locator(canvas)).toHaveCount(1);
  await back(page).click();
  await expect(page).toHaveURL(/#\/architectures\/mansio-current\/diagrams\/context$/);
  await expect(picker(page)).toHaveValue("mansio-current");
  await expect(page.locator(canvas)).toHaveCount(1);
});

test("a deep link to a diagram of another entry reloads to the same diagram", async ({ page }) => {
  await page.goto(
    "./#/architectures/mansio-lanes-proposal/diagrams/lanes-containers",
  );
  await expect(
    page.getByRole("heading", {
      level: 1,
      name: "Containers: proposed data node with statement lanes",
    }),
  )
    .toBeVisible();
  await expect(picker(page)).toHaveValue("mansio-lanes-proposal");
  await page.reload();
  await expect(
    page.getByRole("heading", {
      level: 1,
      name: "Containers: proposed data node with statement lanes",
    }),
  )
    .toBeVisible();
  await expect(page.locator(canvas)).toHaveCount(1);
});

test("unknown routes show Not Found instead of falling back", async ({ page }) => {
  await page.goto("./#/architectures/nope/diagrams/context");
  await expect(page.getByRole("heading", { name: "Not Found" })).toBeVisible();
  await expect(picker(page)).toHaveValue("");
  await expect(page.getByRole("link", { name: "Open mansio today (main 2d30330)" })).toBeVisible();
  await page.goto("./#/somewhere");
  await expect(page.getByRole("heading", { name: "Not Found" })).toBeVisible();
  await page.goto("./#/architectures/mansio-current/diagrams/no-such-diagram");
  await expect(page.getByText(/Not Found: unknown diagram/)).toBeVisible();
});

test("the skip link and the architecture dropdown are keyboard reachable", async ({ page }) => {
  await page.goto("./");
  await expect(page.locator(canvas)).toHaveCount(1);
  await page.keyboard.press("Tab");
  await expect(page.getByRole("link", { name: "Skip to the diagrams" }))
    .toBeFocused();
  // The two intro links come next, then the dropdown.
  for (let i = 0; i < 3; i++) await page.keyboard.press("Tab");
  await expect(picker(page)).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(page).toHaveURL(
    /#\/architectures\/mansio-lanes-proposal\/diagrams\/lanes-context$/,
  );
  await expect(page.locator(canvas)).toHaveCount(1);
});

test("a three-entry catalog works with no code change and opens its defaultId", async ({ page }) => {
  await serveThreeEntryCatalog(page);
  await page.goto("./");
  // The fixture's defaultId is its last entry.
  await expect(page).toHaveURL(/#\/architectures\/fixture-third\/diagrams\/fixture-context$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "System context: fixture system" }),
  ).toBeVisible();
  await expect(page.locator(canvas)).toHaveCount(1);
  await expect(picker(page)).toHaveValue("fixture-third");
  await expect(picker(page).getByRole("option")).toHaveText([
    "mansio today (main 2d30330)",
    "Proposed: statement lanes and tenant placement (scale-out)",
    "Fixture: a third architecture",
  ]);
  await picker(page).selectOption("mansio-current");
  await expect(page).toHaveURL(/#\/architectures\/mansio-current\/diagrams\/context$/);
  await expect(page.locator(canvas)).toHaveCount(1);
  await back(page).click();
  await expect(page).toHaveURL(/#\/architectures\/fixture-third\/diagrams\/fixture-context$/);
});

test("the top bar stays in the viewport while a tall diagram page scrolls", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.goto("./#/architectures/mansio-lanes-proposal/diagrams/lanes-components");
  await expect(page.locator(canvas)).toHaveCount(1);
  const height = await page.evaluate(() => document.documentElement.scrollHeight);
  expect(height).toBeGreaterThan(3 * 720);
  await page.evaluate(() => globalThis.scrollTo(0, document.documentElement.scrollHeight));
  await expect.poll(() => page.evaluate(() => globalThis.scrollY)).toBeGreaterThan(2 * 720);
  await expect(picker(page)).toBeInViewport();
  await expect(back(page)).toBeInViewport();
  // Both still work from the bottom of the page (a covered control would fail the click's hit test).
  await picker(page).selectOption("mansio-current");
  await expect(page).toHaveURL(/#\/architectures\/mansio-current\/diagrams\/context$/);
  await page.evaluate(() => globalThis.scrollTo(0, document.documentElement.scrollHeight));
  await expect(back(page)).toBeInViewport();
  await back(page).click();
  await expect(page).toHaveURL(/#\/architectures\/mansio-lanes-proposal\/diagrams\/lanes-components$/);
});

// One view of each kind PG-02 added; each must lay out on its own canvas.
for (
  const [architecture, diagram, title] of [
    ["mansio-current", "landscape", "System landscape: mansio, its people and its neighbours"],
    ["mansio-current", "code-document-reader", "Code: platform/document_reader.ts"],
    [
      "mansio-lanes-proposal",
      "flow-long-query-interrupt",
      "Flow: a long query stopped at its deadline (proposed)",
    ],
    [
      "mansio-lanes-proposal",
      "lanes-deployment",
      "Deployment (illustrative): two data nodes with statement lanes",
    ],
  ]
) {
  test(`the ${diagram} view renders`, async ({ page }) => {
    await page.goto(`./#/architectures/${architecture}/diagrams/${diagram}`);
    await expect(page.getByRole("heading", { level: 1, name: title })).toBeVisible();
    await expect(page.locator(canvas)).toHaveCount(1);
  });
}

// DP-01: use the production catalog, and render every proposed view, not just a fixture.
const planes = JSON.parse(readFileSync(
  new URL("../public/architectures/mansio-planes-proposal.json", import.meta.url),
  "utf8",
)) as { diagrams: { id: string; title: string }[] };

for (const diagram of planes.diagrams) {
  test(`the planes ${diagram.id} view renders`, async ({ page }) => {
    await page.goto(`./#/architectures/mansio-planes-proposal/diagrams/${diagram.id}`);
    await expect(page.getByRole("heading", { level: 1, name: diagram.title, exact: true })).toBeVisible();
    await expect(page.locator(canvas)).toHaveCount(1);
    await expect(picker(page)).toHaveValue("mansio-planes-proposal");
  });
}

// DP-02: the Fable revision renders every view from the production catalog too.
const fable = JSON.parse(readFileSync(
  new URL("../public/architectures/mansio-planes-fable.json", import.meta.url),
  "utf8",
)) as { diagrams: { id: string; title: string }[] };

for (const diagram of fable.diagrams) {
  test(`the fable ${diagram.id} view renders`, async ({ page }) => {
    await page.goto(`./#/architectures/mansio-planes-fable/diagrams/${diagram.id}`);
    await expect(page.getByRole("heading", { level: 1, name: diagram.title, exact: true })).toBeVisible();
    await expect(page.locator(canvas)).toHaveCount(1);
    await expect(picker(page)).toHaveValue("mansio-planes-fable");
  });
}

test("the Fable revision is selectable and Back preserves the current architecture", async ({ page }) => {
  await page.goto("./");
  await expect(page.locator(canvas)).toHaveCount(1);
  await picker(page).selectOption("mansio-planes-fable");
  await expect(page).toHaveURL(/#\/architectures\/mansio-planes-fable\/diagrams\/planes-context$/);
  await expect(page.locator(canvas)).toHaveCount(1);
  await back(page).click();
  await expect(picker(page)).toHaveValue("mansio-current");
});

test("the planes proposal is selectable and Back preserves the current architecture", async ({ page }) => {
  await page.goto("./");
  await expect(page.locator(canvas)).toHaveCount(1);
  await picker(page).selectOption("mansio-planes-proposal");
  await expect(page).toHaveURL(/#\/architectures\/mansio-planes-proposal\/diagrams\/planes-context$/);
  await expect(page.locator(canvas)).toHaveCount(1);
  await back(page).click();
  await expect(picker(page)).toHaveValue("mansio-current");
});
