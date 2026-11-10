// The MACsec example's form, statuses, and the animation of a provider
// dropping the MKA exchange (docs/examples/macsec.md), from a running
// examples/macsec lab with both circuits secured:
//   node example-macsec.mjs UI_URL OUT_DIR WORK_DIR light|dark
// OUT_DIR gets the PNGs; WORK_DIR the animation's frames and frames.json, for
// animate.py. example.sh runs the UI and calls this for both schemes. EAPOL
// tunneling is turned off and on over SSH on sr1, with containerlab's default
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
function tunneling(on) {
  const cmds = ["edit-config private", `/configure port 1/1/c1/1 ethernet dot1x tunneling ${on}`, "commit", "quit-config", "logout"].join("\n") + "\n";
  spawnSync("sshpass", ["-p", password, "ssh", "-tt", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "admin@clab-macsec-sr1"], { input: cmds, timeout: 60000 });
}
const ctl = (node, ...args) => execFileSync("docker", ["exec", `clab-macsec-${node}`, "clabgenctl", ...args], { encoding: "utf8" });
const secured = (node, name) => JSON.parse(ctl(node, "proto", "show", name, "-o", "json")).status.mka.state === "secured";

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

// A clean start: tunneling on, both circuits secured, nothing lost lately.
tunneling("true");
for (let i = 0; i < 60 && !(secured("ce-a", "epl-macsec") && secured("ce-a", "evpl-macsec")); i++) await sleep(1000);
for (const f of ["epl-ping", "evpl-ping", "epl-macsec", "evpl-macsec"]) ctl("ce-a", "proto", "run", f, "clear");
await sleep(3000);

// The port-level instance: its status, and its form.
await go("#/a/clab-macsec-ce-a/p/epl-macsec");
await page.locator(".card").filter({ has: page.getByRole("heading", { name: "Status" }) }).screenshot({ path: `${out}/macsec-status${suffix}.png` });
await page.getByRole("button", { name: "Edit config" }).click();
await sleep(800);
await page.locator(".modal .section-nav").getByRole("button", { name: /mka/ }).click();
await sleep(500);
await flat();
await page.locator(".modal").screenshot({ path: `${out}/macsec-form${suffix}.png` });

// The VLAN 100 instance's frame: the tag in front of the SecTAG.
await go("#/a/clab-macsec-ce-a/p/evpl-macsec");
await page.locator(".ms-frame-wrap").screenshot({ path: `${out}/macsec-frame-vlan${suffix}.png` });

// The animation: the Flows page while sr1 drops the MKA exchange, with a look
// at ce-a's protocols when the session is gone and when it's back.
const frames = [];
let n = 0;
async function frame(ms, scene) {
  const file = `f${String(n++).padStart(4, "0")}.png`;
  await page.screenshot({ path: `${work}/${file}` });
  frames.push({ file, ms, scene });
}
async function flowsPage() {
  await go("#/flows", 2500);
  await page.addStyleTag({ content: ".brand-version { display: none !important; }" });
}
async function protocolsPage() {
  await go("#/a/clab-macsec-ce-a/protocols", 1500);
  await page.addStyleTag({ content: ".brand-version { display: none !important; }" });
}
async function tick(scene) {
  const t0 = Date.now();
  await frame(700, scene);
  await sleep(Math.max(0, 1000 - (Date.now() - t0)));
}
await flowsPage();
await frame(2500, "flows");
tunneling("false");
for (let i = 0; i < 12; i++) await tick("flows");
await protocolsPage();
await frame(3500, "mka");
await flowsPage();
tunneling("true");
for (let i = 0; i < 30 && !secured("ce-a", "epl-macsec"); i++) await tick("flows");
for (let i = 0; i < 4; i++) await tick("flows");
await frame(2500, "flows");
await protocolsPage();
await frame(4000, "mka2");
writeFileSync(`${work}/frames.json`, JSON.stringify(frames, null, 1));

await browser.close();
for (const f of ["epl-ping", "evpl-ping"]) ctl("ce-a", "proto", "run", f, "clear");
