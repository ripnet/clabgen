import { expect, test } from "@playwright/test";
import { fillYaml, mockAgent, yamlText } from "./mock";

const svc = "clabgen.v1.AgentService";
const p = { name: "pk", type: "packet", device: "d1", state: "running", config: {} };

test("scenario: where the config is saved, and reset", async ({ page }) => {
  await mockAgent(page, p, () => undefined);
  let source = "saved";
  let resets = 0;
  const config = () => ({ file: "/home/u/lab/clab-b2b/gen1/clabgen.yml", source, scenario: "/etc/clabgen/scenario.yml" });
  await page.route(`**/agents/a1/${svc}/GetInfo`, (r) => r.fulfill({ json: { hostname: "gen1", version: "v0.4.0", persistence: config() } }));
  await page.route(`**/agents/a1/${svc}/GetScenario`, (r) => r.fulfill({ json: { yaml: "devices: []\n" } }));
  await page.route(`**/agents/a1/${svc}/ResetConfig`, (r) => {
    resets++;
    source = "scenario";
    return r.fulfill({ json: { persistence: config() } });
  });
  await page.goto("./#/a/a1/scenario");
  const panel = page.locator(".saved-config");
  await expect(panel).toContainText("Saved to /home/u/lab/clab-b2b/gen1/clabgen.yml");
  await expect(panel).toContainText("this start: restored from it");
  await expect(panel).toContainText("Edits to the scenario file apply after a reset");

  page.once("dialog", (d) => d.dismiss());
  await panel.getByRole("button", { name: "Reset to the topology's scenario" }).click();
  expect(resets).toBe(0); // cancelled
  page.once("dialog", (d) => d.accept());
  await panel.getByRole("button", { name: "Reset to the topology's scenario" }).click();
  await expect(panel).toContainText("started from the topology's scenario (/etc/clabgen/scenario.yml)");
  expect(resets).toBe(1);
});

test("scenario: an agent that doesn't save says why", async ({ page }) => {
  await mockAgent(page, p, () => undefined);
  await page.route(`**/agents/a1/${svc}/GetInfo`, (r) =>
    r.fulfill({ json: { hostname: "gen1", version: "v0.4.0", persistence: { notSaved: "the topology's folder isn't mounted at /clab (add binds: [.:/clab] to the node)", source: "scenario" } } }),
  );
  await page.goto("./#/a/a1/scenario");
  await expect(page.locator(".saved-config")).toContainText("Not saved across lab restarts: the topology's folder isn't mounted at /clab");
});

const doc = `devices:
- name: gen
  port: eth1
  addresses: [10.0.0.2/24]
protocols:
- name: ping
  type: icmp
  device: gen
  config:
    destination: 10.0.0.1
- name: refl
  type: twamp-reflector
  device: gen
  config:
    mode: stamp
`;

test("scenario editor: colours, suggestions from the schema, problems, folding, Ctrl-S", async ({ page }) => {
  await mockAgent(page, p, () => undefined);
  const applied: { yaml: string; replace?: boolean }[] = [];
  await page.route(`**/agents/a1/${svc}/GetScenario`, (r) => r.fulfill({ json: { yaml: doc } }));
  await page.route(`**/agents/a1/${svc}/ApplyScenario`, (r) => {
    applied.push(r.request().postDataJSON());
    return r.fulfill({ json: {} });
  });
  await page.goto("./#/a/a1/scenario");
  const ed = page.getByLabel("Scenario YAML");
  await expect.poll(() => yamlText(ed)).toBe(doc.trimEnd() + "\n");
  await expect(page.locator(".cm-yaml-key").first()).toHaveText("devices");

  // A new field in the reflector's config: its type's fields, by name.
  const line = page.locator(".cm-line", { hasText: "mode: stamp" });
  await line.click();
  await page.keyboard.press("End");
  await page.keyboard.press("Enter");
  await page.keyboard.type("dro");
  const pop = page.locator(".cm-tooltip-autocomplete");
  await expect(pop.locator("li[aria-selected]")).toContainText("drop_pct");
  // A list just opened ignores Enter for 75 ms (no accidental accepts).
  await page.waitForTimeout(150);
  await page.keyboard.press("Enter");
  await page.keyboard.type("500");
  // Out of range: marked, with the reason on hover.
  await expect(page.locator(".cm-lintRange-error")).toHaveCount(1, { timeout: 5000 });
  // Hover tooltips open after a pause over the text; retry under load.
  await expect(async () => {
    await page.mouse.move(0, 0);
    await page.locator(".cm-lintRange-error").hover();
    await expect(page.locator(".cm-tooltip-lint")).toContainText("should be at most 100", { timeout: 1500 });
  }).toPass();
  await expect(page.locator(".hint", { hasText: "Edited, not applied yet." })).toBeVisible();

  // Values: the enum, after "key: ".
  await fillYaml(ed, doc.replace("mode: stamp", "mode: "));
  await page.locator(".cm-line", { hasText: /^\s+mode:/ }).click();
  await page.keyboard.press("End");
  await page.keyboard.press("Control+Space");
  await expect(pop).toContainText("twamp-light");

  // Folding every entry: one line each, saying what it is.
  await fillYaml(ed, doc);
  await page.getByRole("button", { name: "fold every entry" }).click();
  await expect(page.locator(".cm-foldPlaceholder")).toHaveText(["… on eth1 · 2 lines", "… icmp on gen · 4 lines", "… twamp-reflector on gen · 4 lines"]);
  await page.getByRole("button", { name: "unfold all" }).click();
  await expect(page.locator(".cm-foldPlaceholder")).toHaveCount(0);

  // Ctrl-S applies (merge); the edits survive leaving the tab.
  await fillYaml(ed, doc + "- {name: r2, type: twamp-reflector, device: gen}\n");
  await page.goto("./#/a/a1/protocols");
  await page.goto("./#/a/a1/scenario");
  await expect.poll(() => yamlText(ed)).toContain("name: r2");
  await ed.click();
  await page.keyboard.press("ControlOrMeta+s");
  await expect.poll(() => applied.length).toBe(1);
  expect(applied[0].yaml).toContain("name: r2");
  expect(applied[0].replace).toBeFalsy();
});

test("scenario editor: popups follow the dark theme", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "dark" });
  await mockAgent(page, p, () => undefined);
  await page.route(`**/agents/a1/${svc}/GetScenario`, (r) => r.fulfill({ json: { yaml: doc.replace("mode: stamp", "mode: stamp\n    drop_pct: 500") } }));
  await page.goto("./#/a/a1/scenario");
  // Problems show as soon as the schemas arrive, before any edit.
  await expect(page.locator(".cm-lintRange-error")).toHaveCount(1);
  await page.locator(".cm-line", { hasText: "mode: stamp" }).click();
  await page.keyboard.press("End");
  await page.keyboard.press("Enter");
  await page.keyboard.type("d");
  const pop = page.locator(".cm-tooltip-autocomplete");
  await expect(pop).toBeVisible();
  // The page's dark surface (--surface), not CodeMirror's light grey.
  await expect(pop).toHaveCSS("background-color", "rgb(22, 27, 36)");
  await expect(pop.locator("li").first()).toContainText("≤ 10000000");
});
