// The TWAMP example's reflector status, its form, and the animation of a
// burst of lost replies then a muted reflector (docs/examples/twamp.md), from
// a running examples/twamp lab with sr1's sessions answered:
//   node example-twamp.mjs UI_URL OUT_DIR WORK_DIR light|dark
// OUT_DIR gets the PNGs; WORK_DIR the animation's frames and frames.json, for
// animate.py. example.sh runs the UI and calls this for both schemes.
import { createRequire } from "node:module";
import { execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";

const require = createRequire(new URL("../../web/package.json", import.meta.url));
const { chromium } = require("playwright");

const [ui, out, work, scheme] = process.argv.slice(2);
mkdirSync(work, { recursive: true });
const suffix = scheme === "dark" ? "-dark" : "";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const ctl = (...args) => execFileSync("docker", ["exec", "clab-twamp-gen", "clabgenctl", ...args], { encoding: "utf8" });
const status = () => JSON.parse(ctl("proto", "show", "reflector", "-o", "json")).status;

const browser = process.env.PLAYWRIGHT_CHROMIUM
  ? await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM })
  : await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 1100 }, colorScheme: scheme });

async function go(hash, settle = 2000) {
  await page.goto(ui + hash);
  await page.waitForLoadState("networkidle").catch(() => {});
  await sleep(settle);
}
const flat = async () => {
  await page.addStyleTag({ content: ".modal-back { background: var(--bg) !important; }" });
  await sleep(800);
};

// A clean start: no faults, answering, counters cleared, all three of sr1's
// sessions (two OAM-PM, one link measurement) heard again.
ctl("proto", "set", "reflector", "delay_us=0", "drop_pct=0");
ctl("proto", "run", "reflector", "unmute");
ctl("proto", "run", "reflector", "clear");
for (let i = 0; i < 30 && status().active < 3; i++) await sleep(1000);
await sleep(8000);

const card = () => page.locator(".card").filter({ has: page.getByRole("heading", { name: "Status" }) });
await go("#/a/clab-twamp-gen/p/reflector");
await card().screenshot({ path: `${out}/twamp-status${suffix}.png` });

await page.getByRole("button", { name: "Edit config" }).click();
await sleep(800);
await flat();
await page.locator(".modal").screenshot({ path: `${out}/twamp-form${suffix}.png` });

// The animation: burst-loss of 5 from the Actions card, then mute for a few
// seconds and unmute, on the reflector's page.
const frames = [];
let n = 0;
async function frame(ms, scene) {
  const file = `f${String(n++).padStart(4, "0")}.png`;
  await page.screenshot({ path: `${work}/${file}` });
  frames.push({ file, ms, scene });
}
async function tick(scene) {
  const t0 = Date.now();
  await frame(700, scene);
  await sleep(Math.max(0, 1000 - (Date.now() - t0)));
}
await go("#/a/clab-twamp-gen/p/reflector", 0);
await page.reload(); // closes the form
await page.waitForLoadState("networkidle").catch(() => {});
await sleep(2500);
await page.addStyleTag({ content: ".brand-version { display: none !important; }" });
const actions = page.locator(".card").filter({ has: page.getByRole("heading", { name: "Actions" }) });
await frame(2000, "steady");
await actions.getByRole("tab", { name: "burst-loss" }).click();
await actions.locator("input").first().fill("5");
await frame(1500, "burst");
await actions.getByRole("button", { name: "Run burst-loss" }).click();
for (let i = 0; i < 7; i++) await tick("burst");
await actions.getByRole("tab", { name: "mute", exact: true }).click();
await frame(1200, "mute");
await actions.getByRole("button", { name: "Run mute" }).click();
for (let i = 0; i < 6; i++) await tick("muted");
await actions.getByRole("tab", { name: "unmute" }).click();
await actions.getByRole("button", { name: "Run unmute" }).click();
for (let i = 0; i < 4; i++) await tick("back");
await frame(2500, "back");
writeFileSync(`${work}/frames.json`, JSON.stringify(frames, null, 1));

await browser.close();
ctl("proto", "run", "reflector", "unmute");
