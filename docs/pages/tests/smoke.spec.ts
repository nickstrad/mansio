import { readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";
import type { Page } from "@playwright/test";

// A diagram is rendered once the SDK's canvas reports a finished layout.
const canvas = "[data-layout-ready]";

const picker = (page: Page) => page.getByRole("combobox", { name: "Architecture" });
const back = (page: Page) =>
  page.getByRole("navigation", { name: "Diagram navigation" }).getByRole("button", { name: "Back" });

const MAIN = "mansio-go-platform";
const main = JSON.parse(readFileSync(
  new URL(`../public/architectures/${MAIN}.json`, import.meta.url),
  "utf8",
)) as { title: string; diagrams: { id: string; title: string }[] };

/**
 * Serves the two-entry fixture catalog (the real bundle plus tests/fixtures/fixture-third.json, which is never
 * published) in place of catalog.json. The site code is the production build, unchanged.
 */
async function serveTwoEntryCatalog(page: Page) {
  const fixture = (name: string) => readFileSync(new URL(`fixtures/${name}`, import.meta.url), "utf8");
  await page.route(
    "**/catalog.json",
    (route) => route.fulfill({ contentType: "application/json", body: fixture("catalog-two.json") }),
  );
  await page.route(
    "**/architectures/fixture-third.json",
    (route) => route.fulfill({ contentType: "application/json", body: fixture("fixture-third.json") }),
  );
}

test("the site opens on main and lists only main", async ({ page }) => {
  await page.goto("./");
  await expect(page).toHaveURL(new RegExp(`#/architectures/${MAIN}/diagrams/context$`));
  await expect(page.getByRole("heading", { level: 1, name: "mansio Go platform: context" })).toBeVisible();
  await expect(page.locator(canvas)).toHaveCount(1);
  await expect(picker(page)).toHaveValue(MAIN);
  await expect(picker(page).getByRole("option")).toHaveText([main.title]);
});

test("a deep link to a diagram reloads to the same diagram, and Back returns", async ({ page }) => {
  await page.goto("./");
  await expect(page.locator(canvas)).toHaveCount(1);
  await page.goto(`./#/architectures/${MAIN}/diagrams/components`);
  const heading = page.getByRole("heading", {
    level: 1,
    name: "agentd: admission, IR and per-database execution",
  });
  await expect(heading).toBeVisible();
  await page.reload();
  await expect(heading).toBeVisible();
  await expect(page.locator(canvas)).toHaveCount(1);
  await back(page).click();
  await expect(page).toHaveURL(new RegExp(`#/architectures/${MAIN}/diagrams/context$`));
});

test("unknown routes show Not Found instead of falling back", async ({ page }) => {
  await page.goto("./#/architectures/nope/diagrams/context");
  await expect(page.getByRole("heading", { name: "Not Found" })).toBeVisible();
  await expect(picker(page)).toHaveValue("");
  await expect(page.getByRole("link", { name: `Open ${main.title}` })).toBeVisible();
  await page.goto("./#/somewhere");
  await expect(page.getByRole("heading", { name: "Not Found" })).toBeVisible();
  await page.goto(`./#/architectures/${MAIN}/diagrams/no-such-diagram`);
  await expect(page.getByText(/Not Found: unknown diagram/)).toBeVisible();
});

test("the skip link and the architecture dropdown are keyboard reachable", async ({ page }) => {
  await page.goto("./");
  await expect(page.locator(canvas)).toHaveCount(1);
  await page.keyboard.press("Tab");
  await expect(page.getByRole("link", { name: "Skip to the diagrams" })).toBeFocused();
  // The two intro links come next, then the dropdown.
  for (let i = 0; i < 3; i++) await page.keyboard.press("Tab");
  await expect(picker(page)).toBeFocused();
});

test("a two-entry catalog works with no code change and opens its defaultId", async ({ page }) => {
  await serveTwoEntryCatalog(page);
  await page.goto("./");
  // The fixture's defaultId is its last entry.
  await expect(page).toHaveURL(/#\/architectures\/fixture-third\/diagrams\/fixture-context$/);
  await expect(page.getByRole("heading", { level: 1, name: "System context: fixture system" })).toBeVisible();
  await expect(page.locator(canvas)).toHaveCount(1);
  await expect(picker(page)).toHaveValue("fixture-third");
  await expect(picker(page).getByRole("option")).toHaveText([main.title, "Fixture: a third architecture"]);
  await picker(page).selectOption(MAIN);
  await expect(page).toHaveURL(new RegExp(`#/architectures/${MAIN}/diagrams/context$`));
  await expect(page.locator(canvas)).toHaveCount(1);
  await back(page).click();
  await expect(page).toHaveURL(/#\/architectures\/fixture-third\/diagrams\/fixture-context$/);
});

test("the top bar stays in the viewport while a tall diagram page scrolls", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.goto(`./#/architectures/${MAIN}/diagrams/components`);
  await expect(page.locator(canvas)).toHaveCount(1);
  const height = await page.evaluate(() => document.documentElement.scrollHeight);
  expect(height).toBeGreaterThan(720);
  await page.evaluate(() => globalThis.scrollTo(0, document.documentElement.scrollHeight));
  await expect(picker(page)).toBeInViewport();
  await expect(back(page)).toBeInViewport();
});

// Every view of main lays out on its own canvas.
for (const diagram of main.diagrams) {
  test(`the ${diagram.id} view renders`, async ({ page }) => {
    await page.goto(`./#/architectures/${MAIN}/diagrams/${diagram.id}`);
    await expect(page.getByRole("heading", { level: 1, name: diagram.title, exact: true })).toBeVisible();
    await expect(page.locator(canvas)).toHaveCount(1);
    await expect(picker(page)).toHaveValue(MAIN);
  });
}
