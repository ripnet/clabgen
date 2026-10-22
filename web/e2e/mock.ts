import { readFileSync } from "node:fs";
import type { Page, Route } from "@playwright/test";

// The agent's protocol types, written by the Go test TestUITypesFixture
// (CLABGEN_WRITE_FIXTURES=1 go test ./internal/server).
const types = JSON.parse(readFileSync(new URL("./fixtures/protocol-types.json", import.meta.url), "utf8"));

const svc = "clabgen.v1.AgentService";

export type Protocol = { name: string; type: string; device: string; state: string; config: Record<string, unknown>; disabled?: boolean };

export type MockOpts = {
  more?: Protocol[]; // further protocols on the agent
  refuseToggle?: string[]; // names SetProtocolDisabled fails for
  group?: MockGroup;
};

// MockGroup is a device group "name" of count members running one bgp
// protocol; disabled holds the member indices (1-based) that are off.
export type MockGroup = { name: string; count: number; specDisabled: boolean; disabled: Set<number> };

// members parses a selection like "1-3,5" (empty: all).
function members(sel: string, count: number): number[] {
  if (!sel || sel === "all") return Array.from({ length: count }, (_, i) => i + 1);
  return sel.split(",").flatMap((part) => {
    const [a, b] = part.split("-").map(Number);
    return Array.from({ length: (b ?? a) - a + 1 }, (_, i) => a + i);
  });
}

function groupMsg(g: MockGroup) {
  const states: Record<string, number> = {};
  const ms = Array.from({ length: g.count }, (_, i) => {
    const state = g.disabled.has(i + 1) ? "disabled" : "running";
    states[state] = (states[state] ?? 0) + 1;
    return { index: i + 1, device: `${g.name}-${i + 1}`, mac: "", vlans: "", protocols: [{ name: `${g.name}-${i + 1}-bgp`, state, ...(state === "running" ? { health: "established" } : {}) }] };
  });
  const health = states.running ? { established: states.running } : undefined;
  return {
    name: g.name,
    spec: { name: g.name, count: g.count, device: { port: "eth1" }, protocols: [{ name: "bgp", type: "bgp", ...(g.specDisabled ? { disabled: true } : {}) }] },
    status: { devices: g.count, protocols: [{ name: "bgp", type: "bgp", states, ...(health ? { health } : {}), ...(g.specDisabled ? { disabled: true } : {}) }], members: ms },
    yaml: "",
  };
}

// mockAgent serves one agent "a1" holding protocol p (and opts.more).
// Unary calls answer with Connect JSON: update decides UpdateProtocol (an
// error message rejects it); other calls get an empty response.
export async function mockAgent(page: Page, p: Protocol, update: (config: Record<string, unknown>) => string | undefined, opts: MockOpts = {}) {
  const all = [p, ...(opts.more ?? [])];
  const named = (r: Route) => {
    const name = (r.request().postDataJSON() as { name: string }).name;
    return all.find((x) => x.name === name);
  };
  const fail = (r: Route, code: string, message: string) =>
    r.fulfill({ status: code === "not_found" ? 404 : 400, contentType: "application/json", body: JSON.stringify({ code, message }) });
  const ok = (r: Route, body: unknown) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/agents", (r) => ok(r, { agents: [{ id: "a1", name: "a1", source: "test" }], allow_manual: false }));
  await page.route(`**/agents/a1/${svc}/*`, (r) => ok(r, {}));
  // A server stream that ends at once: an end-of-stream envelope.
  await page.route(`**/agents/a1/${svc}/WatchEvents`, (r) => {
    const end = Buffer.from("{}");
    const head = Buffer.alloc(5);
    head[0] = 2;
    head.writeUInt32BE(end.length, 1);
    return r.fulfill({ status: 200, contentType: "application/connect+json", body: Buffer.concat([head, end]) });
  });
  await page.route(`**/agents/a1/${svc}/ListProtocolTypes`, (r) => ok(r, types));
  await page.route(`**/agents/a1/${svc}/GetProtocol`, (r) => {
    const x = named(r);
    return x ? ok(r, { protocol: x }) : fail(r, "not_found", "no such protocol");
  });
  await page.route(`**/agents/a1/${svc}/ListProtocols`, (r) => ok(r, { protocols: all }));
  await page.route(`**/agents/a1/${svc}/SetProtocolDisabled`, (r) => {
    const req = r.request().postDataJSON() as { name: string; disabled?: boolean };
    const x = named(r);
    if (!x) return fail(r, "not_found", "no such protocol");
    if (opts.refuseToggle?.includes(req.name)) return fail(r, "failed_precondition", `protocol ${req.name}: refused`);
    x.disabled = !!req.disabled;
    x.state = x.disabled ? "disabled" : "running";
    return ok(r, { protocol: x });
  });
  const g = opts.group;
  if (g) {
    await page.route(`**/agents/a1/${svc}/ListDeviceGroups`, (r) => ok(r, { groups: [groupMsg(g)] }));
    await page.route(`**/agents/a1/${svc}/GetDeviceGroup`, (r) => ok(r, { group: groupMsg(g) }));
    await page.route(`**/agents/a1/${svc}/SetGroupDisabled`, (r) => {
      const req = r.request().postDataJSON() as { members?: string; disabled?: boolean };
      const sel = members(req.members ?? "", g.count);
      if (sel.length === g.count) g.specDisabled = !!req.disabled;
      for (const i of sel) {
        if (req.disabled) g.disabled.add(i);
        else g.disabled.delete(i);
      }
      return ok(r, { results: sel.map((i) => ({ index: i, protocol: `${g.name}-${i}-bgp` })) });
    });
  }
  await page.route(`**/agents/a1/${svc}/UpdateProtocol`, (r) => {
    const req = r.request().postDataJSON() as { config: Record<string, unknown> };
    const err = update(req.config);
    if (err) return r.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ code: "invalid_argument", message: err }) });
    p.config = req.config;
    return ok(r, { protocol: p });
  });
}
