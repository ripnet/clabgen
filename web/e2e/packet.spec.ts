import { expect, test, type Page } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const svc = "clabgen.v1.AgentService";

const pk = (): Protocol => ({
  name: "pk1",
  type: "packet",
  device: "d1",
  state: "running",
  config: { layers: [{ ipv4: { dst: "10.0.0.2" } }, { payload: { size: 32 } }], mode: "continuous", rate: 10, timeout: "1s" },
});

type Req = { type: string; device: string; config: Record<string, unknown>; args?: { index?: number } };

// A preview the way the agent lays one out: an implied Ethernet header,
// then a region per config layer.
function previewOf(cfg: Record<string, unknown>, index: number) {
  if ((cfg.pcap as { data?: string } | undefined)?.data)
    return {
      hex: "ffffffffffff020000000001".padEnd(120, "0"),
      length: 60,
      summary: `captured frame ${index + 1}`,
      index,
      layers: [{ layer: 0, title: "Ethernet", offset: 0, length: 14, fields: [{ name: "dst", bit_offset: 0, bits: 48, value: "ff:ff:ff:ff:ff:ff" }] }],
      frames: [
        { index: 0, length: 60, summary: "ARP 10.0.0.1 > 10.0.0.9" },
        { index: 1, length: 90, summary: "IPv4 10.0.0.1 > 10.0.0.2, UDP 1 > 2" },
      ],
      decoded: index === 1 ? [{ ethernet: { dst: "02:00:00:00:00:02" } }, { ipv4: { dst: "10.0.0.2" } }, { udp: { dst_port: 2 } }] : [{ arp: { target_ip: "10.0.0.9" } }],
    };
  const layers = cfg.layers as Record<string, Record<string, unknown>>[];
  const out = [{ layer: -1, title: "Ethernet", offset: 0, length: 14, fields: [{ name: "dst", bit_offset: 0, bits: 48, value: "02:00:00:00:00:02" }] }];
  let off = 14;
  layers.forEach((l, i) => {
    const kind = Object.keys(l)[0];
    const fields = kind === "ipv4" ? [{ name: "src", bit_offset: (off + 12) * 8, bits: 32, value: "10.0.0.1" }, { name: "dst", bit_offset: (off + 16) * 8, bits: 32, value: String(l.ipv4.dst) }] : [];
    out.push({ layer: i, title: kind.toUpperCase(), offset: off, length: 20, fields });
    off += 20;
  });
  return { hex: "00".repeat(off), length: off, summary: `${layers.map((l) => Object.keys(l)[0]).join("/")} packet ${index}`, index, layers: out };
}

async function open(page: Page, p: Protocol, saved: Record<string, unknown>[]) {
  await mockAgent(page, p, (c) => {
    saved.push(c);
    return undefined;
  });
  const reqs: Req[] = [];
  await page.route(`**/agents/a1/${svc}/PreviewProtocol`, (r) => {
    const req = r.request().postDataJSON() as Req;
    reqs.push(req);
    const l = (req.config.layers ?? []) as Record<string, unknown>[];
    if (!req.config.pcap && l.length && Object.keys(l[0])[0] === "udp")
      return r.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ code: "invalid_argument", message: "invalid config: layer 1 (udp): needs an IPv4 or IPv6 layer before it" }) });
    return r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ result: previewOf(req.config, req.args?.index ?? 0) }) });
  });
  await page.goto("./#/a/a1/p/pk1");
  await page.getByRole("button", { name: "Edit config" }).click();
  return { modal: page.locator(".modal"), reqs };
}

test("layers, live preview, modifiers", async ({ page }) => {
  const saved: Record<string, unknown>[] = [];
  const { modal, reqs } = await open(page, pk(), saved);
  const cards = modal.locator(".pkt-layer");
  await expect(cards).toHaveCount(2);
  await expect(cards.first()).toContainText("IPv4");
  await expect(cards.first()).toContainText("10.0.0.1 → 10.0.0.2");
  await expect(modal.locator(".pkt-summary")).toHaveText("ipv4/payload packet 0");
  expect(reqs[0]).toMatchObject({ type: "packet", device: "d1" });

  // A new layer goes before the payload; the preview follows.
  await modal.getByLabel("Add a layer").selectOption("udp");
  await expect(cards).toHaveCount(3);
  await expect(cards.nth(1)).toContainText("UDP");
  await expect(modal.locator(".pkt-summary")).toHaveText("ipv4/udp/payload packet 0");

  // Editing a field.
  await cards.first().getByRole("button", { name: /IPv4/ }).click();
  await cards.first().locator("input").first().fill("10.0.0.9");
  await expect(cards.first()).toContainText("→ 10.0.0.9");

  // Moving a layer so it's invalid: the agent's error shows, the last preview stays (dimmed).
  await cards.nth(1).getByRole("button", { name: "Move up" }).click();
  await expect(modal.locator(".pkt-preview .error-box")).toContainText("needs an IPv4 or IPv6 layer before it");
  await expect(modal.locator(".pkt-preview .stale")).toBeVisible();
  await cards.first().getByRole("button", { name: "Move down" }).click();
  await expect(modal.locator(".pkt-preview .error-box")).toHaveCount(0);

  // Stepping packets asks for that index.
  await modal.getByRole("button", { name: "Next packet" }).click();
  await expect(modal.locator(".pkt-summary")).toHaveText("ipv4/udp/payload packet 1");

  // + modifier on a preview field adds one for that field.
  await modal.locator(".pkt-fields tr", { hasText: "src" }).getByRole("button", { name: "+ modifier" }).first().click();
  await expect(modal.locator(".section-nav").getByRole("button", { name: /modifiers/ })).toContainText("1");

  await modal.getByRole("button", { name: /^Review/ }).click();
  await modal.getByRole("button", { name: "Apply" }).click();
  await expect(modal).toHaveCount(0);
  const c = saved[0];
  expect(c.layers).toEqual([{ ipv4: { dst: "10.0.0.9" } }, { udp: {} }, { payload: { size: 32 } }]);
  expect(c.modifiers).toEqual([{ field: "ipv4.src", mode: "increment" }]);
});

test("pcap import: edit a frame as layers, or replay", async ({ page }) => {
  const saved: Record<string, unknown>[] = [];
  const { modal } = await open(page, pk(), saved);
  const capture = { name: "x.pcap", mimeType: "application/octet-stream", buffer: Buffer.from([0xd4, 0xc3, 0xb2, 0xa1, 1, 2, 3]) };
  await modal.locator("input[type=file]").setInputFiles(capture);
  const frames = modal.locator(".pkt-frame");
  await expect(frames).toHaveCount(2);
  await frames.nth(1).click();
  const cards = modal.locator(".pkt-layer");
  await expect(cards).toHaveCount(3);
  await expect(cards.nth(2)).toContainText("UDP");

  await modal.locator("input[type=file]").setInputFiles(capture);
  await modal.getByRole("button", { name: "Replay all 2 frames" }).click();
  await expect(modal.getByRole("heading", { name: "Replaying a capture" })).toBeVisible();
  await expect(modal.locator(".pkt-summary")).toHaveText("captured frame 1");
  await modal.getByRole("button", { name: /^Review/ }).click();
  await modal.getByRole("button", { name: "Apply" }).click();
  await expect(modal).toHaveCount(0);
  expect((saved[0].pcap as { data: string }).data).toBe(Buffer.from([0xd4, 0xc3, 0xb2, 0xa1, 1, 2, 3]).toString("base64"));
});
