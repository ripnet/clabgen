import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const svc = "clabgen.v1.AgentService";

const flow = (name: string, flowId: string, kind: "echo" | "one-way"): Protocol =>
  ({
    name,
    type: kind === "echo" ? "icmp" : "udp",
    device: "d1",
    state: "running",
    config: { destination: "10.0.0.2" },
    status: {
      state: "running",
      flow_id: flowId,
      destination: "10.0.0.2",
      source: "10.0.0.1",
      rate: 1000,
      tracks_replies: kind === "echo",
      tx: 1000,
      rx: 1000,
      lost: 0,
      loss_pct: 0,
      outage_max_ms: 0,
      outage_current_ms: 0,
      recent: [],
      timeline: [],
    },
  }) as unknown as Protocol;

const row = (over: Record<string, unknown>) => ({
  side: "sender",
  device: "d1",
  one_way: false,
  running: true,
  status: "no_impact",
  loss_ms: 0,
  lost: 0,
  clean_ms: 60000,
  resolution_ms: 1,
  outages: [],
  ...over,
});

const reply = (flows: unknown[], events: unknown[] = []) => ({
  contentType: "application/json",
  body: JSON.stringify({ now: "2026-11-03T12:00:00Z", flows, events }),
});

const clean = [
  row({ name: "echo1", flow_id: "aaaa0001" }),
  row({ name: "oneway1", flow_id: "aaaa0002", one_way: true }),
  row({ side: "receiver", device: "far", flow_id: "aaaa0002", one_way: true }),
  row({ side: "receiver", device: "far", flow_id: "aaaa0001", one_way: true, lost: 9 }), // the echo's outbound leg: not what's reported
];

const failed = [
  row({ name: "echo1", flow_id: "aaaa0001", status: "restored", loss_ms: 212, lost: 212, first_loss_ms: 10, last_loss_ms: 222, outages: [{ start_ms: 10, end_ms: 222, lost: 212 }] }),
  row({ name: "oneway1", flow_id: "aaaa0002", one_way: true }),
  row({ side: "receiver", device: "far", flow_id: "aaaa0002", one_way: true, status: "restored", loss_ms: 100, lost: 100, first_loss_ms: 12, last_loss_ms: 412, outages: [{ start_ms: 12, end_ms: 412, lost: 100 }] }),
];

async function open(page: import("@playwright/test").Page) {
  await page.goto("./#/");
  await page.locator(".sidebar").getByRole("link", { name: "Flows" }).click();
  await page.getByRole("button", { name: /Convergence test/ }).click();
}

test("a failure and its repair, measured", async ({ page }) => {
  await mockAgent(page, flow("echo1", "aaaa0001", "echo"), () => undefined, { more: [flow("oneway1", "aaaa0002", "one-way")] });
  await page.route(`**/agents/a1/${svc}/ListPorts`, (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify({ ports: [{ name: "eth1", adminUp: true }, { name: "eth2", adminUp: true }] }) }),
  );
  const steps: { name: string; up?: boolean }[] = [];
  await page.route(`**/agents/a1/${svc}/SetPortAdmin`, (r) => {
    steps.push(r.request().postDataJSON());
    return r.fulfill({ contentType: "application/json", body: "{}" });
  });
  const asked: { since?: string; flowIds?: string[] }[] = [];
  await page.route(`**/agents/a1/${svc}/GetConvergence`, (r) => {
    const q = r.request().postDataJSON();
    asked.push(q);
    if (!q.since) return r.fulfill(reply(clean.filter((f) => f.flow_id !== "aaaa0001" || f.side === "sender")));
    return steps.length === 1
      ? r.fulfill(reply(failed, [{ time: "2026-11-03T12:00:00.031Z", source: "protocol/isis1", kind: "bfd-down", message: "BFD session down" }]))
      : r.fulfill(reply(clean.filter((f) => f.side === "sender" || f.flow_id === "aaaa0002")));
  });

  await open(page);
  await expect(page.locator(".conv")).toContainText("2 flows listed");
  await page.getByLabel("Failure kind").selectOption("port");
  await page.getByLabel("Failure port", { exact: true }).selectOption("eth2");
  await page.getByLabel("Repair kind").selectOption("port");
  await page.getByLabel("Repair port", { exact: true }).selectOption("eth2");
  await expect(page.getByLabel("Repair port state")).toHaveValue("up");
  await page.getByRole("button", { name: "Run test" }).click();

  // Both steps ran, in order, on the port picked.
  const fail = page.locator(".conv-phase").first();
  await expect(fail.locator("h3")).toContainText("a1: port eth2 down");
  await expect(page.locator(".conv-phase")).toHaveCount(2);
  expect(steps).toEqual([{ name: "eth2" }, { name: "eth2", up: true }]);
  expect(asked.every((q) => q.flowIds?.length === 2)).toBe(true);

  // An echo flow's result is its sender's, a one-way flow's its receiver's.
  const rows = fail.locator("tbody tr");
  await expect(rows.filter({ hasText: "echo1" })).toContainText("212 ms");
  await expect(rows.filter({ hasText: "oneway1" })).toContainText("100 ms");
  await expect(rows.filter({ hasText: "oneway1" })).toContainText("a1 · far");
  await expect(rows.filter({ hasText: "oneway1" })).toContainText("+412 ms");
  await expect(fail.locator(".stat", { hasText: "Worst loss" }).locator(".stat-n")).toHaveText("212 ms");
  await expect(fail.locator(".stat", { hasText: "Flows lost traffic" }).locator(".stat-n")).toHaveText("2/2");
  await expect(fail.locator(".conv-lost")).toHaveCount(2);
  await expect(fail.locator(".conv-event")).toHaveCount(1);
  await expect(fail.locator("details")).toContainText("BFD session down");

  const repair = page.locator(".conv-phase").nth(1);
  await expect(repair.locator(".stat", { hasText: "Flows lost traffic" }).locator(".stat-n")).toHaveText("0/2");
  await expect(repair.locator("tbody tr").first()).toContainText("no impact");

  const [csv] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Export CSV" }).click()]);
  expect(csv.suggestedFilename()).toMatch(/^convergence-.*\.csv$/);
});

test("flows not clean to start with: says why, runs only if asked", async ({ page }) => {
  await mockAgent(page, flow("echo1", "aaaa0001", "echo"), () => undefined);
  let measured = 0;
  await page.route(`**/agents/a1/${svc}/GetConvergence`, (r) => {
    const q = r.request().postDataJSON();
    if (q.since) measured++;
    return r.fulfill(reply([row({ name: "echo1", flow_id: "aaaa0001", status: "restored", loss_ms: 7, lost: 7, outages: [] })]));
  });
  await open(page);
  await page.getByRole("button", { name: "Run test" }).click();
  await expect(page.locator(".conv-problems")).toContainText("echo1 lost 7 packets in the last 5 s");
  expect(measured).toBe(0);
  await page.getByRole("button", { name: "Run anyway" }).click();
  await expect(page.locator(".conv-phase")).toHaveCount(1);
  expect(measured).toBeGreaterThan(0);
});

test("no flows running: refused", async ({ page }) => {
  await mockAgent(page, flow("echo1", "aaaa0001", "echo"), () => undefined);
  await page.route(`**/agents/a1/${svc}/GetConvergence`, (r) => r.fulfill(reply([row({ name: "echo1", flow_id: "aaaa0001", status: "stopped", running: false })])));
  await open(page);
  await page.getByRole("button", { name: "Run test" }).click();
  await expect(page.locator(".conv")).toContainText("No flows running");
});

test("a flow to many prefixes: which ones lost traffic", async ({ page }) => {
  await mockAgent(page, flow("many", "aaaa0003", "echo"), () => undefined);
  const prefixes = {
    total: 100,
    impacted: 2,
    probe_ms: 500,
    measured: true,
    list: [
      { prefix: "198.51.100.0/24", lost: 4, loss_ms: 2000, first_loss_ms: 40, last_loss_ms: 2100 },
      { prefix: "198.51.101.0/24", lost: 3, loss_ms: 1500, first_loss_ms: 50, last_loss_ms: 1600 },
    ],
  };
  await page.route(`**/agents/a1/${svc}/GetConvergence`, (r) => {
    const q = r.request().postDataJSON();
    if (!q.since) return r.fulfill(reply([row({ name: "many", flow_id: "aaaa0003", prefixes: { ...prefixes, impacted: 0, list: [] } })]));
    return r.fulfill(reply([row({ name: "many", flow_id: "aaaa0003", status: "restored", loss_ms: 35, lost: 7, first_loss_ms: 40, last_loss_ms: 300, outages: [{ start_ms: 40, end_ms: 300, lost: 7 }], prefixes })]));
  });
  await open(page);
  await page.getByRole("button", { name: "Run test" }).click();
  const phase = page.locator(".conv-phase").first();
  await expect(phase.locator("tbody tr").first()).toContainText("2/100");
  await phase.getByText("many: 2 of 100 prefixes lost traffic").click();
  const list = phase.locator("details").first();
  await expect(list).toContainText("198.51.100.0/24");
  await expect(list).toContainText("2.00 s");
  // 500 ms between probes, against 260 ms of loss: some may have been missed.
  await expect(list.locator(".warn-box")).toContainText("probed every 500 ms");
});

test("flow destinations from BGP routes", async ({ page }) => {
  const bgp = {
    name: "bgp1",
    type: "bgp",
    device: "d1",
    state: "running",
    config: {
      peers: [{ address: "10.0.0.2", remote_as: 65002 }],
      routes: [{ prefix: "198.51.100.0/24", count: 50 }, { prefix: "203.0.113.0/24", next_hop: "10.0.0.9" }, { prefix: "192.0.2.0/24", rd: "65000:1" }],
    },
  } as unknown as Protocol;
  let updated: Record<string, unknown> | undefined;
  await mockAgent(page, flow("f1", "aaaa0004", "echo"), (c) => { updated = c; return undefined; }, { more: [bgp] });
  await page.route(`**/agents/a1/${svc}/RunProtocolAction`, (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify({ result: { routes: [
      { prefix: "100.64.0.0/24", family: "ipv4-unicast" }, { prefix: "100.64.1.0/24", family: "ipv4-unicast" }, { prefix: "10.9.0.0/16", family: "ipv4-vpn" },
    ] } }) }),
  );
  await page.goto("./#/a/a1/p/f1");
  await page.getByRole("button", { name: "Edit config" }).click();
  await page.getByRole("button", { name: "From BGP routes" }).click();
  const body = page.locator(".route-picker-body");
  // A VPN route isn't offered; one with its next hop elsewhere says so.
  await expect(body.locator(".route-picker-row")).toHaveCount(2);
  await expect(body).toContainText("198.51.100.0/24 ×50");
  await expect(body).toContainText("next hop 10.0.0.9: not answered by clabgen");

  // Received routes: consecutive ones become a block, other families are left out.
  await body.getByRole("tab", { name: "received" }).click();
  await body.getByLabel("BGP speaker").selectOption({ index: 1 });
  await body.getByLabel("BGP peer").selectOption("10.0.0.2");
  await body.getByRole("button", { name: "Load routes" }).click();
  await expect(body.locator(".route-picker-row")).toHaveCount(1);
  await expect(body).toContainText("100.64.0.0/24 ×2");

  await body.getByRole("tab", { name: "advertised" }).click();
  await body.locator(".route-picker-row").filter({ hasText: "198.51.100.0/24" }).getByRole("checkbox").check();
  await body.getByRole("button", { name: "Use 50 prefixes" }).click();
  await expect(body).toHaveCount(0);
  await page.getByRole("button", { name: /^Review \d/ }).click();
  await page.getByRole("button", { name: "Apply", exact: true }).click();
  await expect.poll(() => updated?.destinations).toEqual([{ prefix: "198.51.100.0/24", count: 50 }]);
  expect(updated?.destination).toBeUndefined();
});
