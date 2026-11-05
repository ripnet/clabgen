import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const bgp: Protocol = {
  name: "bgp",
  type: "bgp",
  device: "d1",
  state: "running",
  config: {
    local_as: 65000,
    peers: [{ address: "192.0.2.12", remote_as: 65000, families: ["ipv4-vpn", "ipv6-vpn"] }],
    routes: [{ prefix: "198.51.100.0/24", count: 10, rd: "65000:1", label: 3001, route_targets: ["65000:100"] }],
  },
  status: { local_as: 65000, peers: [], advertised: {} },
} as unknown as Protocol;

const base = { state: "running", flow_id: "1", source: "198.51.100.1", rate: 10, tracks_replies: true, tx: 100, rx: 100, lost: 0, loss_pct: 0, outage_max_ms: 0, outage_current_ms: 0, recent: [], timeline: [] };
const config = { source: "198.51.100.1", route_target: "65000:100", mode: "continuous", rate: 10, timeout: "1s", ttl: 64, ip_checksum: "correct", seq_mode: "increment", icmp_checksum: "correct", payload_size: 56, payload_pattern: "increment", signature: true };

const inVPN = (status: Record<string, unknown>): Protocol =>
  ({ name: "f1", type: "icmp", device: "d1", state: "running", config: { ...config, destination: "203.0.113.1" }, status: { ...base, destination: "203.0.113.1", ...status } }) as unknown as Protocol;

test("a flow in a VPN shows its labels, and says when there's no way there", async ({ page }) => {
  await mockAgent(page, inVPN({ route_target: "65000:100", vpn_destinations: 10, vpn_resolved: 7, vpn_labels: [524283, 131070], vpn_route: "203.0.113.0/24", vpn_peer: "192.0.2.12", next_hop: "10.0.1.1" }), () => undefined);
  await page.goto("/#/a/a1/p/f1");
  const path = page.locator(".flow-path");
  await expect(path).toContainText("in VPN 65000:100");
  await expect(path).toContainText("labels 524283 131070");
  await expect(path).toContainText("to PE 192.0.2.12");
  await expect(path).toContainText("7 of 10 destinations reachable");

  await mockAgent(page, inVPN({ route_target: "65000:100", vpn_destinations: 1 }), () => undefined);
  await page.reload();
  await expect(page.locator(".flow-path")).toContainText("no way there, packets count as lost");
});

test("the VPN picker fills in the route target, destinations and source", async ({ page }) => {
  let sent: Record<string, unknown> | undefined;
  const plain = { name: "f1", type: "icmp", device: "d1", state: "running", config: { ...config, route_target: undefined, source: undefined, destination: "10.0.0.2" }, status: { ...base, destination: "10.0.0.2" } } as unknown as Protocol;
  await mockAgent(
    page,
    plain,
    (c) => {
      sent = c;
      return undefined;
    },
    { more: [bgp] },
  );
  await page.route("**/RunProtocolAction", async (route) => {
    const body = route.request().postDataJSON() as { action: string };
    if (body.action !== "rib") return route.fallback();
    const routes = [
      { family: "ipv4-vpn", prefix: "65000:100:203.0.113.0/24 label [131070]", vpn_prefix: "203.0.113.0/24", route_targets: ["65000:100"] },
      { family: "ipv4-vpn", prefix: "65000:100:203.0.114.0/24 label [131070]", vpn_prefix: "203.0.114.0/24", route_targets: ["65000:100"] },
      { family: "ipv6-vpn", prefix: "65000:100:2001:db8:c::/48 label [131070]", vpn_prefix: "2001:db8:c::/48", route_targets: ["65000:100"] },
      { family: "ipv4-unicast", prefix: "10.9.9.0/24" },
    ];
    await route.fulfill({ json: { result: { routes, total: routes.length } } });
  });
  await page.goto("/#/a/a1/p/f1");
  await page.getByRole("button", { name: "Edit config" }).click();
  await page.getByRole("button", { name: "In a VPN…" }).click();
  await page.getByRole("button", { name: /65000:100 · 2 IPv4 routes/ }).click();
  await expect(page.getByRole("button", { name: "Not in a VPN" })).toBeVisible();
  await page.getByRole("button", { name: /^Review / }).click();
  await page.getByRole("button", { name: /Apply/ }).click();
  await expect.poll(() => sent?.route_target).toBe("65000:100");
  expect(sent?.source).toBe("198.51.100.1");
  expect(sent?.destination).toBeUndefined();
  expect(sent?.destinations).toEqual([{ prefix: "203.0.113.0/24", count: 2 }]);
});
