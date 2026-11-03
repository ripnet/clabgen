// The BGP routes example's form, results and animation
// (docs/examples/bgp-routes.md), from a running examples/bgp-routes lab with
// to-routes clean:
//   node example-bgp-routes.mjs UI_URL OUT_DIR WORK_DIR light|dark
// OUT_DIR gets the PNGs; WORK_DIR the animation's frames and frames.json, for
// animate.py. example.sh runs the UI and calls this for both schemes.
import { createRequire } from "node:module";
import { mkdirSync, writeFileSync } from "node:fs";

const require = createRequire(new URL("../../web/package.json", import.meta.url));
const { chromium } = require("playwright");

const [ui, out, work, scheme] = process.argv.slice(2);
mkdirSync(work, { recursive: true });
const suffix = scheme === "dark" ? "-dark" : "";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const browser = process.env.PLAYWRIGHT_CHROMIUM
  ? await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM })
  : await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 1320 }, colorScheme: scheme });

async function go(hash, settle = 2000) {
  await page.goto(ui + hash);
  await page.waitForLoadState("networkidle").catch(() => {});
  await sleep(settle);
}

// The flow's config, with the routes picked.
await go("#/a/clab-bgp-routes-sender/p/to-routes");
await page.getByRole("button", { name: "Edit config" }).click();
await page.getByRole("button", { name: "From BGP routes" }).click();
await sleep(1500);
for (const c of await page.locator(".route-picker-row input").all()) await c.check();
await page.addStyleTag({ content: ".modal-back { background: var(--bg) !important; }" });
await sleep(800);
await page.locator(".modal").screenshot({ path: `${out}/bgp-routes-form${suffix}.png` });

// The test, set up on the Flows page.
await go("#/");
await page.reload();
await sleep(1500);
await page.locator(".sidebar").getByRole("link", { name: "Flows" }).click();
await sleep(2500);
await page.getByRole("button", { name: /Convergence test/ }).click();
await page.addStyleTag({ content: ".brand-version { display: none !important; }" });
await sleep(500);

const frames = [];
let n = 0;
async function frame(ms) {
  const file = `f${String(n++).padStart(4, "0")}.png`;
  await page.screenshot({ path: `${work}/${file}` });
  frames.push({ file, ms, scene: "test" });
}
const pick = async (label, text) => {
  const sel = page.getByLabel(label, { exact: true });
  const value = await sel.evaluate((el, t) => [...el.options].find((o) => o.textContent.includes(t))?.value, text);
  await sel.selectOption(value);
  await sleep(500);
};
// The action's routes argument: one block of five.
async function routes(step) {
  const form = page.locator(".conv-step").nth(step);
  await form.getByRole("button", { name: "Add", exact: true }).click();
  await sleep(300);
  await form.getByRole("button", { name: /show all options/ }).click();
  await sleep(300);
  const field = (name) => form.locator(".field").filter({ has: page.locator(".field-label", { hasText: new RegExp(`^${name}\\b`) }) }).locator("input").first();
  await field("prefix").fill("198.51.10.0/24");
  await field("count").fill("5");
}

await frame(1500);
await pick("Failure kind", "Protocol action");
await pick("Failure agent", "routes");
await pick("Failure protocol", "bgp");
await pick("Failure action", "withdraw");
await routes(0);
await frame(1500);
await pick("Repair kind", "Protocol action");
await pick("Repair agent", "routes");
await pick("Repair protocol", "bgp");
await pick("Repair action", "advertise");
await routes(1);
await page.getByLabel("Clean for").fill("6");
await page.getByLabel("Give up after").fill("12");
await frame(2000);
await page.getByRole("button", { name: "Run test" }).click();

for (let i = 0; i < 120; i++) {
  const t0 = Date.now();
  await frame(600);
  if ((await page.locator(".conv-phase").count()) === 2 && (await page.getByRole("button", { name: "Run test" }).count())) break;
  await sleep(Math.max(0, 1000 - (Date.now() - t0)));
}
const list = page.locator(".conv-phase").first().locator("details").first();
await list.locator("summary").click();
await sleep(500);
await page.locator(".conv-phase").first().scrollIntoViewIfNeeded();
await frame(5000);
writeFileSync(`${work}/frames.json`, JSON.stringify(frames, null, 1));
await page.locator(".conv-phase").first().screenshot({ path: `${out}/bgp-routes-failure${suffix}.png` });

await browser.close();
await sleep(20000); // nothing lost lately, for the next run
