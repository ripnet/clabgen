import { expect, test } from "@playwright/test";
import { mockAgent } from "./mock";

const svc = "clabgen.v1.AgentService";

// An enveloped Connect stream of messages, then end-of-stream.
function stream(msgs: unknown[]): Buffer {
  const parts = [...msgs.map((m) => [0, m] as const), [2, {}] as const].map(([flag, m]) => {
    const b = Buffer.from(JSON.stringify(m));
    const h = Buffer.alloc(5);
    h[0] = flag;
    h.writeUInt32BE(b.length, 1);
    return Buffer.concat([h, b]);
  });
  return Buffer.concat(parts);
}

const frame = (i: number, dir: string, summary: string) => ({
  time: `2026-10-24T10:00:0${i}.123456789Z`,
  port: "eth1",
  direction: dir,
  data: Buffer.from("ffffffffffff0200000000010800" + "45".padEnd(92, "0"), "hex").toString("base64"),
  length: 60,
  summary,
});

test("capture: live frames, decode, download, rebuild as a flow", async ({ page }) => {
  await mockAgent(page, { name: "pk", type: "packet", device: "d1", state: "running", config: {} }, () => undefined);
  await page.route(`**/agents/a1/${svc}/ListPorts`, (r) => r.fulfill({ json: { ports: [{ name: "eth1" }, { name: "eth2" }] } }));
  await page.route(`**/agents/a1/${svc}/ListDevices`, (r) => r.fulfill({ json: { devices: [{ name: "d1", port: "eth1", mac: "02:00:00:00:00:01", vlans: [] }] } }));
  let req: Record<string, unknown> | undefined;
  await page.route(`**/agents/a1/${svc}/Capture`, (r) => {
    const body = r.request().postDataBuffer()!;
    req = JSON.parse(body.subarray(5).toString());
    return r.fulfill({
      status: 200,
      contentType: "application/connect+json",
      body: stream([frame(1, "tx", "02:00:00:00:00:01 > ff:ff:ff:ff:ff:ff, IPv4 10.0.0.1 > 10.0.0.2, UDP 1 > 2, 60 bytes"), frame(2, "rx", "ARP 10.0.0.2 > 10.0.0.1, 60 bytes")]),
    });
  });
  await page.route(`**/agents/a1/${svc}/DecodeFrame`, (r) =>
    r.fulfill({
      json: {
        result: {
          hex: "ff".repeat(14),
          length: 14,
          summary: "decoded summary",
          layers: [{ layer: 0, title: "Ethernet", offset: 0, length: 14, fields: [{ name: "dst", bit_offset: 0, bits: 48, value: "ff:ff:ff:ff:ff:ff" }] }],
          decoded: [{ ethernet: { dst: "ff:ff:ff:ff:ff:ff", src: "02:00:00:00:00:01" } }, { vlan: { vid: 9 } }, { payload: { size: 46 } }],
        },
      },
    }),
  );
  await page.route(`**/agents/a1/${svc}/PreviewProtocol`, (r) => r.fulfill({ json: { result: { hex: "00", length: 1, summary: "preview", index: 0, layers: [] } } }));

  await page.goto("./#/a/a1/capture");
  await page.getByText("eth2").click(); // pick a port
  await page.getByLabel("direction").selectOption("tx");
  await expect(page.locator(".capture-ws code.ws-cmd")).toContainText("capture.pcapng?port=eth2&direction=tx");
  await expect(page.locator(".capture-ws").nth(1)).toContainText("clabgenctl wireshark install --ui");
  await page.getByRole("button", { name: /Start/ }).click();
  const rows = page.locator(".capture-row");
  await expect(rows).toHaveCount(2);
  expect(req).toMatchObject({ ports: ["eth2"], direction: "tx", summaries: true });
  await expect(rows.first()).toContainText("UDP 1 > 2");
  await expect(rows.nth(1)).toContainText("in");

  // The filter narrows the list.
  await page.getByLabel("Filter shown frames").fill("arp");
  await expect(rows).toHaveCount(1);
  await page.getByLabel("Filter shown frames").fill("");

  await rows.first().click();
  await expect(page.locator(".capture-detail")).toContainText("decoded summary");
  await expect(page.locator(".capture-detail .pkt-fields")).toContainText("ff:ff:ff:ff:ff:ff");

  const [dl] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download .pcapng" }).click()]);
  const buf = await (await dl.createReadStream()).toArray().then((c) => Buffer.concat(c as Buffer[]));
  expect(buf.readUInt32LE(0)).toBe(0x0a0d0d0a);
  expect(buf.includes(Buffer.from("eth1"))).toBeTruthy();

  // Rebuild: the device's own MAC and the captured tags are left to the device.
  await page.getByRole("button", { name: "Build a flow from it" }).click();
  const modal = page.locator(".modal");
  await expect(modal.getByRole("heading", { name: "New flow from a captured frame" })).toBeVisible();
  await expect(modal.locator(".pkt-layer")).toHaveCount(2);
  await expect(modal.locator(".pkt-layer").first()).toContainText("Ethernet");
});

for (const secure of [true, false]) {
  test(`capture: copy the Wireshark command (${secure ? "HTTPS or localhost" : "plain HTTP"})`, async ({ page, context }) => {
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    // A UI opened as http://host:8090 is not a secure context: no Clipboard API.
    if (!secure) await page.addInitScript(() => Object.defineProperty(window, "isSecureContext", { value: false }));
    await mockAgent(page, { name: "pk", type: "packet", device: "d1", state: "running", config: {} }, () => undefined);
    await page.route(`**/agents/a1/${svc}/ListPorts`, (r) => r.fulfill({ json: { ports: [{ name: "eth1" }] } }));
    await page.goto("./#/a/a1/capture");
    const cmd = await page.locator(".ws-cmd").innerText();
    expect(cmd).toContain("| wireshark -k -i -");
    await page.locator(".capture-ws").first().getByRole("button", { name: "copy" }).click();
    await expect(page.locator(".toast").last()).toContainText("Copied");
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(cmd);
  });
}

test("capture: an agent without capture says so", async ({ page }) => {
  await mockAgent(page, { name: "pk", type: "packet", device: "d1", state: "running", config: {} }, () => undefined, { agentVersion: "v0.1.0" });
  await page.route(`**/agents/a1/${svc}/DecodeFrame`, (r) => r.fulfill({ status: 404, json: { code: "unimplemented", message: "clabgen.v1.AgentService/DecodeFrame is not implemented" } }));
  await page.goto("./#/a/a1/capture");
  await expect(page.locator(".capture .warn-box")).toContainText("This agent (v0.1.0) has no packet capture");
  await expect(page.getByRole("button", { name: /Start/ })).toBeDisabled();
  await expect(page.locator(".ws-cmd")).toBeHidden();
});

test("capture: Open in Wireshark through Edgeshark", async ({ page }) => {
  await mockAgent(page, { name: "pk", type: "packet", device: "d1", state: "running", config: {} }, () => undefined);
  await page.route(`**/agents/a1/${svc}/ListPorts`, (r) => r.fulfill({ json: { ports: [{ name: "eth1" }, { name: "eth2" }] } }));
  await page.route(`**/agents/a1/${svc}/ListDevices`, (r) => r.fulfill({ json: { devices: [{ name: "d1", port: "eth1", mac: "02:00:00:00:00:01", vlans: [] }] } }));
  // Without Edgeshark (the mock's /api/info has none): no button.
  await page.goto("./#/a/a1/capture");
  await expect(page.getByText("● Start")).toBeVisible();
  await expect(page.getByRole("link", { name: "Open in Wireshark" })).toHaveCount(0);

  // A discovered container, and packetflix published on port 5001.
  await page.route("**/api/agents", (r) => r.fulfill({ json: { agents: [{ id: "clab-lab-a1", name: "a1", lab: "lab", source: "docker" }], allow_manual: false } }));
  await page.route("**/api/info", (r) => r.fulfill({ json: { version: "v1.0.0", update_available: false, edgeshark: { port: 5001 } } }));
  await page.route(`**/agents/clab-lab-a1/${svc}/**`, (r) => {
    const m = r.request().url().split("/").pop();
    if (m === "ListPorts") return r.fulfill({ json: { ports: [{ name: "eth1" }, { name: "eth2" }] } });
    if (m === "ListDevices") return r.fulfill({ json: { devices: [{ name: "d1", port: "eth1", mac: "02:00:00:00:00:01", vlans: [] }] } });
    return r.fulfill({ json: {} });
  });
  await page.goto("./#/a/clab-lab-a1/capture");
  await page.reload();
  const link = page.getByRole("link", { name: "Open in Wireshark" });
  const href = async () => decodeURIComponent((await link.getAttribute("href"))!);
  await expect(link).toBeVisible();
  // Every port, as the container on Edgeshark's host knows it.
  expect(await href()).toBe('packetflix:ws://127.0.0.1:5001/capture?container={"name":"clab-lab-a1","type":"docker"}&nif=eth1/eth2');
  // The ports picked.
  await page.getByRole("checkbox", { name: "eth2" }).check();
  await expect.poll(href).toBe('packetflix:ws://127.0.0.1:5001/capture?container={"name":"clab-lab-a1","type":"docker"}&nif=eth2');
  // A device: its port, and its MAC as the filter.
  await page.getByRole("combobox").first().selectOption("d1");
  await expect.poll(href).toBe('packetflix:ws://127.0.0.1:5001/capture?container={"name":"clab-lab-a1","type":"docker"}&nif=eth1&filter=ether host 02:00:00:00:00:01');
});
