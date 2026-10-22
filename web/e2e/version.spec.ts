import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const p = (): Protocol => ({ name: "bgp1", type: "bgp", device: "r1", state: "running", config: {} });

test("version, newer release and an older agent", async ({ page }) => {
  await mockAgent(page, p(), () => undefined, {
    agentVersion: "v0.1.0",
    ui: { version: "v0.2.0", latest: "v0.3.0", latest_url: "https://example.test/releases/v0.3.0", update_available: true },
  });
  await page.goto("./#/");
  await expect(page.locator(".brand-version")).toHaveText("v0.2.0");
  const link = page.getByRole("link", { name: "v0.3.0 available" });
  await expect(link).toHaveAttribute("href", "https://example.test/releases/v0.3.0");

  // The older agent: flagged in the nav, on the overview and its page.
  const nav = page.locator(".sidebar .side-link", { hasText: "a1" });
  await expect(nav.locator(".side-ver.warn")).toHaveText("update");
  await expect(nav).toHaveAttribute("title", /older than the UI/);
  await expect(page.locator(".warn-box")).toContainText("1 agent runs an older clabgen than this UI");
  await nav.click();
  await expect(page.locator(".agent-meta")).toContainText("v0.1.0 (UI v0.2.0)");
});

test("development builds: shown, not flagged", async ({ page }) => {
  await mockAgent(page, p(), () => undefined, { agentVersion: "414cf7c", ui: { version: "a4cd446", update_available: false } });
  await page.goto("./#/a/a1/protocols");
  await expect(page.locator(".brand-version")).toHaveText("dev a4cd446");
  await expect(page.getByRole("link", { name: /available/ })).toHaveCount(0);
  await expect(page.locator(".sidebar .side-ver")).toHaveCount(0);
  await expect(page.locator(".agent-meta")).toContainText("dev 414cf7c (UI dev a4cd446)");
});

test("same release: nothing extra", async ({ page }) => {
  await mockAgent(page, p(), () => undefined, { agentVersion: "v0.2.0", ui: { version: "v0.2.0", update_available: false } });
  await page.goto("./#/a/a1/protocols");
  await expect(page.locator(".brand-version")).toHaveText("v0.2.0");
  await expect(page.locator(".agent-meta")).not.toContainText("(UI");
  await expect(page.locator(".sidebar .side-ver")).toHaveCount(0);
});

test("a server without /api/info shows no version", async ({ page }) => {
  await mockAgent(page, p(), () => undefined, { agentVersion: "v0.2.0" });
  await page.goto("./#/a/a1/protocols");
  await expect(page.locator(".agent-meta")).toContainText("v0.2.0");
  await expect(page.locator(".brand-version")).toHaveCount(0);
});
