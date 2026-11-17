// The YAML editor's schema help, without a browser: where the cursor is,
// what it offers, what it flags. Against the agent's real scenario schema.
import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { check, hoverAt, keyOffers, placeAt, resolve, valueOffers, type Ctx } from "../src/components/yaml/schema";

const fx = JSON.parse(readFileSync(new URL("./fixtures/protocol-types.json", import.meta.url), "utf8"));
const types = Object.fromEntries(fx.types.map((t: { name: string; configSchema: string }) => [t.name, JSON.parse(t.configSchema)]));
const ctx: Ctx = { root: JSON.parse(fx.scenarioSchema), types };

// at: the place at the "|" in doc, and the offers there.
function at(doc: string) {
  const i = doc.indexOf("|");
  const text = doc.slice(0, i) + doc.slice(i + 1);
  const before = text.slice(0, i).split("\n");
  const p = placeAt(text, before.length - 1, before.at(-1)!.length);
  const s = resolve(ctx, text, p.path);
  const offers = !s ? [] : p.kind === "key" ? keyOffers(s, p.present) : valueOffers(resolve(ctx, text, p.path, p.key) ?? s, p.key, text);
  return { p, labels: offers.map((o) => o.label), offers };
}

test("top-level keys, then a list item's fields (go-yaml's unindented lists)", () => {
  expect(at("|").labels).toEqual(["ports", "devices", "device_groups", "protocols"]);
  expect(at("devices:\n- name: gen\n|").labels).toEqual(["ports", "device_groups", "protocols"]);
  const item = at("protocols:\n- name: p1\n  |");
  expect(item.p.path).toEqual([{ key: "protocols" }, { item: 0, line: 1, col: 2 }]);
  expect(item.labels).toEqual(["type", "device", "config", "disabled"]);
  expect(item.offers[0].detail).toContain("required");
});

test("indented lists, a second item, and its fields", () => {
  const r = at("protocols:\n  - name: a\n    type: bgp\n  - name: b\n    |");
  expect(r.p.path).toEqual([{ key: "protocols" }, { item: 1, line: 3, col: 4 }]);
  expect(r.labels).toEqual(["type", "device", "config", "disabled"]);
});

test("a protocol's config takes its type's fields", () => {
  const doc = (type: string) => `protocols:\n- name: p\n  type: ${type}\n  device: gen\n  config:\n    |`;
  expect(at(doc("udp")).labels).toContain("destination");
  expect(at(doc("udp")).labels.slice(0, 3)).toContain("destination"); // x-common ranks first
  expect(at(doc("bgp")).labels).toContain("local_as");
  expect(at(doc("bgp")).labels).not.toContain("destination");
  // type after config, as go-yaml sorts keys
  expect(at("protocols:\n- config:\n    |\n  device: gen\n  name: p\n  type: icmp").labels).toContain("destination");
});

test("deeper: a list of objects inside a config", () => {
  const r = at("protocols:\n- name: b\n  type: bgp\n  config:\n    peers:\n    - address: 10.0.0.1\n      |");
  expect(r.labels).toContain("remote_as");
  expect(r.labels).not.toContain("address");
});

test("values: enums, booleans, devices", () => {
  expect(at("protocols:\n- name: p\n  type: |").labels).toContain("twamp-reflector");
  expect(at("ports:\n  eth1:\n    admin: |").labels).toEqual(["up", "down"]);
  expect(at("protocols:\n- name: p\n  disabled: |").labels).toEqual(["true", "false"]);
  const devs = at("devices:\n- name: gen\n  port: eth1\n- name: cpe\n  port: eth2\nprotocols:\n- name: p\n  device: |");
  expect(devs.labels).toEqual(["gen", "cpe"]);
  expect(at("protocols:\n- name: t\n  type: twamp-reflector\n  config:\n    mode: |").labels).toEqual(["stamp", "twamp-light"]);
});

test("flow style", () => {
  expect(at("protocols:\n  - {name: r, type: twamp-reflector, |}").labels).toEqual(["device", "config", "disabled"]);
  expect(at("protocols:\n  - {name: r, type: twamp-reflector, config: {mo|}}").labels).toContain("mode");
  expect(at("devices:\n  - {name: g, port: eth1, vlans: [{|}]}").labels).toEqual(["tpid", "vid", "pcp", "dei"]);
  expect(at("protocols:\n  - {name: r, type: twamp-reflector, config: {mode: |}}").labels).toEqual(["stamp", "twamp-light"]);
});

test("device groups: the device template and members' protocols", () => {
  expect(at("device_groups:\n- name: cpe\n  count: 10\n  device:\n    |").labels).toContain("addresses");
  expect(at("device_groups:\n- name: cpe\n  protocols:\n  - type: dhcp\n    config:\n      |").labels.length).toBeGreaterThan(0);
});

test("hover names the field and what it takes", () => {
  const text = "protocols:\n- name: t\n  type: twamp-reflector\n  config:\n    drop_pct: 5";
  const h = hoverAt(ctx, text, 4, 6)!;
  expect(h.key).toBe("drop_pct");
  expect(h.at.schema.description).toMatch(/replies/i);
  expect(hoverAt(ctx, text, 1, 3)!.at.schema.description).toMatch(/unique/);
});

test("check: unknown fields with a suggestion, types, enums, ranges, required", () => {
  const msgs = (t: string) => check(ctx, t).map((p) => p.message);
  expect(msgs("devices:\n- name: g\n  port: eth1\n")).toEqual([]);
  expect(msgs("devices:\n- name: g\n  prot: eth1\n")).toEqual(["unknown field prot (did you mean port?)", "port is required"]);
  expect(msgs("protocols:\n- name: t\n  type: twamp-reflector\n  device: g\n  config:\n    drop_pct: 500\n    mode: stmp\n")).toEqual([
    "should be at most 100",
    "stmp is not one of stamp, twamp-light (did you mean stamp?)",
  ]);
  expect(msgs("protocols:\n- name: t\n  type: twamp-reflectr\n  device: g\n")[0]).toMatch(/did you mean twamp-reflector/);
  expect(msgs("protocols:\n- name: t\n  type: udp\n  device: g\n  config: {rate: fast}\n")).toEqual(["fast: should be a number"]);
  expect(msgs("ports: {eth1: {admin: sideways}}")[0]).toMatch(/not one of up, down/);
  expect(msgs("devices: [")[0]).toBeTruthy(); // YAML syntax
  // where: the unknown key itself
  const t = "devices:\n- name: g\n  prot: eth1\n";
  const p = check(ctx, t)[0];
  expect(t.slice(p.from, p.to)).toBe("prot");
});

test("check: templates pass in device groups, not elsewhere", () => {
  const g = `device_groups:
- name: cpe
  count: 4
  device:
    port: eth1
    vlans: [{vid: {start: 100}}]
    addresses: [{start: 10.1.0.2/30, step: 0.0.0.4}]
  protocols:
  - type: bgp
    config: {local_as: {start: 65001}, router_id: "10.0.0.{i}"}
`;
  expect(check(ctx, g).map((p) => p.message)).toEqual([]);
  expect(check(ctx, "devices:\n- name: g\n  port: eth1\n  vlans: [{vid: {start: 100}}]\n").map((p) => p.message)).toEqual(["should be integer, not fields"]);
});

test("a protocol's config on its own (the form's YAML view)", () => {
  const own: Ctx = { root: types["twamp-reflector"] };
  expect(keyOffers(resolve(own, "", [])!, []).map((o) => o.label)).toContain("delay_us");
  expect(check(own, "delay_us: x\n").map((p) => p.message)).toEqual(["x: should be a number"]);
});
