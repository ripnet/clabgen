import { defineConfig } from "@playwright/test";

// UI tests run against the Vite dev server with the agent API mocked (see
// e2e/mock.ts); the protocol schemas come from the Go-generated fixture.
const port = 5179;

export default defineConfig({
  testDir: "e2e",
  timeout: 30_000,
  use: {
    baseURL: `http://127.0.0.1:${port}/`,
    // PLAYWRIGHT_CHROMIUM: a Chromium to use instead of Playwright's own.
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM } : {},
  },
  webServer: {
    command: `npx vite --port ${port} --strictPort --host 127.0.0.1`,
    url: `http://127.0.0.1:${port}/`,
    reuseExistingServer: !process.env.CI,
  },
});
