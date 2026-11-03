// The RSVP example's forms, statuses, and the animation of a convergence test
// with the protected link failing (docs/examples/rsvp.md), from a running
// examples/rsvp lab with the LSP up:
//   node example-rsvp.mjs UI_URL OUT_DIR WORK_DIR light|dark
// OUT_DIR gets the PNGs; WORK_DIR the animation's frames and frames.json, for
// animate.py. example.sh runs the UI and calls this for both schemes. The port
// is failed over SSH on sr1, with containerlab's default credentials
// (SRSIM_PASSWORD overrides them), and needs sshpass.
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
function sr1Port(state) {
  const cmds = ["edit-config private", `/configure port 1/1/c2/1 admin-state ${state}`, "commit", "quit-config", "logout"].join("\n") + "\n";
  spawnSync("sshpass", ["-p", password, "ssh", "-tt", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "admin@clab-rsvp-sr1"], { input: cmds, timeout: 60000 });
}
const ctl = (node, ...args) => execFileSync("docker", ["exec", `clab-rsvp-${node}`, "clabgenctl", ...args], { encoding: "utf8" });

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

// Back on the primary path, with protection available, for a clean start.
sr1Port("enable");
await sleep(15000);
ctl("head", "proto", "run", "rsvp", "resignal", "lsp=to-tail");
for (let i = 0; i < 60; i++) {
  if (ctl("head", "proto", "show", "rsvp", "-o", "json").includes('"available"')) break;
  await sleep(3000);
}

// The head: its page and its config (LSPs section).
await go("#/a/clab-rsvp-head/p/rsvp");
await page.locator(".rsvp-lsp").first().screenshot({ path: `${out}/rsvp-head${suffix}.png` });
await page.getByRole("button", { name: "Edit config" }).click();
await page.locator(".modal .section-nav").getByRole("button", { name: /lsps/ }).click();
await sleep(500);
await page.locator(".modal .caret").first().click();
await sleep(500);
await flat();
await page.locator(".modal").screenshot({ path: `${out}/rsvp-form${suffix}.png` });

// The flow's form.
await go("#/a/clab-rsvp-head/p/into-lsp");
await page.getByRole("button", { name: "Edit config" }).click();
await sleep(1000);
await flat();
await page.locator(".modal").screenshot({ path: `${out}/rsvp-flow${suffix}.png` });

// The tail's page.
await go("#/a/clab-rsvp-tail/p/rsvp");
await page.locator(".main .table-wrap").first().screenshot({ path: `${out}/rsvp-tail${suffix}.png` });

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
await page.getByLabel("Clean for").fill("8");
await page.getByLabel("Give up after").fill("30");
await frame(2000);
await page.getByRole("button", { name: "Run test" }).click();
await sleep(1000);
await frame(1000);
sr1Port("disable");

for (let i = 0; i < 80; i++) {
  const t0 = Date.now();
  await frame(600);
  if ((await page.locator(".conv-phase").count()) >= 1 && (await page.getByRole("button", { name: "Run test" }).count())) break;
  await sleep(Math.max(0, 1000 - (Date.now() - t0)));
}
await sleep(500);
await page.getByText(/events? during this step/).first().click();
await sleep(500);
await frame(6000);
writeFileSync(`${work}/frames.json`, JSON.stringify(frames, null, 1));

await browser.close();
sr1Port("enable");
await sleep(20000); // the link back, and nothing lost lately, for the next run
