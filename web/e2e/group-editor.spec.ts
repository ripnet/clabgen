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
    const s = (r.request().postDataJSON() as { group: { spec: Spec } }).group.spec;
    const mep = s.protocols?.[0]?.config?.mep_id as { start: number } | number | undefined;
    const members = [1, 2, s.count].map((i) => ({
      index: i,
      device: { name: `${s.name}-${i}`, port: s.device.port, vlans: s.device.vlans },
      mac: `02:00:00:00:00:${String(i).padStart(2, "0")}`,
      protocols: (s.protocols ?? []).map((p) => ({
        name: `${s.name}-${i}-${p.name ?? p.type}`,
        type: p.type,
        config: { ...p.config, mep_id: typeof mep === "object" ? mep.start + i - 1 : mep },
      })),
    }));
    const plan = existing && { unchanged: Math.min(existing.count, s.count), changed: 0, added: Math.max(0, s.count - existing.count), removed: Math.max(0, existing.count - s.count) };
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
  await expect(rows).toHaveCount(3);
  await expect(rows.nth(0)).toContainText("rmep-1");
  await expect(rows.nth(0)).toContainText("mep_id 101");
  await expect(rows.nth(2)).toContainText("rmep-100");
  await expect(rows.nth(2)).toContainText("mep_id 200");
  await expect(rows.nth(2)).toContainText("300");

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
  await expect(m.getByRole("button", { name: "Apply" })).toBeDisabled();
  // The stepped value shows as start and step.
  await m.getByRole("button", { name: "cfm-mep" }).click();
  await expect(m.getByLabel("mep id start")).toHaveValue("101");
  await m.getByLabel("Member count").fill("12");
  await expect(m.locator(".group-plan")).toContainText("2 added, 10 unchanged");
  await m.getByRole("button", { name: "Apply" }).click();
  await expect(page.locator(".modal")).toHaveCount(0);
  expect(sent.update).toMatchObject({ name: "rmep", count: 12, protocols: [{ type: "cfm-mep", config: { mep_id: { start: 101 } } }] });
});
