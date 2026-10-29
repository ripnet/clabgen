import { expect, test, type Page } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const bgp = (): Protocol => ({
  name: "bgp1",
  type: "bgp",
  device: "r1",
  state: "running",
  config: {
    local_as: 65000,
    router_id: "192.0.2.1",
    hold_time: 30,
    peers: [{ address: "10.0.0.1", remote_as: 65001, families: ["ipv4-unicast"] }],
    routes: [{ prefix: "100.64.0.0/24", peers: ["10.0.0.1"] }],
  },
});

// validate stands in for the agent's config check.
const validate = (c: Record<string, unknown>) => {
  for (const p of (c.peers ?? []) as Record<string, unknown>[])
    if (p.remote_as === undefined) return `invalid config: peer ${p.address}: remote_as is required`;
  return undefined;
};

// edit opens the config editor of protocol p and returns the modal.
async function edit(page: Page, p: Protocol, update = validate) {
  await mockAgent(page, p, update);
  await page.goto("./#/a/a1/p/bgp1");
  await page.getByRole("button", { name: "Edit config" }).click();
  return page.locator(".modal");
}

test("sections, filter and nothing to apply", async ({ page }) => {
  const modal = await edit(page, bgp());
  const nav = modal.locator(".section-nav");
  await expect(nav.getByRole("button", { name: /peers/ })).toContainText("1");
  await expect(modal.getByRole("button", { name: "Review changes" })).toBeDisabled();

  // The filter goes to a section with hits and dims the others.
  await modal.getByLabel("Find a field").fill("remote_as");
  await expect(nav.getByRole("button", { name: /peers/ })).toHaveClass(/active/);
  await expect(nav.getByRole("button", { name: /general/ })).toHaveClass(/dim/);
  await modal.locator(".obj-item-summary").first().click();
  await expect(modal.locator(".obj-item-body .field")).toHaveCount(1);
  await expect(modal.locator(".obj-item-body .field-label")).toHaveText(/remote as/);
  await modal.getByLabel("Find a field").fill("");
  expect(await modal.locator(".obj-item-body .field").count()).toBeGreaterThan(1);
});

test("review, rejection marked by field, fix and apply", async ({ page }) => {
  const p = bgp();
  const modal = await edit(page, p);
  const nav = modal.locator(".section-nav");

  // local_as in general; a new peer without its remote AS.
  await modal.locator(".field", { hasText: "local as" }).locator("input").fill("65010");
  await expect(nav.getByRole("button", { name: /general/ }).locator(".section-changed")).toBeVisible();
  await nav.getByRole("button", { name: /peers/ }).click();
  await modal.getByRole("button", { name: "Add" }).click();
  await modal.locator(".obj-item-body .field", { hasText: "address" }).first().locator("input").fill("10.0.0.2");
  await expect(nav.getByRole("button", { name: /peers/ }).locator(".section-changed")).toBeVisible();

  await modal.getByRole("button", { name: "Review 2 changes" }).click();
  const rows = modal.locator(".diff-table tbody tr");
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0)).toContainText("local_as");
  await expect(rows.nth(0)).toContainText("65000");
  await expect(rows.nth(0)).toContainText("65010");
  await expect(rows.nth(1)).toContainText("peers[1]");
  await expect(rows.nth(1)).toContainText("new");
  await expect(rows.nth(1)).toContainText("10.0.0.2");

  // Rejected: back on the form, only the peer named and its field marked
  // (not the routes' peers field).
  await modal.getByRole("button", { name: "Apply" }).click();
  await expect(modal.getByRole("alert")).toContainText("peer 10.0.0.2: remote_as is required");
  await expect(nav.getByRole("button", { name: /peers/ })).toHaveClass(/error/);
  await expect(nav.getByRole("button", { name: /peers/ })).toHaveClass(/active/);
  await expect(nav.locator("button.error")).toHaveCount(1);
  const bad = modal.locator(".obj-item.error");
  await expect(bad).toHaveCount(1);
  await expect(bad).toHaveClass(/open/);
  await expect(bad.locator(".field.error")).toHaveCount(1);
  await expect(bad.locator(".field.error")).toContainText("remote as");
  await bad.locator(".field.error input").fill("65002");

  await modal.getByRole("button", { name: /Review/ }).click();
  await modal.getByRole("button", { name: "Apply" }).click();
  await expect(page.locator(".modal")).toHaveCount(0);
  expect(p.config.local_as).toBe(65010);
  expect(p.config.routes).toEqual([{ prefix: "100.64.0.0/24", peers: ["10.0.0.1"] }]);
  expect(p.config.peers).toEqual([
    { address: "10.0.0.1", remote_as: 65001, families: ["ipv4-unicast"] },
    { address: "10.0.0.2", remote_as: 65002 },
  ]);
});

test("YAML view", async ({ page }) => {
  const p = bgp();
  const modal = await edit(page, p);
  await modal.getByRole("button", { name: "edit as YAML" }).click();
  const text = modal.getByLabel("Configuration as YAML");
  await expect(text).toHaveValue(/local_as: 65000\n/);
  await expect(text).toHaveValue(/- address: 10\.0\.0\.1\n/);

  await text.fill("local_as: 65000\nrouter_id: 192.0.2.1\nhold_time: 45\npeers:\n  - address: 10.0.0.1\n    remote_as: 65001\nroutes:\n  - prefix: 100.64.0.0/24\n    peers: [10.0.0.1]\n");
  await modal.getByRole("button", { name: "form view" }).click();
  await expect(modal.locator(".field", { hasText: "hold time" }).locator("input")).toHaveValue("45");

  // A broken document keeps the last good value.
  await modal.getByRole("button", { name: "edit as YAML" }).click();
  await text.fill("peers: [");
  await expect(modal.locator(".error-text")).toBeVisible();
  await modal.getByRole("button", { name: "form view" }).click();

  await modal.getByRole("button", { name: "Review 2 changes" }).click();
  await expect(modal.locator(".diff-table")).toContainText("hold_time");
  await expect(modal.locator(".diff-table")).toContainText("peers[0].families");
});

test("a peer offers its families without showing all options", async ({ page }) => {
  const p = bgp();
  (p.config.peers as Record<string, unknown>[])[0] = { address: "10.0.0.1", remote_as: 65001 };
  const modal = await edit(page, p);
  await modal.locator(".section-nav").getByRole("button", { name: /peers/ }).click();
  await modal.getByRole("button", { name: /10\.0\.0\.1 AS 65001/ }).click();
  await expect(modal.getByRole("checkbox", { name: "ipv6-unicast" })).toBeVisible();
  await expect(modal.getByRole("button", { name: /show all options/ })).toBeVisible();
});

test("BGP: what applying does", async ({ page }) => {
  const modal = await edit(page, bgp());
  const yaml = async (doc: string) => {
    await modal.getByRole("button", { name: "edit as YAML" }).click();
    await modal.getByLabel("Configuration as YAML").fill(doc);
    await modal.getByRole("button", { name: "form view" }).click();
  };
  const head = "local_as: 65000\nrouter_id: 192.0.2.1\nhold_time: 30\n";
  const peer = (extra = "") => `peers:\n  - address: 10.0.0.1\n    remote_as: 65001\n    families: [ipv4-unicast]\n${extra}`;
  await yaml(head + peer() + "routes:\n  - prefix: 100.64.0.0/24\n    peers: [10.0.0.1]\n  - prefix: 100.64.1.0/24\n");
  await expect(modal.locator(".info-box")).toContainText("Applies in place");
  await yaml(head + peer("    hold_time: 60\n") + "routes:\n  - prefix: 100.64.0.0/24\n    peers: [10.0.0.1]\n");
  await expect(modal.locator(".warn-box")).toContainText("resets the session with 10.0.0.1");
  await yaml(head.replace("65000", "65009") + peer() + "routes:\n  - prefix: 100.64.0.0/24\n    peers: [10.0.0.1]\n");
  await expect(modal.locator(".warn-box")).toContainText("restarts the instance");
});

test("IS-IS, OSPF, CFM and LLDP: what applying does", async ({ page }) => {
  const cases: { type: string; config: Record<string, unknown>; inPlace: string; restart: string }[] = [
    { type: "isis", config: { system_id: "0000.0000.000a", metric: 10, hello_interval: 3 }, inPlace: "metric: 30", restart: "hello_interval: 5" },
    { type: "ospf", config: { router_id: "192.0.2.1", area: "0.0.0.0", cost: 10, hello_interval: 10 }, inPlace: "cost: 30", restart: "hello_interval: 5" },
    { type: "cfm-mep", config: { level: 5, mep_id: 1, ccm_enabled: true }, inPlace: "ccm_enabled: false", restart: "level: 4" },
    { type: "lldp", config: { mode: "tx-rx", system_name: "a" }, inPlace: "system_name: b", restart: "mode: rx-only" },
  ];
  for (const c of cases) {
    const p: Protocol = { name: "p1", type: c.type, device: "r1", state: "running", config: c.config };
    await mockAgent(page, p, () => undefined);
    await page.goto("./#/a/a1/p/p1");
    await page.reload(); // a new protocol under the same name
    await page.getByRole("button", { name: "Edit config" }).click();
    const modal = page.locator(".modal");
    const yaml = async (line: string) => {
      const key = line.split(":")[0];
      const rest = Object.entries(c.config).filter(([k]) => k !== key).map(([k, v]) => `${k}: ${v}`);
      await modal.getByRole("button", { name: "edit as YAML" }).click();
      await modal.getByLabel("Configuration as YAML").fill([...rest, line].join("\n") + "\n");
      await modal.getByRole("button", { name: "form view" }).click();
    };
    await yaml(c.inPlace);
    await expect(modal.locator(".info-box"), c.type).toContainText("Applies in place");
    await yaml(c.restart);
    await expect(modal.locator(".warn-box"), c.type).toContainText("restarts the instance");
    await modal.getByRole("button", { name: "Close" }).click();
  }
});
