import { expect, test } from "@playwright/test";
import { mockAgent } from "./mock";

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
