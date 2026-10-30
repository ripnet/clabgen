import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

test("DHCP clients: lease summaries, and leased addresses on the device", async ({ page }) => {
  const v4: Protocol = {
    name: "cpe-v4",
    type: "dhcp",
    device: "cpe",
    state: "running",
    config: {},
    status: { state: "bound", address: "10.0.0.100/24", gateway: "10.0.0.1", counters: {} },
  };
  const v6: Protocol = {
    name: "cpe-v6",
    type: "dhcpv6",
    device: "cpe",
    state: "running",
    config: { ia_pd: true },
    status: { state: "bound", addresses: [{ prefix: "2001:db8::1000", preferred_in_s: 10, valid_in_s: 20 }], ia_pd_status: "NoPrefixAvail none left", counters: {} },
  };
  const waiting: Protocol = { name: "cpe2-v4", type: "dhcp", device: "cpe2", state: "running", config: {}, status: { state: "selecting", counters: {} } };
  await mockAgent(page, v4, () => undefined, { more: [v6, waiting] });
  await page.route("**/agents/a1/clabgen.v1.AgentService/ListDevices", (r) =>
    r.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        devices: [
          { name: "cpe", port: "eth1", mac: "02:00:00:00:00:01", dynamicAddresses: ["10.0.0.100/24", "fe80::1/64"], dynamicGateway4: "10.0.0.1" },
          { name: "cpe2", port: "eth1", mac: "02:00:00:00:00:02" },
        ],
      }),
    }),
  );

  await page.goto("./#/a/a1/protocols");
  await expect(page.locator("tr", { hasText: "cpe-v4" })).toContainText("10.0.0.100/24 · gw 10.0.0.1");
  // An address but no prefix: a warning naming the server's status.
  await expect(page.locator("tr", { hasText: "cpe-v6" })).toContainText("2001:db8::1000 · NoPrefixAvail none left");
  await expect(page.locator("tr", { hasText: "cpe2-v4" })).toContainText("selecting");

  await page.goto("./#/a/a1/devices");
  const row = page.locator("tr", { hasText: "cpe" }).first();
  await expect(row).toContainText("10.0.0.100/24dhcp");
  await expect(row).toContainText("gw 10.0.0.1");
  await expect(row.getByRole("button", { name: "Ping…" })).toBeEnabled();
  const bare = page.locator("tr", { hasText: "cpe2" });
  await expect(bare.getByRole("button", { name: "Ping…" })).toBeDisabled();
});
