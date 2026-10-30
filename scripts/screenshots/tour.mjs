// Drives the UI through the README tour and saves its frames:
//   node tour.mjs UI_URL OUT_DIR light|dark
// OUT_DIR gets frame PNGs, frames.json (each frame's duration and scene)
// and the stills (bgp.png and the UI page's). run.sh deploys a fresh lab and
// the UI first.
import { createRequire } from "node:module";
import { execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";

const require = createRequire(new URL("../../web/package.json", import.meta.url));
const { chromium } = require("playwright");

const [ui, out, scheme] = process.argv.slice(2);
mkdirSync(out, { recursive: true });

const gen = (node, ...args) => execFileSync("docker", ["exec", `clab-demo-${node}`, "clabgenctl", ...args], { encoding: "utf8" });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Back to a healthy lab with fresh flow statistics.
async function reset() {
  gen("gen1", "port", "up", "eth1");
  for (let i = 0; ; i++) {
    const s = gen("gen1", "protocols");
    if (s.includes("established") && s.includes("defect=none") && !s.includes(":loc") && !s.includes("idle")) break;
    if (i > 120) throw new Error("lab not healthy:\n" + s);
    await sleep(500);
  }
  for (const f of ["ping", "udp-echo", "crafted"]) gen("gen1", "proto", "run", f, "clear");
  await sleep(4000); // counters and verdicts to show
}

const browser = process.env.PLAYWRIGHT_CHROMIUM
  ? await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM })
  : await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, colorScheme: scheme });

const frames = [];
let n = 0;
async function frame(scene, ms) {
  const file = `f${String(n++).padStart(4, "0")}.png`;
  await page.screenshot({ path: `${out}/${file}` });
  frames.push({ file, ms, scene });
}
// A live scene: a frame every `every` ms for `total` ms.
async function live(scene, total, every = 500) {
  for (let t = 0; t < total; t += every) {
    const t0 = Date.now();
    await frame(scene, every);
    await sleep(Math.max(0, every - (Date.now() - t0)));
  }
}
async function go(hash, settle = 2500) {
  await page.goto(ui + hash);
  await page.waitForLoadState("networkidle").catch(() => {});
  await sleep(settle);
}
// A click the viewer can see: a ring where the pointer lands.
async function showClick(locator) {
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
}

const still = (name) => page.screenshot({ path: `${out}/${name}.png` });

await reset();

// Stills, before anything flapped. A BGP speaker's peers and routes:
await go("#/a/clab-demo-gen1/p/bgp", 3000);
await still("bgp");
// a protocol page (status, actions, events):
await go("#/a/clab-demo-gen1/p/mep", 3000);
await still("protocol");
// the group editor, with its member preview:
await go("#/a/clab-demo-gen2/g/rmep");
await page.getByRole("button", { name: "Edit", exact: true }).click();
await sleep(2000);
await still("group-editor");
// Config forms, cropped to the dialog, for the docs' UI/YAML tabs (in a
// taller window, so long dialogs aren't cut off):
await page.setViewportSize({ width: 1440, height: 1500 });
// Nothing of the page behind shows at the dialog's rounded corners.
await page.addStyleTag({ content: ".modal-back { background: var(--bg) !important; }" });
const form = async (name) => {
  await sleep(1500);
  await page.locator(".modal").screenshot({ path: `${out}/form-${name}.png` });
};
await go("#/a/clab-demo-gen1/devices", 1500);
await page.locator("tr", { hasText: "v100" }).getByRole("button", { name: "Edit" }).click();
await form("device");
for (const [proto, section] of [["mep"], ["bgp", "bfd"], ["ping"], ["crafted"]]) {
  await go(`#/a/clab-demo-gen1/p/${proto}`, 1500);
  await page.getByRole("button", { name: "Edit config" }).click();
  if (section) {
    await sleep(800);
    await page.locator(".modal nav.section-nav").getByText(section, { exact: true }).click();
  }
  await form(proto === "crafted" ? "packet" : proto);
}
await go("#/a/clab-demo-gen2/g/rmep");
await page.getByRole("button", { name: "Edit", exact: true }).click();
await form("group");
await page.setViewportSize({ width: 1440, height: 900 });
await page.reload(); // drops the style above
// a live capture with one frame decoded:
await go("#/a/clab-demo-gen1/capture", 1500);
await page.getByRole("button", { name: "● Start" }).click();
await sleep(2500);
await page.locator(".capture-row").nth(3).click();
await sleep(1000);
await still("capture");
await page.getByRole("button", { name: "■ Stop" }).click();

// 1. Everything up.
await go("#/");
await frame("overview", 3000);

// 2. A packet built layer by layer, with its decode and hex.
await go("#/a/clab-demo-gen1/p/crafted", 1500);
await page.getByRole("button", { name: "Edit config" }).click();
await sleep(1500);
await frame("packet", 3500);

// 3. Flows passing their checks, counters running.
await go("#/flows");
await live("flows", 3000);

// 4. A MEP and the six remote MEPs it expects (a device group on gen2).
await go("#/a/clab-demo-gen1/p/mep");
await live("mep", 2000, 1000);

// 5. Break it: gen1's port down, as a link failure.
await go("#/a/clab-demo-gen1/ports");
const sw = page.locator("button.toggle").first();
await showClick(sw);
await frame("break", 1200);
await sw.click();
await sleep(600);
await page.evaluate(() => document.getElementById("tour-click")?.remove());
await frame("break", 1500);

// 6. What it did: flows fail their checks, the MEP loses its remotes,
// BGP drops on BFD.
await go("#/flows", 1500);
await live("flows-down", 3000);
await go("#/a/clab-demo-gen1/p/mep", 1500);
await frame("mep-down", 2500);
await go("#/", 2500);
await frame("overview-down", 3500);

// Still: a flow's page after the outage, with the port back up.
gen("gen1", "port", "up", "eth1");
await go("#/a/clab-demo-gen1/p/ping", 10000);
await still("flow");

writeFileSync(`${out}/frames.json`, JSON.stringify(frames, null, 1));
await browser.close();
