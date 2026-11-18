// IS-IS and OSPF against an SR-SIM (docs/isis.md, docs/ospf.md), from a
// running examples/srsim lab with its adjacencies up:
//   node example-srsim.mjs UI_URL OUT_DIR WORK_DIR light|dark
// OUT_DIR gets srsim-isis.png, srsim-isis-form.png, srsim-ospf.png and
// srsim-ospf-form.png (and -dark). example.sh runs the UI and calls this for
// both schemes.
import { createRequire } from "node:module";
import { execFileSync } from "node:child_process";

const require = createRequire(new URL("../../web/package.json", import.meta.url));
const { chromium } = require("playwright");

const [ui, out, , scheme] = process.argv.slice(2);
const suffix = scheme === "dark" ? "-dark" : "";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const protocols = () => execFileSync("docker", ["exec", "clab-srsim-gen1", "clabgenctl", "protocols"], { encoding: "utf8" });
// Settled: IS-IS has its three adjacencies, OSPF its neighbors full.
for (let i = 0; i < 120; i++) {
  const s = protocols();
  if (/^isis .*3 neighbors|^isis .*interfaces=\[i500:1 i502:1 i503:1\]/m.test(s) && /^ospf .*nbrs=\[[^\]]*full[^\]]*full/m.test(s)) break;
  await sleep(2000);
}
await sleep(5000);

const browser = process.env.PLAYWRIGHT_CHROMIUM
  ? await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM })
  : await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 1100 }, colorScheme: scheme });

async function go(hash, settle = 3000) {
  await page.goto(ui + hash);
  await page.waitForLoadState("networkidle").catch(() => {});
  await sleep(settle);
}

for (const p of ["isis", "ospf"]) {
  // The page as it opens: the summary, interfaces and adjacencies.
  await page.setViewportSize({ width: 1440, height: 1100 });
  await go(`#/a/clab-srsim-gen1/p/${p}`);
  await page.screenshot({ path: `${out}/srsim-${p}${suffix}.png` });
  // The config form, on a plain background.
  await page.setViewportSize({ width: 1440, height: 1500 });
  await page.getByRole("button", { name: "Edit config" }).click();
  await sleep(800);
  await page.addStyleTag({ content: ".modal-back { background: var(--bg) !important; }" });
  await sleep(800);
  await page.locator(".modal").screenshot({ path: `${out}/srsim-${p}-form${suffix}.png` });
}
await browser.close();
