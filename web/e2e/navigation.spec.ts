import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const proto = { name: "lldp", type: "lldp", device: "d1", state: "running", config: {}, status: {} } as unknown as Protocol;

test("an agent opens on the tab it was last on, also after a reload", async ({ page }) => {
  await mockAgent(page, proto, () => undefined);
  await page.goto("/#/a/a1/events");
  await expect(page.locator(".tab.active")).toHaveText(/events/i);
  await page.goto("/#/flows");
  await page.locator(".sidebar").getByRole("link", { name: "a1" }).click();
  await expect(page).toHaveURL(/#\/a\/a1\/events$/);
  await expect(page.locator(".tab.active")).toHaveText(/events/i);

  await page.goto("/#/a/a1/ports");
  await page.reload();
  await page.goto("/#/");
  await page.locator(".sidebar").getByRole("link", { name: "a1" }).click();
  await expect(page).toHaveURL(/#\/a\/a1\/ports$/);

  // An agent without a remembered tab opens on the last one used anywhere.
  await page.evaluate(() => localStorage.setItem("clabgen.agentTabs", JSON.stringify({ agents: { elsewhere: "scenario" }, last: "scenario" })));
  await page.goto("/#/flows");
  await page.reload();
  await page.locator(".sidebar").getByRole("link", { name: "a1" }).click();
  await expect(page).toHaveURL(/#\/a\/a1\/scenario$/);
});
