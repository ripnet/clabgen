// The wire example's screenshots (docs/examples/wire.md), from a running
// examples/wire lab with its pings going:
//   node example-wire.mjs UI_URL OUT_DIR WORK_DIR light|dark
// OUT_DIR gets the PNGs; WORK_DIR the animation's frames and frames.json, for
// animate.py. example.sh runs the UI and calls this for both schemes. sr1's
// evpl SAP is changed and put back over SSH, with containerlab's default
// credentials (SRSIM_PASSWORD overrides them), and needs sshpass.
import { createRequire } from "node:module";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";

const require = createRequire(new URL("../../web/package.json", import.meta.url));
const { chromium } = require("playwright");

const [ui, out, work, scheme] = process.argv.slice(2);
mkdirSync(work, { recursive: true });
const suffix = scheme === "dark" ? "-dark" : "";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const password = process.env.SRSIM_PASSWORD || "NokiaSros1!";
// sap: the evpl SAP on sr1, ":3162" (as configured) or ":*" (keeps every tag).
function sap(to) {
  const from = to === "*" ? "3162" : "*";
  const cmds = ["edit-config private", `delete /configure service epipe "evpl" sap 1/1/c3/1:${from}`,
    `/configure service epipe "evpl" sap 1/1/c3/1:${to} admin-state enable`, "commit", "quit-config", "logout"].join("\n") + "\n";
  spawnSync("sshpass", ["-p", password, "ssh", "-tt", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "admin@clab-wire-sr1"], { input: cmds, timeout: 60000 });
}
const ctl = (node, ...args) => execFileSync("docker", ["exec", `clab-wire-${node}`, "clabgenctl", ...args], { encoding: "utf8" });
const clear = () => ctl("tap", "proto", "run", "core", "clear");

const browser = process.env.PLAYWRIGHT_CHROMIUM
  ? await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM })
  : await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 1100 }, colorScheme: scheme });

async function go(hash, settle = 2000) {
  await page.goto(ui + hash);
  await page.waitForLoadState("networkidle").catch(() => {});
  await sleep(settle);
  await page.addStyleTag({ content: ".brand-version { display: none !important; }" });
}
const flat = async () => {
  await page.addStyleTag({ content: ".modal-back { background: var(--bg) !important; }" });
  await sleep(800);
};

// A clean start: the SAP as configured, nothing seen from before.
sap("3162");
clear();
await sleep(6000);

// The flow's Wire section, and the form's wire fields.
await go("#/a/clab-wire-ce-a/p/evpl-ping", 3000);
await page.locator(".wire-section").screenshot({ path: `${out}/wire-flow${suffix}.png` });
await page.getByRole("button", { name: "Edit config" }).click();
await sleep(800);
await page.locator(".modal .section-nav").getByRole("button", { name: /wire/ }).click();
await sleep(500);
await flat();
await page.locator(".modal").screenshot({ path: `${out}/wire-form${suffix}.png` });

// The monitor's page, in a window tall enough for all of it.
await page.setViewportSize({ width: 1440, height: 1900 });
await go("#/a/clab-wire-tap/p/core", 3000);
await page.locator(".card").filter({ has: page.getByRole("heading", { name: "Status" }) }).screenshot({ path: `${out}/wire-monitor${suffix}.png` });

await page.setViewportSize({ width: 1440, height: 1100 });

// The animation: evpl-ping's page while sr1's SAP keeps both tags, then
// with it put back and the monitor cleared.
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
await go("#/a/clab-wire-ce-a/p/evpl-ping", 3000);
const section = page.locator(".wire-section");
await section.scrollIntoViewIfNeeded();
await frame(2500, "pass");
sap("*");
for (let i = 0; i < 6; i++) await tick("pass");
await frame(3500, "fail");
sap("3162");
clear();
for (let i = 0; i < 6; i++) await tick("fail");
await frame(3000, "pass2");
writeFileSync(`${work}/frames.json`, JSON.stringify(frames, null, 1));

await browser.close();
