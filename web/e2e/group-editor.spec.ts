import { expect, test, type Page, type Route } from "@playwright/test";
import { mockAgent } from "./mock";

const svc = "clabgen.v1.AgentService";
const ok = (r: Route, body: unknown) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

type Spec = { name: string; count: number; device: Record<string, unknown>; protocols?: { name?: string; type: string; config?: Record<string, unknown> }[] };

// group mocks the group calls: PreviewDeviceGroup renders members 1, 2 and
// the last from the spec's count and a stepped mep_id (as the agent would),
// with a plan against `existing`; create/update record the spec sent.
async function group(page: Page, existing?: Spec) {
  const sent: { create?: Spec; update?: Spec } = {};
  await mockAgent(page, { name: "x", type: "lldp", device: "r1", state: "running", config: {} }, () => undefined);
  await page.route(`**/agents/a1/${svc}/ListPorts`, (r) => ok(r, { ports: [{ name: "eth1" }, { name: "eth2" }] }));
  await page.route(`**/agents/a1/${svc}/PreviewDeviceGroup`, (r) => {
    const req = r.request().postDataJSON() as { group: { spec: Spec }; members?: number[] };
    const s = req.group.spec;
    const mep = s.protocols?.[0]?.config?.mep_id as { start: number } | number | undefined;
    const members = (req.members?.length ? req.members : [1, 2, s.count]).map((i) => ({
      index: i,
      device: { name: `${s.name}-${i}`, port: s.device.port, vlans: s.device.vlans },
      mac: `02:00:00:00:00:${String(i).padStart(2, "0")}`,
      protocols: (s.protocols ?? []).map((p) => ({
        name: `${s.name}-${i}-${p.name ?? p.type}`,
        type: p.type,
        config: { ...p.config, mep_id: typeof mep === "object" ? mep.start + i - 1 : mep },
      })),
    }));
    const kept = existing ? Math.min(existing.count, s.count) : 0;
    const same = existing && JSON.stringify(s.protocols) === JSON.stringify(existing.protocols);
    const plan = existing && { unchanged: same ? kept : 0, changed: same ? 0 : kept, added: Math.max(0, s.count - existing.count), removed: Math.max(0, existing.count - s.count) };
    return ok(r, { result: { count: s.count, members, ...(plan ? { plan } : {}) } });
  });
  await page.route(`**/agents/a1/${svc}/CreateDeviceGroup`, (r) => {
    const s = (r.request().postDataJSON() as { group: { spec: Spec } }).group.spec;
    if (s.count > 4096) return r.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ code: "invalid_argument", message: `group ${s.name}: count must be 1-4096` }) });
    sent.create = s;
    return ok(r, { group: { name: s.name, spec: s } });
  });
  if (existing) {
    const msg = { name: existing.name, spec: existing, status: { devices: existing.count, protocols: [] }, yaml: "" };
    await page.route(`**/agents/a1/${svc}/ListDeviceGroups`, (r) => ok(r, { groups: [msg] }));
    await page.route(`**/agents/a1/${svc}/GetDeviceGroup`, (r) => ok(r, { group: msg }));
    await page.route(`**/agents/a1/${svc}/UpdateDeviceGroup`, (r) => {
      sent.update = (r.request().postDataJSON() as { group: { spec: Spec } }).group.spec;
      return ok(r, { group: msg });
    });
  }
  return sent;
}

test("build a group of 100 MEPs as a form", async ({ page }) => {
  const sent = await group(page);
  await page.goto("./#/a/a1/groups");
  await page.getByRole("button", { name: "New group" }).click();
  const m = page.locator(".modal");
  await expect(m.locator(".group-preview")).toContainText("Name the group");
  await m.getByLabel("Group name").fill("rmep");
  await m.getByLabel("Member count").fill("100");
  await m.getByRole("button", { name: "Add VLAN tag" }).click();
  await m.getByLabel("VLAN 1", { exact: true }).fill("300");
  await m.getByLabel("Add a protocol").selectOption("cfm-mep");
  await m.getByRole("button", { name: "mep id: step per member" }).click();
  await m.getByLabel("mep id start").fill("101");

  const rows = m.locator(".group-preview tbody tr");
  await expect(rows).toHaveCount(4); // 1, 2, a row for 3-99, 100
  await expect(rows.nth(0)).toContainText("rmep-1");
  await expect(rows.nth(0)).toContainText("mep_id 101");
  await expect(rows.nth(3)).toContainText("rmep-100");
  await expect(rows.nth(3)).toContainText("mep_id 200");
  await expect(rows.nth(3)).toContainText("300");

  // The YAML view shows the same spec, without the protocol's defaults.
  await m.getByRole("button", { name: "edit as YAML" }).first().click();
  const yaml = m.getByLabel("Group as YAML");
  await expect(yaml).toHaveValue(/mep_id:\n\s+start: 101/);
  await expect(yaml).not.toHaveValue(/ais:/);
  await m.getByRole("button", { name: "form view" }).first().click();

  // A rejection stays in the editor.
  await m.getByLabel("Member count").fill("5000");
  await m.getByRole("button", { name: "Create" }).click();
  await expect(m.getByRole("alert")).toContainText("count must be 1-4096");
  await m.getByLabel("Member count").fill("100");
  await m.getByRole("button", { name: "Create" }).click();
  await expect(page.locator(".modal")).toHaveCount(0);
  expect(sent.create).toMatchObject({ name: "rmep", count: 100, device: { port: "eth1", vlans: [{ vid: 300 }] } });
  const cfg = sent.create!.protocols![0];
  expect(cfg.type).toBe("cfm-mep");
  expect(cfg.config!.mep_id).toEqual({ start: 101 });
  expect(cfg.config!.ais).toBeUndefined();
});

test("edit a group: what applying changes", async ({ page }) => {
  const existing: Spec = { name: "rmep", count: 10, device: { port: "eth1", vlans: [{ vid: 300 }] }, protocols: [{ type: "cfm-mep", config: { mep_id: { start: 101 } } }] };
  const sent = await group(page, existing);
  await page.goto("./#/a/a1/g/rmep");
  await page.getByRole("button", { name: "Edit" }).first().click();
  const m = page.locator(".modal");
  await expect(m.locator(".group-plan")).toContainText("nothing changes");
  await expect(m.getByRole("button", { name: "No changes" })).toBeDisabled();
  // The stepped value shows as start and step.
  await m.getByRole("button", { name: "cfm-mep" }).click();
  await expect(m.getByLabel("mep id start")).toHaveValue("101");
  await m.getByLabel("Member count").fill("12");
  await expect(m.locator(".group-plan")).toContainText("2 added, 10 unchanged");
  await m.getByRole("button", { name: "Review 1 change" }).click();
  const rows = m.locator(".diff-table tbody tr");
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("count");
  await expect(rows.first()).toContainText("10");
  await expect(rows.first()).toContainText("12");
  // Back keeps the edit; Apply sends it.
  await m.getByRole("button", { name: "Back" }).click();
  await expect(m.getByLabel("Member count")).toHaveValue("12");
  await m.getByRole("button", { name: "Review 1 change" }).click();
  await m.getByRole("button", { name: "Apply" }).click();
  await expect(page.locator(".modal")).toHaveCount(0);
  expect(sent.update).toMatchObject({ name: "rmep", count: 12, protocols: [{ type: "cfm-mep", config: { mep_id: { start: 101 } } }] });
});

test("a setting left at its default shows its default", async ({ page }) => {
  // The spec leaves ccm_enabled out: it is on (the default).
  const existing: Spec = { name: "rmep", count: 10, device: { port: "eth1" }, protocols: [{ name: "cfm-mep", type: "cfm-mep", config: { level: 3, mep_id: { start: 1 } } }] };
  const sent = await group(page, existing);
  await page.goto("./#/a/a1/g/rmep");
  await page.getByRole("button", { name: "Edit" }).first().click();
  const m = page.locator(".modal");
  await m.locator(".section-nav").getByRole("button", { name: "cfm-mep" }).click();
  const ccm = m.locator(".field", { hasText: "ccm enabled" }).getByRole("checkbox");
  await expect(ccm).toBeChecked();
  await expect(m.getByRole("button", { name: "No changes" })).toBeDisabled();
  await ccm.uncheck();
  await expect(m.locator(".group-plan")).toContainText("10 changed");
  await m.getByRole("button", { name: "Review 1 change" }).click();
  await expect(m.locator(".diff-table")).toContainText("protocols[0].config.ccm_enabled");
  await m.getByRole("button", { name: "Apply" }).click();
  await expect(page.locator(".modal")).toHaveCount(0);
  expect(sent.update?.protocols?.[0].config).toMatchObject({ ccm_enabled: false, level: 3 });
});

test("preview any member; skipped members show as a row", async ({ page }) => {
  await group(page);
  await page.goto("./#/a/a1/groups");
  await page.getByRole("button", { name: "New group" }).click();
  const m = page.locator(".modal");
  await m.getByLabel("Group name").fill("rmep");
  await m.getByLabel("Member count").fill("100");
  await m.getByLabel("Add a protocol").selectOption("cfm-mep");
  await m.getByRole("button", { name: "mep id: step per member" }).click();
  await m.getByLabel("mep id start").fill("101");
  const rows = m.locator(".group-preview tbody tr");
  await expect(rows).toHaveCount(4);
  await expect(rows.nth(2)).toHaveClass(/gap-row/);
  await expect(rows.nth(2)).toContainText("97 more (3–99)");

  await m.getByLabel("Member to show").fill("57");
  await m.getByRole("button", { name: "Show", exact: true }).click();
  await expect(rows).toHaveCount(6);
  await expect(rows.nth(2)).toContainText("54 more (3–56)");
  await expect(rows.nth(3)).toContainText("rmep-57");
  await expect(rows.nth(3)).toContainText("mep_id 157");
  await expect(rows.nth(4)).toContainText("42 more (58–99)");
  // Out of range: nothing to show.
  await m.getByLabel("Member to show").fill("101");
  await expect(m.getByRole("button", { name: "Show", exact: true })).toBeDisabled();

  await m.getByRole("button", { name: "Stop showing member 57" }).click();
  await expect(rows).toHaveCount(4);
});

test("make a group from a device and its protocols", async ({ page }) => {
  const sent = await group(page);
  await page.route(`**/agents/a1/${svc}/ListDevices`, (r) =>
    ok(r, { devices: [{ name: "r1", port: "eth2", mac: "02:00:00:00:00:99", macDerived: false, vlans: [{ vid: 300 }], addresses: ["10.1.0.2/30"], gateway4: "10.1.0.1" }] }),
  );
  await page.goto("./#/a/a1/devices");
  await page.getByRole("button", { name: "Make group…" }).click();
  const m = page.locator(".modal");
  await expect(m.locator(".info-box")).toContainText("Started from r1");
  await expect(m.locator(".info-box")).toContainText("fixed MAC is left out");
  await expect(m.getByLabel("port")).toHaveValue("eth2");
  await expect(m.getByLabel("address 1", { exact: true })).toHaveValue("10.1.0.2/30");
  await expect(m.locator(".section-nav")).toContainText("x");
  // Step the address and gateway, then create.
  await m.getByLabel("Group name").fill("ce");
  await m.getByRole("button", { name: "address 1: step per member" }).click();
  await m.getByLabel("address 1 step").fill("0.0.0.4");
  await m.getByRole("button", { name: "Create" }).click();
  await expect(page.locator(".modal")).toHaveCount(0);
  expect(sent.create).toMatchObject({
    name: "ce",
    count: 10,
    device: { port: "eth2", vlans: [{ vid: 300 }], addresses: [{ start: "10.1.0.2/30", step: "0.0.0.4" }], gateway4: "10.1.0.1" },
    protocols: [{ name: "x", type: "lldp" }],
  });
  expect(sent.create!.device.mac).toBeUndefined();
});
