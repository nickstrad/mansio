import { defineConfig, devices } from "@playwright/test";

// Smoke test of the production build (run `npm run build` first; `npm run check` does). It serves dist/ with
// `vite preview` at the GitHub Pages base path, so asset and worker URLs are exercised under /mansio/.
const port = 4319;

export default defineConfig({
  testDir: "tests",
  testMatch: "**/*.spec.ts",
  // Keep run artifacts out of the tree: the root deno fmt/lint scan docs/pages/ and test-results/ is not gitignored.
  outputDir: "node_modules/.cache/playwright-results",
  timeout: 60_000,
  reporter: "line",
  use: { baseURL: `http://127.0.0.1:${port}/mansio/` },
  projects: [{
    name: "chromium",
    use: {
      ...devices["Desktop Chrome"],
      viewport: { width: 1600, height: 1000 },
    },
  }],
  webServer: {
    command: `npx vite preview --base /mansio/ --host 127.0.0.1 --port ${port} --strictPort`,
    url: `http://127.0.0.1:${port}/mansio/`,
    reuseExistingServer: false,
  },
});
