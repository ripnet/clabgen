import { expect, test, type Page } from "@playwright/test";
import { mockAgent } from "./mock";
import { layout, pairs, type TLink, type TNode } from "../src/components/topology/layout";

const nodes: TNode[] = [
  { name: "gen", kind: "linux", image: "ghcr.io/ripnet/clabgen:latest", agent: "clab-lab1-gen" },
  { name: "r1", kind: "nokia_srsim", image: "nokia_srsim:26.10.R1" },
  { name: "r2", kind: "nokia_srsim", image: "nokia_srsim:26.10.R1" },
  { name: "sw", kind: "nokia_srsim" },
];
const links: TLink[] = [
  { a: "gen", a_if: "eth1", z: "r1", z_if: "e1-1-c1-1" },
  { a: "gen", a_if: "eth2", z: "r1", z_if: "e1-1-c2-1" },
  { a: "r1", a_if: "e1-1-c3-1", z: "gen", z_if: "eth3" },
  { a: "r1", a_if: "e1-1-c10-1", z: "r2", z_if: "e1-1-c10-1" },
  ...Array.from({ length: 6 }, (_, i) => ({ a: "r2", a_if: `e1-1-c${i + 1}-1`, z: "sw", z_if: `e1-1-c${i + 1}-1` })),
  { a: "sw", a_if: "eth9", z: "host", z_if: "tap0" },
];

test("layout: the same each time, no boxes overlapping, containerlab's positions kept", () => {
  const a = layout(nodes, links);
  expect(layout(nodes, links)).toEqual(a);
  const names = Object.keys(a);
  for (let i = 0; i < names.length; i++)
    for (let j = i + 1; j < names.length; j++) {
      const p = a[names[i]];
      const q = a[names[j]];
      const apart = Math.abs(p.x - q.x) >= (p.w + q.w) / 2 || Math.abs(p.y - q.y) >= (p.h + q.h) / 2;
      expect(apart, `${names[i]} and ${names[j]} overlap`).toBe(true);
    }
  const given = nodes.map((n, i) => ({ ...n, x: i * 400, y: 100 }));
  expect(layout(given, links).r2).toMatchObject({ x: 800, y: 100 });
  // Saved (dragged) positions win; the rest are laid out around them.
  const saved = layout(nodes, links, { r1: { x: 5000, y: 5000 } });
  expect(saved.r1).toMatchObject({ x: 5000, y: 5000 });
  for (const n of ["gen", "r2", "sw"]) expect(Math.hypot(saved[n].x - 5000, saved[n].y - 5000)).toBeGreaterThan(100);
});

test("pairs: links between the same two nodes together, seen from one side", () => {
  const ps = pairs(links);
  const gr = ps.find((p) => p.a === "gen" && p.z === "r1")!;
  expect(gr.links.map((l) => `${l.a_if}-${l.z_if}`)).toEqual(["eth1-e1-1-c1-1", "eth2-e1-1-c2-1", "eth3-e1-1-c3-1"]);
  expect(ps.find((p) => p.a === "r2" && p.z === "sw")!.links).toHaveLength(6);
});

// at: a node's position from its transform.
const at = (t: string | null) => (t ?? "").match(/-?[\d.]+/g)!.map(Number);
const near = (a: number[], b: number[]) => a.every((v, i) => Math.abs(v - b[i]) < 1);

async function lab(page: Page, opts: { topology?: boolean; note?: string; source?: string } = {}) {
  await mockAgent(page, { name: "pk", type: "packet", device: "d1", state: "running", config: {} }, () => undefined);
  await page.route("**/api/agents", (r) =>
    r.fulfill({ json: { agents: [{ id: "clab-lab1-gen", name: "gen", lab: "lab1", source: opts.source ?? "docker", url: "172.20.20.2:8080" }], allow_manual: false } }),
  );
  await page.route("**/api/info", (r) => r.fulfill({ json: { version: "dev", update_available: false, topology: opts.topology ?? true } }));
  await page.route("**/api/labs/lab1/topology", (r) =>
    r.fulfill({
      json: opts.note
        ? { lab: "lab1", nodes: nodes.slice(0, 2), links: [], note: opts.note }
        : { lab: "lab1", nodes, links, from: "clab-lab1-gen:/clab/clab-lab1/topology-data.json" },
    }),
  );
}

test("a lab's nodes and links: parallel lines, a bundle, the agent links to its page", async ({ page }) => {
  await lab(page);
  await page.goto("./#/");
  await page.getByRole("link", { name: "Show lab lab1's nodes and links" }).click();
  await expect(page).toHaveURL(/#\/lab\/lab1$/);
  await expect(page.locator(".topo")).toContainText("4 nodes · 11 links");
  // host, named only by a link, is drawn too (dashed).
  await expect(page.locator(".topo-node")).toHaveCount(5);
  await expect(page.locator(".topo-node.external")).toContainText("host");
  await expect(page.locator(".topo-node.agent")).toContainText("gen");
  // Three links gen–r1: three lines; six r2–sw: one line with a count.
  await expect(page.locator(".topo-pair:not(.bundle)").filter({ has: page.locator("title", { hasText: "gen eth1" }) }).locator("line")).toHaveCount(3);
  const bundle = page.locator(".topo-pair.bundle");
  await expect(bundle).toHaveCount(1);
  await expect(bundle.locator(".topo-badge-text")).toHaveText("×6");
  await bundle.locator(".topo-badge").click();
  await expect(page.locator(".topo-sel")).toContainText("r2 ↔ sw: 6 links");
  await expect(page.locator(".topo-sel")).toContainText("e1-1-c6-1 ↔ e1-1-c6-1");
  await expect(page.locator(".topo-table tbody tr")).toHaveCount(11);

  // Dragging a node moves it and is remembered; a click opens the agent.
  const r2 = page.locator(".topo-node", { hasText: "r2" });
  const before = await r2.getAttribute("transform");
  const box = (await r2.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2 + 80, box.y + box.height / 2 + 40, { steps: 5 });
  await page.mouse.up();
  const after = await r2.getAttribute("transform");
  expect(after).not.toBe(before);
  await page.reload();
  await expect.poll(async () => near(at(await page.locator(".topo-node", { hasText: "r2" }).getAttribute("transform")), at(after)), "kept after a reload").toBe(true);
  await page.getByRole("button", { name: "Rearrange" }).click();
  await expect.poll(async () => near(at(await page.locator(".topo-node", { hasText: "r2" }).getAttribute("transform")), at(before)), "laid out again").toBe(true);

  await page.locator(".topo-node.agent").click();
  await expect(page).toHaveURL(/#\/a\/clab-lab1-gen\//);
});

test("no icon without topology support or for agents added by hand; a lab without links says why", async ({ page }) => {
  await lab(page, { topology: false });
  await page.goto("./#/");
  await expect(page.locator(".side-group-title", { hasText: "lab1" })).toBeVisible();
  await expect(page.locator(".side-topo")).toHaveCount(0);

  await page.unrouteAll();
  await lab(page, { source: "manual" });
  await page.reload();
  await expect(page.locator(".side-group-title", { hasText: "lab1" })).toBeVisible();
  await expect(page.locator(".side-topo")).toHaveCount(0);

  await page.unrouteAll();
  await lab(page, { note: "Links unknown: no clabgen node mounts the topology's folder at /clab" });
  await page.goto("./#/lab/lab1");
  await expect(page.locator(".warn-box")).toContainText("no clabgen node mounts");
  await expect(page.locator(".topo-node")).toHaveCount(2);
});

test("link state from the agent's ports: up, down, shut, unknown; router links have none", async ({ page }) => {
  await lab(page);
  let reachable = true;
  await page.route("**/agents/clab-lab1-gen/clabgen.v1.AgentService/ListPorts", (r) =>
    reachable
      ? r.fulfill({
          json: {
            ports: [
              { name: "eth1", adminUp: true, operUp: true },
              { name: "eth2", adminUp: true, operUp: false },
              { name: "eth3", adminUp: false, operUp: false },
            ],
          },
        })
      : r.fulfill({ status: 503, body: "" }),
  );
  await page.goto("./#/lab/lab1");
  const link = (name: string) => page.locator(".topo-pair g", { has: page.locator("title", { hasText: name }) });
  await expect(link("gen eth1")).toHaveClass(/st-up/);
  await expect(link("gen eth2")).toHaveClass(/st-down/);
  await expect(link("gen eth2").locator("title")).toContainText("· down");
  await expect(link("gen eth2").locator(".topo-end")).toHaveCount(1);
  await expect(link("gen eth3")).toHaveClass(/st-admin-down/);
  await expect(link("r1 e1-1-c10-1")).toHaveClass(/st-none/);
  await expect(page.locator(".topo-legend")).toContainText("shut (admin down)");
  const row = (port: string) => page.locator(".topo-table tr", { hasText: port });
  await expect(row("eth2")).toContainText("down");
  await expect(row("eth3")).toContainText("shut (admin down)");

  // The agent stops answering: its links are unknown, not down.
  reachable = false;
  await expect(link("gen eth1")).toHaveClass(/st-unknown/, { timeout: 8000 });
  await expect(link("gen eth2")).toHaveClass(/st-unknown/);
});

test("a bundle counts its links that are down", async ({ page }) => {
  await mockAgent(page, { name: "pk", type: "packet", device: "d1", state: "running", config: {} }, () => undefined);
  await page.route("**/api/agents", (r) => r.fulfill({ json: { agents: [{ id: "clab-lab2-gen", name: "gen", lab: "lab2", source: "docker" }], allow_manual: false } }));
  await page.route("**/api/info", (r) => r.fulfill({ json: { version: "dev", update_available: false, topology: true } }));
  const ls = Array.from({ length: 8 }, (_, i) => ({ a: "gen", a_if: `eth${i + 1}`, z: "sw1", z_if: `e1-1-c${i + 1}-1` }));
  await page.route("**/api/labs/lab2/topology", (r) =>
    r.fulfill({ json: { lab: "lab2", nodes: [{ name: "gen", kind: "linux", agent: "clab-lab2-gen" }, { name: "sw1", kind: "nokia_srsim" }], links: ls } }),
  );
  await page.route("**/agents/clab-lab2-gen/clabgen.v1.AgentService/ListPorts", (r) =>
    r.fulfill({ json: { ports: ls.map((l, i) => ({ name: l.a_if, adminUp: true, operUp: i !== 2 && i !== 5 })) } }),
  );
  await page.goto("./#/lab/lab2");
  const b = page.locator(".topo-pair.bundle");
  await expect(b.locator(".topo-badge-text")).toHaveText("×8 · 2 down");
  await expect(b).toHaveClass(/st-down/);
  await b.locator(".topo-badge").click();
  await expect(page.locator(".topo-sel")).toContainText("eth3 ↔ e1-1-c3-1 down");
});
