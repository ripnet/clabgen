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

test("disable all and enable all", async ({ page }) => {
  const mk = (name: string, disabled = false): Protocol => ({ name, type: "bgp", device: "r1", state: disabled ? "disabled" : "running", config: {}, disabled });
  const ps = [mk("a"), mk("b"), mk("c", true)];
  await mockAgent(page, ps[0], () => undefined, { more: ps.slice(1) });
  await page.goto("./#/a/a1/protocols");
  const head = page.locator(".card-head");
  await expect(head.getByRole("button", { name: "Enable all (1)" })).toBeVisible();

  // Disabling asks first; dismissing changes nothing.
  page.once("dialog", (d) => d.dismiss());
  await head.getByRole("button", { name: "Disable all (2)" }).click();
  expect(ps.map((p) => !!p.disabled)).toEqual([false, false, true]);

  page.once("dialog", (d) => d.accept());
  await head.getByRole("button", { name: "Disable all (2)" }).click();
  await expect(head.getByRole("button", { name: "Enable all (3)" })).toBeVisible();
  await expect(head.getByRole("button", { name: /Disable all/ })).toHaveCount(0);
  expect(ps.every((p) => p.disabled)).toBe(true);

  await head.getByRole("button", { name: "Enable all (3)" }).click();
  await expect(head.getByRole("button", { name: "Disable all (3)" })).toBeVisible();
  expect(ps.some((p) => p.disabled)).toBe(false);
});

test("enable all reports the protocols it could not change", async ({ page }) => {
  const mk = (name: string): Protocol => ({ name, type: "bgp", device: "r1", state: "disabled", config: {}, disabled: true });
  const ps = [mk("a"), mk("b")];
  await mockAgent(page, ps[0], () => undefined, { more: ps.slice(1), refuseToggle: ["b"] });
  await page.goto("./#/a/a1/protocols");
  await page.getByRole("button", { name: "Enable all (2)" }).click();
  await expect(page.getByText("Could not enable b: protocol b: refused")).toBeVisible();
  expect(ps.map((p) => !!p.disabled)).toEqual([false, true]);
  await expect(page.getByRole("button", { name: "Enable all (1)" })).toBeVisible();
});
