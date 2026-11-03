// The convergence example's stills, config forms and animation
// (docs/examples/convergence.md), from a running examples/convergence lab
// with traffic on the primary paths:
//   node example-convergence.mjs UI_URL OUT_DIR WORK_DIR light|dark
// OUT_DIR gets the PNGs; WORK_DIR the animation's frames and frames.json,
// for animate.py. example.sh runs the UI and calls this for both schemes.
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
const page = await browser.newPage({ viewport: { width: 1440, height: 1500 }, colorScheme: scheme });

async function go(hash, settle = 2000) {
  await page.goto(ui + hash);
  await page.waitForLoadState("networkidle").catch(() => {});
  await sleep(settle);
}

// Config forms, cropped to the dialog, for the UI/YAML tabs.
const form = async (name) => {
  await page.addStyleTag({ content: ".modal-back { background: var(--bg) !important; }" });
  await sleep(1000);
  await page.locator(".modal").screenshot({ path: `${out}/convergence-form-${name}${suffix}.png` });
};
await go("#/a/clab-convergence-sender/p/ping-plain");
await page.getByRole("button", { name: "Edit config" }).click();
await form("flow");
await go("#/a/clab-convergence-router/p/isis-bfd");
await page.getByRole("button", { name: "Edit config" }).click();
await page.locator(".modal .section-nav").getByRole("button", { name: /bfd/ }).click();
await form("isis");

// The test, set up on the Flows page: down and up on the router's eth1.
await page.setViewportSize({ width: 1440, height: 1320 });
await go("#/");
await page.reload();
await sleep(1500);
await page.locator(".sidebar").getByRole("link", { name: "Flows" }).click();
await sleep(2500);
await page.getByRole("button", { name: /Convergence test/ }).click();
await page.addStyleTag({ content: ".topbar-version, .brand-version { display: none !important; }" });
await sleep(500);

const frames = [];
let n = 0;
async function frame(ms) {
  const file = `f${String(n++).padStart(4, "0")}.png`;
  await page.screenshot({ path: `${work}/${file}` });
  frames.push({ file, ms, scene: "test" });
}
// A click the viewer can see: a ring where the pointer lands, then the click.
async function ring(locator, ms = 900) {
  const b = await locator.boundingBox();
  await page.evaluate(({ x, y }) => {
    const d = document.createElement("div");
    d.id = "tour-click";
    Object.assign(d.style, {
      position: "fixed", left: `${x - 22}px`, top: `${y - 22}px`, width: "44px", height: "44px", borderRadius: "50%",
      border: "3px solid #f59e0b", background: "rgba(245,158,11,.25)", zIndex: 99999, pointerEvents: "none",
    });
    document.body.appendChild(d);
  }, { x: b.x + b.width / 2, y: b.y + b.height / 2 });
  await frame(ms);
  await page.evaluate(() => document.getElementById("tour-click")?.remove());
}
const pick = async (label, text) => {
  const sel = page.getByLabel(label, { exact: true });
  const value = await sel.evaluate((el, t) => [...el.options].find((o) => o.textContent.includes(t))?.value, text);
  await sel.selectOption(value);
  await sleep(600);
};

await frame(1500);
await pick("Failure kind", "Port");
await pick("Failure agent", "router");
await pick("Failure port", "eth1");
await frame(1200);
await pick("Repair kind", "Port");
await pick("Repair agent", "router");
await pick("Repair port", "eth1");
await page.getByLabel("Clean for").fill("20");
await frame(1500);
const run = page.getByRole("button", { name: "Run test" });
await ring(run);
await run.click();

// A frame a second while it runs: failure, wait, repair, wait.
for (let i = 0; i < 120; i++) {
  const t0 = Date.now();
  await frame(i < 8 ? 1000 : 350);
  if ((await page.locator(".conv-phase").count()) === 2 && (await page.getByRole("button", { name: "Run test" }).count())) break;
  await sleep(Math.max(0, 1000 - (Date.now() - t0)));
}
await sleep(500);
await frame(5000);
writeFileSync(`${work}/frames.json`, JSON.stringify(frames, null, 1));

// The results alone.
await page.locator(".conv-phase").first().screenshot({ path: `${out}/convergence-failure${suffix}.png` });
await page.locator(".conv-phase").nth(1).screenshot({ path: `${out}/convergence-repair${suffix}.png` });
await page.locator(".conv").evaluate((el) => {
  el.querySelectorAll(".conv-phase").forEach((p) => (p.style.display = "none"));
});
await sleep(300);
await page.locator(".conv").screenshot({ path: `${out}/convergence-form-test${suffix}.png` });

await browser.close();
// Time for the next run: traffic back on the primary path, nothing lost lately.
await sleep(30000);
