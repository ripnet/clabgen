// The DHCP example's stills, config forms and animation
// (docs/examples/dhcp.md), from a running examples/dhcp lab with every
// client bound:
//   node example-dhcp.mjs UI_URL OUT_DIR WORK_DIR light|dark
// OUT_DIR gets the PNGs; WORK_DIR the animation's frames and frames.json,
// for animate.py. example.sh runs the UI and calls this for both schemes.
import { createRequire } from "node:module";
import { execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";

const require = createRequire(new URL("../../web/package.json", import.meta.url));
const { chromium } = require("playwright");

const [ui, out, work, scheme] = process.argv.slice(2);
mkdirSync(work, { recursive: true });
const suffix = scheme === "dark" ? "-dark" : "";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const client = (...args) => execFileSync("docker", ["exec", "clab-dhcp-client", "clabgenctl", ...args], { encoding: "utf8" });

// Every subscriber bound again (after the last run's animation, say).
async function settled() {
  for (let i = 0; ; i++) {
    if (client("groups").includes("v4: 10 bound; v6: 10 bound")) return;
    if (i > 120) throw new Error("subscribers not bound:\n" + client("groups"));
    await sleep(1000);
  }
}

const browser = process.env.PLAYWRIGHT_CHROMIUM
  ? await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM })
  : await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, colorScheme: scheme });

async function go(hash, settle = 3000) {
  await page.goto(ui + hash);
  await page.waitForLoadState("networkidle").catch(() => {});
  await sleep(settle);
}
async function still(name, hash, size = { width: 1440, height: 900 }) {
  await page.setViewportSize(size);
  await go(hash);
  await page.screenshot({ path: `${out}/dhcp-${name}${suffix}.png` });
}

await settled();

// Pages: client1's DHCPv6 client, the group, the servers' leases (the
// group wide and tall enough for all ten members and every column).
await still("client", "#/a/clab-dhcp-client/p/client1-v6");
await still("group", "#/a/clab-dhcp-client/g/subscribers", { width: 1600, height: 1240 });
await still("server4", "#/a/clab-dhcp-server/p/dhcp4");
await still("server6", "#/a/clab-dhcp-server/p/dhcp6");

// Config forms, cropped to the dialog, for the UI/YAML tabs (in a taller
// window, so long dialogs aren't cut off).
await page.setViewportSize({ width: 1440, height: 1500 });
const form = async (name) => {
  // Nothing of the page behind shows at the dialog's rounded corners.
  await page.addStyleTag({ content: ".modal-back { background: var(--bg) !important; }" });
  await sleep(1000);
  await page.locator(".modal").screenshot({ path: `${out}/dhcp-form-${name}${suffix}.png` });
};
for (const [name, proto] of [["server4", "dhcp4"], ["server6", "dhcp6"]]) {
  await go(`#/a/clab-dhcp-server/p/${proto}`, 1500);
  await page.getByRole("button", { name: "Edit config" }).click();
  const modal = page.locator(".modal");
  await modal.locator(".section-nav").getByRole("button", { name: /pools/ }).click();
  await modal.locator(".obj-item-summary").first().click();
  await modal.getByRole("button", { name: /show all options/ }).click();
  await form(name);
}
await go("#/a/clab-dhcp-client/p/client1-v6", 1500);
await page.getByRole("button", { name: "Edit config" }).click();
await form("client");
await go("#/a/clab-dhcp-client/g/subscribers", 1500);
await page.getByRole("button", { name: "Edit", exact: true }).click();
await form("group");

// The animation: release the DHCPv6 leases of five subscribers from the
// group page, watch them go, then restart them and watch them come back.
await page.setViewportSize({ width: 1600, height: 1240 });
await go("#/a/clab-dhcp-client/g/subscribers");
await page.reload(); // closes the group editor, drops the style above
await sleep(3000);
const frames = [];
let n = 0;
async function frame(ms) {
  const file = `f${String(n++).padStart(4, "0")}.png`;
  await page.screenshot({ path: `${work}/${file}` });
  frames.push({ file, ms, scene: "group" });
}
async function live(total, every = 500) {
  for (let t = 0; t < total; t += every) {
    const t0 = Date.now();
    await frame(every);
    await sleep(Math.max(0, every - (Date.now() - t0)));
  }
}
// A click the viewer can see: a ring where the pointer lands, then the click.
async function click(locator, ms = 900) {
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
  await locator.click();
  await page.evaluate(() => document.getElementById("tour-click")?.remove());
}
const actions = page.locator(".card").filter({ has: page.getByRole("heading", { name: "Actions", exact: true }) });

await frame(2500);
await click(actions.getByRole("button", { name: "v6", exact: true }));
await click(actions.getByRole("tab", { name: "release", exact: true }));
const members = actions.getByPlaceholder(/^all/);
await click(members, 600);
await members.fill("1-5");
await frame(1200);
await click(actions.getByRole("button", { name: "Run release" }));
await live(4000);
await click(actions.getByRole("tab", { name: "restart", exact: true }));
await click(actions.getByRole("button", { name: "Run restart" }));
await live(6000);
await frame(2500);

writeFileSync(`${work}/frames.json`, JSON.stringify(frames, null, 1));
await browser.close();
await settled();
