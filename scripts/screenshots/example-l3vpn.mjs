// The L3VPN example's forms, statuses, and the animation of a convergence test
// with one route withdrawn (docs/examples/l3vpn.md), from a running
// examples/l3vpn lab with the flows clean:
//   node example-l3vpn.mjs UI_URL OUT_DIR WORK_DIR light|dark
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
const ctl = (node, ...args) => execFileSync("docker", ["exec", `clab-l3vpn-${node}`, "clabgenctl", ...args], { encoding: "utf8" });
const route = 'routes=[{"prefix":"203.0.116.0/24"}]';

const browser = process.env.PLAYWRIGHT_CHROMIUM
  ? await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM })
  : await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 1320 }, colorScheme: scheme });

async function go(hash, settle = 2000) {
  await page.goto(ui + hash);
  await page.waitForLoadState("networkidle").catch(() => {});
  await sleep(settle);
}

const flat = async () => {
  await page.addStyleTag({ content: ".modal-back { background: var(--bg) !important; }" });
  await sleep(800);
};

// A clean start: every route there, and nothing lost lately.
ctl("ce", "proto", "run", "bgp", "advertise", route);
await sleep(3000);
for (const [n, f] of [["pe", "to-ce"], ["pe", "to-ce-v6"], ["ce", "to-pe"]]) ctl(n, "proto", "run", f, "clear");
await sleep(3000);

// The flow's page: labels, the route and the far PE.
await go("#/a/clab-l3vpn-pe/p/to-ce");
await page.locator(".flow-path").screenshot({ path: `${out}/l3vpn-path${suffix}.png` });

// Its form, with the VPN picker open.
await page.getByRole("button", { name: "Edit config" }).click();
await sleep(800);
await page.getByRole("button", { name: "In a VPN…" }).click();
await page.locator(".vpn-choice").first().waitFor();
await sleep(500);
await flat();
await page.locator(".modal").screenshot({ path: `${out}/l3vpn-flow${suffix}.png` });

// The routes received from sr2, with their labels.
await go("#/a/clab-l3vpn-pe/p/bgp");
await page.getByRole("cell", { name: /192\.0\.2\.12/ }).first().click();
await sleep(1500);
await page.getByRole("heading", { name: /Routes from/ }).locator("..").screenshot({ path: `${out}/l3vpn-rib${suffix}.png` });

// The test, on the Flows page.
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
await page.getByLabel("Clean for").fill("5");
await page.getByLabel("Give up after").fill("20");
await frame(2000);
await page.getByRole("button", { name: "Run test" }).click();
await sleep(1000);
await frame(1000);
ctl("ce", "proto", "run", "bgp", "withdraw", route);
await sleep(3000);
await frame(1500);
ctl("ce", "proto", "run", "bgp", "advertise", route);

for (let i = 0; i < 60; i++) {
  const t0 = Date.now();
  await frame(600);
  if ((await page.locator(".conv-phase").count()) >= 1 && (await page.getByRole("button", { name: "Run test" }).count())) break;
  await sleep(Math.max(0, 1000 - (Date.now() - t0)));
}
await sleep(500);
await frame(6000);
writeFileSync(`${work}/frames.json`, JSON.stringify(frames, null, 1));

await browser.close();
await sleep(10000); // nothing lost lately, for the next run
