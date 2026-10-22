import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

test("disable and enable a protocol", async ({ page }) => {
  const p: Protocol = { name: "bgp1", type: "bgp", device: "r1", state: "running", config: { local_as: 65000 } };
  await mockAgent(page, p, () => undefined);

  // From the list: the toggle doesn't open the protocol.
  await page.goto("./#/a/a1/protocols");
  const row = page.locator("tr", { hasText: "bgp1" });
  await row.getByRole("button", { name: "on" }).click();
  await expect(row).toContainText("disabled");
  expect(p.disabled).toBe(true);
  expect(page.url()).toContain("/protocols");

  // On the protocol page.
  await row.getByRole("link", { name: "bgp1" }).click();
  await expect(page.getByText("Disabled: the configuration is kept")).toBeVisible();
  await page.getByRole("button", { name: "off" }).click();
  await expect(page.locator(".proto-state")).toContainText("running");
  expect(p.disabled).toBe(false);
  expect(p.config).toEqual({ local_as: 65000 });
});
