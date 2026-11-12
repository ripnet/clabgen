import { expect, test } from "@playwright/test";
import { mockAgent, type Protocol } from "./mock";

const peA = "1c:b9:05:00:00:01";
const peB = "1c:b9:05:00:00:02";
const svc = (label: number, src: string, dst: string, data: unknown[], oam: unknown[]) => ({
  label,
  transport: [524287],
  flow_label: true,
  control_word: true,
  src,
  dst,
  frames: 40,
  last_s: 0.4,
  data,
  oam,
});
const ccm = (tags: string) => ({ kind: "CCM", level: 4, mep_id: 1, ma: "string(DOMAIN4)/string(E2101)", tags, src: "1c:b9:05:00:00:09", count: 19, last_s: 0.5 });

const monitor: Protocol = {
  name: "mon",
  type: "monitor",
  device: "tap",
  state: "running",
  config: { control_word: "auto" },
  status: {
    frames: 1568,
    mpls: 1538,
    undecoded: 0,
    links: [
      { src: peA, dst: peB, frames: 780, last_s: 0.1 },
      { src: peB, dst: peA, frames: 788, last_s: 0.1 },
    ],
    services: [
      svc(524286, peA, peB, [{ flow_id: "55a7d087", kind: "icmp", tags: "2101,101", src: "02:00:00:00:0a:01", dst: "02:00:00:00:0b:01", ip_src: "192.168.101.1", ip_dst: "192.168.101.2", count: 20, last_s: 0.3 }], [ccm("2101")]),
      svc(524286, peB, peA, [{ flow_id: "55a7d087", kind: "icmp", reply: true, tags: "2101,101", src: "02:00:00:00:0b:01", dst: "02:00:00:00:0a:01", ip_src: "192.168.101.2", ip_dst: "192.168.101.1", count: 20, last_s: 0.3 }], [ccm("2101")]),
      svc(524270, peA, peB, [], [ccm("2117,117")]),
      { src: "", dst: "", frames: 30, last_s: 1, other: [{ kind: "0x8870", tags: "untagged", count: 30, last_s: 1 }] },
    ],
  },
} as unknown as Protocol;

const ping = (wire: Record<string, unknown> | undefined): Protocol =>
  ({
    name: "ping-2101",
    type: "icmp",
    device: "s2101",
    state: "running",
    config: { destination: "192.168.101.2", rate: 1, mode: "continuous", timeout: "1s", ...(wire ? { wire } : {}) },
    status: {
      state: "running",
      flow_id: "55a7d087",
      source: "192.168.101.1",
      destination: "192.168.101.2",
      rate: 1,
      tracks_replies: true,
      tx: 20,
      rx: 20,
      lost: 0,
      loss_pct: 0,
      outage_max_ms: 0,
      outage_current_ms: 0,
      recent: [],
      timeline: [],
      ...(wire ? { wire } : {}),
    },
  }) as unknown as Protocol;

test("a flow checks its tags and its service's OAM tags on the monitored link, both ways", async ({ page }) => {
  await mockAgent(page, ping({ data: "2101,101", oam: "2101,101" }), () => undefined, { more: [monitor] });
  await page.goto("/#/a/a1/p/ping-2101");
  const head = page.locator(".wire-head");
  await expect(head).toContainText("fail");
  const rows = page.locator(".wire-table tbody tr");
  await expect(rows).toHaveCount(2);
  const there = rows.filter({ hasText: "a-to-z" });
  await expect(there).toContainText("a1/mon");
  await expect(there.locator(".wire-frame")).toContainText("524286");
  await expect(there.locator(".wire-frame")).toContainText("FAT");
  await expect(there.locator(".wire-seen.ok")).toContainText("2101101"); // data: both tags, as expected
  await expect(there.locator(".wire-seen.bad")).toContainText("2101"); // OAM: one tag only
  await expect(rows.filter({ hasText: "z-to-a" })).toContainText("back (replies)");

  // Expecting one tag on OAM: it passes.
  await mockAgent(page, ping({ data: "2101,101", oam: "2101" }), () => undefined, { more: [monitor] });
  await page.reload();
  await expect(page.locator(".wire-head")).toContainText("pass");
});

test("a flow without an expectation still shows where it was seen", async ({ page }) => {
  await mockAgent(page, ping(undefined), () => undefined, { more: [monitor] });
  await page.goto("/#/a/a1/p/ping-2101");
  await expect(page.locator(".wire-head")).toHaveText("Wire");
  await expect(page.locator(".wire-table tbody tr")).toHaveCount(2);
});

test("a monitor lists services with their flows, named, and OAM", async ({ page }) => {
  await mockAgent(page, monitor, () => undefined, { more: [ping({ data: "2101,101" })] });
  await page.goto("/#/a/a1/p/mon");
  await expect(page.locator(".proto-state")).toContainText("1,568 frames · 3 services · 1 flow");
  const table = page.locator(".wire-table");
  await expect(table.locator("tbody tr")).toHaveCount(3); // plain Ethernet hidden
  await expect(table).toContainText("label 524270");
  await expect(table).toContainText("a1/ping-2101");
  await expect(table).toContainText("string(DOMAIN4)/string(E2101)");
  await page.getByLabel("Filter services").fill("2117");
  await expect(table.locator("tbody tr")).toHaveCount(1);
  await page.getByLabel("Filter services").fill("");
  await page.getByText("plain Ethernet on the link").click();
  await expect(table.locator("tbody tr")).toHaveCount(4);
  await expect(table).toContainText("0x8870 ×30");
});

test("the flow lists show each flow's tags and its OAM's, both ways, red where they're wrong", async ({ page }) => {
  await mockAgent(page, ping({ data: "2101,101", oam: "2101,101" }), () => undefined, { more: [monitor] });
  await page.goto("/#/flows");
  const cell = page.locator(".wire-cell");
  await expect(cell).toHaveCount(1);
  const [data, oam] = [cell.locator(".wire-cell-line").nth(0), cell.locator(".wire-cell-line").nth(1)];
  // Alike both ways: shown once (⇄).
  await expect(data.locator(".wire-dir")).toHaveText("⇄");
  await expect(data.locator(".wire-seen.ok")).toHaveCount(1); // as expected
  await expect(oam.locator(".wire-seen.bad")).toHaveCount(1); // one tag, two expected
  await expect(oam.locator(".wire-seen.bad").first()).toHaveAttribute("title", "want 2101,101");
  // The wire failure is a failure: the Fail tile counts it.
  await expect(page.locator(".stat").filter({ hasText: "Fail" })).toContainText("1");

  // On the agent's Traffic tab, a service without OAM shows none, in warning.
  const noOAM = JSON.parse(JSON.stringify(monitor)) as Protocol;
  for (const s of (noOAM.status as { services: { oam?: unknown[] }[] }).services) s.oam = [];
  await mockAgent(page, ping({ data: "2101,101", oam: "2101" }), () => undefined, { more: [noOAM] });
  await page.goto("/#/a/a1/traffic");
  await expect(page.locator(".wire-cell .wire-none.warn")).toHaveCount(2); // each way: none isn't shown once
  await expect(page.locator(".wire-cell .wire-seen.ok")).toHaveCount(1);
});

test("the form calls the wire fields what they are: what's expected", async ({ page }) => {
  await mockAgent(page, ping({ data: "2101,101", oam: "2101" }), () => undefined, { more: [monitor] });
  await page.goto("/#/a/a1/p/ping-2101");
  await page.getByRole("button", { name: "Edit config" }).click();
  const nav = page.locator(".modal .section-nav");
  await expect(nav.getByRole("button", { name: /expected on the wire/ })).toBeVisible();
  await expect(nav.getByRole("button", { name: /^wire/ })).toHaveCount(0);
});

test("OAM a reused label carried before the flow isn't the flow's; an old stack is shown, not judged", async ({ page }) => {
  const flow = (tags: string, first_s: number, last_s: number) => ({ flow_id: "55a7d087", kind: "icmp", tags, src: "02:00:00:00:0a:01", ip_src: "192.168.101.1", count: 20, first_s, last_s });
  const old = { kind: "CCM", level: 4, mep_id: 1, ma: "string(DOMAIN4)/string(E2050)", tags: "2050", count: 400, first_s: 900, last_s: 300 };
  const reused = {
    ...monitor,
    status: {
      ...(monitor.status as Record<string, unknown>),
      // Before this flow, the label carried another service (E2050), and the
      // flow itself first went with one tag too many.
      services: [svc(524286, peA, peB, [flow("2101,2101,101", 120, 60), flow("2101,101", 50, 0.3)], [old, { ...ccm("2101"), first_s: 119 }])],
    },
  } as unknown as Protocol;
  await mockAgent(page, ping({ data: "2101,101", oam: "2101", direction: "a-to-z" }), () => undefined, { more: [reused] });
  await page.goto("/#/a/a1/p/ping-2101");
  await expect(page.locator(".wire-head")).toContainText("pass");
  const row = page.locator(".wire-table tbody tr").filter({ hasText: "a-to-z" });
  await expect(row.locator(".wire-seen.stale")).toContainText("old");
  await expect(row.locator(".wire-seen.ok")).toHaveCount(2); // data now, and E2101's CCMs
  await expect(row).not.toContainText("2050");
});

// aged: the monitor as seen `secs` later with nothing new (the mirror
// stopped); with oamToo false, only the flow's frames stopped.
const aged = (secs: number, oamToo = true): Protocol => {
  const st = monitor.status as { services: { data?: { last_s: number }[]; oam?: { last_s: number }[] }[] };
  const older = <T extends { last_s: number }>(l?: T[]) => l?.map((x) => ({ ...x, last_s: x.last_s + secs }));
  return { ...monitor, status: { ...st, services: st.services.map((v) => ({ ...v, data: older(v.data), oam: oamToo ? older(v.oam) : v.oam })) } } as unknown as Protocol;
};

test("verdicts are on recent traffic: tags not seen for a while are shown greyed, pending, not judged", async ({ page }) => {
  // OAM expected with one tag: with the mirror on, the two-tag OAM fails.
  await mockAgent(page, ping({ data: "2101,101", oam: "2101" }), () => undefined, { more: [aged(40)] });
  await page.goto("/#/a/a1/p/ping-2101");
  await expect(page.locator(".wire-head")).toContainText("pending");
  const rows = page.locator(".wire-table tbody tr");
  await expect(rows).toHaveCount(2);
  await expect(page.locator(".wire-table tr.wire-idle")).toHaveCount(2);
  const there = rows.filter({ hasText: "a-to-z" });
  await expect(there).toContainText("not seen for 40 s");
  await expect(there.locator(".wire-seen.idle")).toHaveCount(2); // last data and OAM, shown
  await expect(there.locator(".wire-seen.bad")).toHaveCount(0); // and not judged
  await expect(there.locator(".verdict")).toContainText("pending");

  // Only the flow stopped: its data is pending, its service's OAM still judged.
  await mockAgent(page, ping({ data: "2101,101", oam: "2101" }), () => undefined, { more: [aged(25, false)] });
  await page.reload();
  const row = page.locator(".wire-table tbody tr").filter({ hasText: "a-to-z" });
  await expect(row).not.toHaveClass(/wire-idle/);
  await expect(row.locator(".wire-seen.idle")).toHaveCount(1);
  await expect(row).toContainText("not seen for 25 s");
  await expect(row.locator(".wire-seen.ok")).toHaveCount(1); // OAM: one tag, as expected
  await expect(page.locator(".wire-head")).toContainText("pending");

  // The flow lists: greyed, not red, and not counted as a failure.
  await mockAgent(page, ping({ data: "2101,101", oam: "2101,101" }), () => undefined, { more: [aged(40)] });
  await page.goto("/#/flows");
  const cell = page.locator(".wire-cell");
  await expect(cell.locator(".wire-seen.idle")).toHaveCount(2); // data and OAM, alike both ways
  await expect(cell.locator(".wire-seen.bad")).toHaveCount(0);
  await expect(cell.locator(".wire-seen.idle").first()).toHaveAttribute("title", /not seen for 40 s: pending/);
  await expect(page.locator(".stat").filter({ hasText: "Fail" })).toContainText("0");
});

test("Clear monitors clears every monitor", async ({ page }) => {
  await mockAgent(page, ping({ data: "2101,101" }), () => undefined, { more: [monitor] });
  const cleared: string[] = [];
  await page.route("**/agents/a1/*/RunProtocolAction", (r) => {
    const req = r.request().postDataJSON() as { name: string; action: string };
    cleared.push(`${req.name} ${req.action}`);
    return r.fulfill({ status: 200, contentType: "application/json", body: "{}" });
  });
  await page.goto("/#/a/a1/p/ping-2101");
  await page.locator(".wire-head-row").getByRole("button", { name: /Clear monitors/ }).click();
  await expect.poll(() => cleared).toEqual(["mon clear"]);
});
