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

test("DHCP servers: pool use, and how each lease was relayed", async ({ page }) => {
  const v4: Protocol = {
    name: "srv4",
    type: "dhcp-server",
    device: "srv",
    state: "running",
    config: {},
    status: {
      pools: [{ subnet: "10.2.0.0/24", range: "10.2.0.10-10.2.0.20", size: "11", bound: 1, offered: 0 }],
      leases: [
        { address: "10.2.0.10", state: "bound", mac: "02:00:00:00:0c:01", client_id: "01020000000c01", hostname: "cpe-1", expires_in_s: 590, relay: { giaddr: "10.2.0.1", hops: 2, circuit_id: "eth1:100", remote_id: "bng1" } },
      ],
      counters: { rx_discover: 1 },
    },
  };
  const v6: Protocol = {
    name: "srv6",
    type: "dhcpv6-server",
    device: "srv",
    state: "running",
    config: {},
    status: {
      duid: "00030001020000000001",
      paused: true,
      pools: [{ link: "2001:db8:2::/64", addresses: "2001:db8:2::100-2001:db8:2::1ff", addresses_bound: 1, prefixes: "2001:db8:200::/40 by /56", prefixes_bound: 0 }],
      bindings: [
        {
          prefix: "2001:db8:2::100",
          kind: "address",
          state: "bound",
          duid: "00030001020000000c01",
          iaid: 1,
          preferred_in_s: 500,
          valid_in_s: 800,
          relays: [
            { link_address: "::", peer_address: "2001:db8:2::1", hop_count: 1, interface_id: "uplink" },
            { link_address: "2001:db8:2::1", peer_address: "fe80::c1", hop_count: 0, interface_id: "ge-0/0/1.100", remote_id: "6527:cpe-7" },
          ],
        },
      ],
      counters: {},
    },
  };
  await mockAgent(page, v4, () => undefined, { more: [v6] });
  await page.goto("./#/a/a1/protocols");
  await expect(page.locator("tr", { hasText: "srv4" })).toContainText("10.2.0.0/24 1/11");
  await expect(page.locator("tr", { hasText: "srv6" })).toContainText("paused · 2001:db8:2::/64 1 addr, 0 pd");

  await page.locator("tr", { hasText: "srv4" }).getByRole("link", { name: "srv4" }).click();
  await expect(page.locator("tr", { hasText: "cpe-1" })).toContainText("10.2.0.1 (hops 2) · circuit eth1:100 · remote bng1");
  await page.goto("./#/a/a1/protocols");
  await page.locator("tr", { hasText: "srv6" }).getByRole("link", { name: "srv6" }).click();
  const row = page.locator("tr", { hasText: "2001:db8:2::100" });
  // The relay nearest the client first.
  await expect(row.locator("td").last()).toHaveText("link 2001:db8:2::1 · interface ge-0/0/1.100 · remote 6527:cpe-7link :: · interface uplink");
});

test("DHCP server pools: a placeholder names the default", async ({ page }) => {
  const p: Protocol = { name: "srv6", type: "dhcpv6-server", device: "srv", state: "running", config: { pools: [{ link: "2001:db8:1::/64" }] } };
  await mockAgent(page, p, () => undefined);
  await page.goto("./#/a/a1/p/srv6");
  await page.getByRole("button", { name: "Edit config" }).click();
  const modal = page.locator(".modal");
  await modal.locator(".section-nav").getByRole("button", { name: /pools/ }).click();
  await modal.locator(".obj-item-summary").first().click();
  await modal.getByRole("button", { name: /show all options/ }).click();
  const field = (label: RegExp) => modal.locator(".obj-item-body .field").filter({ has: page.locator(".field-label", { hasText: label }) }).locator("input");
  // "(default: …)" without its closing parenthesis.
  await expect(field(/^valid/)).toHaveAttribute("placeholder", "the server-wide valid");
  await expect(field(/^preferred/)).toHaveAttribute("placeholder", "the server-wide preferred");
});

test("DHCP client groups: bound members are healthy", async ({ page }) => {
  const group = { name: "clients", count: 3, specDisabled: false, disabled: new Set<number>(), health: "bound" };
  await mockAgent(page, { name: "x", type: "dhcp", device: "c", state: "running", config: {} }, () => undefined, { group });
  await page.goto("./#/a/a1/g/clients");
  await expect(page.locator(".group-proto")).toContainText("3 bound");
  await expect(page.locator(".group-proto .dot")).toHaveClass(/ok/);
  const rows = page.locator(".card", { has: page.getByRole("heading", { name: "Members" }) }).locator("tbody tr");
  await expect(rows.nth(0).locator(".dot")).toHaveClass(/ok/);
});
