import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const rsvp: Protocol = {
  name: "rsvp",
  type: "rsvp",
  device: "d1",
  state: "running",
  config: { router_id: "192.0.2.1", tail_mode: "label", label_base: 100000, refresh_interval: 30, hello_interval: 5, retry_interval: 5, lsps: [{ name: "to-tail", to: "192.0.2.2", protection: "link" }] },
  status: {
    router_id: "192.0.2.1",
    heads: [
      {
        name: "to-tail",
        to: "192.0.2.2",
        tunnel_id: 1,
        lsp_id: 7,
        state: "up",
        label: 524287,
        next_hop: "10.0.1.1",
        protection: "link",
        path: [
          { address: "10.0.1.1", label: 524287, protection: "in use" },
          { address: "10.0.2.2", label: 100000 },
        ],
        locally_repaired: true,
        setups: 1,
      },
    ],
    tails: [{ name: "back", from: "192.0.2.9", to: "192.0.2.1", tunnel_id: 2, lsp_id: 3, label: 3, previous_hop: "10.0.1.1", style: "shared explicit", expires_in_s: 100, since: "2026-01-01T00:00:00Z" }],
    neighbors: [{ address: "10.0.1.1", hellos: "up" }],
    counters: {},
  },
};

const flow: Protocol = {
  name: "f1",
  type: "icmp",
  device: "d1",
  state: "running",
  config: { destination: "192.0.2.2", mode: "continuous", rate: 1, timeout: "1s", ttl: 64, ip_checksum: "correct", seq_mode: "increment", icmp_checksum: "correct", payload_size: 56, payload_pattern: "increment", signature: true },
  status: { state: "running", flow_id: "1", destination: "192.0.2.2", source: "10.0.1.2", rate: 1, tracks_replies: true, tx: 1, rx: 1, lost: 0, loss_pct: 0, outage_max_ms: 0, outage_current_ms: 0, recent: [], timeline: [], lsp: "to-tail" },
};

test("an RSVP router's page shows LSPs, paths and a local repair", async ({ page }) => {
  await mockAgent(page, rsvp, () => undefined);
  await page.goto("/#/a/a1/p/rsvp");
  const lsp = page.locator(".rsvp-lsp");
  await expect(lsp).toContainText("to-tail");
  await expect(lsp).toContainText("label 524287");
  await expect(lsp).toContainText("10.0.1.1 [524287] (in use)");
  await expect(lsp).toContainText("repaired this LSP locally");
  await expect(page.getByRole("cell", { name: "3 (implicit null)" })).toBeVisible();
  await expect(page.getByRole("cell", { name: "192.0.2.9" })).toBeVisible();
});

test("a flow picks an LSP on its device, and says when it's down", async ({ page }) => {
  let sent: Record<string, unknown> | undefined;
  await mockAgent(
    page,
    flow,
    (c) => {
      sent = c;
      return undefined;
    },
    { more: [rsvp] },
  );
  await page.goto("/#/a/a1/p/f1");
  await expect(page.getByText("in LSP")).toContainText("down, packets count as lost");
  await page.getByRole("button", { name: "Edit config" }).click();
  const pick = page.getByLabel("Send into an LSP");
  await expect(pick.locator("option")).toHaveCount(2);
  await pick.selectOption("to-tail");
  await page.getByRole("button", { name: /^Review / }).click();
  await page.getByRole("button", { name: /Apply/ }).click();
  await expect.poll(() => sent?.lsp).toBe("to-tail");
});
