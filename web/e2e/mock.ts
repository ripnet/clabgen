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
};

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
  await page.route(`**/agents/a1/${svc}/UpdateProtocol`, (r) => {
    const req = r.request().postDataJSON() as { config: Record<string, unknown> };
    const err = update(req.config);
    if (err) return r.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ code: "invalid_argument", message: err }) });
    p.config = req.config;
    return ok(r, { protocol: p });
  });
}
